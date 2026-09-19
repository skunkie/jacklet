// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLogDestination_DefaultsToStdout(t *testing.T) {
	destination, closeLog, err := logDestination("")
	require.NoError(t, err)
	defer closeLog()
	require.Equal(t, os.Stdout, destination)
}

func TestOpenLogFile_CreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "jacklet.log")

	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	_, err = file.Write([]byte("first\n"))
	require.NoError(t, err)
	require.FileExists(t, path)
}

func TestOpenLogFile_AppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	require.NoError(t, os.WriteFile(path, []byte("earlier\n"), 0o600))

	file, err := openLogFile(path)
	require.NoError(t, err)
	_, err = file.Write([]byte("later\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "earlier\nlater\n", string(contents))
}

func TestRotatingFile_CloseIsFinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)

	_, err = file.Write([]byte("before\n"))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, file.Close(), "closing an already closed log is harmless")

	_, err = file.Write([]byte("after\n"))
	require.ErrorIs(t, err, os.ErrClosed)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "before\n", string(contents))
}

// writeUntilRotated writes record repeatedly until the file has rotated at
// least want times.
func writeUntilRotated(t *testing.T, file *rotatingFile, record string, want int) {
	t.Helper()
	// One record per maxLogBytes/len(record) writes rotates once; a margin
	// covers the partial first generation.
	for range (maxLogBytes/len(record) + 2) * want {
		_, err := file.Write([]byte(record))
		require.NoError(t, err)
	}
}

func TestRotatingFile_RotatesAtTheSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	writeUntilRotated(t, file, strings.Repeat("x", 4095)+"\n", 1)

	require.FileExists(t, path)
	require.FileExists(t, path+".1")

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.LessOrEqual(t, info.Size(), int64(maxLogBytes), "the live file stays under the cap")
}

func TestRotatingFile_KeepsOnlyTheRetainedGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	writeUntilRotated(t, file, strings.Repeat("x", 4095)+"\n", 5)

	// Spelled out rather than derived from logRetained: a test that reads
	// the same constant the code does agrees with it by construction, and
	// would keep passing if the retention changed by accident.
	require.FileExists(t, path+".1")
	require.FileExists(t, path+".2")
	require.FileExists(t, path+".3")
	require.NoFileExists(t, path+".4", "a generation past the retention limit is dropped, not kept")
}

// TestRotatingFile_ClosesBeforeRenaming pins the ordering rotate depends
// on. Windows refuses to rename a file that is still open, so a rotate
// that renamed first would fail there; on Unix it would succeed, so the
// ordering has to be observed as it happens rather than inferred from the
// state rotate leaves behind.
func TestRotatingFile_ClosesBeforeRenaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	_, err = file.Write([]byte("before\n"))
	require.NoError(t, err)

	handle := file.file
	renames := 0
	original := renameFile
	t.Cleanup(func() { renameFile = original })
	renameFile = func(oldpath, newpath string) error {
		renames++
		_, err := handle.Write(nil)
		require.ErrorIs(t, err, os.ErrClosed,
			"the log file was still open when rotate renamed it; Windows would refuse this")
		return original(oldpath, newpath)
	}

	require.NoError(t, file.rotate())
	require.Positive(t, renames, "rotate renamed nothing, so the ordering went unchecked")
	require.NotSame(t, handle, file.file)

	rotated, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Equal(t, "before\n", string(rotated))

	_, err = file.Write([]byte("after\n"))
	require.NoError(t, err)
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "after\n", string(current), "the reopened file starts empty")
}

func TestRotatingFile_WritesConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	const writers, perWriter = 8, 200
	record := strings.Repeat("y", 63) + "\n"

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				_, err := file.Write([]byte(record))
				require.NoError(t, err)
			}
		})
	}
	wg.Wait()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(writers*perWriter*len(record)), info.Size(),
		"every record reached the file exactly once")
}

// TestRotatingFile_KeepsLoggingWhenARotationFails covers the failure that
// would otherwise be permanent: rotation closes the file first, so a
// rename that fails must not leave it closed for the life of the process.
func TestRotatingFile_KeepsLoggingWhenARotationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	_, err = file.Write([]byte("before\n"))
	require.NoError(t, err)

	original := renameFile
	t.Cleanup(func() { renameFile = original })
	renameFile = func(string, string) error { return errors.New("rename refused") }

	require.Error(t, file.rotate(), "a failed rotation is reported")

	_, err = file.Write([]byte("after\n"))
	require.NoError(t, err, "logging continues after a failed rotation")

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "before\nafter\n", string(contents),
		"the unrotated file is still the one being written")
}

func TestRotatingFile_RecoversWhenReopeningFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	_, err = file.Write([]byte("before\n"))
	require.NoError(t, err)

	original := openFile
	t.Cleanup(func() { openFile = original })
	failures := 2
	openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if failures > 0 {
			failures--
			return nil, errors.New("open refused")
		}
		return original(name, flag, perm)
	}

	require.Error(t, file.rotate(), "the first reopen failure is reported")
	_, err = file.Write([]byte("dropped\n"))
	require.Error(t, err, "a record cannot be written while reopening still fails")
	_, err = file.Write([]byte("after\n"))
	require.NoError(t, err, "a later write retries reopening the log")

	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "after\n", string(current))
	rotated, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Equal(t, "before\n", string(rotated))
}

// TestRotatingFile_TracksTheSizeOfAnUnrotatedFile pins the consequence of
// carrying on: the size has to come from the file, or the next write
// would believe it had a fresh one and never rotate again.
func TestRotatingFile_TracksTheSizeOfAnUnrotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	_, err = file.Write([]byte(strings.Repeat("x", 4096)))
	require.NoError(t, err)

	original := renameFile
	t.Cleanup(func() { renameFile = original })
	renameFile = func(string, string) error { return errors.New("rename refused") }
	require.Error(t, file.rotate())

	require.Equal(t, int64(4096), file.size, "the size is what the file holds, not zero")
}

// failRenames makes every rename fail and counts the attempts.
func failRenames(t *testing.T) *atomic.Int64 {
	t.Helper()
	var attempts atomic.Int64
	original := renameFile
	t.Cleanup(func() { renameFile = original })
	renameFile = func(string, string) error {
		attempts.Add(1)
		return errors.New("rename refused")
	}
	return &attempts
}

// TestRotatingFile_KeepsWritingWhileRotationKeepsFailing covers what the
// two tests above cannot: they call rotate directly, on a file below the
// cap, so neither reaches Write's own handling of a rotation that fails.
// Over the cap the failure repeats on every record, which is where
// dropping one record turns into dropping all of them.
func TestRotatingFile_KeepsWritingWhileRotationKeepsFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	record := strings.Repeat("x", 4095) + "\n"
	writeUntilRotated(t, file, record, 1)
	failRenames(t)

	// Back over the cap, so every record from here on wants to rotate.
	for file.size+int64(len(record)) <= maxLogBytes {
		_, err = file.Write([]byte(record))
		require.NoError(t, err)
	}

	before := file.size
	for range 3 {
		n, err := file.Write([]byte("after\n"))
		require.NoError(t, err, "a record is written even though the file could not be rotated")
		require.Equal(t, len("after\n"), n)
	}

	require.Equal(t, before+int64(3*len("after\n")), file.size,
		"the file grows past the cap rather than losing the records")

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(contents), strings.Repeat("after\n", 3)),
		"all three records reached the file")
}

// TestRotatingFile_WaitsBeforeRetryingAFailedRotation pins the other half:
// carrying on must not mean attempting the rename that just failed once
// per record for as long as the cause lasts.
func TestRotatingFile_WaitsBeforeRetryingAFailedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jacklet.log")
	file, err := openLogFile(path)
	require.NoError(t, err)
	defer file.Close()

	record := strings.Repeat("x", 4095) + "\n"
	for file.size+int64(len(record)) <= maxLogBytes {
		_, err = file.Write([]byte(record))
		require.NoError(t, err)
	}

	attempts := failRenames(t)
	clock := time.Now()
	original := timeNow
	t.Cleanup(func() { timeNow = original })
	timeNow = func() time.Time { return clock }

	for range 5 {
		_, err = file.Write([]byte("over\n"))
		require.NoError(t, err)
	}
	// One attempt, not five: the shift gives up on its first failure.
	require.Equal(t, int64(1), attempts.Load(),
		"the rename is not retried on every record")

	clock = clock.Add(rotateRetryInterval)
	_, err = file.Write([]byte("over\n"))
	require.NoError(t, err)
	require.Equal(t, int64(2), attempts.Load(),
		"the rotation is attempted again once the interval has passed")
}

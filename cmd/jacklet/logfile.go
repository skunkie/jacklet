// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxLogBytes is how large the log file grows before it is rotated.
const maxLogBytes = 10 << 20

// logRetained is how many rotated files are kept alongside the current one.
const logRetained = 3

// rotateRetryInterval is how long a failed rotation is left before it is
// attempted again. Once the file is over the cap every record would
// otherwise try to rotate it, so a rename something is holding open for a
// moment would mean one failed rename per record until it let go.
const rotateRetryInterval = time.Minute

// rotatingFile is an io.WriteCloser that caps its file at maxLogBytes,
// keeping logRetained older generations beside it.
//
// Jacklet rotates its own log only because a log file is written where
// nothing else is there to do it: a Windows service has no stdout for a
// service manager to route, and a process that runs for months would
// otherwise fill the disk.
type rotatingFile struct {
	file     *os.File
	isClosed bool
	mu       sync.Mutex
	path     string
	retryAt  time.Time
	size     int64
}

// timeNow is time.Now, indirected so a test can reach the far side of the
// interval between rotation attempts without waiting for it.
var timeNow = time.Now

// openFile is os.OpenFile, indirected so a test can cover a transient
// failure while a rotated log is reopened.
var openFile = os.OpenFile

// openLogFile opens path for appending, creating its directory if needed.
func openLogFile(path string) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating the log directory: %w", err)
	}
	file, err := openFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening the log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("reading the log file size: %w", err)
	}
	return &rotatingFile{file: file, path: path, size: info.Size()}, nil
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.isClosed {
		return 0, os.ErrClosed
	}

	// A missing handle here means an earlier reopen failed. Retry on every
	// record so a momentary sharing or filesystem error does not silence a
	// minute of logs, and refresh the size before deciding whether this
	// write needs another rotation.
	if f.file == nil {
		if err := f.reopen(); err != nil {
			return 0, err
		}
	}

	// Rotate before the write rather than after, so a single record is
	// never split across two files.
	if f.size > 0 && f.size+int64(len(p)) > maxLogBytes && !timeNow().Before(f.retryAt) {
		if err := f.rotate(); err != nil {
			// The record is still written, to whichever file rotate left
			// open. Refusing to write it would mean dropping every record
			// for as long as the rotation kept failing -- the file is over
			// the cap, so the next record would try to rotate, fail, and
			// be dropped in turn -- which turns a file that is too large
			// into no log at all, and the size is much the lesser
			// problem. Nor is a report lost by not returning the error:
			// the only writer here is slog, which discards what a
			// handler's writer returns.
			f.retryAt = timeNow().Add(rotateRetryInterval)
		}
	}
	// rotate closes the handle before moving the file, so its own reopen
	// failure leaves this write one immediate recovery attempt.
	if f.file == nil {
		if err := f.reopen(); err != nil {
			return 0, err
		}
	}

	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *rotatingFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isClosed = true
	if f.file == nil {
		return nil
	}
	file := f.file
	f.file = nil
	return file.Close()
}

// renameFile is os.Rename, indirected so a test can observe the state of
// the log handle at the moment the rename happens. The ordering it guards
// has no observable effect on Unix, which is the whole reason it needs
// pinning somewhere other than a Windows-only test.
var renameFile = os.Rename

// rotate closes the current file, shifts the kept generations along, and
// reopens the path.
//
// Closing first is what makes this work on Windows, which refuses to
// rename a file that is still open; on Unix the rename would succeed
// either way, so nothing there would catch the ordering being wrong.
func (f *rotatingFile) rotate() error {
	if f.file != nil {
		file := f.file
		f.file = nil
		if err := file.Close(); err != nil {
			return fmt.Errorf("closing the log file to rotate it: %w", err)
		}
	}

	// Whatever came of the shuffle, the file is reopened: leaving it
	// closed would silence every later write for the life of the process,
	// which is a worse answer to a failed rename than carrying on with a
	// file that did not rotate.
	shiftErr := f.shiftGenerations()
	return errors.Join(shiftErr, f.reopen())
}

// shiftGenerations moves the kept generations along and the current file
// into the first of them.
func (f *rotatingFile) shiftGenerations() error {
	if err := os.Remove(f.generation(logRetained)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the oldest log file: %w", err)
	}
	for i := logRetained - 1; i >= 1; i-- {
		if err := renameFile(f.generation(i), f.generation(i+1)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotating the log files: %w", err)
		}
	}
	if err := renameFile(f.path, f.generation(1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotating the log file: %w", err)
	}
	return nil
}

// reopen reopens the log file and takes its size from what is on disk,
// which is zero after a rotation and the previous size after one that
// failed.
func (f *rotatingFile) reopen() error {
	file, err := openFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("reopening the log file: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("reading the log file size: %w", err)
	}
	f.file = file
	f.size = info.Size()
	return nil
}

// generation names the nth rotated file, oldest last.
func (f *rotatingFile) generation(n int) string {
	return fmt.Sprintf("%s.%d", f.path, n)
}

// logDestination returns where log records are written: the named file
// when one is configured, and stdout otherwise.
//
// Stdout is the default everywhere because it lets the deployment
// environment own log routing and retention. A file is for a deployment
// that has no such environment.
func logDestination(path string) (io.Writer, func(), error) {
	if path == "" {
		return os.Stdout, func() {}, nil
	}
	file, err := openLogFile(path)
	if err != nil {
		return nil, nil, err
	}
	return file, func() { file.Close() }, nil
}

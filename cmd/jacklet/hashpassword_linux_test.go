// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// terminalTimeout bounds every wait on the pseudo-terminal, so a prompt that
// never appears fails the test instead of hanging it.
const terminalTimeout = 5 * time.Second

// pseudoTerminal is a pseudo-terminal pair: what is written to master is
// typed on slave, and what is written to slave is read back from master.
type pseudoTerminal struct {
	master          *os.File
	output          strings.Builder
	shouldKeepSlave bool
	slave           *os.File
}

// openPseudoTerminal opens a pseudo-terminal, or skips the test where the
// system offers none.
func openPseudoTerminal(t *testing.T) *pseudoTerminal {
	t.Helper()
	// Opened without Fd, so the master stays on the runtime poller and its
	// reads honor a deadline.
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	raw, err := master.SyscallConn()
	require.NoError(t, err)
	var number int
	var ioctlErr error
	require.NoError(t, raw.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr != nil {
			return
		}
		number, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN)
	}))
	require.NoError(t, ioctlErr)

	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	require.NoError(t, err)
	terminal := &pseudoTerminal{master: master, slave: slave}
	t.Cleanup(func() {
		if !terminal.shouldKeepSlave {
			slave.Close()
		}
	})
	return terminal
}

// keepSlaveOpen leaves the slave open for the rest of the process. A read
// abandoned on it still restores the terminal by its raw descriptor number
// when it returns, and closing the slave first would free that number for
// the next file opened, which the late restore would then reconfigure.
func (p *pseudoTerminal) keepSlaveOpen() {
	p.shouldKeepSlave = true
}

// readUntil reads from the master until everything read so far contains
// want.
func (p *pseudoTerminal) readUntil(t *testing.T, want string) {
	t.Helper()
	require.NoError(t, p.master.SetReadDeadline(time.Now().Add(terminalTimeout)))
	buf := make([]byte, 256)
	for !strings.Contains(p.output.String(), want) {
		n, err := p.master.Read(buf)
		p.output.Write(buf[:n])
		require.NoError(t, err, "the terminal never showed %q; it showed %q", want, p.output.String())
	}
}

// isEchoing reports whether the terminal shows what is typed on it.
func (p *pseudoTerminal) isEchoing(t *testing.T) bool {
	t.Helper()
	isEchoing, err := p.echoes()
	require.NoError(t, err)
	return isEchoing
}

// echoes reads the terminal's echo flag. It reports an error rather than
// failing the test, so a condition polled on another goroutine can call it.
func (p *pseudoTerminal) echoes() (bool, error) {
	termios, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		return false, err
	}
	return termios.Lflag&unix.ECHO != 0, nil
}

// awaitEchoOff waits until the terminal stops showing what is typed. The
// prompt is written just before echo is turned off, so seeing it is not yet
// enough to type safely.
func (p *pseudoTerminal) awaitEchoOff(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		isEchoing, err := p.echoes()
		return err == nil && !isEchoing
	}, terminalTimeout, time.Millisecond, "echo was never turned off for the password")
}

// hashPasswordAsync runs hashPassword on the terminal, reporting its error on
// the returned channel and its output in out.
func hashPasswordAsync(p *pseudoTerminal, out *strings.Builder) <-chan error {
	done := make(chan error, 1)
	go func() { done <- hashPassword(p.slave, out, p.slave) }()
	return done
}

// awaitResult waits for hashPassword to return.
func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(terminalTimeout):
		require.FailNow(t, "hash-password never returned")
		return nil
	}
}

// A password typed at a terminal is read twice and never shown, and the
// terminal echoes again once it has been read.
func TestHashPassword_HidesTerminalInput(t *testing.T) {
	terminal := openPseudoTerminal(t)
	require.True(t, terminal.isEchoing(t), "a new terminal should echo")

	var out strings.Builder
	done := hashPasswordAsync(terminal, &out)
	terminal.readUntil(t, "Password: ")
	terminal.awaitEchoOff(t)
	_, err := terminal.master.WriteString("example-pass\n")
	require.NoError(t, err)
	terminal.readUntil(t, "Repeat it: ")
	terminal.awaitEchoOff(t)
	_, err = terminal.master.WriteString("example-pass\n")
	require.NoError(t, err)

	require.NoError(t, awaitResult(t, done))
	require.True(t, strings.HasPrefix(out.String(), "pbkdf2-sha256$"), "unexpected hash %q", out.String())
	require.NotContains(t, terminal.output.String(), "example-pass", "the password was echoed as it was typed")
	require.True(t, terminal.isEchoing(t), "the terminal was left without echo")
}

// An interrupt at the prompt restores the terminal it turned echo off on,
// rather than ending the process with nothing shown as typed afterwards.
func TestHashPassword_RestoresTheTerminalOnInterrupt(t *testing.T) {
	terminal := openPseudoTerminal(t)

	var out strings.Builder
	done := hashPasswordAsync(terminal, &out)
	// The prompt is written after the interrupt handler is installed, so the
	// signal below reaches it rather than ending the test binary.
	terminal.readUntil(t, "Password: ")
	terminal.awaitEchoOff(t)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	err := awaitResult(t, done)
	require.ErrorIs(t, err, errInterrupted)
	require.True(t, terminal.isEchoing(t), "the interrupted prompt left the terminal without echo")
	require.Empty(t, out.String(), "a hash was printed for an abandoned prompt")
	// The abandoned read is still waiting on the terminal, and ends when the
	// master closes.
	terminal.keepSlaveOpen()
}

// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

//go:build !windows

package main

import "errors"

// runService reports that there is no service to manage.
//
// The subcommand exists on every platform so that the help, the manual
// page and the tests covering them describe one program rather than one
// per operating system.
func runService(args []string) error {
	_ = args
	return errors.New("the service commands manage a Windows service and are available on Windows only")
}

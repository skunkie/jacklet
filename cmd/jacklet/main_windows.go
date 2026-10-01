// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
)

// runMain starts the server, under the Service Control Manager when
// Windows launched it as a service and interactively otherwise.
//
// One binary serves both, so an operator running jacklet.exe in a terminal
// and a service manager starting it get the same program.
func runMain(logger *slog.Logger, cfg *settings) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}

	if !isService {
		// Windows delivers no SIGTERM: a console process is stopped with
		// Ctrl+C, which arrives as os.Interrupt.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		return run(ctx, logger, cfg, nil)
	}

	// Every later start reads the hash back as an ordinary setting, before
	// the configuration is parsed. Only the start that performs the
	// exchange is too late for that, so it takes the hash from here.
	//
	// It wins over whatever was parsed, rather than filling in for it. On
	// a first install there is nothing to lose to, but an upgrade that
	// supplies a new password has already had the old hash read into the
	// configuration, and deferring to that would leave the service
	// accepting the password the installer just replaced -- and refusing
	// the one the operator typed -- until something restarted it, which
	// may be months. A hash is also what the settings say wins over a
	// plaintext password, so this displaces nothing that would have
	// outranked it.
	hash, err := consumeBootstrapPassword(registry.LOCAL_MACHINE, logger)
	if err != nil {
		return err
	}
	if hash != "" {
		cfg.AdminPasswordHash = hash
	}

	handler := newServiceHandler(cfg, openEventLog(), logger)
	return svc.Run(serviceName, handler)
}

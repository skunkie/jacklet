// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// runMain starts the server, shutting it down on an interrupt or a
// SIGTERM from a service manager.
func runMain(logger *slog.Logger, cfg *settings) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return run(ctx, logger, cfg, nil)
}

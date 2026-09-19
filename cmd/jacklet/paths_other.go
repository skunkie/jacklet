// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

//go:build !windows

package main

import "log/slog"

// applyServiceSettings does nothing.
//
// Elsewhere a service manager supplies the environment a service runs
// with, so there is nothing for Jacklet to read back.
func applyServiceSettings(logger *slog.Logger) { _ = logger }

// resolveServicePaths leaves the configuration alone.
//
// Elsewhere a service manager sets the working directory, so the
// configured paths resolve against it as they do for any other process.
func resolveServicePaths(cfg *settings) { _ = cfg }

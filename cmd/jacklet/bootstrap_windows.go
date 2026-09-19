// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/windows/registry"

	"github.com/torrplay/jacklet/pkg/admin"
)

// productKeyPath is Jacklet's own registry key, separate from the service
// key. The service account is granted write access here so it can consume
// the bootstrap password; granting it on the service key instead would let
// the service rewrite its own ImagePath.
const productKeyPath = `SOFTWARE\` + serviceName

// bootstrapKeyPath holds the plaintext admin password an installer
// collected, until the first start exchanges it for a hash.
const bootstrapKeyPath = productKeyPath + `\Setup`

// bootstrapPasswordValue holds the plaintext until the first start.
const bootstrapPasswordValue = "AdminPassword"

// passwordHashSetting is where the hash is kept afterwards as an ordinary
// PascalCase setting, so it is carried across an upgrade and removed on an
// uninstall with the rest of them rather than outliving both.
const passwordHashSetting = "AdminPasswordHash"

// consumeBootstrapPassword exchanges an installer-supplied plaintext
// password for a hash, and removes the plaintext.
//
// An installer cannot hash the password itself: the Windows Installer has
// no way to compute one, and handing the plaintext to a helper would put
// it on a command line, where every other process can read it and the
// installer log records it. So it is written once, in a key only
// administrators and the service account can read, and this exchanges it
// at the first start.
//
// The hash is written before the plaintext is removed, so an interrupted
// first start leaves one or the other, never neither.
// It returns the hash it stored, so that the start which performs the
// exchange can use it too rather than waiting for the next one to read it
// back.
func consumeBootstrapPassword(root registry.Key, logger *slog.Logger) (string, error) {
	setup, err := registry.OpenKey(root, bootstrapKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("opening the setup key: %w", err)
	}
	defer setup.Close()

	password, _, err := setup.GetStringValue(bootstrapPasswordValue)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the bootstrap password: %w", err)
	}
	if password == "" {
		return "", deleteBootstrapPassword(setup)
	}

	hash, err := admin.HashPassword(password)
	if err != nil {
		return "", fmt.Errorf("hashing the bootstrap password: %w", err)
	}

	settings, _, err := registry.CreateKey(root, settingsKeyPath, registry.SET_VALUE)
	if err != nil {
		return "", fmt.Errorf("opening the settings key: %w", err)
	}
	defer settings.Close()

	if err := settings.SetStringValue(passwordHashSetting, hash); err != nil {
		return "", fmt.Errorf("storing the admin password hash: %w", err)
	}
	if err := deleteBootstrapPassword(setup); err != nil {
		return "", err
	}

	logger.Info("hashed the admin password supplied by the installer and removed the plaintext")
	return hash, nil
}

// deleteBootstrapPassword removes the plaintext value.
func deleteBootstrapPassword(setup registry.Key) error {
	if err := setup.DeleteValue(bootstrapPasswordValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("removing the bootstrap password: %w", err)
	}
	return nil
}

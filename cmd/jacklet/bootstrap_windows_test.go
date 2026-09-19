// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/registry"

	"github.com/torrplay/jacklet/pkg/admin"
)

// testRegistryRoot returns a registry root the test can write without
// elevation, cleaned up afterwards. The production root is HKLM, which
// needs administrator rights and is machine-wide.
func testRegistryRoot(t *testing.T) registry.Key {
	t.Helper()

	root, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\JackletTest`, registry.ALL_ACCESS)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = registry.DeleteKey(root, settingsKeyPath)
		_ = registry.DeleteKey(root, bootstrapKeyPath)
		_ = registry.DeleteKey(root, installerKeyPath)
		_ = registry.DeleteKey(root, productKeyPath)
		root.Close()
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\JackletTest`)
	})
	return root
}

// storedHash reads back the hash the bootstrap produced.
func storedHash(t *testing.T, root registry.Key) string {
	t.Helper()
	settings, err := readServiceSettings(root)
	require.NoError(t, err)
	value, _ := settings.lookup(passwordHashSetting)
	return value
}

// writeBootstrapPassword seeds the plaintext an installer would have left.
func writeBootstrapPassword(t *testing.T, root registry.Key, password string) {
	t.Helper()
	setup, _, err := registry.CreateKey(root, bootstrapKeyPath, registry.ALL_ACCESS)
	require.NoError(t, err)
	defer setup.Close()
	require.NoError(t, setup.SetStringValue(bootstrapPasswordValue, password))
}

func TestConsumeBootstrapPassword_HashesAndRemovesThePlaintext(t *testing.T) {
	root := testRegistryRoot(t)
	writeBootstrapPassword(t, root, "installer-password")

	_, err := consumeBootstrapPassword(root, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	hash := storedHash(t, root)
	require.NotEmpty(t, hash)
	require.NotContains(t, hash, "installer-password", "the stored value is a hash, not the password")

	password, err := admin.NewPassword("", hash)
	require.NoError(t, err)
	require.True(t, password.Hashed())

	setup, err := registry.OpenKey(root, bootstrapKeyPath, registry.QUERY_VALUE)
	require.NoError(t, err)
	defer setup.Close()
	_, _, err = setup.GetStringValue(bootstrapPasswordValue)
	require.ErrorIs(t, err, registry.ErrNotExist, "the plaintext is gone once it has been hashed")
}

func TestConsumeBootstrapPassword_IsANoOpWithoutABootstrap(t *testing.T) {
	root := testRegistryRoot(t)

	_, err := consumeBootstrapPassword(root, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.Empty(t, storedHash(t, root))
}

func TestConsumeBootstrapPassword_ReplacesAnEarlierHash(t *testing.T) {
	root := testRegistryRoot(t)
	writeBootstrapPassword(t, root, "first-password")
	_, err := consumeBootstrapPassword(root, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	first := storedHash(t, root)

	writeBootstrapPassword(t, root, "second-password")
	_, err = consumeBootstrapPassword(root, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	second := storedHash(t, root)

	require.NotEqual(t, first, second, "a newly supplied password replaces the stored hash")
}

func TestConsumeBootstrapPassword_StoresTheHashAsASetting(t *testing.T) {
	root := testRegistryRoot(t)
	writeBootstrapPassword(t, root, "installer-password")
	_, err := consumeBootstrapPassword(root, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	// Stored as a setting rather than beside them, so that it is carried
	// across an upgrade and removed on an uninstall with the rest.
	settings, err := readServiceSettings(root)
	require.NoError(t, err)
	value, ok := settings.lookup(passwordHashSetting)
	require.True(t, ok, "the hash is one of the settings")
	require.NotEmpty(t, value)
}

func TestReadServiceSettings_IsEmptyWithoutAKey(t *testing.T) {
	settings, err := readServiceSettings(testRegistryRoot(t))
	require.NoError(t, err)
	require.Empty(t, settings)
}

func TestReadServiceSettings_ReadsEveryValue(t *testing.T) {
	root := testRegistryRoot(t)
	key, _, err := registry.CreateKey(root, settingsKeyPath, registry.ALL_ACCESS)
	require.NoError(t, err)
	defer key.Close()
	require.NoError(t, key.SetStringValue("ApiKey", "secret"))
	require.NoError(t, key.SetStringValue("Port", "9118"))

	settings, err := readServiceSettings(root)
	require.NoError(t, err)
	require.Equal(t, serviceConfiguration{"ApiKey=secret", "Port=9118"}, settings)

	value, ok := settings.lookup("Port")
	require.True(t, ok)
	require.Equal(t, "9118", value)
}

func TestRemoveProductRegistry_RemovesConfigurationAndBootstrapState(t *testing.T) {
	root := testRegistryRoot(t)
	writeBootstrapPassword(t, root, "installer-password")

	settings, _, err := registry.CreateKey(root, settingsKeyPath, registry.ALL_ACCESS)
	require.NoError(t, err)
	require.NoError(t, settings.SetStringValue("ApiKey", "secret"))
	require.NoError(t, settings.Close())

	installer, _, err := registry.CreateKey(root, installerKeyPath, registry.ALL_ACCESS)
	require.NoError(t, err)
	require.NoError(t, installer.SetStringValue("FirewallRule", "1"))
	require.NoError(t, installer.Close())

	product, _, err := registry.CreateKey(root, productKeyPath, registry.ALL_ACCESS)
	require.NoError(t, err)
	require.NoError(t, product.SetStringValue("InstallFolder", `C:\Program Files\Jacklet`))
	require.NoError(t, product.Close())

	require.NoError(t, removeProductRegistry(root))
	for _, path := range []string{bootstrapKeyPath, settingsKeyPath, installerKeyPath, productKeyPath} {
		_, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
		require.ErrorIs(t, err, registry.ErrNotExist, "%s was left behind", path)
	}
	require.NoError(t, removeProductRegistry(root), "removal stays repeatable when every key is already gone")
}

func TestRemoveProductRegistry_ReportsAnUnexpectedSubkey(t *testing.T) {
	root := testRegistryRoot(t)
	unexpectedPath := settingsKeyPath + `\Unexpected`
	unexpected, _, err := registry.CreateKey(root, unexpectedPath, registry.ALL_ACCESS)
	require.NoError(t, err)
	require.NoError(t, unexpected.Close())
	t.Cleanup(func() { _ = registry.DeleteKey(root, unexpectedPath) })

	err = removeProductRegistry(root)
	require.ErrorContains(t, err, settingsKeyPath)
}

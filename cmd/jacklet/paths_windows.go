// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
)

// programDataDirName is the directory Jacklet keeps its state in, under
// the machine-wide application data directory.
const programDataDirName = "Jacklet"

// stateDir returns the machine-wide directory Jacklet's state belongs in,
// %PROGRAMDATA%\Jacklet.
func stateDir() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, programDataDirName)
}

// applyServiceSettings puts the settings an installed service was
// configured with into the process environment, for anything the
// environment does not already carry.
//
// Doing it here, before the configuration is parsed, is what keeps one
// order of precedence on every platform: a flag still beats the
// environment, the environment still beats these, and these beat the
// built-in defaults. Nothing downstream needs to know the registry
// exists.
func applyServiceSettings(logger *slog.Logger) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return
	}

	config, err := readServiceSettings(registry.LOCAL_MACHINE)
	if err != nil {
		logger.Warn("could not read the configured settings", "error", err)
		return
	}

	for _, entry := range config {
		name, value, ok := splitSetting(entry)
		if !ok {
			continue
		}
		setting, found := findServiceSetting(name)
		if !found {
			continue
		}
		environmentName := envPrefix + setting.Environment
		if _, set := os.LookupEnv(environmentName); set {
			continue
		}
		if err := os.Setenv(environmentName, value); err != nil {
			logger.Warn("could not apply a configured setting", "setting", name, "error", err)
		}
	}
}

// resolveServicePaths anchors relative paths to the state directory when
// running as a service.
//
// The log file is anchored with the rest, and this runs before the log is
// opened: a relative path resolved against a service's working directory
// would put it in the system directory, and failing to create it there
// ends the process before there is anywhere to report why.
//
// A service's working directory is the system directory, so a relative
// default like "jacklet.db" would put the database in C:\Windows\System32
// — writable by some service accounts and not others, and in neither case
// where anyone would look for it. The installed configuration names
// absolute paths; this covers a configuration that has lost one.
func resolveServicePaths(cfg *settings) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return
	}

	for _, path := range []*string{&cfg.ConfigDir, &cfg.DBPath, &cfg.DefinitionsDir, &cfg.LogFile} {
		if *path != "" && !filepath.IsAbs(*path) {
			*path = filepath.Join(stateDir(), *path)
		}
	}
}

// stateSubdirs are the directories the service writes its state into.
var stateSubdirs = []string{"config", "definitions", "logs"}

// prepareStateDir creates the service's state directory and grants the
// service account access to it.
//
// The service runs as an unprivileged built-in account, which has no
// access to a directory created by an administrator; without the grant the
// service would start and then fail on its first write. Access is granted
// only here, under the state directory, and never to the service's own
// registry key, where it would let the service rewrite its own ImagePath.
func prepareStateDir() error {
	root := stateDir()
	for _, dir := range append([]string{root}, stateSubdirsUnder(root)...) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return grantServiceAccountAccess(root)
}

// stateSubdirsUnder returns the state subdirectories under root.
func stateSubdirsUnder(root string) []string {
	dirs := make([]string, 0, len(stateSubdirs))
	for _, name := range stateSubdirs {
		dirs = append(dirs, filepath.Join(root, name))
	}
	return dirs
}

// grantServiceAccountAccess adds an inheritable grant for the service
// account to path, leaving the inherited entries in place.
func grantServiceAccountAccess(path string) error {
	account, err := windows.CreateWellKnownSid(windows.WinLocalServiceSid)
	if err != nil {
		return fmt.Errorf("resolving the %s account: %w", serviceAccount, err)
	}

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the permissions on %s: %w", path, err)
	}
	existing, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("reading the permissions on %s: %w", path, err)
	}

	// Merged into the existing entries rather than replacing them, so
	// administrators keep the access they inherit.
	updated, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessMode:        windows.GRANT_ACCESS,
		AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.DELETE,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(account),
		},
	}}, existing)
	if err != nil {
		return fmt.Errorf("building the permissions for %s: %w", path, err)
	}

	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, updated, nil); err != nil {
		return fmt.Errorf("granting %s access to %s: %w", serviceAccount, path, err)
	}
	return nil
}

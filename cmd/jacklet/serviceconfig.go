// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// serviceConfiguration is a service's configured settings, as a sorted list
// of NAME=value strings.
//
// The names are the PascalCase values stored in the registry. They are
// translated to JACKLET_* environment names only at the startup boundary.
type serviceConfiguration []string

type serviceSetting struct {
	Environment string
	IsSecret    bool
	Name        string
}

var serviceSettingSchema = []serviceSetting{
	{Environment: "ADMIN_PASSWORD", IsSecret: true, Name: "AdminPassword"},
	{Environment: "ADMIN_PASSWORD_HASH", IsSecret: true, Name: "AdminPasswordHash"},
	{Environment: "API_KEY", IsSecret: true, Name: "ApiKey"},
	{Environment: "BASE_URL", Name: "BaseUrl"},
	{Environment: "CONFIG_DIR", Name: "ConfigDir"},
	{Environment: "CONTACT_EMAIL", Name: "ContactEmail"},
	{Environment: "DB_PATH", Name: "DatabasePath"},
	{Environment: "DEFINITIONS_DIR", Name: "DefinitionsDir"},
	{Environment: "FLARESOLVERR_URL", Name: "FlareSolverrUrl"},
	{Environment: "LOG_FILE", Name: "LogFile"},
	{Environment: "PORT", Name: "Port"},
	{Environment: "RETENTION_DAYS", Name: "RetentionDays"},
}

func findServiceSetting(name string) (serviceSetting, bool) {
	for _, setting := range serviceSettingSchema {
		if strings.EqualFold(setting.Name, name) {
			return setting, true
		}
	}
	return serviceSetting{}, false
}

// splitSetting splits a NAME=value entry. An entry with no "=" has no
// value to carry and is reported as malformed rather than silently read as
// a name with an empty value.
func splitSetting(entry string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(entry, "=")
	if !ok || name == "" {
		return "", "", false
	}
	return name, value, true
}

// set returns the configuration with name bound to value, replacing any
// existing binding. Entries stay sorted by name, so the stored value does
// not churn when a setting is rewritten.
func (e serviceConfiguration) set(name, value string) serviceConfiguration {
	updated := append(e.unset(name), name+"="+value)
	slices.Sort(updated)
	return updated
}

// unset returns the configuration without name.
func (e serviceConfiguration) unset(name string) serviceConfiguration {
	remaining := make(serviceConfiguration, 0, len(e))
	for _, entry := range e {
		if existing, _, ok := splitSetting(entry); ok && existing == name {
			continue
		}
		remaining = append(remaining, entry)
	}
	return remaining
}

// lookup returns the value bound to name.
func (e serviceConfiguration) lookup(name string) (string, bool) {
	for _, entry := range e {
		if existing, value, ok := splitSetting(entry); ok && existing == name {
			return value, true
		}
	}
	return "", false
}

// String renders the configuration for display, one setting per line, with
// every credential's value masked.
func (e serviceConfiguration) String() string {
	var out strings.Builder
	for _, entry := range e {
		name, value, ok := splitSetting(entry)
		if !ok {
			// Printed as it stands, in place of the setting it was meant
			// to be: this is the one view of the configuration there is.
			fmt.Fprintf(&out, "%s\t(malformed; expected NAME=value)\n", entry)
			continue
		}
		setting, found := findServiceSetting(name)
		if found && setting.IsSecret && value != "" {
			value = "********"
		}
		fmt.Fprintf(&out, "%s=%s\n", name, value)
	}
	return out.String()
}

// settingName validates the name of a setting an operator asked to change.
//
// Only names in the registry schema are editable, and the canonical spelling
// is returned so the registry stays stable despite its case-insensitive names.
func settingName(name string) (string, error) {
	if name == "" {
		return "", errors.New("no setting was named")
	}
	if strings.ContainsAny(name, "= \t") {
		return "", fmt.Errorf("invalid setting name %q: expected a bare name such as Port", name)
	}
	setting, ok := findServiceSetting(name)
	if !ok {
		return "", fmt.Errorf("unknown setting %q", name)
	}
	return setting.Name, nil
}

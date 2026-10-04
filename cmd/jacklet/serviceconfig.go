// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/scraper"
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
	// Validate checks a value as `jacklet service config set` is given it,
	// so a typo is refused while the operator is still at the prompt. It
	// runs the check startup runs, and is never handed an empty value,
	// which leaves the setting at its default. Nil accepts any value.
	Validate func(value string) error
}

var serviceSettingSchema = []serviceSetting{
	{Environment: "ADMIN_PASSWORD", IsSecret: true, Name: "AdminPassword"},
	{Environment: "ADMIN_PASSWORD_HASH", IsSecret: true, Name: "AdminPasswordHash", Validate: checkedBy(adminPasswordHash)},
	{Environment: "ADMIN_PREFIX", Name: "AdminPrefix", Validate: checkedBy(admin.ParsePrefix)},
	{Environment: "API_KEY", IsSecret: true, Name: "ApiKey"},
	{Environment: "BASE_URL", Name: "BaseUrl", Validate: checkedBy(publicBaseURLValue)},
	{Environment: "CONFIG_DIR", Name: "ConfigDir"},
	{Environment: "CONTACT_EMAIL", Name: "ContactEmail", Validate: checkedBy(contactAddress)},
	{Environment: "DB_PATH", Name: "DatabasePath"},
	{Environment: "DEFINITIONS_DIR", Name: "DefinitionsDir"},
	{Environment: "FLARESOLVERR_SESSIONS", Name: "FlareSolverrSessions", Validate: checkedBy(flareSolverrSessionLimit)},
	{Environment: "FLARESOLVERR_URL", Name: "FlareSolverrUrl", Validate: checkedBy(scraper.FlareSolverrEndpoint)},
	{Environment: "LOG_FILE", Name: "LogFile"},
	{Environment: "LOG_LEVEL", Name: "LogLevel", Validate: checkedBy(logLevel)},
	{Environment: "MAGNET_TRACKERS", Name: "MagnetTrackers", Validate: checkedBy(magnetTrackerList)},
	{Environment: "PORT", Name: "Port", Validate: checkedBy(listenPort)},
	{Environment: "RETENTION_DAYS", Name: "RetentionDays", Validate: checkedBy(retentionPeriod)},
	{Environment: "TRUSTED_PROXIES", Name: "TrustedProxies", Validate: checkedBy(trustedProxyList)},
	{Environment: "USER_AGENT", Name: "UserAgent", Validate: checkedBy(userAgentValue)},
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

// checkedBy adapts a startup parser to a serviceSetting's Validate,
// keeping its error and discarding what it parsed.
func checkedBy[T any](parse func(string) (T, error)) func(string) error {
	return func(value string) error {
		_, err := parse(value)
		return err
	}
}

// adminPasswordHash reads ADMIN_PASSWORD_HASH as startup does, reporting a
// hash `jacklet hash-password` could not have produced.
func adminPasswordHash(hash string) (*admin.Password, error) {
	password, err := admin.NewPassword("", hash)
	if err != nil {
		return nil, fmt.Errorf("invalid admin password hash: %w", err)
	}
	return password, nil
}

// publicBaseURLValue is publicBaseURL with only the URL kept, the shape
// checkedBy takes.
func publicBaseURLValue(value string) (string, error) {
	baseURL, _, err := publicBaseURL(value)
	return baseURL, err
}

// validateSettingValue checks value against the schema entry of the setting
// name, which is canonical, as settingName returns it. An empty value is
// always accepted: it leaves the setting at its default.
func validateSettingValue(name, value string) error {
	setting, ok := findServiceSetting(name)
	if !ok || setting.Validate == nil || value == "" {
		return nil
	}
	return setting.Validate(value)
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

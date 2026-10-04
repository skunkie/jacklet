// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"flag"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
)

func TestServiceConfiguration_Set_AddsAndReplaces(t *testing.T) {
	env := serviceConfiguration{"Port=9117"}

	env = env.set("DatabasePath", `C:\ProgramData\Jacklet\jacklet.db`)
	require.Equal(t, serviceConfiguration{
		`DatabasePath=C:\ProgramData\Jacklet\jacklet.db`,
		"Port=9117",
	}, env, "entries stay sorted by name")

	env = env.set("Port", "9118")
	require.Equal(t, serviceConfiguration{
		`DatabasePath=C:\ProgramData\Jacklet\jacklet.db`,
		"Port=9118",
	}, env, "setting an existing name replaces it instead of adding a duplicate")
}

func TestServiceConfiguration_Set_KeepsAValueContainingEquals(t *testing.T) {
	env := serviceConfiguration{}.set("BaseUrl", "https://example.test/?a=b")

	value, ok := env.lookup("BaseUrl")
	require.True(t, ok)
	require.Equal(t, "https://example.test/?a=b", value, "only the first = separates the name")
}

func TestServiceConfiguration_Unset(t *testing.T) {
	env := serviceConfiguration{"Port=9117", "ApiKey=secret"}

	env = env.unset("ApiKey")
	require.Equal(t, serviceConfiguration{"Port=9117"}, env)

	require.Equal(t, env, env.unset("Missing"), "unsetting an absent name changes nothing")
}

func TestServiceConfiguration_Lookup(t *testing.T) {
	env := serviceConfiguration{"Port=9117", "BaseUrl="}

	value, ok := env.lookup("Port")
	require.True(t, ok)
	require.Equal(t, "9117", value)

	value, ok = env.lookup("BaseUrl")
	require.True(t, ok, "a name bound to an empty value is still bound")
	require.Empty(t, value)

	_, ok = env.lookup("Missing")
	require.False(t, ok)
}

func TestServiceConfiguration_String(t *testing.T) {
	t.Run("masks credentials", func(t *testing.T) {
		env := serviceConfiguration{
			"AdminPassword=hunter2",
			"AdminPasswordHash=pbkdf2$1$abc",
			"ApiKey=0123456789abcdef",
			"Port=9117",
		}

		printed := env.String()
		require.NotContains(t, printed, "hunter2")
		require.NotContains(t, printed, "0123456789abcdef")
		require.NotContains(t, printed, "pbkdf2$1$abc")
		require.Contains(t, printed, "ApiKey=********")
		require.Contains(t, printed, "Port=9117", "a setting that is not a credential is shown in full")
	})

	// Entries built by set always carry an "=", so String meets a malformed
	// one only from a configuration assembled by hand. It prints such an
	// entry in full, which is what this pins.
	t.Run("shows a malformed entry", func(t *testing.T) {
		printed := serviceConfiguration{"Port"}.String()
		require.Contains(t, printed, "Port")
		require.Contains(t, printed, "malformed")
	})

	t.Run("does not mask an unset credential", func(t *testing.T) {
		printed := serviceConfiguration{"ApiKey="}.String()
		require.Contains(t, printed, "ApiKey=\n", "masking an empty value would imply one is set")
	})
}

func TestSettingName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "canonical", input: "Port"},
		{name: "case insensitive", input: "port", want: "Port"},
		{name: "empty", input: "", wantErr: "no setting was named"},
		{name: "environment name", input: "JACKLET_PORT", wantErr: "unknown setting"},
		{name: "carries a value", input: "Port=9117", wantErr: "invalid setting name"},
		{name: "carries a space", input: "Port Number", wantErr: "invalid setting name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := settingName(tc.input)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			want := tc.want
			if want == "" {
				want = tc.input
			}
			require.Equal(t, want, got)
		})
	}
}

func TestValidateSettingValue(t *testing.T) {
	hash, err := admin.HashPassword("sample password")
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		setting string
		value   string
		wantErr string
	}{
		{name: "a password hash", setting: "AdminPasswordHash", value: hash},
		{name: "a malformed password hash", setting: "AdminPasswordHash", value: "not-a-hash", wantErr: "invalid admin password hash"},
		// A prefix's form is checked here; a collision with a route the
		// server serves is found only when the service starts.
		{name: "an admin prefix", setting: "AdminPrefix", value: "/ops"},
		{name: "an admin prefix ending in a slash", setting: "AdminPrefix", value: "/ops/", wantErr: "admin prefix"},
		{name: "a base URL", setting: "BaseUrl", value: "https://jacklet.example.com"},
		{name: "a base URL with a path", setting: "BaseUrl", value: "https://example.com/jacklet", wantErr: "invalid base URL"},
		{name: "a contact email", setting: "ContactEmail", value: "ops@example.org"},
		{name: "a contact email with a display name", setting: "ContactEmail", value: "Ops <ops@example.org>", wantErr: "invalid contact email"},
		{name: "a FlareSolverr session limit", setting: "FlareSolverrSessions", value: "4"},
		{name: "a FlareSolverr session limit of zero", setting: "FlareSolverrSessions", value: "0", wantErr: "invalid FlareSolverr sessions"},
		{name: "a FlareSolverr URL", setting: "FlareSolverrUrl", value: "http://localhost:8191"},
		{name: "a FlareSolverr URL without a scheme", setting: "FlareSolverrUrl", value: "localhost:8191", wantErr: "invalid FlareSolverr URL"},
		{name: "a known log level", setting: "LogLevel", value: "debug"},
		{name: "a log level in another case", setting: "LogLevel", value: "WARN"},
		{name: "an unknown log level", setting: "LogLevel", value: "verbose", wantErr: "invalid log level"},
		{name: "magnet trackers", setting: "MagnetTrackers", value: "udp://tracker.example.org:6969/announce"},
		{name: "a magnet tracker with a bad scheme", setting: "MagnetTrackers", value: "ftp://tracker.example.org/announce", wantErr: "invalid magnet tracker"},
		{name: "a port", setting: "Port", value: "9118"},
		{name: "a port out of range", setting: "Port", value: "70000", wantErr: "invalid port"},
		{name: "a retention", setting: "RetentionDays", value: "7"},
		{name: "a negative retention", setting: "RetentionDays", value: "-1", wantErr: "invalid retention days"},
		{name: "trusted proxies", setting: "TrustedProxies", value: "127.0.0.1,10.0.0.0/8"},
		{name: "a trusted proxy that is no address", setting: "TrustedProxies", value: "proxy.example.org", wantErr: "invalid trusted proxy"},
		{name: "a user agent", setting: "UserAgent", value: "Any/1.0"},
		{name: "a user agent with a control character", setting: "UserAgent", value: "Any/1.0\x7f", wantErr: "invalid user agent"},
		{name: "a setting without a check", setting: "DatabasePath", value: "anything at all"},
		// Empty leaves a setting at its default when the service starts,
		// so it passes even where the setting's own check refuses it.
		{name: "an empty log level", setting: "LogLevel", value: ""},
		{name: "an empty retention", setting: "RetentionDays", value: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSettingValue(tc.setting, tc.value)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// The schema is the only thing that decides which settings a Windows
// service can be given, and a setting missing from it fails silently: the
// name is refused as unknown, and a value already in the registry is
// skipped at startup without a warning. So it is checked against the flag
// set rather than maintained beside it and trusted.
func TestServiceSettingSchemaCoversEverySetting(t *testing.T) {
	fs, _, _ := newSettingsFlags(io.Discard)

	configured := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { configured[envName(f.Name)] = true })
	for _, secret := range secretEnvOnly {
		configured[envPrefix+secret.name] = true
	}

	inSchema := map[string]bool{}
	for _, setting := range serviceSettingSchema {
		environment := envPrefix + setting.Environment
		require.True(t, configured[environment],
			"the schema offers %s, which nothing reads", environment)
		require.False(t, inSchema[environment], "%s appears twice", environment)
		inSchema[environment] = true

		canonical, err := settingName(setting.Name)
		require.NoError(t, err, "%s is not resolvable by its own name", setting.Name)
		require.Equal(t, setting.Name, canonical,
			"%s is shadowed by an earlier entry that matches it case-insensitively", setting.Name)
	}

	for environment := range configured {
		require.True(t, inSchema[environment],
			"%s cannot be configured on Windows: it has no entry in serviceSettingSchema", environment)
	}
}

// Credentials are the settings whose values String masks, so an entry
// added without the flag makes one readable on a shared screen.
func TestServiceSettingSchemaMarksEveryCredentialSecret(t *testing.T) {
	for _, secret := range secretEnvOnly {
		index := slices.IndexFunc(serviceSettingSchema, func(s serviceSetting) bool {
			return s.Environment == secret.name
		})
		require.GreaterOrEqual(t, index, 0, "no schema entry carries the %s credential", secret.name)
		require.True(t, serviceSettingSchema[index].IsSecret,
			"%s is a credential but is not masked", serviceSettingSchema[index].Name)
	}
}

// A setting added to the schema without a check would be stored however it
// was mistyped, so each one either has a check or is named here with the
// reason it has none.
func TestServiceSettingSchemaChecksEveryCheckableSetting(t *testing.T) {
	//nolint:gosec // G101: setting names mapped to why each is unchecked, not credentials.
	takenAsGiven := map[string]string{
		"AdminPassword":  "any password is a valid one",
		"ApiKey":         "any key is a valid one",
		"ConfigDir":      "whether a path works depends on the service account",
		"DatabasePath":   "whether a path works depends on the service account",
		"DefinitionsDir": "whether a path works depends on the service account",
		"LogFile":        "whether a path works depends on the service account",
	}
	for _, setting := range serviceSettingSchema {
		_, isTakenAsGiven := takenAsGiven[setting.Name]
		require.NotEqual(t, isTakenAsGiven, setting.Validate != nil,
			"%s must either have a Validate check or be listed as taken as given, not both or neither", setting.Name)
	}
	for name := range takenAsGiven {
		_, found := findServiceSetting(name)
		require.True(t, found, "%s is listed as taken as given but is not in the schema", name)
	}
}

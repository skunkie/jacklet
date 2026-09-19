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
)

func TestServiceConfiguration_SetAddsAndReplaces(t *testing.T) {
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

func TestServiceConfiguration_SetKeepsAValueContainingEquals(t *testing.T) {
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

func TestServiceConfiguration_StringMasksCredentials(t *testing.T) {
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
}

// Entries built by set always carry an "=", so String meets a
// malformed one only from a configuration assembled by hand. It
// prints such an entry in full, which is what this pins.
func TestServiceConfiguration_StringShowsAMalformedEntry(t *testing.T) {
	printed := serviceConfiguration{"Port"}.String()
	require.Contains(t, printed, "Port")
	require.Contains(t, printed, "malformed")
}

func TestServiceConfiguration_StringDoesNotMaskAnUnsetCredential(t *testing.T) {
	printed := serviceConfiguration{"ApiKey="}.String()
	require.Contains(t, printed, "ApiKey=\n", "masking an empty value would imply one is set")
}

func TestSettingName(t *testing.T) {
	for _, tc := range []struct {
		name, input, want, wantErr string
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

// The schema is the only thing that decides which settings a Windows
// service can be given, and a setting missing from it fails silently: the
// name is refused as unknown, and a value already in the registry is
// skipped at startup without a warning. So it is checked against the flag
// set rather than maintained beside it and trusted.
func TestServiceSettingSchema_CoversEverySetting(t *testing.T) {
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
func TestServiceSettingSchema_MarksEveryCredentialSecret(t *testing.T) {
	for _, secret := range secretEnvOnly {
		index := slices.IndexFunc(serviceSettingSchema, func(s serviceSetting) bool {
			return s.Environment == secret.name
		})
		require.GreaterOrEqual(t, index, 0, "no schema entry carries the %s credential", secret.name)
		require.True(t, serviceSettingSchema[index].IsSecret,
			"%s is a credential but is not masked", serviceSettingSchema[index].Name)
	}
}

// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// YAML 1.2 lists "\/" among the escapes a double-quoted scalar may use.
// gopkg.in/yaml.v3 is the only parser in play that refuses it, and four
// definitions in Jackett's own repository are unusable here for no other
// reason.

// The shapes the four real definitions carry, verbatim in spirit:
// a regex argument, a flow sequence, and a selector whose own quotes
// are escaped.
func TestUnmarshalYAML_RepairsTheSlashEscape(t *testing.T) {
	document := []byte(`
id: escaped
name: escaped
links:
  - http://tracker.example/
login:
  method: form
  path: /login
  test:
    selector: "a[onclick=\"return post2url('\/logout', {x: 1});\"]"
search:
  paths:
    - path: /
  rows:
    selector: .row
  fields:
    title:
      selector: a
      filters:
        - name: re_replace
          args: "torrent-category-(\\d+)\/"
        - name: replace
          args: ["N\/A", ""]
`)

	// It is genuinely rejected without the repair, or this test proves
	// nothing.
	var plain Tracker
	require.Error(t, yaml.Unmarshal(document, &plain))

	var def Tracker
	require.NoError(t, unmarshalYAML(document, &def))
	require.Equal(t, "escaped", def.ID)
	require.Equal(t, `a[onclick="return post2url('/logout', {x: 1});"]`, def.Login.Test.Selector)

	filters := def.Search.Fields[0].Field.Filters
	require.Len(t, filters, 2)
	require.Equal(t, `torrent-category-(\d+)/`, filters[0].Args)
	require.Equal(t, []any{"N/A", ""}, filters[1].Args)
}

// TestUnmarshalYAML_LeavesWorkingFilesAlone is the safety property:
// the repair runs only after a plain decode has already failed, so a file
// that parses today keeps parsing exactly as it did.
func TestUnmarshalYAML_LeavesWorkingFilesAlone(t *testing.T) {
	document := []byte(`
id: ordinary
name: ordinary
search:
  fields:
    # A backslash-slash outside a double-quoted scalar is literal text,
    # and must stay that way.
    single:
      text: 'a\/b'
    plain:
      text: a\/b
    escaped:
      text: "a\\/b"
`)

	var direct, viaRepair Tracker
	require.NoError(t, yaml.Unmarshal(document, &direct))
	require.NoError(t, unmarshalYAML(document, &viaRepair))
	require.Equal(t, direct, viaRepair)

	byName := map[string]string{}
	for _, f := range viaRepair.Search.Fields {
		byName[f.Name] = f.Field.Text
	}
	require.Equal(t, `a\/b`, byName["single"])
	require.Equal(t, `a\/b`, byName["plain"])
	require.Equal(t, `a\/b`, byName["escaped"], `"\\/" is an escaped backslash then a slash`)
}

// TestUnmarshalYAML_ReportsTheOriginalError checks a file broken for
// some other reason is reported as written, not as the repair left it.
func TestUnmarshalYAML_ReportsTheOriginalError(t *testing.T) {
	var def Tracker
	err := unmarshalYAML([]byte("id: broken\nname: [unclosed\n"), &def)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "escape", "the error should describe the file, not the repair")
}

func TestRepairSlashEscapes(t *testing.T) {
	for _, tc := range []struct {
		hasChanged     bool
		in, name, want string
	}{
		{
			hasChanged: true,
			in:         `a: "x\/y"`,
			name:       "inside a double-quoted scalar",
			want:       `a: "x/y"`,
		},
		{
			in:   `a: 'x\/y'`,
			name: "a single-quoted scalar is left alone",
			want: `a: 'x\/y'`,
		},
		{
			in:   `a: x\/y`,
			name: "a plain scalar is left alone",
			want: `a: x\/y`,
		},
		{
			in:   `a: 1 # x\/y`,
			name: "a comment is left alone",
			want: `a: 1 # x\/y`,
		},
		{
			hasChanged: true,
			in:         `a: "x\\\/y"`,
			name:       "an escaped backslash is not the start of an escape",
			want:       `a: "x\\/y"`,
		},
		{
			in:   `a: "x\"y" # z\/w`,
			name: "an escaped quote does not end the scalar",
			want: `a: "x\"y" # z\/w`,
		},
		{
			// The case that desynced an earlier version of this scanner:
			// "-" is a YAML indicator only when a blank follows it, so the
			// closing quote of this plain scalar does not open one.
			hasChanged: true,
			in:         "selector: td a[href*=\"/torrent-category-\"]\nargs: \"x\\/y\"",
			name:       "a quote inside a plain scalar does not open a scalar",
			want:       "selector: td a[href*=\"/torrent-category-\"]\nargs: \"x/y\"",
		},
		{
			hasChanged: true,
			in:         `- "x\/y"`,
			name:       "a sequence entry does open one",
			want:       `- "x/y"`,
		},
		{
			hasChanged: true,
			in:         `a: ["x\/y", "z"]`,
			name:       "so does a flow sequence",
			want:       `a: ["x/y", "z"]`,
		},
		{in: "a: b\n", name: "nothing to do", want: "a: b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, hasChanged := repairSlashEscapes([]byte(tc.in))
			require.Equal(t, tc.want, string(got))
			require.Equal(t, tc.hasChanged, hasChanged)
		})
	}
}

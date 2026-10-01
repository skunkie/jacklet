// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// Cardigann has exactly two template functions, "join" and "re_replace".
// Jackett applies them by substituting over the template text with a
// regex, so it never parses what a definition writes; Jacklet hands the
// text to Go's template engine, which does — see escapeTemplateLiterals.

func TestTemplateJoin(t *testing.T) {
	require.Equal(t, "1,2,3", templateJoin([]string{"1", "2", "3"}, ","))
	require.Equal(t, "1 OR 2", templateJoin([]string{"1", "2"}, " OR "))
	require.Empty(t, templateJoin(nil, ","))
	require.Empty(t, templateJoin([]string{}, ","))
	// Not a list: joining one value is the value.
	require.Equal(t, "solo", templateJoin("solo", ","))
	require.Equal(t, "1,2", templateJoin([]any{1, 2}, ","))
}

func TestTemplateReReplace(t *testing.T) {
	got, err := templateReReplace("some show name", `[\s]+`, "%")
	require.NoError(t, err)
	require.Equal(t, "some%show%name", got)

	// A capture group, written .NET-style as Jackett definitions do.
	got, err = templateReReplace("abcdef", "(.).*", "$1")
	require.NoError(t, err)
	require.Equal(t, "a", got)

	// An invalid pattern is reported rather than silently sending the
	// tracker a value the definition meant to rewrite.
	_, err = templateReReplace("x", "([", "y")
	require.Error(t, err)
}

func TestRewriteCardigannTemplate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a regex escape is not a Go escape, so it is doubled",
			in:   `{{ re_replace .Keywords "[\s]+" "%" }}`,
			want: `{{ re_replace .Keywords "[\\s]+" "%" }}`,
		},
		{
			name: "a valid Go escape is left alone",
			in:   `{{ re_replace .Keywords "\t" "-" }}`,
			want: `{{ re_replace .Keywords "\t" "-" }}`,
		},
		{
			name: "an already-escaped backslash passes through whole",
			in:   `{{ re_replace .Keywords "a\\b" "-" }}`,
			want: `{{ re_replace .Keywords "a\\b" "-" }}`,
		},
		{
			name: "an escaped quote does not end the literal",
			in:   `{{ re_replace .Keywords "a\"b" "-" }}`,
			want: `{{ re_replace .Keywords "a\"b" "-" }}`,
		},
		{
			name: "text outside an action is untouched",
			in:   `a\sb {{ .Keywords }} c\sd`,
			want: `a\sb {{ .Keywords }} c\sd`,
		},
		{
			name: "a backslash outside a literal but inside an action is untouched",
			in:   `{{ if .Keywords }}a\sb{{ end }}`,
			want: `{{ if .Keywords }}a\sb{{ end }}`,
		},
		{
			name: "several actions are each handled",
			in:   `{{ re_replace .A "\d" "" }}-{{ re_replace .B "\w" "" }}`,
			want: `{{ re_replace .A "\\d" "" }}-{{ re_replace .B "\\w" "" }}`,
		},
		{name: "no action at all", in: `plain \s text`, want: `plain \s text`},
		{
			name: "a hyphenated config key becomes an index lookup",
			in:   `{{ .Config.cat-id }}`,
			want: `{{ (index .Config "cat-id") }}`,
		},
		{
			name: "a hyphenated result key too",
			in:   `{{ .Result.title-raw }}`,
			want: `{{ (index .Result "title-raw") }}`,
		},
		{
			name: "several hyphens in one key",
			in:   `{{ .Config.a-b-c }}`,
			want: `{{ (index .Config "a-b-c") }}`,
		},
		{
			name: "a key without a hyphen is left as a field lookup",
			in:   `{{ .Config.sort }}`,
			want: `{{ .Config.sort }}`,
		},
		{
			name: "rewritten in place inside a larger expression",
			in:   `{{ re_replace .Config.cat-id "_" "" }}`,
			want: `{{ re_replace (index .Config "cat-id") "_" "" }}`,
		},
		{
			name: "a hyphen between actions is ordinary text",
			in:   `{{ .Config.sort }}-{{ .Config.type }}`,
			want: `{{ .Config.sort }}-{{ .Config.type }}`,
		},
		{
			name: "a hyphenated name inside a quoted argument is not rewritten",
			in:   `{{ re_replace .Keywords ".Config.a-b" "" }}`,
			want: `{{ re_replace .Keywords ".Config.a-b" "" }}`,
		},
		{
			name: "only the two context maps are rewritten",
			in:   `{{ .Query.a-b }}`,
			want: `{{ .Query.a-b }}`,
		},
		{
			name: "both rewrites in one action",
			in:   `{{ re_replace .Config.cat-id "[\s]+" "" }}`,
			want: `{{ re_replace (index .Config "cat-id") "[\\s]+" "" }}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, rewriteCardigannTemplate(tc.in))
		})
	}
}

// TestRenderTemplate_HyphenatedSettingNames is the end-to-end check: these
// are the shapes Jackett's own definitions carry, which Go's parser
// rejects as a subtraction before the rewrite.
func TestRenderTemplate_HyphenatedSettingNames(t *testing.T) {
	data := templateData{
		Config:   map[string]any{"cat-id": "42", "filter-verified": cardigannTrue, "sort": "seeders"},
		Keywords: "some show",
		Result:   map[string]string{"title-raw": "Some Release"},
	}

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "a hyphenated setting", in: `{{ .Config.cat-id }}`, want: "42"},
		{name: "a hyphenated result field", in: `{{ .Result.title-raw }}`, want: "Some Release"},
		{name: "as a function argument", in: `{{ re_replace .Config.cat-id "4" "9" }}`, want: "92"},
		{
			name: "as a condition",
			in:   `{{ if .Config.filter-verified }}yes{{ else }}no{{ end }}`,
			want: "yes",
		},
		{
			name: "the shape a real definition carries",
			in:   `search-{{ if .Keywords }}{{ .Keywords }}{{ else }}{{ .Today.Year }}{{ end }}-{{ .Config.cat-id }}-{{ .Config.sort }}-1.html`,
			want: "search-some show-42-seeders-1.html",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, renderTemplate(tc.in, data, slog.New(slog.DiscardHandler)))
		})
	}
}

// TestRenderTemplate_CardigannFunctions is the end-to-end check: these are
// the exact expressions Jackett's own definitions carry.
func TestRenderTemplate_CardigannFunctions(t *testing.T) {
	data := templateData{
		Categories: []string{"12", "34"},
		Config:     map[string]any{"sort": "seeders_desc"},
		Keywords:   "some show name",
	}

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "join", in: `{{ join .Categories "," }}`, want: "12,34"},
		{name: "join with a longer separator", in: `{{ join .Categories " OR " }}`, want: "12 OR 34"},
		{name: "re_replace with a regex escape", in: `{{ re_replace .Keywords "[\s]+" "%" }}`, want: "some%show%name"},
		{name: "re_replace with a capture group", in: `{{ re_replace .Keywords "(.).*" "$1" }}`, want: "s"},
		{name: "re_replace over a config value", in: `{{ re_replace .Config.sort "_" "" }}`, want: "seedersdesc"},
		{
			name: "composed with the rest of the template",
			in:   `search/{{ if .Keywords }}{{ re_replace .Keywords "\s+" "-" }}{{ else }}all{{ end }}`,
			want: "search/some-show-name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, renderTemplate(tc.in, data, slog.New(slog.DiscardHandler)))
		})
	}
}

// TestScraperCardigannFunctionsReachTheTracker runs them through a real
// scrape, which is where a template that fails to parse would show itself:
// renderTemplate returns the string unresolved, sending the tracker the
// literal "{{ ... }}".
func TestScraperCardigannFunctionsReachTheTracker(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "funcs-tracker", server.URL, `    cats: "{{ join .Categories \",\" }}"
    nm: "{{ re_replace .Keywords \"[\\s]+\" \"%\" }}"`)
	def.Caps.CategoryMappings = []CategoryMapping{{Cat: "Movies", ID: "77"}}

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def,
		SearchParams{Categories: []string{"2000"}, Query: "some show", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "77", got[0].Get("cats"))
	require.Equal(t, "some%show", got[0].Get("nm"))
}

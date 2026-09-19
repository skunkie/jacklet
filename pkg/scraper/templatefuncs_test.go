// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
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
	for _, tc := range []struct{ in, name, want string }{
		{
			in:   `{{ re_replace .Keywords "[\s]+" "%" }}`,
			name: "a regex escape is not a Go escape, so it is doubled",
			want: `{{ re_replace .Keywords "[\\s]+" "%" }}`,
		},
		{
			in:   `{{ re_replace .Keywords "\t" "-" }}`,
			name: "a valid Go escape is left alone",
			want: `{{ re_replace .Keywords "\t" "-" }}`,
		},
		{
			in:   `{{ re_replace .Keywords "a\\b" "-" }}`,
			name: "an already-escaped backslash passes through whole",
			want: `{{ re_replace .Keywords "a\\b" "-" }}`,
		},
		{
			in:   `{{ re_replace .Keywords "a\"b" "-" }}`,
			name: "an escaped quote does not end the literal",
			want: `{{ re_replace .Keywords "a\"b" "-" }}`,
		},
		{
			in:   `a\sb {{ .Keywords }} c\sd`,
			name: "text outside an action is untouched",
			want: `a\sb {{ .Keywords }} c\sd`,
		},
		{
			in:   `{{ if .Keywords }}a\sb{{ end }}`,
			name: "a backslash outside a literal but inside an action is untouched",
			want: `{{ if .Keywords }}a\sb{{ end }}`,
		},
		{
			in:   `{{ re_replace .A "\d" "" }}-{{ re_replace .B "\w" "" }}`,
			name: "several actions are each handled",
			want: `{{ re_replace .A "\\d" "" }}-{{ re_replace .B "\\w" "" }}`,
		},
		{in: `plain \s text`, name: "no action at all", want: `plain \s text`},
		{
			in:   `{{ .Config.cat-id }}`,
			name: "a hyphenated config key becomes an index lookup",
			want: `{{ (index .Config "cat-id") }}`,
		},
		{
			in:   `{{ .Result.title-raw }}`,
			name: "a hyphenated result key too",
			want: `{{ (index .Result "title-raw") }}`,
		},
		{
			in:   `{{ .Config.a-b-c }}`,
			name: "several hyphens in one key",
			want: `{{ (index .Config "a-b-c") }}`,
		},
		{
			in:   `{{ .Config.sort }}`,
			name: "a key without a hyphen is left as a field lookup",
			want: `{{ .Config.sort }}`,
		},
		{
			in:   `{{ re_replace .Config.cat-id "_" "" }}`,
			name: "rewritten in place inside a larger expression",
			want: `{{ re_replace (index .Config "cat-id") "_" "" }}`,
		},
		{
			in:   `{{ .Config.sort }}-{{ .Config.type }}`,
			name: "a hyphen between actions is ordinary text",
			want: `{{ .Config.sort }}-{{ .Config.type }}`,
		},
		{
			in:   `{{ re_replace .Keywords ".Config.a-b" "" }}`,
			name: "a hyphenated name inside a quoted argument is not rewritten",
			want: `{{ re_replace .Keywords ".Config.a-b" "" }}`,
		},
		{
			in:   `{{ .Query.a-b }}`,
			name: "only the two context maps are rewritten",
			want: `{{ .Query.a-b }}`,
		},
		{
			in:   `{{ re_replace .Config.cat-id "[\s]+" "" }}`,
			name: "both rewrites in one action",
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

	for _, tc := range []struct{ in, name, want string }{
		{in: `{{ .Config.cat-id }}`, name: "a hyphenated setting", want: "42"},
		{in: `{{ .Result.title-raw }}`, name: "a hyphenated result field", want: "Some Release"},
		{in: `{{ re_replace .Config.cat-id "4" "9" }}`, name: "as a function argument", want: "92"},
		{
			in:   `{{ if .Config.filter-verified }}yes{{ else }}no{{ end }}`,
			name: "as a condition",
			want: "yes",
		},
		{
			in:   `search-{{ if .Keywords }}{{ .Keywords }}{{ else }}{{ .Today.Year }}{{ end }}-{{ .Config.cat-id }}-{{ .Config.sort }}-1.html`,
			name: "the shape a real definition carries",
			want: "search-some show-42-seeders-1.html",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, renderTemplate(tc.in, data, testLogger()))
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

	for _, tc := range []struct{ in, name, want string }{
		{in: `{{ join .Categories "," }}`, name: "join", want: "12,34"},
		{in: `{{ join .Categories " OR " }}`, name: "join with a longer separator", want: "12 OR 34"},
		{in: `{{ re_replace .Keywords "[\s]+" "%" }}`, name: "re_replace with a regex escape", want: "some%show%name"},
		{in: `{{ re_replace .Keywords "(.).*" "$1" }}`, name: "re_replace with a capture group", want: "s"},
		{in: `{{ re_replace .Config.sort "_" "" }}`, name: "re_replace over a config value", want: "seedersdesc"},
		{
			in:   `search/{{ if .Keywords }}{{ re_replace .Keywords "\s+" "-" }}{{ else }}all{{ end }}`,
			name: "composed with the rest of the template",
			want: "search/some-show-name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, renderTemplate(tc.in, data, testLogger()))
		})
	}
}

// TestScraper_CardigannFunctionsReachTheTracker runs them through a real
// scrape, which is where a template that fails to parse would show itself:
// renderTemplate returns the string unresolved, sending the tracker the
// literal "{{ ... }}".
func TestScraper_CardigannFunctionsReachTheTracker(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "funcs-tracker", server.URL, `    cats: "{{ join .Categories \",\" }}"
    nm: "{{ re_replace .Keywords \"[\\s]+\" \"%\" }}"`)
	def.Caps.CategoryMappings = []CategoryMapping{{Cat: "Movies", ID: "77"}}

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Categories: []string{"2000"}, Query: "some show", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "77", got[0].Get("cats"))
	require.Equal(t, "some%show", got[0].Get("nm"))
}

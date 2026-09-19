// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These cover the base template variables Jackett supplies to every
// definition — ".Config.sitelink", ".True"/".False" and ".Today.Year" —
// which real Cardigann definitions lean on far more than they do on the
// search parameters. A definition referencing one Jacklet does not provide
// does not fail loudly: renderTemplate logs at debug and returns the
// string unresolved, so the tracker is sent the literal template text.

// TestScraper_SiteLinkIsTheMirrorInUse covers ".Config.sitelink", which
// definitions use to build absolute URLs. It must name the mirror that
// answered, not the definition's first link, which after a failover is the
// host that just failed.
func TestScraper_SiteLinkIsTheMirrorInUse(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "sitelink-tracker", server.URL,
		`    link: "{{ .Config.sitelink }}browse.php"`)
	// A dead primary, so the scrape fails over and the live mirror is the
	// second candidate rather than Links[0].
	def.Links = append([]string{"http://127.0.0.1:1/"}, def.Links...)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, server.URL+"/browse.php", got[0].Get("link"))
}

// TestScraper_SiteLinkYieldsToADefinitionsOwnSetting checks an operator's
// configured value wins: a definition declaring "sitelink" as a setting
// means it to be configurable.
func TestScraper_SiteLinkYieldsToADefinitionsOwnSetting(t *testing.T) {
	base, err := url.Parse("http://mirror.example/")
	require.NoError(t, err)

	own := withSiteLink(map[string]any{siteLinkSetting: "http://configured.example/"}, base)
	require.Equal(t, "http://configured.example/", own[siteLinkSetting])

	// An empty one is a default nobody filled in, so Cardigann's value
	// still applies.
	empty := withSiteLink(map[string]any{siteLinkSetting: ""}, base)
	require.Equal(t, "http://mirror.example/", empty[siteLinkSetting])

	// The resolved config is shared across a definition's mirrors, so it
	// must not be written through.
	shared := map[string]any{"other": "value"}
	withSiteLink(shared, base)
	require.NotContains(t, shared, siteLinkSetting)
}

// TestScraper_TrueAndFalse covers Cardigann's boolean literals. The shape
// that matters is "eq <something> .False", which is how definitions test a
// parameter or a checkbox for being unset.
func TestScraper_TrueAndFalse(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "bool-tracker", server.URL, `    t: "{{ .True }}"
    f: "[{{ .False }}]"
    unset: "{{ if eq .Query.IMDBID .False }}yes{{ else }}no{{ end }}"
    set: "{{ if eq .Query.TMDBID .False }}yes{{ else }}no{{ end }}"
    checkbox: "{{ if eq .Config.freeleech .False }}off{{ else }}on{{ end }}"`)
	// A checkbox setting, which must compare against .False rather than
	// blowing the whole expression up as a Go bool would.
	def.Settings = []Setting{{Default: false, Name: "freeleech", Type: "checkbox"}}

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", TMDBID: "550", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "True", got[0].Get("t"))
	require.Equal(t, "[]", got[0].Get("f"))
	require.Equal(t, "yes", got[0].Get("unset"))
	require.Equal(t, "no", got[0].Get("set"))
	require.Equal(t, "off", got[0].Get("checkbox"))
}

// TestScraper_CheckboxStillReadsAsABoolean guards the reason the old
// representation existed: "{{ if .Config.x }}" has to keep working now
// that a checkbox is a string.
func TestScraper_CheckboxStillReadsAsABoolean(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "checkbox-tracker", server.URL,
		`    on: "{{ if .Config.on }}yes{{ else }}no{{ end }}"
    off: "{{ if .Config.off }}yes{{ else }}no{{ end }}"`)
	def.Settings = []Setting{
		{Default: true, Name: "on", Type: "checkbox"},
		{Default: false, Name: "off", Type: "checkbox"},
	}

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "yes", got[0].Get("on"))
	require.Equal(t, "no", got[0].Get("off"))
}

func TestNewTodayVars(t *testing.T) {
	// Outside January, the current year.
	june := newTodayVars(time.Date(2024, time.June, 15, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "2024", june.Year)
	require.Equal(t, "2024-06-15", june.String())

	// In January it is the previous year, as it is in Jackett: a listing
	// whose printed date carries no year is far likelier to be from last
	// December than from the last few days.
	january := newTodayVars(time.Date(2024, time.January, 3, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "2023", january.Year)
	require.Equal(t, "2024-01-03", january.String(), "the date itself is not shifted")
}

// TestScraper_TodayRendersBothForms checks ".Today.Year" resolves without
// costing the bare ".Today" that Jacklet already offered.
func TestScraper_TodayRendersBothForms(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "today-tracker", server.URL, `    year: "{{ .Today.Year }}"
    date: "{{ .Today }}"`)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))

	now := time.Now()
	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, newTodayVars(now).Year, got[0].Get("year"))
	require.Equal(t, now.Format("2006-01-02"), got[0].Get("date"))
}

func TestApplyFilter_CaseSpellings(t *testing.T) {
	logger := testLogger()
	// Cardigann accepts both spellings of each, and real definitions use
	// both.
	for _, name := range []string{"tolower", "tolowercase"} {
		got, err := applyFilter("MiXeD", Filter{Name: name}, templateData{}, logger)
		require.NoError(t, err, name)
		require.Equal(t, "mixed", got, name)
	}
	for _, name := range []string{"toupper", "touppercase"} {
		got, err := applyFilter("MiXeD", Filter{Name: name}, templateData{}, logger)
		require.NoError(t, err, name)
		require.Equal(t, "MIXED", got, name)
	}
}

func TestApplyFilter_ValidFilename(t *testing.T) {
	logger := testLogger()
	for _, tc := range []struct{ in, name, want string }{
		{in: "Some.Release-2024", name: "leaves a valid name alone", want: "Some.Release-2024"},
		{in: `a/b\c:d*e?f"g<h>i|j`, name: "replaces the reserved characters", want: "a_b_c_d_e_f_g_h_i_j"},
		{in: "a\tb\nc", name: "replaces control characters", want: "a_b_c"},
		{in: "Тест", name: "keeps non-ASCII", want: "Тест"},
		{in: "/", name: "a name left empty becomes an underscore", want: "_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyFilter(tc.in, Filter{Name: "validfilename"}, templateData{}, logger)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

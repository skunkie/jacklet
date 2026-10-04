// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// A tracker that serves windows-1251 expects its search terms in
// windows-1251 too, in a GET's query string as in a POST's form. Submitting
// UTF-8 does not fail loudly — the site reads the query as mojibake and
// returns its default listing — so this is pinned by asserting the bytes
// actually sent.
func TestScraperEncodesFormInTrackerCharset(t *testing.T) {
	for _, method := range []string{"get", "post"} {
		t.Run(method, func(t *testing.T) {
			var gotSent, gotContentType string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSent = r.URL.RawQuery
				if method == "post" {
					body, _ := io.ReadAll(r.Body)
					gotSent = string(body)
				}
				gotContentType = r.Header.Get("Content-Type")
				fmt.Fprint(w, `<div class="row"><a href="/d/1">Result</a></div>`)
			}))
			defer server.Close()

			dir := t.TempDir()
			def := loadTestTracker(t, dir, "cp1251", `
id: cp1251
name: CP1251 Site
encoding: windows-1251
links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
      method: `+method+`
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

			db := &fakeStore{}

			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: db})
			require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "тестовый"}))

			// "тестовый" in windows-1251 is 8 single bytes.
			require.Contains(t, gotSent, "nm=%F2%E5%F1%F2%EE%E2%FB%E9")
			require.NotContains(t, gotSent, "%D1%82", "the term was submitted as UTF-8")
			if method == "post" {
				require.Contains(t, gotContentType, "charset=windows-1251")
			}
		})
	}
}

// A definition with no declared encoding, or an explicit UTF-8 one, must
// still submit UTF-8.
func TestScraperDefaultsToUTF8Form(t *testing.T) {
	for _, encoding := range []string{"", "utf-8", "UTF-8"} {
		for _, method := range []string{"get", "post"} {
			t.Run("encoding="+encoding+"/"+method, func(t *testing.T) {
				var gotSent string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotSent = r.URL.RawQuery
					if method == "post" {
						body, _ := io.ReadAll(r.Body)
						gotSent = string(body)
					}
					fmt.Fprint(w, `<div class="row"><a href="/d/1">Result</a></div>`)
				}))
				defer server.Close()

				dir := t.TempDir()
				encodingLine := ""
				if encoding != "" {
					encodingLine = "encoding: " + encoding + "\n"
				}
				def := loadTestTracker(t, dir, "utf8site", `
id: utf8site
name: UTF8 Site
`+encodingLine+`links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
      method: `+method+`
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

				db := &fakeStore{}

				scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: db})
				require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "тестовый"}))
				require.Contains(t, gotSent, "nm=%D1%82%D0%B5%D1%81%D1%82%D0%BE%D0%B2%D1%8B%D0%B9")
			})
		}
	}
}

func TestEncodeForm(t *testing.T) {
	form := url.Values{"nm": {"тестовый"}, "z": {"a b"}}

	t.Run("transcodes to the declared charset", func(t *testing.T) {
		got, err := encodeForm(form, "windows-1251")
		require.NoError(t, err)
		require.Equal(t, "nm=%F2%E5%F1%F2%EE%E2%FB%E9&z=a+b", got)
	})

	t.Run("leaves utf-8 alone", func(t *testing.T) {
		got, err := encodeForm(form, "")
		require.NoError(t, err)
		require.Equal(t, form.Encode(), got)
	})

	t.Run("reports an unknown charset", func(t *testing.T) {
		_, err := encodeForm(form, "not-a-charset")
		require.Error(t, err)
	})
}

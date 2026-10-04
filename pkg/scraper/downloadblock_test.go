// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testTorrent is the start of a torrent file: a bencoded dictionary.
const testTorrent = "d8:announce22:http://tracker.example/e"

// downloadTracker loads a definition whose download block is block, linked
// to site, through the same store a real definition goes through.
func downloadTracker(t *testing.T, site *httptest.Server, block string) *Tracker {
	t.Helper()
	return loadTestTracker(t, t.TempDir(), "example", "id: example\nname: Example Tracker\nlinks:\n  - "+site.URL+"/\n"+block)
}

// readDownload returns a download's body, closing it.
func readDownload(t *testing.T, download *Download) string {
	t.Helper()
	require.NotNil(t, download.Body, "the download has no body")
	defer download.Body.Close()
	body, err := io.ReadAll(download.Body)
	require.NoError(t, err)
	return string(body)
}

// A row commonly stores a release's details page, and the block names the
// link on it that serves the torrent file. Fetching the stored link itself
// would hand the client a web page.
func TestScraperDownloadsTheLinkASelectorFinds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/details.php", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "7", r.URL.Query().Get("id"), "the release's page lost its own query")
		w.Write([]byte(`<html><body><a class="other" href="/elsewhere">Other</a><a class="dl" href="dl.php?id=7">Download</a></body></html>`))
	})
	mux.HandleFunc("/dl.php", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "7", r.URL.Query().Get("id"), "the link was not resolved against the details page")
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	def := downloadTracker(t, site, `download:
  selectors:
    - selector: a.missing
      attribute: href
    - selector: a.dl
      attribute: href
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/details.php?id=7")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download), "the torrent file was not served whole")
}

// Jackett tests each selector's link unless the definition says not to, so
// a link that serves something else hands the turn to the next selector.
func TestScraperTriesTheNextSelectorWhenALinkIsNotATorrent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/details.php", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><a class="first" href="/notice">Notice</a><a class="second" href="/file.torrent">Torrent</a></body></html>`))
	})
	mux.HandleFunc("/notice", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("please say thanks first"))
	})
	mux.HandleFunc("/file.torrent", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	const selectors = `  selectors:
    - selector: a.first
      attribute: href
    - selector: a.second
      attribute: href
`
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	t.Run("tested by default", func(t *testing.T) {
		def := downloadTracker(t, site, "download:\n"+selectors)
		download, err := scrpr.Download(t.Context(), def, site.URL+"/details.php")
		require.NoError(t, err)
		require.Equal(t, testTorrent, readDownload(t, download), "a link that is not a torrent file was taken")
	})

	t.Run("untested when the definition says so", func(t *testing.T) {
		def := downloadTracker(t, site, "testlinktorrent: false\ndownload:\n"+selectors)
		download, err := scrpr.Download(t.Context(), def, site.URL+"/details.php")
		require.NoError(t, err)
		require.Equal(t, "please say thanks first", readDownload(t, download), "the first link was tested anyway")
	})

	t.Run("no selector matches", func(t *testing.T) {
		def := downloadTracker(t, site, "download:\n  selectors:\n    - selector: a.missing\n      attribute: href\n")
		_, err := scrpr.Download(t.Context(), def, site.URL+"/details.php")
		require.ErrorContains(t, err, "no download selector matched")
	})
}

// A selector may read a magnet, which is handed on rather than fetched.
func TestScraperDownloadSelectorCanFindAMagnet(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><script>addLinkToDocument("0123456789abcdef0123456789abcdef01234567")</script></body></html>`))
	}))
	defer site.Close()

	def := downloadTracker(t, site, `download:
  selectors:
    - selector: script:contains(addLinkToDocument)
      filters:
        - name: regexp
          args: "addLinkToDocument\\(\"(.*?)\""
        - name: prepend
          args: "magnet:?xt=urn:btih:"
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/details/1")
	require.NoError(t, err)
	require.Equal(t, magnet, download.Magnet)
	require.Nil(t, download.Body, "a magnet came with a body")
}

// The before request runs first, built from the stored link's own parts,
// and a selector may read its answer instead of the release's page.
func TestScraperDownloadRunsTheBeforeRequestFirst(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	record := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/thanks.php", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "42", r.PostForm.Get("torrentid"), "the input was not built from the stored link")
		w.Write([]byte(`<html><body><a href="download.php?id=42&amp;key=secret">Download</a></body></html>`))
	})
	mux.HandleFunc("/download.php", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	def := downloadTracker(t, site, `download:
  before:
    path: thanks.php
    method: post
    inputs:
      torrentid: "{{ .DownloadUri.Query.id }}"
  selectors:
    - selector: a[href^="download.php"]
      attribute: href
      usebeforeresponse: true
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/details.php?id=42")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download))
	require.Equal(t, []string{"POST /thanks.php", "GET /download.php"}, requests,
		"the release's page was fetched although the selector reads the before answer")
}

// A block with only a before request downloads the stored link after it.
func TestScraperDownloadWithOnlyABeforeRequestFetchesTheStoredLink(t *testing.T) {
	var hasThanked atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/ajax.php", func(_ http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "say_thanks", r.URL.Query().Get("action"), "a GET before request's inputs travel in the query")
		hasThanked.Store(true)
	})
	mux.HandleFunc("/download.php", func(w http.ResponseWriter, _ *http.Request) {
		assert.True(t, hasThanked.Load(), "the torrent file was fetched before the before request")
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	def := downloadTracker(t, site, `download:
  before:
    path: ajax.php
    method: get
    inputs:
      action: say_thanks
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download.php?id=1")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download))
}

// A before request's path may be read off the release's page.
func TestScraperDownloadBeforeRequestCanReadItsPath(t *testing.T) {
	var hasThanked atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/viewtopic.php", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("thanks") != "" {
			hasThanked.Store(true)
			return
		}
		w.Write([]byte(`<html><body><ul class="buttons"><li><a href="./viewtopic.php?t=5&amp;thanks=9">Thanks</a></li></ul><a class="dl" href="/file.torrent">Get</a></body></html>`))
	})
	mux.HandleFunc("/file.torrent", func(w http.ResponseWriter, _ *http.Request) {
		assert.True(t, hasThanked.Load(), "the torrent file was fetched before the before request")
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	def := downloadTracker(t, site, `download:
  before:
    pathselector:
      selector: ul.buttons li:last-child a
      attribute: href
      filters:
        - name: re_replace
          args: ["^\\./", ""]
  selectors:
    - selector: a.dl
      attribute: href
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/viewtopic.php?t=5")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download))
}

// An info hash block builds a magnet from what the page shows, and a bare
// ":root" reads the whole answer, which is how an API's JSON is read.
func TestScraperDownloadBuildsAMagnetFromTheInfoHash(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/json_info", r.URL.Path)
		assert.Equal(t, "1234", r.URL.Query().Get("hashes"), "the input was not built from the stored link's path")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result": [{"hash": "0123456789ABCDEF0123456789ABCDEF01234567", "name": "Sample Release"}]}`))
	}))
	defer site.Close()

	def := downloadTracker(t, site, `download:
  before:
    path: api/json_info
    inputs:
      hashes: "{{ re_replace .DownloadUri.AbsolutePath \"/info/\" \"\" }}"
  infohash:
    usebeforeresponse: true
    hash:
      selector: :root
      filters:
        - name: regexp
          args: ([A-F|a-f|0-9]{40})
    title:
      selector: :root
      filters:
        - name: regexp
          args: name\". \"(.+?)\"
`)
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler),
		Options{MagnetTrackers: []string{"udp://tracker.example:6969/announce"}})

	download, err := scrpr.Download(t.Context(), def, site.URL+"/info/1234")
	require.NoError(t, err)
	require.Equal(t, "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&dn=Sample+Release"+
		"&tr="+url.QueryEscape("udp://tracker.example:6969/announce"), download.Magnet)
}

// Every request a download makes carries the tracker's session, so none of
// them may leave the tracker, whichever part of the block names it.
func TestScraperDownloadBlockStaysOnTheTracker(t *testing.T) {
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignHits.Add(1)
		w.Write([]byte(testTorrent))
	}))
	defer foreign.Close()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><a class="dl" href="` + foreign.URL + `/file.torrent">Get</a></body></html>`))
	}))
	defer site.Close()

	tests := []struct {
		name  string
		block string
	}{
		{name: "a selector's link", block: "download:\n  selectors:\n    - selector: a.dl\n      attribute: href\n"},
		{name: "a before request", block: "download:\n  before:\n    path: " + foreign.URL + "/thanks\n"},
		{name: "a before request's read path", block: "download:\n  before:\n    pathselector:\n      selector: a.dl\n      attribute: href\n"},
	}
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := downloadTracker(t, site, tc.block)
			_, err := scrpr.Download(t.Context(), def, site.URL+"/details.php")
			require.ErrorIs(t, err, ErrForeignDownloadURL)
			require.Equal(t, 1, strings.Count(err.Error(), def.Name), "the error does not name the tracker once: %v", err)
		})
	}
	require.Zero(t, foreignHits.Load(), "a request reached a host the tracker does not own")
}

// A selector's link answering with a page is an ordinary miss when a later
// selector serves the file, so it must not cost the tracker its login; a
// download that ends on a page is still read as a lapsed session.
func TestScraperDownloadClearsTheLoginOnlyWhenItEndsOnAPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/details.php", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><a class="first" href="/notice">Notice</a><a class="second" href="/file.torrent">Torrent</a></body></html>`))
	})
	mux.HandleFunc("/notice", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body>Say thanks first</body></html>`))
	})
	mux.HandleFunc("/file.torrent", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	t.Run("a later selector serves the file", func(t *testing.T) {
		def := downloadTracker(t, site, `download:
  selectors:
    - selector: a.first
      attribute: href
    - selector: a.second
      attribute: href
`)
		scrpr.markLoggedIn(TrackerID(def))
		download, err := scrpr.Download(t.Context(), def, site.URL+"/details.php")
		require.NoError(t, err)
		require.Equal(t, testTorrent, readDownload(t, download))
		require.True(t, scrpr.loginIsFresh(TrackerID(def)), "a selector's page cleared a working login")
	})

	t.Run("the download ends on a page", func(t *testing.T) {
		def := &Tracker{ID: "example", Links: []string{site.URL + "/"}, Name: "Example Tracker"}
		scrpr.markLoggedIn(TrackerID(def))
		_, err := scrpr.Download(t.Context(), def, site.URL+"/notice")
		require.ErrorIs(t, err, errDownloadWebPage)
		require.False(t, scrpr.loginIsFresh(TrackerID(def)), "a download answered with a page kept the login")
	})
}

// A tracker may answer a repeated "thanks" with an error status and serve
// the file all the same, so the before request's status does not stop the
// download, as it does not in Jackett.
func TestScraperDownloadGoesOnPastABeforeRequestsErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/thanks.php", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "already thanked", http.StatusForbidden)
	})
	mux.HandleFunc("/download.php", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	def := downloadTracker(t, site, `download:
  before:
    path: thanks.php
    method: post
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download.php?id=1")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download))
}

// Through FlareSolverr, an error status with no body leaves the browser on
// its own error page rather than reporting the status, and the download goes
// on past it all the same.
func TestScraperDownloadGoesOnPastABeforeRequestsBrowserErrorPage(t *testing.T) {
	fake := newFakeFlareSolverr("<html><body>This page isn't working</body></html>")
	fake.solutionURL = "chrome-error://chromewebdata/"
	flare := httptest.NewServer(fake)
	defer flare.Close()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testTorrent))
	}))
	defer site.Close()

	def := downloadTracker(t, site, "download:\n  before:\n    path: thanks.php\n    method: post\n")
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	scrpr.useFlareSolverr(t, flare.URL, TrackerID(def))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download.php?id=1")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download))
}

// A before request the browser could not deliver at all is not an answer
// the tracker gave, so the download fails rather than going on without it.
func TestScraperDownloadStopsAtABeforeRequestTheBrowserCouldNotSend(t *testing.T) {
	fake := newFakeFlareSolverr(browserNetworkErrorPage)
	fake.solutionURL = "chrome-error://chromewebdata/"
	flare := httptest.NewServer(fake)
	defer flare.Close()
	var hasDownloaded atomic.Bool
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hasDownloaded.Store(true)
		w.Write([]byte(testTorrent))
	}))
	defer site.Close()

	def := downloadTracker(t, site, "download:\n  before:\n    path: thanks.php\n    method: post\n")
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	scrpr.useFlareSolverr(t, flare.URL, TrackerID(def))

	_, err := scrpr.Download(t.Context(), def, site.URL+"/download.php?id=1")
	require.ErrorContains(t, err, "ERR_CONNECTION_REFUSED")
	require.False(t, hasDownloaded.Load(), "the torrent was fetched past a before request that never reached the tracker")
}

// A before request built from a link parameter the link lacks is not sent
// with "<no value>" in its place: the download fails, as it does in Jackett,
// whose request cannot be built without the variable either.
func TestScraperDownloadBeforeRequestNeedsItsVariables(t *testing.T) {
	var hasThanked atomic.Bool
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hasThanked.Store(true)
		w.Write([]byte(testTorrent))
	}))
	defer site.Close()

	def := downloadTracker(t, site, `download:
  before:
    path: thanks.php
    method: post
    inputs:
      torrentid: "{{ .DownloadUri.Query.id }}"
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	_, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
	require.ErrorContains(t, err, `input "torrentid"`)
	require.False(t, hasThanked.Load(), "a request was sent without the variable it is built from")
}

// The info hash is read off a page and written into the magnet as it
// stands, so a value that is not a hash is refused rather than adding to
// the link a client is sent to.
func TestScraperDownloadRefusesAnInfoHashThatIsNotOne(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html><body><span class="hash">0123&amp;tr=http://other.example/announce</span><h1>Sample Release</h1></body></html>`))
	}))
	defer site.Close()

	def := downloadTracker(t, site, `download:
  infohash:
    hash:
      selector: span.hash
    title:
      selector: h1
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/details/1")
	require.ErrorContains(t, err, "matched no info hash")
	require.NotContains(t, err.Error(), "other.example", "the page's text reached the error, which is logged and sent to the client")
	require.Nil(t, download)
}

// A before request is commonly answered with a redirect, and the hint for
// a search path left unfollowed names a setting a download block does not
// have, so it is not logged for one.
func TestScraperDownloadBeforeRedirectGivesNoSearchPathHint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/thanks.php", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/details.php", http.StatusFound)
	})
	mux.HandleFunc("/download.php", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testTorrent))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	def := downloadTracker(t, site, "download:\n  before:\n    path: thanks.php\n    method: post\n")
	var logged strings.Builder
	scrpr := New(NewConfigStore(""), "", slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download.php?id=1")
	require.NoError(t, err)
	require.Equal(t, testTorrent, readDownload(t, download))
	require.NotContains(t, logged.String(), "followredirect", "a download's before request was told to set a search path's option")
}

// A tracker may redirect a download link to a magnet, which is the answer
// rather than a host to follow.
func TestScraperDownloadAnswersARedirectToAMagnet(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, magnet, http.StatusFound)
	}))
	defer site.Close()

	def := &Tracker{ID: "example", Links: []string{site.URL + "/"}, Name: "Example Tracker"}
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
	require.NoError(t, err)
	require.Equal(t, magnet, download.Magnet)
}

func TestNewDownloadURIVars(t *testing.T) {
	link, err := url.Parse("https://tracker.example/forum/details.php?id=42&hit=1")
	require.NoError(t, err)

	got := newDownloadURIVars(link)
	require.Equal(t, "/forum/details.php", got.AbsolutePath)
	require.Equal(t, "https://tracker.example/forum/details.php?id=42&hit=1", got.AbsoluteUri)
	require.Equal(t, "tracker.example", got.Host)
	require.Equal(t, "/forum/details.php?id=42&hit=1", got.PathAndQuery)
	require.Equal(t, "443", got.Port, "the scheme's default port was not filled in")
	require.Equal(t, "42", got.Query["id"])
	require.Equal(t, "?hit=1&id=42", got.Query.String())
	require.Equal(t, "https", got.Scheme)

	data := templateData{DownloadUri: got}
	require.Equal(t, "42", renderTemplate("{{ .DownloadUri.Query.id }}", data, slog.New(slog.DiscardHandler)))
	require.Empty(t, newDownloadURIVars(&url.URL{Scheme: "http", Host: "tracker.example"}).Query.String(),
		"a link without a query renders an empty one")
}

func TestWithQuery(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		target  string
		want    string
	}{
		{name: "nothing to add", encoded: "", target: "https://tracker.example/details.php?id=1", want: "https://tracker.example/details.php?id=1"},
		{name: "no query yet", encoded: "q=a", target: "https://tracker.example/search.php", want: "https://tracker.example/search.php?q=a"},
		{name: "appended to a query", encoded: "action=2", target: "https://tracker.example/get.php?id=1", want: "https://tracker.example/get.php?id=1&action=2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, err := url.Parse(tc.target)
			require.NoError(t, err)
			require.Equal(t, tc.want, withQuery(target, tc.encoded).String())
			require.Equal(t, tc.target, target.String(), "the target itself was changed")
		})
	}
}

// keepIfTorrent reads the first byte to judge a body, so the body has to
// stream whole afterwards.
func TestKeepIfTorrent(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "torrent file", body: testTorrent},
		{name: "empty body", body: ""},
		{name: "something else", body: "<html>", wantErr: errNotTorrent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			download := &Download{Body: io.NopCloser(strings.NewReader(tc.body))}
			err := keepIfTorrent(download)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.body, readDownload(t, download))
		})
	}
}

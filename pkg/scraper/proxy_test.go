// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// proxyChildEnv marks the copy of the test binary in which
// TestScraperSendsRequestsThroughEnvironmentProxy makes its scrape. Go reads
// the proxy variables once per process, on the first request, and earlier
// tests in this binary may already have made requests, so the variables
// take effect reliably only in a process started with them.
const proxyChildEnv = "JACKLET_TEST_PROXY_CHILD"

// The Scraper reaches a tracker through the proxy the standard variables
// name, because its clients use Go's default transport, which reads them. A
// client given a transport of its own would stop proxying with no other
// symptom, and an operator whose trackers are reachable only through a proxy
// would see every one of them fail.
func TestScraperSendsRequestsThroughEnvironmentProxy(t *testing.T) {
	if os.Getenv(proxyChildEnv) == "1" {
		scrapeThroughEnvironmentProxy(t)
		return
	}

	var (
		mu           sync.Mutex
		proxiedHosts []string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxiedHosts = append(proxiedHosts, r.Host)
		mu.Unlock()
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Example Release</a></div>`)
	}))
	defer proxy.Close()

	//nolint:gosec // G204: os.Args[0] is this test binary, run again for one test.
	child := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestScraperSendsRequestsThroughEnvironmentProxy$", "-test.count=1")
	// Any proxy variable inherited from the environment that runs the tests
	// is dropped, in either spelling, rather than overridden with an empty
	// value: Windows treats HTTP_PROXY and http_proxy as one variable, so an
	// empty http_proxy appended after HTTP_PROXY would clear the proxy set
	// here.
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
			return true
		default:
			return false
		}
	})
	env = append(env,
		proxyChildEnv+"=1",
		"HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL,
	)
	child.Env = env
	output, err := child.CombinedOutput()
	require.NoError(t, err, "the scrape in the child process failed:\n%s", output)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"tracker.test"}, proxiedHosts,
		"the tracker request did not go through the proxy the environment named")
}

// scrapeThroughEnvironmentProxy scrapes a tracker whose host resolves
// nowhere, so the scrape can only succeed by way of the proxy. A loopback
// test server would not do: Go never proxies a request for a loopback host.
func scrapeThroughEnvironmentProxy(t *testing.T) {
	t.Helper()
	def := loadTestTracker(t, t.TempDir(), "proxied", `
id: proxied
name: Proxied Site
links:
  - http://tracker.test/
search:
  paths:
    - path: "/"
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
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))
	require.Len(t, db.all(), 1, "the row the proxy served was not stored")
}

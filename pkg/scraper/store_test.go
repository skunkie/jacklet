// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDefinition writes a minimal definition named id, padded so that
// successive versions differ in size as well as modification time.
func writeDefinition(t *testing.T, dir, id, name string) {
	t.Helper()
	body := fmt.Sprintf("id: %s\nname: %s\nlinks:\n  - http://example.test/\n", id, name)
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".yml"), []byte(body), 0o600))
}

func trackerNames(trackers []Tracker) []string {
	names := make([]string, len(trackers))
	for i := range trackers {
		names[i] = trackers[i].Name
	}
	return names
}

func TestDefinitionStore_HotReload(t *testing.T) {
	dir := t.TempDir()
	store := NewDefinitionStore(dir, testLogger())

	t.Run("starts empty", func(t *testing.T) {
		trackers, errs, err := store.Trackers()
		require.NoError(t, err)
		require.Empty(t, trackers)
		require.Empty(t, errs)
	})

	t.Run("picks up a new definition", func(t *testing.T) {
		writeDefinition(t, dir, "alpha", "Alpha")
		trackers, _, err := store.Trackers()
		require.NoError(t, err)
		require.Equal(t, []string{"Alpha"}, trackerNames(trackers))
	})

	t.Run("picks up an edit", func(t *testing.T) {
		writeDefinition(t, dir, "alpha", "Alpha Renamed")
		trackers, _, err := store.Trackers()
		require.NoError(t, err)
		require.Equal(t, []string{"Alpha Renamed"}, trackerNames(trackers))
	})

	t.Run("picks up a deletion", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(dir, "alpha.yml")))
		trackers, _, err := store.Trackers()
		require.NoError(t, err)
		require.Empty(t, trackers)
	})
}

// An unchanged file must not be re-read. Making it unreadable after the
// first load proves the second load never touched the disk: an uncached
// store would surface a permission error instead.
func TestDefinitionStore_DoesNotRereadUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beta.yml")
	writeDefinition(t, dir, "beta", "Beta")

	store := NewDefinitionStore(dir, testLogger())
	trackers, errs, err := store.Trackers()
	require.NoError(t, err)
	require.Empty(t, errs)
	require.Equal(t, []string{"Beta"}, trackerNames(trackers))

	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("running with privileges that ignore file permissions")
	}

	trackers, errs, err = store.Trackers()
	require.NoError(t, err)
	require.Empty(t, errs, "an unchanged file should have been served from cache")
	require.Equal(t, []string{"Beta"}, trackerNames(trackers))
}

// A definition that parsed before and is broken now keeps serving its last
// good version, and reports the error too.
func TestDefinitionStore_KeepsLastGoodParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gamma.yml")
	writeDefinition(t, dir, "gamma", "Gamma")

	store := NewDefinitionStore(dir, testLogger())
	trackers, _, err := store.Trackers()
	require.NoError(t, err)
	require.Equal(t, []string{"Gamma"}, trackerNames(trackers))

	require.NoError(t, os.WriteFile(path, []byte("id: gamma\n  name: [unbalanced\n"), 0o600))

	trackers, errs, err := store.Trackers()
	require.NoError(t, err)
	require.Len(t, errs, 1, "the parse failure should be reported")
	require.Contains(t, errs[0].Path, "gamma.yml")
	require.Equal(t, []string{"Gamma"}, trackerNames(trackers), "the last good parse should still be served")

	// Find must see it too, so an indexer stays reachable through a bad edit.
	def, err := store.Find("gamma")
	require.NoError(t, err)
	require.Equal(t, "Gamma", def.Name)

	// Repairing the file takes effect again.
	writeDefinition(t, dir, "gamma", "Gamma Fixed")
	trackers, errs, err = store.Trackers()
	require.NoError(t, err)
	require.Empty(t, errs)
	require.Equal(t, []string{"Gamma Fixed"}, trackerNames(trackers))
}

// A file that never parsed has no last good version to fall back on, so it
// is reported and omitted.
func TestDefinitionStore_ReportsNeverValidDefinition(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.yml"), []byte("\tnot: [valid\n"), 0o600))

	trackers, errs, err := NewDefinitionStore(dir, testLogger()).Trackers()
	require.NoError(t, err)
	require.Empty(t, trackers)
	require.Len(t, errs, 1)
}

// Definitions are supplied by the operator, so an absent directory is the
// ordinary state of a fresh install rather than a fault.
func TestDefinitionStore_MissingDirectoryYieldsNoDefinitions(t *testing.T) {
	store := NewDefinitionStore(filepath.Join(t.TempDir(), "absent"), testLogger())

	trackers, errs, err := store.Trackers()
	require.NoError(t, err)
	require.Empty(t, trackers)
	require.Empty(t, errs)

	_, err = store.Find("anything")
	require.ErrorIs(t, err, ErrTrackerNotFound)
}

// A directory that appears later is picked up without a restart.
func TestDefinitionStore_DirectoryCreatedLater(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "definitions")
	store := NewDefinitionStore(dir, testLogger())

	trackers, _, err := store.Trackers()
	require.NoError(t, err)
	require.Empty(t, trackers)

	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeDefinition(t, dir, "late", "Late Tracker")

	trackers, _, err = store.Trackers()
	require.NoError(t, err)
	require.Equal(t, []string{"Late Tracker"}, trackerNames(trackers))
}

// Find returns a copy, so one request cannot alter what the next sees.
func TestDefinitionStore_FindReturnsACopy(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "delta.yml"), []byte(`
id: delta
name: Delta
links:
  - http://example.test/
caps:
  modes:
    search: [q]
login:
  inputs:
    username: original
search:
  inputs:
    q: original
  paths:
    - path: /search
settings:
  - name: choice
    type: select
    options:
      one: One
`), 0o600))
	store := NewDefinitionStore(dir, testLogger())

	first, err := store.Find("delta")
	require.NoError(t, err)
	first.Name = "Mutated"
	first.Links[0] = "http://mutated.test/"
	first.Caps.Modes["search"][0] = "mutated"
	first.Login.Inputs["username"] = "mutated"
	first.Search.Inputs["q"] = "mutated"
	first.Settings[0].Options["one"] = "Mutated"

	listed, errs, err := store.Trackers()
	require.NoError(t, err)
	require.Empty(t, errs)
	require.Len(t, listed, 1)
	listed[0].Search.Paths[0].Path = "/mutated"

	second, err := store.Find("delta")
	require.NoError(t, err)
	require.Equal(t, "Delta", second.Name)
	require.Equal(t, []string{"http://example.test/"}, second.Links)
	require.Equal(t, []string{"q"}, second.Caps.Modes["search"])
	require.Equal(t, "original", second.Login.Inputs["username"])
	require.Equal(t, "original", second.Search.Inputs["q"])
	require.Equal(t, "/search", second.Search.Paths[0].Path)
	require.Equal(t, "One", second.Settings[0].Options["one"])
}

func TestDefinitionStore_RejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	for file, name := range map[string]string{"one.yml": "One", "two.yml": "Two"} {
		body := fmt.Sprintf("id: shared\nname: %s\nlinks:\n  - http://example.test/\n", name)
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600))
	}
	store := NewDefinitionStore(dir, testLogger())

	trackers, errs, err := store.Trackers()
	require.NoError(t, err)
	require.Empty(t, trackers)
	require.Len(t, errs, 2)
	require.ErrorContains(t, errs[0], `duplicate tracker id "shared"`)
	require.ErrorContains(t, errs[1], `duplicate tracker id "shared"`)

	_, err = store.Find("shared")
	require.ErrorContains(t, err, `duplicate tracker id "shared"`)
	require.NotErrorIs(t, err, ErrTrackerNotFound)

	require.NoError(t, os.Remove(filepath.Join(dir, "two.yml")))
	def, err := store.Find("shared")
	require.NoError(t, err)
	require.Equal(t, "One", def.Name)
}

func TestDefinitionStore_FindReportsUnknownID(t *testing.T) {
	store := NewDefinitionStore(t.TempDir(), testLogger())
	_, err := store.Find("nope")
	require.ErrorIs(t, err, ErrTrackerNotFound)
}

func TestDefinitionStore_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		writeDefinition(t, dir, fmt.Sprintf("t%d", i), fmt.Sprintf("Tracker %d", i))
	}
	store := NewDefinitionStore(dir, testLogger())

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 20 {
				_, _, err := store.Trackers()
				if !assert.NoError(t, err) {
					return
				}
				if _, err := store.Find(fmt.Sprintf("t%d", i%5)); !assert.NoError(t, err) {
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestDefinitionStore_ExtensionCaseIsIgnored covers definitions whose
// extension is not lowercase.
func TestDefinitionStore_ExtensionCaseIsIgnored(t *testing.T) {
	dir := t.TempDir()
	for _, file := range []string{"a.yml", "b.YML", "c.Yaml", "d.YAML", "e.yaml"} {
		body := fmt.Sprintf("id: %s\nname: %s\nlinks:\n  - http://example.test/\n", file, file)
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600))
	}
	// Neither is a definition, whatever its case.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.TXT"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600))

	store := NewDefinitionStore(dir, testLogger())
	trackers, errs, err := store.Trackers()
	require.NoError(t, err)
	require.Empty(t, errs)
	require.ElementsMatch(t,
		[]string{"a.yml", "b.YML", "c.Yaml", "d.YAML", "e.yaml"},
		trackerNames(trackers))
}

func TestIsDefinitionFile(t *testing.T) {
	for _, name := range []string{"a.yml", "a.yaml", "a.YML", "a.YAML", "a.Yml", "a.yAmL"} {
		require.True(t, isDefinitionFile(name), name)
	}
	for _, name := range []string{"a.txt", "a.json", "a.yml.bak", "a", "yml", ".yml.txt"} {
		require.False(t, isDefinitionFile(name), name)
	}
}

func TestCheckStatus(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		status  int
		want    string
		wantErr bool
	}{
		{name: "a success is not an error", rawURL: "https://tracker.test/x", status: 200},
		{
			// The query is dropped because it carries the passkey on many
			// trackers, and this message reaches logs and the panel.
			name:    "a query is redacted, and said to be",
			rawURL:  "https://tracker.test/download.php?id=12345&passkey=secret",
			status:  404,
			want:    "tracker responded Not Found for https://tracker.test/download.php (query redacted)",
			wantErr: true,
		},
		{
			name:    "a URL with no query is reported whole",
			rawURL:  "https://tracker.test/download.php",
			status:  503,
			want:    "tracker responded Service Unavailable for https://tracker.test/download.php",
			wantErr: true,
		},
		//nolint:gosec // G101: an invented credential in test data, to prove it is dropped.
		{
			name:    "credentials in the URL are dropped as well",
			rawURL:  "https://user:pw@tracker.test/dl",
			status:  403,
			want:    "tracker responded Forbidden for https://tracker.test/dl",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.Parse(tc.rawURL)
			require.NoError(t, err)

			err = checkStatus(tc.status, parsed)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.want)
			require.NotContains(t, err.Error(), "secret")
			require.NotContains(t, err.Error(), "pw@")
		})
	}
}

// TestTorrentFrom_ResolvesAgainstThePage pins how a row's relative links
// are resolved. A definition whose search path sits in a subdirectory
// carries links relative to that directory, and resolving them against the
// site root instead addresses a page that does not exist.
func TestTorrentFrom_ResolvesAgainstThePage(t *testing.T) {
	tests := []struct {
		details     string
		download    string
		name        string
		page        string
		wantDetails string
		wantLink    string
	}{
		{
			details:     "viewtopic.php?t=456",
			download:    "download.php?id=123",
			name:        "a search path in a subdirectory keeps the subdirectory",
			page:        "https://tracker.test/forum/tracker.php",
			wantDetails: "https://tracker.test/forum/viewtopic.php?t=456",
			wantLink:    "https://tracker.test/forum/download.php?id=123",
		},
		{
			details:     "/view.php?t=456",
			download:    "/dl.php?id=123",
			name:        "a root-relative link is resolved against the host",
			page:        "https://tracker.test/forum/tracker.php",
			wantDetails: "https://tracker.test/view.php?t=456",
			wantLink:    "https://tracker.test/dl.php?id=123",
		},
		{
			details:     "https://other.test/view.php?t=456",
			download:    "https://other.test/dl.php?id=123",
			name:        "an absolute link is left alone",
			page:        "https://tracker.test/forum/tracker.php",
			wantDetails: "https://other.test/view.php?t=456",
			wantLink:    "https://other.test/dl.php?id=123",
		},
		{
			details:     "details.php?t=456",
			download:    "download.php?id=123",
			name:        "a search path at the root resolves at the root",
			page:        "https://tracker.test/search.php",
			wantDetails: "https://tracker.test/details.php?t=456",
			wantLink:    "https://tracker.test/download.php?id=123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pageURL, err := url.Parse(tc.page)
			require.NoError(t, err)

			got, ok := torrentFrom(&Tracker{Name: "Example Tracker"}, pageURL, map[string]string{
				"details":  tc.details,
				"download": tc.download,
				"title":    "Example Release",
			})
			require.True(t, ok)
			require.Equal(t, tc.wantLink, got.DownloadURL)
			require.Equal(t, tc.wantDetails, got.DetailsURL)
		})
	}
}

// TestTorrentFrom_TorrentAndMagnet covers a row carrying both a torrent
// file and a magnet, which a tracker commonly offers together.
func TestTorrentFrom_TorrentAndMagnet(t *testing.T) {
	pageURL, err := url.Parse("https://tracker.test/forum/tracker.php")
	require.NoError(t, err)
	public := &Tracker{Name: "Example Tracker", Type: "public"}

	t.Run("both are kept, each in its own field", func(t *testing.T) {
		got, ok := torrentFrom(public, pageURL, map[string]string{
			"download": "download.php?id=123",
			"magnet":   "magnet:?xt=urn:btih:abc123",
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Equal(t, "https://tracker.test/forum/download.php?id=123", got.DownloadURL)
		require.Equal(t, "magnet:?xt=urn:btih:abc123", got.Magnet)
	})

	t.Run("a magnet in the download field is stored as a magnet", func(t *testing.T) {
		got, ok := torrentFrom(public, pageURL, map[string]string{
			"download": "magnet:?xt=urn:btih:abc123",
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Empty(t, got.DownloadURL, "a magnet is not something to fetch through the tracker")
		require.Equal(t, "magnet:?xt=urn:btih:abc123", got.Magnet)
	})

	t.Run("a torrent alone leaves the magnet empty", func(t *testing.T) {
		got, ok := torrentFrom(public, pageURL, map[string]string{
			"download": "download.php?id=123",
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Empty(t, got.Magnet)
	})
}

// TestTorrentFrom_InfoHashMagnetNeedsAPublicTracker covers building a
// magnet from an info hash alone. Such a link names no trackers, so a
// client can only reach peers through the DHT.
func TestTorrentFrom_InfoHashMagnetNeedsAPublicTracker(t *testing.T) {
	pageURL, err := url.Parse("https://tracker.test/browse.php")
	require.NoError(t, err)

	for _, trackerType := range []string{"public", "semi-private", "Public"} {
		t.Run("built for a "+trackerType+" tracker", func(t *testing.T) {
			got, ok := torrentFrom(&Tracker{Name: "T", Type: trackerType}, pageURL, map[string]string{
				"infohash": "abc123",
				"title":    "Example Release",
			})
			require.True(t, ok)
			require.Contains(t, got.Magnet, "urn:btih:abc123")
			require.Contains(t, got.Magnet, "dn=Example+Release")
		})
	}

	// A private tracker's torrents are deliberately off the DHT, so such a
	// link cannot resolve, and it carries no passkey either.
	for _, trackerType := range []string{"private", "", "semi-public"} {
		t.Run("withheld for type "+strconv.Quote(trackerType), func(t *testing.T) {
			got, ok := torrentFrom(&Tracker{Name: "T", Type: trackerType}, pageURL, map[string]string{
				"infohash": "abc123",
				"title":    "Example Release",
			})
			require.True(t, ok)
			require.Empty(t, got.Magnet)
		})
	}

	t.Run("a scraped magnet is kept whatever the tracker type", func(t *testing.T) {
		got, ok := torrentFrom(&Tracker{Name: "T", Type: "private"}, pageURL, map[string]string{
			"infohash": "abc123",
			"magnet":   "magnet:?xt=urn:btih:abc123&tr=https%3A%2F%2Ftracker.test%2Fannounce",
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Contains(t, got.Magnet, "tr=")
	})
}

// TestTorrentFrom_MagnetFieldMustBeAMagnet covers a definition whose
// magnet field holds something else. The value is scraped from a
// third-party page and is later handed to a client, or redirected to, so
// only a magnet is kept.
func TestTorrentFrom_MagnetFieldMustBeAMagnet(t *testing.T) {
	pageURL, err := url.Parse("https://tracker.test/browse.php")
	require.NoError(t, err)
	def := &Tracker{Name: "T", Type: "public"}

	for _, raw := range []string{
		"https://evil.test/",
		"//evil.test/",
		"javascript:alert(1)",
		"not a url at all",
		"",
	} {
		t.Run("refused: "+strconv.Quote(raw), func(t *testing.T) {
			got, ok := torrentFrom(def, pageURL, map[string]string{
				"download": "dl.php?id=1",
				"magnet":   raw,
				"title":    "Example Release",
			})
			require.True(t, ok)
			require.Empty(t, got.Magnet)
			require.Equal(t, "https://tracker.test/dl.php?id=1", got.DownloadURL)
		})
	}

	t.Run("the scheme is matched whatever its case", func(t *testing.T) {
		got, ok := torrentFrom(def, pageURL, map[string]string{
			"magnet": "MAGNET:?xt=urn:btih:abc123",
			"title":  "Example Release",
		})
		require.True(t, ok)
		require.Equal(t, "MAGNET:?xt=urn:btih:abc123", got.Magnet)
	})
}

// TestMagnetInfoHash covers reading a v1 info hash back out of a magnet,
// which is what gives a row an info hash when its definition scrapes only
// the link.
func TestMagnetInfoHash(t *testing.T) {
	const hexHash = "0123456789abcdef0123456789abcdef01234567"
	const base32Hash = "MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U"

	for _, tc := range []struct {
		magnet string
		name   string
		want   string
	}{
		{
			magnet: "magnet:?xt=urn:btih:" + hexHash + "&dn=Example+Release",
			name:   "hex",
			want:   hexHash,
		},
		{
			magnet: "magnet:?xt=urn:btih:" + base32Hash,
			name:   "base32",
			want:   base32Hash,
		},
		{
			magnet: "magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01",
			name:   "uppercase hex is left as the link spelled it",
			want:   "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
		},
		{
			magnet: "magnet:?xt=URN:BTIH:" + hexHash,
			name:   "the urn is matched without regard to case",
			want:   hexHash,
		},
		{
			magnet: "magnet:?dn=Example+Release&tr=https%3A%2F%2Ftracker.test%2Fannounce&xt=urn:btih:" + hexHash,
			name:   "trackers and a display name do not get in the way",
			want:   hexHash,
		},

		// A hybrid torrent names both of its hashes, in either order, so
		// the v1 one is picked out rather than whichever comes first.
		{
			magnet: "magnet:?xt=urn:btmh:1220abcdef&xt=urn:btih:" + hexHash,
			name:   "hybrid, v2 first",
			want:   hexHash,
		},
		{
			magnet: "magnet:?xt=urn:btih:" + hexHash + "&xt=urn:btmh:1220abcdef",
			name:   "hybrid, v1 first",
			want:   hexHash,
		},

		// A v2 multihash is not an info hash: reporting one would
		// advertise a value no client matching on an info hash can use.
		{
			magnet: "magnet:?xt=urn:btmh:1220abcdef0123456789abcdef0123456789abcdef",
			name:   "v2 only",
			want:   "",
		},

		// The two above are refused for their length as much as for their
		// urn, so they cannot show that the urn is read at all. These give
		// the v2 entry a payload shaped exactly like an info hash, which
		// no real multihash is, leaving the urn as the only thing that can
		// tell them apart -- the property the doc comment claims and the
		// one that separates this from taking whichever "xt" comes first.
		{
			magnet: "magnet:?xt=urn:btmh:" + hexHash,
			name:   "a v2 multihash shaped like an info hash is still not one",
			want:   "",
		},
		{
			magnet: "magnet:?xt=urn:btmh:" + hexHash + "&xt=urn:btih:" + base32Hash,
			name:   "hybrid whose v2 hash would pass for one",
			want:   base32Hash,
		},
		{
			magnet: "magnet:?dn=Example+Release",
			name:   "no xt at all",
			want:   "",
		},
		{
			magnet: "magnet:?xt=urn:btih:abc123",
			name:   "a hash of the wrong length",
			want:   "",
		},
		{
			magnet: "magnet:?xt=urn:btih:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
			name:   "hex of the right length but not hex",
			want:   "",
		},
		{
			magnet: "magnet:?xt=urn:btih:",
			name:   "an empty hash",
			want:   "",
		},
		{
			magnet: "https://tracker.test/download.php?id=1",
			name:   "not a magnet",
			want:   "",
		},
		{
			magnet: "",
			name:   "empty",
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, magnetInfoHash(tc.magnet))
		})
	}
}

// TestTorrentFrom_InfoHashFromMagnet covers the reverse of building a
// magnet from an info hash: a definition that scrapes only a magnet still
// names the hash, inside it, and a client matching on one would otherwise
// see nothing.
//
// Unlike building a magnet, this is not withheld from a private tracker.
// Reading a hash out of a link already in hand discloses nothing the link
// did not, where minting a trackerless magnet would put a private
// tracker's torrent on the DHT.
func TestTorrentFrom_InfoHashFromMagnet(t *testing.T) {
	pageURL, err := url.Parse("https://tracker.test/browse.php")
	require.NoError(t, err)
	const hash = "0123456789abcdef0123456789abcdef01234567"

	for _, trackerType := range []string{"public", "semi-private", "private", ""} {
		t.Run("read for type "+strconv.Quote(trackerType), func(t *testing.T) {
			got, ok := torrentFrom(&Tracker{Name: "T", Type: trackerType}, pageURL, map[string]string{
				"magnet": "magnet:?xt=urn:btih:" + hash + "&tr=https%3A%2F%2Ftracker.test%2Fannounce",
				"title":  "Example Release",
			})
			require.True(t, ok)
			require.Equal(t, hash, got.InfoHash)
		})
	}

	t.Run("a magnet in the download field names it too", func(t *testing.T) {
		got, ok := torrentFrom(&Tracker{Name: "T", Type: "public"}, pageURL, map[string]string{
			"download": "magnet:?xt=urn:btih:" + hash,
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Equal(t, hash, got.InfoHash)
	})

	t.Run("a scraped hash wins over the one in the link", func(t *testing.T) {
		const scraped = "fedcba9876543210fedcba9876543210fedcba98"
		got, ok := torrentFrom(&Tracker{Name: "T", Type: "public"}, pageURL, map[string]string{
			"infohash": scraped,
			"magnet":   "magnet:?xt=urn:btih:" + hash,
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Equal(t, scraped, got.InfoHash, "the definition's own field is the authority")
	})

	t.Run("a torrent file alone leaves it empty", func(t *testing.T) {
		got, ok := torrentFrom(&Tracker{Name: "T", Type: "public"}, pageURL, map[string]string{
			"download": "download.php?id=123",
			"title":    "Example Release",
		})
		require.True(t, ok)
		require.Empty(t, got.InfoHash)
	})
}

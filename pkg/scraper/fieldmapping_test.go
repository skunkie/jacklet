// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"log/slog"
	"maps"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildTorrent maps one scraped row, with the two fields every row needs
// already filled in, so a case only states the field under test.
func buildTorrent(t *testing.T, extra map[string]string) Torrent {
	t.Helper()

	def := &Tracker{ID: "t", Name: "T", Type: "public"}
	pageURL, err := url.Parse("https://tracker.invalid/search.php")
	require.NoError(t, err)

	row := map[string]string{"title": "Some.Release", "download": "/dl/1"}
	maps.Copy(row, extra)

	torrent, ok := torrentFrom(def, pageURL, nil, row)
	require.True(t, ok, "torrentFrom rejected %v", extra)
	return torrent
}

// Jackett parses every numeric field through ParseUtil, which is lenient:
// it keeps the digits and treats both separators as digit grouping. A
// strict parse turns a tracker's "1,234" into 0, which reads downstream as
// a release with no seeders and gets it discarded.
func TestFieldMappingLenientNumbers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		get   func(Torrent) any
		value string
		want  any
	}{
		{name: "plain count", field: "seeders", get: func(r Torrent) any { return r.Seeders }, value: "12", want: 12},
		{name: "thousands separator", field: "seeders", get: func(r Torrent) any { return r.Seeders }, value: "1,234", want: 1234},
		{name: "space separator", field: "seeders", get: func(r Torrent) any { return r.Seeders }, value: "1 234", want: 1234},
		{name: "european grouping", field: "seeders", get: func(r Torrent) any { return r.Seeders }, value: "1.234", want: 1234},
		{name: "surrounding text", field: "seeders", get: func(r Torrent) any { return r.Seeders }, value: "12 seeders", want: 12},
		{name: "placeholder dash", field: "seeders", get: func(r Torrent) any { return r.Seeders }, value: "-", want: 0},
		{name: "leechers", field: "leechers", get: func(r Torrent) any { return r.Leechers }, value: "1,234", want: 1234},
		{name: "grabs", field: "grabs", get: func(r Torrent) any { return r.Grabs }, value: "1,234", want: 1234},
		{name: "files", field: "files", get: func(r Torrent) any { return r.Files }, value: "1,234", want: 1234},
		{name: "minimumseedtime", field: "minimumseedtime", get: func(r Torrent) any { return r.MinimumSeedTime }, value: "172,800", want: 172800},
		{name: "decimal comma factor", field: "downloadvolumefactor", get: func(r Torrent) any { return r.DownloadVolumeFactor }, value: "0,5", want: 0.5},
		{name: "freeleech", field: "downloadvolumefactor", get: func(r Torrent) any { return r.DownloadVolumeFactor }, value: "0", want: 0.0},
		{name: "upload factor", field: "uploadvolumefactor", get: func(r Torrent) any { return r.UploadVolumeFactor }, value: "2", want: 2.0},
		{name: "minimum ratio", field: "minimumratio", get: func(r Torrent) any { return r.MinimumRatio }, value: "1,5", want: 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.get(buildTorrent(t, map[string]string{tc.field: tc.value})))
		})
	}
}

// An absent volume factor keeps Cardigann's default of 1 rather than
// coercing an empty string to 0, which would advertise every release as
// freeleech.
func TestFieldMappingVolumeFactorDefaults(t *testing.T) {
	torrent := buildTorrent(t, nil)
	require.Equal(t, 1.0, torrent.DownloadVolumeFactor)
	require.Equal(t, 1.0, torrent.UploadVolumeFactor)
}

// Jackett zeroes a peer count at or above 5,000,000 (its issue #6558):
// some trackers put a placeholder in the column that parses into an
// enormous number, and such a release would outrank every genuine one.
func TestFieldMappingImplausiblePeerCountIsZeroed(t *testing.T) {
	require.Equal(t, 4999999, buildTorrent(t, map[string]string{"seeders": "4999999"}).Seeders)
	require.Equal(t, 0, buildTorrent(t, map[string]string{"seeders": "5000000"}).Seeders)
	require.Equal(t, 0, buildTorrent(t, map[string]string{"leechers": "9999999"}).Leechers)
}

// Jackett runs the date field through DateTimeUtil.FromUnknown, so a
// definition need not declare a date filter for a tracker that prints a
// Unix timestamp or relative text.
func TestFieldMappingDateWithoutAFilter(t *testing.T) {
	t.Run("unix timestamp", func(t *testing.T) {
		got := buildTorrent(t, map[string]string{"date": "1710526200"}).Published
		require.Equal(t, "2024-03-15T18:10:00Z", got)
	})

	t.Run("rfc1123z", func(t *testing.T) {
		got := buildTorrent(t, map[string]string{"date": "Fri, 15 Mar 2024 18:10:00 +0000"}).Published
		require.Equal(t, "2024-03-15T18:10:00Z", got)
	})

	for _, tc := range []struct {
		name    string
		value   string
		wantAgo time.Duration
	}{
		{name: "relative", value: "2 hours ago", wantAgo: 2 * time.Hour},
		{name: "abbreviated", value: "5 min ago", wantAgo: 5 * time.Minute},
		{name: "now", value: "now", wantAgo: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildTorrent(t, map[string]string{"date": tc.value}).Published
			parsed, err := time.Parse(time.RFC3339, got)
			require.NoError(t, err)
			require.WithinDuration(t, time.Now().Add(-tc.wantAgo), parsed, 5*time.Second)
		})
	}
}

// Cardigann spells the IMDb field both ways and Jackett reads them as one
// case, so a definition using "imdbid" must not lose it.
func TestFieldMappingIMDBIDSpellings(t *testing.T) {
	// Stored as digits, like every other external id; the "tt" prefix is
	// put back when the feed is rendered.
	require.Equal(t, "1234567", buildTorrent(t, map[string]string{"imdb": "tt1234567"}).IMDBID)
	require.Equal(t, "1234567", buildTorrent(t, map[string]string{"imdbid": "tt1234567"}).IMDBID)
	require.Equal(t, "1234567", buildTorrent(t, map[string]string{"imdb": "1234567"}).IMDBID)
	require.Empty(t, buildTorrent(t, map[string]string{"imdb": "n/a"}).IMDBID)
}

// parseSize follows ParseUtil.GetBytes: it takes the digits wherever they
// sit, treats a lone separator as a decimal point and several as digit
// grouping, and finds the unit among the string's letters.
func TestParseSizeJackettForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{in: "3.5 GB", want: 3758096384},
		{in: "296,98 MB", want: 311406100},
		{in: "1.018,29 MB", want: 1067754455},
		{in: "1,234.56 GB", want: 1325598706237},
		{in: "Size: 3.5 GB", want: 3758096384},
		{in: "12345678", want: 12345678},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// Every remaining Cardigann field reaches the stored torrent. Jackett
// reduces an external id to its digits and normalizes a genre list, so a
// definition yields the same values here as it does there.
func TestFieldMappingRemainingCardigannFields(t *testing.T) {
	torrent := buildTorrent(t, map[string]string{
		"tmdbid": "tmdb/12345", "tvmazeid": "82", "traktid": "tt999",
		// Definitions commonly scrape an href rather than a bare id.
		"tvdbid":   "https://thetvdb.com/?tab=series&id=81189",
		"doubanid": "26387939", "rageid": "7",
		"genre": "Sci_Fi, Action, Action", "year": "2,024",
		"poster": "/img/cover.jpg",
		"author": "A. Writer", "booktitle": "The Book", "publisher": "Pub House",
		"artist": "The Band", "album": "The Album", "label": "Some Label", "track": "03",
	})

	t.Run("external ids keep only their digits", func(t *testing.T) {
		require.Equal(t, "12345", torrent.TMDBID)
		require.Equal(t, "81189", torrent.TVDBID)
		require.Equal(t, "82", torrent.TVMazeID)
		require.Equal(t, "999", torrent.TraktID)
		require.Equal(t, "26387939", torrent.DoubanID)
		require.Equal(t, "7", torrent.RageID)
	})

	t.Run("genres lose underscores and repeats", func(t *testing.T) {
		require.Equal(t, "Sci Fi, Action", torrent.Genres)
	})

	t.Run("year is coerced", func(t *testing.T) {
		require.Equal(t, 2024, torrent.Year)
	})

	t.Run("poster resolves against the page", func(t *testing.T) {
		require.Equal(t, "https://tracker.invalid/img/cover.jpg", torrent.Poster)
	})

	t.Run("book metadata", func(t *testing.T) {
		require.Equal(t, "A. Writer", torrent.Author)
		require.Equal(t, "The Book", torrent.BookTitle)
		require.Equal(t, "Pub House", torrent.Publisher)
	})

	t.Run("music metadata", func(t *testing.T) {
		require.Equal(t, "The Band", torrent.Artist)
		require.Equal(t, "The Album", torrent.Album)
		require.Equal(t, "Some Label", torrent.Label)
		require.Equal(t, "03", torrent.Track)
	})
}

// A row that scrapes none of them stores none of them, rather than a set
// of empty-but-present values.
func TestFieldMappingRemainingFieldsAbsent(t *testing.T) {
	torrent := buildTorrent(t, nil)
	require.Empty(t, torrent.TMDBID)
	require.Empty(t, torrent.TVDBID)
	require.Empty(t, torrent.Genres)
	require.Zero(t, torrent.Year)
	require.Empty(t, torrent.Poster)
	require.Empty(t, torrent.Artist)
}

// Cardigann says a date filter's argument is a Go layout, but Jackett also
// accepts a .NET custom format there and real definitions use both. The
// two orderings of the same ambiguous value must not agree.
func TestDateParseDeclaredLayouts(t *testing.T) {
	parse := func(t *testing.T, layout, value string) time.Time {
		t.Helper()
		got, err := applyFilter(value, Filter{Args: layout, Name: "dateparse"}, templateData{}, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		parsed, err := time.Parse(time.RFC3339, got)
		require.NoError(t, err)
		return parsed
	}

	t.Run("dotnet day-first", func(t *testing.T) {
		got := parse(t, "dd/MM/yyyy", "03/02/2024")
		require.Equal(t, time.February, got.Month())
		require.Equal(t, 3, got.Day())
	})

	t.Run("dotnet month-first reads the same value differently", func(t *testing.T) {
		got := parse(t, "MM/dd/yyyy", "03/02/2024")
		require.Equal(t, time.March, got.Month())
		require.Equal(t, 2, got.Day())
	})

	t.Run("dotnet with time", func(t *testing.T) {
		got := parse(t, "dd.MM.yyyy HH:mm", "15.03.2024 18:30")
		require.Equal(t, 18, got.Hour())
		require.Equal(t, 30, got.Minute())
	})

	t.Run("dotnet month names", func(t *testing.T) {
		require.Equal(t, time.March, parse(t, "d MMM yyyy", "5 Mar 2024").Month())
	})

	t.Run("go layout still works", func(t *testing.T) {
		got := parse(t, "02/01/2006", "03/02/2024")
		require.Equal(t, time.February, got.Month())
		require.Equal(t, 3, got.Day())
	})

	// "Mon" and "Jan" are not .NET specifiers, so this must be read as a
	// Go layout rather than mangled through the .NET path.
	t.Run("go layout with names", func(t *testing.T) {
		got := parse(t, "Mon, 02 Jan 2006", "Fri, 15 Mar 2024")
		require.Equal(t, time.March, got.Month())
		require.Equal(t, 15, got.Day())
	})

	// Jackett leaves the value untouched when the declared layout does not
	// fit, rather than guessing at a date the definition already described.
	t.Run("a value the layout does not fit is left alone", func(t *testing.T) {
		got, err := applyFilter("not a date", Filter{Args: "yyyy-MM-dd", Name: "dateparse"}, templateData{}, slog.New(slog.DiscardHandler))
		require.Error(t, err)
		require.Equal(t, "not a date", got)
	})

	// A Go layout carrying no year defaults to the current one, stepping
	// back when that would put the date in the future -- a tracker printing
	// "31 Dec" in January means last December.
	t.Run("a yearless layout is never in the future", func(t *testing.T) {
		got, err := applyFilter("31 Dec", Filter{Args: "02 Jan", Name: "dateparse"}, templateData{}, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		parsed, err := time.Parse(time.RFC3339, got)
		require.NoError(t, err)
		require.False(t, parsed.After(time.Now()))
		require.Equal(t, time.December, parsed.Month())
	})
}

// The Cyrillic units are multi-byte, so ordering the known spellings by
// byte length put the one-rune "б" ahead of "гб" and a labelled size came
// back as a single byte -- successfully, which is worse than an error.
func TestParseSize_CyrillicUnitsWithLabel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{in: "1.5 ГБ", want: 1610612736},
		{in: "Размер: 1.5 ГБ", want: 1610612736},
		{in: "Размер 700 КБ", want: 716800},
		{in: "1,5 гб", want: 1610612736},
		{in: "Размер: 512 Б", want: 512},
		// The Latin spellings keep working alongside them.
		{in: "Size: 3.5 GB", want: 3758096384},
		{in: "700 KiB", want: 716800},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	// A unit that is genuinely unknown is still an error rather than a
	// silently wrong number.
	for _, in := range []string{"1.5 parsecs", "1.5 световых лет"} {
		_, err := parseSize(in)
		require.Error(t, err, "%q", in)
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1.5 GB", want: 1610612736},
		{in: "500 MB", want: 524288000},
		{in: "1.2GB", want: 1288490188},
		{in: "1,5 GB", want: 1610612736},
		{in: "1.2 TB", want: 1319413953331},
		{in: "3 TiB", want: 3298534883328},
		{in: "700 KiB", want: 716800},
		{in: "1 024 MB", want: 1073741824},
		{in: "1.5 ГБ", want: 1610612736},
		{in: "512 b", want: 512},
		// Many definitions select a site's raw byte count, which carries no
		// unit at all.
		{in: "86261275745", want: 86261275745},
		{in: "1568746393", want: 1568746393},
		{in: "0", want: 0},
		{in: "", wantErr: true},
		{in: "unknown", wantErr: true},
		{in: "N/A", wantErr: true},
		{in: "1.5 parsecs", wantErr: true},
		{in: "GB", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

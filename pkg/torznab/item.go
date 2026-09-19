// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/database"
)

// This file renders a stored torrent into what a client sees. It reads a
// database.Torrent and knows nothing about how one is stored or found.

// pubDate renders a torrent's release date in the RFC1123Z form RSS
// requires. The store holds UTC RFC 3339, or an empty string when the
// definition yielded no parseable date; in that case the feed is generated
// with the current time, since a client sorting by date is better served
// by a plausible timestamp than by a missing element.
func pubDate(row *database.Torrent) string {
	if t, err := time.Parse(time.RFC3339, row.Published); err == nil {
		return t.Format(time.RFC1123Z)
	}
	return time.Now().Format(time.RFC1123Z)
}

// downloadLink is what a client is told to fetch. A magnet is handed over
// as-is, since a client resolves one itself and there is nothing to
// authenticate. An HTTP link goes back through Jacklet, which re-fetches
// it with the tracker's own session: the client has no session of its own,
// so the tracker's direct link would give it a 403 or a login page saved
// as a ".torrent".
//
// The link carries the row's id rather than the tracker URL, so the
// endpoint cannot be asked to fetch an arbitrary address.
func downloadLink(row *database.Torrent, baseURL, indexerID, apiKey string) string {
	if row.DownloadURL == "" {
		// Nothing to fetch on the client's behalf, so the magnet is the
		// link when there is one, and there is no link when there is not.
		return row.Magnet
	}
	// A magnet resolves in the client, so it is handed over as it stands.
	// The scraper keeps one in Magnet, but this package is importable and
	// a caller may have stored it as the download link.
	if strings.HasPrefix(row.DownloadURL, "magnet:") {
		return row.DownloadURL
	}
	link := fmt.Sprintf("%s/api/v2.0/indexers/%s/download/%d", baseURL, url.PathEscape(indexerID), row.ID)
	if apiKey == "" {
		return link
	}
	return link + "?" + url.Values{"apikey": {apiKey}}.Encode()
}

// summary is the item's description: what the definition scraped, or the
// title when it scraped nothing.
func summary(row *database.Torrent) string {
	if row.Description != "" {
		return row.Description
	}
	return row.Name
}

// guid returns a stable identifier for the row. The details page URL is
// preferred because it is a real permalink. The title is the fallback,
// and is only as unique as the tracker makes it: the store distinguishes
// same-titled releases by info hash, but a title is all there is to
// report for a definition that scrapes neither a hash nor a details page.
func guid(row *database.Torrent) string {
	if row.DetailsURL != "" {
		return row.DetailsURL
	}
	return row.Name
}

// attributes renders a torrent's "torznab:attr" elements. The volume
// factors are always emitted: a client reading no downloadvolumefactor
// cannot tell a freeleech release from an ordinary one, and Cardigann's
// default of 1 is meaningful rather than merely absent. The optional
// identifiers are emitted only when the definition scraped them.
func attributes(row *database.Torrent) []Attribute {
	attrs := []Attribute{
		{Name: "category", Value: strconv.Itoa(row.Category)},
		{Name: "seeders", Value: strconv.Itoa(row.Seeders)},
		{Name: "leechers", Value: strconv.Itoa(row.Leechers)},
		{Name: "peers", Value: strconv.Itoa(row.Seeders + row.Leechers)},
		{Name: "size", Value: strconv.FormatInt(row.Size, 10)},
		{Name: "grabs", Value: strconv.Itoa(row.Grabs)},
		{Name: "downloadvolumefactor", Value: formatFactor(row.DownloadVolumeFactor)},
		{Name: "uploadvolumefactor", Value: formatFactor(row.UploadVolumeFactor)},
	}

	optional := []Attribute{
		// A client that prefers a magnet takes this one; "link" stays the
		// torrent file, which is what a private tracker needs.
		{Name: "magneturl", Value: row.Magnet},
		{Name: "infohash", Value: row.InfoHash},
		// Jackett emits the IMDb id twice under two spellings: "imdb" is
		// the bare number and "imdbid" the canonical "tt"-prefixed form.
		// Clients look for one or the other, so both go out.
		{Name: "imdb", Value: imdbNumber(row.IMDBID)},
		{Name: "imdbid", Value: IMDBID(row.IMDBID)},
		{Name: "tvdbid", Value: row.TVDBID},
		{Name: "tmdbid", Value: row.TMDBID},
		{Name: "tvmazeid", Value: row.TVMazeID},
		{Name: "traktid", Value: row.TraktID},
		{Name: "doubanid", Value: row.DoubanID},
		{Name: "rageid", Value: row.RageID},
		{Name: "genre", Value: row.Genres},
		// Jackett names the cover image "coverurl" rather than "poster",
		// and a client looking for the image looks for that name.
		{Name: "coverurl", Value: row.Poster},
		// Book metadata, which Readarr matches on.
		{Name: "author", Value: row.Author},
		{Name: "booktitle", Value: row.BookTitle},
		{Name: "publisher", Value: row.Publisher},
		// Music metadata, which Lidarr matches on.
		{Name: "artist", Value: row.Artist},
		{Name: "album", Value: row.Album},
		{Name: "label", Value: row.Label},
		{Name: "track", Value: row.Track},
	}
	for _, attr := range optional {
		if attr.Value != "" {
			attrs = append(attrs, attr)
		}
	}

	if row.Year > 0 {
		attrs = append(attrs, Attribute{Name: "year", Value: strconv.Itoa(row.Year)})
	}
	if row.Files > 0 {
		attrs = append(attrs, Attribute{Name: "files", Value: strconv.Itoa(row.Files)})
	}
	if row.MinimumRatio > 0 {
		attrs = append(attrs, Attribute{Name: "minimumratio", Value: formatFactor(row.MinimumRatio)})
	}
	if row.MinimumSeedTime > 0 {
		attrs = append(attrs, Attribute{Name: "minimumseedtime", Value: strconv.Itoa(row.MinimumSeedTime)})
	}
	return attrs
}

// imdbWidth is the width IMDb zero-pads an id to. Ids have since grown
// past it; padding only ever adds digits, so a longer one is left alone.
const imdbWidth = 7

// imdbNumber renders a stored IMDb id -- which is bare digits, as every
// external id is stored -- as Jackett's "imdb" attribute: the number,
// zero-padded. An id with no digits yields "", which leaves the attribute
// out rather than advertising "0000000" as Jackett does: a client would
// take that for a real id and try to match on it.
func imdbNumber(digits string) string {
	if digits == "" {
		return ""
	}
	if padding := imdbWidth - len(digits); padding > 0 {
		return strings.Repeat("0", padding) + digits
	}
	return digits
}

// IMDBID renders a stored IMDb id in its canonical "tt"-prefixed form,
// which is Jackett's "imdbid" attribute and what the JSON results
// endpoint reports. It is empty when nothing was scraped.
func IMDBID(digits string) string {
	if number := imdbNumber(digits); number != "" {
		return "tt" + number
	}
	return ""
}

// formatFactor renders a ratio-style value without trailing zeros, so a
// plain "1" is not advertised as "1.000000".
func formatFactor(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// toTorznabItem renders a torrent as a Torznab XML feed item. link is what
// the client should fetch, from downloadLink.
func toTorznabItem(row *database.Torrent, link string) Item {
	isPermaLink := "false"
	if row.DetailsURL != "" {
		isPermaLink = "true"
	}

	// Item takes the wire-order exemption from the alphabetical field
	// rule, so this literal follows the declaration rather than the
	// alphabet: read top to bottom it is the <item> that goes out.
	return Item{
		Title:       row.Name,
		GUID:        &GUID{IsPermaLink: isPermaLink, Value: guid(row)},
		Link:        link,
		Comments:    row.DetailsURL,
		PubDate:     pubDate(row),
		Category:    strconv.Itoa(row.Category),
		Description: summary(row),
		Enclosure: &Enclosure{
			Length: row.Size,
			Type:   torrentContentType,
			URL:    link,
		},
		Attributes: attributes(row),
	}
}

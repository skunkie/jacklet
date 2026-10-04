// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"crypto/sha1" //nolint:gosec // G505: the fingerprint a definition pins
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A definition's pinned certificate is trusted where it fails the standard
// verification, as in Jackett, and only for that definition's own hosts: a
// tracker serving a certificate no root vouches for is reached once its
// definition pins it, and not otherwise, and a pin another definition made
// for its own host does nothing for it.
func TestScraperTrustsPinnedCertificates(t *testing.T) {
	tracker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Pinned Release</a></div>`)
	}))
	defer tracker.Close()
	sum := sha1.Sum(tracker.Certificate().Raw) //nolint:gosec // G401: the fingerprint a definition pins
	colonSeparated := make([]string, 0, len(sum))
	for _, b := range sum {
		colonSeparated = append(colonSeparated, hex.EncodeToString([]byte{b}))
	}
	fingerprint := strings.Join(colonSeparated, ":")

	definition := func(id, link, certificates string) string {
		return `
id: ` + id + `
name: ` + id + `
links:
  - ` + link + `/
` + certificates + `search:
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
`
	}

	for _, tc := range []struct {
		name         string
		certificates string
		hasOtherPin  bool
		wantErr      string
	}{
		{name: "an unpinned certificate", wantErr: "certificate"},
		{name: "a pinned certificate", certificates: "certificates:\n  - " + fingerprint + "\n"},
		{name: "another certificate pinned", certificates: "certificates:\n  - 0000000000000000000000000000000000000000\n", wantErr: "certificate"},
		{name: "the certificate pinned for another host", hasOtherPin: true, wantErr: "certificate"},
		{
			// The tracker's host has a pin of its own, so its requests go
			// through the pins, which must still be the tracker's own.
			name:         "the certificate pinned for another host beside a pin of its own",
			certificates: "certificates:\n  - 0000000000000000000000000000000000000000\n",
			hasOtherPin:  true,
			wantErr:      "certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
			if tc.hasOtherPin {
				other := loadTestTracker(t, t.TempDir(), "other", definition("other", "https://other.invalid", "certificates:\n  - "+fingerprint+"\n"))
				scrpr.certificatePins.trust(other)
			}
			def := loadTestTracker(t, t.TempDir(), "pinned", definition("pinned", tracker.URL, tc.certificates))

			err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"Pinned Release"}, storedTitles(t, scrpr, def))
		})
	}
}

// A pinned host's transport also makes its handshake with an https:// proxy,
// which names the proxy rather than the tracker, so its certificate is
// checked against that name and the tracker's pins do not apply to it; only
// a handshake that names no host, as one to an IP address does, is checked
// against the host the transport was made for.
func TestCertificatePins_TransportFor_ChecksTheNameAskedFor(t *testing.T) {
	tracker := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tracker.Close()
	cert := tracker.Certificate()
	sum := sha1.Sum(cert.Raw) //nolint:gosec // G401: the fingerprint a definition pins

	pins := newCertificatePins()
	pins.trust(&Tracker{Certificates: []string{hex.EncodeToString(sum[:])}, Links: []string{"https://tracker.invalid/"}})
	verify := pins.transportFor("tracker.invalid").TLSClientConfig.VerifyConnection

	require.NoError(t, verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}),
		"a handshake naming no host was not checked against the transport's own")
	require.NoError(t, verify(tls.ConnectionState{ServerName: "tracker.invalid", PeerCertificates: []*x509.Certificate{cert}}))
	require.Error(t, verify(tls.ConnectionState{ServerName: "proxy.invalid", PeerCertificates: []*x509.Certificate{cert}}),
		"the tracker's pin was accepted for a handshake with another host")
}

// A download registers the definition's pins too, since a stored release
// can be grabbed before anything has searched the tracker since startup.
func TestScraper_Download_TrustsPinnedCertificates(t *testing.T) {
	tracker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		fmt.Fprint(w, "d4:infode")
	}))
	defer tracker.Close()
	sum := sha1.Sum(tracker.Certificate().Raw) //nolint:gosec // G401: the fingerprint a definition pins

	def := &Tracker{
		Certificates: []string{hex.EncodeToString(sum[:])},
		ID:           "pinned",
		Links:        []string{tracker.URL + "/"},
		Name:         "pinned",
	}
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	download, err := scrpr.Download(t.Context(), def, tracker.URL+"/download/1")
	require.NoError(t, err)
	download.Body.Close()
}

// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"crypto/sha1" //nolint:gosec // G505: a certificate's SHA-1 fingerprint is how definitions name it, not a signature
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// certificatePins holds the certificates definitions trust beyond the
// system's roots, as Jackett does: a definition's "certificates" lists the
// SHA-1 fingerprints of a tracker's own certificates, typically one that
// has expired or is self-signed, and each is trusted only for the hosts in
// that definition's links. A pin is consulted only when a certificate fails
// the standard verification, so it can never reject a connection, and it
// cannot weaken one to a host its definition does not name.
//
// It is also the RoundTripper the tracker clients share. A request to a
// host nothing pins goes through Go's default transport, verified as Go
// verifies it. A request to a pinned host goes through a transport of that
// host's own, because the check that consults the pins has to know which
// host the connection was meant for, and the TLS connection state does not
// say so when the host is an IP address.
type certificatePins struct {
	base *http.Transport
	// byHost maps a host to the fingerprints pinned for it, and
	// transports to the transport its requests use.
	byHost     map[string]map[string]bool
	mu         sync.RWMutex
	transports map[string]*http.Transport
}

// newCertificatePins returns an empty set of pins over a clone of Go's
// default transport, which keeps the standard proxy variables.
func newCertificatePins() *certificatePins {
	return &certificatePins{
		base:       http.DefaultTransport.(*http.Transport).Clone(),
		byHost:     make(map[string]map[string]bool),
		transports: make(map[string]*http.Transport),
	}
}

// trust registers def's pinned certificates for each host its links and
// legacy links name. Pins are only ever added, as in Jackett, so one stays
// trusted until the process restarts even after its definition drops it.
func (p *certificatePins) trust(def *Tracker) {
	if len(def.Certificates) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, baseURL := range candidateBaseURLs(def) {
		host := strings.ToLower(baseURL.Hostname())
		if p.byHost[host] == nil {
			p.byHost[host] = make(map[string]bool)
		}
		for _, fingerprint := range def.Certificates {
			p.byHost[host][normalizeFingerprint(fingerprint)] = true
		}
	}
}

// RoundTrip sends req through the transport its host calls for.
func (p *certificatePins) RoundTrip(req *http.Request) (*http.Response, error) {
	return p.transportFor(strings.ToLower(req.URL.Hostname())).RoundTrip(req)
}

// transportFor returns the transport for host: its own when it has pins,
// built on first use, and the base transport otherwise.
func (p *certificatePins) transportFor(host string) *http.Transport {
	p.mu.RLock()
	transport, isPinned := p.transports[host], p.byHost[host] != nil
	p.mu.RUnlock()
	if !isPinned {
		return p.base
	}
	if transport != nil {
		return transport
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if transport = p.transports[host]; transport == nil {
		transport = p.base.Clone()
		transport.TLSClientConfig = &tls.Config{
			// The chain is verified by VerifyConnection instead, which
			// does what this setting turns off and then consults the pins.
			InsecureSkipVerify: true, //nolint:gosec // G402: verifyConnection performs the standard verification
			MinVersion:         tls.VersionTLS12,
			// The transport also makes its TLS handshake with an https://
			// proxy through this config, so the name the handshake asked
			// for decides which host is checked; only an IP address, for
			// which the state carries no name, falls back to host.
			VerifyConnection: func(state tls.ConnectionState) error {
				name := state.ServerName
				if name == "" {
					name = host
				}
				return p.verifyConnection(strings.ToLower(name), state)
			},
		}
		p.transports[host] = transport
	}
	return transport
}

// verifyConnection verifies a server's certificate chain for host as
// crypto/tls does by default, against the system's roots, and accepts a
// chain that fails only when its leaf is pinned for host.
func (p *certificatePins) verifyConnection(host string, state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign}
	}
	options := x509.VerifyOptions{
		DNSName:       host,
		Intermediates: x509.NewCertPool(),
	}
	for _, cert := range state.PeerCertificates[1:] {
		options.Intermediates.AddCert(cert)
	}
	leaf := state.PeerCertificates[0]
	_, err := leaf.Verify(options)
	if err != nil && p.isPinned(host, leaf) {
		return nil
	}
	return err
}

// isPinned reports whether cert is a certificate a definition trusts for
// host.
func (p *certificatePins) isPinned(host string, cert *x509.Certificate) bool {
	sum := sha1.Sum(cert.Raw) //nolint:gosec // G401: matched against a fingerprint, not used to sign or verify
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byHost[host][strings.ToUpper(hex.EncodeToString(sum[:]))]
}

// normalizeFingerprint writes a fingerprint as hex in upper case, as
// isPinned compares it, whatever separators or case a definition used.
func normalizeFingerprint(fingerprint string) string {
	fingerprint = strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(fingerprint))
	return strings.ToUpper(fingerprint)
}

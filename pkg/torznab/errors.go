// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"encoding/xml"
	"net/http"
)

// errorCode is a Torznab error code. Torznab inherits Newznab's error
// numbering, and a client branches on the code rather than on the status:
// 100 is "your key is wrong, stop retrying", where 900 is "something went
// wrong, try again later". Every code Jacklet sends is a constant of this
// type, which is how the OpenAPI spec's list of codes is held to them.
type errorCode int

// The codes are the ones Jackett sends in the same situations, taken from
// its own filters (RequiresApiKey, RequiresConfiguredIndexer) and the
// torznab action.
const (
	// errInvalidAPIKey is Jackett's code for a rejected apikey.
	errInvalidAPIKey errorCode = 100
	// errIndexerNotSupported is Jackett's code for an indexer it has no
	// definition for, which it reports as "not supported", and for a
	// search the addressed indexer cannot answer.
	errIndexerNotSupported errorCode = 201
	// errNoSuchFunction is the Newznab code for an unknown "t". Jackett
	// has no equivalent: anything it does not recognize falls through to
	// a search, so this one is Newznab's rather than copied.
	errNoSuchFunction errorCode = 202
	// errMetaIndexerOnly is Jackett's code for a function the addressed
	// indexer does not offer, which it sends for "t=indexers" on a
	// non-meta indexer.
	errMetaIndexerOnly errorCode = 203
	// errUnknown is Jackett's catch-all, which it sends for any exception
	// the search itself raises.
	errUnknown errorCode = 900
)

// errorWriter reports a failure in whichever format the endpoint speaks,
// so the shared pipeline does not have to know whether its caller is
// serving XML, JSON or a file.
//
// The Torznab code is passed to every writer even though only the XML one
// renders it: it describes the failure rather than the rendering, and a
// caller that had to know which writer it was talking to would defeat the
// point of passing one.
type errorWriter func(w http.ResponseWriter, status int, code errorCode, message string)

// torznabError is Torznab's error document, which is a response in its own
// right rather than a bare status code: a client reads the code to tell a
// rejected key from a tracker that happened to be down.
type torznabError struct {
	Code        errorCode `xml:"code,attr"`
	Description string    `xml:"description,attr"`
	XMLName     xml.Name  `xml:"error"`
}

// writeTorznabError is the errorWriter for the Torznab XML endpoint.
func (t *Torznab) writeTorznabError(w http.ResponseWriter, status int, code errorCode, message string) {
	t.writeXMLStatus(w, status, torznabError{Code: code, Description: message})
}

// writePlainError is the errorWriter for an endpoint that serves neither
// XML nor JSON, where there is no document shape to put the code in and
// the status carries the failure on its own.
func writePlainError(w http.ResponseWriter, status int, _ errorCode, message string) {
	http.Error(w, message, status)
}

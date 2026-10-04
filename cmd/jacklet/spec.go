// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// specVersionLine matches the info object's version, the only key indented
// by exactly two spaces under that name in the document.
var specVersionLine = regexp.MustCompile(`(?m)^ {2}version: .*$`)

// versionedSpec returns a copy of spec with info.version set to version, so
// the served document names the build answering it rather than whatever
// the file says. The rest of the document is left byte for byte.
func versionedSpec(spec []byte, version string) ([]byte, error) {
	if matches := specVersionLine.FindAllIndex(spec, -1); len(matches) != 1 {
		return nil, fmt.Errorf("the OpenAPI document has %d info.version lines, want 1", len(matches))
	}
	stamped := "  version: " + strconv.Quote(version)
	return specVersionLine.ReplaceAllLiteral(spec, []byte(stamped)), nil
}

// serveSpec serves spec as application/yaml. The type is set rather than
// looked up from a file name, since Go's own table has no entry for .yaml
// and the host's, where it has one, is absent from a distroless image,
// which would leave the type to be sniffed as plain text. ServeContent is
// given no name, so the type set here is the only one it can serve.
func serveSpec(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(spec))
	}
}

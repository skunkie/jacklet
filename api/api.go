// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package api holds the OpenAPI document describing Jacklet's HTTP API, the
// Jackett-compatible indexer endpoints Sonarr, Radarr and Prowlarr are
// pointed at. It is embedded so a program serving the API can serve its
// description alongside it, without reading the source tree at run time.
package api

import (
	_ "embed"
	"slices"
)

//go:embed openapi.yaml
var openAPI []byte

// OpenAPI returns the OpenAPI document as written, in YAML. Its
// info.version is a placeholder, dev, for the server to replace with the
// version of the build serving it. Each call returns its own copy.
func OpenAPI() []byte {
	return slices.Clone(openAPI)
}

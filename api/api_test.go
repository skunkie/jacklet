// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAPI(t *testing.T) {
	spec := OpenAPI()
	require.Contains(t, string(spec), "\nopenapi: ", "the embedded file is not an OpenAPI document")
	require.Contains(t, string(spec), "\n  version: dev\n",
		"the document's info.version is not the placeholder the server replaces")

	spec[0]++
	require.NotEqual(t, spec, OpenAPI(), "a caller's change to its copy reached the embedded document")
}

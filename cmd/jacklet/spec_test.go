// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/api"
)

func TestVersionedSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		version string
		want    string
		wantErr string
	}{
		{
			name:    "the info version is replaced and nothing else",
			spec:    "info:\n  title: Example\n  version: dev\n\npaths:\n  /x:\n    version: kept\n",
			version: "v1.2.3",
			want:    "info:\n  title: Example\n  version: \"v1.2.3\"\n\npaths:\n  /x:\n    version: kept\n",
		},
		{
			name:    "a document without an info version is refused",
			spec:    "info:\n  title: Example\n",
			version: "v1.2.3",
			wantErr: "has 0 info.version lines",
		},
		{
			name:    "a document with two info versions is refused",
			spec:    "info:\n  version: one\n  version: two\n",
			version: "v1.2.3",
			wantErr: "has 2 info.version lines",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := versionedSpec([]byte(tc.spec), tc.version)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}
}

// TestVersionedSpec_EmbeddedDocument guards the embedded spec itself, so an
// edit that reshapes its info object fails here rather than at startup.
func TestVersionedSpec_EmbeddedDocument(t *testing.T) {
	spec, err := versionedSpec(api.OpenAPI(), "v9.8.7")
	require.NoError(t, err)
	require.Contains(t, string(spec), "\n  version: \"v9.8.7\"\n")
}

// TestRunServesTheStampedSpec holds run to serving the stamped spec at
// /openapi.yaml, and the raw file nowhere, whatever method a request uses.
// The build version is set to one the file does not hold, since at its
// default of dev the unstamped file would pass as well.
func TestRunServesTheStampedSpec(t *testing.T) {
	previous := version
	version = "v9.8.7-test"
	t.Cleanup(func() { version = previous })

	port := freePort(t)
	startServer(t, testSettings(t, port))
	client := &http.Client{Timeout: 10 * time.Second}

	tests := []struct {
		method          string
		wantBody        string
		wantContentType string
		wantStatus      int
	}{
		{
			method:          http.MethodGet,
			wantBody:        "\n  version: \"v9.8.7-test\"\n",
			wantContentType: "application/yaml",
			wantStatus:      http.StatusOK,
		},
		// The static file server answers every method, so a method the
		// route does not take reaches it, and it must not hold the file.
		{method: http.MethodPost, wantStatus: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(strings.ToLower(tc.method), func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), tc.method, "http://127.0.0.1:"+port+"/openapi.yaml", http.NoBody)
			require.NoError(t, err)
			resp, err := client.Do(request)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, tc.wantStatus, resp.StatusCode)
			if tc.wantContentType != "" {
				require.Equal(t, tc.wantContentType, resp.Header.Get("Content-Type"))
			}
			require.NotContains(t, string(body), "version: dev", "the server answered with the unstamped spec")
			require.Contains(t, string(body), tc.wantBody)
		})
	}
}

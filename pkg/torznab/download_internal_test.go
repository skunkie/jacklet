// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTorrentFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "a plain title is kept", in: "Example Release 2024", want: "Example Release 2024.torrent"},
		{name: "unsafe characters are replaced", in: `Sample/Show: "S01E02"`, want: "Sample_Show_ _S01E02_.torrent"},
		{name: "an empty title gets a placeholder", in: " ... ", want: "torrent.torrent"},
		{
			name: "a long ASCII title is cut at the byte limit",
			in:   strings.Repeat("a", maxFilenameBytes+10),
			want: strings.Repeat("a", maxFilenameBytes) + ".torrent",
		},
		{
			// The cut lands just after the dots, which would otherwise end
			// the name before its extension.
			name: "a cut name loses its trailing dots",
			in:   strings.Repeat("a", maxFilenameBytes-3) + "... more",
			want: strings.Repeat("a", maxFilenameBytes-3) + ".torrent",
		},
		{
			// Each Cyrillic letter is two bytes, and the leading "a" puts
			// the byte limit inside one, so a byte cut would split it.
			name: "a long non-ASCII title is cut at a character boundary",
			in:   "a" + strings.Repeat("Тестовый Релиз ", 20),
			want: "a" + strings.TrimSpace(strings.Repeat("Тестовый Релиз ", 20)[:maxFilenameBytes-2]) + ".torrent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := torrentFilename(tc.in)
			require.Equal(t, tc.want, got)
			require.True(t, utf8.ValidString(got), "the filename is not valid UTF-8")
		})
	}
}

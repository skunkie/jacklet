// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A torrent file is served as Jackett serves it through BencodeNET: keys
// sorted by their bytes at every level, a string's length without leading
// zeros, and nothing after the file.
func TestCanonicalTorrent(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{name: "already sorted", in: "d4:infod6:lengthi5e4:name4:testee", want: "d4:infod6:lengthi5e4:name4:testee"},
		{name: "unsorted keys", in: "d4:name4:test8:announce3:urle", want: "d8:announce3:url4:name4:teste"},
		{name: "unsorted nested keys", in: "d4:infod4:name4:test6:lengthi5eee", want: "d4:infod6:lengthi5e4:name4:testee"},
		{name: "dictionary in a list", in: "d5:filesld4:pathl1:ae6:lengthi1eeee", want: "d5:filesld6:lengthi1e4:pathl1:aeeee"},
		// Keys compare by their bytes, so a prefix sorts first and an
		// uppercase letter before a lowercase one.
		{name: "byte order", in: "d1:b0:2:ab0:1:a0:1:B0:e", want: "d1:B0:1:a0:2:ab0:1:b0:e"},
		{name: "length with leading zeros", in: "d03:key005:valuee", want: "d3:key5:valuee"},
		{name: "negative integer", in: "d1:ai-42ee", want: "d1:ai-42ee"},
		{name: "largest integer", in: "d1:ai9223372036854775807ee", want: "d1:ai9223372036854775807ee"},
		{name: "empty string and list", in: "d0:0:1:llee", want: "d0:0:1:llee"},
		{name: "trailing data dropped", in: "d1:ai1ee\n<!-- served by node 3 -->", want: "d1:ai1ee"},
		{name: "nested to the limit", in: nestedLists(maxBencodeDepth - 1), want: nestedLists(maxBencodeDepth - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalTorrent([]byte(tc.in))
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}
}

// What BencodeNET refuses to parse is refused, so a body Jackett would not
// serve is not served either.
func TestCanonicalTorrent_Refuses(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{name: "empty", in: ""},
		{name: "not a dictionary", in: "l1:ae"},
		{name: "unterminated dictionary", in: "d1:ai1e"},
		{name: "unterminated list", in: "d1:ali1ee"},
		{name: "unterminated integer", in: "d1:ai1"},
		{name: "key without a value", in: "d1:ae"},
		{name: "integer key", in: "di1ei2ee"},
		{name: "duplicate key", in: "d1:ai1e1:ai2ee"},
		{name: "duplicate key apart", in: "d1:ai1e1:bi2e1:ai3ee"},
		{name: "integer with no digits", in: "d1:aiee"},
		{name: "sign with no digits", in: "d1:ai-ee"},
		{name: "plus sign", in: "d1:ai+5ee"},
		{name: "leading zero", in: "d1:ai05ee"},
		{name: "negative zero", in: "d1:ai-0ee"},
		{name: "non-digit in an integer", in: "d1:ai1x2ee"},
		{name: "integer past int64", in: "d1:ai9223372036854775808ee"},
		{name: "integer of twenty digits", in: "d1:ai00000000000000000001ee"},
		{name: "string cut short", in: "d1:a5:abce"},
		{name: "string length of eleven digits", in: "d1:a00000000001:xe"},
		{name: "string length without a colon", in: "d1:a3abce"},
		{name: "unknown value", in: "d1:axe"},
		{name: "nested too deep", in: nestedLists(maxBencodeDepth)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := canonicalTorrent([]byte(tc.in))
			require.Error(t, err)
		})
	}
}

// nestedLists is a dictionary holding lists nested count deep under one
// key.
func nestedLists(count int) string {
	return "d1:a" + strings.Repeat("l", count) + strings.Repeat("e", count) + "e"
}

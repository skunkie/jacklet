// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupportedCategories(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", ID: "hd"},
		{Cat: "Movies", ID: "movies"},
		{Cat: "Movies/HD", ID: "duplicate"},
		{Cat: "Unknown", ID: "unknown"},
	}}}

	got := SupportedCategories(def)
	require.Len(t, got, 2, "want Movies and Movies/HD, deduplicated")
	require.Equal(t, 2000, got[0].ID, "want Movies sorted first")
	require.Equal(t, 2040, got[1].ID, "want Movies/HD sorted second")
}

func TestDefinitionErrorUnwraps(t *testing.T) {
	want := errors.New("invalid definition")
	got := DefinitionError{Err: want, Path: "example.yml"}
	require.ErrorIs(t, got, want, "DefinitionError does not unwrap its cause")
	require.EqualError(t, got, "example.yml: invalid definition")
}

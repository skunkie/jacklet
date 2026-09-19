// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
)

// testPasswordHash derives the hash of testPassword once. PBKDF2 is
// deliberately expensive, so hashing it per test would dominate the
// suite's runtime, especially under -race.
var testPasswordHash = sync.OnceValues(func() (string, error) {
	return admin.HashPassword(testPassword)
})

func TestHashPassword(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)

	t.Run("is self-describing", func(t *testing.T) {
		parts := strings.Split(hash, "$")
		require.Len(t, parts, 4, "hash = %q", hash)
		require.Equal(t, "pbkdf2-sha256", parts[0], "scheme")
		require.Equal(t, "600000", parts[1], "iterations")
	})

	t.Run("does not contain the password", func(t *testing.T) {
		require.NotContains(t, hash, testPassword, "the hash leaks the password")
	})

	t.Run("is salted", func(t *testing.T) {
		other, err := admin.HashPassword(testPassword)
		require.NoError(t, err)
		require.NotEqual(t, hash, other, "hashing the same password twice produced the same hash")
	})

	t.Run("rejects an empty password", func(t *testing.T) {
		_, err := admin.HashPassword("")
		require.Error(t, err)
	})
}

func TestPassword_VerifyAgainstHash(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)

	cred, err := admin.NewPassword("", hash)
	require.NoError(t, err)

	require.True(t, cred.Configured(), "a hash should count as configured")
	require.True(t, cred.Hashed(), "Hashed() should be true")
	require.True(t, cred.Verify(testPassword), "the correct password was rejected")
	require.False(t, cred.Verify("wrong"), "an incorrect password was accepted")
	require.False(t, cred.Verify(""), "an empty password was accepted")
}

func TestPassword_Plaintext(t *testing.T) {
	cred, err := admin.NewPassword(testPassword, "")
	require.NoError(t, err)

	require.False(t, cred.Hashed(), "Hashed() should be false for a plaintext credential")
	require.True(t, cred.Verify(testPassword), "the correct password was rejected")
	require.False(t, cred.Verify("wrong"), "an incorrect password was accepted")
}

// A hash must win over a plaintext value, so setting both gets the safer
// credential rather than the weaker one.
func TestPassword_HashTakesPrecedence(t *testing.T) {
	hash, err := admin.HashPassword("from-the-hash")
	require.NoError(t, err)

	cred, err := admin.NewPassword("from-the-plaintext", hash)
	require.NoError(t, err)

	require.True(t, cred.Verify("from-the-hash"), "the hashed password was rejected")
	require.False(t, cred.Verify("from-the-plaintext"),
		"the plaintext password was accepted even though a hash was configured")
}

// A malformed hash must fail loudly: silently ignoring it would leave the
// panel accepting some other password, or none.
func TestPassword_RejectsMalformedHash(t *testing.T) {
	tests := []struct {
		hash string
		name string
	}{
		{hash: "hunter2", name: "not a hash at all"},
		{hash: "pbkdf2-sha256$600000$abc", name: "wrong field count"},
		{hash: "scrypt$600000$YWJj$YWJj", name: "unknown scheme"},
		{hash: "pbkdf2-sha256$zero$YWJj$YWJj", name: "bad iteration count"},
		{hash: "pbkdf2-sha256$0$YWJj$YWJj", name: "zero iterations"},
		{hash: "pbkdf2-sha256$600000$!!!$YWJj", name: "bad salt encoding"},
		{hash: "pbkdf2-sha256$600000$YWJj$!!!", name: "bad key encoding"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.NewPassword("", tc.hash)
			require.Error(t, err, "a malformed hash produced a usable credential")
			require.ErrorIs(t, err, admin.ErrMalformedHash)
		})
	}
}

func TestPassword_Unconfigured(t *testing.T) {
	cred, err := admin.NewPassword("", "")
	require.NoError(t, err)
	require.False(t, cred.Configured(), "an empty credential should not count as configured")
	require.False(t, cred.Verify(""), "an unconfigured credential must reject everything")
	require.False(t, cred.Verify("anything"), "an unconfigured credential must reject everything")
}

// Surrounding whitespace is easy to pick up from a shell or a YAML file
// and must not stop a hash being recognized.
func TestPassword_TolerantOfSurroundingWhitespace(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)

	cred, err := admin.NewPassword("", "  "+hash+"\n")
	require.NoError(t, err, "a padded hash was rejected")
	require.True(t, cred.Verify(testPassword), "the correct password was rejected")
}

// The panel must actually authenticate against a hashed credential.
func TestAdmin_SignsInWithHashedPassword(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)

	f := newPanelWithHash(t, hash)
	cookie, _ := f.signIn(t)
	require.NotNil(t, cookie, "no session cookie")

	page := f.get(t, "/admin", cookie)
	require.Contains(t, page, "Demo Tracker",
		"the dashboard did not render for a hash-authenticated session")
}

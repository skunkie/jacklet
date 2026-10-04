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

func TestPassword_Verify_AgainstHash(t *testing.T) {
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

func TestPasswordPlaintext(t *testing.T) {
	cred, err := admin.NewPassword(testPassword, "")
	require.NoError(t, err)

	require.False(t, cred.Hashed(), "Hashed() should be false for a plaintext credential")
	require.True(t, cred.Verify(testPassword), "the correct password was rejected")
	require.False(t, cred.Verify("wrong"), "an incorrect password was accepted")
}

// A hash must win over a plaintext value, so setting both gets the safer
// credential rather than the weaker one.
func TestPasswordHashTakesPrecedence(t *testing.T) {
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
func TestPasswordRejectsMalformedHash(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{name: "not a hash at all", hash: "hunter2"},
		{name: "wrong field count", hash: "pbkdf2-sha256$600000$abc"},
		{name: "unknown scheme", hash: "scrypt$600000$YWJj$YWJj"},
		{name: "bad iteration count", hash: "pbkdf2-sha256$zero$YWJj$YWJj"},
		{name: "zero iterations", hash: "pbkdf2-sha256$0$YWJj$YWJj"},
		{name: "bad salt encoding", hash: "pbkdf2-sha256$600000$!!!$YWJj"},
		{name: "bad key encoding", hash: "pbkdf2-sha256$600000$YWJj$!!!"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.NewPassword("", tc.hash)
			require.Error(t, err, "a malformed hash produced a usable credential")
			require.ErrorIs(t, err, admin.ErrMalformedHash)
		})
	}
}

// A hash short of fields has usually had a "$" expanded on its way into
// the environment, and the error is the operator's only clue to that.
func TestNewPassword_ExpandedHash(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)
	parts := strings.Split(hash, "$")

	tests := []struct {
		name     string
		hash     string
		wantHint bool
	}{
		// "$<salt>" read as a variable that is not set.
		{name: "salt expanded to nothing", hash: parts[0] + "$" + parts[1] + "$" + parts[3], wantHint: true},
		// A shell expanding inside double quotes consumes every "$", fusing
		// whatever survives into one field.
		{name: "every separator consumed", hash: strings.ReplaceAll(hash, "$", ""), wantHint: true},
		{name: "a field too many", hash: hash + "$extra", wantHint: false},
		// A plaintext password in the hash variable has no "$" to expand.
		{name: "a plaintext password", hash: "hunter2", wantHint: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.NewPassword("", tc.hash)
			require.ErrorIs(t, err, admin.ErrMalformedHash)
			if tc.wantHint {
				require.ErrorContains(t, err, "write each $ as $$",
					"the error did not say how a hash loses fields")
			} else {
				require.NotContains(t, err.Error(), "expanded",
					"nothing was expanded, so suggesting it misleads")
			}
		})
	}
}

func TestPasswordUnconfigured(t *testing.T) {
	cred, err := admin.NewPassword("", "")
	require.NoError(t, err)
	require.False(t, cred.Configured(), "an empty credential should not count as configured")
	require.False(t, cred.Verify(""), "an unconfigured credential must reject everything")
	require.False(t, cred.Verify("anything"), "an unconfigured credential must reject everything")
}

// Surrounding whitespace is easy to pick up from a shell or a YAML file
// and must not stop a hash being recognized.
func TestPasswordTolerantOfSurroundingWhitespace(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)

	cred, err := admin.NewPassword("", "  "+hash+"\n")
	require.NoError(t, err, "a padded hash was rejected")
	require.True(t, cred.Verify(testPassword), "the correct password was rejected")
}

// The panel must actually authenticate against a hashed credential.
func TestAdminSignsInWithHashedPassword(t *testing.T) {
	hash, err := testPasswordHash()
	require.NoError(t, err)

	fixture := newPanelWithHash(t, hash)
	cookie, _ := fixture.signIn(t)
	require.NotNil(t, cookie, "no session cookie")

	page := fixture.get(t, "/admin", cookie)
	require.Contains(t, page, "Demo Tracker",
		"the dashboard did not render for a hash-authenticated session")
}

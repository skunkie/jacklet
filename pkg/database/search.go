// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"database/sql/driver"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// foldFunction is the SQL function name registered for Unicode-aware case
// folding.
const foldFunction = "unicode_lower"

// foldedNameExpr is the SQL expression to compare a torrent name against a
// pattern from likePattern. SQLite's own LIKE and lower() fold case for
// ASCII only, so a search for "тест" would never match a stored
// "Тест" — this applies Go's Unicode-aware lowercasing instead.
const foldedNameExpr = foldFunction + "(name)"

// registerFold installs the folding function exactly once per process. The
// driver applies it to connections opened afterwards, so this must run
// before sql.Open.
var registerFold = sync.OnceValue(func() error {
	return sqlite.RegisterDeterministicScalarFunction(foldFunction, 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if len(args) != 1 {
				return nil, nil
			}
			s, ok := args[0].(string)
			if !ok {
				return args[0], nil
			}
			return foldCase(s), nil
		})
})

// foldCase normalizes text for case-insensitive comparison. It is applied
// to both the stored name and the search term, so the two are folded the
// same way.
func foldCase(s string) string {
	return strings.ToLower(s)
}

// likePattern turns one search term into a pattern for foldedNameExpr: it
// folds the term's case and escapes the LIKE metacharacters, so a term
// containing "%" or "_" matches those characters literally. Use it with
// `ESCAPE '\'`.
func likePattern(term string) string {
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(foldCase(term))
	return "%" + escaped + "%"
}

// nameMatchClause builds the SQL fragment and bound arguments requiring a
// torrent's name to contain every one of terms, case-insensitively. An
// empty terms list yields an empty fragment, which matches everything.
func nameMatchClause(terms []string) (string, []any) {
	var clause strings.Builder
	args := make([]any, 0, len(terms))
	for _, term := range terms {
		clause.WriteString(" AND " + foldedNameExpr + ` LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(term))
	}
	return clause.String(), args
}

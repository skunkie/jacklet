// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package routetest holds what the tests that keep the OpenAPI spec and the
// registered routes in step share: the operations the spec documents and a
// request target built from a path template; the patterns a function
// registers on a mux, read out of its source because a *http.ServeMux will
// not list them, and the identifier check that recognizes the mux; the
// operation a pattern serves, including HEAD on a GET route; the rules
// every registered route is held to, such as naming its method; and what
// a route's handler does, the statuses it writes, the query parameters it
// reads and the content types it sets, read out of the package's source,
// against what the spec documents for its operation.
// pkg/torznab's tests hold the API's paths to Routes, and cmd/jacklet's
// hold the binary's own paths to run; each finds its mux its own way and
// hands it here.
package routetest

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// APIPrefix begins every path pkg/torznab's Routes registers. The spec's
// other paths are the binary's own, registered by cmd/jacklet's run.
const APIPrefix = "/api/v2.0/"

// methods are the keys of an OpenAPI path item that name an operation, as
// opposed to its summary, parameters and the like.
var methods = []string{"delete", "get", "head", "options", "patch", "post", "put", "trace"}

// pathParameter matches a path template's parameter.
var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

// Operations returns the operations spec documents under every path
// include accepts, each as "METHOD /path" with the method upper-cased, the
// way a ServeMux pattern spells it.
func Operations(tb testing.TB, spec []byte, include func(path string) bool) map[string]bool {
	tb.Helper()
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(tb, yaml.Unmarshal(spec, &doc), "failed to read the spec's paths as path items")

	documented := make(map[string]bool)
	for _, path := range slices.Sorted(maps.Keys(doc.Paths)) {
		if !include(path) {
			continue
		}
		for key := range doc.Paths[path] {
			if slices.Contains(methods, key) {
				documented[strings.ToUpper(key)+" "+path] = true
			}
		}
	}
	return documented
}

// SampleTarget fills each parameter in a path template with a sample
// value, giving a path a request can be sent to.
func SampleTarget(path string) string {
	return pathParameter.ReplaceAllString(path, "sample")
}

// Operation returns a registered pattern's method, "" for a method-less
// one, and the operation it serves as the spec spells it: the pattern less
// a trailing "{$}", since net/http's trailing-slash form of a path is the
// same route to a client and is documented once, under the path without
// it.
func Operation(pattern string) (method, operation string) {
	method, path, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod {
		method, path = "", pattern
	}
	path = strings.TrimSuffix(path, "/{$}")
	return method, strings.TrimSpace(method + " " + path)
}

// IsIdent reports whether an expression is the identifier named name, the
// way a mux variable is recognized wherever it appears.
func IsIdent(node ast.Expr, name string) bool {
	ident, ok := node.(*ast.Ident)
	return ok && ident.Name == name
}

// Serves reports whether a route registered as operation serves the
// documented one: the same operation, or HEAD on the path of a GET route,
// which net/http answers with the GET handler.
func Serves(operation, documented string) bool {
	if operation == documented {
		return true
	}
	path, isHead := strings.CutPrefix(documented, http.MethodHead+" ")
	return isHead && operation == http.MethodGet+" "+path
}

// AssertDocumented checks patterns, the routes to check, against the
// operations the spec documents; registered is every route owner
// registers, which a trailing-slash form's base is looked up in, so a
// caller can leave routes it exempts out of patterns alone. A pattern must
// name its method, since a method-less one answers every method whether
// documented or not; a trailing-slash form must have its base registered;
// and the operation documentedAs names for the pattern must be documented.
// A nil documentedAs names it as Operation does. Every failure is
// reported, not only the first, so one run lists all the drift.
func AssertDocumented(tb testing.TB, owner string, patterns, registered []string, documented map[string]bool, documentedAs func(pattern string) string) {
	tb.Helper()
	if documentedAs == nil {
		documentedAs = func(pattern string) string {
			_, operation := Operation(pattern)
			return operation
		}
	}
	for _, pattern := range patterns {
		if method, _ := Operation(pattern); !assert.NotEmpty(tb, method,
			"%s registers %q, which names no method, so it answers every method the spec does not document", owner, pattern) {
			continue
		}
		if base, ok := strings.CutSuffix(pattern, "/{$}"); ok {
			assert.True(tb, slices.Contains(registered, base),
				"%q is the trailing-slash form of %q, which %s does not register", pattern, base, owner)
		}
		operation := documentedAs(pattern)
		message := fmt.Sprintf("%s registers the route %q, but the spec does not document it", owner, pattern)
		if operation != pattern {
			message += fmt.Sprintf(" as %q", operation)
		}
		assert.True(tb, documented[operation], message)
	}
}

// Route is a pattern a function registers and the handler it registers it
// with, as written in the call.
type Route struct {
	Handler ast.Expr
	Pattern string
}

// Routes returns the routes body registers on the mux variable named mux,
// read from every Handle and HandleFunc call on it: the pattern from its
// first argument and the handler from its second. A pattern that is not a
// string literal fails the test, as does any
// use of the variable that is neither such a call nor in accounted, the
// uses the caller has already vouched for: handing the mux to a helper or
// copying it to another variable would register routes no one reads, and
// a route the test cannot read is a route it does not cover. owner names
// the function in failure messages.
func Routes(tb testing.TB, fset *token.FileSet, owner string, body *ast.BlockStmt, mux string, accounted map[*ast.Ident]bool) []Route {
	tb.Helper()

	known := maps.Clone(accounted)
	if known == nil {
		known = make(map[*ast.Ident]bool)
	}

	var routes []Route
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !IsIdent(sel.X, mux) || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return true
		}
		require.Len(tb, call.Args, 2, "%s calls %s without a pattern and a handler", owner, sel.Sel.Name)
		lit, ok := call.Args[0].(*ast.BasicLit)
		require.True(tb, ok && lit.Kind == token.STRING,
			"%s: %s registers a route whose pattern is not a string literal", fset.Position(call.Pos()), owner)
		pattern, err := strconv.Unquote(lit.Value)
		require.NoError(tb, err)
		routes = append(routes, Route{Handler: call.Args[1], Pattern: pattern})
		known[sel.X.(*ast.Ident)] = true
		return true
	})

	ast.Inspect(body, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && ident.Name == mux {
			require.True(tb, known[ident],
				"%s: %s uses its mux other than to register a route on it, so the routes registered through that use cannot be read",
				fset.Position(ident.Pos()), owner)
		}
		return true
	})

	require.NotEmpty(tb, routes, "%s registers no routes; this test has stopped covering it", owner)
	return routes
}

// RoutePatterns returns the patterns of routes, in the order they were
// registered.
func RoutePatterns(routes []Route) []string {
	patterns := make([]string, len(routes))
	for i, route := range routes {
		patterns[i] = route.Pattern
	}
	return patterns
}

// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package routetest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServes(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		documented, operation string
		want                  bool
	}{
		{name: "same operation", documented: "GET /healthz", operation: "GET /healthz", want: true},
		{name: "head on a get route", documented: "HEAD /healthz", operation: "GET /healthz", want: true},
		{name: "get on a head route", documented: "GET /healthz", operation: "HEAD /healthz", want: false},
		{name: "other method", documented: "POST /healthz", operation: "GET /healthz", want: false},
		{name: "head on another path", documented: "HEAD /docs", operation: "GET /healthz", want: false},
		{name: "method-less route", documented: "GET /healthz", operation: "/healthz", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Serves(tc.operation, tc.documented))
		})
	}
}

func TestOperation(t *testing.T) {
	for _, tc := range []struct {
		pattern                   string
		wantMethod, wantOperation string
	}{
		{pattern: "GET /healthz", wantMethod: "GET", wantOperation: "GET /healthz"},
		{pattern: "GET /api/v2.0/indexers/{$}", wantMethod: "GET", wantOperation: "GET /api/v2.0/indexers"},
		{pattern: "GET /api/v2.0/indexers/{id}/results/torznab/{ignored}", wantMethod: "GET", wantOperation: "GET /api/v2.0/indexers/{id}/results/torznab/{ignored}"},
		{pattern: "/healthz", wantMethod: "", wantOperation: "/healthz"},
		{pattern: "/static/{$}", wantMethod: "", wantOperation: "/static"},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			method, operation := Operation(tc.pattern)
			require.Equal(t, tc.wantMethod, method)
			require.Equal(t, tc.wantOperation, operation)
		})
	}
}

func TestSampleTarget(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/healthz", "/healthz"},
		{"/api/v2.0/indexers/{id}/results", "/api/v2.0/indexers/sample/results"},
		{"/api/v2.0/indexers/{id}/download/{row}", "/api/v2.0/indexers/sample/download/sample"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, SampleTarget(tc.in))
		})
	}
}

func TestOperations(t *testing.T) {
	spec := []byte(`
paths:
  /api/v2.0/indexers:
    summary: not an operation
    parameters: []
    get:
      summary: x
  /healthz:
    get:
      summary: x
    head:
      summary: x
`)
	require.Equal(t,
		map[string]bool{"GET /healthz": true, "HEAD /healthz": true},
		Operations(t, spec, func(path string) bool { return path == "/healthz" }),
		"only the included path's operations are listed, and only the keys that name a method")
	require.Equal(t,
		map[string]bool{"GET /api/v2.0/indexers": true, "GET /healthz": true, "HEAD /healthz": true},
		Operations(t, spec, func(string) bool { return true }))
}

// failureRecorder is a testing.TB that records a failure instead of
// reporting it, so a test can assert that a check fails. FailNow ends the
// calling goroutine as testing's own does, so recordFailures runs a check
// that may call it on a goroutine of its own.
type failureRecorder struct {
	testing.TB
	messages []string
}

func (r *failureRecorder) Helper() {}

func (r *failureRecorder) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *failureRecorder) FailNow() { runtime.Goexit() }

// recordFailures runs check with a failureRecorder and returns the
// failures it recorded.
func recordFailures(t *testing.T, check func(tb testing.TB)) []string {
	t.Helper()
	recorder := &failureRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		check(recorder)
	}()
	<-done
	return recorder.messages
}

// routesOf parses a function named routes from src and runs Routes on its
// body, with its parameter named mux as the mux and the identifiers vouch
// picks as accounted for. It returns each route as its pattern and its
// handler as written, or the failure messages when Routes failed.
func routesOf(t *testing.T, src string, vouch func(*ast.FuncDecl) map[*ast.Ident]bool) (routes map[string]string, failures []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "routes.go", "package sample\n"+src, 0)
	require.NoError(t, err)
	decl := file.Decls[0].(*ast.FuncDecl)

	var accounted map[*ast.Ident]bool
	if vouch != nil {
		accounted = vouch(decl)
	}
	failures = recordFailures(t, func(tb testing.TB) {
		tb.Helper()
		routes = make(map[string]string)
		for _, route := range Routes(tb, fset, "routes", decl.Body, "mux", accounted) {
			routes[route.Pattern] = types.ExprString(route.Handler)
		}
	})
	return routes, failures
}

func TestRoutes(t *testing.T) {
	t.Run("reads every literal pattern and its handler", func(t *testing.T) {
		routes, failures := routesOf(t, `func routes(mux *http.ServeMux) {
	mux.Handle("GET /a", h)
	mux.HandleFunc("GET /b/{$}", s.B)
	other.HandleFunc("GET /elsewhere", f)
}`, nil)
		require.Empty(t, failures)
		require.Equal(t, map[string]string{"GET /a": "h", "GET /b/{$}": "s.B"}, routes,
			"a Handle call on another receiver is not a route on this mux")
	})

	t.Run("accepts a vouched-for use", func(t *testing.T) {
		routes, failures := routesOf(t, `func routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /a", f)
	serve(mux)
}`, func(decl *ast.FuncDecl) map[*ast.Ident]bool {
			call := decl.Body.List[1].(*ast.ExprStmt).X.(*ast.CallExpr)
			return map[*ast.Ident]bool{call.Args[0].(*ast.Ident): true}
		})
		require.Empty(t, failures)
		require.Equal(t, map[string]string{"GET /a": "f"}, routes)
	})

	for _, tc := range []struct {
		name, src, want string
	}{
		{
			name: "fails on a copy of the mux",
			src: `func routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /a", f)
	other := mux
	other.HandleFunc("GET /b", f)
}`,
			want: "routes.go:4:11: routes uses its mux other than to register a route on it, so the routes registered through that use cannot be read",
		},
		{
			name: "fails on the mux handed to a helper",
			src: `func routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /a", f)
	register(mux)
}`,
			want: "routes.go:4:11: routes uses its mux other than to register a route on it, so the routes registered through that use cannot be read",
		},
		{
			name: "fails on a pattern that is not a literal",
			src: `func routes(mux *http.ServeMux) {
	mux.HandleFunc(prefix+"/a", f)
}`,
			want: "routes.go:3:2: routes registers a route whose pattern is not a string literal",
		},
		{
			name: "fails when nothing is registered",
			src:  `func routes(mux *http.ServeMux) {}`,
			want: "routes registers no routes; this test has stopped covering it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, failures := routesOf(t, tc.src, nil)
			require.Len(t, failures, 1)
			require.Contains(t, failures[0], tc.want)
		})
	}
}

func TestAssertDocumented(t *testing.T) {
	aliases := map[string]string{"GET /feed": "GET /feed/api"}
	documentedAs := func(pattern string) string {
		_, operation := Operation(pattern)
		if alias, ok := aliases[operation]; ok {
			return alias
		}
		return operation
	}
	documented := map[string]bool{"GET /healthz": true, "GET /feed/api": true}

	for _, tc := range []struct {
		name     string
		patterns []string
		want     []string
	}{
		{name: "documented routes pass", patterns: []string{"GET /healthz", "GET /healthz/{$}", "GET /feed"}},
		{
			name:     "a method-less route fails",
			patterns: []string{"/healthz"},
			want:     []string{`owner registers "/healthz", which names no method, so it answers every method the spec does not document`},
		},
		{
			name:     "a trailing-slash form without its base fails",
			patterns: []string{"GET /status/{$}"},
			want: []string{
				`"GET /status/{$}" is the trailing-slash form of "GET /status", which owner does not register`,
				`owner registers the route "GET /status/{$}", but the spec does not document it as "GET /status"`,
			},
		},
		{
			name:     "an undocumented route fails under its own name",
			patterns: []string{"GET /status"},
			want:     []string{`owner registers the route "GET /status", but the spec does not document it`},
		},
		{
			name:     "every failure is reported",
			patterns: []string{"/a", "GET /b"},
			want: []string{
				`owner registers "/a", which names no method`,
				`owner registers the route "GET /b", but the spec does not document it`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registered := append([]string{"GET /healthz"}, tc.patterns...)
			recorder := &failureRecorder{TB: t}
			AssertDocumented(recorder, "owner", tc.patterns, registered, documented, documentedAs)
			require.Len(t, recorder.messages, len(tc.want), "%q", recorder.messages)
			for i, want := range tc.want {
				require.Contains(t, recorder.messages[i], want)
			}
		})
	}

	t.Run("a nil documentedAs names a route as Operation does", func(t *testing.T) {
		recorder := &failureRecorder{TB: t}
		AssertDocumented(recorder, "owner", []string{"GET /healthz/{$}"}, []string{"GET /healthz", "GET /healthz/{$}"}, documented, nil)
		require.Empty(t, recorder.messages)
		AssertDocumented(recorder, "owner", []string{"GET /feed"}, []string{"GET /feed"}, documented, nil)
		require.Len(t, recorder.messages, 1, "without documentedAs, no alias applies")
		require.Contains(t, recorder.messages[0], `owner registers the route "GET /feed", but the spec does not document it`)
	})
}

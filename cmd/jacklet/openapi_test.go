// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/internal/routetest"
)

// muxRecipients are the calls run may hand its mux to, each with where the
// routes it registers are held to the spec. Any other call receiving it
// fails the test, since routes registered there would go unread.
var muxRecipients = map[string]string{
	"stack.Routes": "the API, which pkg/torznab's tests hold to the spec, and the admin panel, which is not in it",
}

// undocumentedRoutes are the patterns run registers that are not API
// endpoints, each with the reason it has no place in the spec.
var undocumentedRoutes = map[string]string{
	"/":         "serves the embedded static assets, the spec itself among them",
	"GET /docs": "serves the API reference, a page for a person",
	"GET /{$}":  "redirects the bare root to /docs",
}

// TestOpenAPIPathsMatchTheRoutes holds the spec's paths outside the API to
// the routes run registers, in both directions: each documented operation
// must be a pattern run registers, and each pattern run registers, read out
// of its source since a *http.ServeMux will not list them, must be
// documented or listed in undocumentedRoutes.
//
// The test lives beside run rather than with the rest of the spec's tests
// in pkg/torznab, so a change to how the binary builds its mux is caught
// here and does not break a library package's tests.
//
// Each documented operation is also sent to a running server, which is
// worth a port and a database because it is the one check the source cannot
// make: that a route read out of run is really registered when run runs,
// rather than in a branch it never takes, and that its handler answers its
// own path. A handler that itself answers 404 or 405 for a documented path
// therefore fails as a route the server does not answer.
func TestOpenAPIPathsMatchTheRoutes(t *testing.T) {
	spec, err := publicFS.ReadFile("public/openapi.yaml")
	require.NoError(t, err, "failed to read the embedded spec")
	documented := routetest.Operations(t, spec, func(path string) bool {
		return !strings.HasPrefix(path, routetest.APIPrefix)
	})

	// The source is read first: it needs no server, and a run it can no
	// longer read fails here before one is started.
	_, routes, recipients := runRoutes(t)
	registered := routetest.RoutePatterns(routes)

	port := freePort(t)
	startServer(t, testSettings(t, port))

	// The first answer is the one that says whether the path is routed,
	// so redirects are not followed.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}

	for _, operation := range slices.Sorted(maps.Keys(documented)) {
		method, path, _ := strings.Cut(operation, " ")
		// The static file server answers every path a file exists at, so
		// an answer alone does not mean a route serves the operation.
		isRegistered := slices.ContainsFunc(registered, func(pattern string) bool {
			_, served := routetest.Operation(pattern)
			return routetest.Serves(served, operation)
		})
		if !assert.True(t, isRegistered, "the spec documents %q, which no route run registers serves", operation) {
			continue
		}
		request, err := http.NewRequestWithContext(t.Context(), method, fmt.Sprintf("http://127.0.0.1:%s%s", port, routetest.SampleTarget(path)), http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(request)
		require.NoError(t, err)
		resp.Body.Close()
		assert.NotContains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, resp.StatusCode,
			"run registers a route for %q, but the running server does not answer it", operation)
	}

	var checked []string
	for _, pattern := range registered {
		if undocumentedRoutes[pattern] != "" {
			continue
		}
		method, operation := routetest.Operation(pattern)
		if strings.HasPrefix(strings.TrimPrefix(operation, method+" "), routetest.APIPrefix) {
			assert.Failf(t, "API route registered by run",
				"run registers %q, under %s, where every route belongs in torznab's Routes", pattern, routetest.APIPrefix)
			continue
		}
		checked = append(checked, pattern)
	}
	routetest.AssertDocumented(t, "run", checked, registered, documented, nil)

	// An entry left behind by a route that was removed would otherwise go
	// on excusing a pattern nobody registers.
	for _, pattern := range slices.Sorted(maps.Keys(undocumentedRoutes)) {
		assert.Contains(t, registered, pattern, "%q is listed as undocumented, but run does not register it", pattern)
	}
	for _, callee := range slices.Sorted(maps.Keys(muxRecipients)) {
		assert.True(t, recipients[callee], "%s is listed in muxRecipients, but run does not hand it the mux", callee)
	}
}

// TestOpenAPIOperationsMatchTheHandlers holds what the spec documents for
// each of the binary's own operations to what its handler's code does, in
// both directions, as pkg/torznab's test of the same name does for the
// API: the statuses it can write, the query parameters it reads and the
// content types it sets.
func TestOpenAPIOperationsMatchTheHandlers(t *testing.T) {
	spec, err := publicFS.ReadFile("public/openapi.yaml")
	require.NoError(t, err, "failed to read the embedded spec")
	documented := routetest.Document(t, spec, func(path string) bool {
		return !strings.HasPrefix(path, routetest.APIPrefix)
	})

	source, routes, _ := runRoutes(t)
	behaviors := make(map[string]routetest.Behavior)
	for operation := range documented {
		for _, route := range routes {
			if _, served := routetest.Operation(route.Pattern); routetest.Serves(served, operation) {
				behaviors[operation] = behaviors[operation].Merge(source.Behavior(t, route.Handler))
			}
		}
	}
	routetest.AssertBehavior(t, documented, behaviors, nil)
}

// TestOpenAPISecurityMatchesTheServer holds every documented operation's
// security to a server started with an API key: one the spec secures
// answers 401 without the key and something else with it, and one the
// spec leaves open, such as /healthz for an orchestrator's probe, never
// answers 401. A secured endpoint that lets a request through is a key
// that protects nothing, and an open one that refuses it is a probe that
// reports a healthy server as down.
func TestOpenAPISecurityMatchesTheServer(t *testing.T) {
	spec, err := publicFS.ReadFile("public/openapi.yaml")
	require.NoError(t, err, "failed to read the embedded spec")
	documented := routetest.Document(t, spec, func(string) bool { return true })

	const sampleKey = "sample-api-key"
	port := freePort(t)
	cfg := testSettings(t, port)
	cfg.APIKey = sampleKey
	startServer(t, cfg)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	status := func(method, path, key string) int {
		t.Helper()
		target := fmt.Sprintf("http://127.0.0.1:%s%s", port, routetest.SampleTarget(path))
		if key != "" {
			target += "?apikey=" + key
		}
		request, err := http.NewRequestWithContext(t.Context(), method, target, http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(request)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, operation := range slices.Sorted(maps.Keys(documented)) {
		method, path, _ := strings.Cut(operation, " ")
		if documented[operation].IsSecured {
			assert.Equal(t, http.StatusUnauthorized, status(method, path, ""),
				"the spec secures %s, but it answers without the API key", operation)
			assert.NotEqual(t, http.StatusUnauthorized, status(method, path, sampleKey),
				"the spec secures %s with the API key, but it refuses the key", operation)
		} else {
			assert.NotEqual(t, http.StatusUnauthorized, status(method, path, ""),
				"the spec leaves %s open, but it asks for the API key", operation)
		}
	}
}

// runRoutes returns this package's source, the routes run registers on the
// mux its http.Server serves, and the muxRecipients calls it hands that
// mux to.
// Building the mux, naming it as the server's Handler and handing it to a
// muxRecipients call are the uses of it vouched for here; routetest.Routes
// reads the routes and fails on any other use.
func runRoutes(t *testing.T) (source *routetest.Source, routes []routetest.Route, recipients map[string]bool) {
	t.Helper()

	source = routetest.LoadSource(t, ".")
	decl := source.Func(t, "", "run")
	served := servedMux(t, decl)
	mux := served.Name

	// The server's own Handler field is the one use that hands the mux
	// on without registering through it, and one assignment builds it;
	// a second would leave routes on an instance the server never sees.
	accounted := map[*ast.Ident]bool{served: true}
	recipients = make(map[string]bool)
	var builds int
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if call, ok := n.Rhs[0].(*ast.CallExpr); ok && len(n.Lhs) == 1 && types.ExprString(call.Fun) == "http.NewServeMux" {
				if ident, ok := n.Lhs[0].(*ast.Ident); ok && ident.Name == mux {
					accounted[ident] = true
					builds++
				}
			}
		case *ast.CallExpr:
			callee := types.ExprString(n.Fun)
			for _, arg := range n.Args {
				if routetest.IsIdent(arg, mux) && muxRecipients[callee] != "" {
					recipients[callee] = true
					accounted[arg.(*ast.Ident)] = true
				}
			}
		}
		return true
	})
	require.Equal(t, 1, builds, "want run to build the mux its server serves exactly once; this test needs updating")

	return source, routetest.Routes(t, source.Fset, "run", decl.Body, mux, accounted), recipients
}

// servedMux returns the mux variable run's http.Server is handed as its
// Handler, as it appears in that field. Naming it by what the server
// serves keeps a second mux built for something else from being read in
// its place.
func servedMux(t *testing.T, decl *ast.FuncDecl) *ast.Ident {
	t.Helper()
	var muxes []*ast.Ident
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok || types.ExprString(lit.Type) != "http.Server" {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok && types.ExprString(kv.Key) == "Handler" {
				ident, ok := kv.Value.(*ast.Ident)
				require.True(t, ok, "run's server is handed %s rather than a mux variable; this test needs updating", types.ExprString(kv.Value))
				muxes = append(muxes, ident)
			}
		}
		return true
	})
	require.Len(t, muxes, 1, "want run to build exactly one http.Server with a Handler; this test needs updating")
	return muxes[0]
}

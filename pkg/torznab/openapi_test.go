// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"fmt"
	"go/ast"
	"go/token"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/internal/routetest"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// These tests walk the whole spec and report every point of drift in one
// run, so the loops below assert rather than require: stopping at the
// first mismatch would hide the rest of the work a contributor has to do.

// These tests hold cmd/jacklet/public/openapi.yaml to what the handlers in
// this package actually serve, and to the repository's ordering
// conventions for lookup tables and parameters. The paths outside
// /api/v2.0/ are the binary's own, so cmd/jacklet's tests hold those to
// the routes it registers. The spec is served to every Torznab client's
// operator through /docs, so it drifting from the code is a documentation
// bug that nothing else would catch.
//
// They live here, reaching across to cmd/jacklet, because this is the only
// package that can see both the spec and the response structs — those are
// unexported, and exporting an internal wire shape to make a test's life
// easier would be the wrong trade.

// specPath is the spec, relative to this package's directory.
var specPath = filepath.Join("..", "..", "cmd", "jacklet", "public", "openapi.yaml")

// loadSpec parses the spec into a yaml.Node tree, which — unlike
// unmarshaling into a map — preserves the order keys are written in.
// Order is the whole point of half of these tests.
func loadSpec(t *testing.T) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(specPath)
	require.NoError(t, err, "failed to read the spec")

	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal(data, &doc), "the spec is not valid YAML")
	require.Len(t, doc.Content, 1, "want exactly one YAML document")
	return doc.Content[0]
}

// mapKeys returns a mapping node's keys, in document order.
func mapKeys(node *yaml.Node) []string {
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		keys = append(keys, node.Content[i].Value)
	}
	return keys
}

// child returns the value node stored under key, or nil.
func child(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// mustChild is child for a key the spec is required to have.
func mustChild(t *testing.T, node *yaml.Node, key string) *yaml.Node {
	t.Helper()
	got := child(node, key)
	require.NotNil(t, got, "the spec has no %q", key)
	return got
}

// schemas returns the component schemas, keyed by name.
func schemas(t *testing.T, root *yaml.Node) map[string]*yaml.Node {
	t.Helper()
	node := mustChild(t, mustChild(t, root, "components"), "schemas")
	out := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		out[node.Content[i].Value] = node.Content[i+1]
	}
	return out
}

// TestOpenAPILookupSectionsAreAlphabetical covers the ordering rule for
// the parts of the spec that are lookup tables: nobody reads
// components/parameters top to bottom, so alphabetical is what makes an
// entry findable — and what tells a contributor adding a parameter where
// it goes, instead of at the end.
func TestOpenAPILookupSectionsAreAlphabetical(t *testing.T) {
	root := loadSpec(t)
	components := mustChild(t, root, "components")

	sorted := func(label string, keys []string) {
		t.Helper()
		want := slices.Clone(keys)
		slices.Sort(want)
		assert.Equal(t, want, keys, "%s is not alphabetical", label)
	}

	sorted("paths", mapKeys(mustChild(t, root, "paths")))
	sorted("components", mapKeys(components))
	for _, section := range []string{"parameters", "responses", "schemas"} {
		sorted("components/"+section, mapKeys(mustChild(t, components, section)))
	}
	for name, schema := range schemas(t, root) {
		if props := child(schema, "properties"); props != nil {
			sorted("properties of "+name, mapKeys(props))
		}
	}
}

// TestOpenAPIRequiredMatchesProperties pins the invariant that makes the
// spec self-checking: every field of every response struct is emitted
// unconditionally — none carries "omitempty" — so a property that is not
// required would be describing a response Jacklet never sends.
func TestOpenAPIRequiredMatchesProperties(t *testing.T) {
	for name, schema := range schemas(t, loadSpec(t)) {
		props := child(schema, "properties")
		if props == nil {
			continue
		}
		want := mapKeys(props)
		slices.Sort(want)

		requiredNode := child(schema, "required")
		if !assert.NotNil(t, requiredNode, "%s: declares properties but no required list", name) {
			continue
		}
		got := make([]string, 0, len(requiredNode.Content))
		for _, item := range requiredNode.Content {
			got = append(got, item.Value)
		}
		assert.Equal(t, want, got, "%s: required does not match its properties", name)
	}
}

// TestOpenAPISchemasMatchTheResponseStructs is the check that actually
// stops drift: adding a field to one of these structs without documenting
// it, or renaming its JSON tag, fails here rather than silently leaving
// /docs describing a response that no longer exists.
func TestOpenAPISchemasMatchTheResponseStructs(t *testing.T) {
	// ErrorResponse has no struct of its own — writeJSONError encodes a
	// map — so it is checked by hand below rather than by reflection.
	cases := map[string]any{
		"JackettCapability": jackettCapability{},
		"JackettIndexer":    jackettIndexer{},
		"JackettResult":     jackettResult{},
		"SearchIndexer":     searchIndexer{},
		"SearchResults":     searchResults{},
	}

	all := schemas(t, loadSpec(t))
	for name, value := range cases {
		schema, ok := all[name]
		if !assert.True(t, ok, "the spec has no schema %q", name) {
			continue
		}

		want := jsonFieldNames(t, reflect.TypeOf(value))
		slices.Sort(want)
		got := mapKeys(mustChild(t, schema, "properties"))
		slices.Sort(got)
		assert.Equal(t, want, got, "%s: documented properties do not match the struct's JSON fields", name)
	}

	if schema, ok := all["ErrorResponse"]; assert.True(t, ok, "the spec has no schema %q", "ErrorResponse") {
		assert.Equal(t, []string{"error"}, mapKeys(mustChild(t, schema, "properties")),
			"ErrorResponse must document the single %q field writeJSONError encodes", "error")
	}

	for name := range all {
		_, covered := cases[name]
		assert.True(t, covered || name == "ErrorResponse",
			"schema %q is not checked against a struct; add it to this test", name)
	}
}

// jsonFieldNames returns the JSON names a struct marshals to. A field
// carrying "omitempty" is rejected outright: the required-matches-
// properties rule above assumes every field is always emitted, so the two
// tests would quietly disagree if one were added.
//
// It follows encoding/json rather than reflect's plain field list, which
// is not the same set: an unexported field never reaches the wire, and an
// embedded struct puts its own fields there instead of its type name.
// Counting either the way reflect reports it would have the test demand a
// property the API never sends, or miss one it does.
func jsonFieldNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	names := make([]string, 0, typ.NumField())
	for field := range typ.Fields() {
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")

		// An embedded struct that was given no name of its own promotes
		// its fields into this one. This runs before the export check
		// because an embedded unexported *type* still promotes its
		// exported fields.
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				names = append(names, jsonFieldNames(t, embedded)...)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}

		if name == "" {
			name = field.Name
		}
		assert.NotContains(t, strings.Split(opts, ","), "omitempty",
			"%s.%s carries omitempty; the spec's required lists assume every field is always emitted",
			typ.Name(), field.Name)
		names = append(names, name)
	}
	return names
}

// jsonOnlyParams are the parameters the JSON results endpoint documents
// and the XML feed does not. Each is a name Jackett itself binds only at
// that address: it reads "Query" and "Category[]" there rather than
// Torznab's "q" and "cat", "rageid" rather than "rid", and "Tracker[]"
// nowhere else at all. Documenting them on the feed would promise a
// client something neither Jacklet nor Jackett offers there.
var jsonOnlyParams = map[string]bool{
	"JackettCategory": true,
	"JackettQuery":    true,
	"JackettRageId":   true,
	"Tracker":         true,
}

// TestOpenAPISearchEndpointsShareTheirParameters holds the XML search and
// the JSON results endpoints to the same documented parameters, apart from
// jsonOnlyParams. They run the identical pipeline (searchParamsFromQuery,
// search), differing only in how they render the answer, so a parameter
// documented on one and not the other is a documentation bug either way
// round.
func TestOpenAPISearchEndpointsShareTheirParameters(t *testing.T) {
	root := loadSpec(t)
	paths := mustChild(t, root, "paths")

	params := func(path string) []string {
		t.Helper()
		operation := mustChild(t, mustChild(t, paths, path), "get")
		var out []string
		for _, entry := range mustChild(t, operation, "parameters").Content {
			if ref := child(entry, "$ref"); ref != nil {
				out = append(out, strings.TrimPrefix(ref.Value, "#/components/parameters/"))
				continue
			}
			// An inline parameter, named rather than referenced.
			out = append(out, mustChild(t, entry, "name").Value)
		}
		return out
	}

	xml := params("/api/v2.0/indexers/{id}/results/torznab/api")
	// "t" selects the operation and exists only on the XML endpoint; the
	// JSON one always runs a plain search, as Jackett's does. "configured"
	// belongs to one of those operations rather than to the search (see
	// nonSearchParams).
	xml = slices.DeleteFunc(xml, func(name string) bool {
		return name == "t" || nonSearchParams[name] != ""
	})

	json := slices.DeleteFunc(params("/api/v2.0/indexers/{id}/results"), func(name string) bool {
		return jsonOnlyParams[name]
	})

	assert.Equal(t, xml, json,
		"the two search endpoints document different parameters")
}

// TestOpenAPIOperationParametersAreGrouped pins the reading order of an
// operation's parameter list, which is the one place in the spec that is
// deliberately *not* alphabetical: Scalar renders parameters in document
// order, so this list is what an operator configuring Sonarr or Radarr
// reads top to bottom. Grouping keeps a pair like season/ep together and
// leads with the parameters every client sends.
//
// Adding a parameter means placing it in this list, which is the point:
// appending it to the end is how the order drifted before.
func TestOpenAPIOperationParametersAreGrouped(t *testing.T) {
	order := []string{
		// which indexer, and what to ask of it
		"IndexerId", "row", "t", "configured",
		// the core search, each followed by the name Jackett binds for
		// it at the JSON results endpoint
		"Query", "JackettQuery", "Category", "JackettCategory",
		// television
		"Season", "Episode",
		// identifiers a tracker may be searched by directly
		"ImdbId", "TvdbId", "TmdbId", "TvMazeId", "TraktId", "TvRageId", "JackettRageId", "DoubanId",
		"Extended", "Year", "Genre",
		// music
		"Artist", "Album", "Track", "Label",
		// books
		"Author", "Publisher", "Title",
		// which of the addressed indexers to search
		"Tracker",
		// paging
		"Limit", "Offset",
	}

	root := loadSpec(t)
	paths := mustChild(t, root, "paths")
	for _, path := range mapKeys(paths) {
		operation := mustChild(t, mustChild(t, paths, path), "get")
		list := child(operation, "parameters")
		if list == nil {
			continue
		}

		var got []string
		for _, entry := range list.Content {
			if ref := child(entry, "$ref"); ref != nil {
				got = append(got, strings.TrimPrefix(ref.Value, "#/components/parameters/"))
				continue
			}
			got = append(got, mustChild(t, entry, "name").Value)
		}

		// Every name must have a place before anything is sorted by it:
		// a missing one would make the comparison below no ordering at
		// all, and the failure names the parameter rather than whichever
		// pair the sort happened to reach first.
		hasUnplaced := false
		for _, name := range got {
			if !assert.Contains(t, order, name,
				"%s: parameter %q has no place in the reading order; add it to this test", path, name) {
				hasUnplaced = true
			}
		}
		if hasUnplaced {
			continue
		}

		want := slices.Clone(got)
		slices.SortFunc(want, func(a, b string) int {
			return slices.Index(order, a) - slices.Index(order, b)
		})
		assert.Equal(t, want, got, "%s: parameters are not in the documented reading order", path)
	}
}

// TestOpenAPIReferencesResolve checks that every "$ref" names something
// the document actually defines. Scalar renders an unresolvable reference
// as a hole where the response body or the shared 401 should be, and
// nothing else here notices: the orphan check below walks refs only to
// collect parameter names, and schema refs live outside "paths" entirely.
// Reordering this file — moving every schema and component at once, as
// one change has — is exactly when a name gets mistyped.
func TestOpenAPIReferencesResolve(t *testing.T) {
	root := loadSpec(t)

	var walk func(*yaml.Node)
	walk = func(node *yaml.Node) {
		if ref := child(node, "$ref"); ref != nil {
			switch pointer, local := strings.CutPrefix(ref.Value, "#/"); {
			case !local:
				assert.Fail(t, "a non-local $ref",
					"openapi.yaml:%d: $ref %q is not a local reference; this test resolves only those",
					ref.Line, ref.Value)
			case resolvePointer(root, pointer) == nil:
				assert.Fail(t, "an unresolvable $ref",
					"openapi.yaml:%d: $ref %q names nothing the document defines",
					ref.Line, ref.Value)
			}
		}
		for _, item := range node.Content {
			walk(item)
		}
	}
	walk(root)
}

// resolvePointer follows a JSON pointer, already stripped of its leading
// "#/", through the document, and returns nil when any step of it names
// something that is not there.
func resolvePointer(root *yaml.Node, pointer string) *yaml.Node {
	node := root
	for segment := range strings.SplitSeq(pointer, "/") {
		// JSON Pointer's escapes, which a reference into "paths" needs
		// for the slashes in a path like "/api/v2.0/indexers/{id}/results/torznab/api".
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")
		if node = child(node, segment); node == nil {
			return nil
		}
	}
	return node
}

// TestOpenAPINoOrphanComponents catches a component left behind by
// whatever stopped using it: a parameter no endpoint accepts any more, a
// response nothing answers with, a schema nothing returns. An orphan is
// inert — Scalar simply never renders it — but it reads as part of the
// API to the next person, who has no way to tell it apart from the live
// entries around it.
//
// A parameter or a response is only ever reached from an operation, so
// those must be referenced from "paths". A schema is reached through
// another schema — JackettCapability appears only inside JackettIndexer,
// never in an operation — so it is enough that something anywhere refers
// to it.
// The cost of that laxer rule is that a pair of orphan schemas naming
// each other still looks used; the alternative is a false positive on the
// arrangement the spec actually has.
//
// securitySchemes is exempt: "apiKey" is named by the top-level security
// block rather than pointed at by a "$ref", so it has none by design.
func TestOpenAPINoOrphanComponents(t *testing.T) {
	root := loadSpec(t)

	refsIn := func(node *yaml.Node) map[string]bool {
		found := make(map[string]bool)
		var walk func(*yaml.Node)
		walk = func(node *yaml.Node) {
			if ref := child(node, "$ref"); ref != nil {
				found[ref.Value] = true
			}
			for _, item := range node.Content {
				walk(item)
			}
		}
		walk(node)
		return found
	}

	fromPaths := refsIn(mustChild(t, root, "paths"))
	fromAnywhere := refsIn(root)

	components := mustChild(t, root, "components")
	for _, section := range []struct {
		name string
		used map[string]bool
	}{
		{"parameters", fromPaths},
		{"responses", fromPaths},
		{"schemas", fromAnywhere},
	} {
		for _, name := range mapKeys(mustChild(t, components, section.name)) {
			assert.True(t, section.used["#/components/"+section.name+"/"+name],
				"components/%s/%s is defined but nothing references it", section.name, name)
		}
	}
}

// routeAliases maps each registered pattern the spec documents under
// another path to the operation it is documented as. Jackett's action
// segment is ignored whatever it holds, so the spec documents the feed
// once, under the "api" clients send, and says in prose that the segment
// may be anything or nothing.
var routeAliases = map[string]string{
	"GET /api/v2.0/indexers/{id}/results/torznab":           "GET /api/v2.0/indexers/{id}/results/torznab/api",
	"GET /api/v2.0/indexers/{id}/results/torznab/{ignored}": "GET /api/v2.0/indexers/{id}/results/torznab/api",
}

// TestOpenAPIPathsMatchTheRoutes holds the spec's API paths to the routes
// Routes registers, in both directions: a route nobody documented is an
// endpoint /docs hides, and a documented path nothing serves is a promise
// every client following the spec gets a 404 for.
//
// A documented operation is sent to a mux Routes built, so it counts as
// served exactly when net/http would route it, by the route documented as
// that operation; a GET route also answers HEAD. Every route names its
// method, since a method-less one would answer methods nobody documented
// or wrote a handler for. The other direction needs the patterns
// themselves, which a *http.ServeMux will not list, so they are read out
// of the source. A pattern ending in "{$}" is the trailing-slash form of
// the one without it, which Jackett answers as the same route, so it is
// documented by that one.
func TestOpenAPIPathsMatchTheRoutes(t *testing.T) {
	spec, err := os.ReadFile(specPath)
	require.NoError(t, err, "failed to read the spec")
	documented := routetest.Operations(t, spec, func(path string) bool {
		return strings.HasPrefix(path, routetest.APIPrefix)
	})

	registered := make(map[string]bool)
	decl, fset := findFuncDecl(t, "Torznab", "Routes")
	for _, pattern := range routetest.RoutePatterns(routetest.Routes(t, fset, "Routes", decl.Body, serveMuxParam(t, decl), nil)) {
		registered[pattern] = true
	}

	logger := slog.New(slog.DiscardHandler)
	handler := New(nil, scraper.New(scraper.NewConfigStore(""), "", logger), scraper.NewDefinitionStore(t.TempDir(), logger), "", logger)
	mux := http.NewServeMux()
	handler.Routes(mux)
	for _, operation := range slices.Sorted(maps.Keys(documented)) {
		method, path, _ := strings.Cut(operation, " ")
		request := httptest.NewRequest(method, routetest.SampleTarget(path), http.NoBody)
		_, pattern := mux.Handler(request)
		if !assert.NotEmpty(t, pattern, "the spec documents %q, which no route Routes registers serves", operation) {
			continue
		}
		// A wildcard route matches paths it was never meant to serve, so
		// the route that matched must be the one documented here.
		servedAs := documentedOperation(pattern)
		assert.True(t, routetest.Serves(servedAs, operation),
			"the spec documents %q, which only the route %q matches, and that route is documented as %q", operation, pattern, servedAs)
	}

	patterns := slices.Sorted(maps.Keys(registered))
	routetest.AssertDocumented(t, "Routes", patterns, patterns, documented, documentedOperation)

	// An alias left behind by a route that was removed would otherwise go
	// on excusing a pattern nobody registers.
	for _, pattern := range slices.Sorted(maps.Keys(routeAliases)) {
		assert.True(t, registered[pattern], "%q is listed as an alias, but Routes does not register it", pattern)
	}
}

// TestOpenAPIOperationsMatchTheHandlers holds what the spec documents for
// each API operation to what its handler's code does, in both directions:
// the statuses it can write, the query parameters it reads and the
// content types it sets. A status, parameter or format a client meets but
// /docs omits is one the client is not written for, and one documented
// but never produced describes behavior that cannot happen. They are read
// out of the code each handler reaches, so what a shared helper does, such
// as the search path's failures or searchParamsFromQuery's parameters,
// counts for every endpoint that calls it.
func TestOpenAPIOperationsMatchTheHandlers(t *testing.T) {
	spec, err := os.ReadFile(specPath)
	require.NoError(t, err, "failed to read the spec")
	documented := routetest.Document(t, spec, func(path string) bool {
		return strings.HasPrefix(path, routetest.APIPrefix)
	})

	source := routetest.LoadSource(t, ".")
	decl := source.Func(t, "Torznab", "Routes")
	routes := routetest.Routes(t, source.Fset, "Routes", decl.Body, serveMuxParam(t, decl), nil)

	behaviors := make(map[string]routetest.Behavior)
	for operation := range documented {
		for _, route := range routes {
			if routetest.Serves(documentedOperation(route.Pattern), operation) {
				behaviors[operation] = behaviors[operation].Merge(source.Behavior(t, route.Handler))
			}
		}
	}
	// Each exception is compared as AssertBehavior compares names, and
	// must still hold, so one left behind by a change cannot go on
	// excusing a name.
	isListed := func(names map[string]string) func(string) bool {
		return func(name string) bool {
			return slices.ContainsFunc(slices.Collect(maps.Keys(names)), func(listed string) bool { return lookupName(listed) == lookupName(name) })
		}
	}
	for _, operation := range slices.Sorted(maps.Keys(ignoredParameters)) {
		names := ignoredParameters[operation]
		if !assert.Contains(t, documented, operation, "ignoredParameters names an operation the spec does not document") {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(names)) {
			assert.Contains(t, routetest.Normalized(documented[operation].Parameters, lookupName), lookupName(name),
				"%s's %q is listed as ignored, but the spec no longer documents it", operation, name)
			assert.NotContains(t, routetest.Normalized(behaviors[operation].Parameters, lookupName), lookupName(name),
				"%s's %q is listed as ignored, but its handler reads it now", operation, name)
		}
		spec := documented[operation]
		spec.Parameters = slices.DeleteFunc(slices.Clone(spec.Parameters), isListed(names))
		documented[operation] = spec
	}
	for _, operation := range slices.Sorted(maps.Keys(undocumentedParameters)) {
		names := undocumentedParameters[operation]
		if !assert.Contains(t, behaviors, operation, "undocumentedParameters names an operation no route serves") {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(names)) {
			assert.Contains(t, routetest.Normalized(behaviors[operation].Parameters, lookupName), lookupName(name),
				"%s's %q is listed as undocumented, but its handler no longer reads it", operation, name)
			assert.NotContains(t, routetest.Normalized(documented[operation].Parameters, lookupName), lookupName(name),
				"%s's %q is listed as undocumented, but the spec documents it now", operation, name)
		}
		behavior := behaviors[operation]
		behavior.Parameters = slices.DeleteFunc(slices.Clone(behavior.Parameters), isListed(names))
		behaviors[operation] = behavior
	}
	routetest.AssertBehavior(t, documented, behaviors, lookupName)
}

// ignoredParameters are the query parameters the spec documents for an
// operation that its handler deliberately does not read, by operation,
// each with the reason it is documented anyway.
var ignoredParameters = map[string]map[string]string{
	"GET /api/v2.0/indexers": {
		"configured": "accepted for compatibility with Jackett's filter, and every listed indexer is already configured",
	},
}

// undocumentedParameters are the query parameters an operation's handler
// reads that the spec deliberately does not document for it, by
// operation, each with the reason.
var undocumentedParameters = map[string]map[string]string{
	"GET /api/v2.0/indexers/{id}/results": {
		"t": "read by the shared search parser and then set to a plain search, which Jackett's results endpoint always runs",
	},
	"GET /api/v2.0/indexers/{id}/results/torznab/api": {
		"category": `Jackett's name for "cat", documented on the JSON endpoint that binds it`,
		"query":    `Jackett's name for "q", documented on the JSON endpoint that binds it`,
		"rageid":   `Jackett's name for "rid", documented on the JSON endpoint that binds it`,
	},
}

// documentedOperation returns the operation the spec documents a
// registered pattern as: the one routetest.Operation reads, or the
// operation routeAliases names for it.
func documentedOperation(pattern string) string {
	_, operation := routetest.Operation(pattern)
	if alias, ok := routeAliases[operation]; ok {
		return alias
	}
	return operation
}

// serveMuxParam returns the name of a function's *http.ServeMux
// parameter, the mux its routes are registered on.
func serveMuxParam(t *testing.T, decl *ast.FuncDecl) string {
	t.Helper()
	for _, param := range decl.Type.Params.List {
		star, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "ServeMux" && routetest.IsIdent(sel.X, "http") && len(param.Names) == 1 {
			return param.Names[0].Name
		}
	}
	require.FailNow(t, "no mux parameter", "%s takes no named *http.ServeMux parameter; this test needs updating", decl.Name.Name)
	return ""
}

// nonSearchParams are query parameters the search endpoints document that
// no search reads into scraper.SearchParams, because they belong to
// another operation reached through "t" or choose which indexers are
// searched. They are exempt from TestOpenAPIDocumentedParametersAreAccepted
// below, which holds every other documented parameter to a field, while
// TestOpenAPIOperationsMatchTheHandlers still holds each of them to the
// code that reads it.
//
// The exemption is deliberately a named list rather than a skip: a
// parameter lands here only with a reason, and the handler that does read
// it owes a test of its own for the behavior (handleIndexerList's
// "configured" filtering is covered in indexerlist_test.go).
var nonSearchParams = map[string]string{
	"configured": `filters "t=indexers", which is not a search`,
	"Tracker[]":  "narrows which indexers are searched, not what is searched for",
}

// searchParamFields names the scraper.SearchParams field each documented
// query parameter must land in. Naming the field is what gives the test
// below its teeth: checking only that the value arrived *somewhere* would
// pass a pair of parameters wired to each other's field, so a search by
// TVRage id would go out to the tracker as a Trakt id with every test
// green. The cost is one line per parameter, which is the same line the
// spec and searchParamsFromQuery each already carry.
var searchParamFields = map[string]string{
	"album":    "Album",
	"artist":   "Artist",
	"author":   "Author",
	"cat":      "Categories",
	"doubanid": "DoubanID",
	"ep":       "Ep",
	"extended": "Extended",
	"genre":    "Genre",
	"imdbid":   "IMDBID",
	"label":    "Label",
	"limit":    "Limit",
	"offset":   "Offset",
	"q":        "Query",
	// The names Jackett binds at the JSON results endpoint, which land in
	// the same fields as Torznab's own.
	"Query":      "Query",
	"Category[]": "Categories",
	"rageid":     "TVRageID",
	"rid":        "TVRageID",
	"season":     "Season",
	"t":          "Type",
	"title":      "Title",
	"tmdbid":     "TMDBID",
	"track":      "Track",
	"traktid":    "TraktID",
	"tvdbid":     "TVDBID",
	"tvmazeid":   "TVMazeID",
	"publisher":  "Publisher",
	"year":       "Year",
}

// TestOpenAPIDocumentedParametersAreAccepted closes the loop none of the
// tests above can: they compare the spec against itself, so deleting a
// lookup from searchParamsFromQuery leaves every one of them green while
// /docs goes on promising the parameter. Here each documented query
// parameter is actually sent through the parser, and has to arrive in the
// field it belongs in — which catches a name the handler stopped reading,
// a name the spec spelled wrong, and a pair of parameters swapped between
// their fields.
func TestOpenAPIDocumentedParametersAreAccepted(t *testing.T) {
	root := loadSpec(t)

	// A value no parser would produce on its own, so finding it in the
	// parsed params means it was carried there rather than defaulted.
	const sentinel = "openapi-test-sentinel"

	// Both search endpoints: the JSON one documents the names Jackett
	// binds only there, and a documented alias the handler stopped
	// reading would otherwise go unnoticed.
	paths := []string{
		"/api/v2.0/indexers/{id}/results/torznab/api",
		"/api/v2.0/indexers/{id}/results",
	}
	paramNodes := make([]*yaml.Node, 0, len(paths))
	for _, path := range paths {
		paramNodes = append(paramNodes, operationParameters(t, root, path)...)
	}

	seen := make(map[string]bool)
	for _, param := range paramNodes {
		if mustChild(t, param, "in").Value != "query" {
			continue
		}
		name := mustChild(t, param, "name").Value
		if nonSearchParams[name] != "" || seen[name] {
			continue
		}
		seen[name] = true

		t.Run(name, func(t *testing.T) {
			fieldName, ok := searchParamFields[name]
			require.True(t, ok, "no field is named for %q; add it to searchParamFields", name)

			declared, ok := reflect.TypeFor[scraper.SearchParams]().FieldByName(fieldName)
			require.True(t, ok, "scraper.SearchParams has no field %q; update searchParamFields", fieldName)

			// Paging is parsed into ints, so it takes a number rather
			// than the sentinel.
			raw := sentinel
			if declared.Type.Kind() == reflect.Int {
				raw = "7"
			}

			params, _, _ := searchParamsFromQuery(url.Values{name: {raw}}, torznabLimits)
			field := reflect.ValueOf(params).FieldByName(fieldName)

			var got string
			switch field.Kind() {
			case reflect.Slice:
				if field.Len() == 1 {
					got = field.Index(0).String()
				} else {
					got = fmt.Sprint(field.Interface())
				}
			case reflect.Int:
				got = strconv.Itoa(int(field.Int()))
			default:
				got = field.String()
			}
			require.Equal(t, raw, got,
				"%s=%s did not reach SearchParams.%s; searchParamsFromQuery does not read it into that field",
				name, raw, fieldName)
		})
	}

	// The loop above only consults the entries the spec still names, so
	// one left behind by a parameter that was removed would never be
	// mentioned again — and would go on claiming a field to the next
	// person to read the map.
	documented := documentedQueryParams(t, root)
	for name := range searchParamFields {
		assert.True(t, documented[name],
			"searchParamFields has an entry for %q, which the spec no longer documents as a query parameter", name)
	}
}

// TestOpenAPIModesMatchTheHandler holds the "t" values the spec allows on
// the Torznab endpoint to the ones ServeHTTP answers, read out of its
// switch on the parameter, in both directions: a value the handler accepts
// and the spec leaves out is one a client reading the spec never sends,
// and one the spec lists and the handler refuses fails every client that
// trusts it.
func TestOpenAPIModesMatchTheHandler(t *testing.T) {
	var documented []string
	for _, param := range operationParameters(t, loadSpec(t), "/api/v2.0/indexers/{id}/results/torznab/api") {
		if child(param, "name").Value != "t" {
			continue
		}
		for _, value := range mustChild(t, mustChild(t, param, "schema"), "enum").Content {
			documented = append(documented, value.Value)
		}
	}
	require.NotEmpty(t, documented, "the spec lists no values for \"t\"")

	source := routetest.LoadSource(t, ".")
	cases := queryValueCases(t, source, source.Func(t, "Torznab", "ServeHTTP"), "t")
	accepted := slices.Sorted(maps.Keys(cases))

	slices.Sort(documented)
	require.Equal(t, accepted, documented, "the spec's \"t\" enum and the values ServeHTTP answers differ")

	// The search modes are named again in canonicalSearchType, which gives
	// a definition its mode, so each value the switch searches with must
	// arrive as one it knows.
	modes := []string{"book", "movie", "music", "search", "tvsearch"}
	var searched []string
	for _, value := range accepted {
		if callsMethod(cases[value], "handleSearch") {
			searched = append(searched, value)
		}
	}
	require.NotEmpty(t, searched, "no case of ServeHTTP's switch on \"t\" calls handleSearch")
	for _, value := range searched {
		assert.Contains(t, modes, canonicalSearchType(value),
			"ServeHTTP searches with %q, which canonicalSearchType hands a definition as no mode it knows", value)
	}
}

// callsMethod reports whether clause's body calls a method named name.
func callsMethod(clause *ast.CaseClause, name string) bool {
	isCalled := false
	for _, stmt := range clause.Body {
		ast.Inspect(stmt, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
					isCalled = true
				}
			}
			return !isCalled
		})
	}
	return isCalled
}

// queryValueCases returns the string cases of the switch in decl on the
// request's query parameter name, each with the clause it selects, failing
// when there is no such switch or a case is not a literal, since the
// values it accepts could then not be read.
func queryValueCases(t *testing.T, source *routetest.Source, decl *ast.FuncDecl, name string) map[string]*ast.CaseClause {
	t.Helper()
	cases := make(map[string]*ast.CaseClause)
	isFound := false
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		sw, ok := node.(*ast.SwitchStmt)
		if !ok || !isQueryGet(source, sw.Tag, name) {
			return true
		}
		isFound = true
		for _, stmt := range sw.Body.List {
			clause := stmt.(*ast.CaseClause)
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "a case on %q is not a string literal", name)
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				cases[value] = clause
			}
		}
		return false
	})
	require.True(t, isFound, "%s does not switch on the query parameter %q", decl.Name.Name, name)
	return cases
}

// isQueryGet reports whether expr is a Get(name) call on a value derived
// from a request's query, as in r.URL.Query().Get("t"), read as the
// per-operation parameter check reads one.
func isQueryGet(source *routetest.Source, expr ast.Expr, name string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	get, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || get.Sel.Name != "Get" || !source.IsQuery(get.X) {
		return false
	}
	arg, ok := call.Args[0].(*ast.BasicLit)
	return ok && arg.Kind == token.STRING && arg.Value == strconv.Quote(name)
}

// documentedQueryParams is the set of query parameters both search
// endpoints document, under the names the spec writes. The JSON endpoint
// documents the names Jackett binds only there on top of the shared list,
// which is what jsonOnlyParams holds.
func documentedQueryParams(t *testing.T, root *yaml.Node) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, path := range []string{
		"/api/v2.0/indexers/{id}/results/torznab/api",
		"/api/v2.0/indexers/{id}/results",
	} {
		for _, param := range operationParameters(t, root, path) {
			if mustChild(t, param, "in").Value == "query" {
				out[mustChild(t, param, "name").Value] = true
			}
		}
	}
	return out
}

// lookupName is the key the handler looks a documented parameter up by.
// A parameter is documented under the spelling a client sends, while the
// lookup ignores case and the array brackets, so "Category[]" and "cat"
// are documented names of keys "category" and "cat".
func lookupName(documented string) string {
	return strings.ToLower(strings.TrimSuffix(documented, "[]"))
}

// findFuncDecl returns the declaration of the named function in this
// package, a method of the named receiver type or a package-level function
// when receiver is "", with the file set that places it.
func findFuncDecl(t *testing.T, receiver, funcName string) (*ast.FuncDecl, *token.FileSet) {
	t.Helper()
	source := routetest.LoadSource(t, ".")
	return source.Func(t, receiver, funcName), source.Fset
}

// operationParameters returns an operation's parameter definitions, with
// each "$ref" resolved to the component it names so the caller sees the
// same fields whether the parameter was shared or written inline.
func operationParameters(t *testing.T, root *yaml.Node, path string) []*yaml.Node {
	t.Helper()
	operation := mustChild(t, mustChild(t, mustChild(t, root, "paths"), path), "get")
	list := child(operation, "parameters")
	if list == nil {
		return nil
	}

	defined := mustChild(t, mustChild(t, root, "components"), "parameters")
	out := make([]*yaml.Node, 0, len(list.Content))
	for _, entry := range list.Content {
		ref := child(entry, "$ref")
		if ref == nil {
			out = append(out, entry)
			continue
		}
		resolved := child(defined, strings.TrimPrefix(ref.Value, "#/components/parameters/"))
		require.NotNil(t, resolved, "%s: parameter %q is referenced but not defined", path, ref.Value)
		out = append(out, resolved)
	}
	return out
}

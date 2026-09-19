// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	"github.com/torrplay/jacklet/pkg/scraper"
)

// These tests walk the whole spec and report every point of drift in one
// run, so the loops below assert rather than require: stopping at the
// first mismatch would hide the rest of the work a contributor has to do.

// These tests hold cmd/jacklet/public/openapi.yaml to what the handlers in
// this package actually serve, and to the repository's ordering
// conventions for lookup tables and parameters. The spec is served to every Torznab client's operator
// through /docs, so it drifting from the code is a documentation bug that
// nothing else would catch.
//
// They live here, reaching across to cmd/jacklet, because this is the only
// package that can see both the spec and the response structs — those are
// unexported, and exporting an internal wire shape to make a test's life
// easier would be the wrong trade.

// specPath is the spec, relative to this package's directory.
var specPath = filepath.Join("..", "..", "cmd", "jacklet", "public", "openapi.yaml")

// loadSpec parses the spec into a yaml.Node tree, which — unlike
// unmarshalling into a map — preserves the order keys are written in.
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

// TestOpenAPI_LookupSectionsAreAlphabetical covers the ordering rule for
// the parts of the spec that are lookup tables: nobody reads
// components/parameters top to bottom, so alphabetical is what makes an
// entry findable — and what tells a contributor adding a parameter where
// it goes, instead of at the end.
func TestOpenAPI_LookupSectionsAreAlphabetical(t *testing.T) {
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

// TestOpenAPI_RequiredMatchesProperties pins the invariant that makes the
// spec self-checking: every field of every response struct is emitted
// unconditionally — none carries "omitempty" — so a property that is not
// required would be describing a response Jacklet never sends.
func TestOpenAPI_RequiredMatchesProperties(t *testing.T) {
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

// TestOpenAPI_SchemasMatchTheResponseStructs is the check that actually
// stops drift: adding a field to one of these structs without documenting
// it, or renaming its JSON tag, fails here rather than silently leaving
// /docs describing a response that no longer exists.
func TestOpenAPI_SchemasMatchTheResponseStructs(t *testing.T) {
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

// TestOpenAPI_SearchEndpointsShareTheirParameters holds the XML search and
// the JSON results endpoints to the same documented parameters, apart from
// jsonOnlyParams. They run the identical pipeline (searchParamsFromQuery,
// search), differing only in how they render the answer, so a parameter
// documented on one and not the other is a documentation bug either way
// round.
func TestOpenAPI_SearchEndpointsShareTheirParameters(t *testing.T) {
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
	// JSON one always searches. "configured" belongs to one of those
	// operations rather than to the search (see nonSearchParams).
	xml = slices.DeleteFunc(xml, func(name string) bool {
		return name == "t" || nonSearchParams[name] != ""
	})

	json := slices.DeleteFunc(params("/api/v2.0/indexers/{id}/results"), func(name string) bool {
		return jsonOnlyParams[name]
	})

	assert.Equal(t, xml, json,
		"the two search endpoints document different parameters")
}

// TestOpenAPI_OperationParametersAreGrouped pins the reading order of an
// operation's parameter list, which is the one place in the spec that is
// deliberately *not* alphabetical: Scalar renders parameters in document
// order, so this list is what an operator configuring Sonarr or Radarr
// reads top to bottom. Grouping keeps a pair like season/ep together and
// leads with the parameters every client sends.
//
// Adding a parameter means placing it in this list, which is the point:
// appending it to the end is how the order drifted before.
func TestOpenAPI_OperationParametersAreGrouped(t *testing.T) {
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
		unplaced := false
		for _, name := range got {
			if !assert.Contains(t, order, name,
				"%s: parameter %q has no place in the reading order; add it to this test", path, name) {
				unplaced = true
			}
		}
		if unplaced {
			continue
		}

		want := slices.Clone(got)
		slices.SortFunc(want, func(a, b string) int {
			return slices.Index(order, a) - slices.Index(order, b)
		})
		assert.Equal(t, want, got, "%s: parameters are not in the documented reading order", path)
	}
}

// TestOpenAPI_ReferencesResolve checks that every "$ref" names something
// the document actually defines. Scalar renders an unresolvable reference
// as a hole where the response body or the shared 401 should be, and
// nothing else here notices: the orphan check below walks refs only to
// collect parameter names, and schema refs live outside "paths" entirely.
// Reordering this file — moving every schema and component at once, as
// one change has — is exactly when a name gets mistyped.
func TestOpenAPI_ReferencesResolve(t *testing.T) {
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

// TestOpenAPI_NoOrphanComponents catches a component left behind by
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
func TestOpenAPI_NoOrphanComponents(t *testing.T) {
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

// nonSearchParams are query parameters the XML endpoint documents that no
// search ever reads, because they belong to another operation reached
// through "t". They are exempt from the two tests below, which hold every
// documented parameter to a scraper.SearchParams field.
//
// The exemption is deliberately a named list rather than a skip: a
// parameter lands here only with a reason, and the handler that does read
// it owes a test of its own for the behaviour (handleIndexerList's
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

// TestOpenAPI_DocumentedParametersAreAccepted closes the loop none of the
// tests above can: they compare the spec against itself, so deleting a
// q.Get from searchParamsFromQuery leaves every one of them green while
// /docs goes on promising the parameter. Here each documented query
// parameter is actually sent through the parser, and has to arrive in the
// field it belongs in — which catches a name the handler stopped reading,
// a name the spec spelled wrong, and a pair of parameters swapped between
// their fields.
func TestOpenAPI_DocumentedParametersAreAccepted(t *testing.T) {
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

// TestOpenAPI_AcceptedParametersAreDocumented is the other direction, and
// the one the test above cannot cover: it walks the spec, so a parameter
// the handler starts reading but nobody documents leaves every test green
// while /docs quietly omits it. The names the handler reads are string
// literals rather than anything reflection can reach, so they are read out
// of the source.
func TestOpenAPI_AcceptedParametersAreDocumented(t *testing.T) {
	documented := make(map[string]bool)
	for name := range documentedQueryParams(t, loadSpec(t)) {
		documented[lookupName(name)] = true
	}

	for _, name := range queryKeysRead(t, "searchParamsFromQuery") {
		assert.True(t, documented[name],
			"searchParamsFromQuery reads the query parameter %q, which the spec does not document", name)
	}
}

// queryKeysRead returns the query-parameter names a function reads, by
// parsing this package and collecting the string literals it looks them
// up by: `q.Get("name")` and `q["name"]` on the function's own url.Values
// parameter, and `q.get`/`q.list`/`q.number` on a searchQuery built from
// that parameter. Binding to one of those two identifiers is what keeps a
// lookup on some other string-keyed thing — an http.Header, a package
// table — from being reported as a query parameter nobody documented.
//
// The searchQuery form is not optional coverage: a lookup that moved from
// url.Values onto the helper and was not followed here would leave this
// test reading no names at all, which is a pass rather than a failure.
func queryKeysRead(t *testing.T, funcName string) []string {
	t.Helper()

	decl := findFuncDecl(t, funcName)
	values := queryValuesParam(t, decl, funcName)
	indexed := searchQueryLocal(decl, values)

	var keys []string
	add := func(args []ast.Expr) {
		for _, arg := range args {
			if key, ok := stringLiteral(arg); ok {
				keys = append(keys, key)
			}
		}
	}

	ast.Inspect(decl, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Get" && len(n.Args) == 1 && isIdent(sel.X, values) {
				add(n.Args)
				return true
			}
			// number takes a fallback after the name; get and list take
			// names alone, so only literals are collected either way.
			if indexed != "" && isIdent(sel.X, indexed) {
				switch sel.Sel.Name {
				case "get", "list":
					add(n.Args)
				case "number":
					add(n.Args[:1])
				}
			}
		case *ast.IndexExpr:
			if !isIdent(n.X, values) {
				return true
			}
			if key, ok := stringLiteral(n.Index); ok {
				keys = append(keys, key)
			}
		}
		return true
	})

	require.NotEmpty(t, keys, "%s reads no query parameters; this test has stopped covering it", funcName)
	slices.Sort(keys)
	return slices.Compact(keys)
}

// searchQueryLocal returns the name of the searchQuery the function built
// from its url.Values parameter, or "" when it built none.
func searchQueryLocal(decl *ast.FuncDecl, values string) string {
	var name string
	ast.Inspect(decl, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 || !isIdent(call.Args[0], values) {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "newSearchQuery" {
			return true
		}
		if lhs, ok := assign.Lhs[0].(*ast.Ident); ok {
			name = lhs.Name
		}
		return true
	})
	return name
}

// findFuncDecl parses this package's non-test files and returns the
// declaration of the named package-level function.
func findFuncDecl(t *testing.T, funcName string) *ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err, "failed to read the package directory")

	var decl *ast.FuncDecl
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "failed to parse %s", name)
		for _, node := range file.Decls {
			if fn, ok := node.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == funcName {
				decl = fn
			}
		}
	}
	require.NotNil(t, decl, "no function %q in this package; this test needs updating", funcName)
	return decl
}

// stringLiteral returns the value of an untyped string literal, and
// whether the expression was one. A key built any other way is not a
// literal this test can read.
func stringLiteral(node ast.Expr) (string, bool) {
	lit, ok := node.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

// isIdent reports whether an expression is the named identifier.
func isIdent(node ast.Expr, name string) bool {
	ident, ok := node.(*ast.Ident)
	return ok && ident.Name == name
}

// queryValuesParam returns the name of the function's url.Values
// parameter, which is the only thing queryKeysRead treats as the query.
func queryValuesParam(t *testing.T, decl *ast.FuncDecl, funcName string) string {
	t.Helper()
	for _, param := range decl.Type.Params.List {
		sel, ok := param.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Values" || !isIdent(sel.X, "url") {
			continue
		}
		require.Len(t, param.Names, 1,
			"%s takes several url.Values parameters in one field; this test expects one named parameter", funcName)
		return param.Names[0].Name
	}
	require.FailNowf(t, "no query parameter",
		"%s takes no url.Values parameter; this test needs updating", funcName)
	return ""
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

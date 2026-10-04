// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package routetest

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testPackage is the source Behavior is read against: a routes function
// registering a handler of each shape it accepts and refuses, helpers they
// reach by call, by value and by named constant, handlers reading query
// parameters and setting content types, and code no handler reaches.
const testPackage = `package sample

import (
	"encoding/json"
	"fmt"
	web "net/http"
	"net/url"
)

const upstreamFailure = web.StatusGatewayTimeout

var stored = func(w web.ResponseWriter, r *web.Request) { w.WriteHeader(web.StatusPartialContent) }

func routes(mux *web.ServeMux, handlers []web.Handler) {
	s := &server{}
	local := func(w web.ResponseWriter, r *web.Request) { w.WriteHeader(web.StatusResetContent) }
	built := health()
	method := s.Results
	files := web.FileServer(nil)
	var later web.HandlerFunc
	later = plain
	split, _ := pair()
	mux.HandleFunc("GET /search", s.Search)
	mux.HandleFunc("GET /typed", s.Typed)
	mux.HandleFunc("GET /dynamic", s.Dynamic)
	mux.Handle("GET /value", s)
	mux.HandleFunc("GET /later", later)
	mux.Handle("GET /split", split)
	mux.HandleFunc("GET /method", method)
	mux.Handle("GET /localfiles", files)
	mux.HandleFunc("GET /local", local)
	mux.Handle("GET /built", built)
	mux.HandleFunc("GET /printed", s.Printed)
	mux.HandleFunc("GET /encoded", s.Encoded)
	mux.HandleFunc("GET /results", s.Results)
	mux.HandleFunc("GET /passing", s.Passing)
	mux.HandleFunc("GET /upstream", s.Upstream)
	mux.HandleFunc("GET /shadowed", s.Shadowed)
	mux.HandleFunc("GET /plain", plain)
	mux.HandleFunc("GET /stored", stored)
	mux.Handle("GET /health", health())
	mux.HandleFunc("GET /literal", func(w web.ResponseWriter, r *web.Request) { respond(w) })
	mux.HandleFunc("GET /notfound", web.NotFound)
	mux.Handle("GET /files", web.FileServer(nil))
	mux.Handle("GET /indexed", handlers[0])
}

type server struct{}

func (s *server) ServeHTTP(w web.ResponseWriter, r *web.Request) {
	s.fail(w, web.StatusBadGateway)
}

func (s *server) fail(w web.ResponseWriter, status int) {
	web.Error(w, fmt.Sprint(status), status)
}

func (s *server) Results(w web.ResponseWriter, r *web.Request) {
	if r.URL.Path == "" {
		writeError(w, web.StatusNotFound)
		return
	}
	respond(w)
}

func (s *server) Passing(w web.ResponseWriter, r *web.Request) {
	use(w, s.conflict)
}

func (s *server) Upstream(w web.ResponseWriter, r *web.Request) {
	writeError(w, upstreamFailure)
}

func (s *server) Shadowed(w web.ResponseWriter, r *web.Request) {
	url := &catalog{}
	url.missing(w)
	fmt := &catalog{}
	fmt.gone(w)
}

func (s *server) Printed(w web.ResponseWriter, r *web.Request) {
	fmt.Fprintln(w, "ok")
}

func (s *server) Encoded(w web.ResponseWriter, r *web.Request) {
	_ = json.NewEncoder(w).Encode(map[string]string{})
}

func (s *server) Search(w web.ResponseWriter, r *web.Request) {
	_ = r.URL.Query().Get("q")
	_ = r.Header.Get("X-Not-A-Parameter")
	find(r.URL.Query())
	_ = newQuery(r.URL.Query()).list("tracker")
	logger{}.With("query", r.URL.Query()).Info("searching")
}

type logger struct{}

func (l logger) With(args ...any) logger { return l }

func (l logger) Info(message string) {}

func find(values url.Values) { _ = values["cat"] }

type query map[string][]string

func newQuery(values url.Values) query { return query(values) }

func (q query) list(name string) []string { return q[name] }

func (s *server) Typed(w web.ResponseWriter, r *web.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("content-type", "text/plain")
	header := w.Header()
	header["Content-Type"] = []string{"text/csv"}
}

func (s *server) Dynamic(w web.ResponseWriter, r *web.Request) {
	w.Header().Set("Content-Type", r.Header.Get("Accept"))
}

func (s *server) conflict(w web.ResponseWriter) {
	w.WriteHeader(web.StatusConflict)
}

func plain(w web.ResponseWriter, r *web.Request) { w.WriteHeader(web.StatusCreated) }

func use(w web.ResponseWriter, write func(web.ResponseWriter)) { write(w) }

func writeError(w web.ResponseWriter, status int) { web.Error(w, "failed", status) }

func respond(w web.ResponseWriter) { w.WriteHeader(web.StatusAccepted) }

func health() web.HandlerFunc {
	return func(w web.ResponseWriter, r *web.Request) {
		w.WriteHeader(web.StatusServiceUnavailable)
	}
}

func unreached(w web.ResponseWriter) { w.WriteHeader(web.StatusTeapot) }

func pair() (web.Handler, error) { return health(), nil }

// s shares its name with the server routes holds in a local, which is
// what a handler named s is read as.
func s(w web.ResponseWriter) { w.WriteHeader(web.StatusTeapot) }
`

// testPackageOther is a second file of testPackage. It imports net/url,
// which makes url a package's name here and nowhere else, so a variable
// named url in testPackage is still followed.
const testPackageOther = `package sample

import (
	"net/url"
	web "net/http"
)

type catalog struct{ base *url.URL }

func (c *catalog) missing(w web.ResponseWriter) { w.WriteHeader(web.StatusNotFound) }

func (c *catalog) gone(w web.ResponseWriter) { w.WriteHeader(web.StatusGone) }
`

// testPackageTest is a test file beside testPackage, which LoadSource
// skips: were it read, every handler would reach its status.
const testPackageTest = `package sample

import "net/http"

func respond(w http.ResponseWriter) { w.WriteHeader(http.StatusLengthRequired) }
`

// loadTestPackage writes testPackage, testPackageOther and testPackageTest
// to a directory and loads it, with the handlers its routes function
// registers by pattern.
func loadTestPackage(t *testing.T) (*Source, map[string]ast.Expr) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sample.go"), []byte(testPackage), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.go"), []byte(testPackageOther), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte(testPackageTest), 0o600))
	source := LoadSource(t, dir)

	decl := source.Func(t, "", "routes")
	handlers := make(map[string]ast.Expr)
	for _, route := range Routes(t, source.Fset, "routes", decl.Body, "mux", nil) {
		handlers[route.Pattern] = route.Handler
	}
	return source, handlers
}

func TestSource_Behavior(t *testing.T) {
	source, handlers := loadTestPackage(t)
	for _, tc := range []struct {
		name, pattern string
		want          []int
	}{
		{name: "a local value serves through its ServeHTTP, not a function it shadows", pattern: "GET /value", want: []int{502}},
		{name: "a local function is read where it is assigned", pattern: "GET /local", want: []int{205}},
		{name: "a local a call returns is read as the call, not ServeHTTP", pattern: "GET /built", want: []int{503}},
		{name: "a local method value is read as the method, not ServeHTTP", pattern: "GET /method", want: []int{202, 404}},
		{name: "printing a body answers 200", pattern: "GET /printed", want: []int{200}},
		{name: "encoding a body answers 200", pattern: "GET /encoded", want: []int{200}},
		{name: "a method value follows the helpers it calls", pattern: "GET /results", want: []int{202, 404}},
		{name: "a method passed as a value is followed", pattern: "GET /passing", want: []int{409}},
		{name: "a status held in a package constant is found", pattern: "GET /upstream", want: []int{504}},
		{name: "a variable named like an import is followed", pattern: "GET /shadowed", want: []int{404, 410}},
		{name: "a plain function is read, not ServeHTTP", pattern: "GET /plain", want: []int{201}},
		{name: "a package variable holding a handler is read", pattern: "GET /stored", want: []int{206}},
		{name: "a call is followed into the handler it returns", pattern: "GET /health", want: []int{503}},
		{name: "a function literal is read in place", pattern: "GET /literal", want: []int{202}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, source.Behavior(t, handlers[tc.pattern]).Statuses,
				"only the code the handler reaches counts, and never a test file's")
		})
	}

	for _, tc := range []struct {
		name, pattern string
		want          Behavior
	}{
		{
			// A logger's With is handed the query, but what it returns is
			// the logger's, so "searching" is no parameter.
			name:    "reads a parameter through every form derived from the query",
			pattern: "GET /search",
			want:    Behavior{Parameters: []string{"cat", "q", "tracker"}},
		},
		{
			name:    "reads a literal content type set, added or assigned, without its parameters",
			pattern: "GET /typed",
			want:    Behavior{ContentTypes: []string{"application/json", "text/csv", "text/plain"}},
		},
		{
			name:    "marks a content type computed at run time",
			pattern: "GET /dynamic",
			want:    Behavior{SetsDynamicContentType: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, source.Behavior(t, handlers[tc.pattern]),
				"a header read is not a parameter, and only a Content-Type header is a content type")
		})
	}

	for _, tc := range []struct {
		name, pattern, want string
	}{
		{name: "refuses another package's handler", pattern: "GET /notfound", want: "is another package's"},
		{name: "refuses a handler another package builds", pattern: "GET /files", want: "is built by another package"},
		{name: "refuses another package's handler held in a local", pattern: "GET /localfiles", want: "is built by another package"},
		{name: "refuses a local declared without a value", pattern: "GET /later", want: "the handler later is declared without a single value of its own"},
		{name: "refuses a local declared from several values", pattern: "GET /split", want: "the handler split is declared without a single value of its own"},
		{name: "refuses a shape it cannot read", pattern: "GET /indexed", want: "is not a shape this test reads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures := recordFailures(t, func(tb testing.TB) {
				tb.Helper()
				source.Behavior(tb, handlers[tc.pattern])
			})
			require.Len(t, failures, 1)
			require.Contains(t, failures[0], tc.want)
		})
	}
}

func TestLoadSource(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sample.go"), []byte(testPackage), 0o600))
	require.Same(t, LoadSource(t, dir), LoadSource(t, filepath.Join(dir, ".")),
		"a directory is parsed once, however its path is spelled")
}

func TestSource_IsQuery(t *testing.T) {
	source, _ := loadTestPackage(t)
	// The operand each "_ = x.Get(...)" or "_ = x[...]" statement reads.
	operand := func(stmt ast.Stmt) ast.Expr {
		value := stmt.(*ast.AssignStmt).Rhs[0]
		if call, ok := value.(*ast.CallExpr); ok {
			return call.Fun.(*ast.SelectorExpr).X
		}
		return value.(*ast.IndexExpr).X
	}
	search := source.Func(t, "server", "Search").Body.List
	require.True(t, source.IsQuery(operand(search[0])), "r.URL.Query() is the query")
	require.False(t, source.IsQuery(operand(search[1])), "r.Header is not the query")
	find := source.Func(t, "", "find").Body.List
	require.True(t, source.IsQuery(operand(find[0])), "a url.Values parameter is the query")
}

func TestSource_Func(t *testing.T) {
	source, _ := loadTestPackage(t)
	require.Equal(t, "Results", source.Func(t, "server", "Results").Name.Name)
	require.Equal(t, "health", source.Func(t, "", "health").Name.Name)

	failures := recordFailures(t, func(tb testing.TB) {
		tb.Helper()
		source.Func(tb, "", "Results")
	})
	require.Len(t, failures, 1, "Results is a method, not a package-level function")
	require.Contains(t, failures[0], `want one declaration of "Results" on ""`)
}

func TestDocument(t *testing.T) {
	spec := []byte(`
security:
  - apiKey: []
  - {}
components:
  securitySchemes:
    apiKey: {type: apiKey, in: query, name: apikey}
  parameters:
    Query: {name: q, in: query}
    Id: {name: id, in: path}
  responses:
    Failed:
      description: x
      content:
        text/plain: {}
paths:
  /search/{id}:
    get:
      parameters:
        - $ref: "#/components/parameters/Id"
        - $ref: "#/components/parameters/Query"
        - {name: cat, in: query}
      responses:
        "200": {description: x, content: {application/json: {}}}
        "500": {$ref: "#/components/responses/Failed"}
  /healthz:
    get:
      security: []
      responses:
        "503": {description: x}
        "200": {description: x}
  /skipped:
    get:
      responses: {"200": {description: x}}
`)
	require.Equal(t,
		map[string]Documented{
			"GET /search/{id}": {
				ContentTypes: []string{"application/json", "text/plain"},
				IsSecured:    true,
				Parameters:   []string{"apikey", "cat", "q"},
				Statuses:     []int{200, 500},
			},
			"GET /healthz": {Statuses: []int{200, 503}},
		},
		Document(t, spec, func(path string) bool { return path != "/skipped" }),
		"references resolve, a path parameter is not a query parameter, the API key counts where it secures, and an alternative naming no scheme leaves the key required")

	for _, tc := range []struct {
		name, spec, want string
	}{
		{
			name: "fails on a response keyed by anything but a status",
			spec: `
paths:
  /healthz:
    get:
      responses:
        default: {description: x}
`,
			want: `GET /healthz documents the response "default", which is not a status`,
		},
		{
			name: "fails on security naming an undefined scheme",
			spec: `
security:
  - apikey: []
paths:
  /healthz:
    get:
      responses:
        "200": {description: x}
`,
			want: `GET /healthz's security names the scheme "apikey", which the spec does not define`,
		},
		{
			name: "fails on a reference to an undefined parameter",
			spec: `
paths:
  /healthz:
    get:
      parameters:
        - $ref: "#/components/parameters/Missing"
      responses:
        "200": {description: x}
`,
			want: `GET /healthz references the parameter "#/components/parameters/Missing", which the spec does not define`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures := recordFailures(t, func(tb testing.TB) {
				tb.Helper()
				Document(tb, []byte(tc.spec), func(string) bool { return true })
			})
			require.Len(t, failures, 1)
			require.Contains(t, failures[0], tc.want)
		})
	}
}

func TestNormalized(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, Normalized([]string{"B", "a", "b"}, strings.ToLower))
	require.Equal(t, []string{"B", "a", "b"}, Normalized([]string{"b", "B", "a"}, nil),
		"a nil normalize sorts the names as written")
}

func TestBehavior_Merge(t *testing.T) {
	merged := Behavior{ContentTypes: []string{"application/xml"}, Parameters: []string{"q"}, Statuses: []int{404}}.
		Merge(Behavior{Parameters: []string{"cat", "q"}, SetsDynamicContentType: true, Statuses: []int{200, 404}})
	require.Equal(t, Behavior{
		ContentTypes:           []string{"application/xml"},
		Parameters:             []string{"cat", "q"},
		SetsDynamicContentType: true,
		Statuses:               []int{200, 404},
	}, merged)
}

func TestAssertBehavior(t *testing.T) {
	documented := map[string]Documented{
		"GET /a": {ContentTypes: []string{"application/xml"}, Parameters: []string{"Query"}, Statuses: []int{404, 503}},
		"GET /b": {Statuses: []int{200}},
	}
	behaviors := map[string]Behavior{
		"GET /a": {ContentTypes: []string{"text/xml"}, Parameters: []string{"query", "t"}, Statuses: []int{404, 500}},
	}
	failures := recordFailures(t, func(tb testing.TB) {
		tb.Helper()
		AssertBehavior(tb, documented, behaviors, strings.ToLower)
	})
	require.Len(t, failures, 5, "%q", failures)
	for i, want := range []string{
		"GET /a's handler can answer 500, which the spec does not document",
		"the spec documents 503 for GET /a, which no code its handler reaches writes",
		`GET /a's handler reads the query parameter "t", which the spec does not document`,
		"GET /a's handler sets the content type text/xml, which the spec does not document",
		"the spec documents the content type application/xml for GET /a, which no code its handler reaches sets",
	} {
		require.Contains(t, failures[i], want)
	}

	failures = recordFailures(t, func(tb testing.TB) {
		tb.Helper()
		AssertBehavior(tb, map[string]Documented{"GET /a": {Parameters: []string{"Query"}}},
			map[string]Behavior{"GET /a": {Parameters: []string{"query"}}}, nil)
	})
	require.Len(t, failures, 2, "without normalize, names compare as written")

	require.Empty(t, recordFailures(t, func(tb testing.TB) {
		tb.Helper()
		AssertBehavior(tb, map[string]Documented{"GET /a": {ContentTypes: []string{"application/x-bittorrent"}, Statuses: []int{200}}},
			map[string]Behavior{"GET /a": {SetsDynamicContentType: true, Statuses: []int{200}}}, nil)
	}), "a content type computed at run time may be the documented one, and an operation with no handler is left to the path test")
}

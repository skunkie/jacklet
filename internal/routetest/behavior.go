// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package routetest

import (
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Source is a package's non-test Go files, parsed, with every function,
// method, variable and constant they declare at package level looked up by
// name.
type Source struct {
	Fset *token.FileSet
	// decls holds every declaration of a name, a method of any receiver
	// alongside a function, since a call is followed by name alone.
	decls map[string][]*ast.FuncDecl
	// imports maps each file to the import path of every name it imports
	// a package under; a name is a package's only in a file that imports
	// it, and only where nothing local declares it.
	imports map[string]map[string]string
	// values holds the package-level variable and constant declarations
	// of each name.
	values map[string][]*ast.ValueSpec
}

// sources holds every Source LoadSource has parsed, by absolute
// directory. Nothing changes a Source once it is loaded, so every test in
// a binary can share one.
var sources sync.Map

// LoadSource parses the non-test Go files in dir, once per test binary:
// a later call for the same directory returns the Source the first one
// parsed. Files for every platform are read, whatever their build
// constraints, so a function declared once per platform has each
// declaration followed.
func LoadSource(tb testing.TB, dir string) *Source {
	tb.Helper()
	abs, err := filepath.Abs(dir)
	require.NoError(tb, err, "failed to resolve %s", dir)
	if cached, ok := sources.Load(abs); ok {
		return cached.(*Source)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(tb, err, "failed to read %s", dir)

	source := &Source{
		Fset:    token.NewFileSet(),
		decls:   make(map[string][]*ast.FuncDecl),
		imports: make(map[string]map[string]string),
		values:  make(map[string][]*ast.ValueSpec),
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(source.Fset, path, nil, 0)
		require.NoError(tb, err, "failed to parse %s", name)
		imports := make(map[string]string)
		source.imports[path] = imports
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			require.NoError(tb, err)
			imported := importPath[strings.LastIndex(importPath, "/")+1:]
			if spec.Name != nil {
				imported = spec.Name.Name
			}
			imports[imported] = importPath
		}
		for _, node := range file.Decls {
			switch decl := node.(type) {
			case *ast.FuncDecl:
				source.decls[decl.Name.Name] = append(source.decls[decl.Name.Name], decl)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for _, ident := range value.Names {
							source.values[ident.Name] = append(source.values[ident.Name], value)
						}
					}
				}
			}
		}
	}
	// A concurrent first load may have stored its own parse meanwhile;
	// every caller gets whichever was stored first.
	stored, _ := sources.LoadOrStore(abs, source)
	return stored.(*Source)
}

// Func returns the declaration of the named function: a method of the
// named receiver type, or a package-level function when receiver is "".
func (s *Source) Func(tb testing.TB, receiver, name string) *ast.FuncDecl {
	tb.Helper()
	var found []*ast.FuncDecl
	for _, fn := range s.decls[name] {
		if receiverName(fn) == receiver {
			found = append(found, fn)
		}
	}
	require.Len(tb, found, 1, "want one declaration of %q on %q; this test needs updating", name, receiver)
	return found[0]
}

// receiverName returns the type name of a method's receiver, pointer or
// not, or "" for a package-level function.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// Behavior is what a handler's code can do, as read from its source.
type Behavior struct {
	// ContentTypes are the media types it sets a Content-Type header to,
	// where the value is a literal, or that a function of another package
	// it calls sets, as http.Error sets text/plain and http.Redirect
	// text/html.
	ContentTypes []string
	// Parameters are the query parameters it reads, by the names its code
	// looks them up under.
	Parameters []string
	// SetsDynamicContentType reports whether it also sets a Content-Type
	// computed at run time, which no literal in the source names.
	SetsDynamicContentType bool
	// Statuses are the HTTP statuses it can write.
	Statuses []int
}

// Merge returns what either b or other can do, as for an operation two
// routes serve.
func (b Behavior) Merge(other Behavior) Behavior {
	union := func(a, b []string) []string {
		return slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(a), b...))))
	}
	return Behavior{
		ContentTypes:           union(b.ContentTypes, other.ContentTypes),
		Parameters:             union(b.Parameters, other.Parameters),
		SetsDynamicContentType: b.SetsDynamicContentType || other.SetsDynamicContentType,
		Statuses:               slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(b.Statuses), other.Statuses...)))),
	}
}

// Behavior reads what a handler can do, as registered in a Handle or
// HandleFunc call, from the code it reaches. A handler is followed into
// each function, method, variable and constant of the package its code
// names, called or passed as a value, which is how what a shared helper
// does, such as writing a failure or reading a parameter, is found for
// every handler that reaches it. A name is followed to every declaration
// that has it, whatever the receiver, so the result can overstate what a
// handler does. It misses only code no name in the handler leads to, such
// as a function reached through a struct field a constructor fills.
//
// The statuses are every net/http status constant in that code. The 200 a
// handler answers when it writes a body without a header has no constant,
// so it is listed when the code writes one: a Write, WriteString or Encode
// call on any value, or one of io's copies, fmt's Fprint functions or
// net/http's ServeContent and ServeFile family. That too can overstate,
// since a body written after an error's header counts. A responder that
// writes a status of its own, as http.NotFound writes 404, adds it.
//
// The parameters are every string literal passed to a method of, or used
// to index, a value derived from the request's URL query: the result of a
// Query() call, a url.Values parameter, or a local or call built from
// either, the way r.URL.Query().Get("t") and a helper wrapping url.Values
// both read one. The content types are the literal values a Content-Type
// header is set, added or assigned, through a Header() call or a local
// holding one, without their parameters, and the media type each call to
// one of responders sets.
//
// The handler may be a function literal, a function or a variable holding
// one, a method value (t.Results), a value whose ServeHTTP serves (t), or a
// call that returns the handler (healthCheck(store)). Which of these an
// identifier is comes from what it is declared as, not its name alone, so
// a local value shadowing a package function is read as the value. One
// from another package is refused, since its code is not in this source.
func (s *Source) Behavior(tb testing.TB, handler ast.Expr) Behavior {
	tb.Helper()
	walk := handlerWalk{
		constants:    httpConstants(tb),
		contentTypes: make(map[string]bool),
		parameters:   make(map[string]bool),
		seen:         make(map[ast.Node]bool),
		source:       s,
		statuses:     make(map[int]bool),
	}
	s.walkHandler(tb, &walk, handler)
	return Behavior{
		ContentTypes:           slices.Sorted(maps.Keys(walk.contentTypes)),
		Parameters:             slices.Sorted(maps.Keys(walk.parameters)),
		SetsDynamicContentType: walk.setsDynamicContentType,
		Statuses:               slices.Sorted(maps.Keys(walk.statuses)),
	}
}

// walkHandler walks the code a handler of any shape Behavior reads runs.
// A local is read as the value it is declared with, by the same rules as
// a handler written in its place, so naming a handler first changes
// nothing about how it is read. One declared without a single value of its
// own, as by var h http.Handler or h, err := build(), is refused, since
// what it holds is not where it is declared; a later assignment to a local
// is not read.
func (s *Source) walkHandler(tb testing.TB, walk *handlerWalk, handler ast.Expr) {
	tb.Helper()
	switch h := handler.(type) {
	case *ast.FuncLit:
		walk.node(h.Body)
	case *ast.Ident:
		if value, isLocal := s.localValue(h); isLocal {
			_, isParam := h.Obj.Decl.(*ast.Field)
			require.True(tb, value != nil || isParam,
				"%s: the handler %s is declared without a single value of its own, so what it holds cannot be read",
				s.Fset.Position(h.Pos()), h.Name)
			switch value.(type) {
			case *ast.CallExpr, *ast.FuncLit, *ast.Ident, *ast.SelectorExpr:
				s.walkHandler(tb, walk, value)
			default:
				// A parameter, or a local holding a value such as
				// &server{}, serves through its ServeHTTP.
				walk.decls(s.decls["ServeHTTP"])
			}
		} else if s.declaresAtPackageLevel(h.Name) {
			walk.node(h)
		} else {
			walk.decls(s.decls["ServeHTTP"])
		}
	case *ast.SelectorExpr:
		require.False(tb, s.isImport(h.X), "%s: the handler %s is another package's, so what it does cannot be read",
			s.Fset.Position(h.Pos()), types.ExprString(h))
		walk.decls(s.decls[h.Sel.Name])
	case *ast.CallExpr:
		if sel, ok := h.Fun.(*ast.SelectorExpr); ok {
			require.False(tb, s.isImport(sel.X), "%s: the handler %s is built by another package, so what it does cannot be read",
				s.Fset.Position(h.Pos()), types.ExprString(h))
		}
		walk.node(h)
	default:
		require.FailNowf(tb, "unreadable handler", "%s: the handler %s is not a shape this test reads",
			s.Fset.Position(handler.Pos()), types.ExprString(handler))
	}
}

// declaresAtPackageLevel reports whether the package declares name as a
// function or a variable or constant, rather than only as a method.
func (s *Source) declaresAtPackageLevel(name string) bool {
	for _, fn := range s.decls[name] {
		if fn.Recv == nil {
			return true
		}
	}
	return len(s.values[name]) > 0
}

// localValue reports whether ident names a variable or constant declared
// inside a function, a parameter or receiver included, and returns the
// value it is declared with, when its declaration gives one.
func (s *Source) localValue(ident *ast.Ident) (value ast.Expr, isLocal bool) {
	if ident.Obj == nil || (ident.Obj.Kind != ast.Var && ident.Obj.Kind != ast.Con) {
		return nil, false
	}
	switch decl := ident.Obj.Decl.(type) {
	case *ast.AssignStmt:
		for i, lhs := range decl.Lhs {
			if IsIdent(lhs, ident.Name) && len(decl.Rhs) == len(decl.Lhs) {
				return decl.Rhs[i], true
			}
		}
	case *ast.ValueSpec:
		if slices.Contains(s.values[ident.Name], decl) {
			return nil, false
		}
		for i, name := range decl.Names {
			if name.Name == ident.Name && i < len(decl.Values) {
				return decl.Values[i], true
			}
		}
	}
	return nil, true
}

// isImport reports whether an expression is the name of an imported
// package.
func (s *Source) isImport(node ast.Expr) bool {
	return s.importPath(node) != ""
}

// importPath returns the path of the package an expression names, or ""
// when it names none: an identifier names a package only where its own
// file imports one under it and nothing in that file declares it, the
// way a parameter named url shadows net/url.
func (s *Source) importPath(node ast.Expr) string {
	ident, ok := node.(*ast.Ident)
	if !ok || ident.Obj != nil {
		return ""
	}
	if file := s.Fset.File(ident.Pos()); file != nil {
		return s.imports[file.Name()][ident.Name]
	}
	return ""
}

// handlerWalk is one Behavior call's traversal: the declarations it has
// entered and what it has found the handler doing.
type handlerWalk struct {
	constants              map[string]int
	contentTypes           map[string]bool
	parameters             map[string]bool
	seen                   map[ast.Node]bool
	setsDynamicContentType bool
	source                 *Source
	statuses               map[int]bool
}

// decls enters each declaration not entered before.
func (w *handlerWalk) decls(decls []*ast.FuncDecl) {
	for _, fn := range decls {
		if !w.seen[fn] && fn.Body != nil {
			w.seen[fn] = true
			w.node(fn.Body)
		}
	}
}

// values enters each variable or constant declaration not entered before.
func (w *handlerWalk) values(specs []*ast.ValueSpec) {
	for _, spec := range specs {
		if !w.seen[spec] {
			w.seen[spec] = true
			for _, value := range spec.Values {
				w.node(value)
			}
		}
	}
}

// node records what node does and enters every function of the package it
// names.
func (w *handlerWalk) node(node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			w.call(n)
		case *ast.AssignStmt:
			w.headerAssignment(n)
		case *ast.IndexExpr:
			if w.source.IsQuery(n.X) {
				w.parameter(n.Index)
			}
		case *ast.SelectorExpr:
			if path := w.source.importPath(n.X); path != "" {
				if status, isKnown := w.constants[n.Sel.Name]; isKnown && path == "net/http" {
					w.statuses[status] = true
				}
				return false
			}
			w.decls(w.source.decls[n.Sel.Name])
			// The selected name is followed above; only the expression
			// it is selected from is left to read.
			w.node(n.X)
			return false
		case *ast.Ident:
			// A local names nothing at package level, whatever it shares a
			// name with; its value is read where it is assigned.
			if _, isLocal := w.source.localValue(n); isLocal {
				return true
			}
			for _, fn := range w.source.decls[n.Name] {
				if fn.Recv == nil {
					w.decls([]*ast.FuncDecl{fn})
				}
			}
			w.values(w.source.values[n.Name])
		}
		return true
	})
}

// call records what a call does: write a body, set a content type, or
// read query parameters by name.
func (w *handlerWalk) call(call *ast.CallExpr) {
	if w.writesBody(call) {
		w.statuses[http.StatusOK] = true
	}
	w.contentType(call)
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && w.source.IsQuery(sel.X) {
		for _, arg := range call.Args {
			w.parameter(arg)
		}
	}
}

// parameter records a query parameter's name, when expr is a literal one.
func (w *handlerWalk) parameter(expr ast.Expr) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if name, err := strconv.Unquote(lit.Value); err == nil {
			w.parameters[name] = true
		}
	}
}

// IsQuery reports whether expr is derived from the request's URL query: a
// Query() call, a url.Values parameter, a local built from one, or a call
// to one of the package's own functions taking one, the way a helper
// wraps the query. A method called with the query, such as a logger's
// With, does not pass it on, since what it returns is its receiver's, not
// the query's.
func (s *Source) IsQuery(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return s.IsQuery(e.X)
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Query" && len(e.Args) == 0 {
			return true
		}
		fn, ok := e.Fun.(*ast.Ident)
		if !ok || !s.declaresAtPackageLevel(fn.Name) {
			return false
		}
		if _, isLocal := s.localValue(fn); isLocal {
			return false
		}
		return slices.ContainsFunc(e.Args, s.IsQuery)
	case *ast.Ident:
		value, isLocal := s.localValue(e)
		if !isLocal {
			return false
		}
		if value != nil {
			return s.IsQuery(value)
		}
		field, ok := e.Obj.Decl.(*ast.Field)
		if !ok {
			return false
		}
		sel, ok := field.Type.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Values" && s.importPath(sel.X) == "net/url"
	}
	return false
}

// responder is what a function of another package that answers a request
// itself writes: the media type it sets, and the status it writes when
// the status is not one of its arguments, which no constant in the
// handler's own code then names.
type responder struct {
	mediaType string
	status    int
}

// responders are the functions of other packages, by import path, that
// answer a request themselves.
var responders = map[string]map[string]responder{
	"net/http": {
		"Error":    {mediaType: "text/plain"},
		"NotFound": {mediaType: "text/plain", status: http.StatusNotFound},
		"Redirect": {mediaType: "text/html"},
	},
}

// contentType records the media type a Header().Set or Header().Add of
// "Content-Type" sets, or one of responders sets along with any status of
// its own, or that it sets one computed at run time.
func (w *handlerWalk) contentType(call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if path := w.source.importPath(sel.X); path != "" {
		if answer, ok := responders[path][sel.Sel.Name]; ok {
			w.contentTypes[answer.mediaType] = true
			if answer.status != 0 {
				w.statuses[answer.status] = true
			}
		}
		return
	}
	if (sel.Sel.Name != "Set" && sel.Sel.Name != "Add") || len(call.Args) != 2 || !w.isHeader(sel.X) {
		return
	}
	if isContentTypeKey(call.Args[0]) {
		w.mediaType(call.Args[1])
	}
}

// headerAssignment records the media type an assignment to a header's
// "Content-Type" entry sets, as w.Header()["Content-Type"] = []string{...}
// does.
func (w *handlerWalk) headerAssignment(assign *ast.AssignStmt) {
	for i, lhs := range assign.Lhs {
		index, ok := lhs.(*ast.IndexExpr)
		if !ok || !w.isHeader(index.X) || !isContentTypeKey(index.Index) || len(assign.Rhs) != len(assign.Lhs) {
			continue
		}
		values, ok := assign.Rhs[i].(*ast.CompositeLit)
		if !ok || len(values.Elts) != 1 {
			w.setsDynamicContentType = true
			continue
		}
		w.mediaType(values.Elts[0])
	}
}

// isHeader reports whether expr is a response's header: a Header() call,
// or a local holding one.
func (w *handlerWalk) isHeader(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Header" && len(e.Args) == 0
	case *ast.Ident:
		value, isLocal := w.source.localValue(e)
		return isLocal && value != nil && w.isHeader(value)
	}
	return false
}

// isContentTypeKey reports whether expr is the literal "Content-Type", in
// any case.
func isContentTypeKey(expr ast.Expr) bool {
	key, ok := expr.(*ast.BasicLit)
	return ok && key.Kind == token.STRING && strings.EqualFold(key.Value, `"Content-Type"`)
}

// mediaType records the media type a header value names, without its
// parameters, or that the value is computed at run time.
func (w *handlerWalk) mediaType(expr ast.Expr) {
	value, ok := expr.(*ast.BasicLit)
	if !ok || value.Kind != token.STRING {
		w.setsDynamicContentType = true
		return
	}
	if unquoted, err := strconv.Unquote(value.Value); err == nil {
		mediaType, _, _ := strings.Cut(unquoted, ";")
		w.contentTypes[strings.ToLower(strings.TrimSpace(mediaType))] = true
	}
}

// bodyWriters are the functions of other packages, by import path, that
// write a response body.
var bodyWriters = map[string][]string{
	"fmt":      {"Fprint", "Fprintf", "Fprintln"},
	"io":       {"Copy", "CopyBuffer", "CopyN", "WriteString"},
	"net/http": {"ServeContent", "ServeFile", "ServeFileFS"},
}

// writesBody reports whether a call writes a response body: a Write,
// WriteString or Encode method called on any value, or one of
// bodyWriters.
func (w *handlerWalk) writesBody(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if path := w.source.importPath(sel.X); path != "" {
		return slices.Contains(bodyWriters[path], sel.Sel.Name)
	}
	return slices.Contains([]string{"Encode", "Write", "WriteString"}, sel.Sel.Name)
}

// httpConstantsOnce guards httpConstantsByName, which type-checking
// net/http from source makes worth computing once per test binary.
var (
	httpConstantsOnce   sync.Once
	httpConstantsByName map[string]int
	errHTTPConstants    error
)

// httpConstants returns the value of every net/http status constant, by
// name, read from net/http's own declarations.
func httpConstants(tb testing.TB) map[string]int {
	tb.Helper()
	httpConstantsOnce.Do(func() {
		pkg, err := importer.ForCompiler(token.NewFileSet(), "source", nil).Import("net/http")
		if err != nil {
			errHTTPConstants = err
			return
		}
		httpConstantsByName = make(map[string]int)
		for _, name := range pkg.Scope().Names() {
			value, ok := pkg.Scope().Lookup(name).(*types.Const)
			if !ok || !strings.HasPrefix(name, "Status") {
				continue
			}
			if status, isExact := constant.Int64Val(value.Val()); isExact {
				httpConstantsByName[name] = int(status)
			}
		}
	})
	require.NoError(tb, errHTTPConstants, "failed to read net/http's status constants")
	return httpConstantsByName
}

// Documented is what the spec documents for one operation.
type Documented struct {
	// ContentTypes are the media types its responses document.
	ContentTypes []string
	// IsSecured reports whether a server holding a key requires it: its
	// own security, or the document's when it states none, names a scheme
	// in some alternative. An alternative naming none, as {} does, is how
	// the spec lets a server started without a key answer unauthenticated,
	// so it does not leave open an operation a key protects; security: []
	// does.
	IsSecured bool
	// Parameters are its query parameters, and the query parameter of
	// every API-key scheme its security names.
	Parameters []string
	// Statuses are the statuses of its responses.
	Statuses []int
}

// specNode is the part of a spec Document reads: its paths, its
// components and the security that applies where an operation states
// none.
type specNode struct {
	Components struct {
		Parameters      map[string]specParameter `yaml:"parameters"`
		Responses       map[string]specResponse  `yaml:"responses"`
		SecuritySchemes map[string]struct {
			In   string `yaml:"in"`
			Name string `yaml:"name"`
			Type string `yaml:"type"`
		} `yaml:"securitySchemes"`
	} `yaml:"components"`
	Paths    map[string]map[string]yaml.Node `yaml:"paths"`
	Security []map[string][]string           `yaml:"security"`
}

// specParameter is a parameter as an operation lists it, inline or by
// reference.
type specParameter struct {
	In   string `yaml:"in"`
	Name string `yaml:"name"`
	Ref  string `yaml:"$ref"`
}

// specResponse is a response as an operation lists it, inline or by
// reference.
type specResponse struct {
	Content map[string]yaml.Node `yaml:"content"`
	Ref     string               `yaml:"$ref"`
}

// specOperation is the part of an operation Document reads.
type specOperation struct {
	Parameters []specParameter         `yaml:"parameters"`
	Responses  map[string]specResponse `yaml:"responses"`
	Security   *[]map[string][]string  `yaml:"security"`
}

// Document returns what the spec documents for each operation under every
// path include accepts, keyed as Operations keys them. A response the spec
// keys by anything but a status, such as "default", fails the test, since
// no handler's statuses can be held to it, as does a reference to a
// component the spec does not define.
func Document(tb testing.TB, spec []byte, include func(path string) bool) map[string]Documented {
	tb.Helper()
	var doc specNode
	require.NoError(tb, yaml.Unmarshal(spec, &doc), "failed to read the spec's paths, components and security")

	documented := make(map[string]Documented)
	for path, item := range doc.Paths {
		if !include(path) {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(item)) {
			if !slices.Contains(methods, key) {
				continue
			}
			node := item[key]
			var op specOperation
			require.NoError(tb, node.Decode(&op), "failed to read %s %s", key, path)
			operation := strings.ToUpper(key) + " " + path
			documented[operation] = doc.document(tb, operation, op)
		}
	}
	return documented
}

// document reads one operation.
func (doc *specNode) document(tb testing.TB, operation string, op specOperation) Documented {
	tb.Helper()
	parameters := make(map[string]bool)
	for _, param := range op.Parameters {
		if param.Ref != "" {
			resolved, ok := doc.Components.Parameters[strings.TrimPrefix(param.Ref, "#/components/parameters/")]
			require.True(tb, ok, "%s references the parameter %q, which the spec does not define", operation, param.Ref)
			param = resolved
		}
		if param.In == "query" {
			parameters[param.Name] = true
		}
	}

	security := doc.Security
	if op.Security != nil {
		security = *op.Security
	}
	var isSecured bool
	for _, requirement := range security {
		isSecured = isSecured || len(requirement) > 0
		for _, name := range slices.Sorted(maps.Keys(requirement)) {
			scheme, ok := doc.Components.SecuritySchemes[name]
			require.True(tb, ok, "%s's security names the scheme %q, which the spec does not define", operation, name)
			if scheme.Type == "apiKey" && scheme.In == "query" {
				parameters[scheme.Name] = true
			}
		}
	}

	contentTypes := make(map[string]bool)
	statuses := make([]int, 0, len(op.Responses))
	for code, response := range op.Responses {
		status, err := strconv.Atoi(code)
		require.NoError(tb, err, "%s documents the response %q, which is not a status", operation, code)
		statuses = append(statuses, status)
		if response.Ref != "" {
			resolved, ok := doc.Components.Responses[strings.TrimPrefix(response.Ref, "#/components/responses/")]
			require.True(tb, ok, "%s references the response %q, which the spec does not define", operation, response.Ref)
			response = resolved
		}
		for mediaType := range response.Content {
			contentTypes[mediaType] = true
		}
	}
	slices.Sort(statuses)
	return Documented{
		ContentTypes: slices.Sorted(maps.Keys(contentTypes)),
		IsSecured:    isSecured,
		Parameters:   slices.Sorted(maps.Keys(parameters)),
		Statuses:     statuses,
	}
}

// Normalized returns names passed through normalize, sorted, without
// duplicates; a nil normalize leaves each name as written.
func Normalized(names []string, normalize func(name string) string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name
		if normalize != nil {
			out[i] = normalize(name)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

// AssertBehavior holds each operation's documented statuses, query
// parameters and content types to behaviors, what the handlers serving it
// do, in both directions: what a handler does must be documented, and what
// is documented must be something a handler does. Parameter names go
// through normalize first on both sides, for a handler that looks them up
// ignoring case; a nil normalize compares them as written. A documented
// content type is not required of a handler that also sets one computed at
// run time, which may be the one documented. An operation with no handler
// in behaviors is left to the test that holds paths to routes. Every
// failure is reported, not only the first.
func AssertBehavior(tb testing.TB, documented map[string]Documented, behaviors map[string]Behavior, normalize func(name string) string) {
	tb.Helper()
	normalized := func(names []string) []string { return Normalized(names, normalize) }
	for _, operation := range slices.Sorted(maps.Keys(documented)) {
		behavior, ok := behaviors[operation]
		if !ok {
			continue
		}
		spec := documented[operation]
		for _, status := range behavior.Statuses {
			assert.Contains(tb, spec.Statuses, status,
				"%s's handler can answer %d, which the spec does not document", operation, status)
		}
		for _, status := range spec.Statuses {
			assert.Contains(tb, behavior.Statuses, status,
				"the spec documents %d for %s, which no code its handler reaches writes", status, operation)
		}

		read, listed := normalized(behavior.Parameters), normalized(spec.Parameters)
		for _, name := range read {
			assert.Contains(tb, listed, name,
				"%s's handler reads the query parameter %q, which the spec does not document", operation, name)
		}
		for _, name := range listed {
			assert.Contains(tb, read, name,
				"the spec documents the query parameter %q for %s, which no code its handler reaches reads", name, operation)
		}

		for _, mediaType := range behavior.ContentTypes {
			assert.Contains(tb, spec.ContentTypes, mediaType,
				"%s's handler sets the content type %s, which the spec does not document", operation, mediaType)
		}
		if !behavior.SetsDynamicContentType {
			for _, mediaType := range spec.ContentTypes {
				assert.Contains(tb, behavior.ContentTypes, mediaType,
					"the spec documents the content type %s for %s, which no code its handler reaches sets", mediaType, operation)
			}
		}
	}
}

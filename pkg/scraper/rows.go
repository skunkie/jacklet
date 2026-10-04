// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// resultRow is one scraped result, abstracting over the two response
// formats a definition can declare: an HTML element selected by CSS, or a
// JSON object addressed by dotted key path. Field extraction is written
// once against this interface rather than twice per format.
type resultRow interface {
	// debugString renders the row as the tracker served it -- markup for
	// an HTML row, JSON for a JSON one. It backs the "strdump" row filter,
	// whose whole purpose is showing what a selector is being written
	// against, so it must not be reduced to extracted values.
	debugString() string
	// lookup returns the value a field selects, and whether the selector
	// matched anything at all. An empty selector addresses the row itself.
	lookup(field Field) (value string, isMatched bool)
	// matches reports whether a field's "case" arm matches the row: for
	// markup, whether the arm's selector matches the row itself or
	// anything in it, and for JSON, whether the arm equals the row's
	// value, since Jackett compares a JSON arm with the value its field
	// selected.
	matches(selector string) bool
	// narrow returns the row reduced to what selector selects, the way a
	// field reads it, and whether it selected anything. A field's "case"
	// arms are resolved against this narrowed row, as Jackett tests them
	// against the field's selection rather than against the whole row.
	narrow(selector string) (resultRow, bool)
}

// htmlRow is a result row selected from an HTML document.
type htmlRow struct {
	sel *goquery.Selection
}

// debugString is the row's own markup, which is what a definition author
// is comparing their selectors against.
func (r htmlRow) debugString() string {
	html, err := goquery.OuterHtml(r.sel)
	if err != nil {
		return fmt.Sprintf("<unrenderable row: %v>", err)
	}
	return html
}

// rootSelector reports whether selector starts at the document root, as a
// definition's ":root" prefix asks, and returns the rest of it. Jackett
// strips the prefix and runs what remains from the topmost element, which
// is how a field or a case arm reads something outside its own row, such
// as a page-wide banner; goquery would match ":root" itself but return
// only what lies inside the row.
func rootSelector(selector string) (rest string, isFromRoot bool) {
	rest, isFromRoot = strings.CutPrefix(selector, ":root")
	return strings.TrimSpace(rest), isFromRoot
}

// scope returns where a selector runs and what to run there: the row
// itself, or, for a ":root" selector, the document's topmost element and
// the selector without its prefix. A bare ":root" selects that element.
func (r htmlRow) scope(selector string) (*goquery.Selection, string) {
	rest, isFromRoot := rootSelector(selector)
	if !isFromRoot {
		return r.sel, selector
	}
	root := r.sel.Parents().Last()
	if root.Length() == 0 {
		root = r.sel
	}
	return root, rest
}

func (r htmlRow) matches(selector string) bool {
	if selector == "*" || selector == "" {
		return true
	}
	scope, rest := r.scope(selector)
	if rest == "" {
		return true
	}
	return findIn(scope, rest).Length() > 0 || (scope == r.sel && isMatch(r.sel, rest))
}

// selection resolves selector to the one element a field reads. A selector
// the row itself matches selects the row, as Jackett resolves it, before
// any descendant is searched: a definition whose rows are links reads each
// link's own href by naming the link, and searching only inside the row
// would find nothing and drop every row. Of several matches only the first
// is taken, as Jackett's QuerySelector returns it; goquery's Text would
// join them all, so a title selected by "a" in a row of several links
// would read as every link's text run together.
func (r htmlRow) selection(selector string) (*goquery.Selection, bool) {
	scope, rest := r.scope(selector)
	sel := scope
	// A bare ":root" names the root, and a selector the row matches names
	// the row; anything else is searched for below the scope.
	if isRow := scope == r.sel && isMatch(r.sel, rest); rest != "" && !isRow {
		sel = findIn(scope, rest)
	}
	if sel.Length() == 0 {
		return nil, false
	}
	return sel.First(), true
}

func (r htmlRow) narrow(selector string) (resultRow, bool) {
	sel, ok := r.selection(selector)
	if !ok {
		return nil, false
	}
	return htmlRow{sel: sel}, true
}

func (r htmlRow) lookup(field Field) (string, bool) {
	sel := r.sel
	if field.Selector != "" {
		selected, ok := r.selection(field.Selector)
		if !ok {
			return "", false
		}
		sel = selected
	}

	if field.Remove != "" {
		// Clone first: the row is reused by later fields, which must still
		// see the elements this field strips out of its own value.
		sel = sel.Clone()
		findIn(sel, field.Remove).Remove()
	}

	if field.Attribute != "" {
		value, ok := sel.Attr(field.Attribute)
		return value, ok
	}
	return sel.Text(), true
}

// jsonRow is a result row taken from a JSON response. parent is the
// element of the rows array it came from, which differs from value when
// "rows.attribute" narrowed that element to an object inside it, and is
// what a field selector starting with ".." reads, as in Jackett.
type jsonRow struct {
	parent any
	value  any
}

// resolve looks selector up in the row, or in the element it came from
// when the selector starts with "..".
func (r jsonRow) resolve(selector string) (any, bool) {
	scope := r.value
	if strings.HasPrefix(selector, "..") && r.parent != nil {
		scope = r.parent
	}
	return jsonSelect(scope, strings.TrimLeft(selector, "."))
}

// jsonSelect resolves a JSON selector as Jackett does: the path before the
// first ":" addresses a value, the value itself when the path is blank,
// and each ":has(...)", ":not(...)" or ":contains(...)" after it must hold
// for that value, or nothing is selected. ":has" and ":not" test whether
// their argument, itself a selector, selects anything from the value, and
// ":contains" whether the value's text contains its argument as written.
func jsonSelect(value any, selector string) (any, bool) {
	path, filters, _ := strings.Cut(selector, ":")
	target := value
	if strings.TrimSpace(path) != "" {
		v, ok := jsonLookup(value, path)
		if !ok {
			return nil, false
		}
		target = v
	}
	for name, arg := range jsonFilters(filters) {
		var isSatisfied bool
		switch name {
		case "has", "not":
			_, isSatisfied = jsonSelect(target, arg)
			if name == "not" {
				isSatisfied = !isSatisfied
			}
		case "contains":
			isSatisfied = strings.Contains(jsonText(target), arg)
		default:
			// Jackett logs an unknown filter and goes on without it.
			continue
		}
		if !isSatisfied {
			return nil, false
		}
	}
	return target, true
}

// jsonFilters yields each filter in what follows a selector's path, as
// Jackett's pattern for them reads it: a name up to the first "(", and an
// argument up to the first ")" that ends the selector or comes before the
// next ":", so a filter's argument may be a selector with filters of its
// own, as in ":not(promotion_info.down_multiplier:contains(1))".
func jsonFilters(filters string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		rest := filters
		for {
			open := strings.IndexByte(rest, '(')
			if open < 0 {
				return
			}
			end := filterEnd(rest, open)
			if end < 0 {
				return
			}
			if !yield(strings.TrimPrefix(rest[:open], ":"), rest[open+1:end]) {
				return
			}
			rest = rest[end+1:]
		}
	}
}

// filterEnd returns the index of the ")" closing the filter whose "(" is
// at open: the first one that ends s or comes before a ":", or -1.
func filterEnd(s string, open int) int {
	for j := open + 1; j < len(s); j++ {
		if s[j] == ')' && (j+1 == len(s) || s[j+1] == ':') {
			return j
		}
	}
	return -1
}

// jsonText is the text ":contains" searches: a scalar as a field reads it,
// and an object or array as JSON.
func jsonText(value any) string {
	switch value.(type) {
	case map[string]any, []any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
	return jsonScalar(value)
}

// debugString is the row re-encoded as JSON, the form its key paths address.
func (r jsonRow) debugString() string {
	encoded, err := json.Marshal(r.value)
	if err != nil {
		return fmt.Sprintf("%v", r.value)
	}
	return string(encoded)
}

func (r jsonRow) narrow(selector string) (resultRow, bool) {
	v, ok := r.resolve(selector)
	if !ok {
		return nil, false
	}
	return jsonRow{value: v}, true
}

func (r jsonRow) matches(selector string) bool {
	if selector == "*" || selector == "" {
		return true
	}
	return jsonScalar(r.value) == selector
}

func (r jsonRow) lookup(field Field) (string, bool) {
	// A JSON definition addresses a value by key path; "attribute" is an
	// HTML concept, so it selects a key relative to the field's selector.
	selector := field.Selector
	if field.Attribute != "" {
		if selector == "" {
			selector = field.Attribute
		} else {
			selector += "." + field.Attribute
		}
	}

	if selector == "" {
		return jsonScalar(r.value), true
	}

	v, ok := r.resolve(selector)
	if !ok {
		return "", false
	}
	return jsonScalar(v), true
}

// jsonLookup resolves a dotted key path against a decoded JSON value,
// supporting object keys and bracketed array indexes ("items[0].name").
// A leading "$." is accepted and ignored, since definitions write paths
// both ways.
func jsonLookup(value any, path string) (any, bool) {
	path = strings.TrimPrefix(path, "$.")
	path = strings.TrimPrefix(path, "$")
	if path == "" {
		return value, true
	}

	current := value
	for segment := range strings.SplitSeq(path, ".") {
		if segment == "" {
			continue
		}

		key, indexes := parseIndexes(segment)
		if key != "" {
			obj, ok := current.(map[string]any)
			if !ok {
				return nil, false
			}
			current, ok = obj[key]
			if !ok {
				return nil, false
			}
		}

		for _, index := range indexes {
			arr, ok := current.([]any)
			if !ok || index < 0 || index >= len(arr) {
				return nil, false
			}
			current = arr[index]
		}
	}
	return current, true
}

// parseIndexes splits a path segment such as "items[0][1]" into its key
// and its bracketed indexes.
func parseIndexes(segment string) (key string, indexes []int) {
	key = segment
	if open := strings.IndexByte(segment, '['); open >= 0 {
		key = segment[:open]
		for part := range strings.SplitSeq(segment[open:], "[") {
			part = strings.TrimSuffix(strings.TrimSpace(part), "]")
			if part == "" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			indexes = append(indexes, n)
		}
	}
	return key, indexes
}

// jsonScalar renders a decoded JSON value as the string a field yields.
// Numbers lose the float formatting encoding/json gives them, so an id or
// a byte count reads as "12345" rather than "1.2345e+04".
func jsonScalar(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		// .NET's rendering, which is how Jackett turns a JSON value into
		// text: definitions compare it with "True" and "False", in case
		// arms and templates alike.
		if v {
			return "True"
		}
		return "False"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = jsonScalar(item)
		}
		return strings.Join(parts, ",")
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsonRows resolves a definition's row selector against a decoded JSON
// document and returns its rows, shaped by the rows block as Jackett shapes
// them (splitJSONRow). A selector addressing a single object yields that
// one row, and one addressing null yields none. A selector naming a key the
// document lacks is an error, as in Jackett, so an answer of another shape
// reads as a failure rather than as no results, unless the definition says
// that is how its tracker answers no results
// ("missingAttributeEqualsNoResults").
func jsonRows(document any, spec Rows, selector string) ([]resultRow, error) {
	path, filters, _ := strings.Cut(selector, ":")
	target, ok := jsonLookup(document, path)
	if !ok {
		if spec.MissingAttributeEqualsNoResults {
			return nil, nil
		}
		return nil, fmt.Errorf("the row selector %q matched nothing in the answer", selector)
	}

	var items []any
	switch v := target.(type) {
	case []any:
		items = v
	case map[string]any:
		items = []any{v}
	}
	rows := make([]resultRow, 0, len(items))
	for _, item := range items {
		// The filters after the path keep only the elements they hold
		// for, before the rows block shapes what is left.
		if filters != "" {
			if _, ok := jsonSelect(item, ":"+filters); !ok {
				continue
			}
		}
		rows = append(rows, splitJSONRow(item, spec)...)
	}
	return rows, nil
}

// splitJSONRow turns one element of the rows array into the rows its
// fields are read from: "rows.attribute" narrows the element to an object
// inside it, and "multiple" makes each entry of that object or array a row
// of its own, as a movie's several torrents each are. An element lacking
// the attribute yields no row.
func splitJSONRow(item any, spec Rows) []resultRow {
	selected := item
	if spec.Attribute != "" {
		v, ok := jsonLookup(item, spec.Attribute)
		if !ok || v == nil {
			return nil
		}
		selected = v
	}
	if !spec.Multiple {
		return []resultRow{jsonRow{parent: item, value: selected}}
	}

	var entries []any
	switch v := selected.(type) {
	case []any:
		entries = v
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			entries = append(entries, v[key])
		}
	}
	rows := make([]resultRow, 0, len(entries))
	for _, entry := range entries {
		if _, ok := entry.(map[string]any); ok {
			rows = append(rows, jsonRow{parent: item, value: entry})
		}
	}
	return rows
}

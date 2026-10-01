// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"encoding/json"
	"fmt"
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
	lookup(field Field) (value string, matched bool)
	// matches reports whether a selector finds anything in the row, for
	// resolving a field's "case" arms.
	matches(selector string) bool
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

func (r htmlRow) matches(selector string) bool {
	if selector == "*" || selector == "" {
		return true
	}
	return r.sel.Find(selector).Length() > 0 || r.sel.Is(selector)
}

func (r htmlRow) lookup(field Field) (string, bool) {
	sel := r.sel
	if field.Selector != "" {
		sel = r.sel.Find(field.Selector)
		if sel.Length() == 0 {
			return "", false
		}
	}

	if field.Remove != "" {
		// Clone first: the row is reused by later fields, which must still
		// see the elements this field strips out of its own value.
		sel = sel.Clone()
		sel.Find(field.Remove).Remove()
	}

	if field.Attribute != "" {
		value, ok := sel.Attr(field.Attribute)
		return value, ok
	}
	return sel.Text(), true
}

// jsonRow is a result row taken from a JSON response.
type jsonRow struct {
	value any
}

// debugString is the row re-encoded as JSON, the form its key paths address.
func (r jsonRow) debugString() string {
	encoded, err := json.Marshal(r.value)
	if err != nil {
		return fmt.Sprintf("%v", r.value)
	}
	return string(encoded)
}

func (r jsonRow) matches(selector string) bool {
	if selector == "*" || selector == "" {
		return true
	}
	v, ok := jsonLookup(r.value, selector)
	return ok && jsonScalar(v) != ""
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

	v, ok := jsonLookup(r.value, selector)
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
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsonRows resolves a definition's row selector against a decoded JSON
// document and returns each element of the resulting array. A selector
// addressing a single object yields that one row.
func jsonRows(document any, selector string) []resultRow {
	target, ok := jsonLookup(document, selector)
	if !ok {
		return nil
	}

	switch v := target.(type) {
	case []any:
		rows := make([]resultRow, 0, len(v))
		for _, item := range v {
			rows = append(rows, jsonRow{value: item})
		}
		return rows
	case map[string]any:
		return []resultRow{jsonRow{value: v}}
	default:
		return nil
	}
}

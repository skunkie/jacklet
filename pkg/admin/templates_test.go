// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// recessOrder is the property order of stylelint-config-recess-order 7.8.0,
// the one Bootstrap enforces, cut down to the properties the panel uses. A
// property the panel starts using is inserted at its position in that
// package's groups.js.
var recessOrder = []string{
	"box-sizing",
	"display",
	"flex",
	"flex-wrap",
	"gap",
	"align-items",
	"justify-content",
	"width",
	"min-width",
	"max-width",
	"height",
	"padding",
	"padding-bottom",
	"margin",
	"margin-top",
	"margin-bottom",
	"margin-left",
	"overflow-x",
	"font",
	"font-family",
	"font-size",
	"font-weight",
	"font-variant-numeric",
	"-webkit-font-smoothing",
	"line-height",
	"vertical-align",
	"color",
	"text-align",
	"letter-spacing",
	"word-break",
	"white-space",
	"text-decoration",
	"text-underline-offset",
	"cursor",
	"outline",
	"border-collapse",
	"background",
	"border",
	"border-color",
	"border-bottom",
	"border-radius",
}

var (
	templateAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	cssComment     = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssBlock       = regexp.MustCompile(`([^{}]*)\{([^{}]*)\}`)
)

// attributeGroup places an attribute among Code Guide's groups: class;
// id, name; data-*; src, for, type, href, value; title, alt; role, aria-*;
// tabindex; style. Code Guide orders the groups and not the attributes
// within one, and files every attribute it does not name among the
// "miscellaneous attributes unique to specific elements", the src-to-value
// group, which is why its own examples write <link rel="stylesheet" href>.
func attributeGroup(name string) int {
	switch {
	case strings.HasPrefix(name, "data-"):
		return 2
	case strings.HasPrefix(name, "aria-"):
		return 5
	}
	switch name {
	case "class":
		return 0
	case "id", "name":
		return 1
	case "title", "alt":
		return 4
	case "role":
		return 5
	case "tabindex":
		return 6
	case "style":
		return 7
	}
	return 3
}

func compareAttributes(a, b string) int {
	return attributeGroup(a) - attributeGroup(b)
}

// splitDeclarations splits a declaration block at the semicolons that end a
// declaration, leaving those inside a quoted string or a function such as
// url("data:image/svg+xml;base64,...") as part of the value.
func splitDeclarations(block string) []string {
	var declarations []string
	var quote rune
	depth, start := 0, 0
	for i, r := range block {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ';' && depth == 0:
			declarations = append(declarations, block[start:i])
			start = i + 1
		}
	}
	return append(declarations, block[start:])
}

// checkDeclarations reports a declaration block whose properties are out of
// RECESS order or include one recessOrder does not list. Custom properties
// are variables rather than styling and keep whatever order reads best.
func checkDeclarations(t *testing.T, where, block string) {
	t.Helper()
	var properties []string
	for _, declaration := range splitDeclarations(block) {
		property, _, found := strings.Cut(declaration, ":")
		property = strings.TrimSpace(property)
		if !found || strings.HasPrefix(property, "--") {
			continue
		}
		if !assert.Contains(t, recessOrder, property, "%s: %s is missing from recessOrder; add it at its position in stylelint-config-recess-order", where, property) {
			return
		}
		properties = append(properties, property)
	}
	assert.True(t, slices.IsSortedFunc(properties, func(a, b string) int {
		return slices.Index(recessOrder, a) - slices.Index(recessOrder, b)
	}), "%s: properties are out of RECESS order: %s", where, strings.Join(properties, ", "))
}

// The panel's templates follow Bootstrap's conventions: Code Guide for the
// order of a tag's attributes and RECESS for the order of CSS properties,
// in the stylesheet and in inline styles alike. Every template is walked
// and every violation reported in one run, which is why this uses assert.
func TestTemplatesFollowBootstrapOrdering(t *testing.T) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	require.NoError(t, err)
	require.NotEmpty(t, files, "no templates were embedded")

	for _, file := range files {
		source, err := fs.ReadFile(templateFS, file)
		require.NoError(t, err)

		// Actions are removed rather than evaluated: a conditional
		// attribute such as {{if .Selected}} selected{{end}} is still
		// checked where it sits, and a value is irrelevant to the order.
		tokenizer := html.NewTokenizer(strings.NewReader(templateAction.ReplaceAllString(string(source), "")))
		inStyle := false
		for {
			tokenType := tokenizer.Next()
			if tokenType == html.ErrorToken {
				break
			}
			token := tokenizer.Token()
			switch tokenType {
			case html.StartTagToken, html.SelfClosingTagToken:
				inStyle = token.Data == "style"
				var names []string
				for _, attribute := range token.Attr {
					names = append(names, attribute.Key)
					if attribute.Key == "style" {
						checkDeclarations(t, file+" <"+token.Data+" style>", attribute.Val)
					}
				}
				assert.True(t, slices.IsSortedFunc(names, compareAttributes),
					"%s: <%s> attributes are out of Code Guide order: %s", file, token.Data, strings.Join(names, " "))
			case html.TextToken:
				if !inStyle {
					continue
				}
				for _, match := range cssBlock.FindAllStringSubmatch(cssComment.ReplaceAllString(token.Data, ""), -1) {
					checkDeclarations(t, file+" "+strings.TrimSpace(match[1]), match[2])
				}
			case html.EndTagToken:
				inStyle = false
			}
		}
	}
}

func TestCompareAttributes(t *testing.T) {
	groups := [][]string{
		{"class"},
		{"id", "name"},
		{"data-target"},
		{"src", "for", "type", "href", "value", "action", "method", "rel"},
		{"title", "alt"},
		{"role", "aria-hidden", "aria-label"},
		{"tabindex"},
		{"style"},
	}
	for i, group := range groups {
		for _, a := range group {
			for _, b := range group {
				require.Zero(t, compareAttributes(a, b), "%s and %s share a group, which Code Guide leaves unordered", a, b)
			}
			for _, later := range groups[i+1:] {
				for _, b := range later {
					require.Negative(t, compareAttributes(a, b), "%s should precede %s", a, b)
					require.Positive(t, compareAttributes(b, a), "%s should follow %s", b, a)
				}
			}
		}
	}
}

func TestSplitDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  []string
	}{
		{name: "plain", block: "color: red; margin: 0", want: []string{"color: red", " margin: 0"}},
		{name: "trailing semicolon", block: "color: red;", want: []string{"color: red", ""}},
		{name: "inside a function", block: `background: url(data:image/png;base64,AA); color: red`, want: []string{`background: url(data:image/png;base64,AA)`, " color: red"}},
		{name: "inside quotes", block: `content: "a;b"; color: red`, want: []string{`content: "a;b"`, " color: red"}},
		{name: "quote inside a function", block: `background: url("x;y.svg"); color: red`, want: []string{`background: url("x;y.svg")`, " color: red"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, splitDeclarations(tc.block))
		})
	}
}

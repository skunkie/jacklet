// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

// unquotedAttributeRe matches an attribute selector whose value is
// written without quotes, such as [border=1].
var unquotedAttributeRe = regexp.MustCompile(`\[\s*([^\s~|^$*!=\]]+)\s*([~|^$*]?=)\s*([^\s"'\]]+)\s*\]`)

// unquotedContainsRe matches a :contains whose argument is written without
// quotes, such as :contains(Only Upload).
var unquotedContainsRe = regexp.MustCompile(`:contains\(\s*([^"'()]+?)\s*\)`)

// scopeAttribute marks the element a selector's ":scope" stands for while
// the selector runs (see findIn).
const scopeAttribute = "data-jacklet-scope"

// cssSelector adapts a definition's CSS selector to cascadia, the engine
// goquery runs, which is stricter than AngleSharp, the one Jackett's
// definitions are written and tested against: an attribute value and a
// :contains argument AngleSharp accepts unquoted, such as [border=1] and
// :contains(Only Upload), are quoted, as cascadia requires. A selector
// cascadia rejects matches nothing, so without this a definition's row
// selector written that way finds no row at all.
func cssSelector(selector string) string {
	selector = unquotedAttributeRe.ReplaceAllString(selector, `[$1$2"$3"]`)
	return unquotedContainsRe.ReplaceAllString(selector, `:contains("$1")`)
}

// findIn returns the elements below scope that selector matches, as
// goquery's Find does, with the selector adapted by cssSelector and its
// ":scope" standing for scope itself, as it does in the QuerySelector
// Jackett runs from a row, or for the root element when scope is the whole
// document, as in a document's QuerySelectorAll. cascadia has no
// ":scope", so that element is marked with an attribute for the duration
// of the search and the selector names that attribute instead.
func findIn(scope *goquery.Selection, selector string) *goquery.Selection {
	selector = cssSelector(selector)
	if !strings.Contains(selector, ":scope") {
		return scope.FindMatcher(compileSelector(selector))
	}
	marked := scope
	if len(scope.Nodes) == 1 && scope.Nodes[0].Type == html.DocumentNode {
		marked = scope.ChildrenFiltered("*")
	}
	marked.SetAttr(scopeAttribute, "")
	defer marked.RemoveAttr(scopeAttribute)
	return scope.FindMatcher(compileSelector(strings.ReplaceAll(selector, ":scope", "["+scopeAttribute+"]")))
}

// isMatch reports whether sel itself matches selector, adapted as findIn
// adapts it, with ":scope" standing for sel.
func isMatch(sel *goquery.Selection, selector string) bool {
	selector = cssSelector(selector)
	if !strings.Contains(selector, ":scope") {
		return sel.IsMatcher(compileSelector(selector))
	}
	sel.SetAttr(scopeAttribute, "")
	defer sel.RemoveAttr(scopeAttribute)
	return sel.IsMatcher(compileSelector(strings.ReplaceAll(selector, ":scope", "["+scopeAttribute+"]")))
}

// notMatching returns the elements of sel that selector, adapted as
// findIn adapts it, does not match.
func notMatching(sel *goquery.Selection, selector string) *goquery.Selection {
	return sel.NotMatcher(compileSelector(cssSelector(selector)))
}

// hasAttribute marks the element a relative :has is tested against while
// its argument runs (see relativeHas).
const hasAttribute = "data-jacklet-has"

// compileSelector compiles a selector cssSelector has adapted, as
// goquery's Find would, and also supports what cascadia cannot compile
// itself: an alternative of a group that ends in a relative :has, one
// whose argument starts with a combinator, such as
// "div.post:has(~ div.entry a.download)". AngleSharp, which Jackett's
// definitions are written against, accepts that shape. A group is matched
// as one, so what it selects stays in document order whichever
// alternative selects it. A selector it still cannot compile matches
// nothing, as goquery's would.
func compileSelector(selector string) cascadia.Selector {
	if compiled, err := cascadia.Compile(selector); err == nil {
		return compiled
	}
	alternatives := splitSelectorGroup(selector)
	matchers := make([]cascadia.Selector, len(alternatives))
	for i, alternative := range alternatives {
		compiled, ok := compileAlternative(alternative)
		if !ok {
			return func(*html.Node) bool { return false }
		}
		matchers[i] = compiled
	}
	return func(n *html.Node) bool {
		for _, matcher := range matchers {
			if matcher(n) {
				return true
			}
		}
		return false
	}
}

// compileAlternative compiles one selector of a group, reporting false
// when neither cascadia nor relativeHas can.
func compileAlternative(alternative string) (cascadia.Selector, bool) {
	if compiled, err := cascadia.Compile(alternative); err == nil {
		return compiled, true
	}
	base, combinator, relative, ok := trailingRelativeHas(alternative)
	if !ok {
		return nil, false
	}
	// A :has with nothing before it, or only a combinator, applies to any
	// element, as a pseudo-class without a type selector does.
	if base == "" || strings.ContainsRune(" >+~", rune(base[len(base)-1])) {
		base += "*"
	}
	compiledBase, err := cascadia.Compile(base)
	if err != nil {
		return nil, false
	}
	compiledRelative, err := cascadia.Compile("[" + hasAttribute + "] " + combinator + " " + relative)
	if err != nil {
		return nil, false
	}
	return relativeHas(compiledBase, combinator, compiledRelative), true
}

// relativeHas matches an element that base matches and from which
// relative, a selector starting at the element marked with hasAttribute,
// reaches an element: the element is marked while relative is tried,
// below the element for ">" and below its parent for a sibling
// combinator, where the elements relative can reach are.
func relativeHas(base cascadia.Selector, combinator string, relative cascadia.Selector) cascadia.Selector {
	return func(n *html.Node) bool {
		if !base(n) {
			return false
		}
		searched := n
		if combinator != ">" && n.Parent != nil {
			searched = n.Parent
		}
		n.Attr = append(n.Attr, html.Attribute{Key: hasAttribute})
		defer func() { n.Attr = n.Attr[:len(n.Attr)-1] }()
		return relative.MatchFirst(searched) != nil
	}
}

// trailingRelativeHas splits an alternative ending in a relative :has into
// what precedes the :has, the combinator its argument starts with, and the
// rest of its argument, reporting false for any other alternative.
func trailingRelativeHas(alternative string) (base, combinator, relative string, ok bool) {
	alternative = strings.TrimSpace(alternative)
	open := -1
	for i, depth := 0, 0; i < len(alternative); i = skipSelectorString(alternative, i) + 1 {
		switch alternative[i] {
		case '(', '[':
			if depth == 0 && strings.HasSuffix(alternative[:i], ":has") {
				open = i
			}
			depth++
		case ')', ']':
			depth--
			if depth == 0 && i == len(alternative)-1 && open >= 0 {
				argument := strings.TrimSpace(alternative[open+1 : i])
				if argument != "" && strings.ContainsRune("~+>", rune(argument[0])) {
					return alternative[:open-len(":has")], argument[:1], strings.TrimSpace(argument[1:]), true
				}
			}
			if depth == 0 {
				open = -1
			}
		}
	}
	return "", "", "", false
}

// splitSelectorGroup splits a selector group at its top-level commas, the
// ones outside brackets, parentheses and quotes.
func splitSelectorGroup(selector string) []string {
	var alternatives []string
	start, depth := 0, 0
	for i := 0; i < len(selector); i = skipSelectorString(selector, i) + 1 {
		switch selector[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				alternatives = append(alternatives, selector[start:i])
				start = i + 1
			}
		}
	}
	return append(alternatives, selector[start:])
}

// skipSelectorString returns the index of the quote closing the string
// that opens at i, honoring backslash escapes, or i itself when no string
// opens there.
func skipSelectorString(selector string, i int) int {
	quote := selector[i]
	if quote != '"' && quote != '\'' {
		return i
	}
	for j := i + 1; j < len(selector); j++ {
		switch selector[j] {
		case '\\':
			j++
		case quote:
			return j
		}
	}
	return len(selector) - 1
}

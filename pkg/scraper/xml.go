// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
)

// This file is the third response format a definition can declare, beside
// HTML and JSON: an XML document — in practice always an RSS feed —
// addressed by the same CSS selectors a definition writes for HTML.
//
// It cannot go through goquery, which parses HTML. An HTML parser
// lowercases element names, so a definition's "pubDate" would never match
// <pubDate>; and <link> is a void element in HTML, so the text of an RSS
// <link>https://...</link> ends up outside the element entirely. Both are
// exactly what these definitions select on. Jackett reaches for a separate
// XML parser here for the same reason.
//
// The selector support is deliberately the subset those definitions use:
// element names, attribute predicates, and the descendant and child
// combinators. Anything beyond that matches nothing rather than pretending.

// xmlNode is one element of a parsed XML document.
type xmlNode struct {
	// Attrs is keyed by an attribute's local name, so a namespaced
	// attribute is reachable without the definition spelling the prefix.
	Attrs map[string]string
	// Chardata is the text written directly inside this element, not
	// counting its children's.
	Chardata string
	Children []*xmlNode
	// Name is the element's local name, without any namespace prefix.
	Name string
}

// text is the element's text content, its own and every descendant's,
// which is what a field with no attribute yields. It matches what
// goquery's Text does for the HTML case.
func (n *xmlNode) text(excluded map[*xmlNode]bool) string {
	if excluded[n] {
		return ""
	}
	var sb strings.Builder
	var walk func(*xmlNode)
	walk = func(node *xmlNode) {
		if excluded[node] {
			return
		}
		sb.WriteString(node.Chardata)
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(n)
	return sb.String()
}

// parseXML reads a document into a tree of elements.
//
// The reader is given a pass-through CharsetReader because the body has
// already been decoded to UTF-8 by fetch, using the definition's declared
// encoding. Without it a feed whose declaration still says windows-1251 —
// truthfully, about the bytes that were on the wire — would be refused
// outright for an encoding the decoder has no table for.
func parseXML(body string) (*xmlNode, error) {
	decoder := xml.NewDecoder(strings.NewReader(body))
	decoder.Strict = false
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	root := &xmlNode{Name: ""}
	stack := []*xmlNode{root}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		switch t := token.(type) {
		case xml.StartElement:
			node := &xmlNode{Attrs: make(map[string]string, len(t.Attr)), Name: t.Name.Local}
			for _, attr := range t.Attr {
				node.Attrs[attr.Name.Local] = attr.Value
			}
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, node)
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			node := stack[len(stack)-1]
			node.Chardata += string(t)
		}
	}
	return root, nil
}

// xmlAttrPredicate is one "[name]" or "[name=value]" test.
type xmlAttrPredicate struct {
	// HasValue distinguishes "[name]", which only requires the attribute
	// to be present, from "[name=value]".
	HasValue bool
	Name     string
	Value    string
}

// xmlStep is one compound selector in a chain, plus how it joins the one
// before it.
type xmlStep struct {
	Attrs []xmlAttrPredicate
	// IsChild is true when a ">" separated this step from the previous one,
	// restricting it to direct children rather than any descendant.
	IsChild bool
	// Name is the element name to match; empty or "*" matches any.
	Name string
}

// parseXMLSelector splits a selector such as `rss > channel > item[name=x]`
// into its steps. An unparseable selector yields no steps, which then
// matches nothing.
func parseXMLSelector(selector string) []xmlStep {
	fields := strings.Fields(strings.ReplaceAll(selector, ">", " > "))

	var steps []xmlStep
	child := false
	for _, field := range fields {
		if field == ">" {
			child = true
			continue
		}
		step, ok := parseXMLStep(field)
		if !ok {
			return nil
		}
		step.IsChild = child
		child = false
		steps = append(steps, step)
	}
	return steps
}

// parseXMLStep reads one compound selector: an optional element name
// followed by any number of attribute predicates.
func parseXMLStep(s string) (xmlStep, bool) {
	var step xmlStep

	open := strings.IndexByte(s, '[')
	if open < 0 {
		step.Name = s
		return step, s != ""
	}

	step.Name = s[:open]
	for rest := s[open:]; rest != ""; {
		if rest[0] != '[' {
			return xmlStep{}, false
		}
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return xmlStep{}, false
		}

		predicate := rest[1:end]
		rest = rest[end+1:]

		name, value, hasValue := strings.Cut(predicate, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return xmlStep{}, false
		}
		// A definition may quote the value either way, or not at all.
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		step.Attrs = append(step.Attrs, xmlAttrPredicate{HasValue: hasValue, Name: name, Value: value})
	}
	return step, true
}

// matchesStep reports whether one element satisfies a compound selector.
// Element and attribute names are compared case-sensitively, as they are
// in XML — which is the whole reason this is not an HTML parser.
func (n *xmlNode) matchesStep(step xmlStep) bool {
	if step.Name != "" && step.Name != "*" && step.Name != n.Name {
		return false
	}
	for _, predicate := range step.Attrs {
		value, ok := n.Attrs[predicate.Name]
		if !ok {
			return false
		}
		if predicate.HasValue && value != predicate.Value {
			return false
		}
	}
	return true
}

// selectXML resolves a selector against a set of context elements,
// returning the matches in document order without duplicates.
func selectXML(context []*xmlNode, selector string) []*xmlNode {
	steps := parseXMLSelector(selector)
	if len(steps) == 0 {
		return nil
	}

	current := context
	for i, step := range steps {
		// The first step is always relative to the context, like a CSS
		// selector run against a subtree.
		candidates := descendants(current)
		if step.IsChild && i > 0 {
			candidates = children(current)
		}

		var next []*xmlNode
		for _, node := range candidates {
			if node.matchesStep(step) {
				next = append(next, node)
			}
		}
		current = dedupe(next)
	}
	return current
}

func children(nodes []*xmlNode) []*xmlNode {
	var out []*xmlNode
	for _, node := range nodes {
		out = append(out, node.Children...)
	}
	return out
}

func descendants(nodes []*xmlNode) []*xmlNode {
	var out []*xmlNode
	var walk func(*xmlNode)
	walk = func(node *xmlNode) {
		for _, child := range node.Children {
			out = append(out, child)
			walk(child)
		}
	}
	for _, node := range nodes {
		walk(node)
	}
	return dedupe(out)
}

// dedupe keeps the first occurrence of each element. Two context elements
// can share descendants when one contains the other, and a field must not
// then read the same element twice.
func dedupe(nodes []*xmlNode) []*xmlNode {
	seen := make(map[*xmlNode]bool, len(nodes))
	out := nodes[:0:0]
	for _, node := range nodes {
		if !seen[node] {
			seen[node] = true
			out = append(out, node)
		}
	}
	return out
}

// xmlRow is a result row selected from an XML document.
type xmlRow struct {
	node *xmlNode
}

// debugString renders the row back to XML. The parser keeps structure
// rather than the source bytes, so this is a faithful rendering of what
// was parsed rather than the exact text served -- which is what a
// definition's selectors address in any case. Attributes are sorted so a
// dump is stable between runs.
func (r xmlRow) debugString() string {
	var b strings.Builder
	r.node.writeXML(&b)
	return b.String()
}

func (n *xmlNode) writeXML(b *strings.Builder) {
	b.WriteByte('<')
	b.WriteString(n.Name)
	for _, name := range slices.Sorted(maps.Keys(n.Attrs)) {
		b.WriteByte(' ')
		b.WriteString(name)
		b.WriteString(`="`)
		writeXMLEscaped(b, n.Attrs[name])
		b.WriteByte('"')
	}
	b.WriteByte('>')
	writeXMLEscaped(b, n.Chardata)
	for _, child := range n.Children {
		child.writeXML(b)
	}
	b.WriteString("</")
	b.WriteString(n.Name)
	b.WriteByte('>')
}

// writeXMLEscaped writes text as XML content. Escaping matters most
// exactly where strdump earns its keep: a row whose text or attributes
// carry "&" or "<" is one whose selectors are worth inspecting, and
// writing those through raw would produce markup that is not the document
// the definition is being written against.
func writeXMLEscaped(b *strings.Builder, text string) {
	// strings.Builder never fails to write.
	_ = xml.EscapeText(b, []byte(text))
}

func (r xmlRow) matches(selector string) bool {
	if selector == "*" || selector == "" {
		return true
	}
	if len(selectXML([]*xmlNode{r.node}, selector)) > 0 {
		return true
	}
	// As with the HTML row, a selector naming the row itself counts. Only
	// a single compound selector is tested that way, since an ancestor
	// chain says nothing about a row read on its own.
	steps := parseXMLSelector(selector)
	return len(steps) == 1 && r.node.matchesStep(steps[0])
}

func (r xmlRow) lookup(field Field) (string, bool) {
	nodes := []*xmlNode{r.node}
	if field.Selector != "" {
		nodes = selectXML(nodes, field.Selector)
		if len(nodes) == 0 {
			return "", false
		}
	}

	if field.Attribute != "" {
		// The first match supplies the attribute, as goquery's Attr does.
		value, ok := nodes[0].Attrs[field.Attribute]
		return value, ok
	}

	excluded := map[*xmlNode]bool{}
	for _, node := range selectXML(nodes, field.Remove) {
		excluded[node] = true
	}

	var sb strings.Builder
	for _, node := range nodes {
		sb.WriteString(node.text(excluded))
	}
	return sb.String(), true
}

// xmlRows resolves a definition's row selector against a parsed document.
func xmlRows(root *xmlNode, selector string) []resultRow {
	matched := selectXML([]*xmlNode{root}, selector)
	rows := make([]resultRow, 0, len(matched))
	for _, node := range matched {
		rows = append(rows, xmlRow{node: node})
	}
	return rows
}

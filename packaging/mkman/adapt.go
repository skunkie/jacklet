// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// page is one docs/ file installed as a manual page. Summary is the line
// apropos(1) and whatis(1) read from NAME.
type page struct {
	Slug    string
	Summary string
}

// pages is ordered rather than a map so that SEE ALSO reads the same way
// on every run, and so that the reading order of the documentation is the
// order a reader is pointed through it.
var pages = []page{
	{Slug: "configuration", Summary: "flags, environment variables and per-indexer settings"},
	{Slug: "definitions", Summary: "adding a Cardigann indexer definition to Jacklet"},
	{Slug: "admin", Summary: "the Jacklet web administration panel"},
	{Slug: "internals", Summary: "how Jacklet scrapes, stores and serves a search"},
}

// skipped names the docs/ pages that are deliberately not installed.
// development.md documents building from source, which is not something
// the installed package can do; windows.md documents an installer that
// the packages carrying these pages are the alternative to.
var skipped = []string{"development", "windows"}

var (
	spdxComment = regexp.MustCompile(`(?s)\A<!--.*?-->\n+`)
	backlink    = regexp.MustCompile(`(?m)^\[← Documentation index\]\([^)]*\)\n+`)
	firstTitle  = regexp.MustCompile(`(?s)\A# .+?\n+`)
	docLink     = regexp.MustCompile(`\[([^\]]*)\]\((\w+)\.md\)`)
	// Whatever rewriting leaves behind: an anchor, a path, a page that is
	// not installed. Each would reach the reader as a dangling link.
	strayLink = regexp.MustCompile(`\]\(([^)]*\.md[^)]*)\)`)
	heading   = regexp.MustCompile(`^## (.+)$`)
	// The start or end of a fenced block. CommonMark allows either
	// delimiter, three or more of it, and up to three spaces of indent.
	fenceMarker = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	tableRow    = regexp.MustCompile(`^\|(.+)\|\s*$`)
	tableRule   = regexp.MustCompile(`^\|[\s:|-]+\|\s*$`)
)

// manDate formats the date for a .TH line.
func manDate(when time.Time) string {
	return when.UTC().Format("January 2006")
}

// isPage reports whether slug is installed as a manual page.
func isPage(slug string) bool {
	for _, p := range pages {
		if p.Slug == slug {
			return true
		}
	}
	return false
}

// relink rewrites links between docs/ pages as manual page references, and
// reports any link it could not.
func relink(text string) (string, error) {
	return outsideFences(text, relinkSegment)
}

// relinkSegment rewrites the links in one stretch of prose.
func relinkSegment(text string) (string, error) {
	text = docLink.ReplaceAllStringFunc(text, func(match string) string {
		parts := docLink.FindStringSubmatch(match)
		label, target := parts[1], parts[2]
		if isPage(target) {
			return fmt.Sprintf("jacklet-%s(7)", target)
		}
		// A page that is not installed: keep the words, drop the link.
		return strings.Join(strings.Fields(label), " ")
	})

	if stray := strayLink.FindAllStringSubmatch(text, -1); stray != nil {
		targets := make([]string, 0, len(stray))
		for _, match := range stray {
			targets = append(targets, match[1])
		}
		return "", fmt.Errorf(
			"links to %s cannot be rewritten as manual page references; "+
				"link to a whole page, without an anchor, or add the page to pages in adapt.go",
			strings.Join(targets, ", "))
	}

	return text, nil
}

// outsideFences applies transform to the stretches of text that are not
// inside a fenced block. A fence holds what the author typed -- a command,
// a definition, a table drawn as an example -- so a table, a link or a
// heading within one is content, not markup to adapt, and rewriting it
// would both corrupt the example and, for a link the adaptation cannot
// convert, fail the build over text no reader would follow.
//
// Each stretch is transformed whole rather than line by line, because a
// link's label wraps across lines.
func outsideFences(text string, transform func(string) (string, error)) (string, error) {
	var out, segment []string

	flush := func() error {
		if len(segment) == 0 {
			return nil
		}
		converted, err := transform(strings.Join(segment, "\n"))
		if err != nil {
			return err
		}
		out = append(out, strings.Split(converted, "\n")...)
		segment = segment[:0]
		return nil
	}

	// The delimiter a block was opened with, and the length of its run,
	// since only the same delimiter, at least as long and with nothing
	// after it, closes that block. An empty fence means prose.
	fence, fenceLen := "", 0

	for line := range strings.SplitSeq(text, "\n") {
		marker := fenceMarker.FindStringSubmatch(line)

		switch {
		case fence == "" && marker != nil:
			if err := flush(); err != nil {
				return "", err
			}
			out = append(out, line)
			fence, fenceLen = marker[1][:1], len(marker[1])
		case fence != "":
			if marker != nil {
				run := marker[1]
				if strings.HasPrefix(run, fence) && len(run) >= fenceLen &&
					strings.TrimSpace(marker[2]) == "" {
					fence, fenceLen = "", 0
				}
			}
			out = append(out, line)
		default:
			segment = append(segment, line)
		}
	}
	if err := flush(); err != nil {
		return "", err
	}

	return strings.Join(out, "\n"), nil
}

// upperHeadings uppercases the section headings, as a manual page spells
// them.
func upperHeadings(text string) string {
	// The transform cannot fail, so neither can this.
	out, _ := outsideFences(text, func(segment string) (string, error) {
		lines := strings.Split(segment, "\n")
		for i, line := range lines {
			if heading.MatchString(line) {
				lines[i] = "## " + strings.ToUpper(heading.FindStringSubmatch(line)[1])
			}
		}
		return strings.Join(lines, "\n"), nil
	})
	return out
}

// cells splits one markdown table row.
func cells(line string) []string {
	split := strings.Split(tableRow.FindStringSubmatch(line)[1], "|")
	out := make([]string, 0, len(split))
	for _, cell := range split {
		out = append(out, strings.TrimSpace(cell))
	}
	return out
}

// sentence ends text with a full stop, unless it already closes with
// punctuation of its own.
func sentence(text string) string {
	if strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") ||
		strings.HasSuffix(text, "?") || strings.HasSuffix(text, ":") {
		return text
	}
	return text + "."
}

// tableToList renders one table as a markdown list, one item per row.
func tableToList(header []string, rows [][]string) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		item := "- " + row[0]

		// A single column is the label and nothing else; describing it
		// with itself would say the same thing twice.
		if len(row) > 1 {
			var labelled []string
			for i := 1; i < len(row)-1 && i < len(header)-1; i++ {
				if row[i] != "" {
					labelled = append(labelled, fmt.Sprintf("%s: %s", header[i], row[i]))
				}
			}
			if len(labelled) > 0 {
				item += " (" + strings.Join(labelled, ", ") + ")"
			}
			if described := row[len(row)-1]; described != "" {
				item += " — " + sentence(described)
			}
		}

		out = append(out, item)
	}
	return out
}

// tablesToLists replaces every markdown table with an equivalent list. A
// four column table does not fit a manual page: tbl sizes each column to
// its widest unbreakable word, and the identifiers and flags in these
// tables exceed the width on their own, whatever share of the line they
// are given. A manual page presents that kind of material as a list.
func tablesToLists(text string) string {
	// The transform cannot fail, so neither can this.
	out, _ := outsideFences(text, func(segment string) (string, error) {
		return tablesInSegment(segment), nil
	})
	return out
}

// tablesInSegment replaces the tables in one stretch of prose.
func tablesInSegment(text string) string {
	var out, block []string

	flush := func() {
		// A table is a header, a rule, and its rows; anything else is
		// left as it was found.
		if len(block) > 2 && tableRule.MatchString(block[1]) {
			rows := make([][]string, 0, len(block)-2)
			for _, row := range block[2:] {
				rows = append(rows, cells(row))
			}
			out = append(out, tableToList(cells(block[0]), rows)...)
		} else {
			out = append(out, block...)
		}
		block = block[:0]
	}

	for line := range strings.SplitSeq(text, "\n") {
		if tableRow.MatchString(line) {
			block = append(block, line)
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()

	return strings.Join(out, "\n")
}

// section7 adapts one docs/ page into a section 7 manual page source.
func section7(p page, doc, version, date string) (string, error) {
	body := firstTitle.ReplaceAllString(
		backlink.ReplaceAllString(spdxComment.ReplaceAllString(doc, ""), ""), "")

	body, err := relink(body)
	if err != nil {
		return "", fmt.Errorf("%s.md: %w", p.Slug, err)
	}

	var siblings []string
	for _, other := range pages {
		if other.Slug != p.Slug {
			siblings = append(siblings, fmt.Sprintf("jacklet-%s(7)", other.Slug))
		}
	}

	return fmt.Sprintf(
		"# jacklet-%s 7 %q %q %q\n\n"+
			"## NAME\n\njacklet-%s - %s\n\n"+
			"## DESCRIPTION\n\n%s\n\n"+
			"## SEE ALSO\n\njacklet(1), %s\n",
		p.Slug, date, "jacklet "+version, "Miscellaneous Information Manual",
		p.Slug, p.Summary,
		strings.TrimSpace(upperHeadings(body)),
		strings.Join(siblings, ", ")), nil
}

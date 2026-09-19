// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRelinkRewritesLinksBetweenPages(t *testing.T) {
	got, err := relink("see [Configuration](configuration.md) for more")
	require.NoError(t, err)
	require.Equal(t, "see jacklet-configuration(7) for more", got)
}

// The documentation wraps at column 78, so a link's label runs across
// lines and a line-oriented substitution would miss it.
func TestRelinkRewritesALinkWhoseLabelWraps(t *testing.T) {
	got, err := relink("covered in [Adding an\nindexer](definitions.md) instead")
	require.NoError(t, err)
	require.Equal(t, "covered in jacklet-definitions(7) instead", got)
}

func TestRelinkKeepsTheWordsOfAPageThatIsNotInstalled(t *testing.T) {
	got, err := relink("see [Development](development.md)")
	require.NoError(t, err,
		"a page that is deliberately not installed is not an error")
	require.Equal(t, "see Development", got)
}

func TestRelinkRejectsALinkItCannotRewrite(t *testing.T) {
	for _, link := range []string{
		"[Settings](configuration.md#per-indexer-settings)",
		"[Home](../README.md)",
		"[New](whatever.md#anchor)",
	} {
		_, err := relink("text " + link)
		require.Error(t, err, "%s would reach the reader as a dangling link", link)
		require.Contains(t, err.Error(), ".md")
	}
}

func TestUpperHeadingsLeavesFencedBlocksAlone(t *testing.T) {
	got := upperHeadings("## Per-indexer settings\n\n```bash\n## not a heading\n```\n## Next")
	require.Equal(t,
		"## PER-INDEXER SETTINGS\n\n```bash\n## not a heading\n```\n## NEXT", got)
}

func TestTablesToListsRendersEachRowAsAnItem(t *testing.T) {
	table := strings.Join([]string{
		"| Variable | Flag | Default | Purpose |",
		"| -------- | ---- | ------- | ------- |",
		"| `PORT` | `-port` | `9117` | Port to listen on. |",
		"| `API_KEY` | *(none)* | *(unset)* | Required parameter. |",
	}, "\n")

	require.Equal(t,
		"- `PORT` (Flag: `-port`, Default: `9117`) — Port to listen on.\n"+
			"- `API_KEY` (Flag: *(none)*, Default: *(unset)*) — Required parameter.",
		tablesToLists(table))
}

func TestTablesToListsLeavesOtherContentAlone(t *testing.T) {
	text := "A paragraph.\n\n    | not | a table |\n\nAnother."
	require.Equal(t, text, tablesToLists(text))
}

func TestSection7BuildsThePageAroundTheDocument(t *testing.T) {
	doc := "<!--\nSPDX-FileCopyrightText: 2026 TorrPlay\n-->\n\n" +
		"# Admin panel\n\n[← Documentation index](../README.md#documentation)\n\n" +
		"The panel does things.\n\n## Sessions\n\nMore.\n"

	got, err := section7(page{Slug: "admin", Summary: "the panel"}, doc, "1.2.3", "September 2026")
	require.NoError(t, err)

	require.Equal(t, `# jacklet-admin 7 "September 2026" "jacklet 1.2.3" "Miscellaneous Information Manual"`,
		strings.SplitN(got, "\n", 2)[0])
	require.Contains(t, got, "## NAME\n\njacklet-admin - the panel\n",
		"whatis(1) and apropos(1) read the page's description from NAME")
	require.Contains(t, got, "## DESCRIPTION\n\nThe panel does things.")
	require.Contains(t, got, "## SESSIONS")
	require.Contains(t, got, "## SEE ALSO\n\njacklet(1), jacklet-configuration(7)")
	require.NotContains(t, got, "SPDX", "the licence header is not page content")
	require.NotContains(t, got, "Documentation index", "the backlink means nothing once installed")
	require.NotContains(t, got, "# Admin panel", "the title becomes the .TH line")
}

func TestSection7ReportsWhichDocumentHasTheBadLink(t *testing.T) {
	_, err := section7(page{Slug: "admin"}, "# T\n\n[x](configuration.md#anchor)\n", "1.2.3", "d")
	require.ErrorContains(t, err, "admin.md")
}

func TestToRoffASCII(t *testing.T) {
	require.Equal(t, `a \(em b`, toRoffASCII("a — b"))
	require.Equal(t, "a...", toRoffASCII("a…"))
	require.Equal(t, `\[u2190]`, toRoffASCII("←"),
		"an unmapped character still has to leave as an escape, not a raw byte")
	require.Equal(t, "plain", toRoffASCII("plain"))
}

func TestManDate(t *testing.T) {
	require.Equal(t, "September 2026",
		manDate(time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)))
}

// A table inside a fence is an example of a table, not markup to
// adapt; rewriting it would corrupt what the author typed.
func TestTablesToListsLeavesAFencedTableAlone(t *testing.T) {
	fenced := "```\n| a | b |\n| - | - |\n| x | y |\n```"
	require.Equal(t, fenced, tablesToLists(fenced))
}

func TestRelinkLeavesAFencedLinkAlone(t *testing.T) {
	fenced := "```\nsee [Configuration](configuration.md)\n```"
	got, err := relink(fenced)
	require.NoError(t, err)
	require.Equal(t, fenced, got)
}

// No reader would follow it, so it must not fail the build.
func TestRelinkIgnoresAnUnconvertibleLinkInsideAFence(t *testing.T) {
	fenced := "```\n[Settings](configuration.md#per-indexer-settings)\n```"
	got, err := relink(fenced)
	require.NoError(t, err)
	require.Equal(t, fenced, got)
}

func TestTransformsStillApplyAroundAFence(t *testing.T) {
	text := "## Heading\n\n```\n| a | b |\n```\n\nsee [Configuration](configuration.md)"
	got, err := relink(upperHeadings(tablesToLists(text)))
	require.NoError(t, err)
	require.Equal(t,
		"## HEADING\n\n```\n| a | b |\n```\n\nsee jacklet-configuration(7)", got)
}

func TestTableToListKeepsTheCellsOwnPunctuation(t *testing.T) {
	table := "| Term | Note |\n| ---- | ---- |\n| `a` | Why not? |\n| `b` | Plain |"
	require.Equal(t,
		"- `a` — Why not?\n- `b` — Plain.",
		tablesToLists(table))
}

func TestTableToListOfOneColumn(t *testing.T) {
	table := "| Term |\n| ---- |\n| `a` |\n| `b` |"
	require.Equal(t, "- `a`\n- `b`", tablesToLists(table),
		"a lone column is the label; it is not also its own description")
}

func TestOutsideFencesRecognisesEveryFenceSpelling(t *testing.T) {
	table := "| a | b |\n| - | - |\n| x | y |"
	for name, fenced := range map[string]string{
		"backticks":         "```\n" + table + "\n```",
		"tildes":            "~~~\n" + table + "\n~~~",
		"indented":          "   ```\n" + table + "\n   ```",
		"with a language":   "```markdown\n" + table + "\n```",
		"longer than three": "````\n" + table + "\n````",
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, fenced, tablesToLists(fenced))
		})
	}
}

// A tilde line inside a backtick block is content, so the table after
// it is still fenced.
func TestOutsideFencesClosesOnlyOnAMatchingDelimiter(t *testing.T) {
	fenced := "```\n~~~\n| a | b |\n| - | - |\n| x | y |\n```"
	require.Equal(t, fenced, tablesToLists(fenced))
}

// Degrading to "leave it alone" is the safe direction: the alternative
// is rewriting an example the author never closed.
func TestOutsideFencesTreatsAnUnterminatedFenceAsFenced(t *testing.T) {
	fenced := "```\n| a | b |\n| - | - |\n| x | y |"
	require.Equal(t, fenced, tablesToLists(fenced))
}

// Which pages exist is the packages' installed surface, not an
// implementation detail: the release asserts that every page rendered
// is a page installed, and takes the count from the render, so this is
// what makes adding or dropping one a decision with a test behind it
// rather than a change nothing reports.
func TestPagesAreTheOnesTheReleaseInstalls(t *testing.T) {
	slugs := make([]string, 0, len(pages))
	for _, p := range pages {
		slugs = append(slugs, p.Slug)
		require.NotEmpty(t, p.Summary,
			"jacklet-%s(7) would have an empty NAME, which whatis(1) reads", p.Slug)
	}

	require.Equal(t,
		[]string{"configuration", "definitions", "admin", "internals"}, slugs)
}

func TestEveryPageHasADocumentBehindIt(t *testing.T) {
	for _, p := range pages {
		require.FileExists(t, filepath.Join("..", "..", "docs", p.Slug+".md"),
			"jacklet-%s(7) is rendered from a docs/ page that is not there", p.Slug)
	}
	for _, slug := range skipped {
		require.FileExists(t, filepath.Join("..", "..", "docs", slug+".md"),
			"%s.md is skipped but no longer exists; the exemption is stale", slug)
	}
}

// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cpuguy83/go-md2man/v2/md2man"
)

// roffEscapes covers the characters the documentation uses. groff reads a
// byte stream and warns on every byte of a UTF-8 character unless preconv
// ran first, which is not guaranteed wherever a manual page ends up.
var roffEscapes = map[rune]string{
	'—': `\(em`,
	'…': "...",
}

// toRoffASCII replaces non-ASCII characters with escapes groff
// understands, so that a page is plain ASCII however it is read.
func toRoffASCII(roff string) string {
	var out strings.Builder
	for _, r := range roff {
		switch {
		case r < 128:
			out.WriteRune(r)
		case roffEscapes[r] != "":
			out.WriteString(roffEscapes[r])
		default:
			fmt.Fprintf(&out, `\[u%04X]`, r)
		}
	}
	return out.String()
}

// buildDate is the date the pages carry, which SOURCE_DATE_EPOCH pins for
// a reproducible build.
func buildDate() (time.Time, error) {
	epoch := os.Getenv("SOURCE_DATE_EPOCH")
	if epoch == "" {
		return time.Now(), nil
	}
	seconds, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q is not a timestamp: %w", epoch, err)
	}
	return time.Unix(seconds, 0), nil
}

// checkCoverage reports a docs/ page that is neither installed nor
// deliberately left out, so that adding one is a decision rather than a
// silent omission.
func checkCoverage(docsDir string) error {
	entries, err := filepath.Glob(filepath.Join(docsDir, "*.md"))
	if err != nil {
		return err
	}

	var unknown []string
	for _, entry := range entries {
		slug := strings.TrimSuffix(filepath.Base(entry), ".md")
		if !isPage(slug) && !slices.Contains(skipped, slug) {
			unknown = append(unknown, entry)
		}
	}
	if unknown != nil {
		return fmt.Errorf(
			"%s: neither rendered nor skipped; add each to pages or skipped in adapt.go",
			strings.Join(unknown, ", "))
	}
	return nil
}

// writePage renders one markdown source and writes it gzipped, as man(1)
// expects to find it.
func writePage(path, source string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	// A zero ModTime, and no name, so that rebuilding the same version
	// produces the same bytes.
	compressed := gzip.NewWriter(file)
	if _, err := compressed.Write([]byte(toRoffASCII(string(md2man.Render([]byte(source)))))); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	fmt.Println("wrote", path)
	return nil
}

func run() error {
	version := flag.String("version", "", "the release the pages name, as a tag such as v1.2.3")
	docsDir := flag.String("docs-dir", filepath.Join("..", "..", "docs"), "directory of the documentation")
	source := flag.String("source", filepath.Join("..", "jacklet.1.md"), "source of jacklet(1)")
	outDir := flag.String("out-dir", filepath.Join("..", "..", "man"), "directory to write the pages to")
	flag.Parse()

	if *version == "" {
		return errors.New("-version is required")
	}
	// Manual pages name a release without the tag's leading v.
	release := strings.TrimPrefix(*version, "v")

	when, err := buildDate()
	if err != nil {
		return err
	}
	date := manDate(when)

	if err := checkCoverage(*docsDir); err != nil {
		return err
	}

	command, err := os.ReadFile(*source)
	if err != nil {
		return err
	}
	replaced := strings.NewReplacer("@DATE@", date, "@VERSION@", release).Replace(
		spdxComment.ReplaceAllString(string(command), ""))
	if err := writePage(filepath.Join(*outDir, "man1", "jacklet.1.gz"), tablesToLists(replaced)); err != nil {
		return err
	}

	for _, p := range pages {
		doc, err := os.ReadFile(filepath.Join(*docsDir, p.Slug+".md"))
		if err != nil {
			return err
		}
		adapted, err := section7(p, string(doc), release, date)
		if err != nil {
			return err
		}
		out := filepath.Join(*outDir, "man7", fmt.Sprintf("jacklet-%s.7.gz", p.Slug))
		if err := writePage(out, tablesToLists(adapted)); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mkman:", err)
		os.Exit(1)
	}
}

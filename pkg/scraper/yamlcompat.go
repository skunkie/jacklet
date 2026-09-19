// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"gopkg.in/yaml.v3"
)

// unmarshalYAML decodes one of Jacklet's YAML files — a definition, or a
// config override written in the same style — repairing the one thing
// Jackett's YAML library accepts and Go's does not.
//
// YAML 1.2 lists "\/" among the escapes a double-quoted scalar may use, as
// a way of writing a plain slash. gopkg.in/yaml.v3 descends from libyaml,
// which implements YAML 1.1, and rejects it — it is the *only* YAML 1.2
// escape it rejects. Definitions written against Jackett's YamlDotNet use
// it, and every definition in Jackett's repository is expected to parse
// here, so one is unusable for no other reason without this repair.
//
// The repair runs only after a plain decode has already failed, and its
// result is used only if it then succeeds, so a file that parses today
// keeps parsing exactly as it did. A failure is reported as the original
// error rather than the repaired one, since the repair is an
// implementation detail and the first error describes the file the
// operator actually wrote.
func unmarshalYAML(data []byte, out any) error {
	err := yaml.Unmarshal(data, out)
	if err == nil {
		return nil
	}

	repaired, hasChanged := repairSlashEscapes(data)
	if !hasChanged {
		return err
	}
	if yaml.Unmarshal(repaired, out) != nil {
		return err
	}
	return nil
}

// The states yamlScanner moves between. Everything outside a scalar or a
// comment is "plain", which includes plain scalars: nothing there needs
// repairing, only recognising well enough not to lose track.
const (
	yamlPlain = iota
	yamlComment
	yamlSingleQuoted
	yamlDoubleQuoted
)

// yamlScanner walks a document byte by byte, rewriting "\/" to "/" where
// YAML would have read it as an escape.
type yamlScanner struct {
	// hasBlank says whether any blank followed prev; together they say
	// whether a quote stands where a value begins.
	hasBlank bool
	// hasChanged records whether any escape was actually rewritten, so a
	// document with none is left strictly alone.
	hasChanged bool
	out        []byte
	// prev is the last non-blank byte on the line, zero at its start.
	prev  byte
	state int
}

// repairSlashEscapes rewrites "\/" to "/" inside double-quoted scalars,
// and reports whether it changed anything.
func repairSlashEscapes(data []byte) ([]byte, bool) {
	s := &yamlScanner{out: make([]byte, 0, len(data))}
	for i := 0; i < len(data); {
		i += s.step(data, i)
	}
	return s.out, s.hasChanged
}

// step consumes at least one byte at i and returns how many it took.
func (s *yamlScanner) step(data []byte, i int) int {
	switch s.state {
	case yamlComment:
		return s.inComment(data[i])
	case yamlSingleQuoted:
		return s.inSingleQuoted(data, i)
	case yamlDoubleQuoted:
		return s.inDoubleQuoted(data, i)
	default:
		return s.inPlain(data, i)
	}
}

// emit writes one byte through and keeps the line bookkeeping current.
func (s *yamlScanner) emit(c byte) {
	s.out = append(s.out, c)
	switch c {
	case '\n':
		s.prev, s.hasBlank = 0, false
	case ' ', '\t':
		s.hasBlank = true
	default:
		s.prev, s.hasBlank = c, false
	}
}

func (s *yamlScanner) inComment(c byte) int {
	if c == '\n' {
		s.state = yamlPlain
	}
	s.emit(c)
	return 1
}

func (s *yamlScanner) inPlain(data []byte, i int) int {
	c := data[i]
	switch {
	case c == '#' && (s.prev == 0 || s.hasBlank):
		s.state = yamlComment
	case c == '"' && opensScalar(s.prev, s.hasBlank):
		s.state = yamlDoubleQuoted
	case c == '\'' && opensScalar(s.prev, s.hasBlank):
		s.state = yamlSingleQuoted
	}
	s.emit(c)
	return 1
}

func (s *yamlScanner) inSingleQuoted(data []byte, i int) int {
	c := data[i]
	if c != '\'' {
		s.emit(c)
		return 1
	}
	// "''" is how a single-quoted scalar writes one quote.
	if i+1 < len(data) && data[i+1] == '\'' {
		s.emit(c)
		s.emit('\'')
		return 2
	}
	s.state = yamlPlain
	s.emit(c)
	return 1
}

func (s *yamlScanner) inDoubleQuoted(data []byte, i int) int {
	c := data[i]
	if c == '\\' && i+1 < len(data) {
		if data[i+1] == '/' {
			// The escape YAML 1.2 defines and yaml.v3 refuses.
			s.emit('/')
			s.hasChanged = true
			return 2
		}
		// Any other escape passes through whole, so that the second
		// backslash of "\\" cannot be mistaken for the start of one, nor
		// the quote of "\"" for the end of the scalar.
		s.emit(c)
		s.emit(data[i+1])
		return 2
	}
	if c == '"' {
		s.state = yamlPlain
	}
	s.emit(c)
	return 1
}

// opensScalar reports whether a quote opens a quoted scalar, given the
// last non-blank byte before it (zero at the start of a line) and whether
// any blank came between the two.
//
// The distinction matters: ":", "-" and "?" are YAML indicators only when
// a blank follows them, so the closing quote of a *plain* scalar such as
//
//	selector: td a[href*="/torrent-category-"]
//
// comes right after a "-" and must not be read as opening one. Getting
// that wrong leaves every later line of the file inside a scalar that was
// never opened.
func opensScalar(prev byte, hasBlank bool) bool {
	switch prev {
	case 0, '[', '{', ',':
		return true
	case ':', '-', '?':
		return hasBlank
	default:
		return false
	}
}

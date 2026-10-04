// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// redactedTarget names a request's address for an error message, as " for
// <address>", without credentials or the query string. A tracker's query
// string carries the passkey on many sites, and these messages reach logs
// and the admin panel. Saying that it was dropped keeps the remains from
// reading as the whole URL, which makes a refused request look like a
// malformed one. A nil address names nothing.
func redactedTarget(requestURL *url.URL) string {
	if requestURL == nil {
		return ""
	}
	return " for " + redactedURL(requestURL)
}

// redactedURL is an address without credentials, the query string or the
// fragment, and with any path segment that looks like a passkey (see
// redactedTokens) replaced, since many trackers put the passkey in a
// download link's path. It notes what was dropped, as redactedTarget
// explains.
func redactedURL(requestURL *url.URL) string {
	redacted := *requestURL
	redacted.User = nil
	hadQuery := redacted.RawQuery != "" || redacted.ForceQuery
	hadFragment := redacted.Fragment != "" || redacted.RawFragment != ""
	redacted.RawQuery, redacted.ForceQuery = "", false
	redacted.Fragment, redacted.RawFragment = "", ""
	// The path is appended already escaped, so the "…" redactedTokens
	// writes is not escaped in turn.
	path := redacted.EscapedPath()
	redacted.Path, redacted.RawPath = "", ""
	address := redacted.String() + redactedTokens(path)
	if hadQuery {
		address += " (query redacted)"
	}
	if hadFragment {
		address += " (fragment redacted)"
	}
	return address
}

// minTokenLength is the shortest run of letters and digits that redaction
// takes for a passkey where it cannot tell a value from its surroundings:
// in a logged search path's segments and actions (redactedPath), in a bare
// query FlareSolverr may echo (redactError), and in the path of an address
// an error names (redactedURL), whose query and fragment are dropped
// whatever they hold. Nothing else is searched for tokens.
const minTokenLength = 16

// tokenRun matches a run of letters and digits as long as a token can be.
// '-', '_', '+' and '/' end a run: they join a dated path segment or an id
// list into one long run as readily as they join a token, and '/' cannot
// be told from the path's own separator. A passkey written with them is
// caught only where an unbroken piece of it is long enough on its own, and
// its shorter pieces are left.
var tokenRun = regexp.MustCompile(fmt.Sprintf(`[A-Za-z0-9]{%d,}`, minTokenLength))

// wholeToken matches text that is a single tokenRun.
var wholeToken = regexp.MustCompile("^" + tokenRun.String() + "$")

// looksLikeToken reports whether text could be a passkey: a single run of
// at least minTokenLength letters and digits, with both among them.
func looksLikeToken(text string) bool {
	return wholeToken.MatchString(text) && isMixed(text)
}

// isMixed reports whether text holds both a letter and a digit. A run of
// either alone is far more often a word or a number than a passkey.
func isMixed(text string) bool {
	return strings.ContainsAny(text, "0123456789") &&
		strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) >= 0
}

// redactedTokens replaces each run in text that looksLikeToken with "…".
func redactedTokens(text string) string {
	return tokenRun.ReplaceAllStringFunc(text, func(run string) string {
		// run is a tokenRun already, so only the mix is left to check.
		if isMixed(run) {
			return "…"
		}
		return run
	})
}

// redactedPathValue is a search path's template that a log line renders
// through redactedPath only when it is written, so a line below the
// logger's level costs nothing.
type redactedPathValue string

// LogValue renders the path as redactedPath does.
func (path redactedPathValue) LogValue() slog.Value {
	return slog.StringValue(redactedPath(string(path)))
}

// redactedPath is a search path's template for a log line, with each
// literal value in its query string and fragment replaced by "…", and each
// run elsewhere that looks like a passkey (see redactedTokens): a
// definition may write its passkey into the path itself rather than
// reading it from its settings. Template actions, query keys and the
// separators between them are kept, so the line still shows the
// template's shape and every action stays whole.
func redactedPath(path string) string {
	var out strings.Builder
	inQuery, inValue := false, false
	for rest := path; rest != ""; {
		switch {
		case strings.HasPrefix(rest, "{{"):
			end := strings.Index(rest, "}}")
			if end < 0 {
				// An action that never closes is not a template to show.
				out.WriteString("…")
				return out.String()
			}
			out.WriteString(redactedTokens(rest[:end+2]))
			rest = rest[end+2:]
		case strings.IndexByte("?#&=", rest[0]) >= 0:
			switch rest[0] {
			case '?':
				inQuery, inValue = true, inValue || inQuery
			case '#':
				// A fragment is one value, whatever it holds.
				inQuery, inValue = true, true
			case '&':
				inValue = false
			case '=':
				inValue = inQuery
			}
			out.WriteByte(rest[0])
			rest = rest[1:]
		default:
			end := strings.IndexAny(rest, "?#&=")
			if action := strings.Index(rest, "{{"); action >= 0 && (end < 0 || action < end) {
				end = action
			}
			if end < 0 {
				end = len(rest)
			}
			switch {
			case !inQuery:
				out.WriteString(redactedTokens(rest[:end]))
			case inValue:
				out.WriteString("…")
			// A run followed by "=" outside a value is a key, which names a
			// secret only when it is one itself.
			case strings.HasPrefix(rest[end:], "="):
				out.WriteString(redactedTokens(rest[:end]))
			default:
				out.WriteString("…")
			}
			rest = rest[end:]
		}
	}
	return out.String()
}

// redactAddresses strips the query string and fragment from the URL of each
// *url.Error in err's chain, wherever err's message names it. A failed
// request, and a link that does not parse, are reported with the whole URL,
// which on many trackers carries the passkey, and that message reaches
// logs, the panel and a Torznab client.
func redactAddresses(err error) error {
	return redactError(err, nil, nil)
}

// redactError rewrites err's message as redactAddresses does, treating each
// of addresses as one more URL to redact and replacing each of queries, a
// bare query string or form, wherever it appears; a query with no "=" is
// left alone unless it looksLikeToken. The outer messages are
// kept. The result does not unwrap, so the unredacted *url.Error cannot be
// recovered from it with errors.As, but it still answers errors.Is for
// anything in err's chain.
func redactError(err error, addresses, queries []string) error {
	if err == nil {
		return nil
	}
	for _, urlErr := range urlErrors(err) {
		addresses = append(addresses, urlErr.URL)
	}
	type replacement struct{ new, old string }
	var replacements []replacement
	for _, address := range addresses {
		redacted := "(address redacted)"
		if parsed, parseErr := url.Parse(address); parseErr == nil {
			redacted = redactedURL(parsed)
		}
		if address == "" || redacted == address {
			continue
		}
		// A *url.Error quotes its URL, which may escape characters the
		// bare address leaves alone.
		replacements = append(replacements,
			replacement{new: strconv.Quote(redacted), old: strconv.Quote(address)},
			replacement{new: redacted, old: address})
	}
	for _, query := range queries {
		// A query with no "=" is a bare word, such as the keywords
		// themselves, which would match unrelated text in the message,
		// unless it looks like the passkey on its own.
		if strings.Contains(query, "=") || looksLikeToken(query) {
			replacements = append(replacements, replacement{new: "(query redacted)", old: query})
		}
	}
	// Longest first, so an address that begins another is not replaced
	// inside it, which would leave the longer one's query unmatched.
	slices.SortStableFunc(replacements, func(a, b replacement) int {
		return len(b.old) - len(a.old)
	})
	original := err.Error()
	message := original
	for _, r := range replacements {
		message = strings.ReplaceAll(message, r.old, r.new)
	}
	if message == original {
		return err
	}
	return &redactedError{err: err, message: message}
}

// urlErrors lists every *url.Error in err's chain, which errors.As cannot,
// since it stops at the first.
func urlErrors(err error) []*url.Error {
	var found []*url.Error
	var walk func(error)
	walk = func(err error) {
		//nolint:errorlint // The chain is walked by hand, so each link is examined on its own.
		if urlErr, ok := err.(*url.Error); ok {
			found = append(found, urlErr)
		}
		//nolint:errorlint // As above.
		switch wrapper := err.(type) {
		case interface{ Unwrap() error }:
			if inner := wrapper.Unwrap(); inner != nil {
				walk(inner)
			}
		case interface{ Unwrap() []error }:
			for _, inner := range wrapper.Unwrap() {
				if inner != nil {
					walk(inner)
				}
			}
		}
	}
	walk(err)
	return found
}

// redactedError is an error whose message redactError has rewritten. It
// answers errors.Is for its original's chain but does not unwrap to it,
// since that chain still holds the unredacted addresses.
type redactedError struct {
	err     error
	message string
}

func (e *redactedError) Error() string { return e.message }

func (e *redactedError) Is(target error) bool { return errors.Is(e.err, target) }

// redactingHandler redacts the addresses an error attribute names before a
// record reaches the handler it wraps, so a log line written on the way to
// a failure says no more than the error that failure returns.
type redactingHandler struct {
	slog.Handler
}

func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	shouldRewrite := false
	record.Attrs(func(attr slog.Attr) bool {
		shouldRewrite = needsRedaction(attr)
		return !shouldRewrite
	})
	if !shouldRewrite {
		return h.Handler.Handle(ctx, record)
	}
	redacted := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		redacted.AddAttrs(redactAttr(attr))
		return true
	})
	return h.Handler.Handle(ctx, redacted)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		redacted[i] = redactAttr(attr)
	}
	return redactingHandler{h.Handler.WithAttrs(redacted)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{h.Handler.WithGroup(name)}
}

// needsRedaction reports whether attr holds an error, in a group or not, or
// a value not yet resolved, which might. It resolves nothing, so a value is
// resolved only once, by redactAttr.
func needsRedaction(attr slog.Attr) bool {
	switch attr.Value.Kind() {
	case slog.KindLogValuer:
		return true
	case slog.KindGroup:
		return slices.ContainsFunc(attr.Value.Group(), needsRedaction)
	case slog.KindAny:
		_, isError := attr.Value.Any().(error)
		return isError
	}
	return false
}

// redactAttr is attr resolved, with any error it holds, in a group or not,
// passed through redactAddresses.
func redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	switch attr.Value.Kind() {
	case slog.KindGroup:
		group := attr.Value.Group()
		redacted := make([]slog.Attr, len(group))
		for i, member := range group {
			redacted[i] = redactAttr(member)
		}
		attr.Value = slog.GroupValue(redacted...)
	case slog.KindAny:
		if err, isError := attr.Value.Any().(error); isError {
			attr.Value = slog.AnyValue(redactAddresses(err))
		}
	}
	return attr
}

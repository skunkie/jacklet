// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
)

// encodeForm renders form values as an "application/x-www-form-urlencoded"
// string in the character set a definition declares.
//
// A tracker that serves windows-1251 also expects its search terms in
// windows-1251: submitting UTF-8 bytes to one does not fail, it just
// silently matches nothing, because the site reads the query as mojibake
// and falls back to its default listing. url.Values.Encode always produces
// UTF-8, so a non-UTF-8 definition needs the bytes transcoded before they
// are percent-encoded.
func encodeForm(form url.Values, encodingName string) (string, error) {
	enc, transcode, err := formEncoding(encodingName)
	if err != nil {
		return "", err
	}
	if !transcode {
		return form.Encode(), nil
	}

	encoder := enc.NewEncoder()
	// Match url.Values.Encode's stable, sorted output.
	keys := make([]string, 0, len(form))
	for key := range form {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf strings.Builder
	for _, key := range keys {
		encodedKey, err := encoder.Bytes([]byte(key))
		if err != nil {
			return "", fmt.Errorf("encoding form key %q as %s: %w", key, encodingName, err)
		}
		for _, value := range form[key] {
			encodedValue, err := encoder.Bytes([]byte(value))
			if err != nil {
				return "", fmt.Errorf("encoding form value for %q as %s: %w", key, encodingName, err)
			}
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(percentEncode(encodedKey))
			buf.WriteByte('=')
			buf.WriteString(percentEncode(encodedValue))
		}
	}
	return buf.String(), nil
}

// formEncoding resolves a definition's declared encoding. transcode is
// false when the form's UTF-8 bytes can be submitted as they are.
func formEncoding(name string) (enc encoding.Encoding, transcode bool, err error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "utf-8") || strings.EqualFold(name, "utf8") {
		return nil, false, nil
	}

	enc, err = htmlindex.Get(name)
	if err != nil {
		return nil, false, fmt.Errorf("unknown encoding %q: %w", name, err)
	}
	// htmlindex resolves "utf-8" aliases to the UTF-8 encoding, whose
	// encoder is a no-op; skip it rather than transcoding pointlessly.
	if canonical, nameErr := htmlindex.Name(enc); nameErr == nil && strings.EqualFold(canonical, "utf-8") {
		return nil, false, nil
	}
	return enc, true, nil
}

// percentEncode escapes raw bytes for a form body or query string. It
// works on bytes rather than on a string because the bytes are already in
// the tracker's character set, and re-interpreting them as UTF-8 would
// undo the transcoding.
func percentEncode(b []byte) string {
	return percentEncodeWith(b, isUnreservedURLByte)
}

// percentEncodeWith is percentEncode over a caller-chosen safe set, for
// the "urlencode" filter, whose unreserved set is .NET's rather than
// RFC 3986's.
func percentEncodeWith(b []byte, safe func(byte) bool) string {
	var buf strings.Builder
	buf.Grow(len(b))
	for _, c := range b {
		switch {
		case safe(c):
			buf.WriteByte(c)
		case c == ' ':
			buf.WriteByte('+')
		default:
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

// percentDecode reverses percentEncodeWith, returning raw bytes rather
// than a string: the bytes are in the tracker's character set, and
// treating them as UTF-8 before transcoding would corrupt them.
func percentDecode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '+':
			out = append(out, ' ')
		case '%':
			if i+2 >= len(s) {
				return nil, fmt.Errorf("truncated percent-escape at %d", i)
			}
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("invalid percent-escape %q: %w", s[i:i+3], err)
			}
			out = append(out, byte(n))
			i += 2
		default:
			out = append(out, c)
		}
	}
	return out, nil
}

// encodeBytes renders a UTF-8 string as bytes in a definition's declared
// character set, so what a filter percent-encodes is what the tracker
// expects to receive.
func encodeBytes(value, encodingName string) ([]byte, error) {
	enc, transcode, err := formEncoding(encodingName)
	if err != nil {
		return nil, err
	}
	if !transcode {
		return []byte(value), nil
	}
	return enc.NewEncoder().Bytes([]byte(value))
}

// decodeBytes interprets bytes in a definition's declared character set
// as UTF-8. It is the inverse of encodeBytes.
func decodeBytes(raw []byte, encodingName string) (string, error) {
	enc, transcode, err := formEncoding(encodingName)
	if err != nil {
		return "", err
	}
	if !transcode {
		return string(raw), nil
	}
	decoded, err := enc.NewDecoder().Bytes(raw)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// isUnreservedURLByte reports whether a byte may appear literally in a
// query string, per RFC 3986's unreserved set.
func isUnreservedURLByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~':
		return true
	default:
		return false
	}
}

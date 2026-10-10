// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// maxBencodeDepth bounds how deeply a torrent file's lists and dictionaries
// nest. A torrent file nests a few levels, and a version 2 file tree one
// more per directory, so the bound is far past anything real and only
// keeps a crafted file from recursing through the whole download cap.
const maxBencodeDepth = 64

// maxBencodeIntegerDigits and maxBencodeLengthDigits are the most digits
// BencodeNET accepts in an integer, which it holds as an int64, and in a
// string's length.
const (
	maxBencodeIntegerDigits = 19
	maxBencodeLengthDigits  = 10
)

// canonicalTorrent parses the torrent file data starts with and encodes it
// again, as Jackett's download endpoint does through BencodeNET, since
// Sonarr refuses a torrent file whose dictionary keys are not sorted. Keys
// come out sorted by their bytes, a string's length loses any leading
// zeros, and whatever follows the file is dropped. What BencodeNET refuses
// to parse is refused here too: a duplicate or non-string dictionary key,
// an integer with a leading zero, a negative zero, a sign but no digits, or
// more than an int64 holds, and anything cut short. A file whose info
// dictionary was not sorted gets a different info hash, as it does from
// Jackett.
func canonicalTorrent(data []byte) ([]byte, error) {
	if len(data) == 0 || data[0] != 'd' {
		return nil, errors.New("the body is not a bencoded dictionary")
	}
	if _, err := scanBencode(data, 0, 0); err != nil {
		return nil, err
	}
	torrent, _ := appendBencode(make([]byte, 0, len(data)), data, 0)
	return torrent, nil
}

// scanBencode checks the value that starts at pos in data, depth lists and
// dictionaries deep, and returns where it ends.
func scanBencode(data []byte, pos, depth int) (int, error) {
	if pos >= len(data) {
		return 0, fmt.Errorf("the data ends at byte %d where a value was expected", pos)
	}
	switch kind := data[pos]; {
	case kind == 'i':
		length := bytes.IndexByte(data[pos:], 'e')
		if length < 0 {
			return 0, fmt.Errorf("the integer at byte %d is not terminated", pos)
		}
		if err := checkBencodeInteger(data[pos+1 : pos+length]); err != nil {
			return 0, fmt.Errorf("the integer at byte %d %w", pos, err)
		}
		return pos + length + 1, nil
	case kind == 'l' || kind == 'd':
		if depth >= maxBencodeDepth {
			return 0, fmt.Errorf("the value at byte %d nests more than %d deep", pos, maxBencodeDepth)
		}
		return scanBencodeContainer(data, pos, depth)
	case isDigit(kind):
		_, end, err := readBencodeString(data, pos)
		return end, err
	default:
		return 0, fmt.Errorf("byte %d is %q, which starts no value", pos, kind)
	}
}

// scanBencodeContainer checks the list or dictionary that starts at pos,
// and returns where it ends.
func scanBencodeContainer(data []byte, pos, depth int) (int, error) {
	start, isDictionary := pos, data[pos] == 'd'
	var keys [][]byte
	for pos++; pos < len(data) && data[pos] != 'e'; {
		if isDictionary {
			key, end, err := readBencodeString(data, pos)
			if err != nil {
				return 0, fmt.Errorf("a key of the dictionary at byte %d: %w", start, err)
			}
			keys = append(keys, key)
			pos = end
		}
		end, err := scanBencode(data, pos, depth+1)
		if err != nil {
			return 0, err
		}
		pos = end
	}
	if pos >= len(data) {
		return 0, fmt.Errorf("the value at byte %d is not terminated", start)
	}
	slices.SortFunc(keys, bytes.Compare)
	for i := 1; i < len(keys); i++ {
		if bytes.Equal(keys[i-1], keys[i]) {
			return 0, fmt.Errorf("the dictionary at byte %d has a key twice", start)
		}
	}
	return pos + 1, nil
}

// readBencodeString reads the string that starts at pos and returns its
// bytes and where it ends.
func readBencodeString(data []byte, pos int) ([]byte, int, error) {
	start := pos
	for pos < len(data) && isDigit(data[pos]) {
		if pos-start == maxBencodeLengthDigits {
			return nil, 0, fmt.Errorf("the string at byte %d has a length of more than %d digits", start, maxBencodeLengthDigits)
		}
		pos++
	}
	if pos == start || pos >= len(data) || data[pos] != ':' {
		return nil, 0, fmt.Errorf("byte %d starts no string", start)
	}
	length, err := strconv.Atoi(string(data[start:pos]))
	if err != nil || length > len(data)-pos-1 {
		return nil, 0, fmt.Errorf("the string at byte %d is cut short", start)
	}
	pos++
	return data[pos : pos+length], pos + length, nil
}

// checkBencodeInteger reports why digits, what an integer holds between
// its "i" and its "e", is not one.
func checkBencodeInteger(digits []byte) error {
	unsigned := bytes.TrimPrefix(digits, []byte("-"))
	switch {
	case len(unsigned) == 0:
		return errors.New("has no digits")
	case len(unsigned) > maxBencodeIntegerDigits || !isAllDigits(unsigned):
		return errors.New("is not a 64-bit integer")
	case unsigned[0] == '0' && len(unsigned) > 1:
		return errors.New("has a leading zero")
	case unsigned[0] == '0' && len(unsigned) < len(digits):
		return errors.New("is a negative zero")
	}
	if _, err := strconv.ParseInt(string(digits), 10, 64); err != nil {
		return errors.New("is not a 64-bit integer")
	}
	return nil
}

// appendBencode appends the value that starts at pos in data, which
// scanBencode has checked, to encoded with its dictionaries' keys sorted,
// and returns where the value ends.
func appendBencode(encoded, data []byte, pos int) ([]byte, int) {
	switch data[pos] {
	case 'i':
		end := pos + bytes.IndexByte(data[pos:], 'e') + 1
		return append(encoded, data[pos:end]...), end
	case 'l':
		encoded = append(encoded, 'l')
		for pos++; data[pos] != 'e'; {
			encoded, pos = appendBencode(encoded, data, pos)
		}
		return append(encoded, 'e'), pos + 1
	case 'd':
		type entry struct {
			key   []byte
			value int
		}
		var entries []entry
		for pos++; data[pos] != 'e'; {
			key, value, _ := readBencodeString(data, pos)
			entries = append(entries, entry{key: key, value: value})
			pos, _ = scanBencode(data, value, 0)
		}
		slices.SortFunc(entries, func(a, b entry) int { return bytes.Compare(a.key, b.key) })
		encoded = append(encoded, 'd')
		for _, e := range entries {
			encoded = appendBencodeString(encoded, e.key)
			encoded, _ = appendBencode(encoded, data, e.value)
		}
		return append(encoded, 'e'), pos + 1
	default:
		value, end, _ := readBencodeString(data, pos)
		return appendBencodeString(encoded, value), end
	}
}

// appendBencodeString appends value to encoded as a bencoded string.
func appendBencodeString(encoded, value []byte) []byte {
	encoded = strconv.AppendInt(encoded, int64(len(value)), 10)
	encoded = append(encoded, ':')
	return append(encoded, value...)
}

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool {
	return '0' <= b && b <= '9'
}

// isAllDigits reports whether every byte of digits is an ASCII digit.
func isAllDigits(digits []byte) bool {
	for _, b := range digits {
		if !isDigit(b) {
			return false
		}
	}
	return true
}

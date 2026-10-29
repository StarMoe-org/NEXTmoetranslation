// Package jsonstrict provides a single-pass, near-zero-allocation structural
// JSON validator that checks duplicate object keys, maximum nesting depth,
// forbidden null tokens, lone UTF-16 surrogate escapes, and custom key rules
// without building intermediate token streams or map[string]any trees.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"
)

// Options configures structural constraints enforced during a single byte scan.
type Options struct {
	MaxDepth             int
	ForbidNull           bool
	ForbidLoneSurrogates bool
	RequireValidUTF8     bool
	CheckKey             func(key []byte) error
}

var scannerPool = sync.Pool{
	New: func() any {
		return &scanner{keys: make([][]byte, 0, 128)}
	},
}

// Scan validates that data contains exactly one valid RFC 8259 JSON value
// satisfying opts, with no duplicate object keys at any nesting level and no
// trailing tokens.
func Scan(data []byte, opts Options) error {
	if opts.RequireValidUTF8 && !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}
	s := scannerPool.Get().(*scanner)
	s.data = data
	s.pos = 0
	s.opts = opts
	s.keys = s.keys[:0]
	defer func() {
		clear(s.keys[:cap(s.keys)])
		s.data = nil
		s.opts = Options{}
		scannerPool.Put(s)
	}()
	s.skipWS()
	if s.pos >= len(s.data) {
		return errors.New("JSON is empty")
	}
	if err := s.scanValue(1); err != nil {
		return err
	}
	s.skipWS()
	if s.pos != len(s.data) {
		return errors.New("trailing JSON value")
	}
	return nil
}

const inlineKeyDupLimit = 24

type scanner struct {
	data []byte
	pos  int
	opts Options
	keys [][]byte
}

func (s *scanner) skipWS() {
	d := s.data
	i := s.pos
	for i < len(d) {
		switch d[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			s.pos = i
			return
		}
	}
	s.pos = i
}

func (s *scanner) scanValue(depth int) error {
	if s.opts.MaxDepth > 0 && depth > s.opts.MaxDepth {
		return fmt.Errorf("JSON exceeds maximum nesting depth %d", s.opts.MaxDepth)
	}
	if s.pos >= len(s.data) {
		return errors.New("unexpected end of JSON")
	}
	switch s.data[s.pos] {
	case '{':
		return s.scanObject(depth)
	case '[':
		return s.scanArray(depth)
	case '"':
		_, err := s.scanString(false)
		return err
	case 't':
		if s.pos+4 <= len(s.data) && string(s.data[s.pos:s.pos+4]) == "true" {
			s.pos += 4
			return nil
		}
		return errors.New("invalid JSON literal")
	case 'f':
		if s.pos+5 <= len(s.data) && string(s.data[s.pos:s.pos+5]) == "false" {
			s.pos += 5
			return nil
		}
		return errors.New("invalid JSON literal")
	case 'n':
		if s.pos+4 <= len(s.data) && string(s.data[s.pos:s.pos+4]) == "null" {
			if s.opts.ForbidNull {
				return errors.New("JSON contains null")
			}
			s.pos += 4
			return nil
		}
		return errors.New("invalid JSON literal")
	default:
		if s.data[s.pos] == '-' || (s.data[s.pos] >= '0' && s.data[s.pos] <= '9') {
			return s.scanNumber()
		}
		return fmt.Errorf("unexpected character %q in JSON", s.data[s.pos])
	}
}

func (s *scanner) scanObject(depth int) error {
	s.pos++ // '{'
	s.skipWS()
	if s.pos >= len(s.data) {
		return errors.New("unterminated JSON object")
	}
	if s.data[s.pos] == '}' {
		s.pos++
		return nil
	}

	frameStart := len(s.keys)
	var largeSeen map[string]struct{}

	for {
		if s.pos >= len(s.data) || s.data[s.pos] != '"' {
			s.keys = s.keys[:frameStart]
			return errors.New("expected string key in JSON object")
		}
		key, err := s.scanString(true)
		if err != nil {
			s.keys = s.keys[:frameStart]
			return err
		}
		if s.opts.CheckKey != nil {
			if err := s.opts.CheckKey(key); err != nil {
				s.keys = s.keys[:frameStart]
				return err
			}
		}
		if largeSeen == nil {
			for i := frameStart; i < len(s.keys); i++ {
				if bytes.Equal(s.keys[i], key) {
					s.keys = s.keys[:frameStart]
					return fmt.Errorf("duplicate object key %q", string(key))
				}
			}
			if len(s.keys)-frameStart >= inlineKeyDupLimit {
				largeSeen = make(map[string]struct{}, inlineKeyDupLimit*2)
				for i := frameStart; i < len(s.keys); i++ {
					largeSeen[string(s.keys[i])] = struct{}{}
				}
				largeSeen[string(key)] = struct{}{}
			} else {
				s.keys = append(s.keys, key)
			}
		} else {
			ks := string(key)
			if _, exists := largeSeen[ks]; exists {
				s.keys = s.keys[:frameStart]
				return fmt.Errorf("duplicate object key %q", ks)
			}
			largeSeen[ks] = struct{}{}
		}

		s.skipWS()
		if s.pos >= len(s.data) || s.data[s.pos] != ':' {
			s.keys = s.keys[:frameStart]
			return errors.New("expected ':' after JSON object key")
		}
		s.pos++
		s.skipWS()

		if err := s.scanValue(depth + 1); err != nil {
			s.keys = s.keys[:frameStart]
			return err
		}

		s.skipWS()
		if s.pos >= len(s.data) {
			s.keys = s.keys[:frameStart]
			return errors.New("unterminated JSON object")
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
			s.skipWS()
		case '}':
			s.pos++
			s.keys = s.keys[:frameStart]
			return nil
		default:
			s.keys = s.keys[:frameStart]
			return errors.New("expected ',' or '}' in JSON object")
		}
	}
}

func (s *scanner) scanArray(depth int) error {
	s.pos++ // '['
	s.skipWS()
	if s.pos >= len(s.data) {
		return errors.New("unterminated JSON array")
	}
	if s.data[s.pos] == ']' {
		s.pos++
		return nil
	}
	for {
		if err := s.scanValue(depth + 1); err != nil {
			return err
		}
		s.skipWS()
		if s.pos >= len(s.data) {
			return errors.New("unterminated JSON array")
		}
		switch s.data[s.pos] {
		case ',':
			s.pos++
			s.skipWS()
		case ']':
			s.pos++
			return nil
		default:
			return errors.New("expected ',' or ']' in JSON array")
		}
	}
}

func (s *scanner) scanString(needUnescaped bool) ([]byte, error) {
	d := s.data
	quoteStart := s.pos
	i := quoteStart + 1
	contentStart := i

	// Tight fast loop for unescaped strings (covers >99.9% of keys & values).
	for i < len(d) {
		c := d[i]
		if c == '"' {
			s.pos = i + 1
			return d[contentStart:i], nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return nil, errors.New("invalid control character in JSON string")
		}
		i++
	}

	// Escape-aware loop.
	for i < len(d) {
		c := d[i]
		if c == '"' {
			endQuote := i + 1
			s.pos = endQuote
			if !needUnescaped {
				return nil, nil
			}
			var unescaped string
			if err := json.Unmarshal(d[quoteStart:endQuote], &unescaped); err != nil {
				return nil, err
			}
			return []byte(unescaped), nil
		}
		if c < 0x20 {
			return nil, errors.New("invalid control character in JSON string")
		}
		if c != '\\' {
			i++
			continue
		}
		if i+1 >= len(d) {
			return nil, errors.New("incomplete escape in JSON string")
		}
		switch d[i+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i += 2
		case 'u':
			u, ok := parseHexQuad(d, i+2)
			if !ok {
				return nil, errors.New("invalid Unicode escape in JSON string")
			}
			if s.opts.ForbidLoneSurrogates {
				switch {
				case u >= 0xD800 && u <= 0xDBFF:
					lowIdx := i + 6
					if lowIdx+6 > len(d) || d[lowIdx] != '\\' || d[lowIdx+1] != 'u' {
						return nil, errors.New("JSON contains an escaped lone high surrogate")
					}
					low, okLow := parseHexQuad(d, lowIdx+2)
					if !okLow || low < 0xDC00 || low > 0xDFFF {
						return nil, errors.New("JSON contains an escaped lone high surrogate")
					}
					i = lowIdx + 6
					continue
				case u >= 0xDC00 && u <= 0xDFFF:
					return nil, errors.New("JSON contains an escaped lone low surrogate")
				}
			}
			i += 6
		default:
			return nil, errors.New("invalid escape character in JSON string")
		}
	}
	return nil, errors.New("unterminated JSON string")
}

func (s *scanner) scanNumber() error {
	d := s.data
	i := s.pos
	if d[i] == '-' {
		i++
		if i >= len(d) {
			return errors.New("invalid JSON number")
		}
	}
	switch {
	case d[i] == '0':
		i++
	case d[i] >= '1' && d[i] <= '9':
		i++
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
	default:
		return errors.New("invalid JSON number")
	}
	if i < len(d) && d[i] == '.' {
		i++
		if i >= len(d) || d[i] < '0' || d[i] > '9' {
			return errors.New("invalid fraction in JSON number")
		}
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
	}
	if i < len(d) && (d[i] == 'e' || d[i] == 'E') {
		i++
		if i < len(d) && (d[i] == '+' || d[i] == '-') {
			i++
		}
		if i >= len(d) || d[i] < '0' || d[i] > '9' {
			return errors.New("invalid exponent in JSON number")
		}
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
	}
	s.pos = i
	return nil
}

func parseHexQuad(body []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(body) {
		return 0, false
	}
	var value uint16
	for j := start; j < start+4; j++ {
		value <<= 4
		c := body[j]
		switch {
		case c >= '0' && c <= '9':
			value |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			value |= uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			value |= uint16(c-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

package lyricsevidencepack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
)

// DecodeSelection accepts only canonical, duplicate-free, closed selection JSON.
func DecodeSelection(body []byte) (Selection, error) {
	var selection Selection
	if err := decodeCanonicalJSON(body, &selection, MaxManifestBytes, "evidence selection"); err != nil {
		return Selection{}, err
	}
	if selection.SchemaVersion != SchemaVersionV1 {
		return Selection{}, errors.New("evidence selection schema version is invalid")
	}
	if err := lyricscontract.ValidateOrderedSelection(selection.Evidence); err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// DecodeManifest accepts only canonical, duplicate-free, closed manifest JSON.
func DecodeManifest(body []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeCanonicalJSON(body, &manifest, MaxManifestBytes, "evidence pack manifest"); err != nil {
		return Manifest{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// DecodeCanonicalEnvelope validates one canonical source envelope before use.
func DecodeCanonicalEnvelope(body []byte) (lyricssource.IndexEvidence, error) {
	var envelope lyricssource.IndexEvidence
	if err := decodeCanonicalJSON(body, &envelope, MaxEnvelopeEncodedBytes, "evidence envelope"); err != nil {
		return lyricssource.IndexEvidence{}, err
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(envelope); err != nil {
		return lyricssource.IndexEvidence{}, fmt.Errorf("validate evidence envelope: %w", err)
	}
	return envelope, nil
}

func decodeShard(body []byte) (shardEnvelope, error) {
	var shard shardEnvelope
	if err := decodeCanonicalJSON(body, &shard, MaxShardEncodedBytes, "evidence shard"); err != nil {
		return shardEnvelope{}, err
	}
	return shard, nil
}

func decodeShardItemsNoCopy(body []byte, ordinal int) ([]lyricssource.IndexEvidence, [][]byte, error) {
	prefix := []byte(fmt.Sprintf(`{"schemaVersion":%d,"ordinal":%d,"items":[`, SchemaVersionV1, ordinal))
	if len(body) == 0 || len(body) > MaxShardEncodedBytes || !utf8.Valid(body) ||
		!bytes.HasPrefix(body, prefix) || !bytes.HasSuffix(body, []byte("]}")) {
		return nil, nil, errors.New("evidence shard canonical envelope is invalid")
	}
	if err := validateJSONSurrogates(body, "evidence shard"); err != nil {
		return nil, nil, err
	}
	itemBodies, err := splitJSONArrayElements(body[len(prefix) : len(body)-2])
	if err != nil {
		return nil, nil, err
	}
	items := make([]lyricssource.IndexEvidence, len(itemBodies))
	for index, itemBody := range itemBodies {
		item, err := DecodeCanonicalEnvelope(itemBody)
		if err != nil {
			return nil, nil, fmt.Errorf("decode evidence shard item %d: %w", index, err)
		}
		items[index] = item
	}
	return items, itemBodies, nil
}

func splitJSONArrayElements(body []byte) ([][]byte, error) {
	if len(body) == 0 {
		return [][]byte{}, nil
	}
	items := make([][]byte, 0)
	start := 0
	depth := 0
	inString := false
	escaped := false
	for index, current := range body {
		if inString {
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}
		switch current {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth+2 > MaxJSONDepth {
				return nil, errors.New("evidence shard JSON exceeds its maximum nesting depth")
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return nil, errors.New("evidence shard item nesting is invalid")
			}
		case ',':
			if depth == 0 {
				if index == start {
					return nil, errors.New("evidence shard contains an empty item")
				}
				items = append(items, body[start:index])
				start = index + 1
				if len(items) > lyricscontract.MaxPackItems {
					return nil, errors.New("evidence shard exceeds the item ceiling")
				}
			}
		}
	}
	if inString || escaped || depth != 0 || start >= len(body) {
		return nil, errors.New("evidence shard item array is invalid")
	}
	items = append(items, body[start:])
	if len(items) > lyricscontract.MaxPackItems {
		return nil, errors.New("evidence shard exceeds the item ceiling")
	}
	return items, nil
}

func decodeCanonicalJSON(body []byte, target any, maximum int, label string) error {
	if target == nil {
		return errors.New("JSON target is required")
	}
	if len(body) == 0 {
		return fmt.Errorf("%s JSON is empty", label)
	}
	if len(body) > maximum {
		return fmt.Errorf("%s JSON exceeds %d bytes", label, maximum)
	}
	if !utf8.Valid(body) {
		return fmt.Errorf("%s JSON is not valid UTF-8", label)
	}
	if err := validateJSONSurrogates(body, label); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder, 1, label); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("%s trailing bytes: %w", label, err)
		}
		return fmt.Errorf("%s contains a trailing JSON value", label)
	}
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode %s trailing bytes: %w", label, err)
		}
		return fmt.Errorf("%s contains a trailing JSON value", label)
	}
	comparison := &canonicalComparisonWriter{expected: body}
	encoder := json.NewEncoder(comparison)
	if err := encoder.Encode(target); err != nil {
		return fmt.Errorf("re-encode %s: %w", label, err)
	}
	if !comparison.matches() {
		return fmt.Errorf("%s JSON is not canonical %s", label, CanonicalEncodingV1)
	}
	return nil
}

type canonicalComparisonWriter struct {
	expected []byte
	offset   int
	mismatch bool
}

func (writer *canonicalComparisonWriter) Write(body []byte) (int, error) {
	for _, current := range body {
		var expected byte
		switch {
		case writer.offset < len(writer.expected):
			expected = writer.expected[writer.offset]
		case writer.offset == len(writer.expected):
			expected = '\n'
		default:
			writer.mismatch = true
			writer.offset++
			continue
		}
		if current != expected {
			writer.mismatch = true
		}
		writer.offset++
	}
	return len(body), nil
}

func (writer *canonicalComparisonWriter) matches() bool {
	return writer != nil && !writer.mismatch && writer.offset == len(writer.expected)+1
}

func inspectJSONValue(decoder *json.Decoder, depth int, label string) error {
	if depth > MaxJSONDepth {
		return fmt.Errorf("%s JSON exceeds maximum nesting depth %d", label, MaxJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("inspect %s JSON: %w", label, err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("inspect %s object: %w", label, err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s object contains a non-string key", label)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate %s JSON field %q", label, key)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder, depth+1, label); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid %s JSON object", label)
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder, depth+1, label); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid %s JSON array", label)
		}
	default:
		return fmt.Errorf("invalid %s JSON delimiter", label)
	}
	return nil
}

func validateJSONSurrogates(body []byte, label string) error {
	inString := false
	for index := 0; index < len(body); {
		current := body[index]
		if !inString {
			if current == '"' {
				inString = true
			}
			index++
			continue
		}
		switch current {
		case '"':
			inString = false
			index++
		case '\\':
			if index+1 >= len(body) {
				return fmt.Errorf("%s JSON contains an incomplete escape", label)
			}
			if body[index+1] != 'u' {
				index += 2
				continue
			}
			codeUnit, ok := parseHexQuad(body, index+2)
			if !ok {
				return fmt.Errorf("%s JSON contains an invalid Unicode escape", label)
			}
			switch {
			case codeUnit >= 0xD800 && codeUnit <= 0xDBFF:
				lowIndex := index + 6
				if lowIndex+6 > len(body) || body[lowIndex] != '\\' || body[lowIndex+1] != 'u' {
					return fmt.Errorf("%s JSON contains an escaped lone high surrogate", label)
				}
				low, validLow := parseHexQuad(body, lowIndex+2)
				if !validLow || low < 0xDC00 || low > 0xDFFF {
					return fmt.Errorf("%s JSON contains an escaped lone high surrogate", label)
				}
				index = lowIndex + 6
			case codeUnit >= 0xDC00 && codeUnit <= 0xDFFF:
				return fmt.Errorf("%s JSON contains an escaped lone low surrogate", label)
			default:
				index += 6
			}
		default:
			index++
		}
	}
	return nil
}

func parseHexQuad(body []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(body) {
		return 0, false
	}
	var value uint16
	for _, current := range body[start : start+4] {
		value <<= 4
		switch {
		case current >= '0' && current <= '9':
			value |= uint16(current - '0')
		case current >= 'a' && current <= 'f':
			value |= uint16(current-'a') + 10
		case current >= 'A' && current <= 'F':
			value |= uint16(current-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

package lyricsrootmanifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
)

// DecodeAssemblyRequest accepts only canonical, closed, duplicate-free request JSON.
func DecodeAssemblyRequest(body []byte) (AssemblyRequest, error) {
	var request AssemblyRequest
	if err := decodeCanonicalJSON(body, &request, MaxAssemblyRequestBytes, "lyrics root assembly request"); err != nil {
		return AssemblyRequest{}, err
	}
	if _, err := validateRequest(request, lyricscontract.SchemaVersionV2); err != nil {
		return AssemblyRequest{}, err
	}
	return request, nil
}

// DecodeCanonical rejects malformed, duplicate, unknown, trailing, over-depth,
// non-UTF-8, oversized, noncanonical, and self-contained structurally invalid
// root bytes. A partial or retry return is not supersession proof; consumers
// must additionally call ValidateAgainstParent or DecodeCanonicalAgainstParent.
func DecodeCanonical(body []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeCanonicalJSON(body, &manifest, MaxManifestBytes, "lyrics root manifest"); err != nil {
		return Manifest{}, err
	}
	if err := Validate(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// DecodeCanonicalAgainstParent canonically decodes a partial or retry root and
// proves its direct supersession, catalog compatibility, and strict song subset
// against the exact self-contained validated parent. Final roots must use
// DecodeCanonical; a non-final parent still requires its own retained proof.
func DecodeCanonicalAgainstParent(body []byte, parent Manifest) (Manifest, error) {
	manifest, err := DecodeCanonical(body)
	if err != nil {
		return Manifest{}, err
	}
	if err := ValidateAgainstParent(manifest, parent); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
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
	canonical, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("re-encode %s: %w", label, err)
	}
	if !bytes.Equal(body, canonical) {
		return fmt.Errorf("%s JSON is not canonical %s", label, CanonicalEncodingV1)
	}
	return nil
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

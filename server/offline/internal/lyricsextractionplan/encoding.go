package lyricsextractionplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// historicalSekaipediaRubyGeneratorAlias exists only to decode immutable
// historical plan bytes. Validation and every encoder require the canonical
// non-romaji generator vocabulary.
const historicalSekaipediaRubyGeneratorAlias = "sekaipedia-romaji-kana-v1"

// MarshalCanonical emits the extraction-plan-v1 owned encoding: compact JSON
// in the declared struct-field and array order, using Go's encoding/json string
// escaping, with no trailing newline. This is deliberately not an RFC 8785
// claim; any future encoding change requires a new contract version.
func MarshalCanonical(plan Plan) ([]byte, error) {
	if err := Validate(plan); err != nil {
		return nil, err
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("encode canonical extraction plan: %w", err)
	}
	if len(body) == 0 || len(body) > MaxPlanBytes || !utf8.Valid(body) {
		return nil, errors.New("canonical extraction plan exceeds its encoded boundary")
	}
	return body, nil
}

func CanonicalSHA256(plan Plan) (string, error) {
	body, err := MarshalCanonical(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// DecodeCanonical rejects malformed, duplicate-key, unknown-field, trailing,
// non-UTF-8, over-depth, over-size, semantically invalid, and noncanonical plan
// bytes before returning a usable Plan.
func DecodeCanonical(body []byte) (Plan, error) {
	var plan Plan
	if err := inspectPlanJSON(body); err != nil {
		return Plan{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return Plan{}, fmt.Errorf("decode extraction plan: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return Plan{}, fmt.Errorf("decode extraction plan trailing bytes: %w", err)
		}
		return Plan{}, errors.New("decode extraction plan: trailing JSON value")
	}
	canonical, err := json.Marshal(plan)
	if err != nil {
		return Plan{}, fmt.Errorf("re-encode extraction plan: %w", err)
	}
	if !bytes.Equal(body, canonical) {
		return Plan{}, errors.New("extraction plan bytes are not canonical extraction-plan-v1 JSON")
	}
	normalizeHistoricalPlanAliases(&plan)
	if err := Validate(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func normalizeHistoricalPlanAliases(plan *Plan) {
	if plan == nil {
		return
	}
	for index := range plan.EffectiveVersions.Parsers {
		parser := &plan.EffectiveVersions.Parsers[index]
		if parser.Provider == ProviderSekaipedia && parser.ParserVersion == registeredSekaipediaParser &&
			parser.RubyGeneratorVersion == historicalSekaipediaRubyGeneratorAlias {
			parser.RubyGeneratorVersion = historicalRegisteredSekaipediaRuby
		}
	}
}

// Check validates canonical bytes and binds them to an independently supplied
// expected digest. The expected digest is intentionally outside the plan so it
// cannot self-authenticate.
func Check(body []byte, expectedSHA256 string) (Plan, string, error) {
	if !canonicalSHA256.MatchString(expectedSHA256) {
		return Plan{}, "", errors.New("expected plan digest must be a canonical lowercase SHA-256")
	}
	plan, err := DecodeCanonical(body)
	if err != nil {
		return Plan{}, "", err
	}
	digest := sha256.Sum256(body)
	actual := hex.EncodeToString(digest[:])
	if actual != expectedSHA256 {
		return Plan{}, actual, errors.New("extraction plan does not match the expected digest")
	}
	return plan, actual, nil
}

func SourceSnapshotSHA256(files []SourceFileIdentity) (string, error) {
	if files == nil || len(files) == 0 || len(files) > MaxSourceSnapshotFiles {
		return "", errors.New("source snapshot file identities are required")
	}
	var total int64
	lastPath := ""
	for index, file := range files {
		if !validDataPath(file.Path) || (index > 0 && file.Path <= lastPath) {
			return "", errors.New("source snapshot file identities must use unique paths in ascending order")
		}
		lastPath = file.Path
		if file.SizeBytes <= 0 || file.SizeBytes > MaxSourceFileBytes || !canonicalSHA256.MatchString(file.SHA256) {
			return "", fmt.Errorf("source snapshot file %q has an invalid exact identity", file.Path)
		}
		total += file.SizeBytes
		if total > MaxSourceSnapshotBytes {
			return "", errors.New("source snapshot exceeds its total byte ceiling")
		}
	}
	body, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("encode source snapshot identities: %w", err)
	}
	domain := []byte(SnapshotAlgorithmV1 + "\x00")
	digest := sha256.New()
	_, _ = digest.Write(domain)
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func inspectPlanJSON(body []byte) error {
	if len(body) == 0 {
		return errors.New("extraction plan JSON is empty")
	}
	if len(body) > MaxPlanBytes {
		return fmt.Errorf("extraction plan JSON exceeds %d bytes", MaxPlanBytes)
	}
	if !utf8.Valid(body) {
		return errors.New("extraction plan JSON is not valid UTF-8")
	}
	if err := validateJSONSurrogates(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("extraction plan trailing bytes: %w", err)
		}
		return errors.New("extraction plan contains a trailing JSON value")
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder, depth int) error {
	if depth > MaxPlanJSONDepth {
		return fmt.Errorf("extraction plan JSON exceeds maximum nesting depth %d", MaxPlanJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("inspect extraction plan JSON: %w", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("inspect extraction plan object: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("extraction plan object contains a non-string key")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate extraction plan JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid extraction plan JSON object")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid extraction plan JSON array")
		}
	default:
		return errors.New("invalid extraction plan JSON delimiter")
	}
	return nil
}

func validateJSONSurrogates(body []byte) error {
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
				return errors.New("extraction plan JSON contains an incomplete escape")
			}
			if body[index+1] != 'u' {
				index += 2
				continue
			}
			codeUnit, ok := parseHexQuad(body, index+2)
			if !ok {
				return errors.New("extraction plan JSON contains an invalid Unicode escape")
			}
			switch {
			case codeUnit >= 0xD800 && codeUnit <= 0xDBFF:
				lowIndex := index + 6
				if lowIndex+6 > len(body) || body[lowIndex] != '\\' || body[lowIndex+1] != 'u' {
					return errors.New("extraction plan JSON contains an escaped lone high surrogate")
				}
				low, validLow := parseHexQuad(body, lowIndex+2)
				if !validLow || low < 0xDC00 || low > 0xDFFF {
					return errors.New("extraction plan JSON contains an escaped lone high surrogate")
				}
				index = lowIndex + 6
			case codeUnit >= 0xDC00 && codeUnit <= 0xDFFF:
				return errors.New("extraction plan JSON contains an escaped lone low surrogate")
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

func containsShellFragment(value string) bool {
	return strings.ContainsAny(value, " ;&|$`'\"(){}[]<>*?!~\\\t\r\n")
}

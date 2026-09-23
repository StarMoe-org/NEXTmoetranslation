package lyricsoutcomeartifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

func DecodeCanonical(body []byte) (Artifact, error) {
	if len(body) == 0 || len(body) > MaxArtifactBytes || !utf8.Valid(body) {
		return Artifact{}, errors.New("provider outcome artifact bytes are invalid")
	}
	if err := inspectJSON(body); err != nil {
		return Artifact{}, err
	}
	var artifact Artifact
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode provider outcome artifact: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return Artifact{}, fmt.Errorf("decode provider outcome trailing bytes: %w", err)
		}
		return Artifact{}, errors.New("provider outcome artifact contains a trailing JSON value")
	}
	if err := Validate(artifact); err != nil {
		return Artifact{}, err
	}
	canonical, err := json.Marshal(artifact)
	if err != nil || !bytes.Equal(canonical, body) {
		return Artifact{}, errors.New("provider outcome artifact bytes are not canonical JSON")
	}
	return cloneArtifact(artifact), nil
}

func inspectJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := inspectArtifactJSONValue(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("inspect provider outcome trailing JSON: %w", err)
		}
		return errors.New("provider outcome JSON contains a trailing value")
	}
	return nil
}

func inspectArtifactJSONValue(decoder *json.Decoder, depth int) error {
	if depth > MaxJSONDepth {
		return errors.New("provider outcome JSON exceeds maximum depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("inspect provider outcome JSON: %w", err)
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
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("provider outcome JSON object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("provider outcome JSON contains a duplicate object key")
			}
			seen[key] = struct{}{}
			if err := inspectArtifactJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("provider outcome JSON object is invalid")
		}
	case '[':
		for decoder.More() {
			if err := inspectArtifactJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("provider outcome JSON array is invalid")
		}
	default:
		return errors.New("provider outcome JSON delimiter is invalid")
	}
	return nil
}

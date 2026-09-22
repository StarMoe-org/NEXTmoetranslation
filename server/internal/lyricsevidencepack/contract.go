// Package lyricsevidencepack builds and resolves the bounded private evidence-pack v1 format.
package lyricsevidencepack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
)

const (
	SchemaVersionV1     = 1
	CanonicalEncodingV1 = "moesekai-lyrics-evidence-pack-ordered-json-v1"
	DigestAlgorithmV1   = "sha256-moesekai-lyrics-evidence-pack-v1"

	ManifestFileName = "manifest.json"

	MaxJSONDepth                  = 16
	MaxManifestBytes              = 32 << 20
	MaxEnvelopeEncodedBytes       = 4 << 20
	MaxShardRawBytes              = 16 << 20
	MaxShardEncodedBytes          = 32 << 20
	MaxPackRawBytes         int64 = 512 << 20
	MaxPackEncodedBytes     int64 = 1 << 30
)

var (
	canonicalSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalEvidence = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

	ErrAlreadyPublished = errors.New("lyrics evidence pack is already published")
)

// Selection is the closed command input that declares the exact global evidence union.
type Selection struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Evidence      []lyricscontract.EvidenceRef `json:"evidence"`
}

// ShardManifest binds one deterministic shard without exposing a filesystem path.
type ShardManifest struct {
	Ordinal          int                          `json:"ordinal"`
	SHA256           string                       `json:"sha256"`
	EncodedByteCount int                          `json:"encodedByteCount"`
	RawByteCount     int                          `json:"rawByteCount"`
	ItemCount        int                          `json:"itemCount"`
	FirstEvidenceID  string                       `json:"firstEvidenceId"`
	LastEvidenceID   string                       `json:"lastEvidenceId"`
	Items            []lyricscontract.EvidenceRef `json:"items"`
}

// Totals fixes the aggregate private safety accounting.
type Totals struct {
	ItemCount        int   `json:"itemCount"`
	ShardCount       int   `json:"shardCount"`
	RawByteCount     int64 `json:"rawByteCount"`
	EncodedByteCount int64 `json:"encodedByteCount"`
}

// Manifest is the compact, self-digested index for all bounded shard payloads.
type Manifest struct {
	SchemaVersion     int                          `json:"schemaVersion"`
	CanonicalEncoding string                       `json:"canonicalEncoding"`
	DigestAlgorithm   string                       `json:"digestAlgorithm"`
	SelectionSHA256   string                       `json:"selectionSha256"`
	Selected          []lyricscontract.EvidenceRef `json:"selected"`
	Shards            []ShardManifest              `json:"shards"`
	Totals            Totals                       `json:"totals"`
	PackSHA256        string                       `json:"packSha256"`
}

type shardEnvelope struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Ordinal       int                          `json:"ordinal"`
	Items         []lyricssource.IndexEvidence `json:"items"`
}

func evidenceRefLess(left, right lyricscontract.EvidenceRef) bool {
	return left.EvidenceID < right.EvidenceID
}

func canonicalSelection(input []lyricscontract.EvidenceRef) ([]lyricscontract.EvidenceRef, error) {
	if input == nil || len(input) > lyricscontract.MaxPackItems {
		return nil, errors.New("selected evidence must be an explicit bounded array")
	}
	selected := append([]lyricscontract.EvidenceRef{}, input...)
	sort.Slice(selected, func(left, right int) bool { return evidenceRefLess(selected[left], selected[right]) })
	if err := lyricscontract.ValidateOrderedSelection(selected); err != nil {
		return nil, err
	}
	return selected, nil
}

// SelectionSHA256 returns the domain-separated digest of the canonical unique selection.
func SelectionSHA256(refs []lyricscontract.EvidenceRef) (string, error) {
	selected, err := canonicalSelection(refs)
	if err != nil {
		return "", err
	}
	return lyricscontract.OrderedSelectionSHA256(selected)
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.PackSHA256 = ""
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-evidence-pack-manifest-v1\x00"))
	if _, err := streamCanonicalJSON(manifest, digest); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

type trailingNewlineWriter struct {
	destination io.Writer
	last        byte
	hasLast     bool
	count       int
}

func (writer *trailingNewlineWriter) Write(body []byte) (int, error) {
	if len(body) == 0 {
		return 0, nil
	}
	if writer.hasLast {
		written, err := writer.destination.Write([]byte{writer.last})
		if err != nil {
			return 0, err
		}
		if written != 1 {
			return 0, io.ErrShortWrite
		}
		writer.count++
	}
	if len(body) > 1 {
		written, err := writer.destination.Write(body[:len(body)-1])
		if err != nil {
			return written, err
		}
		if written != len(body)-1 {
			return written, io.ErrShortWrite
		}
		writer.count += written
	}
	writer.last = body[len(body)-1]
	writer.hasLast = true
	return len(body), nil
}

func streamCanonicalJSON(value any, destination io.Writer) (int, error) {
	writer := &trailingNewlineWriter{destination: destination}
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(value); err != nil {
		return 0, err
	}
	if !writer.hasLast || writer.last != '\n' {
		return 0, errors.New("canonical JSON encoder did not terminate with a newline")
	}
	return writer.count, nil
}

// ValidateManifest verifies all ordering, exact-union, bounds, counters, and self-digest invariants.
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersionV1 || manifest.CanonicalEncoding != CanonicalEncodingV1 ||
		manifest.DigestAlgorithm != DigestAlgorithmV1 || !canonicalSHA256.MatchString(manifest.SelectionSHA256) ||
		!canonicalSHA256.MatchString(manifest.PackSHA256) {
		return errors.New("evidence pack manifest envelope is invalid")
	}
	if err := lyricscontract.ValidateOrderedSelection(manifest.Selected); err != nil {
		return err
	}
	selectionDigest, err := lyricscontract.OrderedSelectionSHA256(manifest.Selected)
	if err != nil || selectionDigest != manifest.SelectionSHA256 {
		return errors.New("evidence pack selection digest does not match")
	}
	if manifest.Shards == nil || len(manifest.Shards) > lyricscontract.MaxPackItems {
		return errors.New("evidence pack shards must be an explicit bounded array")
	}
	selectedIndex := 0
	var rawTotal, encodedTotal int64
	for shardIndex, shard := range manifest.Shards {
		if shard.Ordinal != shardIndex || !canonicalSHA256.MatchString(shard.SHA256) || shard.ItemCount <= 0 ||
			shard.ItemCount != len(shard.Items) || shard.RawByteCount <= 0 || shard.RawByteCount > MaxShardRawBytes ||
			shard.EncodedByteCount <= 0 || shard.EncodedByteCount > MaxShardEncodedBytes ||
			len(shard.Items) == 0 || shard.FirstEvidenceID != shard.Items[0].EvidenceID ||
			shard.LastEvidenceID != shard.Items[len(shard.Items)-1].EvidenceID {
			return fmt.Errorf("evidence shard manifest %d is invalid", shardIndex)
		}
		if err := lyricscontract.ValidateOrderedSelection(shard.Items); err != nil {
			return fmt.Errorf("evidence shard manifest %d: %w", shardIndex, err)
		}
		for _, ref := range shard.Items {
			if selectedIndex >= len(manifest.Selected) || manifest.Selected[selectedIndex] != ref {
				return errors.New("evidence shard union does not equal the selected evidence union")
			}
			selectedIndex++
		}
		rawTotal += int64(shard.RawByteCount)
		encodedTotal += int64(shard.EncodedByteCount)
	}
	if selectedIndex != len(manifest.Selected) {
		return errors.New("evidence shard union is missing selected evidence")
	}
	if len(manifest.Selected) == 0 && len(manifest.Shards) != 0 {
		return errors.New("empty evidence selection must not emit shards")
	}
	if manifest.Totals.ItemCount != len(manifest.Selected) || manifest.Totals.ShardCount != len(manifest.Shards) ||
		manifest.Totals.RawByteCount != rawTotal || manifest.Totals.EncodedByteCount != encodedTotal ||
		rawTotal > MaxPackRawBytes || encodedTotal > MaxPackEncodedBytes {
		return errors.New("evidence pack totals do not match bounded shard accounting")
	}
	digest, err := manifestDigest(manifest)
	if err != nil || digest != manifest.PackSHA256 {
		return errors.New("evidence pack manifest digest does not match")
	}
	encodedBytes, err := streamCanonicalJSON(manifest, io.Discard)
	if err != nil || encodedBytes == 0 || encodedBytes > MaxManifestBytes {
		return errors.New("evidence pack manifest exceeds its encoded bound")
	}
	return nil
}

// MarshalManifest emits the sole canonical compact manifest encoding.
func MarshalManifest(manifest Manifest) ([]byte, error) {
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > MaxManifestBytes {
		return nil, errors.New("evidence pack manifest exceeds its encoded bound")
	}
	return body, nil
}

// ShardFileName derives the only allowed shard filename from its ordinal and digest.
func ShardFileName(ordinal int, digest string) (string, error) {
	if ordinal < 0 || ordinal >= lyricscontract.MaxPackItems || !canonicalSHA256.MatchString(digest) {
		return "", errors.New("evidence shard file identity is invalid")
	}
	return fmt.Sprintf("shard-%06d-%s.json", ordinal, digest), nil
}

func cloneEvidence(input lyricssource.IndexEvidence) lyricssource.IndexEvidence {
	input.Categories = append([]string(nil), input.Categories...)
	input.Raw = append([]byte(nil), input.Raw...)
	return input
}

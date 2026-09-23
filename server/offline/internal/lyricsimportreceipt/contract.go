// Package lyricsimportreceipt defines the canonical durable import-receipt v5
// contract shared by the operational offline producer and every release gate.
package lyricsimportreceipt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
)

const (
	SchemaVersion       = 5
	CommitProtocol      = "durable-precommit-v1"
	DatabaseAuditAction = "lyrics.import_stage.receipt"
	StateDigestVersion  = "moesekai-sqlite-logical-state-v1"

	MaxReceiptBytes = 4 << 20
	MaxJSONDepth    = 32
	MaxReceiptItems = 100_000
)

var (
	canonicalSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	compactID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

type Artifact struct {
	RenditionKey   string `json:"renditionKey"`
	ArtifactSHA256 string `json:"artifactSha256"`
}

type Item struct {
	MusicID              int        `json:"musicId"`
	Revision             int        `json:"revision"`
	Changed              bool       `json:"changed"`
	DocumentSHA256       string     `json:"documentSha256"`
	FullTextRenditionKey string     `json:"fullTextRenditionKey"`
	SourceFetchedAt      string     `json:"sourceFetchedAt"`
	Artifacts            []Artifact `json:"artifacts"`
}

type Receipt struct {
	SchemaVersion                int    `json:"schemaVersion"`
	CommitProtocol               string `json:"commitProtocol"`
	DatabaseAuditAction          string `json:"databaseAuditAction"`
	ValidationReceiptSHA256      string `json:"validationReceiptSha256"`
	RootManifestSHA256           string `json:"rootManifestSha256"`
	RootID                       string `json:"rootId"`
	RootSHA256                   string `json:"rootSha256"`
	ManifestSchemaVersion        int    `json:"manifestSchemaVersion"`
	ManifestSHA256               string `json:"manifestSha256"`
	BatchSHA256                  string `json:"batchSha256"`
	EvidenceReceiptPath          string `json:"evidenceReceiptPath"`
	EvidenceReceiptSHA256        string `json:"evidenceReceiptSha256"`
	BackupSHA256                 string `json:"backupSha256"`
	StateDigestVersion           string `json:"stateDigestVersion"`
	BackupStateSHA256            string `json:"backupStateSha256"`
	PreImportDatabaseSHA256      string `json:"preImportDatabaseSha256"`
	PreImportDatabaseStateSHA256 string `json:"preImportDatabaseStateSha256"`
	DatabasePath                 string `json:"databasePath"`
	RecoveryDatabasePath         string `json:"recoveryDatabasePath"`
	ReceiptPath                  string `json:"receiptPath"`
	Operator                     string `json:"operator"`
	ImportedCount                int    `json:"importedCount"`
	ReplayedCount                int    `json:"replayedCount"`
	Items                        []Item `json:"items"`
	PreparedAt                   string `json:"preparedAt"`
}

type Audit struct {
	SchemaVersion int    `json:"schemaVersion"`
	ReceiptPath   string `json:"receiptPath"`
	ReceiptSHA256 string `json:"receiptSha256"`
	ReceiptJSON   string `json:"receiptJson"`
}

// Binding is the single validated authority projected from the immutable
// validation receipt, compact root, manifest, and private evidence receipt.
type Binding struct {
	ValidationReceiptSHA256 string
	RootManifestSHA256      string
	RootID                  string
	RootSHA256              string
	ManifestSchemaVersion   int
	ManifestSHA256          string
	BatchSHA256             string
	EvidenceReceiptPath     string
	EvidenceReceiptSHA256   string
	ReceiptPath             string
}

type Metadata struct {
	BackupSHA256                 string
	BackupStateSHA256            string
	PreImportDatabaseSHA256      string
	PreImportDatabaseStateSHA256 string
	DatabasePath                 string
	RecoveryDatabasePath         string
	Operator                     string
	Items                        []Item
	PreparedAt                   string
}

func New(binding Binding, metadata Metadata) (Receipt, error) {
	receipt := Receipt{
		SchemaVersion:                SchemaVersion,
		CommitProtocol:               CommitProtocol,
		DatabaseAuditAction:          DatabaseAuditAction,
		ValidationReceiptSHA256:      binding.ValidationReceiptSHA256,
		RootManifestSHA256:           binding.RootManifestSHA256,
		RootID:                       binding.RootID,
		RootSHA256:                   binding.RootSHA256,
		ManifestSchemaVersion:        binding.ManifestSchemaVersion,
		ManifestSHA256:               binding.ManifestSHA256,
		BatchSHA256:                  binding.BatchSHA256,
		EvidenceReceiptPath:          binding.EvidenceReceiptPath,
		EvidenceReceiptSHA256:        binding.EvidenceReceiptSHA256,
		BackupSHA256:                 metadata.BackupSHA256,
		StateDigestVersion:           StateDigestVersion,
		BackupStateSHA256:            metadata.BackupStateSHA256,
		PreImportDatabaseSHA256:      metadata.PreImportDatabaseSHA256,
		PreImportDatabaseStateSHA256: metadata.PreImportDatabaseStateSHA256,
		DatabasePath:                 metadata.DatabasePath,
		RecoveryDatabasePath:         metadata.RecoveryDatabasePath,
		ReceiptPath:                  binding.ReceiptPath,
		Operator:                     metadata.Operator,
		Items:                        append([]Item(nil), metadata.Items...),
		PreparedAt:                   metadata.PreparedAt,
	}
	for _, item := range receipt.Items {
		if item.Changed {
			receipt.ImportedCount++
		} else {
			receipt.ReplayedCount++
		}
	}
	if err := ValidateBound(receipt, binding); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func BindingFromReceipt(receipt Receipt) Binding {
	return Binding{
		ValidationReceiptSHA256: receipt.ValidationReceiptSHA256,
		RootManifestSHA256:      receipt.RootManifestSHA256,
		RootID:                  receipt.RootID,
		RootSHA256:              receipt.RootSHA256,
		ManifestSchemaVersion:   receipt.ManifestSchemaVersion,
		ManifestSHA256:          receipt.ManifestSHA256,
		BatchSHA256:             receipt.BatchSHA256,
		EvidenceReceiptPath:     receipt.EvidenceReceiptPath,
		EvidenceReceiptSHA256:   receipt.EvidenceReceiptSHA256,
		ReceiptPath:             receipt.ReceiptPath,
	}
}

func ValidateBound(receipt Receipt, binding Binding) error {
	if err := Validate(receipt); err != nil {
		return err
	}
	if BindingFromReceipt(receipt) != binding {
		return errors.New("durable import receipt does not match its single validated release binding")
	}
	return nil
}

func Validate(receipt Receipt) error {
	if receipt.SchemaVersion != SchemaVersion || receipt.CommitProtocol != CommitProtocol ||
		receipt.DatabaseAuditAction != DatabaseAuditAction ||
		!canonicalSHA256.MatchString(receipt.ValidationReceiptSHA256) ||
		!canonicalSHA256.MatchString(receipt.RootManifestSHA256) || !compactID.MatchString(receipt.RootID) ||
		!canonicalSHA256.MatchString(receipt.RootSHA256) || receipt.ManifestSchemaVersion <= 0 ||
		!canonicalSHA256.MatchString(receipt.ManifestSHA256) || !canonicalSHA256.MatchString(receipt.BatchSHA256) ||
		!canonicalAbsolutePath(receipt.EvidenceReceiptPath) || !canonicalSHA256.MatchString(receipt.EvidenceReceiptSHA256) ||
		!canonicalSHA256.MatchString(receipt.BackupSHA256) || receipt.StateDigestVersion != StateDigestVersion ||
		!canonicalSHA256.MatchString(receipt.BackupStateSHA256) ||
		!canonicalSHA256.MatchString(receipt.PreImportDatabaseSHA256) ||
		!canonicalSHA256.MatchString(receipt.PreImportDatabaseStateSHA256) ||
		receipt.BackupStateSHA256 != receipt.PreImportDatabaseStateSHA256 ||
		!canonicalAbsolutePath(receipt.DatabasePath) || !canonicalAbsolutePath(receipt.RecoveryDatabasePath) ||
		receipt.DatabasePath == receipt.RecoveryDatabasePath || !canonicalAbsolutePath(receipt.ReceiptPath) ||
		receipt.Operator == "" || receipt.Operator != strings.TrimSpace(receipt.Operator) || len(receipt.Operator) > 128 ||
		!utf8.ValidString(receipt.Operator) || strings.ContainsAny(receipt.Operator, "\r\n") ||
		receipt.ImportedCount < 0 || receipt.ReplayedCount < 0 || receipt.Items == nil ||
		len(receipt.Items) > MaxReceiptItems || receipt.ImportedCount+receipt.ReplayedCount != len(receipt.Items) {
		return errors.New("durable import receipt does not satisfy the closed schema-v5 contract")
	}
	if _, err := canonicalTimestamp(receipt.PreparedAt); err != nil {
		return fmt.Errorf("durable import receipt preparedAt: %w", err)
	}
	changed := 0
	lastMusicID := 0
	for index, item := range receipt.Items {
		if item.MusicID <= 0 || index > 0 && item.MusicID <= lastMusicID || item.Revision <= 0 ||
			!canonicalSHA256.MatchString(item.DocumentSHA256) || !boundedText(item.FullTextRenditionKey, 256) ||
			item.Artifacts == nil || len(item.Artifacts) > 64 {
			return fmt.Errorf("durable import receipt item %d is invalid", index)
		}
		if item.SourceFetchedAt != "" {
			if _, err := canonicalTimestamp(item.SourceFetchedAt); err != nil {
				return fmt.Errorf("durable import receipt item %d sourceFetchedAt: %w", index, err)
			}
		}
		for artifactIndex, artifact := range item.Artifacts {
			if !boundedText(artifact.RenditionKey, 256) || !canonicalSHA256.MatchString(artifact.ArtifactSHA256) {
				return fmt.Errorf("durable import receipt item %d artifact %d is invalid", index, artifactIndex)
			}
		}
		if item.Changed {
			changed++
		}
		lastMusicID = item.MusicID
	}
	if changed != receipt.ImportedCount {
		return errors.New("durable import receipt changed counters do not match its item set")
	}
	return nil
}

func DecodeCanonical(body []byte) (Receipt, error) {
	var receipt Receipt
	if err := decodeStrictCanonicalJSON(body, &receipt, MaxReceiptBytes, "durable import receipt"); err != nil {
		return Receipt{}, err
	}
	if err := Validate(receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func MarshalCanonical(receipt Receipt) ([]byte, error) {
	if err := Validate(receipt); err != nil {
		return nil, err
	}
	return marshalCanonicalJSON(receipt, MaxReceiptBytes, "durable import receipt")
}

func MarshalBound(receipt Receipt, binding Binding) ([]byte, error) {
	if err := ValidateBound(receipt, binding); err != nil {
		return nil, err
	}
	return marshalCanonicalJSON(receipt, MaxReceiptBytes, "durable import receipt")
}

func decodeStrictCanonicalJSON(body []byte, target any, maximum int, label string) error {
	if len(body) == 0 || target == nil {
		return fmt.Errorf("%s JSON and target are required", label)
	}
	if len(body) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", label, maximum)
	}
	if !utf8.Valid(body) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	if err := validateJSONDepth(body, label); err != nil {
		return err
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return fmt.Errorf("%s JSON: %w", label, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s contains trailing JSON", label)
	}
	canonical, err := marshalCanonicalJSON(target, maximum, label)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, body) {
		return fmt.Errorf("%s is not the canonical producer encoding", label)
	}
	return nil
}

func marshalCanonicalJSON(value any, maximum int, label string) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", label, err)
	}
	body = append(body, '\n')
	if len(body) == 0 || len(body) > maximum || !utf8.Valid(body) {
		return nil, fmt.Errorf("%s exceeds its canonical byte or UTF-8 bound", label)
	}
	return body, nil
}

func validateJSONDepth(body []byte, label string) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 {
				return fmt.Errorf("%s JSON has invalid nesting", label)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect %s JSON depth: %w", label, err)
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
			if depth > MaxJSONDepth {
				return fmt.Errorf("%s JSON exceeds maximum nesting depth %d", label, MaxJSONDepth)
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("%s JSON has invalid nesting", label)
			}
		}
	}
}

func canonicalAbsolutePath(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && utf8.ValidString(value) &&
		filepath.IsAbs(value) && filepath.Clean(value) == value
}

func canonicalTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !strings.HasSuffix(value, "Z") || parsed.UTC().Format(time.RFC3339Nano) != value || parsed.UnixNano() <= 0 {
		return time.Time{}, errors.New("timestamp must be positive canonical UTC RFC3339Nano")
	}
	return parsed, nil
}

func boundedText(value string, maximum int) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= maximum && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

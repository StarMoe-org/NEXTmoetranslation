package lyricsrecoveryimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsrootmanifest"
)

const (
	ImportReceiptSchemaVersion          = 2
	ImportReceiptKind                   = "lyrics-recovery-import-receipt-v2"
	ImportReceiptSchemaVersionV1        = 1
	ImportReceiptKindV1                 = "lyrics-recovery-import-receipt-v1"
	ImportReceiptRuntimeSchemaVersion   = 27
	ImportReceiptAuditAction            = "lyrics.import_recovery"
	ImportReceiptReceiptAuditAction     = "lyrics.import_recovery.receipt"
	ImportReceiptCommitProtocol         = "durable-precommit-v1"
	ImportReceiptStateDigestVersion     = "moesekai-recovery-logical-state-v1"
	ImportReceiptProtectedDigestVersion = "moesekai-recovery-protected-state-v2"
	MaxImportReceiptBytes               = 4 << 20
)

type ImportReceiptItem struct {
	MusicID                    int                          `json:"musicId"`
	State                      lyricscontract.CoverageState `json:"state"`
	Revision                   int                          `json:"revision"`
	DocumentSHA256             string                       `json:"documentSha256,omitempty"`
	AvailabilityDocumentSHA256 string                       `json:"availabilityDocumentSha256,omitempty"`
}

type ImportStorageCounts struct {
	BatchItems             int `json:"batchItems"`
	EditableLyrics         int `json:"editableLyrics"`
	SourceDocuments        int `json:"sourceDocuments"`
	AvailabilityDocuments  int `json:"availabilityDocuments"`
	Artifacts              int `json:"artifacts"`
	EvidenceSelection      int `json:"evidenceSelection"`
	ArtifactEvidenceLinks  int `json:"artifactEvidenceLinks"`
	ComponentContributions int `json:"componentContributions"`
}

type ImportReceipt struct {
	SchemaVersion             int                     `json:"schemaVersion"`
	Kind                      string                  `json:"kind"`
	RuntimeSchemaVersion      int                     `json:"runtimeSchemaVersion"`
	DatabaseAuditAction       string                  `json:"databaseAuditAction"`
	CommitProtocol            string                  `json:"commitProtocol"`
	ReceiptAuditAction        string                  `json:"receiptAuditAction"`
	RootManifestFileSHA256    string                  `json:"rootManifestFileSha256"`
	RootID                    string                  `json:"rootId"`
	RootSHA256                string                  `json:"rootSha256"`
	ImportManifestFileSHA256  string                  `json:"importManifestFileSha256"`
	BatchSHA256               string                  `json:"batchSha256"`
	EvidenceReceiptFileSHA256 string                  `json:"evidenceReceiptFileSha256"`
	EvidenceReceiptSHA256     string                  `json:"evidenceReceiptSha256"`
	PackSHA256                string                  `json:"packSha256"`
	SelectionSHA256           string                  `json:"selectionSha256"`
	CatalogCount              int                     `json:"catalogCount"`
	MusicIDsSHA256            string                  `json:"musicIdsSha256"`
	Coverage                  lyricscontract.Coverage `json:"coverage"`
	BackupSHA256              string                  `json:"backupSha256"`
	StateDigestVersion        string                  `json:"stateDigestVersion"`
	BackupStateSHA256         string                  `json:"backupStateSha256"`
	PreImportDatabaseSHA256   string                  `json:"preImportDatabaseSha256"`
	PreImportStateSHA256      string                  `json:"preImportStateSha256"`
	PostImportStateSHA256     string                  `json:"postImportStateSha256"`
	ProtectedDigestVersion    string                  `json:"protectedDigestVersion"`
	PreImportProtectedSHA256  string                  `json:"preImportProtectedSha256"`
	PostImportProtectedSHA256 string                  `json:"postImportProtectedSha256"`
	AuditBoundaryID           int64                   `json:"auditBoundaryId"`
	DatabasePath              string                  `json:"databasePath"`
	RecoveryDatabasePath      string                  `json:"recoveryDatabasePath"`
	ReceiptPath               string                  `json:"receiptPath"`
	Actor                     string                  `json:"actor"`
	BatchCreatedAt            string                  `json:"batchCreatedAt"`
	PreparedAt                string                  `json:"preparedAt"`
	// CommittedAt is populated only when DecodeImportReceiptCanonical projects
	// a historical v1 receipt. It is never serialized into or accepted for v2.
	CommittedAt   string              `json:"-"`
	Counts        ImportStorageCounts `json:"counts"`
	Items         []ImportReceiptItem `json:"items"`
	ReceiptSHA256 string              `json:"receiptSha256"`
}

type ImportReceiptBinding struct {
	RootManifestFileSHA256    string
	ImportManifestFileSHA256  string
	EvidenceReceiptFileSHA256 string
	BackupSHA256              string
	BackupStateSHA256         string
	PreImportDatabaseSHA256   string
	PreImportStateSHA256      string
	PostImportStateSHA256     string
	PreImportProtectedSHA256  string
	PostImportProtectedSHA256 string
	AuditBoundaryID           int64
	DatabasePath              string
	RecoveryDatabasePath      string
	ReceiptPath               string
	Actor                     string
	BatchCreatedAt            string
	PreparedAt                string
	Counts                    ImportStorageCounts
	Items                     []ImportReceiptItem
}

type importReceiptV1 struct {
	SchemaVersion             int                     `json:"schemaVersion"`
	Kind                      string                  `json:"kind"`
	RuntimeSchemaVersion      int                     `json:"runtimeSchemaVersion"`
	DatabaseAuditAction       string                  `json:"databaseAuditAction"`
	RootManifestFileSHA256    string                  `json:"rootManifestFileSha256"`
	RootID                    string                  `json:"rootId"`
	RootSHA256                string                  `json:"rootSha256"`
	ImportManifestFileSHA256  string                  `json:"importManifestFileSha256"`
	BatchSHA256               string                  `json:"batchSha256"`
	EvidenceReceiptFileSHA256 string                  `json:"evidenceReceiptFileSha256"`
	EvidenceReceiptSHA256     string                  `json:"evidenceReceiptSha256"`
	PackSHA256                string                  `json:"packSha256"`
	SelectionSHA256           string                  `json:"selectionSha256"`
	CatalogCount              int                     `json:"catalogCount"`
	MusicIDsSHA256            string                  `json:"musicIdsSha256"`
	Coverage                  lyricscontract.Coverage `json:"coverage"`
	DatabasePath              string                  `json:"databasePath"`
	ReceiptPath               string                  `json:"receiptPath"`
	Actor                     string                  `json:"actor"`
	CommittedAt               string                  `json:"committedAt"`
	Counts                    ImportStorageCounts     `json:"counts"`
	Items                     []ImportReceiptItem     `json:"items"`
	ReceiptSHA256             string                  `json:"receiptSha256"`
}

func NewImportReceipt(root lyricsrootmanifest.Manifest, manifest Manifest, evidence EvidenceReceipt, binding ImportReceiptBinding) (ImportReceipt, error) {
	receipt := ImportReceipt{
		SchemaVersion:             ImportReceiptSchemaVersion,
		Kind:                      ImportReceiptKind,
		RuntimeSchemaVersion:      ImportReceiptRuntimeSchemaVersion,
		DatabaseAuditAction:       ImportReceiptAuditAction,
		CommitProtocol:            ImportReceiptCommitProtocol,
		ReceiptAuditAction:        ImportReceiptReceiptAuditAction,
		RootManifestFileSHA256:    binding.RootManifestFileSHA256,
		RootID:                    root.RootID,
		RootSHA256:                root.RootSHA256,
		ImportManifestFileSHA256:  binding.ImportManifestFileSHA256,
		BatchSHA256:               manifest.BatchSHA256,
		EvidenceReceiptFileSHA256: binding.EvidenceReceiptFileSHA256,
		EvidenceReceiptSHA256:     evidence.ReceiptSHA256,
		PackSHA256:                evidence.PackSHA256,
		SelectionSHA256:           evidence.SelectionSHA256,
		CatalogCount:              manifest.Root.CatalogCount,
		MusicIDsSHA256:            manifest.Root.MusicIDsSHA256,
		Coverage:                  manifest.Root.Coverage,
		BackupSHA256:              binding.BackupSHA256,
		StateDigestVersion:        ImportReceiptStateDigestVersion,
		BackupStateSHA256:         binding.BackupStateSHA256,
		PreImportDatabaseSHA256:   binding.PreImportDatabaseSHA256,
		PreImportStateSHA256:      binding.PreImportStateSHA256,
		PostImportStateSHA256:     binding.PostImportStateSHA256,
		ProtectedDigestVersion:    ImportReceiptProtectedDigestVersion,
		PreImportProtectedSHA256:  binding.PreImportProtectedSHA256,
		PostImportProtectedSHA256: binding.PostImportProtectedSHA256,
		AuditBoundaryID:           binding.AuditBoundaryID,
		DatabasePath:              binding.DatabasePath,
		RecoveryDatabasePath:      binding.RecoveryDatabasePath,
		ReceiptPath:               binding.ReceiptPath,
		Actor:                     binding.Actor,
		BatchCreatedAt:            binding.BatchCreatedAt,
		PreparedAt:                binding.PreparedAt,
		Counts:                    binding.Counts,
		Items:                     append([]ImportReceiptItem(nil), binding.Items...),
	}
	digest, err := importReceiptDigest(receipt)
	if err != nil {
		return ImportReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	if err := ValidateImportReceiptAgainst(receipt, root, manifest, evidence); err != nil {
		return ImportReceipt{}, err
	}
	return receipt, nil
}

func ValidateImportReceiptAgainst(receipt ImportReceipt, root lyricsrootmanifest.Manifest, manifest Manifest, evidence EvidenceReceipt) error {
	if err := ValidateImportReceipt(receipt); err != nil {
		return err
	}
	if err := lyricsrootmanifest.Validate(root); err != nil {
		return err
	}
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	if err := ValidateEvidenceReceipt(evidence); err != nil {
		return err
	}
	if evidence.RootID != root.RootID || evidence.RootSHA256 != root.RootSHA256 ||
		manifest.Root.RootID != root.RootID || manifest.Root.RootSHA256 != root.RootSHA256 ||
		receipt.RootID != root.RootID || receipt.RootSHA256 != root.RootSHA256 ||
		receipt.BatchSHA256 != manifest.BatchSHA256 || receipt.EvidenceReceiptSHA256 != evidence.ReceiptSHA256 ||
		receipt.PackSHA256 != evidence.PackSHA256 || receipt.SelectionSHA256 != evidence.SelectionSHA256 ||
		receipt.CatalogCount != manifest.Root.CatalogCount || receipt.MusicIDsSHA256 != manifest.Root.MusicIDsSHA256 ||
		receipt.Coverage != manifest.Root.Coverage || len(receipt.Items) != len(manifest.Items) {
		return errors.New("recovery import receipt does not match its root, manifest, or evidence receipt")
	}
	expectedCounts := ExpectedImportStorageCounts(manifest, evidence)
	if receipt.Counts != expectedCounts {
		return errors.New("recovery import receipt storage counts do not match the import manifest")
	}
	for index, item := range receipt.Items {
		manifestItem := manifest.Items[index]
		if item.MusicID != manifestItem.MusicID || item.State != manifestItem.State ||
			item.DocumentSHA256 != recoveryItemDocumentSHA256(manifestItem) ||
			item.AvailabilityDocumentSHA256 != manifestItem.AvailabilityDocumentSHA256 {
			return fmt.Errorf("recovery import receipt item %d drifted from the import manifest", index)
		}
		requiresPositiveRevision := importReceiptItemOwnsEditableLyrics(manifestItem)
		if requiresPositiveRevision && item.Revision <= 0 || !requiresPositiveRevision && item.Revision != 0 {
			return fmt.Errorf("recovery import receipt item %d revision ownership drifted from the import manifest", index)
		}
	}
	return nil
}

func ValidateImportReceipt(receipt ImportReceipt) error {
	if receipt.SchemaVersion != ImportReceiptSchemaVersion || receipt.Kind != ImportReceiptKind ||
		receipt.RuntimeSchemaVersion != ImportReceiptRuntimeSchemaVersion ||
		receipt.DatabaseAuditAction != ImportReceiptAuditAction || receipt.CommitProtocol != ImportReceiptCommitProtocol ||
		receipt.ReceiptAuditAction != ImportReceiptReceiptAuditAction ||
		!canonicalSHA256.MatchString(receipt.RootManifestFileSHA256) || !compactImportReceiptID(receipt.RootID) ||
		!canonicalSHA256.MatchString(receipt.RootSHA256) || !canonicalSHA256.MatchString(receipt.ImportManifestFileSHA256) ||
		!canonicalSHA256.MatchString(receipt.BatchSHA256) || !canonicalSHA256.MatchString(receipt.EvidenceReceiptFileSHA256) ||
		!canonicalSHA256.MatchString(receipt.EvidenceReceiptSHA256) || !canonicalSHA256.MatchString(receipt.PackSHA256) ||
		!canonicalSHA256.MatchString(receipt.SelectionSHA256) || receipt.CatalogCount <= 0 ||
		!canonicalSHA256.MatchString(receipt.MusicIDsSHA256) || !canonicalSHA256.MatchString(receipt.BackupSHA256) ||
		receipt.StateDigestVersion != ImportReceiptStateDigestVersion ||
		!canonicalSHA256.MatchString(receipt.BackupStateSHA256) || !canonicalSHA256.MatchString(receipt.PreImportDatabaseSHA256) ||
		!canonicalSHA256.MatchString(receipt.PreImportStateSHA256) || receipt.BackupStateSHA256 != receipt.PreImportStateSHA256 ||
		!canonicalSHA256.MatchString(receipt.PostImportStateSHA256) ||
		receipt.ProtectedDigestVersion != ImportReceiptProtectedDigestVersion ||
		!canonicalSHA256.MatchString(receipt.PreImportProtectedSHA256) ||
		!canonicalSHA256.MatchString(receipt.PostImportProtectedSHA256) ||
		receipt.PreImportProtectedSHA256 != receipt.PostImportProtectedSHA256 || receipt.AuditBoundaryID < 0 ||
		!canonicalImportReceiptPath(receipt.DatabasePath) || !canonicalImportReceiptPath(receipt.RecoveryDatabasePath) ||
		!canonicalImportReceiptPath(receipt.ReceiptPath) || receipt.DatabasePath == receipt.RecoveryDatabasePath ||
		receipt.DatabasePath == receipt.ReceiptPath || receipt.RecoveryDatabasePath == receipt.ReceiptPath ||
		receipt.Actor == "" || receipt.Actor != strings.TrimSpace(receipt.Actor) || len(receipt.Actor) > 128 ||
		!utf8.ValidString(receipt.Actor) || strings.ContainsAny(receipt.Actor, "\x00\r\n") || receipt.CommittedAt != "" ||
		receipt.Items == nil || len(receipt.Items) != receipt.CatalogCount || len(receipt.Items) > MaxManifestItems ||
		!canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("recovery import receipt envelope is invalid")
	}
	batchCreatedAt, err := canonicalImportReceiptTimestamp(receipt.BatchCreatedAt, "batchCreatedAt")
	if err != nil {
		return err
	}
	preparedAt, err := canonicalImportReceiptTimestamp(receipt.PreparedAt, "preparedAt")
	if err != nil {
		return err
	}
	if preparedAt.Before(batchCreatedAt) {
		return errors.New("recovery import receipt preparedAt precedes batchCreatedAt")
	}
	if !validImportStorageCounts(receipt.Counts, receipt.CatalogCount) {
		return errors.New("recovery import receipt storage counts are invalid")
	}
	if err := validateImportReceiptItems(receipt.Items, receipt.Coverage); err != nil {
		return err
	}
	digest, err := importReceiptDigest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("recovery import receipt digest does not match")
	}
	body, err := json.Marshal(receipt)
	if err != nil || len(body) > MaxImportReceiptBytes {
		return errors.New("recovery import receipt exceeds its byte boundary")
	}
	return nil
}

func validateImportReceiptItems(items []ImportReceiptItem, coverage lyricscontract.Coverage) error {
	states := map[lyricscontract.CoverageState]int{}
	lastMusicID := 0
	for index, item := range items {
		if item.MusicID <= lastMusicID {
			return errors.New("recovery import receipt items are not strictly ordered")
		}
		lastMusicID = item.MusicID
		switch item.State {
		case lyricscontract.CoverageComplete:
			if item.Revision < 0 || !canonicalSHA256.MatchString(item.DocumentSHA256) || item.AvailabilityDocumentSHA256 != "" {
				return fmt.Errorf("recovery import receipt complete item %d is invalid", index)
			}
		case lyricscontract.CoverageGameOnly:
			if item.DocumentSHA256 != "" && item.AvailabilityDocumentSHA256 != "" ||
				item.DocumentSHA256 == "" && item.AvailabilityDocumentSHA256 == "" ||
				item.DocumentSHA256 != "" && !canonicalSHA256.MatchString(item.DocumentSHA256) ||
				item.AvailabilityDocumentSHA256 != "" && !canonicalSHA256.MatchString(item.AvailabilityDocumentSHA256) || item.Revision < 0 {
				return fmt.Errorf("recovery import receipt Game-only item %d is invalid", index)
			}
		case lyricscontract.CoverageSatisfiedNoLyrics, lyricscontract.CoverageAmbiguous,
			lyricscontract.CoverageMissing, lyricscontract.CoverageIncomplete, lyricscontract.CoverageFailed:
			if item.Revision != 0 || item.DocumentSHA256 != "" || !canonicalSHA256.MatchString(item.AvailabilityDocumentSHA256) {
				return fmt.Errorf("recovery import receipt availability item %d is invalid", index)
			}
		default:
			return fmt.Errorf("recovery import receipt item %d has an unsupported state", index)
		}
		states[item.State]++
	}
	if !coverageCountsMatch(coverage, states, len(items)) {
		return errors.New("recovery import receipt coverage does not match its items")
	}
	return nil
}

func importReceiptItemOwnsEditableLyrics(item Item) bool {
	if item.State != lyricscontract.CoverageComplete && item.State != lyricscontract.CoverageGameOnly {
		return false
	}
	if item.Draft == nil {
		return item.State == lyricscontract.CoverageComplete
	}
	return item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3
}

func ExpectedImportStorageCounts(manifest Manifest, evidence EvidenceReceipt) ImportStorageCounts {
	counts := ImportStorageCounts{BatchItems: len(manifest.Items), EvidenceSelection: evidence.EvidenceCount}
	for _, item := range manifest.Items {
		artifacts := item.Artifacts
		if item.Draft != nil {
			counts.SourceDocuments++
			artifacts = item.Draft.Artifacts
			if item.Draft.Document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
				counts.EditableLyrics++
			}
			if item.Draft.Document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
				bindings, _ := model.EnumerateLyricsSourceRenditionComponents(item.Draft.Document.Renditions)
				counts.ComponentContributions += len(bindings)
			} else {
				counts.ComponentContributions += sourceProvenanceComponentCount(item.Draft.Document.Provenance)
			}
		} else if item.Availability != nil && item.State == lyricscontract.CoverageGameOnly {
			counts.ComponentContributions += availabilityProvenanceComponentCount(item.Availability.Provenance)
		} else if item.Availability != nil {
			counts.AvailabilityDocuments++
		}
		if item.Draft == nil && item.Availability != nil && item.State == lyricscontract.CoverageGameOnly {
			counts.AvailabilityDocuments++
		}
		counts.Artifacts += len(artifacts)
		for _, artifact := range artifacts {
			counts.ArtifactEvidenceLinks += len(artifact.Identity.IndexEvidenceRefs)
		}
	}
	return counts
}

func sourceProvenanceComponentCount(provenance model.LyricsSourceComponentProvenance) int {
	count := 2
	if provenance.PerformerSegmentation != nil {
		count++
	}
	if provenance.Ruby != nil {
		count++
	}
	if provenance.GameText != nil {
		count++
	}
	if provenance.GameProjection != nil {
		count++
	}
	return count
}

func availabilityProvenanceComponentCount(provenance model.LyricsAvailabilityComponentProvenance) int {
	count := 2
	if provenance.PerformerSegmentation != nil {
		count++
	}
	if provenance.Ruby != nil {
		count++
	}
	return count
}

func recoveryItemDocumentSHA256(item Item) string {
	if item.Draft == nil {
		return ""
	}
	return item.Draft.DocumentSHA256
}

func MarshalImportReceiptCanonical(receipt ImportReceipt) ([]byte, error) {
	if err := ValidateImportReceipt(receipt); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if len(body) > MaxImportReceiptBytes {
		return nil, errors.New("recovery import receipt exceeds its canonical byte boundary")
	}
	return body, nil
}

// DecodeImportReceiptCanonical accepts the current v2 contract and historical
// canonical v1 receipts. V1 values are validated in place and projected into
// ImportReceipt without inventing v2-only commit or digest evidence.
func DecodeImportReceiptCanonical(body []byte) (ImportReceipt, error) {
	if len(body) == 0 || len(body) > MaxImportReceiptBytes || !utf8.Valid(body) {
		return ImportReceipt{}, errors.New("recovery import receipt bytes are invalid")
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return ImportReceipt{}, err
	}
	var envelope struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ImportReceipt{}, err
	}
	switch envelope.SchemaVersion {
	case ImportReceiptSchemaVersion:
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var receipt ImportReceipt
		if err := decoder.Decode(&receipt); err != nil {
			return ImportReceipt{}, err
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return ImportReceipt{}, errors.New("recovery import receipt contains trailing JSON")
		}
		if err := ValidateImportReceipt(receipt); err != nil {
			return ImportReceipt{}, err
		}
		canonical, err := MarshalImportReceiptCanonical(receipt)
		if err != nil || !bytes.Equal(canonical, body) {
			return ImportReceipt{}, errors.New("recovery import receipt is not canonical JSON")
		}
		return receipt, nil
	case ImportReceiptSchemaVersionV1:
		legacyReceipt, err := decodeImportReceiptV1Canonical(body)
		if err != nil {
			return ImportReceipt{}, err
		}
		return importReceiptFromV1(legacyReceipt), nil
	default:
		return ImportReceipt{}, errors.New("recovery import receipt schema version is unsupported")
	}
}

func decodeImportReceiptV1Canonical(body []byte) (importReceiptV1, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var receipt importReceiptV1
	if err := decoder.Decode(&receipt); err != nil {
		return importReceiptV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return importReceiptV1{}, errors.New("recovery import receipt contains trailing JSON")
	}
	if err := validateImportReceiptV1(receipt); err != nil {
		return importReceiptV1{}, err
	}
	canonical, err := marshalImportReceiptV1Canonical(receipt)
	if err != nil || !bytes.Equal(canonical, body) {
		return importReceiptV1{}, errors.New("recovery import receipt is not canonical JSON")
	}
	return receipt, nil
}

func validateImportReceiptV1(receipt importReceiptV1) error {
	if receipt.SchemaVersion != ImportReceiptSchemaVersionV1 || receipt.Kind != ImportReceiptKindV1 ||
		receipt.RuntimeSchemaVersion != ImportReceiptRuntimeSchemaVersion || receipt.DatabaseAuditAction != ImportReceiptAuditAction ||
		!canonicalSHA256.MatchString(receipt.RootManifestFileSHA256) || !compactImportReceiptID(receipt.RootID) ||
		!canonicalSHA256.MatchString(receipt.RootSHA256) || !canonicalSHA256.MatchString(receipt.ImportManifestFileSHA256) ||
		!canonicalSHA256.MatchString(receipt.BatchSHA256) || !canonicalSHA256.MatchString(receipt.EvidenceReceiptFileSHA256) ||
		!canonicalSHA256.MatchString(receipt.EvidenceReceiptSHA256) || !canonicalSHA256.MatchString(receipt.PackSHA256) ||
		!canonicalSHA256.MatchString(receipt.SelectionSHA256) || receipt.CatalogCount <= 0 ||
		!canonicalSHA256.MatchString(receipt.MusicIDsSHA256) || !canonicalImportReceiptPath(receipt.DatabasePath) ||
		!canonicalImportReceiptPath(receipt.ReceiptPath) || receipt.DatabasePath == receipt.ReceiptPath ||
		receipt.Actor == "" || receipt.Actor != strings.TrimSpace(receipt.Actor) || len(receipt.Actor) > 128 ||
		!utf8.ValidString(receipt.Actor) || strings.ContainsAny(receipt.Actor, "\x00\r\n") ||
		receipt.Items == nil || len(receipt.Items) != receipt.CatalogCount || len(receipt.Items) > MaxManifestItems ||
		!canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("historical recovery import receipt envelope is invalid")
	}
	if _, err := canonicalImportReceiptTimestamp(receipt.CommittedAt, "committedAt"); err != nil {
		return err
	}
	if !validImportStorageCounts(receipt.Counts, receipt.CatalogCount) {
		return errors.New("historical recovery import receipt storage counts are invalid")
	}
	if err := validateImportReceiptItems(receipt.Items, receipt.Coverage); err != nil {
		return err
	}
	digest, err := importReceiptV1Digest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("historical recovery import receipt digest does not match")
	}
	return nil
}

func marshalImportReceiptV1Canonical(receipt importReceiptV1) ([]byte, error) {
	if err := validateImportReceiptV1(receipt); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if len(body) > MaxImportReceiptBytes {
		return nil, errors.New("historical recovery import receipt exceeds its canonical byte boundary")
	}
	return body, nil
}

func importReceiptFromV1(receipt importReceiptV1) ImportReceipt {
	return ImportReceipt{
		SchemaVersion: receipt.SchemaVersion, Kind: receipt.Kind,
		RuntimeSchemaVersion: receipt.RuntimeSchemaVersion, DatabaseAuditAction: receipt.DatabaseAuditAction,
		RootManifestFileSHA256: receipt.RootManifestFileSHA256, RootID: receipt.RootID, RootSHA256: receipt.RootSHA256,
		ImportManifestFileSHA256: receipt.ImportManifestFileSHA256, BatchSHA256: receipt.BatchSHA256,
		EvidenceReceiptFileSHA256: receipt.EvidenceReceiptFileSHA256, EvidenceReceiptSHA256: receipt.EvidenceReceiptSHA256,
		PackSHA256: receipt.PackSHA256, SelectionSHA256: receipt.SelectionSHA256,
		CatalogCount: receipt.CatalogCount, MusicIDsSHA256: receipt.MusicIDsSHA256, Coverage: receipt.Coverage,
		DatabasePath: receipt.DatabasePath, ReceiptPath: receipt.ReceiptPath, Actor: receipt.Actor,
		CommittedAt: receipt.CommittedAt, Counts: receipt.Counts,
		Items: append([]ImportReceiptItem(nil), receipt.Items...), ReceiptSHA256: receipt.ReceiptSHA256,
	}
}

func importReceiptDigest(receipt ImportReceipt) (string, error) {
	receipt.ReceiptSHA256 = ""
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func importReceiptV1Digest(receipt importReceiptV1) (string, error) {
	receipt.ReceiptSHA256 = ""
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalImportReceiptPath(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && utf8.ValidString(value) &&
		filepath.IsAbs(value) && filepath.Clean(value) == value
}

func canonicalImportReceiptTimestamp(value, field string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !strings.HasSuffix(value, "Z") || parsed.UTC().Format(time.RFC3339Nano) != value || parsed.Unix() <= 0 {
		return time.Time{}, fmt.Errorf("recovery import receipt %s is not canonical UTC", field)
	}
	return parsed, nil
}

func compactImportReceiptID(value string) bool {
	return value != "" && len(value) <= 256 && value == strings.TrimSpace(value) && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n ")
}

func validImportStorageCounts(counts ImportStorageCounts, total int) bool {
	if counts.BatchItems != total || counts.EditableLyrics < 0 || counts.EditableLyrics > counts.SourceDocuments ||
		counts.SourceDocuments < 0 || counts.SourceDocuments > total || counts.AvailabilityDocuments != total-counts.SourceDocuments ||
		counts.Artifacts < 0 || counts.Artifacts > total*64 || counts.EvidenceSelection < 0 ||
		counts.ArtifactEvidenceLinks < 0 || counts.ComponentContributions < 0 {
		return false
	}
	if counts.Artifacts == 0 {
		return counts.ArtifactEvidenceLinks == 0 && counts.ComponentContributions == 0
	}
	return counts.EvidenceSelection > 0 && counts.ArtifactEvidenceLinks > 0 && counts.ComponentContributions > 0
}

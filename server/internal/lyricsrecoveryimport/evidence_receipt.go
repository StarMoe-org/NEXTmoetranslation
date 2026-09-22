package lyricsrecoveryimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/lyricsstaging"
)

const (
	EvidenceReceiptSchemaVersion = 1
	MaxEvidenceReceiptBytes      = 32 << 20
)

// EvidenceReceipt is the compact recovery-import binding to an already
// validated, bounded evidence pack. Raw evidence remains in the pack's shards
// and is hydrated only by exact EvidenceRef during private import.
type EvidenceReceipt struct {
	SchemaVersion    int                          `json:"schemaVersion"`
	RootID           string                       `json:"rootId"`
	RootSHA256       string                       `json:"rootSha256"`
	PackSHA256       string                       `json:"packSha256"`
	SelectionSHA256  string                       `json:"selectionSha256"`
	EvidenceCount    int                          `json:"evidenceCount"`
	ShardCount       int                          `json:"shardCount"`
	RawByteCount     int64                        `json:"rawByteCount"`
	EncodedByteCount int64                        `json:"encodedByteCount"`
	Evidence         []lyricscontract.EvidenceRef `json:"evidence"`
	ReceiptSHA256    string                       `json:"receiptSha256"`
}

func NewEvidenceReceipt(
	root lyricsrootmanifest.Manifest,
	manifest Manifest,
	pack lyricsevidencepack.Manifest,
) (EvidenceReceipt, error) {
	if err := lyricsrootmanifest.Validate(root); err != nil {
		return EvidenceReceipt{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return EvidenceReceipt{}, err
	}
	if err := lyricsevidencepack.ValidateManifest(pack); err != nil {
		return EvidenceReceipt{}, err
	}
	if manifest.Root.RootID != root.RootID || manifest.Root.RootSHA256 != root.RootSHA256 ||
		root.EvidencePack.PackSHA256 != pack.PackSHA256 || root.EvidencePack.SelectionSHA256 != pack.SelectionSHA256 ||
		root.EvidencePack.ItemCount != pack.Totals.ItemCount || root.EvidencePack.ShardCount != pack.Totals.ShardCount ||
		root.EvidencePack.RawByteCount != pack.Totals.RawByteCount ||
		root.EvidencePack.EncodedByteCount != pack.Totals.EncodedByteCount {
		return EvidenceReceipt{}, errors.New("recovery import evidence pack does not match the compact root")
	}
	if err := validateManifestEvidenceUnion(manifest, pack.Selected); err != nil {
		return EvidenceReceipt{}, err
	}
	receipt := EvidenceReceipt{
		SchemaVersion: EvidenceReceiptSchemaVersion,
		RootID:        root.RootID, RootSHA256: root.RootSHA256,
		PackSHA256: pack.PackSHA256, SelectionSHA256: pack.SelectionSHA256,
		EvidenceCount: pack.Totals.ItemCount, ShardCount: pack.Totals.ShardCount,
		RawByteCount: pack.Totals.RawByteCount, EncodedByteCount: pack.Totals.EncodedByteCount,
		Evidence: append([]lyricscontract.EvidenceRef{}, pack.Selected...),
	}
	digest, err := evidenceReceiptDigest(receipt)
	if err != nil {
		return EvidenceReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	if err := ValidateEvidenceReceipt(receipt); err != nil {
		return EvidenceReceipt{}, err
	}
	return cloneEvidenceReceipt(receipt), nil
}

func ValidateEvidenceReceipt(receipt EvidenceReceipt) error {
	if receipt.SchemaVersion != EvidenceReceiptSchemaVersion || receipt.RootID == "" ||
		!canonicalSHA256.MatchString(receipt.RootSHA256) || !canonicalSHA256.MatchString(receipt.PackSHA256) ||
		!canonicalSHA256.MatchString(receipt.SelectionSHA256) || receipt.Evidence == nil ||
		receipt.EvidenceCount != len(receipt.Evidence) || receipt.EvidenceCount < 0 ||
		receipt.EvidenceCount > lyricscontract.MaxPackItems || receipt.ShardCount < 0 ||
		receipt.RawByteCount < 0 || receipt.RawByteCount > lyricsevidencepack.MaxPackRawBytes ||
		receipt.EncodedByteCount < 0 || receipt.EncodedByteCount > lyricsevidencepack.MaxPackEncodedBytes ||
		!canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("recovery import evidence receipt envelope is invalid")
	}
	selectionSHA, err := lyricscontract.OrderedSelectionSHA256(receipt.Evidence)
	if err != nil || selectionSHA != receipt.SelectionSHA256 {
		return errors.New("recovery import evidence receipt selection digest does not match")
	}
	if receipt.EvidenceCount == 0 {
		if receipt.ShardCount != 0 || receipt.RawByteCount != 0 || receipt.EncodedByteCount != 0 {
			return errors.New("empty recovery import evidence receipt has non-empty pack accounting")
		}
	} else if receipt.ShardCount <= 0 || receipt.RawByteCount <= 0 || receipt.EncodedByteCount <= 0 {
		return errors.New("recovery import evidence receipt pack accounting is incomplete")
	}
	digest, err := evidenceReceiptDigest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("recovery import evidence receipt digest does not match")
	}
	body, err := json.Marshal(receipt)
	if err != nil || len(body) == 0 || len(body) > MaxEvidenceReceiptBytes || !utf8.Valid(body) {
		return errors.New("recovery import evidence receipt exceeds its byte boundary")
	}
	return nil
}

func ValidateEvidenceReceiptAgainst(
	receipt EvidenceReceipt,
	root lyricsrootmanifest.Manifest,
	manifest Manifest,
	pack lyricsevidencepack.Manifest,
) error {
	if err := ValidateEvidenceReceipt(receipt); err != nil {
		return err
	}
	expected, err := NewEvidenceReceipt(root, manifest, pack)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(receipt, expected) {
		return errors.New("recovery import evidence receipt does not match the exact root, manifest, and evidence pack")
	}
	return nil
}

func validateManifestEvidenceUnion(manifest Manifest, selected []lyricscontract.EvidenceRef) error {
	selectedByID := make(map[string]lyricscontract.EvidenceRef, len(selected))
	for _, reference := range selected {
		selectedByID[reference.EvidenceID] = reference
	}
	used := make(map[string]bool, len(selected))
	for _, item := range manifest.Items {
		artifacts := item.Artifacts
		if item.Draft != nil {
			artifacts = item.Draft.Artifacts
		}
		for _, artifact := range artifacts {
			if err := lyricsstaging.ValidateRecoveryArtifact(artifact); err != nil {
				return fmt.Errorf("music %d evidence artifact: %w", item.MusicID, err)
			}
			for _, compact := range artifact.Identity.IndexEvidenceRefs {
				reference, found := selectedByID[compact.EvidenceID]
				if !found || reference.Provider != artifact.Identity.Provider || reference.SHA256 != compact.SHA256 {
					return fmt.Errorf("music %d artifact evidence is outside the exact evidence pack", item.MusicID)
				}
				used[reference.EvidenceID] = true
			}
		}
	}
	if len(used) != len(selected) {
		return errors.New("recovery import artifacts do not consume the exact evidence-pack selection")
	}
	return nil
}

func evidenceReceiptDigest(receipt EvidenceReceipt) (string, error) {
	receipt.ReceiptSHA256 = ""
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-recovery-import-evidence-receipt-v1\x00"))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func MarshalEvidenceReceipt(receipt EvidenceReceipt) ([]byte, error) {
	if err := ValidateEvidenceReceipt(receipt); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if len(body) > MaxEvidenceReceiptBytes {
		return nil, errors.New("recovery import evidence receipt exceeds its encoded byte boundary")
	}
	return body, nil
}

func DecodeEvidenceReceipt(body []byte) (EvidenceReceipt, error) {
	if len(body) == 0 || len(body) > MaxEvidenceReceiptBytes || !utf8.Valid(body) {
		return EvidenceReceipt{}, errors.New("recovery import evidence receipt bytes are invalid")
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return EvidenceReceipt{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var receipt EvidenceReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return EvidenceReceipt{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return EvidenceReceipt{}, errors.New("recovery import evidence receipt contains trailing JSON")
	}
	if err := ValidateEvidenceReceipt(receipt); err != nil {
		return EvidenceReceipt{}, err
	}
	canonical, err := MarshalEvidenceReceipt(receipt)
	if err != nil || !bytes.Equal(canonical, body) {
		return EvidenceReceipt{}, errors.New("recovery import evidence receipt is not canonical JSON")
	}
	return cloneEvidenceReceipt(receipt), nil
}

func cloneEvidenceReceipt(receipt EvidenceReceipt) EvidenceReceipt {
	receipt.Evidence = append([]lyricscontract.EvidenceRef{}, receipt.Evidence...)
	return receipt
}

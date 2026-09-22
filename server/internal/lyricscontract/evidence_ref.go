package lyricscontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"moesekai/server/internal/model"
)

const MaxPackItems = 64 << 10

var (
	canonicalSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	canonicalEvidence = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

// EvidenceRef is the compact exact acquisition/evidence identity used by roots
// and shard manifests. SHA256 binds the evidence raw projection while
// EnvelopeSHA256 binds the canonical evidence envelope retained by the pack.
type EvidenceRef struct {
	Provider       model.LyricsSourceProvider `json:"provider"`
	AcquisitionID  string                     `json:"acquisitionId"`
	EvidenceID     string                     `json:"evidenceId"`
	SHA256         string                     `json:"sha256"`
	EnvelopeSHA256 string                     `json:"envelopeSha256"`
}

func ValidateEvidenceRef(ref EvidenceRef) error {
	if !model.IsValidLyricsSourceProvider(ref.Provider) || !canonicalSHA256.MatchString(ref.AcquisitionID) ||
		!canonicalEvidence.MatchString(ref.EvidenceID) || !canonicalSHA256.MatchString(ref.SHA256) ||
		!canonicalSHA256.MatchString(ref.EnvelopeSHA256) {
		return errors.New("exact acquisition/evidence reference is invalid")
	}
	return nil
}

func ValidateOrderedSelection(selected []EvidenceRef) error {
	if selected == nil || len(selected) > MaxPackItems {
		return errors.New("selected evidence must be an explicit bounded array")
	}
	acquisitions := make(map[string]EvidenceRef, len(selected))
	for index, ref := range selected {
		if err := ValidateEvidenceRef(ref); err != nil {
			return err
		}
		if index > 0 && selected[index-1].EvidenceID >= ref.EvidenceID {
			if selected[index-1].EvidenceID == ref.EvidenceID && selected[index-1] == ref {
				return errors.New("selected evidence contains a duplicate identity")
			}
			return errors.New("selected evidence contains a conflicting or unordered identity")
		}
		if previous, exists := acquisitions[ref.AcquisitionID]; exists && previous != ref {
			return errors.New("one acquisition ID resolves to conflicting selected evidence")
		}
		acquisitions[ref.AcquisitionID] = ref
	}
	return nil
}

// OrderedSelectionSHA256 validates and hashes an already ordered unique exact
// selection in one pass without copying the reference slice.
func OrderedSelectionSHA256(selected []EvidenceRef) (string, error) {
	if err := ValidateOrderedSelection(selected); err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-evidence-selection-v1\x00["))
	for index, ref := range selected {
		if index > 0 {
			_, _ = digest.Write([]byte{','})
		}
		body, err := json.Marshal(ref)
		if err != nil {
			return "", err
		}
		_, _ = digest.Write(body)
	}
	_, _ = digest.Write([]byte{']'})
	return hex.EncodeToString(digest.Sum(nil)), nil
}

package lyricscontract

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

const (
	SchemaVersionV2 = 2

	MaxCatalogRecordCount = 10_000
)

type CoverageState string

const (
	CoverageComplete          CoverageState = "complete"
	CoverageGameOnly          CoverageState = "game_only"
	CoverageSatisfiedNoLyrics CoverageState = "satisfied_no_lyrics"
	CoverageCatalogReview     CoverageState = "catalog_review"
	CoverageGameSizeEvidence  CoverageState = "game_size_evidence"
	CoverageAmbiguous         CoverageState = "ambiguous"
	CoverageMissing           CoverageState = "missing"
	CoverageIncomplete        CoverageState = "incomplete"
	CoverageFailed            CoverageState = "failed"
)

// Coverage contains only counters derived from the ordered song refs.
type Coverage struct {
	Total                   int `json:"total"`
	Complete                int `json:"complete"`
	GameOnly                int `json:"gameOnly,omitempty"`
	SatisfiedNoLyrics       int `json:"satisfiedNoLyrics,omitempty"`
	CatalogReview           int `json:"catalogReview"`
	GameSizeEvidence        int `json:"gameSizeEvidence"`
	Ambiguous               int `json:"ambiguous"`
	Missing                 int `json:"missing"`
	Incomplete              int `json:"incomplete"`
	Failed                  int `json:"failed"`
	ProviderOutcomeRefCount int `json:"providerOutcomeRefCount"`
	SelectionRefCount       int `json:"selectionRefCount"`
	UniqueAcquisitionCount  int `json:"uniqueAcquisitionCount"`
	UniqueEvidenceCount     int `json:"uniqueEvidenceCount"`
}

// OrderedMusicIDsSHA256 returns the domain-separated digest of one positive,
// strictly increasing, unique, bounded catalog music-ID sequence.
func OrderedMusicIDsSHA256(musicIDs []int) (string, error) {
	if len(musicIDs) == 0 || len(musicIDs) > MaxCatalogRecordCount {
		return "", errors.New("catalog ordered music IDs must have a positive bounded count")
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("moesekai-lyrics-root-catalog-ordered-music-ids-v1\x00"))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(musicIDs)))
	_, _ = digest.Write(encoded[:])
	lastMusicID := 0
	for index, musicID := range musicIDs {
		if musicID <= 0 || index > 0 && musicID <= lastMusicID {
			return "", errors.New("catalog ordered music IDs must be positive, strictly increasing, and unique")
		}
		binary.BigEndian.PutUint64(encoded[:], uint64(musicID))
		_, _ = digest.Write(encoded[:])
		lastMusicID = musicID
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

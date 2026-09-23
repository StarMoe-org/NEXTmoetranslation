package lyricsrecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"moesekai/server/offline/internal/lyricsreview"
)

// ReviewObservation converts the private replay boundary into the content-free
// facts that the manual-review resolver is allowed to inspect. No lyric text
// crosses this adapter.
func ReviewObservation(replay ReplayResult, result SongResult) (lyricsreview.ResultObservation, []lyricsreview.OutcomeObservation) {
	observation := lyricsreview.ResultObservation{
		MusicID:           result.MusicID,
		State:             string(result.State),
		HasFull:           result.Full != nil,
		HasGame:           result.Game != nil,
		HasGameProjection: result.GameProjection != nil,
	}
	for _, alternate := range result.AlternateVocals {
		kind := ""
		if alternate.Full != nil {
			kind = alternate.Full.Version.Kind
		} else if alternate.Game != nil {
			kind = alternate.Game.Version.Kind
		}
		if kind == "another" || strings.Contains(strings.ToLower(alternate.TabLabel), "another") {
			observation.AnotherCount++
		} else {
			observation.AlternateCount++
		}
	}
	outcomes := make([]lyricsreview.OutcomeObservation, len(replay.Providers))
	for index, provider := range replay.Providers {
		outcome := lyricsreview.OutcomeObservation{
			Provider:       provider.Artifact.Provider,
			OutcomeID:      provider.Artifact.OutcomeID,
			ArtifactSHA256: provider.Artifact.ArtifactSHA256,
			Acquisitions:   make([]lyricsreview.AcquisitionObservation, len(provider.Artifact.Acquisitions)),
		}
		for acquisitionIndex, acquisition := range provider.Artifact.Acquisitions {
			outcome.Acquisitions[acquisitionIndex] = lyricsreview.AcquisitionObservation{
				AcquisitionID:  acquisition.AcquisitionID,
				EvidenceID:     acquisition.EvidenceID,
				SHA256:         acquisition.SHA256,
				EnvelopeSHA256: acquisition.EnvelopeSHA256,
			}
		}
		if candidate := provider.Artifact.Candidate; candidate != nil {
			candidateObservation := &lyricsreview.CandidateObservation{
				PageID:     candidate.PageID,
				RevisionID: candidate.RevisionID,
				SHA1:       candidate.SHA1,
			}
			if provider.Fixed != nil {
				digest := sha256.Sum256(provider.Fixed.Wikitext)
				candidateObservation.ContentSHA256 = hex.EncodeToString(digest[:])
			}
			outcome.Candidate = candidateObservation
		}
		outcomes[index] = outcome
	}
	return observation, outcomes
}

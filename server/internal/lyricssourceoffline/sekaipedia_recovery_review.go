// Package lyricssourceoffline holds the recovery orchestration that depends on
// the offline manual-review contract.
package lyricssourceoffline

import (
	"strings"

	"moesekai/server/internal/lyricsreview"
	"moesekai/server/internal/lyricssource"
)

// RecoverSekaipediaProjectionWithReview applies the same content-free manual
// review resolver used by exact replay before the reparsed projection crosses
// into the recovery-import boundary.
func RecoverSekaipediaProjectionWithReview(
	raw []byte,
	expected lyricssource.FixedIndex,
	policy lyricssource.PerformerSegmentationPolicy,
	musicID int,
	resolver *lyricsreview.Resolver,
) (lyricssource.SekaipediaRecoveryProjection, error) {
	projection, err := lyricssource.RecoverSekaipediaProjection(raw, expected, policy)
	if err != nil {
		return lyricssource.SekaipediaRecoveryProjection{}, err
	}
	if resolver != nil {
		observation := lyricsreview.ProjectionObservation{
			RevisionObservation: lyricsreview.RevisionObservation{
				Provider:       lyricssource.ProviderSekaipedia,
				PageID:         expected.PageID,
				RevisionID:     expected.RevisionID,
				Title:          expected.Title,
				SHA1:           expected.SHA1,
				ContentSHA256:  expected.ContentSHA256,
				ResponseSHA256: expected.RawSHA256,
			},
			HasFull:           len(projection.Full.Lines) != 0,
			HasGame:           projection.Game != nil,
			HasGameProjection: projection.GameProjection != nil,
		}
		for _, alternate := range projection.AlternateVocals {
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
		if err := resolver.ValidateProjection(musicID, observation); err != nil {
			return lyricssource.SekaipediaRecoveryProjection{}, err
		}
	}
	return projection, nil
}

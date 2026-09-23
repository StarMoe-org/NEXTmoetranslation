package lyricsrecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsoutcomeartifact"
)

func TestReviewObservationUsesWikitextContentDigest(t *testing.T) {
	wikitext := []byte("{{Infobox song}}\n== Lyrics ==\nreview adapter fixture")
	digest := sha256.Sum256(wikitext)
	replay := ReplayResult{
		MusicID: 27,
		Providers: []ProviderReplay{{
			Artifact: lyricsoutcomeartifact.Artifact{
				Provider:       model.LyricsSourceProviderSekaipedia,
				OutcomeID:      "outcome:sekaipedia:27:fixture",
				ArtifactSHA256: strings.Repeat("a", 64),
				Candidate: &lyricsoutcomeartifact.CandidateIdentity{
					PageID: 390, RevisionID: 328683,
					SHA1: strings.Repeat("b", 40), RawSHA256: strings.Repeat("c", 64),
				},
			},
			Fixed: &lyricssource.FixedRevision{Wikitext: wikitext},
		}},
	}
	_, outcomes := ReviewObservation(replay, SongResult{MusicID: 27})
	if len(outcomes) != 1 || outcomes[0].Candidate == nil {
		t.Fatalf("review outcome observation = %+v", outcomes)
	}
	if got := outcomes[0].Candidate.ContentSHA256; got != hex.EncodeToString(digest[:]) {
		t.Fatalf("content SHA-256 = %q", got)
	}
	if outcomes[0].Candidate.ContentSHA256 == replay.Providers[0].Artifact.Candidate.RawSHA256 {
		t.Fatal("review adapter confused the MediaWiki response digest with the wikitext content digest")
	}
}

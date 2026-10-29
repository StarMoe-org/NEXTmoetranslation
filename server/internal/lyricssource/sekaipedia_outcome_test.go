package lyricssource

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/model"

	_ "moesekai/server/internal/sqlitedriver"
)

const immutableLyricsCatalogPath = "/private/tmp/moesekai-lyrics-catalog-v18-20260731-704.db"

func TestSekaipediaOfflineOutcomeCanaryUsesImmutableCatalogIdentities(t *testing.T) {
	requireImmutableLyricsCatalog(t)
	for _, test := range []struct {
		name            string
		musicID         int
		policy          PerformerSegmentationPolicy
		wantTitle       string
		wantProducer    string
		wantLyricist    string
		wantComposer    string
		wantArranger    string
		wantFullLines   int
		wantGameLines   int
		forbiddenLyric  string
		forbiddenRomaji string
	}{
		{
			name: "Roki Japanese catalog credits", musicID: 2, policy: PerformerSegmentationSekaiEligible,
			wantTitle: "ロキ", wantProducer: "みきとP | みきとP | みきとP",
			wantLyricist: "みきとP", wantComposer: "みきとP", wantArranger: "みきとP",
			wantFullLines: 64, wantGameLines: 25,
			forbiddenLyric: "さあ 眠眠打破", forbiddenRomaji: "saa minmin daha",
		},
		{
			name: "Journey catalog identity", musicID: 235, policy: PerformerSegmentationSekaiEligible,
			wantTitle: "Journey", wantProducer: "DECO*27 | DECO*27 | Rockwell",
			wantLyricist: "DECO*27", wantComposer: "DECO*27", wantArranger: "Rockwell",
			forbiddenLyric: "溜めてきた", forbiddenRomaji: "tamete kita",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := immutableCatalogMusicIdentity(t, test.musicID, test.policy)
			if identity.JapaneseTitle != test.wantTitle || identity.ProducerMetadata != test.wantProducer ||
				identity.Lyricist != test.wantLyricist || identity.Composer != test.wantComposer ||
				identity.Arranger != test.wantArranger {
				t.Fatalf("immutable catalog identity = %+v", identity)
			}
			fixture := newSekaipediaFixtureServer(t)
			defer fixture.Close()
			provider := fixture.Provider(t)
			if test.musicID == 2 {
				config := provider.config
				config.ContributorAliases = []ProviderContributorAlias{{
					MusicID: 2, CatalogContributor: "みきとP", ProviderContributor: "Mikito-P",
				}}
				provider = newSekaipediaProvider(config, provider.client)
				if got := sekaipediaProviderCreditAliases(
					identity.MusicID, identity.ProducerMetadata, provider.config.ContributorAliases,
				); got != "Mikito-P" {
					t.Fatalf("Roki Japanese producer aliases = %q", got)
				}
				for role, value := range map[string]string{
					"lyricist": identity.Lyricist, "composer": identity.Composer, "arranger": identity.Arranger,
				} {
					if got := sekaipediaProviderCreditAliases(
						identity.MusicID, value, provider.config.ContributorAliases,
					); got != "Mikito-P" {
						t.Fatalf("Roki Japanese %s alias = %q", role, got)
					}
				}
			}
			outcome, err := provider.SearchOutcome(context.Background(), identity)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Status != lyricsprovideroutcome.StatusCandidate || len(outcome.Candidates) != 1 ||
				outcome.Diagnostic.Provider != ProviderSekaipedia ||
				outcome.Diagnostic.Phase != lyricsprovideroutcome.PhaseFinalize ||
				outcome.Diagnostic.ReasonCode != lyricsprovideroutcome.ReasonCandidate ||
				outcome.Diagnostic.Counts.Acquisitions != 2 || outcome.Diagnostic.Counts.Candidates != 1 ||
				len(outcome.Diagnostic.AcquisitionRefs) != 2 {
				t.Fatalf("Sekaipedia canary outcome = %+v", outcome)
			}
			assertProviderDiagnosticContentFree(
				t, outcome.Diagnostic, test.wantTitle, test.forbiddenLyric, test.forbiddenRomaji,
			)
			candidate := outcome.Candidates[0]
			if len(candidate.IndexEvidenceRefs) != 2 || len(candidate.IndexEvidence) != 2 ||
				!isFixedSekaipediaAuthorityEvidence(candidate.IndexEvidence[0], historicalSekaipediaAuthority()) ||
				candidate.IndexEvidence[1].PageID != candidate.PageID ||
				candidate.IndexEvidence[1].RevisionID != candidate.RevisionID ||
				!sameEvidenceRefSet(outcome.Diagnostic.AcquisitionRefs, candidate.IndexEvidenceRefs) {
				t.Fatalf("List/song evidence candidate=%+v diagnostic=%+v", candidate, outcome.Diagnostic)
			}

			fixed, err := provider.FetchFixedCandidateRevision(context.Background(), identity, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if fixed.Document == nil || len(fixed.Document.Full.Lines) == 0 || fixed.Document.GameProjection == nil ||
				len(fixed.Wikitext) == 0 ||
				!bytes.Equal(fixed.Wikitext, SekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)) {
				t.Fatalf("Sekaipedia Full/Game boundary = %+v", fixed)
			}
			if test.wantFullLines > 0 && len(fixed.Document.Full.Lines) != test.wantFullLines {
				t.Fatalf("Full lines=%d want=%d", len(fixed.Document.Full.Lines), test.wantFullLines)
			}
			if test.wantGameLines > 0 && len(fixed.Document.GameProjection.LineIDs) != test.wantGameLines {
				t.Fatalf("Game lines=%d want=%d", len(fixed.Document.GameProjection.LineIDs), test.wantGameLines)
			}
			if test.musicID == 235 && len(fixed.Document.GameProjection.LineIDs) != len(fixed.Document.Full.Lines) {
				t.Fatalf("Journey Game identity projection=%d Full=%d",
					len(fixed.Document.GameProjection.LineIDs), len(fixed.Document.Full.Lines))
			}
			assertSekaipediaNoRomajiBoundary(t, fixed, candidate, outcome.Diagnostic, test.forbiddenRomaji)
			if fixture.ListRequests() != 1 || fixture.TitleRequests() != 1 || fixture.Requests() != 3 {
				t.Fatalf("offline canary requests total/list/title=%d/%d/%d",
					fixture.Requests(), fixture.ListRequests(), fixture.TitleRequests())
			}
		})
	}
}

func TestSekaipediaOfflineOutcomeFallbackControl(t *testing.T) {
	requireImmutableLyricsCatalog(t)
	identity := immutableCatalogMusicIdentity(t, 2, PerformerSegmentationSekaiEligible)
	identity.MusicID = 999
	fixture := newSekaipediaFixtureServer(t)
	defer fixture.Close()
	provider := fixture.Provider(t)
	fandom := &outcomeStubProvider{id: ProviderVocaloidFandom, candidates: []Candidate{{
		Provider: ProviderVocaloidFandom, PageID: 9,
	}}}
	registry, err := newRegistryWithProviders(fandom, provider)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := registry.SearchOutcomes(context.Background(), identity)
	if err != nil || len(outcomes) != 2 {
		t.Fatalf("fallback outcomes=%+v err=%v", outcomes, err)
	}
	if outcomes[0].Provider != ProviderSekaipedia || outcomes[0].Status != lyricsprovideroutcome.StatusNoMatch ||
		outcomes[0].Diagnostic.ReasonCode != lyricsprovideroutcome.ReasonNoSearchHits ||
		outcomes[0].Diagnostic.Phase != lyricsprovideroutcome.PhaseResolveTargets ||
		outcomes[0].Diagnostic.Counts.Acquisitions != 1 || outcomes[0].Diagnostic.Counts.Targets != 0 ||
		outcomes[0].Diagnostic.Counts.Evaluated != 0 || outcomes[0].Diagnostic.Counts.NoMatch != 1 ||
		len(outcomes[0].Diagnostic.AcquisitionRefs) != 1 || len(outcomes[0].Candidates) != 0 ||
		outcomes[1].Provider != ProviderVocaloidFandom || outcomes[1].Status != lyricsprovideroutcome.StatusCandidate ||
		len(outcomes[1].Candidates) != 1 || fandom.searchCalls != 1 {
		t.Fatalf("offline fallback control = %+v fandomCalls=%d", outcomes, fandom.searchCalls)
	}
	if fixture.ListRequests() != 1 || fixture.TitleRequests() != 0 || fixture.Requests() != 1 {
		t.Fatalf("fallback control requests total/list/title=%d/%d/%d",
			fixture.Requests(), fixture.ListRequests(), fixture.TitleRequests())
	}
}

func requireImmutableLyricsCatalog(t *testing.T) {
	t.Helper()
	info, err := os.Stat(immutableLyricsCatalogPath)
	if err != nil {
		t.Skipf("immutable lyrics catalog is unavailable: %v", err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("lyrics catalog must be externally read-only: mode=%s", info.Mode())
	}
}

func immutableCatalogMusicIdentity(
	t *testing.T,
	musicID int,
	policy PerformerSegmentationPolicy,
) MusicIdentity {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+immutableLyricsCatalogPath+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	var queryOnly int
	if err := database.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("immutable catalog query_only=%d err=%v", queryOnly, err)
	}
	var identity MusicIdentity
	err = database.QueryRow(`SELECT music_id, title_ja, producer_metadata, lyricist, composer, arranger
		FROM catalog_music WHERE music_id=?`, musicID).Scan(
		&identity.MusicID, &identity.JapaneseTitle, &identity.ProducerMetadata,
		&identity.Lyricist, &identity.Composer, &identity.Arranger,
	)
	if err != nil {
		t.Fatal(err)
	}
	identity.PerformerSegmentationPolicy = policy
	return identity
}

func sameEvidenceRefSet(left, right []model.LyricsSourceIndexEvidenceRef) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[model.LyricsSourceIndexEvidenceRef]int, len(left))
	for _, reference := range left {
		seen[reference]++
	}
	for _, reference := range right {
		seen[reference]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

func assertSekaipediaNoRomajiBoundary(
	t *testing.T,
	fixed FixedRevision,
	candidate Candidate,
	diagnostic lyricsprovideroutcome.Diagnostic,
	forbiddenRomaji string,
) {
	t.Helper()
	values := map[string]any{
		"candidate":        candidate,
		"diagnostic":       diagnostic,
		"fixed document":   fixed.Document,
		"fixed extraction": fixed.Extraction,
	}
	for name, value := range values {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		lower := bytes.ToLower(body)
		if bytes.Contains(lower, []byte(strings.ToLower(forbiddenRomaji))) ||
			bytes.Contains(lower, []byte(`"romaji"`)) || bytes.Contains(lower, []byte(`"romanized"`)) {
			t.Fatalf("%s crossed the no-romaji boundary: %s", name, body)
		}
	}
	lowerWikitext := bytes.ToLower(fixed.Wikitext)
	if bytes.Contains(lowerWikitext, []byte(strings.ToLower(forbiddenRomaji))) ||
		bytes.Contains(lowerWikitext, []byte("romaji")) {
		t.Fatalf("selected Japanese bytes crossed the no-romaji boundary: %q", fixed.Wikitext)
	}
}

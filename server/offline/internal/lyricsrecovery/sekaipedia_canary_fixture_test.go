package lyricsrecovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

type sekaipediaCanaryFixture struct {
	plan       lyricsextractionplan.RecoveryPlan
	runtime    RuntimeConfig
	ledger     *lyricsacquisition.Ledger
	transports map[model.LyricsSourceProvider]http.RoundTripper
	identities map[int]lyricssource.MusicIdentity
	songs      map[int]SekaipediaCanarySongPlan
}

func newSekaipediaCanaryFixture(t *testing.T, liveCanaryMusicIDs []int) sekaipediaCanaryFixture {
	t.Helper()
	ctx := t.Context()
	root := sekaipediaCanaryPrivateRoot(t)
	ledger, err := lyricsacquisition.CreateLedger(ctx, filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })

	binding, parentMusicIDs := sekaipediaCanaryCatalogBinding(t, root)
	parent := fixtureParentRoot(t, ctx, root, ledger, binding, parentMusicIDs)
	plan := fixtureRecoveryPlan(t, root, binding, parent)
	plan.SourceSnapshot.Algorithm = lyricsextractionplan.RecoverySourceSnapshotAlgorithmV2
	plan.SourceSnapshot.SHA256, err = lyricsextractionplan.RecoverySourceSnapshotSHA256(plan.SourceSnapshot.Files)
	if err != nil {
		t.Fatal(err)
	}
	plan.Execution.LiveCanaryMusicIDs = append([]int(nil), liveCanaryMusicIDs...)

	identities := map[int]lyricssource.MusicIdentity{
		2: {
			MusicID: 2, JapaneseTitle: "ロキ", ProducerMetadata: "MikitoP",
			Lyricist: "MikitoP", Composer: "MikitoP", Arranger: "MikitoP",
			PerformerSegmentationPolicy: lyricssource.PerformerSegmentationSekaiEligible,
		},
		235: {
			MusicID: 235, JapaneseTitle: "Journey", ProducerMetadata: "DECO*27",
			Lyricist: "DECO*27", Composer: "DECO*27", Arranger: "Rockwell",
			PerformerSegmentationPolicy: lyricssource.PerformerSegmentationDisabled,
		},
	}
	allSongs := map[int]SekaipediaCanarySongPlan{
		2:   sekaipediaCanarySongFromFixture(t, 2, "ロキ", "sekaipedia-roki-330574.json"),
		235: sekaipediaCanarySongFromFixture(t, 235, "Journey", "sekaipedia-journey-326737.json"),
	}
	boundSongs := make([]SekaipediaCanarySongPlan, len(liveCanaryMusicIDs))
	for index, musicID := range liveCanaryMusicIDs {
		song, found := allSongs[musicID]
		if !found {
			t.Fatalf("missing local Sekaipedia canary fixture plan for music %d", musicID)
		}
		boundSongs[index] = song
	}
	authority := plan.Providers.Configurations[0].Authorities[0]
	plan.SekaipediaCanary = &lyricsextractionplan.RecoverySekaipediaCanaryPlan{
		List: lyricsextractionplan.RecoverySekaipediaCanaryRevision{
			AcquisitionID: lyricsextractionplan.HistoricalSekaipediaListAcquisitionID,
			PageID:        authority.PageID, RevisionID: authority.RevisionID,
			RevisionTimestamp: authority.RevisionTimestamp, SHA1: authority.SHA1,
			ContentSHA256: authority.ContentSHA256, RawResponseSHA256: authority.RawSHA256,
		},
		Songs: append([]lyricsextractionplan.RecoverySekaipediaCanarySong(nil), boundSongs...),
	}
	runtime, err := RuntimeConfigFromPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = WithSekaipediaCanaryPlan(runtime, plan)
	if err != nil {
		t.Fatal(err)
	}
	return sekaipediaCanaryFixture{
		plan: plan, runtime: runtime, ledger: ledger,
		transports: fixtureProviderTransports(t), identities: identities, songs: allSongs,
	}
}

func sekaipediaCanarySongFromFixture(
	t *testing.T,
	musicID int,
	catalogTitle string,
	fixtureName string,
) SekaipediaCanarySongPlan {
	t.Helper()
	return sekaipediaCanarySongFromResponse(t, musicID, catalogTitle, mustFixture(t, fixtureName))
}

func sekaipediaCanarySongFromResponse(
	t *testing.T,
	musicID int,
	catalogTitle string,
	body []byte,
) SekaipediaCanarySongPlan {
	t.Helper()
	var response struct {
		Query struct {
			Pages []struct {
				PageID    int    `json:"pageid"`
				Title     string `json:"title"`
				Revisions []struct {
					RevisionID int    `json:"revid"`
					Timestamp  string `json:"timestamp"`
					SHA1       string `json:"sha1"`
					Slots      struct {
						Main struct {
							Content string `json:"content"`
						} `json:"main"`
					} `json:"slots"`
				} `json:"revisions"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Query.Pages) != 1 ||
		len(response.Query.Pages[0].Revisions) != 1 {
		t.Fatalf("decode local Sekaipedia canary response for music %d: %v", musicID, err)
	}
	page := response.Query.Pages[0]
	revision := page.Revisions[0]
	contentDigest := sha256.Sum256([]byte(revision.Slots.Main.Content))
	responseDigest := sha256.Sum256(body)
	return SekaipediaCanarySongPlan{
		MusicID: musicID, CatalogTitle: catalogTitle, ProviderTitle: page.Title,
		PageID: page.PageID, RevisionID: revision.RevisionID,
		RevisionTimestamp: revision.Timestamp, SHA1: revision.SHA1,
		ContentSHA256:     hex.EncodeToString(contentDigest[:]),
		RawResponseSHA256: hex.EncodeToString(responseDigest[:]),
	}
}

func sekaipediaCanaryRuntimeWithIdentityMismatch(t *testing.T) sekaipediaCanaryFixture {
	t.Helper()
	fixture := newSekaipediaCanaryFixture(t, []int{2})
	mismatchedResponse := sekaipediaSongIDMismatchResponse(
		t, mustFixture(t, "sekaipedia-roki-330574.json"),
	)
	mismatchedSong := sekaipediaCanarySongFromResponse(t, 2, "ロキ", mismatchedResponse)
	fixture.plan.SekaipediaCanary.Songs[0] = mismatchedSong
	fixture.songs[2] = mismatchedSong
	fixture.transports = withSekaipediaSongIDMismatch(t, fixture.transports)
	runtime, err := RuntimeConfigFromPlan(fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = WithSekaipediaCanaryPlan(runtime, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = runtime
	fixture.identities[2] = lyricssource.MusicIdentity{
		MusicID: 2, JapaneseTitle: "ロキ", ProducerMetadata: "みきとP",
		Lyricist: "みきとP", Composer: "みきとP", Arranger: "みきとP",
		PerformerSegmentationPolicy: lyricssource.PerformerSegmentationSekaiEligible,
	}
	return fixture
}

func sekaipediaCanaryPrivateRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "lyrics-sekaipedia-canary-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func sekaipediaCanaryCatalogBinding(
	t *testing.T,
	root string,
) (lyricsextractionplan.RecoveryCatalogBinding, []int) {
	t.Helper()
	musicIDs := []int{2, 235, 999}
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(musicIDs)
	if err != nil {
		t.Fatal(err)
	}
	return lyricsextractionplan.RecoveryCatalogBinding{
		Path: filepath.Join(root, "catalog.fixture.db"), SizeBytes: 1,
		SourceSHA256:          strings.Repeat("a", 64),
		SchemaVersion:         lyricsextractionplan.CatalogSchemaVersion,
		RuntimeSchemaVersion:  lyricsextractionplan.MaximumCatalogRuntimeSchema,
		RecordCount:           len(musicIDs),
		IdentityPolicyVersion: lyricsextractionplan.CompiledEffectiveVersions().Policies.CatalogIdentity,
		IdentitySHA256:        strings.Repeat("b", 64),
		MusicIDsSHA256:        musicIDsSHA256,
	}, musicIDs
}

func replayCanaryAcquisition(
	t *testing.T,
	ctx context.Context,
	ledger *lyricsacquisition.Ledger,
	id lyricsacquisition.AcquisitionID,
) lyricsacquisition.Acquisition {
	t.Helper()
	acquired, err := ledger.ReplayByAcquisitionID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return acquired
}

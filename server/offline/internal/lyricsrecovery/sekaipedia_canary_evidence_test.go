package lyricsrecovery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"
)

func TestSekaipediaCanaryPlanBindsFixedListAndKnownSongRevisions(t *testing.T) {
	fixture := newSekaipediaCanaryFixture(t, []int{2, 235})
	if fixture.runtime.SekaipediaCanary == nil ||
		fixture.runtime.SekaipediaCanary.RecoveryPlanID != fixture.plan.PlanID ||
		fixture.runtime.SekaipediaCanary.RecoveryPlanSHA256 != fixture.runtime.RecoveryPlanSHA256 ||
		fixture.runtime.SekaipediaCanary.ListAcquisitionID != lyricsextractionplan.HistoricalSekaipediaListAcquisitionID ||
		fixture.runtime.SekaipediaCanary.List != fixture.runtime.Authorities[lyricssource.ProviderSekaipedia][0] ||
		len(fixture.runtime.SekaipediaCanary.Songs) != 2 {
		t.Fatalf("plan-bound Sekaipedia canary runtime=%+v", fixture.runtime.SekaipediaCanary)
	}

	for _, mutation := range []func(*lyricsextractionplan.RecoveryPlan){
		func(plan *lyricsextractionplan.RecoveryPlan) { plan.SekaipediaCanary.List.AcquisitionID = "" },
		func(plan *lyricsextractionplan.RecoveryPlan) {
			plan.SekaipediaCanary.List.AcquisitionID = strings.ToUpper(plan.SekaipediaCanary.List.AcquisitionID)
		},
		func(plan *lyricsextractionplan.RecoveryPlan) { plan.SekaipediaCanary.Songs[0].MusicID = 235 },
		func(plan *lyricsextractionplan.RecoveryPlan) {
			plan.SekaipediaCanary.Songs[0].RawResponseSHA256 = strings.Repeat("0", 63)
		},
		func(plan *lyricsextractionplan.RecoveryPlan) {
			plan.SekaipediaCanary.Songs[0].RevisionTimestamp = "2026-07-15T07:59:12+00:00"
		},
	} {
		candidate := fixture.plan
		canary := *candidate.SekaipediaCanary
		canary.Songs = append([]lyricsextractionplan.RecoverySekaipediaCanarySong(nil), canary.Songs...)
		candidate.SekaipediaCanary = &canary
		mutation(&candidate)
		if _, err := WithSekaipediaCanaryPlan(fixture.runtime, candidate); err == nil {
			t.Fatal("drifted Sekaipedia canary plan data was accepted")
		}
	}

	candidate := fixture.plan
	canary := *candidate.SekaipediaCanary
	canary.Songs = make([]lyricsextractionplan.RecoverySekaipediaCanarySong, MaxSekaipediaCanarySongs+1)
	candidate.SekaipediaCanary = &canary
	if _, err := WithSekaipediaCanaryPlan(fixture.runtime, candidate); err == nil {
		t.Fatal("unbounded Sekaipedia canary song list was accepted")
	}
}

func TestSekaipediaCanaryExactRawEvidenceAndProviderOutcomeEnter(t *testing.T) {
	fixture := newSekaipediaCanaryFixture(t, []int{2, 235})
	session, err := NewAcquisitionSession(fixture.runtime, fixture.ledger, fixture.transports)
	if err != nil {
		t.Fatal(err)
	}
	diagnosticDirectory := filepath.Join(sekaipediaCanaryPrivateRoot(t), "diagnostics")
	if err := os.Mkdir(diagnosticDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, musicID := range []int{2, 235} {
		identity := fixture.identities[musicID]
		sets, progress, diagnostic, err := session.AcquireSekaipediaCanarySong(t.Context(), musicID, identity)
		if err != nil {
			t.Fatalf("music %d Sekaipedia canary: %v", musicID, err)
		}
		if len(sets) != 1 || len(progress) != 1 || len(sets[0].AcquisitionIDs) != 2 ||
			sets[0].Provider != model.LyricsSourceProviderSekaipedia ||
			sets[0].Status != lyricsprovideroutcome.StatusCandidate ||
			sets[0].ReasonCode != lyricsprovideroutcome.ReasonCandidate ||
			progress[0].EnterResult != ProviderOutcomeCompleteCompositionStop ||
			progress[0].FallbackReasonCode != "" || progress[0].SekaipediaCanary == nil {
			t.Fatalf("music %d exact canary terminal=%+v progress=%+v", musicID, sets, progress)
		}
		target := fixture.songs[musicID]
		if diagnostic.MusicID != musicID || diagnostic.Provider != model.LyricsSourceProviderSekaipedia ||
			diagnostic.Status != lyricsprovideroutcome.StatusCandidate ||
			diagnostic.ReasonCode != lyricsprovideroutcome.ReasonCandidate ||
			diagnostic.Phase != lyricsprovideroutcome.PhaseFinalize ||
			diagnostic.Counts.Acquisitions != 2 || diagnostic.Counts.Targets != 1 ||
			diagnostic.Counts.Evaluated != 1 || diagnostic.Counts.Candidates != 1 ||
			diagnostic.EnterResult != ProviderOutcomeCompleteCompositionStop || diagnostic.FallbackReasonCode != "" ||
			diagnostic.List == nil || diagnostic.Song == nil ||
			diagnostic.List.RevisionID != fixture.runtime.SekaipediaCanary.List.RevisionID ||
			diagnostic.List.ContentSHA256 != fixture.runtime.SekaipediaCanary.List.ContentSHA256 ||
			diagnostic.Song.PageID != target.PageID || diagnostic.Song.RevisionID != target.RevisionID ||
			diagnostic.Song.ContentSHA256 != target.ContentSHA256 {
			t.Fatalf("music %d bounded diagnostic=%+v target=%+v", musicID, diagnostic, target)
		}

		for index, proof := range []*SekaipediaCanaryRevisionDiagnostic{
			diagnostic.List, diagnostic.Song,
		} {
			if proof == nil {
				t.Fatalf("music %d proof %d is missing", musicID, index)
			}
			acquired := replayCanaryAcquisition(t, t.Context(), fixture.ledger, sets[0].AcquisitionIDs[index])
			if !acquired.ReplayOnly || acquired.AcquisitionID != proof.AcquisitionID ||
				acquired.Evidence.EvidenceID != proof.EvidenceID ||
				acquired.RawResponseSHA256 != proof.RawResponseSHA256 ||
				acquired.Evidence.RawSHA256 != proof.RawResponseSHA256 ||
				!bytes.Equal(acquired.RawResponse, acquired.Evidence.Raw) || len(acquired.ObservedRevisions) != 1 ||
				acquired.ObservedRevisions[0].RevisionID != int64(proof.RevisionID) ||
				acquired.ObservedRevisions[0].Timestamp != proof.RevisionTimestamp ||
				acquired.ObservedRevisions[0].SHA1 != proof.SHA1 ||
				lyricssource.VerifySekaipediaRevisionContent(acquired.RawResponse, lyricssource.FixedIndex{
					PageID: proof.PageID, RevisionID: proof.RevisionID, RevisionTimestamp: proof.RevisionTimestamp,
					SHA1: proof.SHA1, ContentSHA256: proof.ContentSHA256,
				}) != nil {
				t.Fatalf("music %d proof %d exact evidence mismatch: acquisitionId=%s evidenceId=%s rawSha256=%s revisionId=%d",
					musicID, index, acquired.AcquisitionID, acquired.Evidence.EvidenceID,
					acquired.RawResponseSHA256, proof.RevisionID)
			}
		}

		body, err := json.Marshal(diagnostic)
		if err != nil || len(body) > 8<<10 {
			t.Fatalf("music %d bounded diagnostic bytes=%d err=%v", musicID, len(body), err)
		}
		for _, forbidden := range []string{
			target.CatalogTitle, target.ProviderTitle, "catalogTitle", "providerTitle", "wikitext", "lyrics",
			"romaji", "romanization", "romanized",
		} {
			if forbidden != "" && bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("music %d diagnostic contains forbidden content marker %q: %s", musicID, forbidden, body)
			}
		}
		published, err := PublishSekaipediaCanaryDiagnostic(diagnosticDirectory, diagnostic)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := OpenSekaipediaCanaryDiagnostic(published)
		if err != nil || opened.EnterResult != diagnostic.EnterResult ||
			opened.FallbackReasonCode != diagnostic.FallbackReasonCode ||
			!SekaipediaCanaryCompleteCompositionStop(fixture.runtime, opened) {
			t.Fatalf("music %d published diagnostic=%+v err=%v", musicID, opened, err)
		}
	}

	if fixture.transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper).requestCount() != 4 ||
		fixture.transports[model.LyricsSourceProviderMoegirl].(*fixtureRoundTripper).requestCount() != 0 ||
		fixture.transports[model.LyricsSourceProviderVocaloidFandom].(*fixtureRoundTripper).requestCount() != 0 {
		t.Fatal("complete Sekaipedia canaries crossed the first-provider stopping point")
	}
}

func TestSekaipediaCanaryAcceptsEnvelopePageInfoDriftAndRetainsExactRawEvidence(t *testing.T) {
	fixture := newSekaipediaCanaryFixture(t, []int{2})
	transport := fixture.transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper)
	respond := transport.respond
	returned := make(map[string][]byte, 2)
	transport.respond = func(request *http.Request) ([]byte, error) {
		body, err := respond(request)
		if err != nil {
			return nil, err
		}
		drifted := sekaipediaEnvelopePageInfoDrift(t, body)
		key := "song"
		if request.URL.Query().Get("revids") == strconv.Itoa(fixture.runtime.SekaipediaCanary.List.RevisionID) {
			key = "list"
		}
		returned[key] = append([]byte(nil), drifted...)
		return drifted, nil
	}

	session, err := NewAcquisitionSession(fixture.runtime, fixture.ledger, fixture.transports)
	if err != nil {
		t.Fatal(err)
	}
	sets, _, diagnostic, err := session.AcquireSekaipediaCanarySong(t.Context(), 2, fixture.identities[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].AcquisitionIDs) != 2 ||
		!SekaipediaCanaryCompleteCompositionStop(fixture.runtime, diagnostic) ||
		diagnostic.List == nil || diagnostic.Song == nil {
		t.Fatalf("semantic canary terminal=%+v sets=%+v", diagnostic, sets)
	}
	if diagnostic.List.RawResponseSHA256 == fixture.runtime.SekaipediaCanary.List.RawSHA256 ||
		diagnostic.Song.RawResponseSHA256 == fixture.songs[2].RawResponseSHA256 {
		t.Fatal("envelope raw drift was incorrectly required to match the plan fixture")
	}
	for index, key := range []string{"list", "song"} {
		acquired := replayCanaryAcquisition(t, t.Context(), fixture.ledger, sets[0].AcquisitionIDs[index])
		digest := sha256.Sum256(returned[key])
		if !bytes.Equal(acquired.RawResponse, returned[key]) ||
			acquired.RawResponseSHA256 != hex.EncodeToString(digest[:]) ||
			acquired.Evidence.RawSHA256 != acquired.RawResponseSHA256 ||
			!bytes.Equal(acquired.Evidence.Raw, returned[key]) {
			t.Fatalf("%s raw envelope evidence was not retained exactly", key)
		}
	}
	body, err := MarshalSekaipediaCanaryDiagnostic(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(body)
	for _, forbidden := range [][]byte{[]byte("romaji"), []byte("romanization"), []byte("romanized")} {
		if bytes.Contains(lower, forbidden) {
			t.Fatalf("semantic canary diagnostic leaked forbidden marker %q", forbidden)
		}
	}
}

func TestPlanBoundSekaipediaListReplayUsesExactHistoricalAcquisitionAndOnlySongsGoLive(t *testing.T) {
	fixture := newSekaipediaCanaryFixture(t, []int{2})
	list := fixture.runtime.SekaipediaCanary.List
	raw := sekaipediaEnvelopePageInfoDrift(t, mustFixture(t, "sekaipedia-list-335193.json"))
	sourceLedger, source := commitHistoricalSekaipediaListAcquisition(
		t, raw, exactSekaipediaListCanaryURL(list.RevisionID),
	)
	live := fixture.transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper)
	respond := live.respond
	target := fixture.songs[2]
	live.respond = func(request *http.Request) ([]byte, error) {
		if request.URL.String() != exactSekaipediaSongCanaryURL(target.RevisionID) {
			return nil, errors.New("live Sekaipedia canary did not use the one exact plan-pinned revision request")
		}
		return respond(request)
	}
	sourceReplay := replayCanaryAcquisition(t, t.Context(), sourceLedger, source.AcquisitionID)
	replay, err := NewPlanBoundSekaipediaListReplayTransport(sourceReplay, list, live)
	if err != nil {
		t.Fatal(err)
	}
	fixture.transports[model.LyricsSourceProviderSekaipedia] = replay
	session, err := NewAcquisitionSession(fixture.runtime, fixture.ledger, fixture.transports)
	if err != nil {
		t.Fatal(err)
	}
	sets, _, diagnostic, err := session.AcquireSekaipediaCanarySong(
		t.Context(), 2, fixture.identities[2],
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Consumed() || live.requestCount() != 1 || len(sets) != 1 ||
		len(sets[0].AcquisitionIDs) != 2 || !SekaipediaCanaryCompleteCompositionStop(fixture.runtime, diagnostic) {
		t.Fatalf("plan-bound replay consumed=%t actualLive=%d sets=%+v diagnostic=%+v",
			replay.Consumed(), live.requestCount(), sets, diagnostic)
	}
	retained := replayCanaryAcquisition(t, t.Context(), fixture.ledger, sets[0].AcquisitionIDs[0])
	if retained.Request.Kind != lyricsacquisition.RequestKindFixedIndex ||
		retained.FetchedAt != source.FetchedAt || !bytes.Equal(retained.RawResponse, raw) ||
		!bytes.Equal(retained.RawResponse, retained.Evidence.Raw) ||
		retained.RawResponseSHA256 != source.RawResponseSHA256 ||
		retained.AcquisitionID == source.AcquisitionID {
		t.Fatalf("replayed List evidence mismatch: sourceId=%s retainedId=%s kind=%s sourceRawSha256=%s retainedRawSha256=%s",
			source.AcquisitionID, retained.AcquisitionID, retained.Request.Kind,
			source.RawResponseSHA256, retained.RawResponseSHA256)
	}
	if diagnostic.List == nil || diagnostic.List.RawResponseSHA256 != source.RawResponseSHA256 ||
		diagnostic.List.ContentSHA256 != list.ContentSHA256 {
		t.Fatal("replayed List diagnostic does not retain the source raw hash and plan content hash")
	}

	for name, test := range map[string]struct {
		acquired lyricsacquisition.Acquisition
		expected lyricssource.FixedIndex
	}{
		"wrong page identity": {
			acquired: sourceReplay, expected: func() lyricssource.FixedIndex {
				wrong := list
				wrong.PageID++
				return wrong
			}(),
		},
		"wrong revision identity": {
			acquired: sourceReplay, expected: func() lyricssource.FixedIndex {
				wrong := list
				wrong.RevisionID++
				return wrong
			}(),
		},
		"changed content authority": {
			acquired: sourceReplay, expected: func() lyricssource.FixedIndex {
				wrong := list
				wrong.ContentSHA256 = strings.Repeat("0", 64)
				return wrong
			}(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if candidate, err := NewPlanBoundSekaipediaListReplayTransport(
				test.acquired, test.expected, live,
			); err == nil || candidate != nil {
				t.Fatalf("drifted exact-acquisition replay accepted=%t err=%v", candidate != nil, err)
			}
		})
	}

	wrongRequestLedger, wrongRequest := commitHistoricalSekaipediaListAcquisition(
		t, raw, titleBasedSekaipediaCanaryURL("List of songs"),
	)
	wrongRequestReplay := replayCanaryAcquisition(
		t, t.Context(), wrongRequestLedger, wrongRequest.AcquisitionID,
	)
	if candidate, err := NewPlanBoundSekaipediaListReplayTransport(
		wrongRequestReplay, list, live,
	); err == nil || candidate != nil {
		t.Fatalf("wrong request replay accepted=%t err=%v", candidate != nil, err)
	}
}

func TestRepeatablePlanBoundSekaipediaListReplayCreatesUniqueLocalObservationsWithoutListHTTP(t *testing.T) {
	fixture := newSekaipediaCanaryFixture(t, []int{2, 235})
	list := fixture.runtime.SekaipediaCanary.List
	raw := sekaipediaEnvelopePageInfoDrift(t, mustFixture(t, "sekaipedia-list-335193.json"))
	sourceLedger, source := commitHistoricalSekaipediaListAcquisition(
		t, raw, exactSekaipediaListCanaryURL(list.RevisionID),
	)
	sourceReplay := replayCanaryAcquisition(t, t.Context(), sourceLedger, source.AcquisitionID)
	live := fixture.transports[model.LyricsSourceProviderSekaipedia].(*fixtureRoundTripper)
	replay, err := NewRepeatablePlanBoundSekaipediaListReplayTransport(sourceReplay, list, live)
	if err != nil {
		t.Fatal(err)
	}
	fixture.transports[model.LyricsSourceProviderSekaipedia] = replay
	session, err := NewAcquisitionSession(fixture.runtime, fixture.ledger, fixture.transports)
	if err != nil {
		t.Fatal(err)
	}

	songs := make([]SongAcquisitionSet, 0, 2)
	var previousList lyricsacquisition.Acquisition
	for _, musicID := range []int{2, 235} {
		providers, _, err := session.AcquireSong(t.Context(), musicID, fixture.identities[musicID])
		if err != nil {
			t.Fatalf("music %d repeatable List replay: %v", musicID, err)
		}
		if len(providers) != 1 || len(providers[0].AcquisitionIDs) < 2 {
			t.Fatalf("music %d provider acquisitions=%+v", musicID, providers)
		}
		observed := replayCanaryAcquisition(t, t.Context(), fixture.ledger, providers[0].AcquisitionIDs[0])
		if observed.Request.Kind != lyricsacquisition.RequestKindFixedIndex ||
			observed.FetchedAt == source.FetchedAt || !bytes.Equal(observed.RawResponse, raw) ||
			observed.RawResponseSHA256 != source.RawResponseSHA256 {
			t.Fatalf("music %d local List observation=%+v source=%+v", musicID, observed, source)
		}
		if previousList.AcquisitionID != "" &&
			(observed.AcquisitionID == previousList.AcquisitionID || observed.FetchedAt <= previousList.FetchedAt) {
			t.Fatalf("music %d repeated List observation was not unique and ordered: previous=%s/%s current=%s/%s",
				musicID, previousList.AcquisitionID, previousList.FetchedAt, observed.AcquisitionID, observed.FetchedAt)
		}
		previousList = observed
		songs = append(songs, SongAcquisitionSet{MusicID: musicID, Providers: providers})
	}
	if replay.ReplayCount() != len(songs) || !replay.Consumed() || live.requestCount() != len(songs) {
		t.Fatalf("repeatable List replay count=%d consumed=%t songHTTP=%d songs=%d",
			replay.ReplayCount(), replay.Consumed(), live.requestCount(), len(songs))
	}
	if _, err := NewAcquisitionSet(
		fixture.plan.PlanID, fixture.runtime.RecoveryPlanSHA256, fixture.runtime.Order, songs,
	); err != nil {
		t.Fatalf("unique local List observations did not form a valid exact acquisition set: %v", err)
	}
}

func commitHistoricalSekaipediaListAcquisition(
	t *testing.T,
	raw []byte,
	canonicalRequestURL string,
) (*lyricsacquisition.Ledger, lyricsacquisition.Acquisition) {
	t.Helper()
	root := sekaipediaCanaryPrivateRoot(t)
	ledger, err := lyricsacquisition.CreateLedger(t.Context(), filepath.Join(root, "source-ledger"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	capture, err := lyricssource.CaptureRecoveryHTTPResponse(
		model.LyricsSourceProviderSekaipedia, nil,
		lyricssource.RecoveryHTTPResponse{
			Action: "page", CanonicalRequestURL: canonicalRequestURL,
			FetchedAt: time.Date(2026, 8, 2, 15, 58, 47, 64_046_000, time.UTC), Raw: raw,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if capture.RequestKind != lyricssource.RecoveryRequestRevision {
		t.Fatalf("historical blocker fixture kind=%s, want revision", capture.RequestKind)
	}
	acquired, err := ledger.Commit(t.Context(), recordInputFromCapture(capture))
	if err != nil {
		t.Fatal(err)
	}
	return ledger, acquired
}

func exactSekaipediaListCanaryURL(revisionID int) string {
	query := exactSekaipediaCanaryQueryValues()
	query.Set("revids", strconv.Itoa(revisionID))
	return "https://www.sekaipedia.org/w/api.php?" + query.Encode()
}

func exactSekaipediaSongCanaryURL(revisionID int) string {
	return exactSekaipediaListCanaryURL(revisionID)
}

func titleBasedSekaipediaCanaryURL(title string) string {
	query := exactSekaipediaCanaryQueryValues()
	query.Set("redirects", "1")
	query.Set("rvlimit", "1")
	query.Set("titles", title)
	return "https://www.sekaipedia.org/w/api.php?" + query.Encode()
}

func exactSekaipediaCanaryQueryValues() url.Values {
	return url.Values{
		"action": {"query"}, "cllimit": {"max"}, "format": {"json"}, "formatversion": {"2"},
		"maxlag": {"5"}, "prop": {"revisions|categories"},
		"rvprop": {"ids|timestamp|sha1|content"}, "rvslots": {"main"},
	}
}

func sekaipediaEnvelopePageInfoDrift(t *testing.T, body []byte) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	query, ok := envelope["query"].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia envelope query is missing")
	}
	pages, ok := query["pages"].([]any)
	if !ok || len(pages) != 1 {
		t.Fatal("Sekaipedia envelope page boundary is invalid")
	}
	page, ok := pages[0].(map[string]any)
	if !ok {
		t.Fatal("Sekaipedia envelope page is invalid")
	}
	page["touched"] = "2026-08-02T15:58:47Z"
	page["length"] = float64(1)
	page["pageInfoOnly"] = true
	drifted, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(drifted, body) {
		t.Fatal("Sekaipedia envelope page-info mutation did not change raw bytes")
	}
	return drifted
}

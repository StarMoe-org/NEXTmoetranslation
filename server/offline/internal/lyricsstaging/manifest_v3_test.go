package lyricsstaging

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

func TestDecodeManifestV2RequiresRebuildWithoutOverwritingBytes(t *testing.T) {
	legacy := []byte(`{"schemaVersion":2,"preflight":{},"catalogReference":[],"items":[],"batchSha256":""}`)
	before := append([]byte(nil), legacy...)
	_, err := DecodeManifest(legacy)
	if !errors.Is(err, ErrManifestRebuildRequired) || !strings.Contains(err.Error(), "schema v2") {
		t.Fatalf("legacy manifest error=%v", err)
	}
	if string(legacy) != string(before) {
		t.Fatal("legacy manifest bytes changed while reporting rebuild")
	}
}

func TestBuildDraftConsumesExactProviderDocumentReasonAndProjection(t *testing.T) {
	report, catalogIdentity, fixed := validPreflightAndFixed(t)
	legacy, err := BuildDraft(report.UniqueComplete[0], catalogIdentity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	projection := &model.LyricsSourceGameProjection{LineIDs: []string{legacy.Document.Full.Lines[0].ID}}
	legacy.Document.ReasonCode = model.LyricsSourceVersionReasonUntaggedUncutIdentity
	report.UniqueComplete[0].CompositionReason = legacy.Document.ReasonCode
	legacy.Document.GameProjection = projection
	component := model.LyricsSourceComponentRef{RenditionKey: legacy.Document.Provenance.FullText.RenditionKey}
	legacy.Document.Provenance.GameProjection = &component
	fixed.Provider = legacy.Document.FixedIdentities[0].Provider
	fixed.Origin = legacy.Document.FixedIdentities[0].Origin
	fixed.Section = legacy.Document.FixedIdentities[0].Section
	fixed.RenditionKey = legacy.Document.FixedIdentities[0].RenditionKey
	fixed.VersionReason = report.UniqueComplete[0].Candidate.VersionReason
	fixed.IndexEvidenceRefs = legacy.Document.FixedIdentities[0].IndexEvidenceRefs
	fixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, legacy.Document.FixedIdentities...)
	fixed.Document = &legacy.Document
	draft, err := BuildDraft(report.UniqueComplete[0], catalogIdentity, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Document.ReasonCode != model.LyricsSourceVersionReasonUntaggedUncutIdentity ||
		draft.Document.GameProjection == nil || draft.Document.Provenance.GameProjection == nil {
		t.Fatalf("exact fixed document was not preserved: %+v", draft.Document)
	}
}

func TestManifestV3SupportsMultipleProviderArtifactsAndComponentContributions(t *testing.T) {
	draft := validSemanticDraft(t)
	moegirl := model.LyricsSourceFixedIdentity{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: 99, RevisionID: 100, SHA1: strings.Repeat("b", 40), Title: "合成試験曲/版本资料",
		CanonicalURL: "https://moegirl.icu/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=100",
		FetchedAt:    "2026-07-30T12:35:00Z", Categories: []string{}, Section: "版本资料",
		RenditionKey: "version-evidence", IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "moegirl/index/99/100", SHA256: strings.Repeat("c", 64),
		}},
	}
	artifact := Artifact{Identity: moegirl, RawWikitextByteCount: 17, RawWikitextSHA256: strings.Repeat("d", 64)}
	var err error
	artifact.ArtifactSHA256, err = stagedArtifactDigest(artifact)
	if err != nil {
		t.Fatal(err)
	}
	draft.Artifacts = append(draft.Artifacts, artifact)
	draft.Document.FixedIdentities = append(draft.Document.FixedIdentities, moegirl)
	draft.Document.Provenance.VersionEvidence = model.LyricsSourceComponentRef{RenditionKey: moegirl.RenditionKey}
	draft.DocumentSHA256, err = lyricsSourceDocumentDigest(draft.Document)
	if err != nil {
		t.Fatal(err)
	}
	draft.DraftSHA256, err = draftDigest(draft)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDraft(draft); err != nil {
		t.Fatalf("multi-provider draft: %v", err)
	}
}

func TestRecoveryPeerDraftAcceptsIndependentGamePeerTranslationsAndRejectsExactProjection(t *testing.T) {
	document, artifacts := recoveryPeerTranslationFixture(t, model.LyricsSourceRenditionRelationNone)
	translations := []lyricscontract.RenditionTranslation{{
		RenditionKey: document.Renditions[0].RenditionKey,
		Translations: []string{"主译文"},
		PeerTranslations: []lyricscontract.RenditionPeerTranslation{{
			Side: "game", Locale: "zh-CN", Translations: []string{"游戏译文"},
		}},
	}}
	draft, err := BuildRecoveryPeerDraft(
		42, "多版本试验曲", strings.Repeat("a", 64), 42, []int{}, document, artifacts, translations,
	)
	if err != nil {
		t.Fatalf("independent Game peer translations: %v", err)
	}
	translations[0].PeerTranslations[0].Translations[0] = "mutated"
	if got := draft.RenditionTranslations[0].PeerTranslations[0].Translations[0]; got != "游戏译文" {
		t.Fatalf("peer translation clone=%q", got)
	}
	if err := ValidateDraft(draft); err != nil {
		t.Fatalf("validate independent Game peer draft: %v", err)
	}

	exactDocument, exactArtifacts := recoveryPeerTranslationFixture(t, model.LyricsSourceRenditionRelationExactProjection)
	if _, err := BuildRecoveryPeerDraft(
		42, "多版本试验曲", strings.Repeat("a", 64), 42, []int{}, exactDocument, exactArtifacts,
		[]lyricscontract.RenditionTranslation{{
			RenditionKey: exactDocument.Renditions[0].RenditionKey,
			Translations: []string{"主译文"},
			PeerTranslations: []lyricscontract.RenditionPeerTranslation{{
				Side: "game", Locale: "zh-CN", Translations: []string{"禁止的派生译文"},
			}},
		}},
	); err == nil || !strings.Contains(err.Error(), "no independently persisted game peer side") {
		t.Fatalf("exact projection peer translation error=%v", err)
	}
}

func TestRecoveryPeerDraftRejectsPrimaryAndPeerTranslationsOverDocumentBoundary(t *testing.T) {
	document, artifacts := recoveryPeerTranslationFixture(t, model.LyricsSourceRenditionRelationNone)
	rendition := &document.Renditions[0]
	fullTemplate := rendition.Full.Lines[0]
	gameTemplate := rendition.Game.Lines[0]
	rendition.Full.Lines = make([]model.LyricsSourceFullLine, 128)
	rendition.Game.Lines = make([]model.LyricsSourceFullLine, 128)
	translations := make([]string, 128)
	peerTranslations := make([]string, 128)
	chunk := strings.Repeat("x", 16<<10)
	for index := 0; index < 128; index++ {
		rendition.Full.Lines[index] = fullTemplate
		rendition.Full.Lines[index].ID = fmt.Sprintf("full-%06d", index+1)
		rendition.Game.Lines[index] = gameTemplate
		rendition.Game.Lines[index].ID = fmt.Sprintf("game-%06d", index+1)
		translations[index] = chunk
		peerTranslations[index] = chunk
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("expanded staging document: %v", err)
	}
	_, err := BuildRecoveryPeerDraft(
		42, "多版本试验曲", strings.Repeat("a", 64), 42, []int{}, document, artifacts,
		[]lyricscontract.RenditionTranslation{{
			RenditionKey: document.Renditions[0].RenditionKey,
			Translations: translations, TranslationCredit: "x",
			PeerTranslations: []lyricscontract.RenditionPeerTranslation{{
				Side: "game", Locale: "zh-CN", Translations: peerTranslations,
			}},
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "safe document boundary") {
		t.Fatalf("aggregate staging boundary error=%v", err)
	}
}

func recoveryPeerTranslationFixture(
	t *testing.T,
	relationKind model.LyricsSourceRenditionRelationKind,
) (model.LyricsSourceDocument, []Artifact) {
	t.Helper()
	identity := model.LyricsSourceFixedIdentity{
		Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
		PageID: 42, RevisionID: 420, RevisionTimestamp: "2026-08-14T00:00:00Z",
		SHA1: strings.Repeat("b", 40), Title: "多版本试验曲",
		CanonicalURL: "https://www.sekaipedia.org/wiki/Test?oldid=420",
		FetchedAt:    "2026-08-14T00:01:00Z", Categories: []string{}, Section: "Lyrics",
		RenditionKey: "fixture-source", CompositionRenditionKey: "sekai",
		VersionReason: model.LyricsSourceVersionReasonTaggedFullAndGame,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "revision:sekaipedia:42:420:" + strings.Repeat("c", 64),
			SHA256:     strings.Repeat("d", 64),
		}},
	}
	full := model.LyricsSourceFull{
		Version:    model.LyricsSourceVersion{Kind: "sekai", Label: "Full Version"},
		Performers: []model.LyricsSourcePerformer{}, RubyGeneratorVersion: "sekaipedia-ruby-kana-v2",
		Lines: []model.LyricsSourceFullLine{{
			ID: "full-000001", Text: "主歌词", Segments: []model.LyricsSourceSegment{{
				Text: "主歌词", PerformerIDs: []string{}, Ruby: []model.LyricsSourceRubySpan{{Text: "主歌词"}},
			}}, TrailingPerformerIDs: []string{},
		}},
	}
	game := full
	game.Version.Label = "Game Version"
	game.Lines = []model.LyricsSourceFullLine{{
		ID: "game-000001", Text: "游戏歌词", Segments: []model.LyricsSourceSegment{{
			Text: "游戏歌词", PerformerIDs: []string{}, Ruby: []model.LyricsSourceRubySpan{{Text: "游戏歌词"}},
		}}, TrailingPerformerIDs: []string{},
	}}
	ref := model.LyricsSourceComponentRef{RenditionKey: identity.RenditionKey}
	relation := model.LyricsSourceRenditionRelation{Kind: relationKind}
	if relationKind == model.LyricsSourceRenditionRelationExactProjection {
		game = *model.CloneLyricsSourceFull(&full)
		game.Version.Label = "Game Version"
		game.Lines[0].ID = "game-000001"
		relation.FullRenditionKey = "sekai"
		relation.LineIDs = []string{"full-000001"}
	}
	document := model.LyricsSourceDocument{
		SchemaVersion:   model.LyricsSourceDocumentSchemaVersionV3,
		FixedIdentities: []model.LyricsSourceFixedIdentity{identity},
		Renditions: []model.LyricsSourceRendition{{
			RenditionKey: "sekai", SourceKind: model.LyricsSourceRenditionSekai,
			SourceTabPaths:        []model.LyricsSourceTabPath{{"Full Version"}, {"Game Version"}},
			ReasonCode:            model.LyricsSourceVersionReasonTaggedFullAndGame,
			FullPerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
			GamePerformerEvidence: model.LyricsSourcePerformerEvidenceNone,
			Full:                  &full, Game: &game, Relation: relation,
			Provenance: model.LyricsSourceRenditionProvenance{
				FullText: &ref, GameText: &ref, RelationEvidence: ref, VersionEvidence: ref,
			},
		}},
	}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("peer translation source fixture: %v", err)
	}
	artifact, err := NewRecoveryArtifact(identity, []byte("固定日文证据"))
	if err != nil {
		t.Fatal(err)
	}
	return document, []Artifact{artifact}
}

func TestManifestV3PreservesSubsecondFullTextFetchedAt(t *testing.T) {
	draft := validSemanticDraft(t)
	draft.Source.FetchedAt = "2026-07-30T12:34:57.123Z"
	draft.Artifacts[0].Identity.FetchedAt = draft.Source.FetchedAt
	draft.Document.FixedIdentities[0].FetchedAt = draft.Source.FetchedAt
	draft.Artifacts[0].ArtifactSHA256, _ = stagedArtifactDigest(draft.Artifacts[0])
	draft.DocumentSHA256, _ = lyricsSourceDocumentDigest(draft.Document)
	draft.DraftSHA256, _ = draftDigest(draft)
	if err := ValidateDraft(draft); err != nil {
		t.Fatalf("subsecond full-text fetchedAt: %v", err)
	}
	if draft.Document.FixedIdentities[0].FetchedAt != "2026-07-30T12:34:57.123Z" {
		t.Fatalf("subsecond full-text fetchedAt changed to %q", draft.Document.FixedIdentities[0].FetchedAt)
	}
}

package store

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

type stagedImportSong struct {
	musicID                           int
	title                             string
	text                              string
	translation                       string
	sourceID                          string
	versionKind                       string
	catalogVocalType                  string
	provider                          model.LyricsSourceProvider
	authoritativeVocaloidSegmentation bool
}

func stagedSekaipediaRevisionEvidenceID(baseID, fetchedAt, rawSHA256 string) string {
	identity := strings.Join([]string{
		"lyrics-source-index-evidence-v1",
		string(lyricssource.IndexEvidenceKindMediaWikiRevision),
		string(model.LyricsSourceProviderSekaipedia),
		string(model.LyricsSourceOriginSekaipedia),
		baseID,
		fetchedAt,
		rawSHA256,
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s:%x", baseID, digest)
}

func setupStagedManifestImportStore(t *testing.T, songs []stagedImportSong) (*Store, *db.DB, lyricsstaging.Manifest, lyricsstaging.PrivateEvidenceReceipt) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "staged-import.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := New(database)
	if err := s.UpsertPerformerCatalog([]PerformerCatalogRecord{
		{PerformerID: 21, JapaneseName: "初音ミク", EnglishName: "Miku"},
		{PerformerID: 22, JapaneseName: "鏡音リン", EnglishName: "Rin"},
	}); err != nil {
		t.Fatal(err)
	}
	records := make([]MusicCatalogRecord, len(songs))
	for index, song := range songs {
		vocalType := song.catalogVocalType
		if vocalType == "" {
			vocalType = "sekai"
		}
		records[index] = MusicCatalogRecord{
			MusicID: song.musicID, JapaneseTitle: song.title, Lyricist: "作詞者", Composer: "作曲者",
			Arranger: "編曲者", LyricsVersion: "full", LyricsVersionKnown: true,
			Vocals: []model.CatalogVocalSignal{{VocalID: song.musicID, VocalType: vocalType, CharacterType: "game_character", CharacterID: 21}},
		}
	}
	if err := s.UpsertMusicCatalog(records); err != nil {
		t.Fatal(err)
	}

	report := lyricsstaging.PreflightReport{
		SchemaVersion: lyricsstaging.PreflightSchemaVersion, GeneratedAt: "2026-07-30T12:34:56Z",
		CatalogSchemaVersion: lyricsstaging.CatalogSchemaVersion, CatalogCount: len(songs),
		CatalogReview: []lyricsstaging.PreflightItem{}, GameSizeEvidence: []lyricsstaging.PreflightItem{},
		UniqueComplete: []lyricsstaging.PreflightItem{}, Ambiguous: []lyricsstaging.PreflightItem{},
		Missing: []lyricsstaging.PreflightItem{}, Incomplete: []lyricsstaging.PreflightItem{}, Error: []lyricsstaging.PreflightItem{},
	}
	drafts := make([]lyricsstaging.Draft, len(songs))
	allEvidence := []lyricssource.IndexEvidence{}
	for index, song := range songs {
		identity, err := s.CatalogMusicIdentity(song.musicID)
		if err != nil {
			t.Fatal(err)
		}
		wikitext := []byte("== Lyrics ==\n" + song.text)
		contentSHA1 := sha1.Sum(wikitext)
		contentSHA256 := sha256.Sum256(wikitext)
		fetchedAt := time.Date(2026, time.July, 30, 12, 34+index, 57, 0, time.UTC)
		versionKind := song.versionKind
		if versionKind == "" {
			versionKind = "sekai"
		}
		section, renditionKey := "Lyrics/Project SEKAI Version", "full-sekai"
		if versionKind == "vocaloid" {
			section, renditionKey = "Lyrics/Vocaloid Version", "full-vocaloid"
		}
		provider := song.provider
		if provider == "" {
			provider = model.LyricsSourceProviderVocaloidFandom
		}
		origin := model.LyricsSourceOriginVocaloidFandom
		host := "vocaloid.fandom.com"
		if provider == model.LyricsSourceProviderMoegirl {
			origin = model.LyricsSourceOriginMoegirl
			host = "moegirl.icu"
		} else if provider == model.LyricsSourceProviderSekaipedia {
			origin = model.LyricsSourceOriginSekaipedia
			host = "www.sekaipedia.org"
		}
		canonicalRevision := url.URL{Scheme: "https", Host: host, Path: "/wiki/" + strings.ReplaceAll(song.title, " ", "_")}
		query := canonicalRevision.Query()
		query.Set("oldid", fmt.Sprintf("%d", song.musicID*10+2))
		canonicalRevision.RawQuery = query.Encode()
		revisionTimestamp := ""
		evidenceRaw := append([]byte{}, wikitext...)
		if provider == model.LyricsSourceProviderSekaipedia {
			revisionTimestamp = fetchedAt.Add(-time.Minute).Format(time.RFC3339Nano)
			categoryRows := []map[string]string{{"title": "Category:Lyrics"}, {"title": "Category:Songs"}}
			evidenceRaw, err = json.Marshal(map[string]any{
				"query": map[string]any{"pages": map[string]any{
					fmt.Sprint(song.musicID*10 + 1): map[string]any{
						"pageid": song.musicID*10 + 1, "title": song.title, "categories": categoryRows,
						"revisions": []any{map[string]any{
							"revid": song.musicID*10 + 2, "timestamp": revisionTimestamp,
							"sha1":  hex.EncodeToString(contentSHA1[:]),
							"slots": map[string]any{"main": map[string]any{"content": string(wikitext)}},
						}},
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		evidenceRawSHA256 := sha256.Sum256(evidenceRaw)
		evidenceRawSHA256Hex := hex.EncodeToString(evidenceRawSHA256[:])
		fetchedAtText := fetchedAt.Format(time.RFC3339Nano)
		evidenceBaseID := fmt.Sprintf("fetch:vocaloid-fandom:%d", song.musicID*10+1)
		if provider == model.LyricsSourceProviderMoegirl {
			evidenceBaseID = fmt.Sprintf("search:moegirl:%d", song.musicID*10+1)
		} else if provider == model.LyricsSourceProviderSekaipedia {
			evidenceBaseID = fmt.Sprintf("revision:sekaipedia:%d:%d", song.musicID*10+1, song.musicID*10+2)
		}
		evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
			provider, evidenceBaseID, fetchedAtText, evidenceRawSHA256Hex,
		)
		indexEvidenceRefs := []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: evidenceID, SHA256: evidenceRawSHA256Hex,
		}}
		if provider == model.LyricsSourceProviderSekaipedia {
			listEvidenceID := stagedSekaipediaRevisionEvidenceID(
				"authority:sekaipedia:list-of-songs:335193",
				fetchedAtText,
				"c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
			)
			indexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{
				{EvidenceID: listEvidenceID, SHA256: "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd"},
				{EvidenceID: evidenceID, SHA256: evidenceRawSHA256Hex},
			}
		}
		candidate := lyricsstaging.CandidateIdentity{
			Provider: provider, Origin: origin,
			PageID: song.musicID*10 + 1, RevisionID: song.musicID*10 + 2, RevisionTimestamp: revisionTimestamp,
			SHA1: hex.EncodeToString(contentSHA1[:]), Title: song.title, CanonicalURL: canonicalRevision.String(),
			Categories: []string{"Lyrics", "Songs"}, Section: section, RenditionKey: renditionKey,
			VersionReason:     model.LyricsSourceVersionReasonUntaggedFullOnly,
			IndexEvidenceRefs: indexEvidenceRefs,
		}
		evidence := lyricssource.IndexEvidence{
			EvidenceID: evidenceID, SHA256: hex.EncodeToString(evidenceRawSHA256[:]),
			Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
			Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
			RevisionTimestamp: candidate.RevisionTimestamp,
			MediaWikiSHA1:     candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL,
			Categories: append([]string{}, candidate.Categories...), FetchedAt: fetchedAt.Format(time.RFC3339Nano),
			Raw: evidenceRaw, RawSHA256: hex.EncodeToString(evidenceRawSHA256[:]),
		}
		if provider == model.LyricsSourceProviderSekaipedia {
			listRaw, readErr := os.ReadFile("../lyricssource/testdata/sekaipedia-list-335193.json")
			if readErr != nil {
				t.Fatal(readErr)
			}
			allEvidence = append(allEvidence, lyricssource.IndexEvidence{
				EvidenceID: indexEvidenceRefs[0].EvidenceID,
				SHA256:     "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
				Kind:       lyricssource.IndexEvidenceKindMediaWikiRevision,
				Provider:   model.LyricsSourceProviderSekaipedia,
				Origin:     model.LyricsSourceOriginSekaipedia,
				PageID:     268, RevisionID: 335193, RevisionTimestamp: "2026-07-27T16:29:13Z",
				MediaWikiSHA1: "b216a827f88c59f5e954a120027832fe9cd74413", Title: "List of songs",
				CanonicalURL: "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193",
				Categories:   []string{"Lists", "Project SEKAI"}, FetchedAt: fetchedAt.Format(time.RFC3339Nano),
				Raw: listRaw, RawSHA256: "c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
			})
		}
		allEvidence = append(allEvidence, evidence)
		item := lyricsstaging.PreflightItem{
			MusicID: song.musicID, JapaneseTitle: song.title, CatalogFingerprint: identity.CatalogFingerprint,
			TargetMusicID: song.musicID, AssociationMusicIDs: []int{}, Candidate: &candidate,
			LineCount: 1, SearchAttempts: 1, FetchAttempts: 1,
		}
		report.UniqueComplete = append(report.UniqueComplete, item)
		versionLabel := "Project SEKAI Version"
		performers := []lyricssource.Performer{{PerformerID: song.sourceID, Name: song.sourceID, Color: "#33CCBB"}}
		performerIDs := []string{song.sourceID}
		if versionKind == "vocaloid" {
			versionLabel = "Vocaloid Version"
			if song.authoritativeVocaloidSegmentation {
				performers = []lyricssource.Performer{
					{PerformerID: song.sourceID, Name: song.sourceID, Color: "#33CCBB"},
					{PerformerID: "rin", Name: "rin", Color: "#FFCC11"},
				}
				performerIDs = []string{song.sourceID}
			} else {
				performers = []lyricssource.Performer{}
				performerIDs = []string{}
			}
		}
		var fixedRevisionTimestamp time.Time
		if candidate.RevisionTimestamp != "" {
			fixedRevisionTimestamp, err = time.Parse(time.RFC3339Nano, candidate.RevisionTimestamp)
			if err != nil {
				t.Fatal(err)
			}
		}
		fixed := lyricssource.FixedRevision{
			Provider: candidate.Provider, Origin: candidate.Origin,
			PageID: candidate.PageID, RevisionID: candidate.RevisionID, RevisionTimestamp: fixedRevisionTimestamp, SHA1: candidate.SHA1,
			PageTitle: candidate.Title, CanonicalURL: candidate.CanonicalURL, Categories: append([]string(nil), candidate.Categories...),
			Section: candidate.Section, RenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
			IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
			FetchedAt:         fetchedAt,
			Wikitext:          wikitext, Lines: []lyricssource.ExtractedLine{{Japanese: song.text}},
			Extraction: lyricssource.Extraction{
				Version:              lyricssource.LyricsVersion{Kind: versionKind, Label: versionLabel},
				Performers:           performers,
				RubyGeneratorVersion: "kagome-ipadic-v1",
				Lines: []lyricssource.StructuredLine{{
					Japanese: song.text, Segments: []lyricssource.LyricsSegment{{
						Text: song.text, PerformerIDs: append([]string{}, performerIDs...), Ruby: []lyricssource.RubySpan{{Text: song.text, Reading: "よみ"}},
					}}, TrailingPerformerIDs: append([]string{}, performerIDs...),
				}},
			},
		}
		if song.translation != "" {
			fixed.Translations = []string{song.translation}
		}
		if provider == model.LyricsSourceProviderSekaipedia {
			if len(allEvidence) < 2 {
				t.Fatal("Sekaipedia fixed fixture requires List and song revision evidence")
			}
			fixed.RawSHA256 = hex.EncodeToString(contentSHA256[:])
			fixed.Wikitext = lyricssource.SekaipediaFixedJapaneseWikitext(fixed.Extraction.Lines)
			if len(fixed.Wikitext) == 0 {
				t.Fatal("Sekaipedia fixed fixture produced no selected-Japanese bytes")
			}
			fixed.IndexEvidence = []lyricssource.IndexEvidence{
				allEvidence[len(allEvidence)-2], allEvidence[len(allEvidence)-1],
			}
		}
		if song.authoritativeVocaloidSegmentation {
			textRunes := []rune(song.text)
			if len(textRunes) < 2 {
				t.Fatal("authoritative Vocaloid fixture requires splittable text")
			}
			splitAt := len(textRunes) / 2
			firstSegmentText, secondSegmentText := string(textRunes[:splitAt]), string(textRunes[splitAt:])
			fixed.Extraction.Lines[0].Segments = []lyricssource.LyricsSegment{
				{Text: firstSegmentText, PerformerIDs: []string{song.sourceID}, Ruby: []lyricssource.RubySpan{{Text: firstSegmentText, Reading: "よみ"}}},
				{Text: secondSegmentText, PerformerIDs: []string{"rin"}, Ruby: []lyricssource.RubySpan{{Text: secondSegmentText, Reading: "よみ"}}},
			}
			fixed.Extraction.Lines[0].TrailingPerformerIDs = []string{song.sourceID}
			fixedIdentity := model.LyricsSourceFixedIdentity{
				Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID,
				RevisionID: candidate.RevisionID, RevisionTimestamp: candidate.RevisionTimestamp,
				SHA1: candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL,
				FetchedAt: fetchedAt.Format(time.RFC3339Nano), Categories: append([]string{}, candidate.Categories...),
				Section: candidate.Section, RenditionKey: candidate.RenditionKey,
				CompositionRenditionKey: candidate.RenditionKey, VersionReason: candidate.VersionReason,
				IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...),
			}
			component := model.LyricsSourceComponentRef{RenditionKey: candidate.RenditionKey}
			document := model.LyricsSourceDocument{
				SchemaVersion:   model.LyricsSourceDocumentSchemaVersion,
				ReasonCode:      candidate.VersionReason,
				FixedIdentities: []model.LyricsSourceFixedIdentity{fixedIdentity},
				Provenance: model.LyricsSourceComponentProvenance{
					FullText: component, PerformerSegmentation: &component, Ruby: &component, VersionEvidence: component,
				},
				Full: model.LyricsSourceFull{
					Version: model.LyricsSourceVersion{Kind: "vocaloid", Label: versionLabel},
					Performers: []model.LyricsSourcePerformer{
						{PerformerID: song.sourceID, Name: song.sourceID, Color: "#33CCBB"},
						{PerformerID: "rin", Name: "rin", Color: "#FFCC11"},
					},
					RubyGeneratorVersion: "kagome-ipadic-v1",
					Lines: []model.LyricsSourceFullLine{{
						ID: "full-000001", Text: song.text,
						Segments: []model.LyricsSourceSegment{
							{Text: firstSegmentText, PerformerIDs: []string{song.sourceID}, Ruby: []model.LyricsSourceRubySpan{{Text: firstSegmentText, Reading: "よみ"}}},
							{Text: secondSegmentText, PerformerIDs: []string{"rin"}, Ruby: []model.LyricsSourceRubySpan{{Text: secondSegmentText, Reading: "よみ"}}},
						},
						TrailingPerformerIDs: []string{song.sourceID},
					}},
				},
				PrivateReview: &model.LyricsSourcePrivateReview{
					PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured,
				},
			}
			fixed.FixedIdentities = []model.LyricsSourceFixedIdentity{fixedIdentity}
			fixed.Document = &document
		}
		draft, err := lyricsstaging.BuildDraft(item, lyricsstaging.CatalogIdentity{
			MusicID: identity.MusicID, JapaneseTitle: identity.JapaneseTitle, ProducerMetadata: identity.ProducerMetadata,
			Lyricist: identity.Lyricist, Composer: identity.Composer, Arranger: identity.Arranger,
			Vocals: append([]model.CatalogVocalSignal{}, identity.Vocals...), CatalogFingerprint: identity.CatalogFingerprint,
		}, fixed)
		if err != nil {
			t.Fatal(err)
		}
		drafts[index] = draft
	}
	report.Summary.UniqueComplete = len(report.UniqueComplete)
	receipt, err := lyricsstaging.NewPrivateEvidenceReceipt(allEvidence)
	if err != nil {
		t.Fatal(err)
	}
	report.EvidenceReceipt = &receipt
	manifest, err := lyricsstaging.NewManifest(report, strings.Repeat("a", 64), drafts)
	if err != nil {
		t.Fatal(err)
	}
	return s, database, manifest, receipt
}

func TestImportStagedLyricsManifestPreservesSourceTranslationInPrivateDraft(t *testing.T) {
	s, database, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{
		musicID: 10, title: "翻訳付き試験曲", text: "初音歌う", translation: "初音未来歌唱", sourceID: "miku",
	}})
	if len(manifest.Items) != 1 || len(manifest.Items[0].Translations) != 1 ||
		manifest.Items[0].Translations[0] != "初音未来歌唱" {
		t.Fatalf("staging manifest lost the exact source translation: %+v", manifest.Items)
	}
	results, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(
		context.Background(), manifest, receipt, "offline-operator",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Changed || len(results[0].Lyrics.Lines) != 1 ||
		results[0].Lyrics.Lines[0].Chinese != "初音未来歌唱" || results[0].Lyrics.Lines[0].English != "" {
		t.Fatalf("private staged translation import=%+v", results)
	}
	var chinese, english string
	if err := database.QueryRow(`SELECT zh_cn,en_us FROM song_lyric_lines WHERE music_id=10 AND position=0`).
		Scan(&chinese, &english); err != nil || chinese != "初音未来歌唱" || english != "" {
		t.Fatalf("stored staged translation chinese=%q english=%q err=%v", chinese, english, err)
	}
	replayed, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(
		context.Background(), manifest, receipt, "offline-operator",
	)
	if err != nil || len(replayed) != 1 || replayed[0].Changed ||
		replayed[0].Lyrics.Lines[0].Chinese != "初音未来歌唱" {
		t.Fatalf("translated manifest replay=%+v err=%v", replayed, err)
	}
}

func TestImportStagedLyricsManifestCreatesPrivateEditableDraftAndReplays(t *testing.T) {
	s, database, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{musicID: 10, title: "合成試験曲", text: "初音歌う", sourceID: "miku"}})
	results, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Changed || results[0].Lyrics.Revision != 1 || results[0].Lyrics.Status != "draft" {
		t.Fatalf("first import results=%+v", results)
	}
	lyrics := results[0].Lyrics
	if lyrics.Attribution != "" || lyrics.SourceNote != "" || lyrics.LicenseNote != "" ||
		lyrics.SourceFetchedAt != "2026-07-30T12:34:57Z" || len(lyrics.Lines) != 1 ||
		lyrics.Lines[0].Chinese != "" || lyrics.Lines[0].English != "" || len(lyrics.Lines[0].Segments) != 1 ||
		fmt.Sprint(lyrics.Lines[0].Segments[0].PerformerIDs) != "[21]" ||
		len(lyrics.Lines[0].Segments[0].Ruby) != 1 || lyrics.Lines[0].Segments[0].Ruby[0].Text != "初音歌う" {
		t.Fatalf("imported private draft=%+v", lyrics)
	}
	var publications, audits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM song_lyrics_publications`).Scan(&publications); err != nil || publications != 0 {
		t.Fatalf("publications=%d err=%v", publications, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.import_stage'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("import audits=%d err=%v", audits, err)
	}
	var sourceDocuments, sourceArtifacts, evidenceLinks, contributions int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_documents),
		(SELECT COUNT(*) FROM song_lyrics_source_artifacts),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
		(SELECT COUNT(*) FROM song_lyrics_component_contributions)`).Scan(
		&sourceDocuments, &sourceArtifacts, &evidenceLinks, &contributions); err != nil {
		t.Fatal(err)
	}
	if sourceDocuments != 1 || sourceArtifacts != len(manifest.Items[0].Artifacts) ||
		evidenceLinks != len(manifest.Items[0].Artifacts[0].Identity.IndexEvidenceRefs) || contributions < 2 {
		t.Fatalf("source provenance documents=%d artifacts=%d evidenceLinks=%d contributions=%d",
			sourceDocuments, sourceArtifacts, evidenceLinks, contributions)
	}
	var linkedProvider, linkedEvidenceID, linkedSHA256 string
	var linkedRaw []byte
	if err := database.QueryRow(`SELECT link.provider,link.evidence_id,link.sha256,evidence.raw_bytes
		FROM song_lyrics_source_artifact_index_evidence link
		JOIN lyrics_source_index_evidence evidence
		  ON evidence.provider=link.provider AND evidence.evidence_id=link.evidence_id AND evidence.sha256=link.sha256
		WHERE link.position=0`).Scan(&linkedProvider, &linkedEvidenceID, &linkedSHA256, &linkedRaw); err != nil {
		t.Fatal(err)
	}
	wantEvidence := receipt.IndexEvidence[0]
	if linkedProvider != string(wantEvidence.Provider) || linkedEvidenceID != wantEvidence.EvidenceID ||
		linkedSHA256 != wantEvidence.SHA256 || string(linkedRaw) != string(wantEvidence.Raw) {
		t.Fatalf("linked evidence provider=%q id=%q sha=%q raw=%q want=%+v",
			linkedProvider, linkedEvidenceID, linkedSHA256, linkedRaw, wantEvidence)
	}
	if _, err := database.Exec(`UPDATE song_lyrics_source_documents SET document_sha256=?`, strings.Repeat("f", 64)); err == nil {
		t.Fatal("immutable source document allowed checksum mutation")
	}
	if _, err := database.Exec(`DELETE FROM song_lyrics_source_artifacts`); err == nil {
		t.Fatal("immutable source artifact allowed deletion while its document exists")
	}

	replay, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator")
	if err != nil || len(replay) != 1 || replay[0].Changed || replay[0].Lyrics.Revision != 1 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.import_stage'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("replay audits=%d err=%v", audits, err)
	}
}

func TestImportStagedLyricsManifestRejectsOmittedCrossedDuplicateAndOrphanEvidenceReceipts(t *testing.T) {
	t.Run("omitted successful evidence", func(t *testing.T) {
		s, _, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{
			{musicID: 10, title: "第一曲", text: "第一行", sourceID: "miku"},
			{musicID: 20, title: "第二曲", text: "第二行", sourceID: "miku"},
		})
		omitted, err := lyricsstaging.NewPrivateEvidenceReceipt(receipt.IndexEvidence[:1])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, omitted, "offline-operator"); err == nil || !strings.Contains(err.Error(), "candidate reference is unresolved") {
			t.Fatalf("omitted successful evidence error=%v", err)
		}
	})

	t.Run("crossed receipt", func(t *testing.T) {
		s, _, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{{
			musicID: 10, title: "第一曲", text: "第一行", sourceID: "miku",
		}})
		_, _, _, crossed := setupStagedManifestImportStore(t, []stagedImportSong{{
			musicID: 20, title: "第二曲", text: "第二行", sourceID: "miku",
		}})
		if _, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, crossed, "offline-operator"); err == nil || !strings.Contains(err.Error(), "candidate reference is unresolved") {
			t.Fatalf("crossed receipt error=%v", err)
		}
	})

	t.Run("extra projected evidence remains an import orphan", func(t *testing.T) {
		s, _, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{
			musicID: 10, title: "第一曲", text: "第一行", sourceID: "miku",
		}})
		_, _, _, extra := setupStagedManifestImportStore(t, []stagedImportSong{{
			musicID: 20, title: "第二曲", text: "第二行", sourceID: "miku",
		}})
		evidence := append([]lyricssource.IndexEvidence(nil), receipt.IndexEvidence...)
		evidence = append(evidence, extra.IndexEvidence...)
		orphaned, err := lyricsstaging.NewPrivateEvidenceReceipt(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, orphaned, "offline-operator"); err == nil || !strings.Contains(err.Error(), "private evidence receipt contains orphan evidence") {
			t.Fatalf("extra projected import evidence error=%v", err)
		}
	})

	t.Run("duplicate evidence ID", func(t *testing.T) {
		s, _, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{
			musicID: 10, title: "第一曲", text: "第一行", sourceID: "miku",
		}})
		duplicated := receipt
		duplicated.IndexEvidence = append([]lyricssource.IndexEvidence(nil), receipt.IndexEvidence...)
		duplicated.IndexEvidence = append(duplicated.IndexEvidence, receipt.IndexEvidence[0])
		if _, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, duplicated, "offline-operator"); err == nil || !strings.Contains(err.Error(), "not uniquely ordered by evidence ID") {
			t.Fatalf("duplicate receipt evidence ID error=%v", err)
		}
	})
}

func TestStagedManifestLyricsDraftRequiresExactAuthoritativeVocaloidSegmentationMarker(t *testing.T) {
	s, database, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{{
		musicID: 10, title: "Sekaipedia権威曲", text: "初音歌う", sourceID: "miku",
		versionKind: "vocaloid", catalogVocalType: "original_song",
		provider: model.LyricsSourceProviderSekaipedia, authoritativeVocaloidSegmentation: true,
	}})
	identity, err := s.CatalogMusicIdentity(10)
	if err != nil {
		t.Fatal(err)
	}
	performers, err := loadCatalogPerformerAliases(database)
	if err != nil {
		t.Fatal(err)
	}
	for name, privateReview := range map[string]*model.LyricsSourcePrivateReview{
		"missing": nil,
		"different": {
			PerformerSegmentationEvidence: model.LyricsSourcePerformerSegmentationEvidence("reviewed_structured"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			staged := manifest.Items[0]
			staged.Document.PrivateReview = privateReview
			if _, err := stagedManifestLyricsDraft(staged, identity.Vocals, performers); !errors.Is(err, ErrLyricsStagedManifestDrift) {
				t.Fatalf("Vocaloid segmentation with %s marker error=%v", name, err)
			}
		})
	}
}

func TestImportStagedSekaipediaAuthoritativeVocaloidPersistsTimestampSegmentationAndReplays(t *testing.T) {
	s, database, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{
		musicID: 10, title: "Sekaipedia権威曲", text: "初音歌う", sourceID: "miku",
		versionKind: "vocaloid", catalogVocalType: "original_song",
		provider: model.LyricsSourceProviderSekaipedia, authoritativeVocaloidSegmentation: true,
	}})
	results, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator")
	if err != nil || len(results) != 1 || !results[0].Changed {
		t.Fatalf("Sekaipedia authoritative import=%+v err=%v", results, err)
	}
	segments := results[0].Lyrics.Lines[0].Segments
	if len(segments) != 2 || fmt.Sprint(segments[0].PerformerIDs) != "[21]" ||
		fmt.Sprint(segments[1].PerformerIDs) != "[22]" {
		t.Fatalf("authoritative Vocaloid performer projection=%+v lyrics=%+v", segments, results[0].Lyrics)
	}
	wantIdentity := manifest.Items[0].Artifacts[0].Identity
	wantTimestamp := wantIdentity.RevisionTimestamp
	var evidenceTimestamp, artifactScalarTimestamp, artifactIdentityTimestamp, documentTimestamp, privateMarker string
	var compositionScalar, compositionIdentity, versionReasonScalar, versionReasonIdentity string
	if err := database.QueryRow(`SELECT evidence.revision_timestamp,
		artifact.revision_timestamp,
		json_extract(artifact.fixed_identity_json,'$.revisionTimestamp'),
		json_extract(document.document_json,'$.fixedIdentities[0].revisionTimestamp'),
		json_extract(document.document_json,'$.privateReview.performerSegmentationEvidence'),
		artifact.composition_rendition_key,
		json_extract(artifact.fixed_identity_json,'$.compositionRenditionKey'),
		artifact.version_reason,
		json_extract(artifact.fixed_identity_json,'$.versionReason')
		FROM lyrics_source_index_evidence evidence
		JOIN song_lyrics_source_artifact_index_evidence link
		  ON link.provider=evidence.provider AND link.evidence_id=evidence.evidence_id AND link.sha256=evidence.sha256
		JOIN song_lyrics_source_artifacts artifact
		  ON artifact.document_id=link.document_id AND artifact.rendition_key=link.rendition_key
		JOIN song_lyrics_source_documents document ON document.document_id=artifact.document_id
		WHERE evidence.provider='sekaipedia' AND evidence.page_id=?`, wantIdentity.PageID).Scan(
		&evidenceTimestamp, &artifactScalarTimestamp, &artifactIdentityTimestamp, &documentTimestamp, &privateMarker,
		&compositionScalar, &compositionIdentity, &versionReasonScalar, &versionReasonIdentity); err != nil {
		t.Fatal(err)
	}
	if wantTimestamp == "" || evidenceTimestamp != wantTimestamp || artifactScalarTimestamp != wantTimestamp ||
		artifactIdentityTimestamp != wantTimestamp || documentTimestamp != wantTimestamp ||
		compositionScalar != wantIdentity.CompositionRenditionKey || compositionIdentity != wantIdentity.CompositionRenditionKey ||
		versionReasonScalar != string(wantIdentity.VersionReason) || versionReasonIdentity != string(wantIdentity.VersionReason) ||
		privateMarker != string(model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured) {
		t.Fatalf("Sekaipedia transport evidence=%q artifactScalar=%q artifactIdentity=%q document=%q composition=%q/%q versionReason=%q/%q marker=%q want=%+v",
			evidenceTimestamp, artifactScalarTimestamp, artifactIdentityTimestamp, documentTimestamp,
			compositionScalar, compositionIdentity, versionReasonScalar, versionReasonIdentity, privateMarker, wantIdentity)
	}
	var parents, links, orphanLinks int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM lyrics_source_index_evidence WHERE provider='sekaipedia'),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence WHERE provider='sekaipedia'),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence link
		 LEFT JOIN lyrics_source_index_evidence evidence
		   ON evidence.provider=link.provider AND evidence.evidence_id=link.evidence_id AND evidence.sha256=link.sha256
		 WHERE link.provider='sekaipedia' AND evidence.evidence_id IS NULL)`).Scan(&parents, &links, &orphanLinks); err != nil {
		t.Fatal(err)
	}
	if parents != 2 || links != 2 || orphanLinks != 0 {
		t.Fatalf("Sekaipedia evidence graph parents=%d links=%d orphans=%d", parents, links, orphanLinks)
	}
	replay, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator")
	if err != nil || len(replay) != 1 || replay[0].Changed ||
		len(replay[0].Lyrics.Lines[0].Segments) != 2 ||
		fmt.Sprint(replay[0].Lyrics.Lines[0].Segments[0].PerformerIDs) != "[21]" ||
		fmt.Sprint(replay[0].Lyrics.Lines[0].Segments[1].PerformerIDs) != "[22]" {
		t.Fatalf("Sekaipedia authoritative replay=%+v err=%v", replay, err)
	}
	var conflictingEvidence lyricssource.IndexEvidence
	for _, evidence := range receipt.IndexEvidence {
		if evidence.PageID == manifest.Items[0].Artifacts[0].Identity.PageID {
			conflictingEvidence = evidence
			break
		}
	}
	if conflictingEvidence.EvidenceID == "" {
		t.Fatal("Sekaipedia song revision evidence is missing from the receipt")
	}
	conflictingEvidence.RevisionTimestamp = "2026-07-30T12:33:56Z"
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOrVerifyLyricsIndexEvidenceTx(context.Background(), tx, conflictingEvidence, time.Now()); !errors.Is(err, ErrLyricsSourceArtifactConflict) {
		tx.Rollback()
		t.Fatalf("Sekaipedia immutable evidence conflict=%v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestStagedLyricsSourceDocumentMatchDetectsExistingSourceV3LocalizationDrift(t *testing.T) {
	s := setupLyricsStore(t)
	document, evidenceByIdentity := renditionV3PersistenceDocument(t)
	translations := []lyricsstaging.RenditionTranslation{
		{
			RenditionKey: document.Renditions[0].RenditionKey,
			Translations: []string{"主译文一", "主译文二"},
			PeerTranslations: []lyricsstaging.RenditionPeerTranslation{{
				Side: "game", Locale: "zh-CN", Translations: []string{"游戏译文一", "游戏译文二"},
			}},
		},
		{RenditionKey: document.Renditions[1].RenditionKey, Translations: []string{"虚拟歌手译文"}},
	}
	artifacts := make([]lyricsstaging.Artifact, len(document.FixedIdentities))
	for index, identity := range document.FixedIdentities {
		artifact, err := lyricsstaging.NewRecoveryArtifact(identity, evidenceByIdentity[index][0].Raw)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[index] = artifact
	}
	draft, err := lyricsstaging.BuildRecoveryPeerDraft(
		10, "新曲", recoveryRenditionTestCatalogFingerprint(t, s, 10), 10, []int{}, document, artifacts, translations,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertStagedV3Draft(ctx, tx, draft, "staged-replay-test", strings.Repeat("b", 64), time.Now().Unix(), false); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	exists, matched, err := stagedLyricsSourceDocumentMatches(context.Background(), tx, draft, false)
	_ = tx.Rollback()
	if err != nil || !exists || !matched {
		t.Fatalf("exact source-v3 replay exists=%t matched=%t err=%v", exists, matched, err)
	}

	if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_side_translation_lines
		SET text='篡改的 Game 译文' WHERE side='game' AND position=0`); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	exists, matched, err = stagedLyricsSourceDocumentMatches(context.Background(), tx, draft, false)
	_ = tx.Rollback()
	if err != nil || !exists || matched {
		t.Fatalf("drifted source-v3 replay exists=%t matched=%t err=%v", exists, matched, err)
	}
}

func TestImportStagedLyricsManifestEvidenceReceiptAndCommitHookShareOneTransaction(t *testing.T) {
	s, _, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{
		musicID: 10, title: "合成試験曲", text: "初音歌う", sourceID: "miku",
	}})
	verifyHook := func(tx *sql.Tx, results []StagedLyricsImportItem) error {
		if len(results) != 1 {
			return fmt.Errorf("hook results=%d", len(results))
		}
		var parents, links int
		var raw []byte
		if err := tx.QueryRow(`SELECT
			(SELECT COUNT(*) FROM lyrics_source_index_evidence),
			(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
			(SELECT raw_bytes FROM lyrics_source_index_evidence LIMIT 1)`).Scan(&parents, &links, &raw); err != nil {
			return err
		}
		if parents != 1 || links != 1 || string(raw) != string(receipt.IndexEvidence[0].Raw) {
			return fmt.Errorf("hook evidence parents=%d links=%d raw=%q", parents, links, raw)
		}
		return nil
	}
	results, commitAttempted, err := s.ImportStagedLyricsManifestWithEvidenceReceiptAndCommitHook(
		context.Background(), manifest, receipt, "offline-operator", verifyHook,
	)
	if err != nil || !commitAttempted || len(results) != 1 || !results[0].Changed {
		t.Fatalf("combined first import results=%+v commitAttempted=%t err=%v", results, commitAttempted, err)
	}
	replay, replayCommitAttempted, err := s.ImportStagedLyricsManifestWithEvidenceReceiptAndCommitHook(
		context.Background(), manifest, receipt, "offline-operator", verifyHook,
	)
	if err != nil || !replayCommitAttempted || len(replay) != 1 || replay[0].Changed {
		t.Fatalf("combined replay results=%+v commitAttempted=%t err=%v", replay, replayCommitAttempted, err)
	}
}

func TestImportStagedVocaloidFullNeverAppliesCatalogPerformerFallbackAndReplays(t *testing.T) {
	s, database, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{
		musicID: 10, title: "Vocaloid限定曲", text: "初音歌う", sourceID: "", versionKind: "vocaloid",
		catalogVocalType: "original_song",
	}})
	results, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator")
	if err != nil || len(results) != 1 || !results[0].Changed {
		t.Fatalf("Vocaloid import=%+v err=%v", results, err)
	}
	line := results[0].Lyrics.Lines[0]
	if len(line.Segments) != 1 || line.Segments[0].Text != line.Japanese ||
		line.Segments[0].PerformerIDs == nil || len(line.Segments[0].PerformerIDs) != 0 ||
		manifest.Items[0].Document.Full.Performers == nil || len(manifest.Items[0].Document.Full.Performers) != 0 ||
		manifest.Items[0].Document.Provenance.PerformerSegmentation != nil {
		t.Fatalf("Vocaloid Full segmentation changed: lyrics=%+v document=%+v", line, manifest.Items[0].Document)
	}
	var performerJSON string
	if err := database.QueryRow(`SELECT performer_ids_json FROM song_lyric_segments WHERE music_id=10`).Scan(&performerJSON); err != nil {
		t.Fatal(err)
	}
	if performerJSON != "[]" {
		t.Fatalf("Vocaloid Full persisted performers=%q", performerJSON)
	}
	replay, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator")
	if err != nil || len(replay) != 1 || replay[0].Changed ||
		len(replay[0].Lyrics.Lines[0].Segments[0].PerformerIDs) != 0 {
		t.Fatalf("Vocaloid replay=%+v err=%v", replay, err)
	}
}

func TestStagedLyricsSourceProvenanceSurvivesTransactionalContentBackupRestore(t *testing.T) {
	s, database, manifest, receipt := setupStagedManifestImportStore(t, []stagedImportSong{{musicID: 10, title: "合成試験曲", text: "初音歌う", sourceID: "miku"}})
	if _, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator"); err != nil {
		t.Fatal(err)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.SourceDocuments) != 1 || len(exported.SourceArtifacts) != 1 ||
		len(exported.SourceIndexEvidence) != 1 || len(exported.SourceArtifactEvidence) != 1 ||
		len(exported.SourceContributions) < 2 {
		t.Fatalf("exported source provenance=%+v/%+v/%+v/%+v/%+v", exported.SourceDocuments,
			exported.SourceArtifacts, exported.SourceIndexEvidence, exported.SourceArtifactEvidence, exported.SourceContributions)
	}
	parent := exported.SourceIndexEvidence[0]
	wantEvidence := receipt.IndexEvidence[0]
	if parent.Provider != string(wantEvidence.Provider) || parent.EvidenceID != wantEvidence.EvidenceID ||
		parent.SHA256 != wantEvidence.SHA256 || parent.RawSHA256 != wantEvidence.RawSHA256 ||
		parent.RawByteCount != len(wantEvidence.Raw) || string(parent.RawBytes) != string(wantEvidence.Raw) || parent.CreatedAt <= 0 {
		t.Fatalf("exported parent evidence=%+v want=%+v", parent, wantEvidence)
	}
	link := exported.SourceArtifactEvidence[0]
	wantRef := manifest.Items[0].Artifacts[0].Identity.IndexEvidenceRefs[0]
	if link.DocumentID != exported.SourceDocuments[0].DocumentID ||
		link.RenditionKey != manifest.Items[0].Artifacts[0].Identity.RenditionKey || link.Position != 0 ||
		link.Provider != string(manifest.Items[0].Artifacts[0].Identity.Provider) ||
		link.EvidenceID != wantRef.EvidenceID || link.SHA256 != wantRef.SHA256 {
		t.Fatalf("exported artifact evidence link=%+v want provider=%q ref=%+v", link,
			manifest.Items[0].Artifacts[0].Identity.Provider, wantRef)
	}
	events := EventContentExport{Segments: []EventSegmentRecord{}, Localizations: []EventLocalizationRecord{},
		LocaleMeta: []EventLocaleMetaRecord{}, Scenarios: []EventScenarioRecord{}}
	missingLinks := exported
	missingLinks.SourceArtifactEvidence = nil
	if err := s.ImportTranslationContent(nil, events, missingLinks); err == nil ||
		!strings.Contains(err.Error(), "incomplete evidence links") {
		t.Fatalf("restore accepted missing artifact evidence links: %v", err)
	}
	if err := s.ImportTranslationContent(nil, events, exported); err != nil {
		t.Fatalf("restore source provenance: %v", err)
	}
	var restoredDocuments, restoredLinks int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_documents),
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence)`).Scan(&restoredDocuments, &restoredLinks); err != nil ||
		restoredDocuments != 1 || restoredLinks != 1 {
		t.Fatalf("restored source documents=%d evidenceLinks=%d err=%v", restoredDocuments, restoredLinks, err)
	}
	if replay, err := s.ImportStagedLyricsManifestWithEvidenceReceipt(context.Background(), manifest, receipt, "offline-operator"); err != nil || len(replay) != 1 || replay[0].Changed {
		t.Fatalf("replay after restore=%+v err=%v", replay, err)
	}
}

func TestImportStagedLyricsManifestPreservesEachFixedRevisionFetchTime(t *testing.T) {
	s, _, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{
		{musicID: 10, title: "第一曲", text: "第一行", sourceID: "miku"},
		{musicID: 20, title: "第二曲", text: "第二行", sourceID: "miku"},
	})
	results, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Lyrics.SourceFetchedAt != "2026-07-30T12:34:57Z" ||
		results[1].Lyrics.SourceFetchedAt != "2026-07-30T12:35:57Z" ||
		results[0].Lyrics.SourceFetchedAt == manifest.Preflight.GeneratedAt ||
		results[1].Lyrics.SourceFetchedAt == manifest.Preflight.GeneratedAt {
		t.Fatalf("per-item fetched times=%q/%q preflight=%q", results[0].Lyrics.SourceFetchedAt,
			results[1].Lyrics.SourceFetchedAt, manifest.Preflight.GeneratedAt)
	}
}

func TestImportStagedLyricsManifestConflictsWithNonidenticalExistingDraft(t *testing.T) {
	s, _, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{{musicID: 10, title: "競合曲", text: "歌詞", sourceID: "miku"}})
	results, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator")
	if err != nil {
		t.Fatal(err)
	}
	changed := results[0].Lyrics
	changed.Lines[0].Chinese = "已有翻译"
	if _, err := s.SaveLyrics(changed, "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator"); !errors.Is(err, ErrLyricsStagedManifestConflict) {
		t.Fatalf("nonidentical replay error=%v", err)
	}
	loaded, err := s.GetLyrics(10)
	if err != nil || loaded.Revision != 2 || loaded.Lines[0].Chinese != "已有翻译" {
		t.Fatalf("conflict changed existing draft=%+v err=%v", loaded, err)
	}
}

func TestImportStagedLyricsManifestRollsBackWholeBatch(t *testing.T) {
	s, database, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{
		{musicID: 10, title: "第一曲", text: "第一行", sourceID: "miku"},
		{musicID: 20, title: "第二曲", text: "第二行", sourceID: "miku"},
	})
	if _, err := database.Exec(`CREATE TRIGGER fail_second_staged_import BEFORE INSERT ON audit_log
		WHEN NEW.action='lyrics.import_stage' AND NEW.detail LIKE 'musicId=20 %'
		BEGIN SELECT RAISE(ABORT, 'injected staged import failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator"); err == nil || !strings.Contains(err.Error(), "injected staged import failure") {
		t.Fatalf("batch failure error=%v", err)
	}
	var lyricsCount, lineCount, segmentCount, auditCount int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM song_lyrics`:                                  &lyricsCount,
		`SELECT COUNT(*) FROM song_lyric_lines`:                             &lineCount,
		`SELECT COUNT(*) FROM song_lyric_segments`:                          &segmentCount,
		`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.import_stage'`: &auditCount,
	} {
		if err := database.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if lyricsCount != 0 || lineCount != 0 || segmentCount != 0 || auditCount != 0 {
		t.Fatalf("partial batch persisted lyrics=%d lines=%d segments=%d audits=%d", lyricsCount, lineCount, segmentCount, auditCount)
	}
}

func TestImportStagedLyricsManifestRejectsCatalogDriftAndProjectsDeclaredExternalPerformer(t *testing.T) {
	t.Run("catalog fingerprint", func(t *testing.T) {
		s, database, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{{musicID: 10, title: "漂移曲", text: "歌詞", sourceID: "miku"}})
		if _, err := database.Exec(`UPDATE catalog_music SET composer='改変後' WHERE music_id=10`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator"); !errors.Is(err, ErrLyricsStagedManifestDrift) {
			t.Fatalf("catalog drift error=%v", err)
		}
		if _, err := s.GetLyrics(10); !errors.Is(err, ErrLyricsNotFound) {
			t.Fatalf("catalog drift created draft: %v", err)
		}
	})

	t.Run("audited external performer uses reserved lyrics-only ID", func(t *testing.T) {
		s, database, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{{musicID: 10, title: "映射曲", text: "歌詞", sourceID: "外部歌唱者-01"}})
		results, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator")
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || !results[0].Changed ||
			fmt.Sprint(results[0].Lyrics.Lines[0].Segments[0].PerformerIDs) != "[1001]" {
			t.Fatalf("audited external performer projection=%+v", results)
		}
		var performerJSON string
		if err := database.QueryRow(`SELECT performer_ids_json FROM song_lyric_segments WHERE music_id=10`).Scan(&performerJSON); err != nil {
			t.Fatal(err)
		}
		if performerJSON != "[1001]" {
			t.Fatalf("audited external persisted performers=%q", performerJSON)
		}
		validPerformers, err := s.performerIDs(database)
		if err != nil {
			t.Fatal(err)
		}
		if code, details, _ := validateLyrics(results[0].Lyrics, validPerformers, false); code != "" {
			t.Fatalf("audited external draft validation code=%q details=%v", code, details)
		}
	})

	t.Run("unknown declared external performer remains an editable unpublished draft", func(t *testing.T) {
		s, database, manifest, _ := setupStagedManifestImportStore(t, []stagedImportSong{{musicID: 10, title: "映射曲", text: "歌詞", sourceID: "external_singer"}})
		results, err := s.ImportStagedLyricsManifest(context.Background(), manifest, "offline-operator")
		if err != nil {
			t.Fatal(err)
		}
		performerIDs := results[0].Lyrics.Lines[0].Segments[0].PerformerIDs
		if len(results) != 1 || !results[0].Changed || performerIDs == nil || len(performerIDs) != 0 {
			t.Fatalf("unknown external performer projection=%+v", results)
		}
		var performerJSON string
		if err := database.QueryRow(`SELECT performer_ids_json FROM song_lyric_segments WHERE music_id=10`).Scan(&performerJSON); err != nil {
			t.Fatal(err)
		}
		if performerJSON != "[]" {
			t.Fatalf("unknown external persisted performers=%q", performerJSON)
		}
		validPerformers, err := s.performerIDs(database)
		if err != nil {
			t.Fatal(err)
		}
		if code, details, _ := validateLyrics(results[0].Lyrics, validPerformers, true); code != "" {
			t.Fatalf("unknown external draft publication code=%q details=%v", code, details)
		}
	})
}

func TestMapDeclaredLyricsSourcePerformerIDsRejectsUndeclaredLabel(t *testing.T) {
	if _, err := mapDeclaredLyricsSourcePerformerIDs(
		[]string{"undeclared_singer"},
		map[string]int{"miku": 21},
		map[string]bool{"external_singer": true},
	); !errors.Is(err, ErrLyricsSourcePerformerMapping) {
		t.Fatalf("undeclared performer error=%v", err)
	}
}

package store

import (
	"errors"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

func TestPublishedSourceOnlyLyricsCarrySekaipediaAttribution(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.Attribution = ""
	input.TranslationCredit = ""
	input.ProofreadingCredit = ""
	input.SourceURL = "https://www.sekaipedia.org/wiki/Test_Song?oldid=123"
	input.SourcePageID = 456
	input.SourceRevisionID = 123
	input.SourceSHA1 = strings.Repeat("a", 40)
	input.SourceFetchedAt = "2026-08-16T00:00:00Z"
	saved, _, err := s.SaveImportedLyricsMutation(input, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatalf("publish source-only sekaipedia attribution: %v", err)
	}
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	detail := details[saved.MusicID]
	if detail.Attribution != "" || len(detail.Attributions) != 1 {
		t.Fatalf("source-only detail attribution=%q attributions=%+v", detail.Attribution, detail.Attributions)
	}
	attribution := detail.Attributions[0]
	if attribution.Provider != model.LyricsSourceProviderSekaipedia || attribution.Title != "Test Song" ||
		attribution.RevisionID != 123 || attribution.RevisionURL != input.SourceURL ||
		attribution.LicenseName != "CC BY-SA 4.0" || attribution.LicenseURL != "https://creativecommons.org/licenses/by-sa/4.0/" {
		t.Fatalf("source-only attribution=%+v", attribution)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, saved.MusicID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `"licenseName":"CC BY-SA 4.0"`) ||
		!strings.Contains(stored, `"sourceRevisionId":123`) {
		t.Fatalf("stored v1 payload lacks source fields: %s", stored)
	}

	proofreading := validLyrics()
	proofreading.Attribution = ""
	proofreading.TranslationCredit = ""
	proofreading.ProofreadingCredit = "Proofreader"
	proofreading.SourceURL = input.SourceURL
	proofreading.SourcePageID = input.SourcePageID
	proofreading.SourceRevisionID = input.SourceRevisionID
	proofreading.SourceSHA1 = input.SourceSHA1
	proofreading.SourceFetchedAt = input.SourceFetchedAt
	proofreading.MusicID = 20
	savedProof, _, err := s.SaveImportedLyricsMutation(proofreading, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishLyrics(savedProof.MusicID, savedProof.Revision)
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "incomplete_publication" {
		t.Fatalf("proofreading-only with source must stay unpublished, err=%v", err)
	}

	if err := s.UpsertMusicCatalog([]MusicCatalogRecord{{MusicID: 30, JapaneseTitle: "Stardust Rain"}}); err != nil {
		t.Fatal(err)
	}
	fandomInput := validLyrics()
	fandomInput.Attribution = ""
	fandomInput.TranslationCredit = ""
	fandomInput.ProofreadingCredit = ""
	fandomInput.SourceURL = "https://vocaloid.fandom.com/wiki/Stardust_Rain?oldid=1493252"
	fandomInput.SourcePageID = 265789
	fandomInput.SourceRevisionID = 1493252
	fandomInput.SourceSHA1 = strings.Repeat("f", 40)
	fandomInput.SourceFetchedAt = "2026-08-17T07:00:00Z"
	fandomInput.MusicID = 30
	savedFandom, _, err := s.SaveImportedLyricsMutation(fandomInput, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(savedFandom.MusicID, savedFandom.Revision); err != nil {
		t.Fatalf("fandom source-only publish failed: %v", err)
	}
	_, fandomDetails, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	fandomDetail := fandomDetails[savedFandom.MusicID]
	if len(fandomDetail.Attributions) != 1 {
		t.Fatalf("fandom attributions=%+v", fandomDetail.Attributions)
	}
	fandomAttr := fandomDetail.Attributions[0]
	if fandomAttr.Provider != model.LyricsSourceProviderVocaloidFandom ||
		fandomAttr.LicenseName != "CC BY-SA 3.0" || fandomAttr.LicenseURL != "https://creativecommons.org/licenses/by-sa/3.0/" {
		t.Fatalf("fandom attribution mismatch=%+v", fandomAttr)
	}
}

func TestPublishedLyricsLocalizationProjection(t *testing.T) {
	s := setupLyricsStore(t)
	document, evidenceByIdentity := renditionV3PersistenceDocument(t)
	sekaiRendition := document.Renditions[0]
	if sekaiRendition.Full == nil {
		t.Fatalf("fixture sekai rendition has no full side")
	}
	fullTranslations := make([]string, len(sekaiRendition.Full.Lines))
	for index := range fullTranslations {
		fullTranslations[index] = "译文-" + sekaiRendition.Full.Lines[index].ID
	}
	translations := []lyricscontract.RenditionTranslation{
		{
			RenditionKey:      sekaiRendition.RenditionKey,
			Translations:      fullTranslations,
			TranslationCredit: "雪莹ちゃん",
		},
		{RenditionKey: document.Renditions[1].RenditionKey},
	}
	if err := insertRenditionV3PersistenceGraph(t, s, document, evidenceByIdentity, translations); err != nil {
		t.Fatal(err)
	}
	var documentID int64
	if err := s.db.QueryRow(`SELECT document_id FROM song_lyrics_source_documents WHERE music_id=10`).Scan(&documentID); err != nil {
		t.Fatal(err)
	}
	bump := func(revision int) {
		if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_localizations SET revision=? WHERE document_id=?`, revision, documentID); err != nil {
			t.Fatal(err)
		}
	}

	// Recovery batch state (revision 1) must not be projected.
	bump(1)
	index, details, v4Details, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range index {
		if item.MusicID == 10 {
			t.Fatalf("revision-1 localization 10 projected in index: %+v", item)
		}
	}
	if _, ok := details[10]; ok {
		t.Fatalf("revision-1 localization 10 projected in details")
	}
	if _, ok := v4Details[10]; ok {
		t.Fatalf("revision-1 localization 10 projected in v4Details")
	}

	// An edited localization (revision > 1) with a credit is projected as a
	// validated public v3 complete entry.
	bump(2)
	index, details, v4Details, err = s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	var entry PublicLyricsIndexSong
	var found10 bool
	for _, item := range index {
		if item.MusicID == 10 {
			entry = item
			found10 = true
			break
		}
	}
	if !found10 || details[10].MusicID != 10 {
		t.Fatalf("edited localization 10 projection missing: index=%+v details=%+v", index, details[10])
	}
	if entry.MusicID != 10 || entry.Revision != 2 || entry.State != PublicLyricsStateComplete ||
		len(entry.AvailableVersions) != 2 || entry.AvailableVersions[0] != "full" || entry.AvailableVersions[1] != "game" {
		t.Fatalf("projected index entry=%+v", entry)
	}
	detail := details[10]
	if detail.Version != 3 || detail.Revision != 2 || detail.State != PublicLyricsStateComplete ||
		len(detail.Renditions) != 2 {
		t.Fatalf("projected detail envelope=%+v", detail)
	}
	if detail.Renditions[0].Full == nil || len(detail.Renditions[0].Full.Lines) != len(fullTranslations) ||
		detail.Renditions[0].Full.Lines[0].Chinese != fullTranslations[0] ||
		detail.Renditions[0].TranslationCredits == nil ||
		detail.Renditions[0].TranslationCredits.Translation != "雪莹ちゃん" {
		t.Fatalf("projected detail localization=%+v", detail.Renditions[0])
	}

	// Create a second edition and save it to test v4 projection
	created, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: 10, Revision: 2, Operation: "create", EditionKey: "alternate", Label: "爱死天流",
	}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	alternateSave := cloneLyricsRenditionEditorDocument(t, created)
	alternateSave.Renditions[0].Full.Lines[0].Chinese = "爱死天流译文"
	alternateSave.Renditions[0].TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: "爱死天流"}
	_, _, err = s.SaveLyricsRenditionMutation(alternateSave, "bob")
	if err != nil {
		t.Fatal(err)
	}

	index, details, v4Details, err = s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	v4Doc, hasV4Doc := v4Details[10]
	if !hasV4Doc || v4Doc.Version != 4 || len(v4Doc.TranslationEditions) != 2 {
		t.Fatalf("projected v4 detail=%+v (has=%v)", v4Doc, hasV4Doc)
	}

	// Without a credit the edited document stays bundle-served. Clearing
	// credits happens through an editor save, which bumps the revision like
	// every other localization mutation.
	if _, err := s.db.Exec(`UPDATE song_lyrics_rendition_localizations SET translation_credit='', proofreading_credit='', revision=10 WHERE document_id=?`, documentID); err != nil {
		t.Fatal(err)
	}
	index, details, v4Details, err = s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range index {
		if item.MusicID == 10 {
			t.Fatalf("creditless localization 10 projected: index=%+v", item)
		}
	}
	if _, ok := details[10]; ok {
		t.Fatalf("creditless localization 10 projected in details")
	}
	if _, ok := v4Details[10]; ok {
		t.Fatalf("creditless localization 10 projected in v4Details")
	}
}

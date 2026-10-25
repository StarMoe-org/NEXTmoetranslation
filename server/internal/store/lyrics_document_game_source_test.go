package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

// A song whose full lyrics come from one wiki and whose Game Size from another
// cites both: source.url for Full, the rendition's gameSource for Game.
const (
	lyricsDocumentGameSourceFullURL  = "https://vocaloid.fandom.com/wiki/Synthetic_%28Song%29?oldid=375274"
	lyricsDocumentGameSourceGameURL  = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242"
	lyricsDocumentGameSourceGameName = "合成試験曲"
)

func lyricsDocumentGameSourceRequest(mode string) LyricsDocumentRequest {
	request := lyricsDocumentTestRequest()
	request.Source = LyricsDocumentSource{URL: "https://vocaloid.fandom.com/wiki/Synthetic_(Song)?oldid=375274"}
	rendition := &request.Renditions[0]
	rendition.Game = mode
	rendition.GameSource = &LyricsDocumentSource{URL: lyricsDocumentGameSourceGameURL}
	switch mode {
	case "cut":
		rendition.Lines[0].InGame = true
		rendition.Lines[2].InGame = true
	case "independent":
		rendition.GameLines = []LyricsDocumentLine{
			{Japanese: "{短|みじか}い{試験|しけん}", Chinese: "短短的测试"},
			{Japanese: "おわり", Chinese: "结束", PerformerIDs: []int{2}},
		}
	}
	return request
}

func TestPublishLyricsDocumentGameSourceAttributesTheGameSideAndRoundTrips(t *testing.T) {
	for _, mode := range []string{"same", "cut", "independent"} {
		t.Run(mode, func(t *testing.T) {
			s := setupLyricsDocumentStore(t)
			result, detail := publishLyricsDocumentForTest(t, s, lyricsDocumentGameSourceRequest(mode), 0)

			gameComponents := map[string]bool{"relation": true, "game_text": true, "game_performer_segmentation": true, "game_ruby": true}
			seen := map[string]bool{}
			for _, attribution := range detail.Renditions[0].Provenance {
				component := strings.TrimPrefix(attribution.Component, "renditions/sekai/")
				seen[component] = true
				wantURL, wantProvider, wantTitle := lyricsDocumentGameSourceFullURL, model.LyricsSourceProviderVocaloidFandom, "Synthetic (Song)"
				if gameComponents[component] {
					wantURL, wantProvider, wantTitle = lyricsDocumentGameSourceGameURL, model.LyricsSourceProviderSekaipedia, lyricsDocumentGameSourceGameName
				}
				if attribution.RevisionURL != wantURL || attribution.Provider != wantProvider || attribution.Title != wantTitle {
					t.Fatalf("%s attributed to %s %s %q, want %s %s %q", component, attribution.Provider, attribution.RevisionURL,
						attribution.Title, wantProvider, wantURL, wantTitle)
				}
			}
			if !seen["full_text"] || !seen["relation"] || !seen["version"] || mode == "independent" && !seen["game_text"] {
				t.Fatalf("%s provenance components=%v", mode, seen)
			}
			if got := lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM song_lyrics_source_artifacts AS a
				JOIN song_lyrics_source_documents AS d ON d.document_id=a.document_id WHERE d.music_id=?`, lyricsDocumentTestMusicID); got != 2 {
				t.Fatalf("stored %d source artifacts, want one per revision", got)
			}
			_, details, _, err := s.PublishedLyricsLocalizationProjection()
			if err != nil {
				t.Fatal(err)
			}
			served, err := EncodePublicLyricsV3Detail(details[lyricsDocumentTestMusicID])
			if err != nil {
				t.Fatal(err)
			}
			if string(served) != string(result.Document) {
				t.Fatalf("served detail differs from the response\nserved=%s\nresponse=%s", served, result.Document)
			}

			export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(export.Warnings) != 0 {
				t.Fatalf("export warnings=%+v", export.Warnings)
			}
			gameSource := export.Document.Renditions[0].GameSource
			if export.Document.Source.URL != lyricsDocumentGameSourceFullURL || gameSource == nil ||
				*gameSource != (LyricsDocumentSource{URL: lyricsDocumentGameSourceGameURL, Title: lyricsDocumentGameSourceGameName}) {
				t.Fatalf("exported source=%+v gameSource=%+v", export.Document.Source, gameSource)
			}
			again := export.Document
			again.DryRun = true
			republished, err := s.PublishLyricsDocumentServed(context.Background(), again, LyricsDocumentServed{Detail: served}, "document-admin")
			if err != nil {
				t.Fatal(err)
			}
			if republished.Changes.Changed {
				t.Fatalf("publishing the export changes the song: %+v", republished.Changes)
			}
		})
	}
}

func TestPublishLyricsDocumentGameSourceNamingTheDocumentSourceAddsNothing(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentGameSourceRequest("cut")
	request.Source = LyricsDocumentSource{URL: lyricsDocumentGameSourceGameURL}
	_, detail := publishLyricsDocumentForTest(t, s, request, 0)
	for _, attribution := range detail.Renditions[0].Provenance {
		if attribution.RevisionURL != lyricsDocumentGameSourceGameURL {
			t.Fatalf("provenance=%+v", attribution)
		}
	}
	if got := lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM song_lyrics_source_artifacts`); got != 1 {
		t.Fatalf("stored %d source artifacts, want 1", got)
	}
}

func TestPublishLyricsDocumentGameSourceNeedsAFullAndAGameSide(t *testing.T) {
	for _, mode := range []string{"none", "only"} {
		t.Run(mode, func(t *testing.T) {
			s := setupLyricsDocumentStore(t)
			request := lyricsDocumentGameSourceRequest(mode)
			if mode == "only" {
				request.Renditions[0].GameLines, request.Renditions[0].Lines = request.Renditions[0].Lines, nil
			}
			documentErr := lyricsDocumentErrorFor(t, s, request, 0)
			if documentErr.Code != LyricsDocumentErrorInvalid || len(documentErr.Issues) != 1 ||
				documentErr.Issues[0].Field != "gameSource" || documentErr.Issues[0].Rendition != "sekai" {
				t.Fatalf("error=%+v", documentErr)
			}
		})
	}
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentGameSourceRequest("cut")
	request.Renditions[0].GameSource.URL = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2"
	documentErr := lyricsDocumentErrorFor(t, s, request, 0)
	if documentErr.Code != LyricsDocumentErrorSourceRevision || len(documentErr.Details) != 1 ||
		!strings.Contains(documentErr.Details[0], `rendition "sekai" gameSource`) {
		t.Fatalf("error=%+v", documentErr)
	}
}

func TestPublishLyricsDocumentChangesNameAnAddedGameSource(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentGameSourceRequest("cut")
	request.Renditions[0].GameSource = nil
	publishLyricsDocumentForTest(t, s, request, 0)

	request = lyricsDocumentGameSourceRequest("cut")
	request.ExpectedRevision, request.DryRun = lyricsDocumentExpectedRevisionForTest(t, s, request, 0), true
	result, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin")
	if err != nil {
		var documentErr *LyricsDocumentError
		errors.As(err, &documentErr)
		t.Fatalf("dry run: %v %+v", err, documentErr)
	}
	changes := result.Changes
	if !changes.Changed || changes.Source != nil || len(changes.Renditions) != 1 {
		t.Fatalf("changes=%+v", changes)
	}
	change := changes.Renditions[0].GameSource
	if change == nil || change.Before != nil || change.After == nil ||
		*change.After != (LyricsDocumentSource{URL: lyricsDocumentGameSourceGameURL, Title: lyricsDocumentGameSourceGameName}) ||
		len(changes.Renditions[0].Sides) != 0 {
		t.Fatalf("rendition change=%+v", changes.Renditions[0])
	}
}

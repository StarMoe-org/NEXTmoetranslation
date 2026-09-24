package store

import (
	"testing"

	"moesekai/server/internal/model"
)

// The parser escapes the page segment with url.PathEscape, so "(" and ")"
// are served as %28 and %29, matching the embedded seed's canonical URLs.
const lyricsDocumentProjectSekaiRevisionURL = "https://projectsekai.fandom.com/wiki/Synthetic_%28Song%29?oldid=375274"

func TestPublishLyricsDocumentServesProjectSekaiFandomSource(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	request.Source.URL = "https://projectsekai.fandom.com/wiki/Synthetic_(Song)?oldid=375274"
	result, _ := publishLyricsDocumentForTest(t, s, request, 0)

	_, details, _, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	served, ok := details[lyricsDocumentTestMusicID]
	if !ok {
		t.Fatal("projection does not serve the published document")
	}
	servedBody, err := EncodePublicLyricsV3Detail(served)
	if err != nil {
		t.Fatal(err)
	}
	if string(servedBody) != string(result.Document) {
		t.Fatalf("served detail differs from the response\nserved=%s\nresponse=%s", servedBody, result.Document)
	}
	if len(served.Renditions) != 1 || len(served.Renditions[0].Provenance) == 0 {
		t.Fatalf("served renditions=%+v", served.Renditions)
	}
	for _, attribution := range served.Renditions[0].Provenance {
		if attribution.Provider != model.LyricsSourceProviderVocaloidFandom || attribution.RevisionID != 375274 ||
			attribution.RevisionURL != lyricsDocumentProjectSekaiRevisionURL || attribution.Title != "Synthetic (Song)" {
			t.Fatalf("provenance=%+v", attribution)
		}
	}

	var provider, origin, canonicalURL string
	var revisionID int
	if err := s.db.QueryRow(`SELECT a.provider,a.origin,a.revision_id,a.canonical_revision_url
		FROM song_lyrics_source_artifacts AS a
		JOIN song_lyrics_source_documents AS d ON d.document_id=a.document_id
		WHERE d.music_id=?`, lyricsDocumentTestMusicID).Scan(&provider, &origin, &revisionID, &canonicalURL); err != nil {
		t.Fatal(err)
	}
	if provider != string(model.LyricsSourceProviderVocaloidFandom) || origin != model.LyricsSourceOriginProjectSekaiFandom ||
		revisionID != 375274 || canonicalURL != lyricsDocumentProjectSekaiRevisionURL {
		t.Fatalf("stored artifact provider=%q origin=%q revision=%d url=%q", provider, origin, revisionID, canonicalURL)
	}
}

func TestValidPublicV3RevisionURLAcceptsProjectSekaiFandomOnlyForVocaloidFandom(t *testing.T) {
	for _, test := range []struct {
		provider model.LyricsSourceProvider
		url      string
		want     bool
	}{
		{model.LyricsSourceProviderVocaloidFandom, lyricsDocumentProjectSekaiRevisionURL, true},
		{model.LyricsSourceProviderVocaloidFandom, "https://projectsekai.fandom.com/wiki/Kowarechatta%21%21?oldid=375274", true},
		{model.LyricsSourceProviderVocaloidFandom, "https://vocaloid.fandom.com/wiki/Synthetic_%28Song%29?oldid=375274", true},
		{model.LyricsSourceProviderVocaloidFandom, "https://evil.fandom.com/wiki/Synthetic_%28Song%29?oldid=375274", false},
		{model.LyricsSourceProviderVocaloidFandom, "http://projectsekai.fandom.com/wiki/Synthetic_%28Song%29?oldid=375274", false},
		{model.LyricsSourceProviderVocaloidFandom, "https://projectsekai.fandom.com:8443/wiki/Synthetic_%28Song%29?oldid=375274", false},
		{model.LyricsSourceProviderVocaloidFandom, "https://projectsekai.fandom.com/wiki/Synthetic_%28Song%29?oldid=375275", false},
		{model.LyricsSourceProviderVocaloidFandom, "https://projectsekai.fandom.com/index.php?oldid=375274&title=Synthetic", false},
		{model.LyricsSourceProviderSekaipedia, lyricsDocumentProjectSekaiRevisionURL, false},
		{model.LyricsSourceProviderMoegirl, lyricsDocumentProjectSekaiRevisionURL, false},
	} {
		attribution := PublicLyricsV3ComponentAttribution{
			Provider: test.provider, Title: "Synthetic (Song)", RevisionID: 375274, RevisionURL: test.url,
		}
		if got := validPublicV3RevisionURL(attribution); got != test.want {
			t.Fatalf("%s %s valid=%v, want %v", test.provider, test.url, got, test.want)
		}
	}
}

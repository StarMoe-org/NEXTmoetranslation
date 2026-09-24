package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

const lyricsDocumentTestMusicID = 901

func setupLyricsDocumentStore(t *testing.T) *Store {
	t.Helper()
	s, _ := openLyricsSourcePipelineStore(t)
	if err := s.UpsertMusicCatalog([]MusicCatalogRecord{{
		MusicID: lyricsDocumentTestMusicID, JapaneseTitle: "合成試験曲",
		Vocals: []model.CatalogVocalSignal{
			{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 1, CharacterSequence: 1},
			{VocalID: 1, VocalType: "sekai", CharacterType: "game_character", CharacterID: 2, CharacterSequence: 2},
			{VocalID: 2, VocalType: "virtual_singer", CharacterType: "game_character", CharacterID: 21, CharacterSequence: 1},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]PerformerCatalogRecord{
		{PerformerID: 1, JapaneseName: "星乃一歌"}, {PerformerID: 2, JapaneseName: "天馬咲希"},
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func lyricsDocumentTestRequest() LyricsDocumentRequest {
	return LyricsDocumentRequest{
		MusicID:           lyricsDocumentTestMusicID,
		Source:            LyricsDocumentSource{URL: "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242"},
		TranslationCredit: "合成译者",
		Renditions: []LyricsDocumentRendition{{
			Key: "sekai",
			Lines: []LyricsDocumentLine{
				{Japanese: "{試験|しけん}の{歌|うた}をうたう", Chinese: "唱起测试之歌"},
				{Japanese: "らららと{合成|ごうせい}する", Chinese: "啦啦啦地合成", StanzaBreakBefore: true},
				{Japanese: "{点線|てんせん}をなぞる", Chinese: "描过虚线"},
			},
		}},
	}
}

func publishLyricsDocumentForTest(t *testing.T, s *Store, request LyricsDocumentRequest, bundleRevision int) (LyricsDocumentResult, PublicLyricsV3DetailDocument) {
	t.Helper()
	result, err := s.PublishLyricsDocument(context.Background(), request, bundleRevision, "document-admin")
	if err != nil {
		var documentErr *LyricsDocumentError
		if errors.As(err, &documentErr) {
			t.Fatalf("publish lyrics document: %s %v issues=%+v", documentErr.Code, documentErr.Details, documentErr.Issues)
		}
		t.Fatalf("publish lyrics document: %v", err)
	}
	detail, err := DecodePublicLyricsV3Detail(result.Document)
	if err != nil {
		t.Fatalf("decode published detail: %v\n%s", err, result.Document)
	}
	return result, detail
}

// lyricsDocumentRequiredRevisionForTest is the expectedRevision the
// expected_revision_required answer to request without one names.
func lyricsDocumentRequiredRevisionForTest(t *testing.T, s *Store, request LyricsDocumentRequest, bundleRevision int) *int {
	t.Helper()
	request.ExpectedRevision = nil
	documentErr := lyricsDocumentErrorFor(t, s, request, bundleRevision)
	current, ok := documentErr.Current.(map[string]int)
	if documentErr.Code != LyricsDocumentErrorRevisionRequired || !ok || current["revision"] <= 0 {
		t.Fatalf("publish without expectedRevision error=%+v", documentErr)
	}
	revision := current["revision"]
	return &revision
}

// lyricsDocumentExpectedRevisionForTest is the expectedRevision a publish of
// request needs now, as GET reports it: nil for a song with nothing stored or
// served.
func lyricsDocumentExpectedRevisionForTest(t *testing.T, s *Store, request LyricsDocumentRequest, bundleRevision int) *int {
	t.Helper()
	request.ExpectedRevision, request.DryRun = nil, true
	if _, err := s.PublishLyricsDocument(context.Background(), request, bundleRevision, "document-admin"); err == nil {
		return nil
	}
	return lyricsDocumentRequiredRevisionForTest(t, s, request, bundleRevision)
}

func TestPublishLyricsDocumentFullOnlyWithRubyIsServedByTheProjection(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	result, detail := publishLyricsDocumentForTest(t, s, lyricsDocumentTestRequest(), 0)
	if result.DryRun || result.Revision != 2 || result.PublicPath != "/files/translation/lyrics/music_901.json" {
		t.Fatalf("result=%+v", result)
	}
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
	rendition := detail.Renditions[0]
	if rendition.Key != "sekai" || rendition.Kind != model.LyricsSourceRenditionSekai || rendition.Label != "SEKAI Version" ||
		rendition.Game != nil || rendition.Relation.Kind != model.LyricsSourceRenditionRelationNone {
		t.Fatalf("rendition=%+v", rendition)
	}
	first := rendition.Full.Lines[0]
	wantRuby := []PublicLyricsV3RubySpan{{Text: "試験", Reading: "しけん"}, {Text: "の"}, {Text: "歌", Reading: "うた"}, {Text: "をうたう"}}
	if first.Japanese != "試験の歌をうたう" || first.Chinese != "唱起测试之歌" || !reflect.DeepEqual(first.Segments[0].Ruby, wantRuby) ||
		!reflect.DeepEqual(first.Segments[0].PerformerIDs, []string{"歌唱者-01", "歌唱者-02"}) {
		t.Fatalf("first line=%+v", first)
	}
	if !rendition.Full.Lines[1].StanzaBreakBefore || rendition.TranslationCredits == nil || rendition.TranslationCredits.Translation != "合成译者" {
		t.Fatalf("rendition credits/stanza=%+v", rendition)
	}
	body, _ := json.Marshal(rendition.Provenance)
	if !strings.Contains(string(body), `"revisionUrl":"https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242"`) {
		t.Fatalf("provenance=%s", body)
	}
}

func lyricsDocumentErrorFor(t *testing.T, s *Store, request LyricsDocumentRequest, bundleRevision int) *LyricsDocumentError {
	t.Helper()
	result, err := s.PublishLyricsDocument(context.Background(), request, bundleRevision, "document-admin")
	var documentErr *LyricsDocumentError
	if !errors.As(err, &documentErr) {
		t.Fatalf("publish result=%+v err=%v, want a lyrics document error", result, err)
	}
	return documentErr
}

// lyricsDocumentTableCounts counts every row of every table so a test can
// prove that a call wrote nothing.
func lyricsDocumentTableCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	rows.Close()
	counts := make(map[string]int, len(names))
	for _, name := range names {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM "` + name + `"`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[name] = count
	}
	return counts
}

func lyricsDocumentCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestParseLyricsDocumentSourceURL(t *testing.T) {
	for _, test := range []struct {
		name, raw, title string
		provider         model.LyricsSourceProvider
		origin           string
		wantTitle        string
		revisionID       int
		canonicalURL     string
	}{
		{
			name: "vocaloid fandom", raw: "https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=100",
			provider: model.LyricsSourceProviderVocaloidFandom, origin: "https://vocaloid.fandom.com",
			wantTitle: "Synthetic Test Song", revisionID: 100,
			canonicalURL: "https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=100",
		},
		{
			name: "project sekai fandom", raw: "https://projectsekai.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=375274",
			provider: model.LyricsSourceProviderVocaloidFandom, origin: "https://projectsekai.fandom.com",
			wantTitle: "合成試験曲", revisionID: 375274,
			canonicalURL: "https://projectsekai.fandom.com/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=375274",
		},
		{
			name: "sekaipedia with unescaped page and extra query", raw: "https://www.sekaipedia.org/wiki/合成 試験曲?action=view&oldid=5",
			provider: model.LyricsSourceProviderSekaipedia, origin: "https://www.sekaipedia.org",
			wantTitle: "合成 試験曲", revisionID: 5,
			canonicalURL: "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90_%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=5",
		},
		{
			name: "moegirl index.php", raw: "https://moegirl.icu/index.php?title=%E5%90%88%E6%88%90/%E6%AD%8C&oldid=77",
			provider: model.LyricsSourceProviderMoegirl, origin: "https://moegirl.icu",
			wantTitle: "合成/歌", revisionID: 77,
			canonicalURL: "https://moegirl.icu/wiki/%E5%90%88%E6%88%90/%E6%AD%8C?oldid=77",
		},
		{
			name: "title override", raw: "https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=101", title: " 合成の歌 ",
			provider: model.LyricsSourceProviderVocaloidFandom, origin: "https://vocaloid.fandom.com",
			wantTitle: "合成の歌", revisionID: 101,
			canonicalURL: "https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=101",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseLyricsDocumentSourceURL(test.raw, test.title)
			if err != nil {
				t.Fatal(err)
			}
			if got.provider != test.provider || got.origin != test.origin || got.title != test.wantTitle ||
				got.revisionID != test.revisionID || got.canonicalURL != test.canonicalURL {
				t.Fatalf("parsed=%+v", got)
			}
			attribution := PublicLyricsV3ComponentAttribution{
				Provider: got.provider, Title: got.title, RevisionID: got.revisionID, RevisionURL: got.canonicalURL,
			}
			if !validPublicV3RevisionURL(attribution) {
				t.Fatalf("canonical URL %q fails the public v3 revision URL rule", got.canonicalURL)
			}
		})
	}
	for _, raw := range []string{
		"https://vocaloid.fandom.com/wiki/Synthetic_Test_Song",
		"https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=0",
		"https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=01",
		"https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=abc",
		"https://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=1&oldid=2",
		"https://vocaloid.fandom.com:8443/wiki/Synthetic_Test_Song?oldid=1",
		"https://example.com/wiki/Synthetic_Test_Song?oldid=1",
		"https://zh.moegirl.org.cn/wiki/Synthetic_Test_Song?oldid=1",
		"https://vocaloid.fandom.com/Synthetic_Test_Song?oldid=1",
		"https://vocaloid.fandom.com/wiki/?oldid=1",
		"ftp://vocaloid.fandom.com/wiki/Synthetic_Test_Song?oldid=1",
	} {
		if got, err := parseLyricsDocumentSourceURL(raw, ""); !errors.Is(err, errLyricsDocumentSourceRevision) {
			t.Fatalf("%s parsed=%+v err=%v", raw, got, err)
		}
	}
}

func TestParseLyricsDocumentRuby(t *testing.T) {
	for _, test := range []struct {
		markup string
		text   string
		spans  []model.LyricsSourceRubySpan
	}{
		{"ららら", "ららら", []model.LyricsSourceRubySpan{{Text: "ららら"}}},
		{"{合成|ごうせい}", "合成", []model.LyricsSourceRubySpan{{Text: "合成", Reading: "ごうせい"}}},
		{"ねえ{試験|しけん}の{歌|ウタ}ー", "ねえ試験の歌ー", []model.LyricsSourceRubySpan{
			{Text: "ねえ"}, {Text: "試験", Reading: "しけん"}, {Text: "の"}, {Text: "歌", Reading: "ウタ"}, {Text: "ー"},
		}},
		{"{言|い}・{葉|は}", "言・葉", []model.LyricsSourceRubySpan{{Text: "言", Reading: "い"}, {Text: "・"}, {Text: "葉", Reading: "は"}}},
	} {
		text, spans, problems := parseLyricsDocumentRuby(test.markup)
		if text != test.text || !reflect.DeepEqual(spans, test.spans) || len(problems) != 0 {
			t.Fatalf("%q text=%q spans=%+v problems=%v", test.markup, text, spans, problems)
		}
	}
	for _, test := range []struct {
		markup  string
		problem string
	}{
		{"{合成|ごうせい", "unclosed '{' at character 0"},
		{"合成|ごうせい}", "unbalanced '}' at character 7"},
		{"{合成}", "ruby {合成} at character 0 must be written {kanji|reading}"},
		{"{合成|}", "ruby {合成|} at character 0 has an empty base or reading"},
		{"ら{|ごうせい}", "ruby {|ごうせい} at character 1 has an empty base or reading"},
		{"{合成|ごう|せい}", "ruby {合成|ごう|せい} at character 0 has more than one '|'"},
		{"{あい|あい}", `ruby base "あい" in {あい|あい} at character 0 must contain only kanji`},
		{"らら{合成|gousei}", `ruby reading "gousei" in {合成|gousei} at character 2 must be kana`},
		{"{合成|ーごう}", `ruby reading "ーごう" in {合成|ーごう} at character 0 must be kana`},
		// Indices count characters of the ja markup the agent edits.
		{"{試|し}験の歌", "kanji without a {kanji|reading}: 「験」 at character 5, 「歌」 at character 7"},
	} {
		_, _, problems := parseLyricsDocumentRuby(test.markup)
		if len(problems) == 0 || !strings.Contains(strings.Join(problems, "; "), test.problem) {
			t.Fatalf("%q problems=%v, want %q", test.markup, problems, test.problem)
		}
	}
}

func TestPublishLyricsDocumentGameModes(t *testing.T) {
	for _, mode := range []string{"none", "same", "cut", "independent"} {
		t.Run(mode, func(t *testing.T) {
			s := setupLyricsDocumentStore(t)
			request := lyricsDocumentTestRequest()
			rendition := &request.Renditions[0]
			rendition.Game = mode
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
			_, detail := publishLyricsDocumentForTest(t, s, request, 0)
			got := detail.Renditions[0]
			components := map[string]bool{}
			for _, attribution := range got.Provenance {
				components[strings.TrimPrefix(attribution.Component, "renditions/sekai/")] = true
			}
			gameText := func() []string {
				if got.Game == nil {
					return nil
				}
				var texts []string
				for _, line := range got.Game.Lines {
					texts = append(texts, line.Japanese+"|"+line.Chinese)
				}
				return texts
			}()
			switch mode {
			case "none":
				if got.Game != nil || got.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
					!reflect.DeepEqual(got.AvailableVersions, []string{"full"}) || components["game_text"] {
					t.Fatalf("none rendition=%+v", got)
				}
			case "same":
				if got.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
					!reflect.DeepEqual(got.Relation.LineIDs, []string{"full-000001", "full-000002", "full-000003"}) ||
					!reflect.DeepEqual(gameText, []string{"試験の歌をうたう|唱起测试之歌", "らららと合成する|啦啦啦地合成", "点線をなぞる|描过虚线"}) {
					t.Fatalf("same rendition=%+v game=%v", got, gameText)
				}
			case "cut":
				if got.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection ||
					!reflect.DeepEqual(got.Relation.LineIDs, []string{"full-000001", "full-000003"}) ||
					!reflect.DeepEqual(gameText, []string{"試験の歌をうたう|唱起测试之歌", "点線をなぞる|描过虚线"}) ||
					!reflect.DeepEqual(got.AvailableVersions, []string{"full", "game"}) {
					t.Fatalf("cut rendition=%+v game=%v", got, gameText)
				}
			case "independent":
				if got.Relation.Kind != model.LyricsSourceRenditionRelationNone ||
					!reflect.DeepEqual(gameText, []string{"短い試験|短短的测试", "おわり|结束"}) ||
					!components["game_text"] || !components["game_ruby"] || !components["game_performer_segmentation"] ||
					got.Game.Lines[0].Segments[0].Ruby[0].Reading != "みじか" ||
					!reflect.DeepEqual(got.Game.Lines[1].Segments[0].PerformerIDs, []string{"歌唱者-02"}) {
					t.Fatalf("independent rendition=%+v game=%v components=%v", got, gameText, components)
				}
			}
			for _, component := range []string{"full_text", "full_performer_segmentation", "full_ruby", "relation", "version"} {
				if !components[component] {
					t.Fatalf("%s provenance lacks %s: %v", mode, component, components)
				}
			}
		})
	}
}

func TestPublishLyricsDocumentTwoRenditions(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	request.ProofreadingCredit = "合成校对"
	request.Renditions[0].Lines[1].PerformerIDs = []int{1}
	request.Renditions = append(request.Renditions, LyricsDocumentRendition{
		Key: "vocaloid", Game: "cut",
		Lines: []LyricsDocumentLine{
			{Japanese: "{機械|きかい}の{声|こえ}", Chinese: "机械之声", InGame: true},
			{Japanese: "るるる", Chinese: "噜噜噜"},
		},
	})
	_, detail := publishLyricsDocumentForTest(t, s, request, 0)
	if len(detail.Renditions) != 2 {
		t.Fatalf("renditions=%+v", detail.Renditions)
	}
	byKey := map[string]PublicLyricsV3Rendition{}
	for _, rendition := range detail.Renditions {
		byKey[rendition.Key] = rendition
	}
	sekai, vocaloid := byKey["sekai"], byKey["vocaloid"]
	if !reflect.DeepEqual(sekai.Full.Lines[1].Segments[0].PerformerIDs, []string{"歌唱者-01"}) ||
		!reflect.DeepEqual(sekai.Full.Lines[0].Segments[0].PerformerIDs, []string{"歌唱者-01", "歌唱者-02"}) {
		t.Fatalf("sekai performers=%+v", sekai.Full.Lines)
	}
	if vocaloid.Kind != model.LyricsSourceRenditionVocaloid || vocaloid.Label != "VIRTUAL SINGER Version" ||
		len(vocaloid.Performers) != 1 || vocaloid.Performers[0].PerformerID != "歌唱者-21" ||
		vocaloid.Game == nil || len(vocaloid.Game.Lines) != 1 || vocaloid.Game.Lines[0].Japanese != "機械の声" {
		t.Fatalf("vocaloid rendition=%+v", vocaloid)
	}
	for _, rendition := range []PublicLyricsV3Rendition{sekai, vocaloid} {
		if rendition.TranslationCredits == nil || rendition.TranslationCredits.Translation != "合成译者" ||
			rendition.TranslationCredits.Proofreading != "合成校对" {
			t.Fatalf("%s credits=%+v", rendition.Key, rendition.TranslationCredits)
		}
	}
	edited, err := s.GetLyricsRenditionDocument(lyricsDocumentTestMusicID)
	if err != nil || edited.Revision != 2 || len(edited.Renditions) != 2 {
		t.Fatalf("rendition editor document=%+v err=%v", edited, err)
	}
}

func TestPublishLyricsDocumentReportsEveryLineIssue(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	request.Renditions[0].Lines = []LyricsDocumentLine{
		{Japanese: "{試|し}験の歌", Chinese: "测试之歌"},
		{Japanese: "{合成|gousei}", Chinese: "合成", English: "synthesis"},
		{Japanese: "ららら", PerformerIDs: []int{999}, InGame: true},
	}
	before := lyricsDocumentTableCounts(t, s)
	documentErr := lyricsDocumentErrorFor(t, s, request, 0)
	if documentErr.Code != LyricsDocumentErrorInvalid {
		t.Fatalf("error=%+v", documentErr)
	}
	type located struct {
		line  int
		field string
	}
	got := map[located]string{}
	for _, issue := range documentErr.Issues {
		if issue.Rendition != "sekai" || issue.Side != "full" || issue.Line == nil {
			t.Fatalf("issue=%+v", issue)
		}
		got[located{*issue.Line, issue.Field}] += issue.Message
	}
	for _, want := range []struct {
		line    int
		field   string
		message string
	}{
		{0, "ja", "kanji without a {kanji|reading}: 「験」 at character 5, 「歌」 at character 7"},
		{1, "ja", `ruby reading "gousei" in {合成|gousei} at character 0 must be kana`},
		{1, "en", "English lines are not stored"},
		{2, "performerIds", "performer ID 999 is not a known character"},
		{2, "inGame", "inGame is only valid on Full lines when game is cut"},
	} {
		if !strings.Contains(got[located{want.line, want.field}], want.message) {
			t.Fatalf("line %d %s issues=%q, want %q (all=%+v)", want.line, want.field, got[located{want.line, want.field}], want.message, documentErr.Issues)
		}
	}
	if after := lyricsDocumentTableCounts(t, s); !reflect.DeepEqual(after, before) {
		t.Fatal("a rejected document changed the database")
	}
}

func TestPublishLyricsDocumentRejectsUnpinnedSourcesAndUnknownMusic(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	for _, raw := range []string{
		"https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90",
		"https://example.com/wiki/%E5%90%88%E6%88%90?oldid=4",
		"https://evil.fandom.com/wiki/%E5%90%88%E6%88%90?oldid=375274",
	} {
		request := lyricsDocumentTestRequest()
		request.Source.URL = raw
		if documentErr := lyricsDocumentErrorFor(t, s, request, 0); documentErr.Code != LyricsDocumentErrorSourceRevision {
			t.Fatalf("%s error=%+v", raw, documentErr)
		}
	}
	request := lyricsDocumentTestRequest()
	request.MusicID = 902
	if documentErr := lyricsDocumentErrorFor(t, s, request, 0); documentErr.Code != LyricsDocumentErrorNotFound {
		t.Fatalf("unknown music error=%+v", documentErr)
	}
}

func TestPublishLyricsDocumentRevisionsStayAboveBundleAndCheckExpectedRevision(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	stale := 3
	request.ExpectedRevision = &stale
	documentErr := lyricsDocumentErrorFor(t, s, request, 7)
	if documentErr.Code != LyricsDocumentErrorRevisionConflict || !reflect.DeepEqual(documentErr.Current, map[string]int{"revision": 7}) {
		t.Fatalf("stale bundle revision error=%+v", documentErr)
	}
	current := 7
	request.ExpectedRevision = &current
	first, _ := publishLyricsDocumentForTest(t, s, request, 7)
	if first.Revision != 8 {
		t.Fatalf("first revision=%d, want 8", first.Revision)
	}
	request.ExpectedRevision = &current
	documentErr = lyricsDocumentErrorFor(t, s, request, 7)
	if documentErr.Code != LyricsDocumentErrorRevisionConflict || !reflect.DeepEqual(documentErr.Current, map[string]int{"revision": 8}) {
		t.Fatalf("stale database revision error=%+v", documentErr)
	}
	request.ExpectedRevision = &first.Revision
	second, _ := publishLyricsDocumentForTest(t, s, request, 7)
	if second.Revision != 9 {
		t.Fatalf("second revision=%d, want 9", second.Revision)
	}
	// A song with lyrics stored or served needs expectedRevision; 0 names
	// only a song with nothing.
	if required := lyricsDocumentRequiredRevisionForTest(t, s, request, 0); *required != second.Revision {
		t.Fatalf("required revision=%d, want %d", *required, second.Revision)
	}
	zero := 0
	request.ExpectedRevision = &zero
	if documentErr := lyricsDocumentErrorFor(t, s, request, 0); documentErr.Code != LyricsDocumentErrorRevisionConflict {
		t.Fatalf("expectedRevision 0 over a published song error=%+v", documentErr)
	}
	request.ExpectedRevision = &second.Revision
	third, _ := publishLyricsDocumentForTest(t, s, request, 0)
	if third.Revision != 10 {
		t.Fatalf("third revision=%d, want 10", third.Revision)
	}
}

func TestPublishLyricsDocumentRequiresExpectedRevisionOnlyWhenSomethingIsStoredOrServed(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	zero := 0
	request.ExpectedRevision, request.DryRun = &zero, true
	if _, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin"); err != nil {
		t.Fatalf("expectedRevision 0 on an empty song: %v", err)
	}
	request.ExpectedRevision = nil
	if _, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin"); err != nil {
		t.Fatalf("no expectedRevision on an empty song: %v", err)
	}
	// A served detail alone occupies the song.
	_, err := s.PublishLyricsDocumentServed(context.Background(), request,
		LyricsDocumentServed{Detail: []byte(`{"version":1,"musicId":901,"revision":1}`)}, "document-admin")
	var documentErr *LyricsDocumentError
	if !errors.As(err, &documentErr) || documentErr.Code != LyricsDocumentErrorRevisionRequired ||
		!reflect.DeepEqual(documentErr.Current, map[string]int{"revision": 1}) {
		t.Fatalf("served song without expectedRevision err=%v", err)
	}
	legacy := validLyrics()
	legacy.MusicID = lyricsDocumentTestMusicID
	if _, err := s.SaveLyrics(legacy, "legacy-editor"); err != nil {
		t.Fatal(err)
	}
	if required := lyricsDocumentRequiredRevisionForTest(t, s, request, 0); *required != 1 {
		t.Fatalf("legacy draft required revision=%d", *required)
	}
	request.ExpectedRevision = &zero
	if documentErr := lyricsDocumentErrorFor(t, s, request, 0); documentErr.Code != LyricsDocumentErrorRevisionConflict {
		t.Fatalf("expectedRevision 0 over a legacy draft error=%+v", documentErr)
	}
}

func TestPublishLyricsDocumentDryRunWritesNothing(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	seedCollaborationLedger(t, s, lyricsDocumentTestMusicID, 3)
	before := lyricsDocumentTableCounts(t, s)
	request := lyricsDocumentTestRequest()
	request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 4)
	request.DryRun = true
	dry, detail := publishLyricsDocumentForTest(t, s, request, 4)
	if !dry.DryRun || dry.Revision != 5 || detail.Revision != 5 || len(detail.Renditions) != 1 {
		t.Fatalf("dry run=%+v", dry)
	}
	if after := lyricsDocumentTableCounts(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("dry run changed row counts\nbefore=%v\nafter=%v", before, after)
	}
	if epoch := collaborationEpoch(t, s, lyricsDocumentTestMusicID); epoch != 3 {
		t.Fatalf("dry run changed the collaboration epoch to %d", epoch)
	}
	if _, err := s.GetLyricsRenditionDocument(lyricsDocumentTestMusicID); !errors.Is(err, ErrLyricsNotFound) {
		t.Fatalf("dry run left a document: %v", err)
	}
	request.DryRun = false
	stored, _ := publishLyricsDocumentForTest(t, s, request, 4)
	if stored.Revision != dry.Revision {
		t.Fatalf("stored revision=%d, dry run revision=%d", stored.Revision, dry.Revision)
	}
}

// A dry run performs the writes of the publish and rolls them back, so a
// refusal only the database raises fails the dry run as it fails the publish.
func TestPublishLyricsDocumentDryRunFailsWhereThePublishFails(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER lyrics_document_test_refusal BEFORE INSERT ON song_lyrics_source_artifacts
		BEGIN SELECT RAISE(ABORT, 'synthetic storage refusal'); END`); err != nil {
		t.Fatal(err)
	}
	before := lyricsDocumentTableCounts(t, s)
	request := lyricsDocumentTestRequest()
	for _, dryRun := range []bool{true, false} {
		request.DryRun = dryRun
		if _, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin"); err == nil ||
			!strings.Contains(err.Error(), "synthetic storage refusal") {
			t.Fatalf("dryRun=%t err=%v", dryRun, err)
		}
	}
	if after := lyricsDocumentTableCounts(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused publish changed row counts\nbefore=%v\nafter=%v", before, after)
	}
}

func TestPublishLyricsDocumentReplacesLegacyPublicationAndWithdrawal(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	legacy := validLyrics()
	legacy.MusicID = lyricsDocumentTestMusicID
	saved, err := s.SaveLyrics(legacy, "legacy-editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision, "legacy-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO song_lyrics_public_withdrawals(music_id, withdrawn_at, withdrawn_by)
		VALUES (?, 100, 'legacy-admin')`, lyricsDocumentTestMusicID); err != nil {
		t.Fatal(err)
	}
	seedCollaborationLedger(t, s, lyricsDocumentTestMusicID, 7)
	var legacyRevision int
	if err := s.db.QueryRow(`SELECT MAX((SELECT revision FROM song_lyrics WHERE music_id=?),
		(SELECT revision FROM song_lyrics_publications WHERE music_id=?))`,
		lyricsDocumentTestMusicID, lyricsDocumentTestMusicID).Scan(&legacyRevision); err != nil {
		t.Fatal(err)
	}

	request := lyricsDocumentTestRequest()
	if request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 0); *request.ExpectedRevision != legacyRevision {
		t.Fatalf("required revision=%d, legacy revision=%d", *request.ExpectedRevision, legacyRevision)
	}
	result, _ := publishLyricsDocumentForTest(t, s, request, 0)
	if result.Revision != legacyRevision+1 {
		t.Fatalf("revision=%d, legacy revision=%d", result.Revision, legacyRevision)
	}
	for table, query := range map[string]string{
		"song_lyrics":                    `SELECT COUNT(*) FROM song_lyrics WHERE music_id=?`,
		"song_lyric_lines":               `SELECT COUNT(*) FROM song_lyric_lines WHERE music_id=?`,
		"song_lyrics_publications":       `SELECT COUNT(*) FROM song_lyrics_publications WHERE music_id=?`,
		"song_lyrics_public_withdrawals": `SELECT COUNT(*) FROM song_lyrics_public_withdrawals WHERE music_id=?`,
	} {
		if count := lyricsDocumentCount(t, s, query, lyricsDocumentTestMusicID); count != 0 {
			t.Fatalf("%s still has %d rows for the song", table, count)
		}
	}
	if count := lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM audit_log WHERE action='lyrics.document.publish' AND user='document-admin'
		AND detail LIKE 'musicId=901 revision=%'`); count != 1 {
		t.Fatalf("audit rows=%d", count)
	}
	if epoch := collaborationEpoch(t, s, lyricsDocumentTestMusicID); epoch != 8 {
		t.Fatalf("collaboration epoch=%d, want 8", epoch)
	}
	_, details, _, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	if served, ok := details[lyricsDocumentTestMusicID]; !ok || served.Revision != result.Revision {
		t.Fatalf("projection served=%+v ok=%t", served, ok)
	}
}

func TestPublishLyricsDocumentReplacesAnEarlierDocumentAndItsEditorEdits(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	first, _ := publishLyricsDocumentForTest(t, s, lyricsDocumentTestRequest(), 0)
	edited, err := s.GetLyricsRenditionDocument(lyricsDocumentTestMusicID)
	if err != nil {
		t.Fatal(err)
	}
	edited.Renditions[0].Full.Lines[0].Chinese = "编辑器改过的译文"
	saved, changed, err := s.SaveLyricsRenditionMutation(edited, "console-editor")
	if err != nil || !changed || saved.Revision != first.Revision+1 {
		t.Fatalf("console edit saved=%d changed=%t err=%v", saved.Revision, changed, err)
	}
	request := lyricsDocumentTestRequest()
	request.Renditions[0].Lines = request.Renditions[0].Lines[:2]
	request.Renditions[0].Lines[0].Chinese = "整曲重发的译文"
	request.ExpectedRevision = &saved.Revision
	second, detail := publishLyricsDocumentForTest(t, s, request, 0)
	if second.Revision != saved.Revision+1 || len(detail.Renditions[0].Full.Lines) != 2 ||
		detail.Renditions[0].Full.Lines[0].Chinese != "整曲重发的译文" {
		t.Fatalf("second=%+v detail=%+v", second, detail.Renditions[0].Full)
	}
	if count := lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=?`, lyricsDocumentTestMusicID); count != 1 {
		t.Fatalf("source documents=%d", count)
	}
}

func TestPublishLyricsDocumentReplacesEmbeddedSeedSongsAndReplayStaysValid(t *testing.T) {
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lyrics-document-seed.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(database)
	seedEmbeddedLyricsEditorCatalog(t, database, bundle, false)
	seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
	if _, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle); err != nil {
		database.Close()
		t.Fatal(err)
	}
	var sourceItem, legacyItem embeddedlyricsseed.CatalogItem
	for _, item := range bundle.Manifest.Items {
		if item.SeedKind == "source_v3" && sourceItem.MusicID == 0 {
			sourceItem = item
		}
		if item.SeedKind == "legacy" && legacyItem.MusicID == 0 {
			legacyItem = item
		}
	}
	for _, item := range []embeddedlyricsseed.CatalogItem{sourceItem, legacyItem} {
		request := lyricsDocumentTestRequest()
		request.MusicID = item.MusicID
		request.Source.URL = fmt.Sprintf("https://www.sekaipedia.org/wiki/%%E5%%90%%88%%E6%%88%%90?oldid=%d", 9000+item.MusicID)
		request.Renditions[0].PerformerIDs = []int{1}
		request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 3)
		result, err := s.PublishLyricsDocument(context.Background(), request, 3, "document-admin")
		if err != nil {
			database.Close()
			t.Fatalf("publish over seeded %s music %d: %v", item.SeedKind, item.MusicID, err)
		}
		document, err := s.GetLyricsRenditionDocument(item.MusicID)
		if err != nil || document.Revision != result.Revision || document.Renditions[0].Full.Lines[0].Japanese != "試験の歌をうたう" {
			database.Close()
			t.Fatalf("seeded %s music %d after publish=%+v err=%v", item.SeedKind, item.MusicID, document, err)
		}
		var applyStatus string
		if err := database.QueryRow(`SELECT apply_status FROM embedded_lyrics_editor_seed_items WHERE music_id=?`,
			item.MusicID).Scan(&applyStatus); err != nil || applyStatus != "preserved_existing" {
			database.Close()
			t.Fatalf("seeded %s music %d ledger status=%q err=%v", item.SeedKind, item.MusicID, applyStatus, err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := New(reopened).ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle)
	if err != nil || replay.Replayed != embeddedlyricsseed.ExpectedCatalogCount {
		t.Fatalf("replay after publish result=%+v err=%v", replay, err)
	}
}

// A rendition-level problem does not hide its line problems, every issue
// names where it is, and a long list is cut with a count of the rest.
func TestPublishLyricsDocumentReportsEveryIssueInOneBoundedResponse(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	request.Renditions[0].Game = "sometimes"
	request.Renditions[0].Lines[1].Japanese = "らら{合成}"
	request.Renditions[0].Lines[2].Segments = []LyricsDocumentSegment{{Japanese: "{点線|てんせん}を", PerformerIDs: []int{1}}, {Japanese: "なぞれ"}}
	request.Renditions[0].Lines[2].Chinese = "描过\n虚线"
	documentErr := lyricsDocumentErrorFor(t, s, request, 0)
	requireLyricsDocumentIssue(t, documentErr, "sekai//game", `game "sometimes" must be none, same, cut, independent or only`)
	requireLyricsDocumentIssue(t, documentErr, "sekai/full/ja@1", "ruby {合成} at character 2 must be written {kanji|reading}")
	requireLyricsDocumentIssue(t, documentErr, "sekai/full/segments@2", "the segment ja values must concatenate to the line's ja; they first differ at character 12: the line has \"る\", the segments give \"れ\"")
	requireLyricsDocumentIssue(t, documentErr, "sekai/full/zh@2", "contains a line break or NUL at character 2")

	request = lyricsDocumentTestRequest()
	request.Renditions[0].Lines = nil
	for range maxLyricsDocumentIssues + 50 {
		request.Renditions[0].Lines = append(request.Renditions[0].Lines, LyricsDocumentLine{Japanese: "歌", Chinese: "歌"})
	}
	documentErr = lyricsDocumentErrorFor(t, s, request, 0)
	last := documentErr.Issues[len(documentErr.Issues)-1]
	if len(documentErr.Issues) != maxLyricsDocumentIssues+1 || last.Field != "issues" || last.Message != "50 more issues are not listed; fix the listed ones and send the document again" {
		t.Fatalf("issues=%d last=%+v", len(documentErr.Issues), last)
	}
	for _, issue := range documentErr.Issues[:maxLyricsDocumentIssues] {
		if issue.Rendition != "sekai" || issue.Side != "full" || issue.Line == nil || issue.Message != "kanji without a {kanji|reading}: 「歌」 at character 0" {
			t.Fatalf("issue=%+v", issue)
		}
	}
}

// The document credits are optional when every translated rendition names
// its own, and a rendition without its own falls back to them.
func TestPublishLyricsDocumentNeedsDocumentCreditsOnlyForRenditionsWithoutTheirOwn(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentExportTestRequest()
	request.TranslationCredit, request.ProofreadingCredit = "", ""
	request.Renditions[0].TranslationCredits = &LyricsDocumentCredits{Translation: "合成译者"}
	requireLyricsDocumentIssue(t, lyricsDocumentErrorFor(t, s, request, 0), "//translationCredit",
		"translationCredit or proofreadingCredit is required: rendition(s) vocaloid have zh lines and no translationCredits of their own")
	request.Renditions[1].TranslationCredits = &LyricsDocumentCredits{Proofreading: "合成校对"}
	_, detail := publishLyricsDocumentForTest(t, s, request, 0)
	credits := map[string]PublicLyricsV3TranslationCredits{}
	for _, rendition := range detail.Renditions {
		if rendition.TranslationCredits != nil {
			credits[rendition.Key] = *rendition.TranslationCredits
		}
	}
	if !reflect.DeepEqual(credits, map[string]PublicLyricsV3TranslationCredits{
		"sekai": {Translation: "合成译者"}, "vocaloid": {Proofreading: "合成校对"},
	}) {
		t.Fatalf("credits=%+v", credits)
	}
	// Credits still have to name someone.
	request.Renditions[0].TranslationCredits, request.Renditions[1].TranslationCredits = &LyricsDocumentCredits{}, &LyricsDocumentCredits{}
	request.ExpectedRevision = &detail.Revision
	requireLyricsDocumentIssue(t, lyricsDocumentErrorFor(t, s, request, 0), "//translationCredit",
		"at least one rendition needs a translation or proofreading credit")
}

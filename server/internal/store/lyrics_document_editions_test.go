package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/db"
)

var lyricsDocumentEditionTables = []string{
	"song_lyrics_translation_editions", "song_lyrics_translation_edition_state",
	"song_lyrics_translation_edition_localizations", "song_lyrics_translation_edition_lines",
}

// lyricsDocumentEditionRowsOutside counts the translation edition rows of
// every source document other than documentID.
func lyricsDocumentEditionRowsOutside(t *testing.T, s *Store, documentID int64) int {
	t.Helper()
	total := 0
	for _, table := range lyricsDocumentEditionTables {
		total += lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM `+table+` WHERE document_id<>?`, documentID)
	}
	return total
}

func lyricsDocumentSourceDocumentID(t *testing.T, s *Store, musicID int) int64 {
	t.Helper()
	var documentID int64
	if err := s.db.QueryRow(`SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, musicID).Scan(&documentID); err != nil {
		t.Fatal(err)
	}
	return documentID
}

// lyricsDocumentEditionsTestRequest is lyricsDocumentExportTestRequest with a
// second edition alt, written as an export writes it: the default first,
// zhEditions and editionCredits only where non-empty.
func lyricsDocumentEditionsTestRequest() LyricsDocumentRequest {
	request := lyricsDocumentExportTestRequest()
	request.TranslationEditions = []LyricsTranslationEditionSummary{{Key: "main", Label: "合成主译本"}, {Key: "alt", Label: "合成别译本"}}
	sekai, vocaloid := &request.Renditions[0], &request.Renditions[1]
	sekai.EditionCredits = map[string]LyricsDocumentCredits{"alt": {Translation: "别译者", Proofreading: "别校对"}}
	sekai.Lines[0].ChineseEditions = map[string]string{"alt": "别译测试之歌"}
	sekai.Lines[1].ChineseEditions = map[string]string{"alt": "别译啦啦啦"}
	vocaloid.GameLines[0].ChineseEditions = map[string]string{"alt": "别译游戏版未来之歌"}
	return request
}

// publishLyricsDocumentEditionsForTest publishes request, whose response
// document is the v4 detail of a song with several editions.
func publishLyricsDocumentEditionsForTest(t *testing.T, s *Store, request LyricsDocumentRequest, served []byte) LyricsDocumentResult {
	t.Helper()
	result, err := s.PublishLyricsDocumentServed(context.Background(), request, LyricsDocumentServed{Detail: served}, "document-admin")
	if err != nil {
		var documentErr *LyricsDocumentError
		if errors.As(err, &documentErr) {
			t.Fatalf("publish lyrics document: %s %v issues=%+v", documentErr.Code, documentErr.Details, documentErr.Issues)
		}
		t.Fatalf("publish lyrics document: %v", err)
	}
	return result
}

// servedLyricsV4ForTest is the v4 detail the projection serves for musicID.
func servedLyricsV4ForTest(t *testing.T, s *Store, musicID int) (PublicLyricsV4DetailDocument, []byte) {
	t.Helper()
	_, _, details, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := details[musicID]
	if !ok {
		t.Fatalf("projection serves no v4 detail for music %d", musicID)
	}
	body, err := EncodePublicLyricsV4Detail(detail)
	if err != nil {
		t.Fatal(err)
	}
	return detail, body
}

// lyricsDocumentV4Editions are the translation editions of a v4 detail with
// its default key.
func lyricsDocumentV4Editions(detail PublicLyricsV4DetailDocument) (string, []PublicLyricsV4TranslationEdition) {
	return detail.DefaultTranslationEditionKey, detail.TranslationEditions
}

// A song whose document has a translation edition state publishes, and its
// dry run agrees: the previous editions go with the previous document.
func TestPublishLyricsDocumentReplacesASongWithTranslationEditionState(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	first, _ := publishLyricsDocumentForTest(t, s, lyricsDocumentTestRequest(), 0)
	if _, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: lyricsDocumentTestMusicID, Revision: first.Revision, Operation: "create", EditionKey: "alt", Label: "备选译本",
	}, "console-editor"); err != nil {
		t.Fatal(err)
	}
	request := lyricsDocumentTestRequest()
	request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 0)
	request.DryRun = true
	dry, _ := publishLyricsDocumentForTest(t, s, request, 0)
	request.DryRun = false
	result, _ := publishLyricsDocumentForTest(t, s, request, 0)
	if dry.Revision != result.Revision {
		t.Fatalf("dry run revision=%d publish revision=%d", dry.Revision, result.Revision)
	}
	if left := lyricsDocumentEditionRowsOutside(t, s, -1); left != 0 {
		t.Fatalf("%d translation edition rows survive a publish that carries no editions", left)
	}
	requireProjectionServes(t, s, lyricsDocumentTestMusicID, result)
	if _, err := s.GetLyricsRenditionDocumentEdition(lyricsDocumentTestMusicID, "alt", true); err == nil {
		t.Fatal("the dropped edition still opens in the editor")
	}
}

func TestLyricsDocumentExportOfAMultiEditionSongRoundTripsToTheSameV4Detail(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentEditionsTestRequest()
	result := publishLyricsDocumentEditionsForTest(t, s, request, nil)
	before, served := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	if string(served) != string(result.Document) || before.Revision != result.Revision {
		t.Fatalf("the publish response is not the served v4 detail\nserved=%s\nresponse=%s", served, result.Document)
	}
	if defaultKey, editions := lyricsDocumentV4Editions(before); defaultKey != "main" || len(editions) != 2 ||
		editions[0].Key != "alt" || editions[0].Renditions[0].TranslationCredits == nil ||
		editions[0].Renditions[0].TranslationCredits.Translation != "别译者" ||
		!reflect.DeepEqual(editions[0].Renditions[0].Full.Translations, []string{"别译测试之歌", "别译啦啦啦", ""}) ||
		!reflect.DeepEqual(editions[0].Renditions[1].Game.Translations, []string{"别译游戏版未来之歌"}) ||
		editions[0].Renditions[1].TranslationCredits != nil {
		t.Fatalf("served editions=%+v", editions)
	}

	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = &result.Revision
	if export.ServedVersion != 4 || len(export.Warnings) != 0 || !reflect.DeepEqual(export.Document, request) {
		t.Fatalf("export=%+v\nwant=%+v", export.Document, request)
	}
	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"translationEditions":[{"key":"main","label":"合成主译本"},{"key":"alt","label":"合成别译本"}]`,
		`"zhEditions":{"alt":"别译测试之歌"}`, `"editionCredits":{"alt":{"translation":"别译者","proofreading":"别校对"}}`,
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("export JSON lacks %s:\n%s", fragment, encoded)
		}
	}
	fromDatabase, err := s.ExportLyricsDocumentFromDatabase(context.Background(), lyricsDocumentTestMusicID, LyricsDocumentServed{Detail: served})
	if err != nil || len(fromDatabase.Warnings) != 0 || !reflect.DeepEqual(fromDatabase.Document, request) {
		t.Fatalf("database export=%+v err=%v", fromDatabase, err)
	}
	if changes := lyricsDocumentDryRunChanges(t, s, export.Document, served); !reflect.DeepEqual(changes, LyricsDocumentChanges{Against: LyricsDocumentChangesAgainstServed}) {
		t.Fatalf("unchanged export changes=%+v", changes)
	}

	again := publishLyricsDocumentEditionsForTest(t, s, export.Document, served)
	after, _ := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	if again.Revision != result.Revision+1 || after.Revision != again.Revision {
		t.Fatalf("republish revision=%d served=%d", again.Revision, after.Revision)
	}
	after.Revision, after.UpdatedAt = before.Revision, before.UpdatedAt
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("republished v4 detail differs\nbefore=%+v\nafter=%+v", before, after)
	}
	if left := lyricsDocumentEditionRowsOutside(t, s, lyricsDocumentSourceDocumentID(t, s, lyricsDocumentTestMusicID)); left != 0 {
		t.Fatalf("%d edition rows of the replaced document survive", left)
	}
}

func TestLyricsDocumentRubyFixKeepsEveryTranslationEdition(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	publishLyricsDocumentEditionsForTest(t, s, lyricsDocumentEditionsTestRequest(), nil)
	before, served := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := export.Document
	request.Renditions[0].Lines[0].Japanese = "{試|し}{験|けん}の{歌|うた}"
	result := publishLyricsDocumentEditionsForTest(t, s, request, served)
	after, _ := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	if !reflect.DeepEqual(after.TranslationEditions, before.TranslationEditions) || after.DefaultTranslationEditionKey != "main" {
		t.Fatalf("editions changed by a ruby fix\nbefore=%+v\nafter=%+v", before.TranslationEditions, after.TranslationEditions)
	}
	changes := result.Changes
	if len(changes.Renditions) != 1 || changes.TranslationEditions != nil || len(changes.Renditions[0].Sides) != 1 ||
		!reflect.DeepEqual(changes.Renditions[0].Sides[0].Changed[0].Fields, []string{"ruby"}) || changes.Renditions[0].EditionCredits != nil {
		t.Fatalf("ruby fix changes=%+v", changes)
	}
}

func TestLyricsDocumentLineSplitRealignsEveryTranslationEdition(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	publishLyricsDocumentEditionsForTest(t, s, lyricsDocumentEditionsTestRequest(), nil)
	_, served := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := export.Document
	sekai := &request.Renditions[0]
	split := []LyricsDocumentLine{
		{Japanese: "らららと", Chinese: "啦啦啦", ChineseEditions: map[string]string{"alt": "别译啦"}, StanzaBreakBefore: true, PerformerIDs: []int{1}},
		{Japanese: "{合成|ごうせい}", Chinese: "地合成", ChineseEditions: map[string]string{"alt": "别译合成"}, PerformerIDs: []int{1}},
	}
	sekai.Lines = append(append(append([]LyricsDocumentLine(nil), sekai.Lines[:1]...), split...), sekai.Lines[2:]...)
	publishLyricsDocumentEditionsForTest(t, s, request, served)
	after, _ := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	want := map[string][]string{
		"main": {"测试之歌", "啦啦啦", "地合成", "描过虚线"},
		"alt":  {"别译测试之歌", "别译啦", "别译合成", ""},
	}
	for _, edition := range after.TranslationEditions {
		if got := edition.Renditions[0].Full.Translations; !reflect.DeepEqual(got, want[edition.Key]) {
			t.Fatalf("edition %s sekai lines=%q want %q", edition.Key, got, want[edition.Key])
		}
	}
}

func TestLyricsDocumentAddsRenamesAndSwitchesTheDefaultTranslationEdition(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	plain := lyricsDocumentExportTestRequest()
	publishLyricsDocumentForTest(t, s, plain, 0)

	// Adding: the implicit main edition gains alt.
	request := lyricsDocumentEditionsTestRequest()
	request.TranslationEditions[0].Label = MainLyricsTranslationEditionLabel
	request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 0)
	added := publishLyricsDocumentEditionsForTest(t, s, request, nil)
	if change := added.Changes.TranslationEditions; change == nil ||
		!reflect.DeepEqual(change.Before, []LyricsTranslationEditionSummary{{Key: "main", Label: MainLyricsTranslationEditionLabel}}) ||
		!reflect.DeepEqual(change.After, request.TranslationEditions) {
		t.Fatalf("adding an edition changes=%+v", added.Changes)
	}
	if change := added.Changes.Renditions[0].EditionCredits["alt"]; change.After.Translation != "别译者" || change.Before != (LyricsDocumentCredits{}) {
		t.Fatalf("added edition credits change=%+v", added.Changes.Renditions[0])
	}

	// Renaming both editions and editing one alt line.
	request.TranslationEditions = []LyricsTranslationEditionSummary{{Key: "main", Label: "合成主译本"}, {Key: "alt", Label: " 合成别译本改 "}}
	request.Renditions[0].Lines[0].ChineseEditions = map[string]string{"alt": "改过的别译"}
	request.ExpectedRevision = &added.Revision
	renamed := publishLyricsDocumentEditionsForTest(t, s, request, nil)
	if change := renamed.Changes.TranslationEditions; change == nil ||
		!reflect.DeepEqual(change.After, []LyricsTranslationEditionSummary{{Key: "main", Label: "合成主译本"}, {Key: "alt", Label: "合成别译本改"}}) {
		t.Fatalf("renaming changes=%+v", renamed.Changes)
	}
	full := renamed.Changes.Renditions[0].Sides[0]
	if full.ChangedCount != 1 || !reflect.DeepEqual(full.Changed[0].Fields, []string{"zhEditions"}) ||
		!reflect.DeepEqual(full.Changed[0].ChineseEditions, map[string]LyricsDocumentTextChange{"alt": {Before: "别译测试之歌", After: "改过的别译"}}) {
		t.Fatalf("zhEditions change=%+v", full)
	}
	v4, _ := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	if v4.TranslationEditions[0].Label != "合成别译本改" || v4.TranslationEditions[1].Label != "合成主译本" {
		t.Fatalf("renamed editions=%+v", v4.TranslationEditions)
	}

	// Switching the default: alt comes first, its text in zh and main's in zhEditions.
	switched := request
	switched.TranslationEditions = []LyricsTranslationEditionSummary{{Key: "alt", Label: "合成别译本改"}, {Key: "main", Label: "合成主译本"}}
	switched.Renditions = []LyricsDocumentRendition{
		{
			Key: "sekai", Kind: "sekai", Label: "SEKAI Version", Game: "cut", PerformerIDs: []int{1, 2},
			TranslationCredits: &LyricsDocumentCredits{Translation: "别译者", Proofreading: "别校对"},
			EditionCredits:     map[string]LyricsDocumentCredits{"main": {Translation: "合成译者", Proofreading: "合成校对"}},
			Lines: []LyricsDocumentLine{
				{Japanese: "{試験|しけん}の{歌|うた}", Chinese: "改过的别译", ChineseEditions: map[string]string{"main": "测试之歌"}, InGame: true},
				{Japanese: "らららと{合成|ごうせい}", Chinese: "别译啦啦啦", ChineseEditions: map[string]string{"main": "啦啦啦地合成"}, StanzaBreakBefore: true, PerformerIDs: []int{1}},
				{Japanese: "{点線|てんせん}をなぞる", ChineseEditions: map[string]string{"main": "描过虚线"}, InGame: true, PerformerIDs: []int{}},
			},
		},
		{
			Key: "vocaloid", Kind: "vocaloid", Label: "VIRTUAL SINGER Version", Game: "independent", PerformerIDs: []int{21},
			TranslationCredits: &LyricsDocumentCredits{},
			EditionCredits:     map[string]LyricsDocumentCredits{"main": {Translation: "合成译者", Proofreading: "合成校对"}},
			Lines: []LyricsDocumentLine{
				{Japanese: "ミクのうた", ChineseEditions: map[string]string{"main": "未来之歌"}},
				{Japanese: "ルルル", ChineseEditions: map[string]string{"main": "噜噜噜"}},
			},
			GameLines: []LyricsDocumentLine{{Japanese: "ミクのうた", Chinese: "别译游戏版未来之歌", ChineseEditions: map[string]string{"main": "未来之歌"}, StanzaBreakBefore: true}},
		},
	}
	switched.TranslationCredit, switched.ProofreadingCredit = "", ""
	switched.ExpectedRevision = &renamed.Revision
	result := publishLyricsDocumentEditionsForTest(t, s, switched, nil)
	if change := result.Changes.TranslationEditions; change == nil || change.Before[0].Key != "main" || change.After[0].Key != "alt" {
		t.Fatalf("default switch changes=%+v", result.Changes)
	}
	switchedV4, _ := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	if !reflect.DeepEqual(switchedV4.TranslationEditions, v4.TranslationEditions) || switchedV4.DefaultTranslationEditionKey != "alt" {
		t.Fatalf("switching the default changed the editions\nbefore=%+v\nafter=%+v", v4.TranslationEditions, switchedV4.TranslationEditions)
	}
	_, v3Details, _, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	if got := v3Details[lyricsDocumentTestMusicID].Renditions[0].Full.Lines[0].Chinese; got != "改过的别译" {
		t.Fatalf("v3 detail zh=%q, want the new default edition", got)
	}
}

// A song whose only edition is a renamed main is served as v3, which has no
// edition label; the export and the change baseline take it from the database.
func TestLyricsDocumentExportKeepsTheLabelOfARenamedOnlyEdition(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentExportTestRequest()
	request.TranslationEditions = []LyricsTranslationEditionSummary{{Key: "main", Label: "合成主译本"}}
	result := publishLyricsDocumentEditionsForTest(t, s, request, nil)
	servedV3 := func() []byte {
		t.Helper()
		_, details, v4Details, err := s.PublishedLyricsLocalizationProjection()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := v4Details[lyricsDocumentTestMusicID]; ok {
			t.Fatal("a song with one edition is served as v4")
		}
		body, err := EncodePublicLyricsV3Detail(details[lyricsDocumentTestMusicID])
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	served := servedV3()
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(export.Document.TranslationEditions, request.TranslationEditions) {
		t.Fatalf("served export editions=%+v", export.Document.TranslationEditions)
	}
	if changes := lyricsDocumentDryRunChanges(t, s, export.Document, served); !reflect.DeepEqual(changes, LyricsDocumentChanges{Against: LyricsDocumentChangesAgainstServed}) {
		t.Fatalf("unchanged export changes=%+v", changes)
	}
	fromDatabase, err := s.ExportLyricsDocumentFromDatabase(context.Background(), lyricsDocumentTestMusicID, LyricsDocumentServed{Detail: served})
	if err != nil {
		t.Fatal(err)
	}
	if changes := lyricsDocumentDryRunChanges(t, s, fromDatabase.Document, served); !reflect.DeepEqual(changes, LyricsDocumentChanges{Against: LyricsDocumentChangesAgainstServed}) {
		t.Fatalf("unchanged database export changes=%+v", changes)
	}

	again := publishLyricsDocumentEditionsForTest(t, s, export.Document, served)
	if again.Revision != result.Revision+1 {
		t.Fatalf("republish revision=%d", again.Revision)
	}
	stored, err := s.ExportLyricsDocumentFromDatabase(context.Background(), lyricsDocumentTestMusicID, LyricsDocumentServed{})
	if err != nil || !reflect.DeepEqual(stored.Document.TranslationEditions, request.TranslationEditions) {
		t.Fatalf("stored editions after republishing the export=%+v err=%v", stored.Document.TranslationEditions, err)
	}

	// Dropping the list resets the label, and changes say so.
	plain := export.Document
	plain.TranslationEditions = nil
	plain.ExpectedRevision = &again.Revision
	plain.DryRun = true
	changes := lyricsDocumentDryRunChanges(t, s, plain, servedV3())
	if change := changes.TranslationEditions; change == nil ||
		!reflect.DeepEqual(change.Before, request.TranslationEditions) ||
		!reflect.DeepEqual(change.After, []LyricsTranslationEditionSummary{{Key: "main", Label: MainLyricsTranslationEditionLabel}}) {
		t.Fatalf("dropping the renamed edition changes=%+v", changes)
	}
}

func TestPublishLyricsDocumentLocatesEveryTranslationEditionIssue(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	line := func(index int) *int { return &index }
	for _, tc := range []struct {
		name   string
		mutate func(*LyricsDocumentRequest)
		want   LyricsDocumentIssue
		text   string
	}{
		{"undeclared key", func(r *LyricsDocumentRequest) {
			r.Renditions[0].Lines[2].ChineseEditions = map[string]string{"ghost": "幽灵"}
		},
			LyricsDocumentIssue{Rendition: "sekai", Side: "full", Line: line(2), Field: "zhEditions", Edition: "ghost"}, "not declared"},
		{"default key in zhEditions", func(r *LyricsDocumentRequest) {
			r.Renditions[1].GameLines[0].ChineseEditions = map[string]string{"main": "默认"}
		},
			LyricsDocumentIssue{Rendition: "vocaloid", Side: "game", Line: line(0), Field: "zhEditions", Edition: "main"}, "default edition"},
		{"missing main", func(r *LyricsDocumentRequest) { r.TranslationEditions[0].Key = "first" },
			LyricsDocumentIssue{Field: "translationEditions", Edition: "main"}, "must include the edition main"},
		{"duplicate key", func(r *LyricsDocumentRequest) {
			r.TranslationEditions = append(r.TranslationEditions, LyricsTranslationEditionSummary{Key: "alt", Label: "重复"})
		}, LyricsDocumentIssue{Field: "translationEditions", Edition: "alt"}, "repeated"},
		{"17 editions", func(r *LyricsDocumentRequest) {
			for index := range 15 {
				r.TranslationEditions = append(r.TranslationEditions, LyricsTranslationEditionSummary{Key: "extra" + string(rune('a'+index)), Label: "多余"})
			}
		}, LyricsDocumentIssue{Field: "translationEditions"}, "1 to 16 entries; it has 17"},
		{"bad label", func(r *LyricsDocumentRequest) { r.TranslationEditions[1].Label = "   " },
			LyricsDocumentIssue{Field: "translationEditions", Edition: "alt"}, "label must be 1 to 256 bytes"},
		{"bad key", func(r *LyricsDocumentRequest) { r.TranslationEditions[1].Key = "Alt!" },
			LyricsDocumentIssue{Field: "translationEditions", Edition: "Alt!"}, "must match"},
		{"zhEditions without translationEditions", func(r *LyricsDocumentRequest) { r.TranslationEditions = nil },
			LyricsDocumentIssue{Rendition: "sekai", Side: "full", Line: line(0), Field: "zhEditions", Edition: "alt"}, "needs translationEditions"},
		{"newline in a zhEditions value", func(r *LyricsDocumentRequest) { r.Renditions[0].Lines[1].ChineseEditions["alt"] = "一行\n两行" },
			LyricsDocumentIssue{Rendition: "sekai", Side: "full", Line: line(1), Field: "zhEditions", Edition: "alt"}, "line break"},
		{"undeclared editionCredits key", func(r *LyricsDocumentRequest) {
			r.Renditions[1].EditionCredits = map[string]LyricsDocumentCredits{"ghost": {Translation: "幽灵"}}
		}, LyricsDocumentIssue{Rendition: "vocaloid", Field: "editionCredits", Edition: "ghost"}, "not declared"},
		{"multi-line editionCredits", func(r *LyricsDocumentRequest) {
			r.Renditions[0].EditionCredits["alt"] = LyricsDocumentCredits{Translation: "别\n译者"}
		},
			LyricsDocumentIssue{Rendition: "sekai", Field: "editionCredits.translation", Edition: "alt"}, "line break"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := lyricsDocumentEditionsTestRequest()
			tc.mutate(&request)
			documentErr := lyricsDocumentErrorFor(t, s, request, 0)
			if documentErr.Code != LyricsDocumentErrorInvalid {
				t.Fatalf("error=%+v", documentErr)
			}
			for _, issue := range documentErr.Issues {
				message := issue.Message
				issue.Message = ""
				if reflect.DeepEqual(issue, tc.want) && strings.Contains(message, tc.text) {
					return
				}
			}
			t.Fatalf("issues=%+v, want %+v with %q", documentErr.Issues, tc.want, tc.text)
		})
	}
	// Blank editions carry no content requirement, and the implicit list
	// publishes without edition rows.
	blank := lyricsDocumentExportTestRequest()
	blank.TranslationEditions = []LyricsTranslationEditionSummary{{Key: "main", Label: "合成主译本"}, {Key: "blank", Label: "空白译本"}}
	blank.DryRun = true
	if _, err := s.PublishLyricsDocument(context.Background(), blank, 0, "document-admin"); err != nil {
		t.Fatalf("blank edition dry run: %v", err)
	}
	implicit := lyricsDocumentExportTestRequest()
	implicit.TranslationEditions = []LyricsTranslationEditionSummary{{Key: "main", Label: " 默认译本 "}}
	publishLyricsDocumentForTest(t, s, implicit, 0)
	if rows := lyricsDocumentEditionRowsOutside(t, s, -1); rows != 0 {
		t.Fatalf("the implicit edition list stored %d edition rows", rows)
	}
}

func TestConsoleEditionEditingWorksAfterALyricsDocumentPublish(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	result := publishLyricsDocumentEditionsForTest(t, s, lyricsDocumentEditionsTestRequest(), nil)
	renamed, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: lyricsDocumentTestMusicID, Revision: result.Revision, Operation: "rename", EditionKey: "alt", Label: "控制台改名",
	}, "console-editor")
	if err != nil {
		t.Fatalf("rename after a document publish: %v", err)
	}
	switched, err := s.MutateLyricsTranslationEdition(LyricsTranslationEditionMutation{
		MusicID: lyricsDocumentTestMusicID, Revision: renamed.Revision, Operation: "set-default", EditionKey: "alt",
	}, "console-editor")
	if err != nil || switched.DefaultTranslationEditionKey != "alt" {
		t.Fatalf("set-default after a document publish: %+v %v", switched.DefaultTranslationEditionKey, err)
	}
	document, err := s.GetLyricsRenditionDocumentEdition(lyricsDocumentTestMusicID, "main", true)
	if err != nil || document.TranslationEditionKey != "main" || document.Renditions[0].Full.Lines[1].Chinese != "啦啦啦地合成" {
		t.Fatalf("main edition document=%+v err=%v", document, err)
	}
	// Line 1 is outside the cut Game, whose lines repeat their Full lines.
	document.Renditions[0].Full.Lines[1].Chinese = "控制台改过的译文"
	saved, changed, err := s.SaveLyricsRenditionMutation(document, "console-editor")
	if err != nil || !changed || saved.Revision != switched.Revision+1 {
		t.Fatalf("rendition save revision=%d changed=%t err=%v", saved.Revision, changed, err)
	}
	v4, _ := servedLyricsV4ForTest(t, s, lyricsDocumentTestMusicID)
	if v4.DefaultTranslationEditionKey != "alt" || v4.TranslationEditions[0].Label != "控制台改名" ||
		v4.TranslationEditions[1].Renditions[0].Full.Translations[1] != "控制台改过的译文" {
		t.Fatalf("served after console edits=%+v", v4.TranslationEditions)
	}
}

// Migration v32 gives song 682 editions main and aishitenryu; a whole-song
// publish of its export keeps both.
func TestPublishLyricsDocumentKeepsTheTranslationEditionsOfSong682(t *testing.T) {
	s := setupLyricsStore(t)
	if err := s.UpsertMusicCatalog([]MusicCatalogRecord{{MusicID: 682, JapaneseTitle: "あなたしか見えないの"}}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{db.MigrationV32Song682TranslationEditionsSQL, db.MigrationV33Song682TranslationQEDCorrectionSQL,
		db.MigrationV34Song682TranslationMirrorSyncSQL} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before, served := servedLyricsV4ForTest(t, s, 682)
	export, err := s.ExportLyricsDocument(context.Background(), 682, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []LyricsTranslationEditionSummary{{Key: "main", Label: "雪莹ちゃん"}, {Key: "aishitenryu", Label: "爱死天流"}}; !reflect.DeepEqual(export.Document.TranslationEditions, want) {
		t.Fatalf("exported editions=%+v", export.Document.TranslationEditions)
	}
	request := export.Document
	request.DryRun = true
	if _, err := s.PublishLyricsDocumentServed(context.Background(), request, LyricsDocumentServed{Detail: served}, "document-admin"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	request.DryRun = false
	result := publishLyricsDocumentEditionsForTest(t, s, request, served)
	if result.Changes.Changed {
		t.Fatalf("an unchanged export of song 682 reports changes=%+v", result.Changes)
	}
	after, _ := servedLyricsV4ForTest(t, s, 682)
	if !reflect.DeepEqual(after.TranslationEditions, before.TranslationEditions) || after.DefaultTranslationEditionKey != "main" {
		t.Fatalf("song 682 editions changed\nbefore=%+v\nafter=%+v", before.TranslationEditions, after.TranslationEditions)
	}
	if left := lyricsDocumentEditionRowsOutside(t, s, lyricsDocumentSourceDocumentID(t, s, 682)); left != 0 {
		t.Fatalf("%d edition rows of the migration document survive", left)
	}
}

// A recovery ledger song with a console edition is taken over from its served
// v4 detail, editions included.
func TestTakeOverLyricsDocumentKeepsTheEditionsOfARecoveryLedgerSong(t *testing.T) {
	fixture := setupRecoveryRenditionV3EditorFixture(t)
	s := fixture.store
	seedRecoveryLedgerTranslations(t, fixture, true)
	ledger := recoveryLedgerRowDigests(t, s)
	before, served := servedLyricsV4ForTest(t, s, 10)
	export, err := s.ExportLyricsDocument(context.Background(), 10, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(export.Document.TranslationEditions) != 2 || export.Document.TranslationEditions[1].Key != "alt" {
		t.Fatalf("exported editions=%+v", export.Document.TranslationEditions)
	}
	takeover, err := s.TakeOverLyricsDocument(context.Background(), 10, export.Document.ExpectedRevision,
		LyricsDocumentServed{Detail: served}, "document-admin")
	if err != nil {
		t.Fatalf("take over a ledger song with an edition: %v", err)
	}
	if rows := recoveryTakeoverRowsForTest(t, s); len(rows) != 1 || !rows[10].superseded {
		t.Fatalf("takeovers=%+v", rows)
	}
	requireRecoveryLedgerUnchanged(t, s, ledger)
	after, _ := servedLyricsV4ForTest(t, s, 10)
	if after.Revision != takeover.Revision || !reflect.DeepEqual(after.TranslationEditions, before.TranslationEditions) {
		t.Fatalf("taken-over editions differ\nbefore=%+v\nafter=%+v", before.TranslationEditions, after.TranslationEditions)
	}
	if left := lyricsDocumentEditionRowsOutside(t, s, lyricsDocumentSourceDocumentID(t, s, 10)); left != 0 {
		t.Fatalf("%d edition rows of the ledger document survive", left)
	}
}

package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

// exportPublishedLyricsDocument publishes request, exports the served detail
// and compiles the export again, returning the served detail, the export and
// the detail a PUT of the export would publish.
func exportPublishedLyricsDocument(t *testing.T, s *Store, request LyricsDocumentRequest) (PublicLyricsV3DetailDocument, LyricsDocumentExport, PublicLyricsV3DetailDocument) {
	t.Helper()
	if request.ExpectedRevision == nil {
		request.ExpectedRevision = lyricsDocumentExpectedRevisionForTest(t, s, request, 0)
	}
	result, detail := publishLyricsDocumentForTest(t, s, request, 0)
	export, err := s.ExportLyricsDocument(context.Background(), request.MusicID, result.Document, 0)
	if err != nil {
		t.Fatal(err)
	}
	produced, err := compileLyricsDocumentExport(t, s, export, 0)
	if err != nil {
		t.Fatal(err)
	}
	produced.Revision, produced.UpdatedAt = detail.Revision, detail.UpdatedAt
	if !reflect.DeepEqual(produced, detail) {
		t.Fatalf("a PUT of the export changes the served detail:\n%+v\n%+v", produced, detail)
	}
	return detail, export, produced
}

// lyricsDocumentIssueMessages indexes issues by rendition/side/line/field.
func lyricsDocumentIssueMessages(documentErr *LyricsDocumentError) map[string]string {
	result := map[string]string{}
	for _, issue := range documentErr.Issues {
		key := issue.Rendition + "/" + issue.Side + "/" + issue.Field
		if issue.Line != nil {
			key += fmt.Sprintf("@%d", *issue.Line)
		}
		result[key] += issue.Message + "; "
	}
	return result
}

// lyricsDocumentFidelityRequest is lyricsDocumentTestRequest with every field
// an export writes spelled out, so an export can equal it.
func lyricsDocumentFidelityRequest() LyricsDocumentRequest {
	request := lyricsDocumentTestRequest()
	request.Source.Title = "合成試験曲"
	rendition := &request.Renditions[0]
	rendition.Kind, rendition.Label, rendition.Game, rendition.PerformerIDs = "sekai", "SEKAI Version", "none", []int{1, 2}
	return request
}

func requireLyricsDocumentIssue(t *testing.T, documentErr *LyricsDocumentError, key, message string) {
	t.Helper()
	if documentErr.Code != LyricsDocumentErrorInvalid || !strings.Contains(lyricsDocumentIssueMessages(documentErr)[key], message) {
		t.Fatalf("issues=%v, want %s: %q", lyricsDocumentIssueMessages(documentErr), key, message)
	}
}

func TestLyricsDocumentSegmentsCarryPerSegmentPerformersInTheirOrder(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentFidelityRequest()
	request.Renditions[0] = LyricsDocumentRendition{
		Key: "sekai", Kind: "sekai", Label: "SEKAI Version", Game: "cut", PerformerIDs: []int{1, 2},
		Lines: []LyricsDocumentLine{
			{Japanese: "{試験|しけん}の{歌|うた}をうたう", Chinese: "唱起测试之歌", InGame: true, Segments: []LyricsDocumentSegment{
				{Japanese: "{試験|しけん}の", PerformerIDs: []int{2, 1}}, {Japanese: "{歌|うた}を", PerformerIDs: []int{1}},
				{Japanese: "うたう", PerformerIDs: []int{}},
			}},
			{Japanese: "らららと{合成|ごうせい}する", Chinese: "啦啦啦地合成", StanzaBreakBefore: true, PerformerIDs: []int{2, 1}},
			{Japanese: "{点線|てんせん}をなぞる", Chinese: "描过虚线", InGame: true},
			{Japanese: "おわり", Chinese: "结束"},
		},
	}
	detail, export, _ := exportPublishedLyricsDocument(t, s, request)
	rendition := detail.Renditions[0]
	wantSegments := []PublicLyricsV3Segment{
		{Text: "試験の", PerformerIDs: []string{"歌唱者-02", "歌唱者-01"}, Ruby: []PublicLyricsV3RubySpan{{Text: "試験", Reading: "しけん"}, {Text: "の"}}},
		{Text: "歌を", PerformerIDs: []string{"歌唱者-01"}, Ruby: []PublicLyricsV3RubySpan{{Text: "歌", Reading: "うた"}, {Text: "を"}}},
		{Text: "うたう", PerformerIDs: []string{}, Ruby: []PublicLyricsV3RubySpan{{Text: "うたう"}}},
	}
	if got := rendition.Full.Lines[0].Segments; !reflect.DeepEqual(got, wantSegments) {
		t.Fatalf("segmented line=%+v", got)
	}
	if got := rendition.Game.Lines[0].Segments; !reflect.DeepEqual(got, wantSegments) {
		t.Fatalf("projected Game line=%+v", got)
	}
	if got := rendition.Full.Lines[1].Segments[0].PerformerIDs; !reflect.DeepEqual(got, []string{"歌唱者-02", "歌唱者-01"}) {
		t.Fatalf("line performer order=%v", got)
	}
	if len(rendition.Performers) != 2 || rendition.Performers[0].PerformerID != "歌唱者-01" {
		t.Fatalf("the rendition roster stays a sorted set: %+v", rendition.Performers)
	}
	request.ExpectedRevision = &detail.Revision
	if len(export.Warnings) != 0 || !reflect.DeepEqual(export.Document, request) {
		t.Fatalf("export=%+v warnings=%v\nwant=%+v", export.Document, export.Warnings, request)
	}

	// A segment without performerIds sings with the line's performers.
	request = lyricsDocumentTestRequest()
	request.Renditions[0].Lines[0].PerformerIDs = []int{2}
	request.Renditions[0].Lines[0].Segments = []LyricsDocumentSegment{
		{Japanese: "{試験|しけん}の{歌|うた}"}, {Japanese: "をうたう", PerformerIDs: []int{1, 2}},
	}
	request.ExpectedRevision = &detail.Revision
	_, detail = publishLyricsDocumentForTest(t, s, request, 0)
	if got := detail.Renditions[0].Full.Lines[0].Segments; len(got) != 2 || !reflect.DeepEqual(got[0].PerformerIDs, []string{"歌唱者-02"}) ||
		!reflect.DeepEqual(got[1].PerformerIDs, []string{"歌唱者-01", "歌唱者-02"}) {
		t.Fatalf("inherited segment performers=%+v", got)
	}
}

func TestLyricsDocumentSegmentsAreValidated(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	manySegments := make([]LyricsDocumentSegment, maxLyricsSegmentsPerLine+1)
	for index := range manySegments {
		manySegments[index] = LyricsDocumentSegment{Japanese: "ら"}
	}
	for _, test := range []struct {
		name     string
		ja       string
		segments []LyricsDocumentSegment
		message  string
	}{
		{"not concatenating", "{試験|しけん}の{歌|うた}", []LyricsDocumentSegment{{Japanese: "{試験|しけん}"}, {Japanese: "の"}},
			"the segment ja values must concatenate to the line's ja"},
		{"a character left out", "{試験|しけん}の{歌|うた}", []LyricsDocumentSegment{{Japanese: "{試験|しけん}"}, {Japanese: "{歌|うた}"}},
			`the segment ja values must concatenate to the line's ja; they first differ at character 8: the line has "の{歌|うた}", the segments give "{歌|うた}"`},
		{"empty segment", "らら", []LyricsDocumentSegment{{Japanese: ""}, {Japanese: "らら"}}, "segment 0 has no text"},
		{"blank segment", "ら　ら", []LyricsDocumentSegment{{Japanese: "ら"}, {Japanese: "　"}, {Japanese: "ら"}}, "segment 1 has no text"},
		{"ruby across segments", "{試験|しけん}", []LyricsDocumentSegment{{Japanese: "{試験"}, {Japanese: "|しけん}"}},
			"segment 0: unclosed '{' at character 0"},
		{"unknown performer", "らら", []LyricsDocumentSegment{{Japanese: "ら", PerformerIDs: []int{1}}, {Japanese: "ら", PerformerIDs: []int{999}}},
			"segment 1: performer ID 999 is not a known character"},
		{"no segments", "らら", []LyricsDocumentSegment{}, "segments, when present, must list 1 to 100 segments"},
		{"too many segments", strings.Repeat("ら", len(manySegments)), manySegments, "segments, when present, must list 1 to 100 segments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := lyricsDocumentTestRequest()
			request.Renditions[0].Lines[1] = LyricsDocumentLine{Japanese: test.ja, Chinese: "合成", Segments: test.segments}
			requireLyricsDocumentIssue(t, lyricsDocumentErrorFor(t, s, request, 0), "sekai/full/segments@1", test.message)
		})
	}
}

func TestLyricsDocumentGameOnlyRenditionKeepsTheServedState(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentFidelityRequest()
	request.Renditions = []LyricsDocumentRendition{
		{
			Key: "alternate-synthetic", Kind: "alternate", Label: "Alternate Vocal — Synthetic", PerformerIDs: []int{2}, Game: "only",
			Lines: []LyricsDocumentLine{},
			GameLines: []LyricsDocumentLine{
				{Japanese: "{短|みじか}い{試験|しけん}", Chinese: "短短的测试"},
				{Japanese: "おわり", Chinese: "结束", PerformerIDs: []int{1}, StanzaBreakBefore: true},
			},
		},
		{Key: "sekai", Kind: "sekai", Label: "SEKAI Version", Game: "none", PerformerIDs: []int{1, 2},
			Lines: []LyricsDocumentLine{{Japanese: "{試験|しけん}の{歌|うた}", Chinese: "测试之歌"}}},
	}
	detail, export, _ := exportPublishedLyricsDocument(t, s, request)
	gameOnly := detail.Renditions[0]
	components := map[string]bool{}
	for _, attribution := range gameOnly.Provenance {
		components[strings.TrimPrefix(attribution.Component, "renditions/alternate-synthetic/")] = true
	}
	if detail.State != PublicLyricsStateComplete || gameOnly.Full != nil || gameOnly.Game == nil ||
		!reflect.DeepEqual(gameOnly.AvailableVersions, []string{"game"}) || gameOnly.Label != "Alternate Vocal — Synthetic" ||
		gameOnly.Relation.Kind != model.LyricsSourceRenditionRelationNone || gameOnly.Game.Lines[1].Chinese != "结束" ||
		components["full_text"] || !components["game_text"] || !components["game_ruby"] ||
		gameOnly.TranslationCredits == nil || gameOnly.TranslationCredits.Translation != "合成译者" {
		t.Fatalf("Game-only rendition=%+v components=%v", gameOnly, components)
	}
	request.ExpectedRevision = &detail.Revision
	if len(export.Warnings) != 0 || !reflect.DeepEqual(export.Document, request) {
		t.Fatalf("export=%+v warnings=%v\nwant=%+v", export.Document, export.Warnings, request)
	}

	// A song whose only rendition is Game-only is served as game_only.
	request = LyricsDocumentRequest{
		MusicID: lyricsDocumentTestMusicID, Source: request.Source, TranslationCredit: "合成译者",
		Renditions: []LyricsDocumentRendition{{Key: "sekai", Game: "only", Lines: []LyricsDocumentLine{},
			GameLines: []LyricsDocumentLine{{Japanese: "{短|みじか}い{歌|うた}", Chinese: "短歌"}}}},
	}
	detail, export, _ = exportPublishedLyricsDocument(t, s, request)
	index, _, _, err := s.PublishedLyricsLocalizationProjection()
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != PublicLyricsStateGameOnly || len(index) != 1 || index[0].State != PublicLyricsStateGameOnly ||
		export.Document.Renditions[0].Game != "only" || len(export.Document.Renditions[0].GameLines) != 1 {
		t.Fatalf("detail state=%s index=%+v export=%+v", detail.State, index, export.Document)
	}

	for _, test := range []struct {
		name       string
		rendition  LyricsDocumentRendition
		key        string
		message    string
		lineIssues bool
	}{
		{"lines given", LyricsDocumentRendition{Key: "sekai", Game: "only", Lines: request.Renditions[0].GameLines, GameLines: request.Renditions[0].GameLines},
			"sekai/full/lines", "a Game-only rendition has no lines", false},
		{"no gameLines", LyricsDocumentRendition{Key: "sekai", Game: "only"}, "sekai/game/gameLines", "a Game-only rendition needs gameLines", false},
		{"inGame", LyricsDocumentRendition{Key: "sekai", Game: "only", GameLines: []LyricsDocumentLine{{Japanese: "らら", Chinese: "啦啦", InGame: true}}},
			"sekai/game/inGame@0", "inGame is only valid on Full lines when game is cut", true},
		{"gameLines with none", LyricsDocumentRendition{Key: "sekai", Lines: request.Renditions[0].GameLines, GameLines: request.Renditions[0].GameLines},
			"sekai/game/gameLines", "gameLines are only valid when game is independent or only", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := request
			invalid.Renditions = []LyricsDocumentRendition{test.rendition}
			requireLyricsDocumentIssue(t, lyricsDocumentErrorFor(t, s, invalid, 0), test.key, test.message)
		})
	}
}

func TestLyricsDocumentMarkupEscapesLiteralBraces(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentFidelityRequest()
	request.Renditions[0].Lines[1] = LyricsDocumentLine{
		Japanese: "{{{合成|ごうせい}}}と|と\\{{", Chinese: "{合成}与|与\\{", StanzaBreakBefore: true,
	}
	detail, export, _ := exportPublishedLyricsDocument(t, s, request)
	line := detail.Renditions[0].Full.Lines[1]
	wantRuby := []PublicLyricsV3RubySpan{{Text: "{"}, {Text: "合成", Reading: "ごうせい"}, {Text: "}と|と\\{"}}
	if line.Japanese != "{合成}と|と\\{" || line.Chinese != "{合成}与|与\\{" || !reflect.DeepEqual(line.Segments[0].Ruby, wantRuby) {
		t.Fatalf("served line=%+v", line)
	}
	request.ExpectedRevision = &detail.Revision
	if len(export.Warnings) != 0 || !reflect.DeepEqual(export.Document, request) {
		t.Fatalf("export=%+v warnings=%v", export.Document, export.Warnings)
	}
	for markup, problem := range map[string]string{
		"らら}": "unbalanced '}' at character 2; write }} for a literal }",
		"{らら": "unclosed '{' at character 0; write {{ for a literal {",
	} {
		if _, _, problems := parseLyricsDocumentRuby(markup); len(problems) != 1 || problems[0] != problem {
			t.Fatalf("%q problems=%v", markup, problems)
		}
	}
}

func TestLyricsDocumentRenditionCreditsReplaceTheDocumentCredits(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentFidelityRequest()
	request.ProofreadingCredit = "合成校对"
	request.Renditions = []LyricsDocumentRendition{
		{Key: "alternate-synthetic", Kind: "alternate", Label: "Alternate Vocal — Synthetic", PerformerIDs: []int{2}, Game: "none",
			Lines:              []LyricsDocumentLine{{Japanese: "らら", Chinese: "啦啦"}},
			TranslationCredits: &LyricsDocumentCredits{}},
		{Key: "sekai", Kind: "sekai", Label: "SEKAI Version", Game: "none", PerformerIDs: []int{1, 2},
			Lines: []LyricsDocumentLine{{Japanese: "{試験|しけん}の{歌|うた}", Chinese: "测试之歌"}}},
		{Key: "vocaloid", Kind: "vocaloid", Label: "VIRTUAL SINGER Version", Game: "none", PerformerIDs: []int{21},
			Lines:              []LyricsDocumentLine{{Japanese: "ミクのうた"}},
			TranslationCredits: &LyricsDocumentCredits{Translation: "另一位译者"}},
	}
	detail, export, _ := exportPublishedLyricsDocument(t, s, request)
	credits := map[string]*PublicLyricsV3TranslationCredits{}
	for _, rendition := range detail.Renditions {
		credits[rendition.Key] = rendition.TranslationCredits
	}
	if credits["alternate-synthetic"] != nil ||
		!reflect.DeepEqual(credits["sekai"], &PublicLyricsV3TranslationCredits{Translation: "合成译者", Proofreading: "合成校对"}) ||
		!reflect.DeepEqual(credits["vocaloid"], &PublicLyricsV3TranslationCredits{Translation: "另一位译者"}) {
		t.Fatalf("served credits=%+v", credits)
	}
	request.ExpectedRevision = &detail.Revision
	if len(export.Warnings) != 0 || !reflect.DeepEqual(export.Document, request) {
		t.Fatalf("export=%+v warnings=%v\nwant=%+v", export.Document, export.Warnings, request)
	}
	// A song whose renditions share the document credits exports without any.
	_, export, _ = exportPublishedLyricsDocument(t, s, lyricsDocumentExportTestRequest())
	for _, rendition := range export.Document.Renditions {
		if rendition.TranslationCredits != nil {
			t.Fatalf("%s exports its own credits %+v", rendition.Key, rendition.TranslationCredits)
		}
	}

	for field, credits := range map[string]LyricsDocumentCredits{
		"translationCredits.translation":  {Translation: "一行\n两行"},
		"translationCredits.proofreading": {Proofreading: strings.Repeat("校", 700)},
	} {
		invalid := lyricsDocumentTestRequest()
		invalid.Renditions[0].TranslationCredits = &credits
		requireLyricsDocumentIssue(t, lyricsDocumentErrorFor(t, s, invalid, 0), "sekai//"+field, "must be one line of at most 2048 bytes")
	}
}

func TestExportLyricsDocumentReportsAnUnpublishedLegacyDraftThatPUTReplaces(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	// The song is served from a bundle at revision 1.
	dry := lyricsDocumentTestRequest()
	dry.DryRun = true
	_, bundleDetail := publishLyricsDocumentForTest(t, s, dry, 0)
	bundleDetail.Revision = 1
	served, err := EncodePublicLyricsV3Detail(bundleDetail)
	if err != nil {
		t.Fatal(err)
	}
	// An unpublished legacy draft reaches revision 4.
	legacy := validLyrics()
	legacy.MusicID = lyricsDocumentTestMusicID
	saveDraft := func(revision int) int {
		legacy.Revision = revision
		legacy.Lines[0].Chinese = strings.Repeat("改", revision+1)
		saved, err := s.SaveLyrics(legacy, "legacy-editor")
		if err != nil {
			t.Fatal(err)
		}
		return saved.Revision
	}
	revision := 0
	for revision < 4 {
		revision = saveDraft(revision)
	}
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 1)
	if err != nil {
		t.Fatal(err)
	}
	if codes := lyricsDocumentExportWarningCodes(export.Warnings); *export.Document.ExpectedRevision != 4 ||
		!reflect.DeepEqual(codes, []string{LyricsDocumentExportWarningUnpublishedDraft}) {
		t.Fatalf("expectedRevision=%d warnings=%v", *export.Document.ExpectedRevision, codes)
	}
	draftRows := func() int {
		return lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM song_lyrics WHERE music_id=?`, lyricsDocumentTestMusicID)
	}
	// The served revision no longer matches, so a PUT cannot delete the draft
	// without naming it.
	stale := export.Document
	bundleRevision := 1
	stale.ExpectedRevision = &bundleRevision
	if documentErr := lyricsDocumentErrorFor(t, s, stale, 1); documentErr.Code != LyricsDocumentErrorRevisionConflict ||
		!reflect.DeepEqual(documentErr.Current, map[string]int{"revision": 4}) || draftRows() != 1 {
		t.Fatalf("stale PUT error=%+v drafts=%d", documentErr, draftRows())
	}
	// A legacy save between the export and its PUT is a conflict.
	saveDraft(revision)
	if documentErr := lyricsDocumentErrorFor(t, s, export.Document, 1); documentErr.Code != LyricsDocumentErrorRevisionConflict ||
		!reflect.DeepEqual(documentErr.Current, map[string]int{"revision": 5}) || draftRows() != 1 {
		t.Fatalf("PUT after a concurrent save error=%+v drafts=%d", documentErr, draftRows())
	}
	export, err = s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 1)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := publishLyricsDocumentForTest(t, s, export.Document, 1)
	if result.Revision != 6 || draftRows() != 0 {
		t.Fatalf("revision=%d drafts=%d", result.Revision, draftRows())
	}
}

// A published legacy song with a later draft ("draft-published") is served
// at the publication revision; the export names the newer draft.
func TestExportLyricsDocumentReportsADraftOverALegacyPublication(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourceURL = "https://www.sekaipedia.org/wiki/Test_Song?oldid=77"
	input.SourcePageID, input.SourceRevisionID = 10, 77
	input.SourceSHA1, input.SourceFetchedAt = validSourceSHA1, "2026-07-22T12:00:00Z"
	saved, _, err := s.SaveLyricsMutation(input, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	served := servedLegacyLyricsDetail(t, s, 10)
	input.Revision = saved.Revision
	input.Lines[0].Chinese = "发布后的草稿"
	draft, _, err := s.SaveLyricsMutation(input, "agent")
	if err != nil || draft.Revision != saved.Revision+1 {
		t.Fatalf("draft=%+v err=%v", draft, err)
	}
	export, err := s.ExportLyricsDocument(context.Background(), 10, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := lyricsDocumentExportWarningCodes(export.Warnings)
	if export.ServedRevision != saved.Revision || *export.Document.ExpectedRevision != draft.Revision ||
		!sliceHasString(codes, LyricsDocumentExportWarningUnpublishedDraft) || sliceHasString(codes, LyricsDocumentExportWarningServedRevision) {
		t.Fatalf("served=%d expected=%d warnings=%v", export.ServedRevision, *export.Document.ExpectedRevision, codes)
	}
}

func TestLyricsDocumentLineLimitsNameTheEnforcedSizes(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	request.Renditions[0].Lines[1] = LyricsDocumentLine{Japanese: strings.Repeat("あ", MaxLyricsDocumentJapaneseLineBytes/3+1), Chinese: "长行"}
	request.Renditions[0].Lines[2].Chinese = strings.Repeat("长", maxLyricsLineTextBytes/3+1)
	documentErr := lyricsDocumentErrorFor(t, s, request, 0)
	requireLyricsDocumentIssue(t, documentErr, "sekai/full/ja@1", "ja must be one non-empty line of at most 8192 bytes")
	requireLyricsDocumentIssue(t, documentErr, "sekai/full/zh@2", "zh must be one line of at most 16384 bytes")

	// A zh line above the source model's ja limit is within its own limit.
	request = lyricsDocumentTestRequest()
	request.Renditions[0].Lines[2].Chinese = strings.Repeat("长", 3000)
	request.DryRun = true
	publishLyricsDocumentForTest(t, s, request, 0)
}

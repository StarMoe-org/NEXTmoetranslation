package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"moesekai/server/internal/model"
)

// lyricsDocumentExportComparable normalizes the fields a PUT of an export
// regenerates: revision, updatedAt and the schema version (legacy details are
// republished as v3), positional line ids (the projection is compared by the
// Full positions it selects), sourceTabPaths derived from the label, and
// provenance component names (pjsk.moe lists the distinct attributions), and
// boundaries between adjacent segments with the same single performer or no
// performer, or between adjacent unannotated ruby spans, which render
// identically.
func lyricsDocumentExportComparable(t *testing.T, detail PublicLyricsV3DetailDocument) any {
	t.Helper()
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var normalized PublicLyricsV3DetailDocument
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	normalized.Version, normalized.Revision, normalized.UpdatedAt = 0, 0, ""
	for index := range normalized.Renditions {
		rendition := &normalized.Renditions[index]
		rendition.SourceTabPaths = nil
		var attributions []PublicLyricsV3ComponentAttribution
		seen := map[PublicLyricsV3ComponentAttribution]bool{}
		for _, attribution := range rendition.Provenance {
			attribution.Component = ""
			if !seen[attribution] {
				seen[attribution] = true
				attributions = append(attributions, attribution)
			}
		}
		rendition.Provenance = attributions
		positions := map[string]string{}
		for _, side := range []*PublicLyricsV3Side{rendition.Full, rendition.Game} {
			if side == nil {
				continue
			}
			for lineIndex := range side.Lines {
				if side == rendition.Full {
					positions[side.Lines[lineIndex].ID] = fmt.Sprintf("full#%d", lineIndex)
				}
				side.Lines[lineIndex].ID = ""
				side.Lines[lineIndex].Segments = mergeLyricsDocumentExportSegments(side.Lines[lineIndex].Segments)
			}
		}
		for lineIndex, lineID := range rendition.Relation.LineIDs {
			rendition.Relation.LineIDs[lineIndex] = positions[lineID]
		}
	}
	if encoded, err = json.Marshal(normalized); err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

func mergeLyricsDocumentExportSegments(segments []PublicLyricsV3Segment) []PublicLyricsV3Segment {
	var merged []PublicLyricsV3Segment
	for _, segment := range segments {
		last := len(merged) - 1
		if last >= 0 && len(segment.PerformerIDs) <= 1 && reflect.DeepEqual(merged[last].PerformerIDs, segment.PerformerIDs) {
			merged[last].Text += segment.Text
			merged[last].Ruby = append(merged[last].Ruby, segment.Ruby...)
			continue
		}
		merged = append(merged, segment)
	}
	for index := range merged {
		var spans []PublicLyricsV3RubySpan
		for _, span := range merged[index].Ruby {
			if last := len(spans) - 1; last >= 0 && span.Reading == "" && spans[last].Reading == "" {
				spans[last].Text += span.Text
				continue
			}
			spans = append(spans, span)
		}
		merged[index].Ruby = spans
	}
	return merged
}

func lyricsDocumentExportTreeDiffs(path string, left, right any, diffs []string) []string {
	switch l := left.(type) {
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok {
			return append(diffs, path)
		}
		keys := map[string]bool{}
		for key := range l {
			keys[key] = true
		}
		for key := range r {
			keys[key] = true
		}
		sorted := make([]string, 0, len(keys))
		for key := range keys {
			sorted = append(sorted, key)
		}
		sort.Strings(sorted)
		for _, key := range sorted {
			diffs = lyricsDocumentExportTreeDiffs(path+"."+key, l[key], r[key], diffs)
		}
		return diffs
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return append(diffs, path)
		}
		for index := range l {
			diffs = lyricsDocumentExportTreeDiffs(fmt.Sprintf("%s[%d]", path, index), l[index], r[index], diffs)
		}
		return diffs
	default:
		if !reflect.DeepEqual(left, right) {
			return append(diffs, path)
		}
		return diffs
	}
}

// compileLyricsDocumentExport sends the JSON form of an export through the
// PUT dryRun path and returns the detail PUT would publish.
func compileLyricsDocumentExport(t *testing.T, s *Store, export LyricsDocumentExport, bundleRevision int) (PublicLyricsV3DetailDocument, error) {
	t.Helper()
	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Document LyricsDocumentRequest `json:"document"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(envelope.Document, export.Document) {
		t.Fatalf("export JSON does not decode to its request:\n%+v\n%+v", envelope.Document, export.Document)
	}
	request := envelope.Document
	request.DryRun = true
	result, err := s.PublishLyricsDocument(context.Background(), request, bundleRevision, "export-round-trip")
	if err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	detail, err := DecodePublicLyricsV3Detail(result.Document)
	if err != nil {
		t.Fatalf("decode compiled export: %v", err)
	}
	return detail, nil
}

func lyricsDocumentExportWarningCodes(warnings []LyricsDocumentExportWarning) []string {
	codes := make([]string, len(warnings))
	for index, warning := range warnings {
		codes[index] = warning.Code
		if warning.Line != nil {
			codes[index] += fmt.Sprintf("@%s/%s/%d", warning.Rendition, warning.Side, *warning.Line)
		} else if warning.Rendition != "" {
			codes[index] += "@" + warning.Rendition
		}
	}
	return codes
}

func servedLegacyLyricsDetail(t *testing.T, s *Store, musicID int) []byte {
	t.Helper()
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := details[musicID]
	if !ok {
		t.Fatalf("music %d is not served", musicID)
	}
	body, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func liftedLegacyLyricsDetail(t *testing.T, served []byte) PublicLyricsV3DetailDocument {
	t.Helper()
	var legacy PublicLyricsDetailDocument
	if err := json.Unmarshal(served, &legacy); err != nil {
		t.Fatal(err)
	}
	return (&lyricsDocumentExporter{}).fromLegacyDetail(legacy)
}

func TestExportLyricsDocumentRoundTripsALegacyV1Detail(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourceURL = "https://www.sekaipedia.org/wiki/Test_Song?oldid=77"
	input.SourcePageID, input.SourceRevisionID = 10, 77
	input.SourceSHA1, input.SourceFetchedAt = validSourceSHA1, "2026-07-22T12:00:00Z"
	input.Lines = []model.LyricLine{
		{ID: "line-1", Order: 0, Japanese: "うたう", Chinese: "歌唱", Segments: []model.LyricSegment{{Text: "うたう", PerformerIDs: []int{1}}}},
		{ID: "line-2", Order: 1, Japanese: "ららら", Chinese: "啦啦啦", StanzaBreakBefore: true,
			Segments: []model.LyricSegment{{Text: "らら", PerformerIDs: []int{1, 2}}, {Text: "ら", PerformerIDs: []int{1, 2}}}},
		{ID: "line-3", Order: 2, Japanese: "ルルル", Segments: []model.LyricSegment{{Text: "ルルル", PerformerIDs: []int{1}}}},
	}
	saved, _, err := s.SaveLyricsMutation(input, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	served := servedLegacyLyricsDetail(t, s, 10)
	export, err := s.ExportLyricsDocument(context.Background(), 10, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	expected := saved.Revision
	want := LyricsDocumentRequest{
		MusicID: 10, ExpectedRevision: &expected,
		Source:            LyricsDocumentSource{URL: "https://www.sekaipedia.org/wiki/Test_Song?oldid=77", Title: "Test Song"},
		TranslationCredit: input.Attribution,
		Renditions: []LyricsDocumentRendition{{
			Key: "sekai", Kind: "sekai", Label: "SEKAI Version", Game: "none", PerformerIDs: []int{1},
			Lines: []LyricsDocumentLine{
				{Japanese: "うたう", Chinese: "歌唱"},
				// The main site draws each segment with several performers as its
				// own gradient span, so the export keeps the boundary.
				{Japanese: "ららら", Chinese: "啦啦啦", StanzaBreakBefore: true, Segments: []LyricsDocumentSegment{
					{Japanese: "らら", PerformerIDs: []int{1, 2}}, {Japanese: "ら", PerformerIDs: []int{1, 2}},
				}},
				{Japanese: "ルルル"},
			},
		}},
	}
	if export.ServedVersion != 1 || export.ServedRevision != saved.Revision || !reflect.DeepEqual(export.Document, want) {
		t.Fatalf("export=%+v\nwant document=%+v", export, want)
	}
	if codes := lyricsDocumentExportWarningCodes(export.Warnings); !reflect.DeepEqual(codes, []string{"rendition_inferred@sekai"}) {
		t.Fatalf("warnings=%v", codes)
	}
	produced, err := compileLyricsDocumentExport(t, s, export, 0)
	if err != nil {
		t.Fatal(err)
	}
	if diffs := lyricsDocumentExportTreeDiffs("", lyricsDocumentExportComparable(t, liftedLegacyLyricsDetail(t, served)),
		lyricsDocumentExportComparable(t, produced), nil); len(diffs) != 0 {
		t.Fatalf("compiled v1 export differs at %v", diffs)
	}
}

func TestExportLyricsDocumentWarnsThatALegacyV1DetailHasNoRuby(t *testing.T) {
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
	export, err := s.ExportLyricsDocument(context.Background(), 10, servedLegacyLyricsDetail(t, s, 10), 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := lyricsDocumentExportWarningCodes(export.Warnings)
	for _, code := range []string{"ruby_not_served@sekai/full/0", "english_dropped@sekai/full/0"} {
		if !sliceHasString(codes, code) {
			t.Fatalf("warnings=%v lack %s", codes, code)
		}
	}
	// The served segments sing with different performers and are exported as
	// request segments.
	wantSegments := []LyricsDocumentSegment{{Japanese: "初音", PerformerIDs: []int{1}}, {Japanese: "歌う", PerformerIDs: []int{1, 2}}}
	if line := export.Document.Renditions[0].Lines[0]; line.Japanese != "初音歌う" || !reflect.DeepEqual(line.Segments, wantSegments) || line.PerformerIDs != nil {
		t.Fatalf("exported line=%+v", line)
	}
	_, err = compileLyricsDocumentExport(t, s, export, 0)
	var documentErr *LyricsDocumentError
	if !errors.As(err, &documentErr) || documentErr.Code != LyricsDocumentErrorInvalid || len(documentErr.Issues) != 1 ||
		documentErr.Issues[0].Field != "ja" || documentErr.Issues[0].Line == nil || *documentErr.Issues[0].Line != 0 {
		t.Fatalf("PUT of a ruby-less export err=%v %+v", err, documentErr)
	}
}

func sliceHasString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func TestExportLyricsDocumentRoundTripsALegacyV2Detail(t *testing.T) {
	s := setupLyricsStore(t)
	input, document := publicLyricsV2SekaiFixture(10)
	saved := savePublicLyricsV2Fixture(t, s, input, document)
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	served := servedLegacyLyricsDetail(t, s, 10)
	export, err := s.ExportLyricsDocument(context.Background(), 10, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	rendition := export.Document.Renditions[0]
	if export.ServedVersion != 2 || export.Document.Source.URL != "https://vocaloid.fandom.com/wiki/Public_Test_101?oldid=102" ||
		export.Document.TranslationCredit != "Legacy Translator" || rendition.Game != "cut" ||
		!reflect.DeepEqual(rendition.PerformerIDs, []int{1}) || len(rendition.Lines) != 2 ||
		rendition.Lines[0].Japanese != "{初音|はつね}{歌|うた}う" || !rendition.Lines[0].InGame || rendition.Lines[1].InGame ||
		!rendition.Lines[1].StanzaBreakBefore {
		t.Fatalf("export=%+v", export)
	}
	wantCodes := []string{
		"rendition_inferred@sekai", "source_differs@sekai", "trailing_performers_dropped@sekai/full/0",
		"english_dropped@sekai/full/0", "trailing_performers_dropped@sekai/full/1",
	}
	if codes := lyricsDocumentExportWarningCodes(export.Warnings); !reflect.DeepEqual(codes, wantCodes) {
		t.Fatalf("warnings=%v", codes)
	}
	produced, err := compileLyricsDocumentExport(t, s, export, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Each remaining difference is one the warnings above name.
	diffs := lyricsDocumentExportTreeDiffs("", lyricsDocumentExportComparable(t, liftedLegacyLyricsDetail(t, served)),
		lyricsDocumentExportComparable(t, produced), nil)
	wantDiffs := []string{
		".renditions[0].full.lines[0].en-US", ".renditions[0].full.lines[0].trailingPerformerIds",
		".renditions[0].full.lines[1].trailingPerformerIds",
		".renditions[0].game.lines[0].en-US", ".renditions[0].game.lines[0].trailingPerformerIds",
		".renditions[0].provenance",
	}
	if !reflect.DeepEqual(diffs, wantDiffs) {
		t.Fatalf("compiled v2 export differs at %v", diffs)
	}
}

func lyricsDocumentExportTestRequest() LyricsDocumentRequest {
	return LyricsDocumentRequest{
		MusicID:           lyricsDocumentTestMusicID,
		Source:            LyricsDocumentSource{URL: "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4242", Title: "合成試験曲"},
		TranslationCredit: "合成译者", ProofreadingCredit: "合成校对",
		Renditions: []LyricsDocumentRendition{
			{
				Key: "sekai", Kind: "sekai", Label: "SEKAI Version", Game: "cut", PerformerIDs: []int{1, 2},
				Lines: []LyricsDocumentLine{
					{Japanese: "{試験|しけん}の{歌|うた}", Chinese: "测试之歌", InGame: true},
					{Japanese: "らららと{合成|ごうせい}", Chinese: "啦啦啦地合成", StanzaBreakBefore: true, PerformerIDs: []int{1}},
					{Japanese: "{点線|てんせん}をなぞる", Chinese: "描过虚线", InGame: true, PerformerIDs: []int{}},
				},
			},
			{
				Key: "vocaloid", Kind: "vocaloid", Label: "VIRTUAL SINGER Version", Game: "independent", PerformerIDs: []int{21},
				Lines:     []LyricsDocumentLine{{Japanese: "ミクのうた", Chinese: "未来之歌"}, {Japanese: "ルルル", Chinese: "噜噜噜"}},
				GameLines: []LyricsDocumentLine{{Japanese: "ミクのうた", Chinese: "未来之歌", StanzaBreakBefore: true}},
			},
		},
	}
}

func TestExportLyricsDocumentReproducesAPublishedDocumentRequest(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentExportTestRequest()
	result, detail := publishLyricsDocumentForTest(t, s, request, 0)
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, result.Document, 0)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = &result.Revision
	if export.ServedVersion != 3 || export.ServedRevision != result.Revision || len(export.Warnings) != 0 ||
		!reflect.DeepEqual(export.Document, request) {
		t.Fatalf("export=%+v\nwant=%+v", export, request)
	}
	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Document struct {
			Renditions []struct {
				Lines []map[string]json.RawMessage `json:"lines"`
			} `json:"renditions"`
		} `json:"document"`
		Warnings []LyricsDocumentExportWarning `json:"warnings"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil || body.Warnings == nil ||
		string(body.Document.Renditions[0].Lines[2]["performerIds"]) != "[]" {
		t.Fatalf("export JSON must keep an explicit empty performer list and a warnings array: %s", encoded)
	}
	produced, err := compileLyricsDocumentExport(t, s, export, 0)
	if err != nil {
		t.Fatal(err)
	}
	produced.Revision, produced.UpdatedAt = detail.Revision, detail.UpdatedAt
	if !reflect.DeepEqual(produced, detail) {
		t.Fatalf("compiled export differs from the served detail:\n%+v\n%+v", produced, detail)
	}
}

func TestExportLyricsDocumentWarnsWhereTheRequestCannotCarryTheDetail(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	result, detail := publishLyricsDocumentForTest(t, s, lyricsDocumentExportTestRequest(), 0)
	sekai, vocaloid := &detail.Renditions[0], &detail.Renditions[1]
	// The Game side of an exact projection sings its first line with another
	// performer, which same and cut cannot express.
	sekai.Game.Lines[0].Segments[0].PerformerIDs = []string{"歌唱者-02"}
	// Served performer order that PUT's sorted order changes.
	sekai.Full.Lines[0].Segments[0].PerformerIDs = []string{"歌唱者-02", "歌唱者-01"}
	// The vocaloid Game text is attributed to a later wiki revision.
	for index := range vocaloid.Provenance {
		if vocaloid.Provenance[index].Component == "renditions/vocaloid/game_text" {
			vocaloid.Provenance[index].RevisionID = 4243
			vocaloid.Provenance[index].RevisionURL = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4243"
		}
	}
	detail.Revision = result.Revision + 5
	served, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, served, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantCodes := []string{"source_differs@vocaloid", "game_projection_differs@sekai/game/0", "served_revision_differs"}
	if codes := lyricsDocumentExportWarningCodes(export.Warnings); !reflect.DeepEqual(codes, wantCodes) {
		t.Fatalf("warnings=%v", codes)
	}
	rendition := export.Document.Renditions[0]
	if *export.Document.ExpectedRevision != result.Revision || export.Document.Source.URL != lyricsDocumentExportTestRequest().Source.URL ||
		rendition.Game != "independent" || len(rendition.GameLines) != 2 || rendition.Lines[0].InGame ||
		!reflect.DeepEqual(rendition.GameLines[0].PerformerIDs, []int{2}) || rendition.GameLines[1].PerformerIDs == nil {
		t.Fatalf("export=%+v", export)
	}
	produced, err := compileLyricsDocumentExport(t, s, export, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := produced.Renditions[0].Game.Lines[0].Segments[0].PerformerIDs; !reflect.DeepEqual(got, []string{"歌唱者-02"}) {
		t.Fatalf("independent Game performers=%v", got)
	}
	// The served performer order is kept, as the main site draws it.
	if got := produced.Renditions[0].Full.Lines[0].Segments[0].PerformerIDs; !reflect.DeepEqual(got, []string{"歌唱者-02", "歌唱者-01"}) {
		t.Fatalf("Full performers=%v", got)
	}
}

func TestExportLyricsDocumentCarriesEveryTranslationEditionOfAV4Detail(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	for _, tc := range []struct {
		fixture        string
		codes          []string
		game           string
		zh             []string
		editions       []LyricsTranslationEditionSummary
		zhEditions     []map[string]string
		editionCredits map[string]LyricsDocumentCredits
	}{
		{"detail-multi-edition.fixture.json", nil, "none", []string{"默认译文"},
			[]LyricsTranslationEditionSummary{{Key: "main", Label: "默认译本"}, {Key: "alternate", Label: "另一译本"}},
			[]map[string]string{{"alternate": "另一种译文"}},
			map[string]LyricsDocumentCredits{"alternate": {Translation: "Legacy Translator"}}},
		{"detail-exact-projection.fixture.json", []string{"credit_missing"}, "same", []string{"歌唱", "前进吧"},
			nil, []map[string]string{nil, nil}, nil},
	} {
		body, err := os.ReadFile("../../../contracts/public-lyrics/v4/" + tc.fixture)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			MusicID int `json:"musicId"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		export, err := s.ExportLyricsDocument(context.Background(), envelope.MusicID, body, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		var codes, zh []string
		for _, warning := range export.Warnings {
			if warning.Code != LyricsDocumentExportWarningServedRevision {
				codes = append(codes, warning.Code)
			}
		}
		rendition := export.Document.Renditions[0]
		var zhEditions []map[string]string
		for _, line := range rendition.Lines {
			zh = append(zh, line.Chinese)
			zhEditions = append(zhEditions, line.ChineseEditions)
		}
		if export.ServedVersion != 4 || !reflect.DeepEqual(codes, tc.codes) || rendition.Game != tc.game || !reflect.DeepEqual(zh, tc.zh) ||
			!reflect.DeepEqual(export.Document.TranslationEditions, tc.editions) || !reflect.DeepEqual(zhEditions, tc.zhEditions) ||
			!reflect.DeepEqual(rendition.EditionCredits, tc.editionCredits) {
			t.Fatalf("%s: export=%+v codes=%v", tc.fixture, export, codes)
		}
	}
}

func TestExportLyricsDocumentRejectsAMismatchedOrUnknownDetail(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	if _, err := s.ExportLyricsDocument(context.Background(), 1, []byte(`{"version":3,"musicId":2}`), 0); err == nil {
		t.Fatal("a detail of another song was exported")
	}
	if _, err := s.ExportLyricsDocument(context.Background(), 1, []byte(`{"version":9,"musicId":1}`), 0); !errors.Is(err, ErrLyricsDocumentExportUnsupported) {
		t.Fatalf("unknown version err=%v", err)
	}
}

// A wiki title may contain '?', which the revision URL carries as %3F.
func TestParseLyricsDocumentSourceURLAcceptsQuestionMarkTitles(t *testing.T) {
	got, err := parseLyricsDocumentSourceURL("https://www.sekaipedia.org/wiki/Synthetic_ka%3F?oldid=328787", "")
	if err != nil || got.canonicalURL != "https://www.sekaipedia.org/wiki/Synthetic_ka%3F?oldid=328787" || got.title != "Synthetic ka?" {
		t.Fatalf("parsed=%+v err=%v", got, err)
	}
	if _, err := parseLyricsDocumentSourceURL("https://www.sekaipedia.org/wiki/Synthetic%23Lyrics?oldid=1", ""); err == nil {
		t.Fatal("a section anchor in the page name was accepted")
	}
}

func lyricsDocumentExportWarningMessage(warnings []LyricsDocumentExportWarning, code string) string {
	for _, warning := range warnings {
		if warning.Code == code {
			return warning.Message
		}
	}
	return ""
}

// A legacy draft whose lyrics are not what the site serves is reported
// whatever its revision, and a draft with the served lyrics is not.
func TestExportLyricsDocumentReportsADraftThatDiffersFromTheServedSongWhateverItsRevision(t *testing.T) {
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
	// The site serves the same lyrics from the bundle at revision 7.
	var detail map[string]any
	if err := json.Unmarshal(servedLegacyLyricsDetail(t, s, 10), &detail); err != nil {
		t.Fatal(err)
	}
	detail["revision"] = 7
	served, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	export, err := s.ExportLyricsDocument(context.Background(), 10, served, 7)
	if err != nil {
		t.Fatal(err)
	}
	if sliceHasString(lyricsDocumentExportWarningCodes(export.Warnings), LyricsDocumentExportWarningUnpublishedDraft) {
		t.Fatalf("a draft with the served lyrics is reported: %v", export.Warnings)
	}
	// An older draft with other lyrics is replaced by a PUT.
	input.Revision = saved.Revision
	input.Lines[0].Chinese = "改过的草稿"
	draft, _, err := s.SaveLyricsMutation(input, "agent")
	if err != nil || draft.Revision >= 7 {
		t.Fatalf("draft=%d err=%v", draft.Revision, err)
	}
	export, err = s.ExportLyricsDocument(context.Background(), 10, served, 7)
	if err != nil {
		t.Fatal(err)
	}
	if message := lyricsDocumentExportWarningMessage(export.Warnings, LyricsDocumentExportWarningUnpublishedDraft); !strings.Contains(message, fmt.Sprintf("revision %d", draft.Revision)) ||
		!strings.Contains(message, "from=database") || *export.Document.ExpectedRevision != 7 {
		t.Fatalf("expectedRevision=%d warnings=%+v", *export.Document.ExpectedRevision, export.Warnings)
	}
}

func TestExportLyricsDocumentFromDatabaseReadsTheEditableState(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	ctx := context.Background()
	// Nothing stored: the error names the revision a creating PUT sends.
	for _, bundleRevision := range []int{0, 5} {
		_, err := s.ExportLyricsDocumentFromDatabase(ctx, lyricsDocumentTestMusicID, LyricsDocumentServed{BundleRevision: bundleRevision})
		var documentErr *LyricsDocumentError
		if !errors.As(err, &documentErr) || documentErr.Code != LyricsDocumentErrorNotFound ||
			!reflect.DeepEqual(documentErr.Current, map[string]int{"revision": bundleRevision}) ||
			!strings.Contains(documentErr.Details[0], fmt.Sprintf("expectedRevision %d", bundleRevision)) {
			t.Fatalf("bundle %d: empty song err=%v", bundleRevision, err)
		}
	}

	request := lyricsDocumentExportTestRequest()
	result, _ := publishLyricsDocumentForTest(t, s, request, 0)
	// A console translation edit at a later revision.
	current, err := s.GetLyricsRenditionDocument(lyricsDocumentTestMusicID)
	if err != nil {
		t.Fatal(err)
	}
	edited := cloneLyricsRenditionEditorDocument(t, current)
	edited.Renditions[1].Full.Lines[1].Chinese = "控制台改过的译文"
	saved, changed, err := s.SaveLyricsRenditionMutation(edited, "console-editor")
	if err != nil || !changed {
		t.Fatalf("console edit changed=%t err=%v", changed, err)
	}
	// The site still serves the earlier revision.
	servedExport, err := s.ExportLyricsDocument(ctx, lyricsDocumentTestMusicID, result.Document, 0)
	if err != nil {
		t.Fatal(err)
	}
	message := lyricsDocumentExportWarningMessage(servedExport.Warnings, LyricsDocumentExportWarningServedRevision)
	if !strings.Contains(message, "has not rebuilt yet") || !strings.Contains(message, "no translation or proofreading credit") ||
		servedExport.From != LyricsDocumentExportFromServed {
		t.Fatalf("served export from=%s warnings=%+v", servedExport.From, servedExport.Warnings)
	}
	fromDatabase, err := s.ExportLyricsDocumentFromDatabase(ctx, lyricsDocumentTestMusicID, LyricsDocumentServed{Detail: result.Document})
	if err != nil {
		t.Fatal(err)
	}
	want := servedExport.Document
	want.Renditions = append([]LyricsDocumentRendition(nil), want.Renditions...)
	want.Renditions[1].Lines = append([]LyricsDocumentLine(nil), want.Renditions[1].Lines...)
	want.Renditions[1].Lines[1].Chinese = "控制台改过的译文"
	if fromDatabase.From != LyricsDocumentExportFromDatabase || fromDatabase.ServedRevision != result.Revision ||
		*fromDatabase.Document.ExpectedRevision != saved.Revision || len(fromDatabase.Warnings) != 0 ||
		!reflect.DeepEqual(fromDatabase.Document.Renditions, want.Renditions) {
		t.Fatalf("database export=%+v warnings=%v\nwant=%+v", fromDatabase.Document, fromDatabase.Warnings, want)
	}

	// A withdrawn song still exports, and a PUT of it publishes it again.
	if _, _, err := s.SetSourceV3LyricsWithdrawn(lyricsDocumentTestMusicID, saved.Revision, true, "document-admin"); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := s.ExportLyricsDocumentFromDatabase(ctx, lyricsDocumentTestMusicID, LyricsDocumentServed{})
	if err != nil || !reflect.DeepEqual(lyricsDocumentExportWarningCodes(withdrawn.Warnings), []string{LyricsDocumentExportWarningWithdrawn}) ||
		!reflect.DeepEqual(withdrawn.Document.Renditions, want.Renditions) || withdrawn.ServedVersion != 0 {
		t.Fatalf("withdrawn export=%+v err=%v", withdrawn, err)
	}
	republished, _ := publishLyricsDocumentForTest(t, s, withdrawn.Document, 0)
	if count := lyricsDocumentCount(t, s, `SELECT COUNT(*) FROM song_lyrics_public_withdrawals WHERE music_id=?`, lyricsDocumentTestMusicID); count != 0 ||
		republished.Changes.Against != LyricsDocumentChangesAgainstDatabase || republished.Changes.Changed {
		t.Fatalf("withdrawals=%d changes=%+v", count, republished.Changes)
	}
}

func TestExportLyricsDocumentFromDatabaseReadsALegacyDraft(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourceURL = "https://www.sekaipedia.org/wiki/Test_Song?oldid=77"
	input.SourcePageID, input.SourceRevisionID = 10, 77
	input.SourceSHA1, input.SourceFetchedAt = validSourceSHA1, "2026-07-22T12:00:00Z"
	input.Lines = []model.LyricLine{
		{ID: "line-1", Order: 0, Japanese: "歌う", Chinese: "歌唱", Segments: []model.LyricSegment{{Text: "歌う", PerformerIDs: []int{1},
			Ruby: []model.LyricRubySpan{{Text: "歌", Reading: "うた"}, {Text: "う"}}}}},
		{ID: "line-2", Order: 1, Japanese: "ららら", Chinese: "啦啦啦", StanzaBreakBefore: true,
			Segments: []model.LyricSegment{{Text: "らら", PerformerIDs: []int{1, 2}}, {Text: "ら", PerformerIDs: []int{1, 2}}}},
	}
	saved, _, err := s.SaveLyricsMutation(input, "agent")
	if err != nil {
		t.Fatal(err)
	}
	export, err := s.ExportLyricsDocumentFromDatabase(context.Background(), 10, LyricsDocumentServed{})
	if err != nil {
		t.Fatal(err)
	}
	lines := export.Document.Renditions[0].Lines
	if export.From != LyricsDocumentExportFromDatabase || *export.Document.ExpectedRevision != saved.Revision || len(lines) != 2 ||
		lines[0].Japanese != "{歌|うた}う" || lines[0].Chinese != "歌唱" || len(lines[1].Segments) != 2 || !lines[1].StanzaBreakBefore {
		t.Fatalf("legacy draft export=%+v", export.Document)
	}
	result, _ := publishLyricsDocumentForTest(t, s, export.Document, 0)
	if result.Changes.Against != LyricsDocumentChangesAgainstDatabase || result.Changes.Changed {
		t.Fatalf("publishing the exported draft changes=%+v", result.Changes)
	}
}

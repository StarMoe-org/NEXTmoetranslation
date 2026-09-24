package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// lyricsDocumentDryRunChanges dry-runs request against the served detail and
// returns the change summary.
func lyricsDocumentDryRunChanges(t *testing.T, s *Store, request LyricsDocumentRequest, served []byte) LyricsDocumentChanges {
	t.Helper()
	request.DryRun = true
	result, err := s.PublishLyricsDocumentServed(context.Background(), request, LyricsDocumentServed{Detail: served}, "document-admin")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	return result.Changes
}

func TestLyricsDocumentChangesAreEmptyForAnUnchangedExport(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	result, _ := publishLyricsDocumentForTest(t, s, lyricsDocumentExportTestRequest(), 0)
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, result.Document, 0)
	if err != nil {
		t.Fatal(err)
	}
	if changes := lyricsDocumentDryRunChanges(t, s, export.Document, result.Document); !reflect.DeepEqual(changes, LyricsDocumentChanges{Against: LyricsDocumentChangesAgainstServed}) {
		t.Fatalf("unchanged export changes=%+v", changes)
	}
	// Without the served detail the database state is the baseline.
	request := export.Document
	request.DryRun = true
	dry, err := s.PublishLyricsDocument(context.Background(), request, 0, "document-admin")
	if err != nil || !reflect.DeepEqual(dry.Changes, LyricsDocumentChanges{Against: LyricsDocumentChangesAgainstDatabase}) {
		t.Fatalf("database baseline changes=%+v err=%v", dry.Changes, err)
	}
}

func TestLyricsDocumentChangesNameEveryChangedFieldWithBeforeAndAfter(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	empty := lyricsDocumentExportTestRequest()
	empty.DryRun = true
	first, err := s.PublishLyricsDocument(context.Background(), empty, 0, "document-admin")
	if err != nil || first.Changes.Against != LyricsDocumentChangesAgainstNothing || !first.Changes.Changed ||
		len(first.Changes.RenditionsAdded) != 2 || first.Changes.Source == nil || first.Changes.Source.Before != nil {
		t.Fatalf("first publish changes=%+v err=%v", first.Changes, err)
	}
	result, _ := publishLyricsDocumentForTest(t, s, lyricsDocumentExportTestRequest(), 0)
	export, err := s.ExportLyricsDocument(context.Background(), lyricsDocumentTestMusicID, result.Document, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := export.Document
	request.Source.URL = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90%E8%A9%A6%E9%A8%93%E6%9B%B2?oldid=4243"
	request.TranslationCredit = "新译者"
	sekai := &request.Renditions[0]
	sekai.Lines = append([]LyricsDocumentLine(nil), sekai.Lines...)
	sekai.Lines[0].Japanese = "{試|し}{験|けん}の{歌|うた}"
	sekai.Lines[1].Chinese = "啦啦啦合成了"
	sekai.Lines[1].Segments = []LyricsDocumentSegment{{Japanese: "らららと", PerformerIDs: []int{1}}, {Japanese: "{合成|ごうせい}", PerformerIDs: []int{2}}}
	sekai.Lines[1].PerformerIDs = nil
	sekai.Lines[2].StanzaBreakBefore, sekai.Lines[2].InGame = true, false
	sekai.Lines = append(sekai.Lines, LyricsDocumentLine{Japanese: "{新|あら}たな{歌|うた}", Chinese: "新的歌"})
	vocaloid := &request.Renditions[1]
	vocaloid.Game, vocaloid.GameLines = "none", nil

	changes := lyricsDocumentDryRunChanges(t, s, request, result.Document)
	if changes.Against != LyricsDocumentChangesAgainstServed || !changes.Changed || changes.Truncated ||
		len(changes.RenditionsAdded) != 0 || len(changes.RenditionsRemoved) != 0 || len(changes.Renditions) != 2 {
		t.Fatalf("changes=%+v", changes)
	}
	if changes.Source == nil || changes.Source.Before == nil || changes.Source.Before.URL != export.Document.Source.URL ||
		changes.Source.After.URL != request.Source.URL {
		t.Fatalf("source change=%+v", changes.Source)
	}
	sekaiChange, vocaloidChange := changes.Renditions[0], changes.Renditions[1]
	if sekaiChange.Key != "sekai" || sekaiChange.TranslationCredits == nil ||
		sekaiChange.TranslationCredits.Before.Translation != "合成译者" || sekaiChange.TranslationCredits.After.Translation != "新译者" ||
		len(sekaiChange.Sides) != 1 || sekaiChange.Game != nil {
		t.Fatalf("sekai change=%+v", sekaiChange)
	}
	full := sekaiChange.Sides[0]
	if full.Side != "full" || full.LinesBefore != 3 || full.LinesAfter != 4 || full.AddedCount != 1 || full.RemovedCount != 0 ||
		full.ChangedCount != 3 || !reflect.DeepEqual(full.Added, []LyricsDocumentLineSummary{
		{Line: 3, Japanese: "{新|あら}たな{歌|うた}", Chinese: "新的歌", PerformerIDs: []int{1, 2}, InGame: new(bool)},
	}) {
		t.Fatalf("full side=%+v", full)
	}
	want := []LyricsDocumentLineChange{
		{Before: 0, After: 0, Fields: []string{"ruby"}, Japanese: &LyricsDocumentTextChange{Before: "{試験|しけん}の{歌|うた}", After: "{試|し}{験|けん}の{歌|うた}"}},
		{Before: 1, After: 1, Fields: []string{"segments", "zh"}, Chinese: &LyricsDocumentTextChange{Before: "啦啦啦地合成", After: "啦啦啦合成了"},
			Segments: &LyricsDocumentSegmentsChange{
				Before: []LyricsDocumentSegment{{Japanese: "らららと{合成|ごうせい}", PerformerIDs: []int{1}}},
				After:  []LyricsDocumentSegment{{Japanese: "らららと", PerformerIDs: []int{1}}, {Japanese: "{合成|ごうせい}", PerformerIDs: []int{2}}},
			}},
		{Before: 2, After: 2, Fields: []string{"stanzaBreakBefore", "inGame"}},
	}
	if !reflect.DeepEqual(full.Changed, want) {
		t.Fatalf("changed lines=%+v\nwant=%+v", full.Changed, want)
	}
	if vocaloidChange.Key != "vocaloid" || vocaloidChange.Game == nil || *vocaloidChange.Game != (LyricsDocumentTextChange{Before: "independent", After: "none"}) ||
		len(vocaloidChange.Sides) != 1 || vocaloidChange.Sides[0].Side != "game" || vocaloidChange.Sides[0].RemovedCount != 1 {
		t.Fatalf("vocaloid change=%+v", vocaloidChange)
	}
}

func TestLyricsDocumentChangesListABoundedNumberOfLines(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	request.Renditions[0].Lines = nil
	for index := range maxLyricsDocumentChangeEntries + 10 {
		request.Renditions[0].Lines = append(request.Renditions[0].Lines,
			LyricsDocumentLine{Japanese: fmt.Sprintf("らら%d", index), Chinese: fmt.Sprintf("啦啦%d", index)})
	}
	result, _ := publishLyricsDocumentForTest(t, s, request, 0)
	request.ExpectedRevision = &result.Revision
	for index := range request.Renditions[0].Lines {
		request.Renditions[0].Lines[index].Chinese += "改"
	}
	changes := lyricsDocumentDryRunChanges(t, s, request, result.Document)
	if len(changes.Renditions) != 1 || len(changes.Renditions[0].Sides) != 1 {
		t.Fatalf("changes=%+v", changes)
	}
	side := changes.Renditions[0].Sides[0]
	if !changes.Truncated || side.ChangedCount != maxLyricsDocumentChangeEntries+10 || len(side.Changed) != maxLyricsDocumentChangeEntries {
		t.Fatalf("truncated=%t changed=%d listed=%d", changes.Truncated, side.ChangedCount, len(side.Changed))
	}
}

// Moving the stanza break between two identical adjacent lines aligns as one
// removed and one added line; their summaries show which line carries the
// break, and an added segmented line shows its segments.
func TestLyricsDocumentChangeSummariesShowTheStructureOfAddedAndRemovedLines(t *testing.T) {
	s := setupLyricsDocumentStore(t)
	request := lyricsDocumentTestRequest()
	repeated := LyricsDocumentLine{Japanese: "{繰|く}り{返|かえ}す", Chinese: "重复"}
	withBreak := repeated
	withBreak.StanzaBreakBefore = true
	opening, closing := LyricsDocumentLine{Japanese: "はじまりのうた", Chinese: "开始之歌"}, LyricsDocumentLine{Japanese: "おわりのうた", Chinese: "结束之歌"}
	request.Renditions[0].Lines = []LyricsDocumentLine{opening, withBreak, repeated, closing}
	result, _ := publishLyricsDocumentForTest(t, s, request, 0)
	request.ExpectedRevision = &result.Revision
	duet := LyricsDocumentLine{Japanese: "ふたりで{歌|うた}う", Chinese: "两人合唱", Segments: []LyricsDocumentSegment{
		{Japanese: "ふたりで", PerformerIDs: []int{1}}, {Japanese: "{歌|うた}う", PerformerIDs: []int{2}},
	}}
	request.Renditions[0].Lines = []LyricsDocumentLine{opening, repeated, withBreak, closing, duet}

	changes := lyricsDocumentDryRunChanges(t, s, request, result.Document)
	if len(changes.Renditions) != 1 || len(changes.Renditions[0].Sides) != 1 {
		t.Fatalf("changes=%+v", changes)
	}
	full := changes.Renditions[0].Sides[0]
	moved := func(line int) LyricsDocumentLineSummary {
		return LyricsDocumentLineSummary{Line: line, Japanese: "{繰|く}り{返|かえ}す", Chinese: "重复", StanzaBreakBefore: true, PerformerIDs: []int{1, 2}}
	}
	wantAdded := []LyricsDocumentLineSummary{moved(2), {
		Line: 4, Japanese: "ふたりで{歌|うた}う", Chinese: "两人合唱", PerformerIDs: []int{1, 2}, Segments: duet.Segments,
	}}
	if full.ChangedCount != 0 || !reflect.DeepEqual(full.Removed, []LyricsDocumentLineSummary{moved(1)}) || !reflect.DeepEqual(full.Added, wantAdded) {
		t.Fatalf("removed=%+v\nadded=%+v", full.Removed, full.Added)
	}
}

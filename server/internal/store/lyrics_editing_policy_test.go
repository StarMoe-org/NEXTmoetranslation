package store

import (
	"errors"
	"reflect"
	"testing"

	"moesekai/server/internal/model"
)

func TestOrdinaryFirstSaveAcceptsCompleteManagedProvenance(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourceURL = "https://www.sekaipedia.org/wiki/Test_Song?oldid=123"
	input.SourcePageID = 456
	input.SourceRevisionID = 123
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-08-16T00:00:00Z"
	saved, changed, err := s.SaveLyricsMutation(input, "agent")
	if err != nil || !changed || saved.Revision != 1 {
		t.Fatalf("ordinary provenance first save=%+v changed=%v err=%v", saved, changed, err)
	}
	loaded, err := s.GetLyrics(input.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SourceURL != input.SourceURL || loaded.SourcePageID != 456 || loaded.SourceRevisionID != 123 ||
		loaded.SourceSHA1 != validSourceSHA1 || loaded.SourceFetchedAt != input.SourceFetchedAt {
		t.Fatalf("ordinary provenance persisted=%+v", loaded)
	}

	mismatched := validLyrics()
	mismatched.MusicID = 20
	mismatched.SourceURL = "https://www.sekaipedia.org/wiki/Test_Song?oldid=124"
	mismatched.SourcePageID = 456
	mismatched.SourceRevisionID = 123
	mismatched.SourceSHA1 = validSourceSHA1
	mismatched.SourceFetchedAt = "2026-08-16T00:00:00Z"
	_, _, err = s.SaveLyricsMutation(mismatched, "agent")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("managed oldid mismatch error=%#v", err)
	}
}

func TestPublishedLegacyLyricsAcceptProvenanceAndJapaneseEdits(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourceURL = "https://vocaloid.fandom.com/wiki/Test_Song?oldid=20"
	input.SourcePageID = 10
	input.SourceRevisionID = 20
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:00:00Z"
	saved, _, err := s.SaveImportedLyricsMutation(input, "importer")
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.PublishLyrics(saved.MusicID, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}

	edited := published
	edited.SourceURL = "https://vocaloid.fandom.com/wiki/Test_Song?oldid=21"
	edited.SourceRevisionID = 21
	edited.SourceSHA1 = "1123456789abcdef0123456789abcdef01234567"
	edited.SourceFetchedAt = "2026-07-23T12:00:00Z"
	edited.Lines = []model.LyricLine{
		{ID: "line-a", Order: 0, Japanese: "初音が歌う", Chinese: "初音在唱", Segments: []model.LyricSegment{
			{Text: "初音", PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: "初音", Reading: "はつね"}}},
			{Text: "が歌う", PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: "が"}, {Text: "歌", Reading: "うた"}, {Text: "う"}}},
		}},
		{ID: "line-b", Order: 1, Japanese: "続く", StanzaBreakBefore: true, Segments: []model.LyricSegment{
			{Text: "続く", PerformerIDs: []int{2}, Ruby: []model.LyricRubySpan{{Text: "続", Reading: "つづ"}, {Text: "く"}}},
		}},
	}
	updated, changed, err := s.SaveLyricsMutation(edited, "agent")
	if err != nil || !changed || updated.Revision != saved.Revision+1 || updated.Status != "draft-published" ||
		updated.PublishedRevision != saved.Revision {
		t.Fatalf("post-publication edit=%+v changed=%v err=%v", updated, changed, err)
	}
	reloaded, err := s.GetLyrics(saved.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.SourceRevisionID != 21 || reloaded.SourceURL != edited.SourceURL || len(reloaded.Lines) != 2 ||
		reloaded.Lines[0].Japanese != "初音が歌う" || !reloaded.Lines[1].StanzaBreakBefore ||
		!reflect.DeepEqual(reloaded.Lines[0].Segments[1].Ruby, edited.Lines[0].Segments[1].Ruby) {
		t.Fatalf("post-publication edit reloaded=%+v", reloaded)
	}

	stale := reloaded
	stale.Revision = saved.Revision
	_, _, err = s.SaveLyricsMutation(stale, "agent")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "revision_conflict" || contractErr.Current.Revision != updated.Revision {
		t.Fatalf("stale revision error=%#v", err)
	}

	// A structural change must carry ruby; stored spans cannot be inherited.
	restructured := reloaded
	restructured.Lines = []model.LyricLine{{ID: "line-c", Order: 0, Japanese: "新しい", Segments: []model.LyricSegment{
		{Text: "新しい", PerformerIDs: []int{1}},
	}}}
	_, _, err = s.SaveLyricsMutation(restructured, "agent")
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" {
		t.Fatalf("restructure without ruby error=%#v", err)
	}

	_, _, err = s.SaveImportedLyricsMutation(reloaded, "importer")
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("verified import over an existing document error=%#v", err)
	}
}

func TestLyricsRenditionEditorPersistsIndependentGameLayout(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	renditionIndex := -1
	for index, rendition := range requested.Renditions {
		if rendition.Full != nil && rendition.Game != nil && rendition.Relation.Kind == model.LyricsSourceRenditionRelationNone &&
			len(rendition.Game.Lines) > 1 {
			renditionIndex = index
			break
		}
	}
	if renditionIndex < 0 {
		t.Fatal("fixture has no Full + independent Game rendition")
	}
	gameLine := &requested.Renditions[renditionIndex].Game.Lines[1]
	wantBreak := !gameLine.StanzaBreakBefore
	gameLine.StanzaBreakBefore = wantBreak
	fullBreak := requested.Renditions[renditionIndex].Full.Lines[1].StanzaBreakBefore

	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "layout-editor")
	if err != nil || !changed || saved.Revision != current.Revision+1 {
		t.Fatalf("Game layout save revision=%d changed=%v err=%v", saved.Revision, changed, err)
	}
	reloaded, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	rendition := reloaded.Renditions[renditionIndex]
	if rendition.Game.Lines[1].StanzaBreakBefore != wantBreak || rendition.Full.Lines[1].StanzaBreakBefore != fullBreak {
		t.Fatalf("reloaded Game stanza=%v want=%v Full stanza=%v want=%v",
			rendition.Game.Lines[1].StanzaBreakBefore, wantBreak, rendition.Full.Lines[1].StanzaBreakBefore, fullBreak)
	}
}

func TestLyricsRenditionLayoutOnlySaveWithoutTranslationRequiresTranslation(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	if _, err := s.db.Exec(`DELETE FROM song_lyrics_rendition_localizations`); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	line := &requested.Renditions[0].Full.Lines[1]
	line.StanzaBreakBefore = !line.StanzaBreakBefore

	_, changed, err := s.SaveLyricsRenditionMutation(requested, "layout-editor")
	var contractErr *LyricsRenditionContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "translation_required" || len(contractErr.Details) == 0 || changed {
		t.Fatalf("layout-only save without translation changed=%v err=%#v", changed, err)
	}
	reloaded, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision != current.Revision ||
		reloaded.Renditions[0].Full.Lines[1].StanzaBreakBefore != current.Renditions[0].Full.Lines[1].StanzaBreakBefore {
		t.Fatalf("rejected layout save mutated the document: %+v", reloaded.Renditions[0].Full.Lines[1])
	}
}

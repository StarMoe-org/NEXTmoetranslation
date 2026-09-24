package collab

import (
	"errors"
	"reflect"
	"testing"

	"github.com/reearth/ygo/crdt"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

func TestDocumentUpdateUsesNestedYTypesAndRoundTrips(t *testing.T) {
	document := model.SongLyrics{
		MusicID: 7, Status: "draft", Revision: 3, UpdatedAt: "2026-08-14T00:00:00Z",
		Attribution: "source", TranslationCredit: "translator", ProofreadingCredit: "proofreader",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "歌", Chinese: "歌詞", English: "song",
			Segments: []model.LyricSegment{{Text: "歌", PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: "歌", Reading: "うた"}}}},
		}},
	}
	update, err := documentUpdate(document)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatal(err)
	}
	root := doc.GetMap("lyrics")
	if value, ok := root.Get("attribution"); !ok {
		t.Fatal("missing attribution")
	} else if _, ok := value.(*crdt.YText); !ok {
		t.Fatalf("attribution type=%T want *crdt.YText", value)
	}
	linesValue, ok := root.Get("lines")
	if !ok {
		t.Fatal("missing lines")
	}
	lines, ok := linesValue.(*crdt.YArray)
	if !ok {
		t.Fatalf("lines type=%T want *crdt.YArray", linesValue)
	}
	line, ok := lines.Get(0).(*crdt.YMap)
	if !ok {
		t.Fatalf("line type=%T want *crdt.YMap", lines.Get(0))
	}
	if japanese, ok := line.Get("japanese"); !ok {
		t.Fatal("missing japanese")
	} else if _, ok := japanese.(*crdt.YText); !ok {
		t.Fatalf("japanese type=%T want *crdt.YText", japanese)
	}
	segments := testMapArray(t, line, "segments")
	segment, ok := segments.Get(0).(*crdt.YMap)
	if !ok {
		t.Fatalf("segment type=%T want *crdt.YMap", segments.Get(0))
	}
	if id, ok := segment.Get(structuredItemIDKey); !ok || id != "segments:lyrics.lines[0].segments[0]" {
		t.Fatalf("segment internal id=%v present=%v", id, ok)
	}
	if generation, ok := segment.Get(structuredItemGenerationKey); !ok || generation != "seed:segments:lyrics.lines[0].segments[0]" {
		t.Fatalf("segment generation=%v present=%v", generation, ok)
	}
	ruby := testMapArray(t, segment, "ruby")
	span, ok := ruby.Get(0).(*crdt.YMap)
	if !ok {
		t.Fatalf("ruby span type=%T want *crdt.YMap", ruby.Get(0))
	}
	if id, ok := span.Get(structuredItemIDKey); !ok || id != "ruby:lyrics.lines[0].segments[0].ruby[0]" {
		t.Fatalf("ruby internal id=%v present=%v", id, ok)
	}
	if generation, ok := span.Get(structuredItemGenerationKey); !ok || generation != "seed:ruby:lyrics.lines[0].segments[0].ruby[0]" {
		t.Fatalf("ruby generation=%v present=%v", generation, ok)
	}
	roundTrip, err := materializeDocument(root, documentLegacy, document.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, document) {
		t.Fatalf("round trip=%#v want=%#v", roundTrip, document)
	}
}

func TestMaterializeRejectsConcurrentSegmentStructure(t *testing.T) {
	document, root := seededDocumentForStructuralConflict(t)
	segments := firstTestSegmentArray(t, root)
	first := segments.Get(0).(*crdt.YMap)
	document.Transact(func(txn *crdt.Transaction) {
		origin := "[\"segments:lyrics.lines[0].segments[0]\",\"seed:segments:lyrics.lines[0].segments[0]\"]"
		first.Set(txn, structuredItemOriginKey, origin)
		first.Set(txn, structuredItemGenerationKey, "edit:segments:1:1")
		segments.PushType(txn, testSegment(txn, "segment-branch-b", "edit:segments:2:1", origin))
	})
	if _, err := materializeDocument(root, documentLegacy, 42); !errors.Is(err, ErrDocumentMismatch) {
		t.Fatalf("materialize concurrent segment split err=%v want ErrDocumentMismatch", err)
	}
}

func TestMaterializeRejectsConcurrentRubyStructure(t *testing.T) {
	document, root := seededDocumentForStructuralConflict(t)
	segment := firstTestSegmentArray(t, root).Get(0).(*crdt.YMap)
	ruby := testMapArray(t, segment, "ruby")
	first := ruby.Get(0).(*crdt.YMap)
	document.Transact(func(txn *crdt.Transaction) {
		origin := "[\"ruby:lyrics.lines[0].segments[0].ruby[0]\",\"seed:ruby:lyrics.lines[0].segments[0].ruby[0]\"]"
		first.Set(txn, structuredItemOriginKey, origin)
		first.Set(txn, structuredItemGenerationKey, "edit:ruby:1:1")
		span := crdt.NewMapPrelim()
		span.Set(txn, structuredItemIDKey, "ruby-branch-b")
		span.Set(txn, structuredItemGenerationKey, "edit:ruby:2:1")
		span.Set(txn, structuredItemOriginKey, origin)
		span.Set(txn, "text", newText(txn, "B"))
		span.Set(txn, "reading", newText(txn, ""))
		ruby.PushType(txn, span)
	})
	if _, err := materializeDocument(root, documentLegacy, 42); !errors.Is(err, ErrDocumentMismatch) {
		t.Fatalf("materialize concurrent ruby split err=%v want ErrDocumentMismatch", err)
	}
}

func TestMaterializeAllowsLegacyStructuredArraysWithoutInternalMetadata(t *testing.T) {
	document, root := seededDocumentForStructuralConflict(t)
	segments := firstTestSegmentArray(t, root)
	segment := segments.Get(0).(*crdt.YMap)
	ruby := testMapArray(t, segment, "ruby")
	span := ruby.Get(0).(*crdt.YMap)
	document.Transact(func(txn *crdt.Transaction) {
		segment.Delete(txn, structuredItemIDKey)
		segment.Delete(txn, structuredItemGenerationKey)
		span.Delete(txn, structuredItemIDKey)
		span.Delete(txn, structuredItemGenerationKey)
	})
	materialized, err := materializeDocument(root, documentLegacy, 42)
	if err != nil {
		t.Fatal(err)
	}
	lyrics := materialized.(model.SongLyrics)
	if len(lyrics.Lines) != 1 || len(lyrics.Lines[0].Segments) != 1 || len(lyrics.Lines[0].Segments[0].Ruby) != 1 {
		t.Fatalf("legacy materialized document=%#v", lyrics)
	}
}

func TestMaterializeRejectsMixedStructuredMetadata(t *testing.T) {
	document, root := seededDocumentForStructuralConflict(t)
	segments := firstTestSegmentArray(t, root)
	document.Transact(func(txn *crdt.Transaction) {
		legacy := testSegment(txn, "legacy-segment", "seed:legacy-segment", "")
		legacy.Delete(txn, structuredItemIDKey)
		legacy.Delete(txn, structuredItemGenerationKey)
		legacy.Delete(txn, structuredItemOriginKey)
		segments.PushType(txn, legacy)
	})
	if _, err := materializeDocument(root, documentLegacy, 42); !errors.Is(err, ErrDocumentMismatch) {
		t.Fatalf("materialize mixed metadata err=%v want ErrDocumentMismatch", err)
	}
}

func seededDocumentForStructuralConflict(t *testing.T) (*crdt.Doc, *crdt.YMap) {
	t.Helper()
	update, err := documentUpdate(model.SongLyrics{
		MusicID: 42, Status: "draft", Revision: 0,
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "AB",
			Segments: []model.LyricSegment{{
				Text: "AB", PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: "AB"}},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	document := crdt.New()
	if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
		t.Fatal(err)
	}
	return document, document.GetMap("lyrics")
}

func firstTestSegmentArray(t *testing.T, root *crdt.YMap) *crdt.YArray {
	t.Helper()
	lines := testMapArray(t, root, "lines")
	line, ok := lines.Get(0).(*crdt.YMap)
	if !ok {
		t.Fatalf("line type=%T", lines.Get(0))
	}
	return testMapArray(t, line, "segments")
}

func testMapArray(t *testing.T, target *crdt.YMap, key string) *crdt.YArray {
	t.Helper()
	value, ok := target.Get(key)
	if !ok {
		t.Fatalf("missing %s", key)
	}
	array, ok := value.(*crdt.YArray)
	if !ok {
		t.Fatalf("%s type=%T want *crdt.YArray", key, value)
	}
	return array
}

func testSegment(txn *crdt.Transaction, id, generation, origin string) *crdt.YMap {
	segment := crdt.NewMapPrelim()
	segment.Set(txn, structuredItemIDKey, id)
	segment.Set(txn, structuredItemGenerationKey, generation)
	if origin != "" {
		segment.Set(txn, structuredItemOriginKey, origin)
	}
	segment.Set(txn, "text", newText(txn, "B"))
	performers := crdt.NewArrayPrelim()
	performers.Push(txn, []any{1})
	segment.Set(txn, "performerIds", performers)
	ruby := crdt.NewArrayPrelim()
	span := crdt.NewMapPrelim()
	span.Set(txn, structuredItemIDKey, "ruby:"+id)
	span.Set(txn, structuredItemGenerationKey, "seed:ruby:"+id)
	span.Set(txn, "text", newText(txn, "B"))
	span.Set(txn, "reading", newText(txn, ""))
	ruby.PushType(txn, span)
	segment.Set(txn, "ruby", ruby)
	return segment
}

func TestValidateImmutableDraftAllowsSavedLegacySourceEdits(t *testing.T) {
	current := model.SongLyrics{
		MusicID: 9, Status: "draft", Revision: 1,
		SourceURL: "https://example.com/source", Lines: []model.LyricLine{{ID: "a", Order: 0, Japanese: "A", Segments: []model.LyricSegment{}}},
	}
	draft := current
	draft.SourceURL = "https://example.com/other"
	draft.SourceRevisionID = 7
	draft.Lines = []model.LyricLine{
		{ID: "b", Order: 0, Japanese: "B", Segments: []model.LyricSegment{}},
		{ID: "a", Order: 1, Japanese: "A2", Segments: []model.LyricSegment{}},
	}
	if err := validateImmutableDraft(current, draft); err != nil {
		t.Fatalf("saved legacy provenance, Japanese and line structure edit rejected: %v", err)
	}

	var conflict *CheckpointConflict
	otherSong := draft
	otherSong.MusicID = 10
	if err := validateImmutableDraft(current, otherSong); !errors.As(err, &conflict) || conflict.Code != "source_drift" {
		t.Fatalf("musicId change error=%v", err)
	}
	if err := validateImmutableDraft(current, store.LyricsRenditionDocument{MusicID: 9}); !errors.As(err, &conflict) || conflict.Code != "source_drift" {
		t.Fatalf("document kind change error=%v", err)
	}
}

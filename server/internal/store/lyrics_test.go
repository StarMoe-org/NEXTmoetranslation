package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
)

func TestSaveLyricsMutationBeforeCommitSharesAtomicTransaction(t *testing.T) {
	t.Run("callback failure rolls back authority and callback writes", func(t *testing.T) {
		s := setupLyricsStore(t)
		if _, err := s.db.Exec(`CREATE TABLE atomic_probe (value TEXT NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		sentinel := errors.New("reject shared commit")
		_, _, err := s.SaveLyricsMutationWithBeforeCommit(validLyrics(), "editor", func(tx *sql.Tx, saved model.SongLyrics, changed bool) error {
			if !changed || saved.Revision != 1 || saved.Status != "draft" || saved.UpdatedAt == "" {
				t.Fatalf("callback saved=%+v changed=%t", saved, changed)
			}
			if _, err := tx.Exec(`INSERT INTO atomic_probe(value) VALUES ('collab-ledger')`); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("save error=%v want sentinel", err)
		}
		if _, err := s.GetLyrics(10); !errors.Is(err, ErrLyricsNotFound) {
			t.Fatalf("authoritative lyrics survived rollback: %v", err)
		}
		var probes, audits int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM atomic_probe`).Scan(&probes); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='lyrics.save'`).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if probes != 0 || audits != 0 {
			t.Fatalf("rollback probes=%d audits=%d", probes, audits)
		}
	})

	t.Run("callback success commits authority and callback writes", func(t *testing.T) {
		s := setupLyricsStore(t)
		if _, err := s.db.Exec(`CREATE TABLE atomic_probe (value TEXT NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		saved, changed, err := s.SaveLyricsMutationWithBeforeCommit(validLyrics(), "editor", func(tx *sql.Tx, saved model.SongLyrics, changed bool) error {
			_, err := tx.Exec(`INSERT INTO atomic_probe(value) VALUES (?)`, fmt.Sprintf("revision=%d changed=%t", saved.Revision, changed))
			return err
		})
		if err != nil || !changed || saved.Revision != 1 {
			t.Fatalf("saved=%+v changed=%t err=%v", saved, changed, err)
		}
		var value string
		if err := s.db.QueryRow(`SELECT value FROM atomic_probe`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		loaded, err := s.GetLyrics(10)
		if err != nil || loaded.Revision != 1 || value != "revision=1 changed=true" {
			t.Fatalf("loaded=%+v value=%q err=%v", loaded, value, err)
		}
	})
}

func TestSaveLyricsMutationBeforeCommitRunsForNoOp(t *testing.T) {
	s := setupLyricsStore(t)
	saved, _, err := s.SaveLyricsMutation(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE atomic_probe (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	replayed, changed, err := s.SaveLyricsMutationWithBeforeCommit(saved, "editor", func(tx *sql.Tx, final model.SongLyrics, changed bool) error {
		if changed || final.Revision != saved.Revision {
			t.Fatalf("no-op callback final=%+v changed=%t", final, changed)
		}
		_, err := tx.Exec(`INSERT INTO atomic_probe(value) VALUES ('no-op-checkpoint')`)
		return err
	})
	if err != nil || changed || replayed.Revision != saved.Revision {
		t.Fatalf("replay=%+v changed=%t err=%v", replayed, changed, err)
	}
	var probes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM atomic_probe`).Scan(&probes); err != nil || probes != 1 {
		t.Fatalf("probes=%d err=%v", probes, err)
	}
}

func setupLyricsStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "lyrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := New(database)
	if err := s.UpsertMusicCatalog([]MusicCatalogRecord{
		{MusicID: 10, JapaneseTitle: "新曲", ChineseTitle: "新歌", EnglishTitle: "New Song", IsNewlyWrittenMusic: true},
		{MusicID: 20, JapaneseTitle: "旧曲", IsNewlyWrittenMusic: false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerformerCatalog([]PerformerCatalogRecord{
		{PerformerID: 1, JapaneseName: "初音ミク", ChineseName: "初音未来", EnglishName: "Hatsune Miku"},
		{PerformerID: 2, JapaneseName: "鏡音リン"},
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

const validSourceSHA1 = "0123456789abcdef0123456789abcdef01234567"

func validLyrics() model.SongLyrics {
	return model.SongLyrics{
		MusicID: 10, Revision: 0, Status: "draft",
		Attribution: "Lyrics transcription and translation by the MoeSeka team",
		SourceNote:  "manual transcription", SourceURL: "https://example.invalid/source", LicenseNote: "internal",
		Lines: []model.LyricLine{{
			ID: "line-1", Order: 0, Japanese: "初音歌う", Chinese: "初音歌唱", English: "Miku sings",
			Segments: []model.LyricSegment{
				{Text: "初音", PerformerIDs: []int{1}},
				{Text: "歌う", PerformerIDs: []int{1, 2}},
			},
		}},
	}
}

func TestPrivateLyricsMutationCanonicalizesRequiredEmptyFields(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.Attribution = ""
	for lineIndex := range input.Lines {
		for segmentIndex := range input.Lines[lineIndex].Segments {
			input.Lines[lineIndex].Segments[segmentIndex].PerformerIDs = []int{}
		}
	}
	saved, changed, err := s.SaveLyricsMutation(input, "editor")
	if err != nil || !changed {
		t.Fatalf("save empty private fields: changed=%t err=%v", changed, err)
	}
	body, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"attribution", "translationCredit", "proofreadingCredit"} {
		if string(document[field]) != `""` {
			t.Fatalf("private %s JSON=%s document=%s", field, document[field], body)
		}
	}
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(document["lines"], &lines); err != nil || len(lines) != 1 {
		t.Fatalf("private lines=%s err=%v", document["lines"], err)
	}
	var segments []map[string]json.RawMessage
	if err := json.Unmarshal(lines[0]["segments"], &segments); err != nil || len(segments) != 2 {
		t.Fatalf("private segments=%s err=%v", lines[0]["segments"], err)
	}
	for index, segment := range segments {
		if string(segment["performerIds"]) != `[]` {
			t.Fatalf("segment %d performerIds=%s document=%s", index, segment["performerIds"], body)
		}
	}
}

func TestLyricsCreditsPersistIndependentlyWhenTranslatorAndProofreaderMatch(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.TranslationCredit = "Same Person"
	input.ProofreadingCredit = "Same Person"
	saved, err := s.SaveLyrics(input, "editor")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetLyrics(saved.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TranslationCredit != "Same Person" || loaded.ProofreadingCredit != "Same Person" {
		t.Fatalf("loaded credits translation=%q proofreading=%q", loaded.TranslationCredit, loaded.ProofreadingCredit)
	}
	var translation, proofreading string
	if err := s.db.QueryRow(`SELECT translation_credit,proofreading_credit FROM song_lyrics WHERE music_id=?`, saved.MusicID).
		Scan(&translation, &proofreading); err != nil {
		t.Fatal(err)
	}
	if translation != "Same Person" || proofreading != "Same Person" {
		t.Fatalf("stored credits translation=%q proofreading=%q", translation, proofreading)
	}

	loaded.ProofreadingCredit = "Second Proofreader"
	updated, err := s.SaveLyrics(loaded, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if updated.TranslationCredit != "Same Person" || updated.ProofreadingCredit != "Second Proofreader" {
		t.Fatalf("updated credits translation=%q proofreading=%q", updated.TranslationCredit, updated.ProofreadingCredit)
	}
}

func TestLyricsPublicationAcceptsTranslationOrLegacyAttributionButNotProofreadingAlone(t *testing.T) {
	t.Run("translation credit with empty proofreading", func(t *testing.T) {
		s := setupLyricsStore(t)
		input := validLyrics()
		input.Attribution = ""
		input.TranslationCredit = "Translator"
		input.ProofreadingCredit = ""
		saved, err := s.SaveLyrics(input, "editor")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
			t.Fatalf("publish translation-only credit: %v", err)
		}
		_, details, err := s.PublishedLyrics()
		if err != nil {
			t.Fatal(err)
		}
		if details[saved.MusicID].Attribution != "Translator" {
			t.Fatalf("v1 translation fallback attribution=%q", details[saved.MusicID].Attribution)
		}
	})

	t.Run("legacy attribution", func(t *testing.T) {
		s := setupLyricsStore(t)
		input := validLyrics()
		input.TranslationCredit = ""
		input.ProofreadingCredit = ""
		saved, err := s.SaveLyrics(input, "editor")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
			t.Fatalf("publish legacy attribution: %v", err)
		}
	})

	t.Run("proofreading only", func(t *testing.T) {
		s := setupLyricsStore(t)
		input := validLyrics()
		input.Attribution = ""
		input.TranslationCredit = ""
		input.ProofreadingCredit = "Proofreader"
		saved, err := s.SaveLyrics(input, "editor")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PublishLyrics(saved.MusicID, saved.Revision)
		var contractErr *LyricsContractError
		if !errors.As(err, &contractErr) || contractErr.Code != "incomplete_publication" ||
			!strings.Contains(strings.Join(contractErr.Details, "; "), "translation credit is required") {
			t.Fatalf("proofreading-only publication error=%#v", err)
		}
	})
}

func lyricsWithReadings() model.SongLyrics {
	lyrics := validLyrics()
	lyrics.Lines[0].Segments[0].Ruby = []model.LyricRubySpan{
		{Text: "初", Reading: "はつ"},
		{Text: "音", Reading: "ね"},
	}
	lyrics.Lines[0].Segments[1].Ruby = []model.LyricRubySpan{
		{Text: "歌", Reading: "うた"},
		{Text: "う"},
	}
	return lyrics
}

func TestLyricsCRUDRevisionDriftAndPublication(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Status != "draft" || saved.UpdatedAt == "" {
		t.Fatalf("saved = %+v", saved)
	}

	stale := validLyrics()
	stale.Revision = 0
	_, err = s.SaveLyrics(stale, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "revision_conflict" || contractErr.Current == nil || contractErr.Current.Revision != 1 {
		t.Fatalf("stale save error = %#v", err)
	}

	drift := saved
	drift.Lines[0].Japanese = "初音が歌う"
	drift.Lines[0].Segments[1].Text = "が歌う"
	drift.Lines[0].Segments[1].Ruby = []model.LyricRubySpan{{Text: "が歌う"}}
	_, err = s.SaveLyrics(drift, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("source drift error = %#v", err)
	}
	saved, err = s.GetLyrics(10)
	if err != nil {
		t.Fatal(err)
	}

	published, err := s.PublishLyrics(10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != "published" {
		t.Fatalf("published status = %q", published.Status)
	}
	again, err := s.PublishLyrics(10, 1)
	if err != nil || again.Status != "published" || again.Revision != 1 {
		t.Fatalf("idempotent publish = %+v err=%v", again, err)
	}

	index, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Songs) != 1 || index.Songs[0].Title.English != "New Song" {
		t.Fatalf("public index = %+v", index)
	}
	public := details[10]
	if public.Version != 1 || public.Revision != 1 || public.Attribution != saved.Attribution || len(public.Lines) != 1 {
		t.Fatalf("public detail = %+v", public)
	}

	edited := saved
	edited.Lines[0].English = "Hatsune Miku sings"
	edited, err = s.SaveLyrics(edited, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Revision != 2 || edited.Status != "draft-published" || edited.PublishedRevision != 1 {
		t.Fatalf("edited draft = %+v", edited)
	}
	_, details, err = s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	if details[10].Revision != 1 || details[10].Lines[0].English != "Miku sings" {
		t.Fatalf("draft edit changed published snapshot: %+v", details[10])
	}

	unpublished, err := s.UnpublishLyrics(10, 2)
	if err != nil || unpublished.Status != "draft" {
		t.Fatalf("unpublish = %+v err=%v", unpublished, err)
	}
	unpublished, err = s.UnpublishLyrics(10, 2)
	if err != nil || unpublished.Status != "draft" {
		t.Fatalf("idempotent unpublish = %+v err=%v", unpublished, err)
	}
}

func TestLegacyPrivateLyricsSavePreservesPersistedRuby(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(lyricsWithReadings(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	expectedRuby := make([][]model.LyricRubySpan, len(saved.Lines[0].Segments))
	for segmentIndex := range saved.Lines[0].Segments {
		expectedRuby[segmentIndex] = append([]model.LyricRubySpan(nil), saved.Lines[0].Segments[segmentIndex].Ruby...)
	}

	legacy := saved
	legacy.Lines = append([]model.LyricLine(nil), saved.Lines...)
	legacy.Lines[0].Segments = append([]model.LyricSegment(nil), saved.Lines[0].Segments...)
	legacy.Lines[0].Segments[0].Ruby = nil
	legacy.Lines[0].Segments[1].Ruby = []model.LyricRubySpan{}
	legacy.Lines[0].Chinese = "只修改中文"
	legacy.Lines[0].English = "Only the translation changed"
	legacy.Attribution = "Updated attribution from a legacy private client"

	updated, err := s.SaveLyrics(legacy, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != saved.Revision+1 || updated.Lines[0].Chinese != legacy.Lines[0].Chinese ||
		updated.Lines[0].English != legacy.Lines[0].English || updated.Attribution != legacy.Attribution {
		t.Fatalf("legacy translation save = %+v", updated)
	}
	for segmentIndex := range updated.Lines[0].Segments {
		if !reflect.DeepEqual(updated.Lines[0].Segments[segmentIndex].Ruby, expectedRuby[segmentIndex]) {
			t.Fatalf("segment %d ruby = %+v, want %+v", segmentIndex, updated.Lines[0].Segments[segmentIndex].Ruby, expectedRuby[segmentIndex])
		}
	}
	loaded, err := s.GetLyrics(saved.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	for segmentIndex := range loaded.Lines[0].Segments {
		if !reflect.DeepEqual(loaded.Lines[0].Segments[segmentIndex].Ruby, expectedRuby[segmentIndex]) {
			t.Fatalf("persisted segment %d ruby = %+v, want %+v", segmentIndex, loaded.Lines[0].Segments[segmentIndex].Ruby, expectedRuby[segmentIndex])
		}
	}

	if _, err := s.PublishLyrics(updated.MusicID, updated.Revision); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, updated.MusicID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, `"ruby"`) || strings.Contains(payload, `"reading"`) {
		t.Fatalf("public payload exposed private ruby: %s", payload)
	}
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	for segmentIndex, segment := range details[updated.MusicID].Lines[0].Segments {
		if len(segment.Ruby) != 0 {
			t.Fatalf("public segment %d exposed ruby: %+v", segmentIndex, segment.Ruby)
		}
	}
}

func TestLegacyPrivateLyricsSaveTreatsNilAndEmptyPerformerIDsAsEquivalent(t *testing.T) {
	s := setupLyricsStore(t)
	input := lyricsWithReadings()
	input.Lines[0].Segments[0].PerformerIDs = []int{}
	if _, err := s.SaveLyrics(input, "editor"); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetLyrics(input.MusicID)
	if err != nil {
		t.Fatal(err)
	}

	legacy := current
	legacy.Lines = append([]model.LyricLine(nil), current.Lines...)
	legacy.Lines[0].Segments = append([]model.LyricSegment(nil), current.Lines[0].Segments...)
	legacy.Lines[0].Segments[0].PerformerIDs = nil
	legacy.Lines[0].Segments[0].Ruby = nil
	legacy.Lines[0].English = "Legacy client changed only the translation"

	updated, err := s.SaveLyrics(legacy, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != current.Revision+1 || updated.Lines[0].English != legacy.Lines[0].English ||
		!reflect.DeepEqual(updated.Lines[0].Segments[0].Ruby, current.Lines[0].Segments[0].Ruby) {
		t.Fatalf("nil/empty performer compatibility save = %+v", updated)
	}
}

func TestExistingLyricsRejectMissingRubyWhenSourceStructureChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.SongLyrics)
	}{
		{name: "segment structure", mutate: func(candidate *model.SongLyrics) {
			candidate.Lines[0].Segments = []model.LyricSegment{
				{Text: "初", PerformerIDs: []int{1}},
				{Text: "音歌", PerformerIDs: []int{1, 2}},
				{Text: "う", PerformerIDs: []int{1, 2}},
			}
		}},
		{name: "performer removal", mutate: func(candidate *model.SongLyrics) {
			candidate.Lines[0].Segments = append([]model.LyricSegment(nil), candidate.Lines[0].Segments...)
			for segmentIndex := range candidate.Lines[0].Segments {
				candidate.Lines[0].Segments[segmentIndex].Ruby = nil
			}
			candidate.Lines[0].Segments[0].PerformerIDs = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			saved, err := s.SaveLyrics(lyricsWithReadings(), "editor")
			if err != nil {
				t.Fatal(err)
			}
			candidate := saved
			candidate.Lines = append([]model.LyricLine(nil), saved.Lines...)
			test.mutate(&candidate)
			_, err = s.SaveLyrics(candidate, "editor")
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" ||
				!strings.Contains(strings.Join(contractErr.Details, "; "), "ruby must be supplied") {
				t.Fatalf("missing ruby after %s error = %#v", test.name, err)
			}
			loaded, loadErr := s.GetLyrics(saved.MusicID)
			if loadErr != nil || loaded.Revision != saved.Revision ||
				!reflect.DeepEqual(loaded.Lines[0].Segments[0].Ruby, saved.Lines[0].Segments[0].Ruby) {
				t.Fatalf("rejected %s changed persisted lyrics: %+v err=%v", test.name, loaded, loadErr)
			}
		})
	}
}

func TestSavedLyricsAllowEquivalentResegmentationWithExplicitRuby(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(lyricsWithReadings(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	saved.Lines[0].Segments = []model.LyricSegment{
		{Text: "初", PerformerIDs: []int{1}, Ruby: []model.LyricRubySpan{{Text: "初", Reading: "はつ"}}},
		{Text: "音歌", PerformerIDs: []int{2}, Ruby: []model.LyricRubySpan{{Text: "音", Reading: "ね"}, {Text: "歌", Reading: "うた"}}},
		{Text: "う", PerformerIDs: []int{1, 2}, Ruby: []model.LyricRubySpan{{Text: "う"}}},
	}
	updated, err := s.SaveLyrics(saved, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != saved.Revision+1 || len(updated.Lines[0].Segments) != 3 || updated.Lines[0].Japanese != "初音歌う" ||
		updated.Lines[0].Segments[1].Ruby[1].Reading != "うた" {
		t.Fatalf("equivalent resegmentation = %+v", updated)
	}
}

func TestSavedLyricsRejectNumericOrderDrift(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	saved.Lines[0].Order = 10
	_, err = s.SaveLyrics(saved, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("numeric order drift error = %#v", err)
	}
}

func TestOrdinaryLyricsFirstSaveSourceURLPolicy(t *testing.T) {
	tests := []struct {
		name      string
		sourceURL string
		wantDrift bool
	}{
		{name: "no source URL"},
		{name: "external reference", sourceURL: "https://example.invalid/source"},
		{name: "other Fandom origin", sourceURL: "https://projectsekai.fandom.com/wiki/Song"},
		{name: "other Wiki origin", sourceURL: "https://en.wikipedia.org/wiki/Song"},
		{name: "managed origin", sourceURL: "https://vocaloid.fandom.com/wiki/Song", wantDrift: true},
		{name: "managed origin with oldid", sourceURL: "https://vocaloid.fandom.com/wiki/Song?oldid=123", wantDrift: true},
		{name: "managed origin case insensitive", sourceURL: "HTTPS://VOCALOID.FANDOM.COM/wiki/Song", wantDrift: true},
		{name: "managed origin with explicit default port", sourceURL: "https://vocaloid.fandom.com:443/wiki/Song", wantDrift: true},
		{name: "managed origin with trailing dot", sourceURL: "https://vocaloid.fandom.com./wiki/Song", wantDrift: true},
		{name: "managed hostname with non-default port", sourceURL: "https://vocaloid.fandom.com:444/wiki/Song", wantDrift: true},
		{name: "managed hostname over HTTP", sourceURL: "http://vocaloid.fandom.com/wiki/Song", wantDrift: true},
		{name: "managed legacy alias", sourceURL: "https://vocaloid.wikia.com/wiki/Song", wantDrift: true},
		{name: "managed legacy alias case insensitive", sourceURL: "HTTPS://VOCALOID.WIKIA.COM./wiki/Song", wantDrift: true},
		{name: "managed legacy alias with non-default port", sourceURL: "https://vocaloid.wikia.com:444/wiki/Song", wantDrift: true},
		{name: "managed legacy alias over HTTP", sourceURL: "http://vocaloid.wikia.com/wiki/Song", wantDrift: true},
		{name: "managed-looking subdomain is external", sourceURL: "https://vocaloid.fandom.com.example.invalid/wiki/Song"},
		{name: "managed-legacy-looking subdomain is external", sourceURL: "https://vocaloid.wikia.com.example.invalid/wiki/Song"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			input := validLyrics()
			input.SourceURL = test.sourceURL
			saved, err := s.SaveLyrics(input, "editor")
			var contractErr *LyricsContractError
			if test.wantDrift {
				if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
					t.Fatalf("source URL %q error = %#v", test.sourceURL, err)
				}
				return
			}
			if err != nil || saved.Revision != 1 || saved.SourceURL != test.sourceURL {
				t.Fatalf("source URL %q saved=%+v err=%v", test.sourceURL, saved, err)
			}
		})
	}
}

func TestLegacyURLOnlyManagedLyricsDraftCanEditOnlyWithoutSourceDrift(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourceURL = "https://vocaloid.fandom.com/wiki/Legacy_Song?oldid=123"
	if _, err := s.db.Exec(`INSERT INTO song_lyrics
		(music_id, revision, updated_at, updated_by, attribution, source_note, source_url, license_note, source_hash,
		 source_page_id, source_revision_id, source_sha1, source_fetched_at)
		VALUES (?, 1, ?, 'legacy', ?, ?, ?, ?, ?, 0, 0, '', 0)`,
		input.MusicID, time.Now().Unix(), input.Attribution, input.SourceNote, input.SourceURL, input.LicenseNote,
		lyricsSourceHash(input.Lines)); err != nil {
		t.Fatal(err)
	}
	for _, line := range input.Lines {
		stanzaBreak := 0
		if line.StanzaBreakBefore {
			stanzaBreak = 1
		}
		if _, err := s.db.Exec(`INSERT INTO song_lyric_lines
			(music_id, line_id, position, japanese, zh_cn, en_us, stanza_break_before) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			input.MusicID, line.ID, line.Order, line.Japanese, line.Chinese, line.English, stanzaBreak); err != nil {
			t.Fatal(err)
		}
		for position, segment := range line.Segments {
			performerIDsJSON, err := json.Marshal(segment.PerformerIDs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO song_lyric_segments
				(music_id, line_id, position, text, performer_ids_json) VALUES (?, ?, ?, ?, ?)`,
				input.MusicID, line.ID, position, segment.Text, string(performerIDsJSON)); err != nil {
				t.Fatal(err)
			}
		}
	}

	legacy, err := s.GetLyrics(input.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Lines[0].English = "Editable legacy translation"
	updated, err := s.SaveLyrics(legacy, "editor")
	if err != nil || updated.Revision != 2 || updated.Lines[0].English != "Editable legacy translation" {
		t.Fatalf("legacy edit saved=%+v err=%v", updated, err)
	}

	tests := []struct {
		name   string
		mutate func(*model.SongLyrics)
	}{
		{name: "source URL", mutate: func(candidate *model.SongLyrics) {
			candidate.SourceURL = "https://vocaloid.fandom.com/wiki/Legacy_Song?oldid=124"
		}},
		{name: "provenance", mutate: func(candidate *model.SongLyrics) {
			candidate.SourcePageID = 123
			candidate.SourceRevisionID = 124
			candidate.SourceSHA1 = validSourceSHA1
			candidate.SourceFetchedAt = "2026-07-22T12:34:56Z"
		}},
		{name: "Japanese source structure", mutate: func(candidate *model.SongLyrics) {
			candidate.Lines[0].Japanese = "初音が歌う"
			candidate.Lines[0].Segments[1].Text = "が歌う"
			candidate.Lines[0].Segments[1].Ruby = []model.LyricRubySpan{{Text: "が歌う"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := updated
			candidate.Lines = append([]model.LyricLine(nil), updated.Lines...)
			candidate.Lines[0].Segments = append([]model.LyricSegment(nil), updated.Lines[0].Segments...)
			test.mutate(&candidate)
			_, err := s.SaveLyrics(candidate, "editor")
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
				t.Fatalf("legacy %s drift error = %#v", test.name, err)
			}
		})
	}
}

func TestImportedLyricsPathRejectsNonzeroRevisionForNewDocument(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.Revision = 1
	_, _, err := s.SaveImportedLyricsMutation(input, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "revision_conflict" {
		t.Fatalf("new imported save with nonzero revision error = %#v", err)
	}
}

func TestVerifiedImportedLyricsManagedSourceTransportPolicy(t *testing.T) {
	tests := []struct {
		name      string
		sourceURL string
		wantDrift bool
	}{
		{name: "canonical HTTPS", sourceURL: "https://vocaloid.fandom.com/wiki/Song?oldid=456"},
		{name: "canonical escaped subpage", sourceURL: "https://vocaloid.fandom.com/wiki/%E5%88%9D%E9%9F%B3%E3%83%9F%E3%82%AF/Song?oldid=456"},
		{name: "canonical explicit default port", sourceURL: "https://vocaloid.fandom.com:443/wiki/Song?oldid=456"},
		{name: "legacy HTTPS", sourceURL: "https://vocaloid.wikia.com/wiki/Song?oldid=456"},
		{name: "legacy explicit default port", sourceURL: "https://vocaloid.wikia.com:443/wiki/Song?oldid=456"},
		{name: "external HTTP remains supported", sourceURL: "http://example.invalid/source"},
		{name: "canonical HTTP", sourceURL: "http://vocaloid.fandom.com/wiki/Song?oldid=456", wantDrift: true},
		{name: "canonical non-default port", sourceURL: "https://vocaloid.fandom.com:444/wiki/Song?oldid=456", wantDrift: true},
		{name: "canonical trailing dot", sourceURL: "https://vocaloid.fandom.com./wiki/Song?oldid=456", wantDrift: true},
		{name: "canonical missing oldid", sourceURL: "https://vocaloid.fandom.com/wiki/Song", wantDrift: true},
		{name: "canonical mismatched oldid", sourceURL: "https://vocaloid.fandom.com/wiki/Song?oldid=457", wantDrift: true},
		{name: "canonical extra query", sourceURL: "https://vocaloid.fandom.com/wiki/Song?oldid=456&diff=prev", wantDrift: true},
		{name: "canonical empty port", sourceURL: "https://vocaloid.fandom.com:/wiki/Song?oldid=456", wantDrift: true},
		{name: "canonical empty fragment", sourceURL: "https://vocaloid.fandom.com/wiki/Song?oldid=456#", wantDrift: true},
		{name: "canonical raw Unicode path", sourceURL: "https://vocaloid.fandom.com/wiki/初音ミク/Song?oldid=456", wantDrift: true},
		{name: "canonical lowercase path escapes", sourceURL: "https://vocaloid.fandom.com/wiki/%e5%88%9d%e9%9f%b3%e3%83%9f%e3%82%af/Song?oldid=456", wantDrift: true},
		{name: "canonical space instead of underscore", sourceURL: "https://vocaloid.fandom.com/wiki/Song%20Title?oldid=456", wantDrift: true},
		{name: "canonical wrong path", sourceURL: "https://vocaloid.fandom.com/api.php?oldid=456", wantDrift: true},
		{name: "canonical empty page", sourceURL: "https://vocaloid.fandom.com/wiki/?oldid=456", wantDrift: true},
		{name: "canonical encoded oldid", sourceURL: "https://vocaloid.fandom.com/wiki/Song?oldid=%34%35%36", wantDrift: true},
		{name: "canonical surrounding whitespace", sourceURL: " https://vocaloid.fandom.com/wiki/Song?oldid=456 ", wantDrift: true},
		{name: "legacy HTTP", sourceURL: "http://vocaloid.wikia.com/wiki/Song?oldid=456", wantDrift: true},
		{name: "legacy non-default port", sourceURL: "https://vocaloid.wikia.com:444/wiki/Song?oldid=456", wantDrift: true},
		{name: "managed credentials", sourceURL: "https://user:secret@vocaloid.fandom.com/wiki/Song?oldid=456", wantDrift: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			input := validLyrics()
			input.SourceURL = test.sourceURL
			input.SourcePageID = 123
			input.SourceRevisionID = 456
			input.SourceSHA1 = validSourceSHA1
			input.SourceFetchedAt = "2026-07-22T12:34:56Z"
			saved, changed, err := s.SaveImportedLyricsMutation(input, "editor")
			var contractErr *LyricsContractError
			if test.wantDrift {
				if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
					t.Fatalf("verified source URL %q error = %#v", test.sourceURL, err)
				}
				return
			}
			if err != nil || !changed || saved.Revision != 1 || saved.SourceURL != input.SourceURL || saved.SourceRevisionID != input.SourceRevisionID {
				t.Fatalf("verified source URL %q changed=%t saved=%+v err=%v", test.sourceURL, changed, saved, err)
			}
		})
	}
}

func TestImportedLyricsPathRejectsExistingDocument(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.SaveImportedLyricsMutation(saved, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("existing imported save error = %#v", err)
	}
}

func TestImportedLyricsEligibilityAndDatabaseSaveShareMusicStripe(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourcePageID = 123
	input.SourceRevisionID = 456
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:34:56Z"

	unlock := s.lockLyrics(input.MusicID)
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		_, _, err := s.SaveImportedLyricsMutation(input, "editor")
		finished <- err
	}()
	<-started
	select {
	case err := <-finished:
		unlock()
		t.Fatalf("imported eligibility/save escaped the music stripe: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := s.GetLyrics(input.MusicID); err != ErrLyricsNotFound {
		unlock()
		t.Fatalf("imported lyrics became visible before stripe release: %v", err)
	}
	unlock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("imported save failed after stripe release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("imported save remained blocked after stripe release")
	}
	loaded, err := s.GetLyrics(input.MusicID)
	if err != nil || loaded.Revision != 1 || loaded.SourceSHA1 != validSourceSHA1 {
		t.Fatalf("imported save after stripe release=%+v err=%v", loaded, err)
	}
}

func TestLyricsFirstSaveEligibilityAndSaveShareMusicStripe(t *testing.T) {
	s := setupLyricsStore(t)
	callbackStarted := make(chan struct{})
	allowCallback := make(chan struct{})
	eligibilityDone := make(chan error, 1)
	go func() {
		eligibilityDone <- s.WithLyricsFirstSaveEligibility(10, func() error {
			close(callbackStarted)
			<-allowCallback
			return nil
		})
	}()
	<-callbackStarted

	saveDone := make(chan error, 1)
	go func() {
		_, err := s.SaveLyrics(validLyrics(), "editor")
		saveDone <- err
	}()
	select {
	case err := <-saveDone:
		close(allowCallback)
		t.Fatalf("first save crossed eligibility callback: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowCallback)
	if err := <-eligibilityDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-saveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first save did not continue after eligibility callback")
	}
	if err := s.WithLyricsFirstSaveEligibility(10, func() error {
		t.Fatal("eligibility callback ran for an existing document")
		return nil
	}); !errors.Is(err, ErrLyricsAlreadySaved) {
		t.Fatalf("existing document eligibility error = %v", err)
	}
}

func TestLyricsMutexStripesHandleNonpositiveIDs(t *testing.T) {
	s := setupLyricsStore(t)
	if len(s.lyricsMutexes) != 256 {
		t.Fatalf("lyrics mutex stripe count = %d", len(s.lyricsMutexes))
	}
	for _, musicID := range []int{-1, 0, 1, -256, 256} {
		stripe := lyricsMutexStripe(musicID)
		if stripe < 0 || stripe >= lyricsMutexStripeCount {
			t.Fatalf("musicID=%d stripe=%d", musicID, stripe)
		}
		unlock := s.lockLyrics(musicID)
		unlock()
	}
	if lyricsMutexStripe(-1) != 255 || lyricsMutexStripe(-256) != 0 || lyricsMutexStripe(0) != 0 {
		t.Fatalf("nonpositive stripes: -1=%d -256=%d 0=%d", lyricsMutexStripe(-1), lyricsMutexStripe(-256), lyricsMutexStripe(0))
	}
}

func TestSameSongSavePublishUnpublishAreSerialized(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*Store, model.SongLyrics) error
		mutate  func(*Store, model.SongLyrics) error
	}{
		{name: "publish", mutate: func(s *Store, saved model.SongLyrics) error {
			_, err := s.PublishLyrics(saved.MusicID, saved.Revision)
			return err
		}},
		{name: "unpublish", prepare: func(s *Store, saved model.SongLyrics) error {
			_, err := s.PublishLyrics(saved.MusicID, saved.Revision)
			return err
		}, mutate: func(s *Store, saved model.SongLyrics) error {
			_, err := s.UnpublishLyrics(saved.MusicID, saved.Revision)
			return err
		}},
		{name: "save", mutate: func(s *Store, saved model.SongLyrics) error {
			candidate := saved
			candidate.Lines = append([]model.LyricLine(nil), saved.Lines...)
			candidate.Lines[0].Segments = append([]model.LyricSegment(nil), saved.Lines[0].Segments...)
			candidate.Lines[0].English = "Serialized edit"
			_, err := s.SaveLyrics(candidate, "editor")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			saved, err := s.SaveLyrics(validLyrics(), "editor")
			if err != nil {
				t.Fatal(err)
			}
			if test.prepare != nil {
				if err := test.prepare(s, saved); err != nil {
					t.Fatal(err)
				}
			}

			unlock := s.lockLyrics(saved.MusicID)
			started := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				close(started)
				finished <- test.mutate(s, saved)
			}()
			<-started
			select {
			case err := <-finished:
				unlock()
				t.Fatalf("same-song %s completed while stripe was locked: %v", test.name, err)
			case <-time.After(100 * time.Millisecond):
			}
			unlock()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatalf("same-song %s failed after stripe unlock: %v", test.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("same-song %s remained blocked after stripe unlock", test.name)
			}
		})
	}
}

func TestDifferentSongMutationsCanAcquireLocksConcurrently(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Store, model.SongLyrics) error
	}{
		{name: "save", mutate: func(s *Store, saved model.SongLyrics) error {
			candidate := saved
			candidate.Lines = append([]model.LyricLine(nil), saved.Lines...)
			candidate.Lines[0].Segments = append([]model.LyricSegment(nil), saved.Lines[0].Segments...)
			candidate.Lines[0].English = "Different-song edit"
			_, err := s.SaveLyrics(candidate, "editor")
			return err
		}},
		{name: "publish", mutate: func(s *Store, saved model.SongLyrics) error {
			_, err := s.PublishLyrics(saved.MusicID, saved.Revision)
			return err
		}},
		{name: "unpublish", mutate: func(s *Store, saved model.SongLyrics) error {
			if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
				return err
			}
			_, err := s.UnpublishLyrics(saved.MusicID, saved.Revision)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			first := validLyrics()
			if _, err := s.SaveLyrics(first, "editor"); err != nil {
				t.Fatal(err)
			}
			second := validLyrics()
			second.MusicID = 20
			second.Lines[0].ID = "line-20"
			secondSaved, err := s.SaveLyrics(second, "editor")
			if err != nil {
				t.Fatal(err)
			}
			if lyricsMutexStripe(first.MusicID) == lyricsMutexStripe(second.MusicID) {
				t.Fatal("test music IDs unexpectedly share a mutex stripe")
			}

			unlock := s.lockLyrics(first.MusicID)
			finished := make(chan error, 1)
			go func() { finished <- test.mutate(s, secondSaved) }()
			select {
			case err := <-finished:
				unlock()
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				unlock()
				t.Fatalf("different-song %s blocked on unrelated stripe", test.name)
			}
		})
	}
}

func TestGetLyricsRejectsCorruptPerformerJSON(t *testing.T) {
	s := setupLyricsStore(t)
	if _, err := s.SaveLyrics(validLyrics(), "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE song_lyric_segments SET performer_ids_json='not-json' WHERE music_id=10`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLyrics(10); err == nil || !strings.Contains(err.Error(), "lyrics segment performers") {
		t.Fatalf("corrupt performer JSON error=%v", err)
	}
}

func TestGetLyricsUsesOneSQLiteSnapshot(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}

	readStarted := make(chan struct{})
	allowRead := make(chan struct{})
	readResult := make(chan model.SongLyrics, 1)
	readErr := make(chan error, 1)
	go func() {
		loaded, err := s.getLyricsSnapshot(saved.MusicID, func() {
			close(readStarted)
			<-allowRead
		})
		if err != nil {
			readErr <- err
			return
		}
		readResult <- loaded
	}()
	<-readStarted

	updated := saved
	updated.Lines = append([]model.LyricLine(nil), saved.Lines...)
	updated.Lines[0].Segments = append([]model.LyricSegment(nil), saved.Lines[0].Segments...)
	updated.Lines[0].English = "Committed after snapshot header"
	updated.Lines[0].Segments = []model.LyricSegment{{
		Text: updated.Lines[0].Japanese, PerformerIDs: []int{2},
		Ruby: []model.LyricRubySpan{{Text: updated.Lines[0].Japanese}},
	}}
	updated, err = s.SaveLyrics(updated, "editor")
	if err != nil {
		t.Fatal(err)
	}
	close(allowRead)

	var snapshot model.SongLyrics
	select {
	case err := <-readErr:
		t.Fatal(err)
	case snapshot = <-readResult:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot read did not complete")
	}
	if snapshot.Revision != saved.Revision || snapshot.Lines[0].English != saved.Lines[0].English ||
		len(snapshot.Lines[0].Segments) != 2 || snapshot.Lines[0].Segments[0].PerformerIDs[0] != 1 {
		t.Fatalf("snapshot mixed revisions: %+v", snapshot)
	}
	latest, err := s.GetLyrics(saved.MusicID)
	if err != nil || latest.Revision != updated.Revision || latest.Lines[0].English != updated.Lines[0].English ||
		len(latest.Lines[0].Segments) != 1 || latest.Lines[0].Segments[0].PerformerIDs[0] != 2 {
		t.Fatalf("latest lyrics=%+v err=%v", latest, err)
	}
}

func TestConcurrentLyricsSaveFromSameRevisionHasOneWinner(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, english := range []string{"First concurrent edit", "Second concurrent edit"} {
		wg.Add(1)
		go func(english string) {
			defer wg.Done()
			candidate := saved
			candidate.Lines = append([]model.LyricLine(nil), saved.Lines...)
			candidate.Lines[0].Segments = append([]model.LyricSegment(nil), saved.Lines[0].Segments...)
			candidate.Lines[0].English = english
			<-start
			_, err := s.SaveLyrics(candidate, "editor")
			results <- err
		}(english)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var contractErr *LyricsContractError
		if errors.As(err, &contractErr) && contractErr.Code == "revision_conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent save error = %v", err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent save results successes=%d conflicts=%d", successes, conflicts)
	}
	loaded, err := s.GetLyrics(saved.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != saved.Revision+1 {
		t.Fatalf("concurrent save revision = %d", loaded.Revision)
	}
}

func TestLyricsValidationCodes(t *testing.T) {
	s := setupLyricsStore(t)

	emptySource := validLyrics()
	emptySource.Lines[0].Japanese = ""
	emptySource.Lines[0].Segments = []model.LyricSegment{{Text: "", PerformerIDs: []int{1}}}
	_, err := s.SaveLyrics(emptySource, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" || len(contractErr.Details) == 0 ||
		!strings.Contains(contractErr.Details[0], ".japanese must not be empty") {
		t.Fatalf("empty Japanese source error = %#v", err)
	}

	mismatch := validLyrics()
	mismatch.Lines[0].Segments[1].Text = "不一致"
	_, err = s.SaveLyrics(mismatch, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" || len(contractErr.Details) == 0 {
		t.Fatalf("segment mismatch error = %#v", err)
	}

	invalidPerformer := validLyrics()
	invalidPerformer.Lines[0].Segments[0].PerformerIDs = []int{999}
	_, err = s.SaveLyrics(invalidPerformer, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "invalid_performer" {
		t.Fatalf("performer error = %#v", err)
	}

	duplicatePerformer := validLyrics()
	duplicatePerformer.Lines[0].Segments[0].PerformerIDs = []int{1, 1}
	_, err = s.SaveLyrics(duplicatePerformer, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "invalid_performer" {
		t.Fatalf("duplicate performer error = %#v", err)
	}

	originalOnly := validLyrics()
	originalOnly.Lines[0].Chinese = ""
	originalOnly.Lines[0].English = ""
	saved, err := s.SaveLyrics(originalOnly, "editor")
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.PublishLyrics(10, saved.Revision)
	if err != nil || published.Status != "published" {
		t.Fatalf("original-only publication = %+v err=%v", published, err)
	}
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	if detail := details[10]; len(detail.Lines) != 1 || detail.Lines[0].Japanese != originalOnly.Lines[0].Japanese ||
		detail.Lines[0].Chinese != "" || detail.Lines[0].English != "" {
		t.Fatalf("original-only public detail = %+v", detail)
	}

	missingAttribution := validLyrics()
	missingAttribution.MusicID = 20
	missingAttribution.Attribution = ""
	saved, err = s.SaveLyrics(missingAttribution, "editor")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishLyrics(missingAttribution.MusicID, saved.Revision)
	if !errors.As(err, &contractErr) || contractErr.Code != "incomplete_publication" {
		t.Fatalf("missing attribution publication error = %#v", err)
	}
}

func TestLyricsValidationRejectsOversizedFields(t *testing.T) {
	s := setupLyricsStore(t)
	for _, field := range []string{"translation", "proofreading"} {
		oversized := validLyrics()
		if field == "translation" {
			oversized.TranslationCredit = strings.Repeat("x", maxLyricsMetadataBytes+1)
		} else {
			oversized.ProofreadingCredit = strings.Repeat("x", maxLyricsMetadataBytes+1)
		}
		_, err := s.SaveLyrics(oversized, "editor")
		var contractErr *LyricsContractError
		if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" {
			t.Fatalf("oversized %s credit error=%#v", field, err)
		}
	}

	oversized := validLyrics()
	oversized.Lines[0].English = strings.Repeat("x", maxLyricsLineTextBytes+1)
	_, err := s.SaveLyrics(oversized, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" {
		t.Fatalf("oversized line error = %#v", err)
	}

	oversized = validLyrics()
	oversized.SourceURL = "https://example.invalid/" + strings.Repeat("x", maxLyricsURLBytes)
	_, err = s.SaveLyrics(oversized, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" {
		t.Fatalf("oversized URL error = %#v", err)
	}

	oversized = validLyrics()
	oversized.Lines[0].Japanese = strings.Repeat("a", maxLyricsRubyPerSegment+1)
	oversized.Lines[0].Segments = []model.LyricSegment{{
		Text: oversized.Lines[0].Japanese, PerformerIDs: []int{1},
		Ruby: make([]model.LyricRubySpan, maxLyricsRubyPerSegment+1),
	}}
	for index := range oversized.Lines[0].Segments[0].Ruby {
		oversized.Lines[0].Segments[0].Ruby[index] = model.LyricRubySpan{Text: "a"}
	}
	_, err = s.SaveLyrics(oversized, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "segment_mismatch" ||
		!strings.Contains(strings.Join(contractErr.Details, "; "), "between 1 and 256") {
		t.Fatalf("oversized ruby span count error = %#v", err)
	}

	oversized = validLyrics()
	oversized.Lines[0].Segments[0].PerformerIDs = make([]int, maxLyricsPerformers+1)
	_, err = s.SaveLyrics(oversized, "editor")
	if !errors.As(err, &contractErr) || contractErr.Code != "invalid_performer" ||
		!strings.Contains(strings.Join(contractErr.Details, "; "), "at most 64 performerIds") {
		t.Fatalf("oversized performer count error = %#v", err)
	}
}

func TestLyricsPublicationRejectsEncodedArtifactOverConsumerLimit(t *testing.T) {
	s := setupLyricsStore(t)
	candidate := validLyrics()
	candidate.Attribution = strings.Repeat("\\", maxLyricsMetadataBytes)
	quoted := strings.Repeat("\\", maxLyricsLineTextBytes)
	candidate.Lines = make([]model.LyricLine, 32)
	for index := range candidate.Lines {
		candidate.Lines[index] = model.LyricLine{
			ID:       fmt.Sprintf("source-%d", index),
			Order:    index,
			Japanese: quoted,
			Chinese:  quoted,
			English:  quoted,
			Segments: []model.LyricSegment{{Text: quoted, PerformerIDs: []int{1}}},
		}
	}
	saved, err := s.SaveLyrics(candidate, "editor")
	if err != nil {
		t.Fatalf("raw-size-valid lyrics save: %v", err)
	}
	_, err = s.PublishLyrics(saved.MusicID, saved.Revision)
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "incomplete_publication" ||
		!strings.Contains(strings.Join(contractErr.Details, "; "), "public artifact size limit") {
		t.Fatalf("encoded artifact publication error = %#v", err)
	}
	var publications int
	if queryErr := s.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_publications WHERE music_id=?`, saved.MusicID).Scan(&publications); queryErr != nil {
		t.Fatal(queryErr)
	}
	if publications != 0 {
		t.Fatalf("oversized publication rows = %d", publications)
	}
}

func TestLyricsPublicationAllowsEmptyPerformersAndFreezesProvenance(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourcePageID = 10
	input.SourceRevisionID = 20
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:00:00Z"
	input.Lines[0].Segments[0].PerformerIDs = nil
	saved, _, err := s.SaveImportedLyricsMutation(input, "editor")
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.PublishLyrics(saved.MusicID, saved.Revision)
	if err != nil || published.Status != "published" {
		t.Fatalf("empty performer publication = %+v err=%v", published, err)
	}
	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	detail := details[saved.MusicID]
	if len(detail.Lines) != 1 || len(detail.Lines[0].Segments) != 2 ||
		detail.Lines[0].Segments[0].PerformerIDs == nil || len(detail.Lines[0].Segments[0].PerformerIDs) != 0 {
		t.Fatalf("empty performer public detail = %+v", detail)
	}

	drift := saved
	drift.SourceRevisionID++
	drift.SourceSHA1 = "1123456789abcdef0123456789abcdef01234567"
	_, err = s.SaveLyrics(drift, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("provenance drift error = %#v", err)
	}
}

func TestCatalogFiltersStableMasterdataIDs(t *testing.T) {
	s := setupLyricsStore(t)
	defaultResult, err := s.CatalogMusic("", true, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultResult.Items) != 1 || defaultResult.Items[0].MusicID != 10 || !defaultResult.Items[0].IsNewlyWrittenMusic {
		t.Fatalf("default catalog = %+v", defaultResult)
	}
	all, err := s.CatalogMusic("", false, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 1 || all.NextCursor != "10" {
		t.Fatalf("paged catalog = %+v", all)
	}
	performers, err := s.CatalogPerformers()
	if err != nil {
		t.Fatal(err)
	}
	if len(performers.Items) != 2 || performers.Items[0].PerformerID != 1 {
		t.Fatalf("performer catalog = %+v", performers)
	}
}

func TestLyricsSourceURLRejectsUnsafeSchemesAndCredentials(t *testing.T) {
	s := setupLyricsStore(t)
	for _, sourceURL := range []string{"javascript:alert(1)", "data:text/html,unsafe", "https://user:secret@example.invalid/source"} {
		input := validLyrics()
		input.SourceURL = sourceURL
		_, err := s.SaveLyrics(input, "editor")
		var contractErr *LyricsContractError
		if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
			t.Fatalf("source URL %q error = %#v", sourceURL, err)
		}
	}
}

func TestLyricsSourceProvenanceRejectsMalformedSHA1(t *testing.T) {
	tests := []struct {
		name       string
		sourceSHA1 string
	}{
		{name: "short", sourceSHA1: "0123456789abcdef0123456789abcdef0123456"},
		{name: "uppercase", sourceSHA1: "0123456789abcdef0123456789abcdef0123456A"},
		{name: "nonhex", sourceSHA1: "0123456789abcdef0123456789abcdef0123456g"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			input := validLyrics()
			input.SourcePageID = 123
			input.SourceRevisionID = 456
			input.SourceSHA1 = test.sourceSHA1
			input.SourceFetchedAt = "2026-07-22T12:34:56Z"
			_, _, err := s.SaveImportedLyricsMutation(input, "editor")
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" || len(contractErr.Details) == 0 ||
				!strings.Contains(contractErr.Details[0], "40 lowercase hexadecimal") {
				t.Fatalf("sourceSha1=%q error = %#v", test.sourceSHA1, err)
			}
		})
	}
}

func TestRestoreRejectsManagedSourceTransportPolicyWithoutChangingStoredContent(t *testing.T) {
	s := setupLyricsStore(t)
	existing, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}

	source := setupLyricsStore(t)
	imported := validLyrics()
	imported.SourceURL = "https://vocaloid.fandom.com/wiki/Song?oldid=456"
	imported.SourcePageID = 123
	imported.SourceRevisionID = 456
	imported.SourceSHA1 = validSourceSHA1
	imported.SourceFetchedAt = "2026-07-22T12:34:56Z"
	if _, _, err := source.SaveImportedLyricsMutation(imported, "editor"); err != nil {
		t.Fatal(err)
	}
	exported, err := source.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}

	for name, sourceURL := range map[string]string{
		"canonical HTTP":             "http://vocaloid.fandom.com/wiki/Song?oldid=456",
		"canonical non-default port": "https://vocaloid.fandom.com:444/wiki/Song?oldid=456",
		"canonical trailing dot":     "https://vocaloid.fandom.com./wiki/Song?oldid=456",
		"canonical missing oldid":    "https://vocaloid.fandom.com/wiki/Song",
		"canonical mismatched oldid": "https://vocaloid.fandom.com/wiki/Song?oldid=457",
		"legacy HTTP":                "http://vocaloid.wikia.com/wiki/Song?oldid=456",
		"legacy non-default port":    "https://vocaloid.wikia.com:444/wiki/Song?oldid=456",
		"managed credentials":        "https://user:secret@vocaloid.fandom.com/wiki/Song?oldid=456",
	} {
		t.Run(name, func(t *testing.T) {
			invalid := exported
			invalid.Documents = append([]LyricsDocumentBackupRecord(nil), exported.Documents...)
			invalid.Documents[0].SourceURL = sourceURL
			err := s.ImportTranslationContent(nil, EventContentExport{}, invalid)
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
				t.Fatalf("restored source URL %q error = %#v", sourceURL, err)
			}
			loaded, loadErr := s.GetLyrics(existing.MusicID)
			if loadErr != nil || loaded.Revision != existing.Revision || loaded.SourceURL != existing.SourceURL || loaded.Lines[0].Japanese != existing.Lines[0].Japanese {
				t.Fatalf("failed restore changed existing lyrics: %+v err=%v", loaded, loadErr)
			}
		})
	}
}

func TestRestoreRejectsMalformedLyricsSourceSHA1(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourcePageID = 123
	input.SourceRevisionID = 456
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:34:56Z"
	if _, _, err := s.SaveImportedLyricsMutation(input, "editor"); err != nil {
		t.Fatal(err)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		sourceSHA1 string
	}{
		{name: "short", sourceSHA1: "0123456789abcdef0123456789abcdef0123456"},
		{name: "uppercase", sourceSHA1: "0123456789abcdef0123456789abcdef0123456A"},
		{name: "nonhex", sourceSHA1: "0123456789abcdef0123456789abcdef0123456g"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			malformed := exported
			malformed.Documents = append([]LyricsDocumentBackupRecord(nil), exported.Documents...)
			malformed.Documents[0].SourceSHA1 = test.sourceSHA1
			restored := setupLyricsStore(t)
			err := restored.ImportTranslationContent(nil, EventContentExport{}, malformed)
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" || len(contractErr.Details) == 0 ||
				!strings.Contains(contractErr.Details[0], "40 lowercase hexadecimal") {
				t.Fatalf("malformed restored sourceSha1 error = %v", err)
			}
		})
	}
}

func TestUnprovenancedDraftCanUpdateLinesAndBindProvenance(t *testing.T) {
	s := setupLyricsStore(t)
	// Initial unprovenanced draft with 1 line
	draft := validLyrics()
	draft.SourceURL = ""
	draft.SourcePageID = 0
	draft.SourceRevisionID = 0
	draft.SourceSHA1 = ""
	draft.SourceFetchedAt = ""
	saved, err := s.SaveLyrics(draft, "editor")
	if err != nil {
		t.Fatalf("initial draft save failed: %v", err)
	}

	// Update draft with 2 lines, different text, and bind Fandom source URL
	updated := saved
	updated.SourceURL = "https://projectsekai.fandom.com/wiki/Stardust_Rain"
	updated.SourcePageID = 77201
	updated.SourceRevisionID = 367044
	updated.SourceSHA1 = validSourceSHA1
	updated.SourceFetchedAt = "2026-08-16T12:00:00Z"
	updated.Lines = []model.LyricLine{
		{
			ID:       "1",
			Order:    0,
			Japanese: "夢の続きは 月夜に結われて",
			Chinese:  "梦想的续篇 在月夜下编织相系",
			Segments: []model.LyricSegment{
				{
					Text:         "夢の続きは 月夜に結われて",
					PerformerIDs: []int{1},
					Ruby:         []model.LyricRubySpan{{Text: "夢の続きは 月夜に結われて"}},
				},
			},
		},
		{
			ID:       "2",
			Order:    1,
			Japanese: "零れた帳が 両手に解ける",
			Chinese:  "垂落的夜幕 在双手中轻柔化解",
			Segments: []model.LyricSegment{
				{
					Text:         "零れた帳が 両手に解ける",
					PerformerIDs: []int{2},
					Ruby:         []model.LyricRubySpan{{Text: "零れた帳が 両手に解ける"}},
				},
			},
		},
	}

	saved2, err := s.SaveLyrics(updated, "editor")
	if err != nil {
		t.Fatalf("unprovenanced draft line update failed: %v", err)
	}
	if saved2.Revision != saved.Revision+1 || len(saved2.Lines) != 2 || saved2.SourceURL != updated.SourceURL {
		t.Fatalf("saved2 mismatch: %+v", saved2)
	}

	// Once provenanced, subsequent line changes without import token must be rejected
	drift := saved2
	drift.Lines = []model.LyricLine{saved2.Lines[0]}
	_, err = s.SaveLyrics(drift, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("provenanced line change should fail, got: %v", err)
	}
}

func TestLyricsSourceProvenanceRoundTrip(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourcePageID = 123
	input.SourceRevisionID = 456
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:34:56.123456789Z"
	saved, _, err := s.SaveImportedLyricsMutation(input, "editor")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetLyrics(saved.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SourcePageID != 123 || loaded.SourceRevisionID != 456 || loaded.SourceSHA1 != validSourceSHA1 || loaded.SourceFetchedAt != input.SourceFetchedAt {
		t.Fatalf("source provenance = %+v", loaded)
	}

	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, exported); err != nil {
		t.Fatalf("restore rejected canonical source SHA1: %v", err)
	}
	restoredLyrics, err := restored.GetLyrics(saved.MusicID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredLyrics.SourceSHA1 != validSourceSHA1 || restoredLyrics.SourceFetchedAt != input.SourceFetchedAt {
		t.Fatalf("restored provenance = %+v", restoredLyrics)
	}

	invalid := validLyrics()
	invalid.SourcePageID = 123
	_, err = s.SaveLyrics(invalid, "editor")
	var contractErr *LyricsContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
		t.Fatalf("partial source provenance error = %#v", err)
	}
}

func TestRestoreRejectsMismatchedExactLyricsSourceFetchedAt(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourcePageID = 123
	input.SourceRevisionID = 456
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:34:56.123456789Z"
	if _, _, err := s.SaveImportedLyricsMutation(input, "editor"); err != nil {
		t.Fatal(err)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*LyricsDocumentBackupRecord){
		"different second": func(record *LyricsDocumentBackupRecord) {
			record.SourceFetchedAtRFC3339 = "2026-07-22T12:34:57.123456789Z"
		},
		"noncanonical fraction": func(record *LyricsDocumentBackupRecord) {
			record.SourceFetchedAtRFC3339 = "2026-07-22T12:34:56.123456700Z"
		},
		"exact without seconds": func(record *LyricsDocumentBackupRecord) {
			record.SourceFetchedAt = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := exported
			invalid.Documents = append([]LyricsDocumentBackupRecord(nil), exported.Documents...)
			mutate(&invalid.Documents[0])
			restored := setupLyricsStore(t)
			if err := restored.ImportTranslationContent(nil, EventContentExport{}, invalid); err == nil {
				t.Fatal("restore accepted inconsistent exact sourceFetchedAt")
			}
		})
	}
}

func TestExportLyricsContentUsesOneSQLiteSnapshot(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}

	exportStarted := make(chan struct{})
	allowExport := make(chan struct{})
	exportResult := make(chan LyricsContentExport, 1)
	exportErr := make(chan error, 1)
	go func() {
		exported, err := s.exportLyricsContentSnapshot(context.Background(), func() {
			close(exportStarted)
			<-allowExport
		})
		if err != nil {
			exportErr <- err
			return
		}
		exportResult <- exported
	}()
	<-exportStarted

	updated := saved
	updated.Lines = append([]model.LyricLine(nil), saved.Lines...)
	updated.Lines[0].Segments = []model.LyricSegment{{
		Text: updated.Lines[0].Japanese, PerformerIDs: []int{2},
		Ruby: []model.LyricRubySpan{{Text: updated.Lines[0].Japanese}},
	}}
	updated.Lines[0].English = "Published after export snapshot"
	updated, err = s.SaveLyrics(updated, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(updated.MusicID, updated.Revision); err != nil {
		t.Fatal(err)
	}
	close(allowExport)

	var snapshot LyricsContentExport
	select {
	case err := <-exportErr:
		t.Fatal(err)
	case snapshot = <-exportResult:
	case <-time.After(2 * time.Second):
		t.Fatal("lyrics export snapshot did not complete")
	}
	if len(snapshot.Documents) != 1 || snapshot.Documents[0].Revision != saved.Revision ||
		len(snapshot.Lines) != 1 || snapshot.Lines[0].English != saved.Lines[0].English ||
		len(snapshot.Segments) != 2 || snapshot.Segments[0].PerformerIDsJSON != "[1]" ||
		len(snapshot.Publications) != 1 || snapshot.Publications[0].Revision != saved.Revision ||
		!strings.Contains(snapshot.Publications[0].PayloadJSON, `"en-US":"Miku sings"`) {
		t.Fatalf("export mixed revisions: %+v", snapshot)
	}
	fresh, err := s.ExportLyricsContent()
	if err != nil || fresh.Documents[0].Revision != updated.Revision || fresh.Lines[0].English != updated.Lines[0].English ||
		len(fresh.Segments) != 1 || fresh.Segments[0].PerformerIDsJSON != "[2]" || fresh.Publications[0].Revision != updated.Revision {
		t.Fatalf("fresh export=%+v err=%v", fresh, err)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, snapshot); err != nil {
		t.Fatalf("snapshot export did not restore coherently: %v", err)
	}
	loaded, err := restored.GetLyrics(saved.MusicID)
	if err != nil || loaded.Revision != saved.Revision || loaded.Lines[0].English != saved.Lines[0].English {
		t.Fatalf("restored snapshot=%+v err=%v", loaded, err)
	}
}

func TestLegacyPublicationLineIDsAreCanonicalizedForPublicReadAndRestore(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.Lines[0].ID = "wiki-123-456-1"
	saved, err := s.SaveLyrics(input, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, saved.MusicID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	legacyPayload := strings.Replace(payload, `"id":"line-1"`, `"id":"wiki-123-456-1"`, 1)
	if legacyPayload == payload {
		t.Fatalf("could not construct legacy publication payload: %s", payload)
	}
	if _, err := s.db.Exec(`UPDATE song_lyrics_publications SET payload_json=? WHERE music_id=?`, legacyPayload, saved.MusicID); err != nil {
		t.Fatal(err)
	}

	_, details, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	if got := details[saved.MusicID].Lines[0].ID; got != "line-1" {
		t.Fatalf("legacy public line ID = %q", got)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported.Publications[0].PayloadJSON, `wiki-123-456-1`) {
		t.Fatalf("test export lost the simulated legacy ID: %s", exported.Publications[0].PayloadJSON)
	}
	restored := setupLyricsStore(t)
	if err := restored.ImportTranslationContent(nil, EventContentExport{}, exported); err != nil {
		t.Fatal(err)
	}
	var restoredPayload string
	if err := restored.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, saved.MusicID).Scan(&restoredPayload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(restoredPayload, `wiki-123-456-1`) || !strings.Contains(restoredPayload, `"id":"line-1"`) {
		t.Fatalf("restored publication retained private line identity: %s", restoredPayload)
	}
}

func TestLyricsSourceProvenanceRejectsNonpositiveFetchedAt(t *testing.T) {
	for _, fetchedAt := range []string{"1970-01-01T00:00:00Z", "1969-12-31T23:59:59Z"} {
		t.Run(fetchedAt, func(t *testing.T) {
			s := setupLyricsStore(t)
			input := validLyrics()
			input.SourcePageID = 123
			input.SourceRevisionID = 456
			input.SourceSHA1 = validSourceSHA1
			input.SourceFetchedAt = fetchedAt
			_, _, err := s.SaveImportedLyricsMutation(input, "editor")
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
				t.Fatalf("sourceFetchedAt=%q error = %#v", fetchedAt, err)
			}
		})
	}
}

func TestLyricsSourceFetchedAtPreservesCanonicalNanosecondsForDriftComparison(t *testing.T) {
	s := setupLyricsStore(t)
	input := validLyrics()
	input.SourcePageID = 123
	input.SourceRevisionID = 456
	input.SourceSHA1 = validSourceSHA1
	input.SourceFetchedAt = "2026-07-22T12:34:56.123456789Z"
	saved, _, err := s.SaveImportedLyricsMutation(input, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SourceFetchedAt != input.SourceFetchedAt {
		t.Fatalf("sourceFetchedAt = %q", saved.SourceFetchedAt)
	}
	saved.Lines[0].English = "Canonical retry"
	if _, err := s.SaveLyrics(saved, "editor"); err != nil {
		t.Fatalf("canonical retry reported drift: %v", err)
	}
}

func TestLyricsSourceFetchedAtRejectsNoncanonicalOffsetOrFraction(t *testing.T) {
	for _, fetchedAt := range []string{
		"2026-07-22T20:34:56.9+08:00",
		"2026-07-22T12:34:56.900Z",
	} {
		t.Run(fetchedAt, func(t *testing.T) {
			s := setupLyricsStore(t)
			input := validLyrics()
			input.SourcePageID = 123
			input.SourceRevisionID = 456
			input.SourceSHA1 = validSourceSHA1
			input.SourceFetchedAt = fetchedAt
			_, _, err := s.SaveImportedLyricsMutation(input, "editor")
			var contractErr *LyricsContractError
			if !errors.As(err, &contractErr) || contractErr.Code != "source_drift" {
				t.Fatalf("sourceFetchedAt=%q error = %#v", fetchedAt, err)
			}
		})
	}
}

func TestLyricsMutationsWriteAuditRows(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UnpublishLyrics(saved.MusicID, saved.Revision, "admin"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action IN ('lyrics.save', 'lyrics.publish', 'lyrics.unpublish')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("lyrics audit count = %d", count)
	}
}

func TestRestoreRejectsInvalidLyricsDraftWithoutChangingStoredContent(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	invalid := LyricsContentExport{
		Music:      []CatalogMusicBackupRecord{{MusicID: 10, TitleJA: "新曲", NewlyWritten: 1}},
		Performers: []CatalogPerformerBackupRecord{{PerformerID: 1, NameJA: "初音ミク"}},
		Documents:  []LyricsDocumentBackupRecord{{MusicID: 10, Revision: 1, UpdatedAt: 1, SourceHash: saved.Lines[0].ID}},
		Lines:      []LyricsLineBackupRecord{{MusicID: 10, LineID: "line-1", Japanese: "", Chinese: "歌唱", English: "Sings"}},
		Segments:   []LyricsSegmentBackupRecord{{MusicID: 10, LineID: "line-1", Text: "", PerformerIDsJSON: "[1]"}},
	}
	if err := s.ImportTranslationContent(nil, EventContentExport{}, invalid); err == nil {
		t.Fatal("invalid lyrics draft unexpectedly restored")
	}
	loaded, err := s.GetLyrics(10)
	if err != nil || loaded.Revision != saved.Revision || loaded.Lines[0].Japanese != saved.Lines[0].Japanese {
		t.Fatalf("failed restore changed stored lyrics: %+v err=%v", loaded, err)
	}
}

func TestRestoreRejectsInvalidLyricsPublication(t *testing.T) {
	s := setupLyricsStore(t)
	lyrics := LyricsContentExport{
		Music:      []CatalogMusicBackupRecord{{MusicID: 10, TitleJA: "新曲", NewlyWritten: 1}},
		Performers: []CatalogPerformerBackupRecord{{PerformerID: 1, NameJA: "初音ミク"}},
		Documents:  []LyricsDocumentBackupRecord{{MusicID: 10, Revision: 1, UpdatedAt: 1, SourceHash: "hash"}},
		Lines:      []LyricsLineBackupRecord{{MusicID: 10, LineID: "line-1", Japanese: "歌う", Chinese: "歌唱", English: "Sings"}},
		Segments:   []LyricsSegmentBackupRecord{{MusicID: 10, LineID: "line-1", Text: "歌う", PerformerIDsJSON: "[1]"}},
		Publications: []LyricsPublicationBackupRecord{{
			MusicID: 10, Revision: 1, UpdatedAt: 1,
			PayloadJSON: `{"version":1,"musicId":10,"revision":1,"updatedAt":"1970-01-01T00:00:01Z","lines":[]}`,
		}},
	}
	if err := s.ImportTranslationContent(nil, EventContentExport{}, lyrics); err == nil {
		t.Fatal("invalid public lyrics snapshot unexpectedly restored")
	}
	if _, err := s.GetLyrics(10); err != ErrLyricsNotFound {
		t.Fatalf("failed restore changed lyrics: %v", err)
	}
}

func TestRestoreRejectsDuplicatePublicationJSONKeysWithoutChangingStoredContent(t *testing.T) {
	source := setupLyricsStore(t)
	sourceLyrics, err := source.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.PublishLyrics(sourceLyrics.MusicID, sourceLyrics.Revision); err != nil {
		t.Fatal(err)
	}
	exported, err := source.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Publications) != 1 {
		t.Fatalf("source publications = %+v", exported.Publications)
	}

	tests := []struct {
		name        string
		target      string
		replacement string
	}{
		{name: "top level", target: `"musicId":10`, replacement: `"musicId":10,"musicId":999`},
		{name: "nested line", target: `"japanese":"初音歌う"`, replacement: `"japanese":"初音歌う","japanese":"差し替え"`},
		{name: "object in array", target: `"text":"初音"`, replacement: `"text":"初音","text":"差し替え"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targetStore := setupLyricsStore(t)
			existingInput := validLyrics()
			existingInput.Lines[0].English = "Existing translation must survive"
			existing, err := targetStore.SaveLyrics(existingInput, "editor")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := targetStore.PublishLyrics(existing.MusicID, existing.Revision); err != nil {
				t.Fatal(err)
			}
			var existingPayload string
			if err := targetStore.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, existing.MusicID).Scan(&existingPayload); err != nil {
				t.Fatal(err)
			}

			invalid := exported
			invalid.Publications = append([]LyricsPublicationBackupRecord(nil), exported.Publications...)
			duplicate := strings.Replace(invalid.Publications[0].PayloadJSON, test.target, test.replacement, 1)
			if duplicate == invalid.Publications[0].PayloadJSON {
				t.Fatalf("could not construct duplicate-key payload from %s", invalid.Publications[0].PayloadJSON)
			}
			invalid.Publications[0].PayloadJSON = duplicate
			if err := targetStore.ImportTranslationContent(nil, EventContentExport{}, invalid); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("duplicate-key restore error = %v", err)
			}
			loaded, err := targetStore.GetLyrics(existing.MusicID)
			if err != nil || loaded.Revision != existing.Revision || loaded.Lines[0].English != existing.Lines[0].English {
				t.Fatalf("failed restore changed existing lyrics: %+v err=%v", loaded, err)
			}
			var payloadAfter string
			if err := targetStore.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, existing.MusicID).Scan(&payloadAfter); err != nil {
				t.Fatal(err)
			}
			if payloadAfter != existingPayload {
				t.Fatalf("failed restore changed publication payload\nbefore=%s\nafter=%s", existingPayload, payloadAfter)
			}
		})
	}
}

func TestCatalogAndPublishedLyricsTitlesFollowLocaleEdits(t *testing.T) {
	s := setupLyricsStore(t)
	if _, err := s.ImportCategory("music", model.Category{"title": {
		"新曲": {Text: "中文目录名", Source: model.SourceHuman},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateEntryLocale("music", "title", "新曲", "English Catalog Title", model.SourceHuman, "editor", model.LocaleEnglish); err != nil {
		t.Fatal(err)
	}
	result, err := s.CatalogMusic("", true, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Title.Chinese != "中文目录名" || result.Items[0].Title.English != "English Catalog Title" {
		t.Fatalf("localized catalog = %+v", result)
	}

	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
	index, _, err := s.PublishedLyrics()
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Songs) != 1 || index.Songs[0].Title.Chinese != "中文目录名" || index.Songs[0].Title.English != "English Catalog Title" {
		t.Fatalf("localized public index = %+v", index)
	}
}

func TestPublishedLyricsRejectsWellFormedInvalidStoredPayload(t *testing.T) {
	s := setupLyricsStore(t)
	saved, err := s.SaveLyrics(validLyrics(), "editor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}

	var payload string
	if err := s.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, saved.MusicID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var candidate model.PublicSongLyrics
	if err := json.Unmarshal([]byte(payload), &candidate); err != nil {
		t.Fatal(err)
	}
	candidate.MusicID++
	invalidPayload, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE song_lyrics_publications SET payload_json=? WHERE music_id=?`, string(invalidPayload), saved.MusicID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PublishedLyrics(); err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("well-formed invalid publication error = %v", err)
	}
}

func TestPublishedLyricsRejectsStoredDuplicateJSONKeys(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		replacement string
	}{
		{name: "top level", target: `"musicId":10`, replacement: `"musicId":10,"musicId":999`},
		{name: "nested line", target: `"japanese":"初音歌う"`, replacement: `"japanese":"初音歌う","japanese":"差し替え"`},
		{name: "object in array", target: `"text":"初音"`, replacement: `"text":"初音","text":"差し替え"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := setupLyricsStore(t)
			saved, err := s.SaveLyrics(validLyrics(), "editor")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
				t.Fatal(err)
			}
			var payload string
			if err := s.db.QueryRow(`SELECT payload_json FROM song_lyrics_publications WHERE music_id=?`, saved.MusicID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			duplicate := strings.Replace(payload, test.target, test.replacement, 1)
			if duplicate == payload {
				t.Fatalf("could not construct duplicate-key payload from %s", payload)
			}
			if _, err := s.db.Exec(`UPDATE song_lyrics_publications SET payload_json=? WHERE music_id=?`, duplicate, saved.MusicID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.PublishedLyrics(); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("duplicate-key publication error = %v", err)
			}
		})
	}
}

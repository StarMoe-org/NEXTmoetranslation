package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type lyricsPublicationRowForTest struct {
	revision  int
	updatedAt int64
	payload   string
}

func lyricsPublicationRowsForTest(t *testing.T, s *Store) map[int]lyricsPublicationRowForTest {
	t.Helper()
	rows, err := s.db.Query(`SELECT music_id,revision,updated_at,payload_json FROM song_lyrics_publications`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[int]lyricsPublicationRowForTest{}
	for rows.Next() {
		var musicID int
		var row lyricsPublicationRowForTest
		if err := rows.Scan(&musicID, &row.revision, &row.updatedAt, &row.payload); err != nil {
			t.Fatal(err)
		}
		result[musicID] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// sourceOnlyV1PublicationStore holds a v1 publication written by the source-only
// rule that preceded the served-attribution requirement: no credit, and a
// fandom source URL that yields no served attribution.
func sourceOnlyV1PublicationStore(t *testing.T) *Store {
	t.Helper()
	s := setupLyricsStore(t)
	input := validLyrics()
	input.Attribution, input.TranslationCredit, input.ProofreadingCredit = "", "", ""
	input.SourceURL, input.SourcePageID, input.SourceRevisionID = "https://utaite.fandom.com/wiki/Song?oldid=3", 10, 3
	input.SourceSHA1, input.SourceFetchedAt = validSourceSHA1, "2026-07-22T12:00:00Z"
	saved, _, err := s.SaveLyricsMutation(input, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var contractErr *LyricsContractError
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); !errors.As(err, &contractErr) ||
		contractErr.Code != "incomplete_publication" {
		t.Fatalf("publishing today err=%v, want incomplete_publication", err)
	}
	payload, err := json.Marshal(publicLyricsV1(saved))
	if err != nil {
		t.Fatal(err)
	}
	updatedAt, err := parseTimestamp(saved.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO song_lyrics_publications(music_id,revision,updated_at,payload_json) VALUES (?,?,?,?)`,
		saved.MusicID, saved.Revision, updatedAt, string(payload)); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRestoreKeepsASourceOnlyV1PublicationItsPublishRuleAdmitted(t *testing.T) {
	s := sourceOnlyV1PublicationStore(t)
	before := lyricsPublicationRowsForTest(t, s)
	if len(before) != 1 || !strings.Contains(before[10].payload, `"sourceUrl":"https://utaite.fandom.com/wiki/Song?oldid=3"`) {
		t.Fatalf("fixture publications=%+v", before)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	destination := restoreSeededContentBackup(t, exported)
	after := lyricsPublicationRowsForTest(t, destination)
	if len(after) != 1 || after[10] != before[10] {
		t.Fatalf("restored publications=%+v want %+v", after, before)
	}
	if _, details, err := destination.PublishedLyrics(); err != nil || details[10].MusicID != 10 {
		t.Fatalf("projection of the restored publication err=%v", err)
	}
}

func TestRestoreStillRejectsAV1PublicationWithoutCreditOrSourceAttribution(t *testing.T) {
	s := sourceOnlyV1PublicationStore(t)
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	tampered := cloneLyricsContentExport(t, exported)
	payload := tampered.Publications[0].PayloadJSON
	tampered.Publications[0].PayloadJSON = strings.Replace(payload, `"sourceSha1":"`+validSourceSHA1+`",`, "", 1)
	if tampered.Publications[0].PayloadJSON == payload {
		t.Fatalf("fixture payload lacks sourceSha1: %s", payload)
	}
	if _, err := restoreLyricsBackupIntoFreshStore(t, tampered); err == nil ||
		!strings.Contains(err.Error(), "violates incomplete_publication") {
		t.Fatalf("restore error=%v", err)
	}
}

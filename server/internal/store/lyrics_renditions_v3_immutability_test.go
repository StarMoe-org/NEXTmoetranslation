package store

import (
	"strings"
	"testing"
)

// The source-v3 editor drops the source-document and contribution immutability
// triggers to perform its own write. Migration v27 installs both once, so the
// mutation must leave them in place for every later writer.
func TestSaveLyricsRenditionMutationKeepsSourceImmutabilityTriggers(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	current, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	requested := cloneLyricsRenditionEditorDocument(t, current)
	if requested.Renditions[0].Full == nil || len(requested.Renditions[0].Full.Lines) == 0 {
		t.Fatalf("fixture rendition has no editable full side: %+v", requested.Renditions[0])
	}
	line := &requested.Renditions[0].Full.Lines[0]
	line.StanzaBreakBefore = !line.StanzaBreakBefore

	var documentSHA string
	if err := s.db.QueryRow(`SELECT document_sha256 FROM song_lyrics_source_documents WHERE music_id=10`).Scan(&documentSHA); err != nil {
		t.Fatal(err)
	}
	saved, changed, err := s.SaveLyricsRenditionMutation(requested, "editor")
	if err != nil || !changed || saved.Revision != current.Revision+1 {
		t.Fatalf("save revision=%d changed=%t err=%v", saved.Revision, changed, err)
	}
	var mutatedSHA string
	if err := s.db.QueryRow(`SELECT document_sha256 FROM song_lyrics_source_documents WHERE music_id=10`).Scan(&mutatedSHA); err != nil {
		t.Fatal(err)
	}
	if mutatedSHA == documentSHA {
		t.Fatal("source document was not rewritten, so the trigger drop was never exercised")
	}

	for _, trigger := range []string{
		"song_lyrics_source_documents_immutable_update",
		"song_lyrics_component_contributions_immutable_update",
	} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`, trigger).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("trigger %q count=%d after rendition mutation", trigger, count)
		}
	}

	if _, err := s.db.Exec(`UPDATE song_lyrics_source_documents SET manifest_batch_sha256=? WHERE music_id=10`,
		strings.Repeat("c", 64)); err == nil ||
		!strings.Contains(err.Error(), "song lyrics source documents are immutable") {
		t.Fatalf("post-mutation source document update error=%v", err)
	}
	if _, err := s.db.Exec(`UPDATE song_lyrics_component_contributions SET contribution_sha256=?
		WHERE document_id=(SELECT document_id FROM song_lyrics_source_documents WHERE music_id=10)`,
		strings.Repeat("d", 64)); err == nil ||
		!strings.Contains(err.Error(), "song lyrics component contributions are immutable") {
		t.Fatalf("post-mutation contribution update error=%v", err)
	}

	next, err := s.GetLyricsRenditionDocument(10)
	if err != nil {
		t.Fatal(err)
	}
	again := cloneLyricsRenditionEditorDocument(t, next)
	againLine := &again.Renditions[0].Full.Lines[0]
	againLine.StanzaBreakBefore = !againLine.StanzaBreakBefore
	if _, changed, err := s.SaveLyricsRenditionMutation(again, "editor"); err != nil || !changed {
		t.Fatalf("second source mutation changed=%t err=%v", changed, err)
	}
}

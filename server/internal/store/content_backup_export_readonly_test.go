package store

import (
	"fmt"
	"testing"
)

func artifactEvidenceLinkRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT document_id,rendition_key,position,provider,evidence_id,sha256
		FROM song_lyrics_source_artifact_index_evidence
		ORDER BY document_id,rendition_key,position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var documentID int64
		var renditionKey, provider, evidenceID, sha string
		var position int
		if err := rows.Scan(&documentID, &renditionKey, &position, &provider, &evidenceID, &sha); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%d|%s|%s|%s", documentID, renditionKey, position, provider, evidenceID, sha))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Content export opens its transaction read-only and every caller documents it
// as a snapshot read, so a missing artifact evidence link must be completed in
// the exported result without being written back to the exported database.
func TestExportLyricsContentDoesNotWriteArtifactEvidenceLinks(t *testing.T) {
	s, _ := setupRenditionV3PersistenceStore(t)
	complete, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	if len(complete.SourceArtifactEvidence) == 0 {
		t.Fatal("fixture has no artifact evidence links")
	}

	if _, err := s.db.Exec(`DROP TRIGGER song_lyrics_source_artifact_index_evidence_immutable_delete`); err != nil {
		t.Fatal(err)
	}
	removed := complete.SourceArtifactEvidence[len(complete.SourceArtifactEvidence)-1]
	if _, err := s.db.Exec(`DELETE FROM song_lyrics_source_artifact_index_evidence
		WHERE document_id=? AND rendition_key=? AND position=?`,
		removed.DocumentID, removed.RenditionKey, removed.Position); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER song_lyrics_source_artifact_index_evidence_immutable_delete
		BEFORE DELETE ON song_lyrics_source_artifact_index_evidence
		WHEN EXISTS (SELECT 1 FROM song_lyrics_source_artifacts
		             WHERE document_id=OLD.document_id AND rendition_key=OLD.rendition_key)
		BEGIN SELECT RAISE(ABORT, 'song lyrics source artifact index evidence is immutable'); END`); err != nil {
		t.Fatal(err)
	}

	before := artifactEvidenceLinkRows(t, s)
	if len(before) != len(complete.SourceArtifactEvidence)-1 {
		t.Fatalf("link rows before export=%d want=%d", len(before), len(complete.SourceArtifactEvidence)-1)
	}

	repaired, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export with a missing evidence link: %v", err)
	}
	if len(repaired.SourceArtifactEvidence) != len(complete.SourceArtifactEvidence) {
		t.Fatalf("exported links=%d want=%d", len(repaired.SourceArtifactEvidence), len(complete.SourceArtifactEvidence))
	}

	after := artifactEvidenceLinkRows(t, s)
	if len(after) != len(before) {
		t.Fatalf("export wrote %d artifact evidence link rows into the exported database", len(after)-len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Fatalf("export changed artifact evidence link row %d: before=%q after=%q", i, before[i], after[i])
		}
	}
}

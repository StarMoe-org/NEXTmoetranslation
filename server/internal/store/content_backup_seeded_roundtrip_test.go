package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/db"
	"moesekai/server/internal/embeddedlyricsseed"
	"moesekai/server/internal/model"
)

func seededContentBackupStore(t *testing.T) (*Store, embeddedlyricsseed.Bundle) {
	t.Helper()
	bundle, err := embeddedlyricsseed.Load()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "seeded-source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := New(database)
	// Catalog rows carry evidence that reproduces their fingerprint, as the
	// catalog sync writes them; restore recomputes it.
	records := make([]MusicCatalogRecord, 0, len(bundle.Manifest.Items))
	for _, item := range bundle.Manifest.Items {
		records = append(records, MusicCatalogRecord{MusicID: item.MusicID, JapaneseTitle: item.JapaneseTitle})
	}
	if err := s.UpsertMusicCatalog(records); err != nil {
		t.Fatal(err)
	}
	seedEmbeddedLyricsEditorLegacyPerformers(t, database, bundle)
	if _, err := s.ApplyEmbeddedLyricsEditorSeed(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	return s, bundle
}

// restoreSeededContentBackup restores the JSON form of exported into a fresh
// database and requires the next export to be byte-identical.
func restoreSeededContentBackup(t *testing.T, exported LyricsContentExport) *Store {
	t.Helper()
	body, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LyricsContentExport
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "seeded-destination.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	destination := New(database)
	if err := destination.RestoreBackupContext(context.Background(), restoreContractCategories(), nil, nil,
		EventContentExport{}, decoded, true, "operator"); err != nil {
		t.Fatalf("restore seeded content backup: %v", err)
	}
	second, err := destination.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export restored seeded content: %v", err)
	}
	secondBody, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, secondBody) {
		t.Fatalf("second export differs: first=%d bytes second=%d bytes", len(body), len(secondBody))
	}
	return destination
}

func TestSeededDatabaseContentBackupRoundTripsWithoutParentEvidence(t *testing.T) {
	s, _ := seededContentBackupStore(t)
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export seeded database: %v", err)
	}
	if len(exported.SourceDocuments) != embeddedlyricsseed.ExpectedSourceV3 || len(exported.SourceArtifacts) == 0 ||
		len(exported.SourceIndexEvidence) != 0 || len(exported.SourceArtifactEvidence) != 0 ||
		len(exported.Music) != embeddedlyricsseed.ExpectedCatalogCount {
		t.Fatalf("seeded export documents=%d artifacts=%d parents=%d links=%d music=%d", len(exported.SourceDocuments),
			len(exported.SourceArtifacts), len(exported.SourceIndexEvidence), len(exported.SourceArtifactEvidence), len(exported.Music))
	}
	destination := restoreSeededContentBackup(t, exported)
	var links, parents int
	if err := destination.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
		(SELECT COUNT(*) FROM lyrics_source_index_evidence)`).Scan(&links, &parents); err != nil {
		t.Fatal(err)
	}
	if links != 0 || parents != 0 {
		t.Fatalf("restored seeded database links=%d parents=%d", links, parents)
	}
}

func TestRoutePublishedSongInSeededDatabaseContentBackupRoundTrips(t *testing.T) {
	s, bundle := seededContentBackupStore(t)
	var item embeddedlyricsseed.CatalogItem
	for _, candidate := range bundle.Manifest.Items {
		if candidate.SeedKind == "source_v3" {
			item = candidate
			break
		}
	}
	request := lyricsDocumentTestRequest()
	request.MusicID = item.MusicID
	request.Source.URL = "https://www.sekaipedia.org/wiki/%E5%90%88%E6%88%90?oldid=7310"
	request.Renditions[0].PerformerIDs = []int{1}
	request.ExpectedRevision = lyricsDocumentRequiredRevisionForTest(t, s, request, 3)
	result, err := s.PublishLyricsDocument(context.Background(), request, 3, "document-admin")
	if err != nil {
		t.Fatalf("publish over seeded music %d: %v", item.MusicID, err)
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export seeded database with a route-published song: %v", err)
	}
	destination := restoreSeededContentBackup(t, exported)
	document, err := destination.GetLyricsRenditionDocument(item.MusicID)
	if err != nil || document.Revision != result.Revision || len(document.Renditions) != 1 ||
		document.Renditions[0].Full.Lines[0].Japanese != "試験の歌をうたう" {
		t.Fatalf("restored route-published music %d=%+v err=%v", item.MusicID, document, err)
	}
}

func TestContentBackupParentEvidenceWithOtherBytes(t *testing.T) {
	t.Run("restore rejects a link whose parent bytes changed", func(t *testing.T) {
		_, valid := setupContentBackupEvidenceGraph(t)
		invalid := cloneLyricsContentExport(t, valid)
		replaced := false
		for index, parent := range invalid.SourceIndexEvidence {
			if parent.Provider == string(model.LyricsSourceProviderSekaipedia) {
				continue
			}
			parent.RawBytes = append(append([]byte(nil), parent.RawBytes...), []byte("-SYNTHETIC-MISMATCH")...)
			digest := sha256.Sum256(parent.RawBytes)
			parent.RawByteCount = len(parent.RawBytes)
			parent.SHA256 = hex.EncodeToString(digest[:])
			parent.RawSHA256 = parent.SHA256
			invalid.SourceIndexEvidence[index] = parent
			replaced = true
			break
		}
		if !replaced {
			t.Fatal("evidence graph fixture has no non-Sekaipedia parent")
		}
		destination := setupLyricsStore(t)
		err := destination.ImportTranslationContent(nil, EventContentExport{}, invalid)
		if err == nil || !strings.Contains(err.Error(), "no exact parent evidence") {
			t.Fatalf("restore error=%v", err)
		}
		var documents, links, parents int
		if err := destination.db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM song_lyrics_source_documents),
			(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
			(SELECT COUNT(*) FROM lyrics_source_index_evidence)`).Scan(&documents, &links, &parents); err != nil {
			t.Fatal(err)
		}
		if documents != 0 || links != 0 || parents != 0 {
			t.Fatalf("failed restore wrote documents=%d links=%d parents=%d", documents, links, parents)
		}
	})
	// A database may hold a seed artifact whose reference names an evidence id
	// stored with other bytes. The live graph permits that (the seed never
	// links its references), so the backup must copy it unlinked.
	t.Run("export leaves a seed reference unlinked when the stored parent has other bytes", func(t *testing.T) {
		s, _ := seededContentBackupStore(t)
		exported, err := s.ExportLyricsContent()
		if err != nil {
			t.Fatal(err)
		}
		var artifact LyricsSourceArtifactBackupRecord
		for _, candidate := range exported.SourceArtifacts {
			if candidate.Provider == string(model.LyricsSourceProviderSekaipedia) {
				artifact = candidate
				break
			}
		}
		if artifact.DocumentID == 0 {
			t.Fatal("seeded export has no Sekaipedia artifact")
		}
		identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(artifact.FixedIdentityJSON))
		if err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"query":{"pages":[{"revisions":[{"timestamp":"` + artifact.RevisionTimestamp + `"}]}]}}`)
		digest := sha256.Sum256(raw)
		rawSHA256 := hex.EncodeToString(digest[:])
		if rawSHA256 == identity.IndexEvidenceRefs[0].SHA256 {
			t.Fatal("synthetic parent unexpectedly matches the reference")
		}
		if _, err := s.db.Exec(`INSERT INTO lyrics_source_index_evidence
			(provider,evidence_id,sha256,kind,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
			 canonical_revision_url,categories_json,canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at)
			VALUES (?,?,?,'mediawiki_revision',?,?,?,?,?,?,?,'[]','',?,?,?,?,1)`,
			artifact.Provider, identity.IndexEvidenceRefs[0].EvidenceID, rawSHA256, artifact.Origin, artifact.PageID,
			artifact.RevisionID, artifact.RevisionTimestamp, artifact.MediaWikiSHA1, artifact.PageTitle,
			artifact.CanonicalRevisionURL, artifact.FetchedAt, raw, len(raw), rawSHA256); err != nil {
			t.Fatal(err)
		}
		withParent, err := s.ExportLyricsContent()
		if err != nil {
			t.Fatalf("export with a same-id parent of other bytes: %v", err)
		}
		for _, link := range withParent.SourceArtifactEvidence {
			if link.DocumentID == artifact.DocumentID && link.RenditionKey == artifact.RenditionKey && link.Position == 0 {
				t.Fatalf("export linked the reference to a parent with other bytes: %+v", link)
			}
		}
		for _, parent := range withParent.SourceIndexEvidence {
			if parent.EvidenceID == identity.IndexEvidenceRefs[0].EvidenceID {
				t.Fatalf("export carried the unreferenced parent %s", parent.EvidenceID)
			}
		}
		exportedBody, err := json.Marshal(exported)
		if err != nil {
			t.Fatal(err)
		}
		withParentBody, err := json.Marshal(withParent)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(exportedBody, withParentBody) {
			t.Fatal("an unreferenced parent row changed the export")
		}
		restoreSeededContentBackup(t, withParent)
	})
}

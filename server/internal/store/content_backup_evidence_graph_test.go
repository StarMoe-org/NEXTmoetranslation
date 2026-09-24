package store

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestContentBackupEvidenceGraphRoundTripIsDeterministicPrivateAndV1Readable(t *testing.T) {
	t.Run("source-backed v2", func(t *testing.T) {
		source, first := setupContentBackupEvidenceGraph(t)
		privateCandidate := testRevisionCandidate(model.LyricsSourceProviderVocaloidFandom, 901, 902,
			"Private Backup Evidence", []string{"Lyrics"}, "Lyrics", "full-vocaloid",
			model.LyricsSourceVersionReasonUntaggedFullOnly, []byte("PRIVATE-UNRELATED-EVIDENCE"))
		privateEvidence := privateCandidate.IndexEvidence[0]
		privateCreatedAt := time.Date(2026, time.July, 31, 16, 0, 0, 0, time.UTC)
		sourceTx, err := source.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertOrVerifyLyricsIndexEvidenceTx(context.Background(), sourceTx, privateEvidence, privateCreatedAt); err != nil {
			sourceTx.Rollback()
			t.Fatal(err)
		}
		if err := sourceTx.Commit(); err != nil {
			t.Fatal(err)
		}
		second, err := source.ExportLyricsContent()
		if err != nil {
			t.Fatal(err)
		}
		firstJSON, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		secondJSON, err := json.Marshal(second)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstJSON, secondJSON) {
			t.Fatalf("repeated content exports differ\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
		}
		parentField := bytes.Index(firstJSON, []byte(`"sourceIndexEvidence"`))
		linkField := bytes.Index(firstJSON, []byte(`"sourceArtifactEvidence"`))
		if parentField < 0 || linkField < 0 || parentField >= linkField {
			t.Fatalf("parent evidence is not durably ordered before links: %s", firstJSON)
		}
		expectedEvidenceLinks := 0
		for _, artifact := range first.SourceArtifacts {
			var refs []model.LyricsSourceIndexEvidenceRef
			if err := json.Unmarshal([]byte(artifact.IndexEvidenceRefsJSON), &refs); err != nil {
				t.Fatal(err)
			}
			expectedEvidenceLinks += len(refs)
		}
		if len(first.SourceIndexEvidence) != 3 || len(first.SourceArtifactEvidence) != expectedEvidenceLinks ||
			len(first.Publications) != 1 || !strings.Contains(first.Publications[0].PayloadJSON, `"version":2`) ||
			len(first.Documents) != 1 || first.Documents[0].TranslationCredit != "Same Person" ||
			first.Documents[0].ProofreadingCredit != "Same Person" {
			t.Fatalf("source-backed export graph=%+v links=%+v documents=%+v publications=%+v",
				first.SourceIndexEvidence, first.SourceArtifactEvidence, first.Documents, first.Publications)
		}
		linkedRaw := string(first.SourceIndexEvidence[0].RawBytes)
		if linkedRaw == "" {
			t.Fatal("linked parent raw evidence is empty")
		}
		if len(first.SourceDocuments) != 1 ||
			!strings.Contains(first.SourceDocuments[0].DocumentJSON, `"privateReview"`) ||
			!strings.Contains(first.SourceDocuments[0].DocumentJSON, `"revisionTimestamp"`) {
			t.Fatalf("private canonical source document was not retained: %+v", first.SourceDocuments)
		}
		foundSekaipediaIdentity := false
		for _, artifact := range first.SourceArtifacts {
			if artifact.Provider == string(model.LyricsSourceProviderSekaipedia) {
				foundSekaipediaIdentity = artifact.RevisionTimestamp != "" &&
					artifact.CompositionRenditionKey == "full-vocaloid" &&
					artifact.VersionReason == string(model.LyricsSourceVersionReasonUntaggedFullOnly) &&
					strings.Contains(artifact.FixedIdentityJSON, `"revisionTimestamp"`) &&
					strings.Contains(artifact.FixedIdentityJSON, `"compositionRenditionKey"`) &&
					strings.Contains(artifact.FixedIdentityJSON, `"versionReason"`)
			}
		}
		foundSekaipediaEvidenceTimestamp := false
		for _, parent := range first.SourceIndexEvidence {
			if parent.Provider == string(model.LyricsSourceProviderSekaipedia) && parent.RevisionTimestamp != "" {
				foundSekaipediaEvidenceTimestamp = true
			}
		}
		if !foundSekaipediaIdentity || !foundSekaipediaEvidenceTimestamp {
			t.Fatalf("Sekaipedia identity/evidence timestamp was not retained: artifacts=%+v evidence=%+v",
				first.SourceArtifacts, first.SourceIndexEvidence)
		}

		destination := setupLyricsStore(t)
		tx, err := destination.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertOrVerifyLyricsIndexEvidenceTx(context.Background(), tx, privateEvidence, privateCreatedAt); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		if err := destination.ImportTranslationContent(nil, EventContentExport{}, first); err != nil {
			t.Fatalf("restore source-backed content: %v", err)
		}
		var parentCount, linkCount int
		var preservedPrivateRaw []byte
		if err := destination.db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM lyrics_source_index_evidence),
			(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence),
			(SELECT raw_bytes FROM lyrics_source_index_evidence WHERE provider=? AND evidence_id=?)`,
			privateEvidence.Provider, privateEvidence.EvidenceID).Scan(&parentCount, &linkCount, &preservedPrivateRaw); err != nil {
			t.Fatal(err)
		}
		if parentCount != len(first.SourceIndexEvidence)+1 || linkCount != len(first.SourceArtifactEvidence) ||
			string(preservedPrivateRaw) != string(privateEvidence.Raw) {
			t.Fatalf("restored parents=%d links=%d privateRaw=%q", parentCount, linkCount, preservedPrivateRaw)
		}
		roundTripped, err := destination.ExportLyricsContent()
		if err != nil {
			t.Fatal(err)
		}
		roundTrippedJSON, err := json.Marshal(roundTripped)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstJSON, roundTrippedJSON) {
			t.Fatalf("source bundle restore was not byte-stable\nwant=%s\ngot=%s", firstJSON, roundTrippedJSON)
		}
		loaded, err := destination.GetLyrics(first.Documents[0].MusicID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.TranslationCredit != "Same Person" || loaded.ProofreadingCredit != "Same Person" {
			t.Fatalf("restored private credits translation=%q proofreading=%q", loaded.TranslationCredit, loaded.ProofreadingCredit)
		}
		index, details, err := destination.PublishedLyrics()
		if err != nil || index.Version != 2 || len(index.Songs) != 1 || len(details) != 1 {
			t.Fatalf("restored public v2 index=%+v details=%+v err=%v", index, details, err)
		}
		if credits := details[first.Documents[0].MusicID].TranslationCredits; credits == nil ||
			credits.Translation != "Same Person" || credits.Proofreading != "Same Person" {
			t.Fatalf("restored public credits=%+v", credits)
		}
		publicJSON, err := json.Marshal(struct {
			Index   PublicLyricsIndexDocument          `json:"index"`
			Details map[int]PublicLyricsDetailDocument `json:"details"`
		}{Index: index, Details: details})
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{linkedRaw, string(privateEvidence.Raw), `"rawBytes"`, `"indexEvidenceRefs"`,
			`"documentJson"`, `"fixedIdentityJson"`, `"sourceUrl"`, `"sourceSha1"`, `"privateReview"`,
			`"revisionTimestamp"`, `"compositionRenditionKey"`, `"versionReason"`, "romaji", "romanization"} {
			if strings.Contains(string(publicJSON), forbidden) {
				t.Fatalf("public projection leaked %q: %s", forbidden, publicJSON)
			}
		}
	})

	t.Run("legacy v1", func(t *testing.T) {
		source := setupLyricsStore(t)
		saved, err := source.SaveLyrics(validLyrics(), "backup-test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
			t.Fatal(err)
		}
		exported, err := source.ExportLyricsContent()
		if err != nil {
			t.Fatal(err)
		}
		exportedJSON, err := json.Marshal(exported)
		if err != nil {
			t.Fatal(err)
		}
		if len(exported.SourceIndexEvidence) != 0 || len(exported.SourceArtifactEvidence) != 0 ||
			len(exported.Publications) != 1 || !strings.Contains(exported.Publications[0].PayloadJSON, `"version":1`) ||
			bytes.Contains(exportedJSON, []byte(`"translationCredit"`)) ||
			bytes.Contains(exportedJSON, []byte(`"proofreadingCredit"`)) {
			t.Fatalf("legacy export=%+v JSON=%s", exported, exportedJSON)
		}
		destination := setupLyricsStore(t)
		if err := destination.ImportTranslationContent(nil, EventContentExport{}, exported); err != nil {
			t.Fatal(err)
		}
		loaded, err := destination.GetLyrics(saved.MusicID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Attribution != saved.Attribution || loaded.TranslationCredit != "" || loaded.ProofreadingCredit != "" {
			t.Fatalf("restored legacy credits=%+v", loaded)
		}
		index, details, err := destination.PublishedLyrics()
		if err != nil || index.Version != 1 || len(index.Songs) != 1 || details[saved.MusicID].Version != 1 {
			t.Fatalf("restored v1 index=%+v detail=%+v err=%v", index, details[saved.MusicID], err)
		}
	})
}

func TestContentBackupRoundTripPreservesLegacyUnsuffixedRevisionEvidenceIDs(t *testing.T) {
	_, current := setupContentBackupEvidenceGraph(t)
	legacy := cloneLyricsContentExport(t, current)
	oldID, legacyID := rewriteContentBackupEvidenceIDAsLegacy(t, &legacy, model.LyricsSourceProviderVocaloidFandom)
	if oldID == "" || legacyID == "" || oldID == legacyID {
		t.Fatalf("legacy rewrite old=%q legacy=%q", oldID, legacyID)
	}
	var legacyRecord LyricsSourceIndexEvidenceBackupRecord
	for _, record := range legacy.SourceIndexEvidence {
		if record.Provider == string(model.LyricsSourceProviderVocaloidFandom) {
			legacyRecord = record
			break
		}
	}
	evidence, err := lyricsSourceIndexEvidenceFromBackupRecord(legacyRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := lyricssource.ValidateIndexEvidenceEnvelope(evidence); err == nil {
		t.Fatal("strict live evidence validation accepted a historical unsuffixed ID")
	}

	destination := setupLyricsStore(t)
	if err := destination.ImportTranslationContent(nil, EventContentExport{}, legacy); err != nil {
		t.Fatalf("restore legacy evidence graph: %v", err)
	}
	roundTripped, err := destination.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(roundTripped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy evidence graph was not byte-stable\nwant=%s\ngot=%s", want, got)
	}

	for name, invalid := range map[string]LyricsContentExport{
		"arbitrary ID": func() LyricsContentExport {
			value := cloneLyricsContentExport(t, legacy)
			rewriteContentBackupEvidenceID(t, &value, model.LyricsSourceProviderVocaloidFandom, legacyID, legacyID+":invalid")
			return value
		}(),
		"fetchedAt drift": func() LyricsContentExport {
			value := cloneLyricsContentExport(t, legacy)
			for index := range value.SourceIndexEvidence {
				if value.SourceIndexEvidence[index].Provider == string(model.LyricsSourceProviderVocaloidFandom) {
					value.SourceIndexEvidence[index].FetchedAt = "2026-07-31T00:00:00.000000001Z"
				}
			}
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			rejected := setupLyricsStore(t)
			if err := rejected.ImportTranslationContent(nil, EventContentExport{}, invalid); err == nil {
				t.Fatal("restore accepted invalid historical evidence")
			}
			var documents int
			if err := rejected.db.QueryRow(`SELECT COUNT(*) FROM song_lyrics_source_documents`).Scan(&documents); err != nil {
				t.Fatal(err)
			}
			if documents != 0 {
				t.Fatalf("failed legacy restore wrote %d source documents", documents)
			}
		})
	}
}

func TestContentBackupEvidenceGraphRejectsMissingDuplicateConflictingOrphanAndProviderMismatch(t *testing.T) {
	_, valid := setupContentBackupEvidenceGraph(t)
	orphanCandidate := testRevisionCandidate(model.LyricsSourceProviderVocaloidFandom, 911, 912,
		"Orphan Backup Evidence", []string{"Lyrics"}, "Lyrics", "full-vocaloid",
		model.LyricsSourceVersionReasonUntaggedFullOnly, []byte("ORPHAN-EVIDENCE"))
	orphan := contentBackupIndexEvidenceRecord(orphanCandidate.IndexEvidence[0], valid.SourceIndexEvidence[0].CreatedAt+1)

	tests := []struct {
		name string
		want string
		edit func(*LyricsContentExport)
	}{
		{name: "missing parent", want: "no exact parent evidence", edit: func(content *LyricsContentExport) {
			content.SourceIndexEvidence = nil
		}},
		{name: "duplicate parent", want: "duplicated", edit: func(content *LyricsContentExport) {
			content.SourceIndexEvidence = append(content.SourceIndexEvidence, content.SourceIndexEvidence[0])
		}},
		{name: "conflicting immutable parent", want: "conflicting immutable rows", edit: func(content *LyricsContentExport) {
			conflicting := content.SourceIndexEvidence[0]
			conflicting.CreatedAt++
			content.SourceIndexEvidence = append(content.SourceIndexEvidence, conflicting)
		}},
		{name: "orphan parent", want: "orphan parent evidence", edit: func(content *LyricsContentExport) {
			content.SourceIndexEvidence = append(content.SourceIndexEvidence, orphan)
		}},
		{name: "missing link", want: "incomplete evidence links", edit: func(content *LyricsContentExport) {
			content.SourceArtifactEvidence = nil
		}},
		{name: "duplicate link", want: "duplicated", edit: func(content *LyricsContentExport) {
			content.SourceArtifactEvidence = append(content.SourceArtifactEvidence, content.SourceArtifactEvidence[0])
		}},
		{name: "provider mismatch", want: "provider is invalid", edit: func(content *LyricsContentExport) {
			content.SourceArtifactEvidence[0].Provider = string(model.LyricsSourceProviderMoegirl)
		}},
		{name: "artifact evidence refs mismatch", want: "durable identity fields are not canonical", edit: func(content *LyricsContentExport) {
			content.SourceArtifacts[0].IndexEvidenceRefsJSON = "[]"
		}},
		{name: "missing source document", want: "unknown document", edit: func(content *LyricsContentExport) {
			content.SourceDocuments = nil
		}},
		{name: "duplicate source document", want: "invalid or duplicate identity", edit: func(content *LyricsContentExport) {
			content.SourceDocuments = append(content.SourceDocuments, content.SourceDocuments[0])
		}},
		{name: "missing source artifact", want: "incomplete provenance", edit: func(content *LyricsContentExport) {
			removed := content.SourceArtifacts[0]
			content.SourceArtifacts = content.SourceArtifacts[1:]
			links := content.SourceArtifactEvidence[:0]
			for _, link := range content.SourceArtifactEvidence {
				if link.DocumentID != removed.DocumentID || link.RenditionKey != removed.RenditionKey {
					links = append(links, link)
				}
			}
			content.SourceArtifactEvidence = links
			contributions := content.SourceContributions[:0]
			for _, contribution := range content.SourceContributions {
				if contribution.DocumentID != removed.DocumentID || contribution.RenditionKey != removed.RenditionKey {
					contributions = append(contributions, contribution)
				}
			}
			content.SourceContributions = contributions
			parents := content.SourceIndexEvidence[:0]
			for _, parent := range content.SourceIndexEvidence {
				keep := false
				for _, link := range content.SourceArtifactEvidence {
					if link.Provider == parent.Provider && link.EvidenceID == parent.EvidenceID && link.SHA256 == parent.SHA256 {
						keep = true
					}
				}
				if keep {
					parents = append(parents, parent)
				}
			}
			content.SourceIndexEvidence = parents
		}},
		{name: "duplicate source artifact", want: "duplicated", edit: func(content *LyricsContentExport) {
			content.SourceArtifacts = append(content.SourceArtifacts, content.SourceArtifacts[0])
		}},
		{name: "orphan source artifact", want: "unknown document", edit: func(content *LyricsContentExport) {
			content.SourceArtifacts[0].DocumentID++
		}},
		{name: "source artifact provider mismatch", want: "fixed identity is invalid", edit: func(content *LyricsContentExport) {
			content.SourceArtifacts[0].Provider = string(model.LyricsSourceProviderMoegirl)
		}},
		{name: "stale scalar identity", want: "fixed identity is invalid", edit: func(content *LyricsContentExport) {
			content.SourceArtifacts[0].PageTitle = "stale scalar title"
		}},
		{name: "stale revisionTimestamp scalar", want: "fixed identity is invalid", edit: func(content *LyricsContentExport) {
			for index := range content.SourceArtifacts {
				if content.SourceArtifacts[index].Provider == string(model.LyricsSourceProviderSekaipedia) {
					content.SourceArtifacts[index].RevisionTimestamp = "2026-07-30T23:59:58Z"
				}
			}
		}},
		{name: "stale compositionRenditionKey scalar", want: "fixed identity is invalid", edit: func(content *LyricsContentExport) {
			for index := range content.SourceArtifacts {
				if content.SourceArtifacts[index].Provider == string(model.LyricsSourceProviderSekaipedia) {
					content.SourceArtifacts[index].CompositionRenditionKey = "other-vocaloid"
				}
			}
		}},
		{name: "stale versionReason scalar", want: "fixed identity is invalid", edit: func(content *LyricsContentExport) {
			for index := range content.SourceArtifacts {
				if content.SourceArtifacts[index].Provider == string(model.LyricsSourceProviderSekaipedia) {
					content.SourceArtifacts[index].VersionReason = string(model.LyricsSourceVersionReasonTaggedFullAndGame)
				}
			}
		}},
		{name: "stale JSON-only identity", want: "durable identity fields are not canonical", edit: func(content *LyricsContentExport) {
			identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(content.SourceArtifacts[0].FixedIdentityJSON))
			if err != nil {
				t.Fatal(err)
			}
			identity.CompositionRenditionKey = "other-vocaloid"
			content.SourceArtifacts[0].CompositionRenditionKey = identity.CompositionRenditionKey
			body, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(body)
			content.SourceArtifacts[0].FixedIdentityJSON = string(body)
			content.SourceArtifacts[0].FixedIdentitySHA256 = hex.EncodeToString(digest[:])
		}},
		{name: "fixed identity checksum mismatch", want: "checksum is invalid", edit: func(content *LyricsContentExport) {
			content.SourceArtifacts[0].FixedIdentitySHA256 = strings.Repeat("0", 64)
		}},
		{name: "missing contribution", want: "incomplete provenance", edit: func(content *LyricsContentExport) {
			content.SourceContributions = nil
		}},
		{name: "duplicate contribution", want: "duplicated", edit: func(content *LyricsContentExport) {
			content.SourceContributions = append(content.SourceContributions, content.SourceContributions[0])
		}},
		{name: "component contribution mismatch", want: "checksum is invalid", edit: func(content *LyricsContentExport) {
			content.SourceContributions[0].Component = "ruby"
		}},
		{name: "evidence link order mismatch", want: "is invalid", edit: func(content *LyricsContentExport) {
			content.SourceArtifactEvidence[0].Position = 1
		}},
		{name: "private field in public v2", want: "unknown field rawBytes", edit: func(content *LyricsContentExport) {
			content.Publications[0].PayloadJSON = strings.TrimSuffix(content.Publications[0].PayloadJSON, "}") + `,"rawBytes":"PRIVATE"}`
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := cloneLyricsContentExport(t, valid)
			test.edit(&invalid)
			destination := setupLyricsStore(t)
			err := destination.ImportTranslationContent(nil, EventContentExport{}, invalid)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restore error=%v want substring %q", err, test.want)
			}
			var documents, links int
			if queryErr := destination.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM song_lyrics_source_documents),
				(SELECT COUNT(*) FROM song_lyrics_source_artifact_index_evidence)`).Scan(&documents, &links); queryErr != nil {
				t.Fatal(queryErr)
			}
			if documents != 0 || links != 0 {
				t.Fatalf("failed restore wrote documents=%d links=%d", documents, links)
			}
		})
	}
}

func TestContentBackupEvidenceGraphRejectsExistingImmutableParentConflictWithoutChangingPrivateRow(t *testing.T) {
	_, exported := setupContentBackupEvidenceGraph(t)
	var err error
	parent := exported.SourceIndexEvidence[0]
	evidence, err := lyricsSourceIndexEvidenceFromBackupRecord(parent)
	if err != nil {
		t.Fatal(err)
	}

	destination := setupLyricsStore(t)
	conflictingCreatedAt := time.UnixMilli(parent.CreatedAt + 1).UTC()
	tx, err := destination.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOrVerifyLyricsIndexEvidenceTx(context.Background(), tx, evidence, conflictingCreatedAt); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := destination.ImportTranslationContent(nil, EventContentExport{}, exported); !errors.Is(err, ErrLyricsSourceArtifactConflict) {
		t.Fatalf("immutable parent conflict error=%v", err)
	}
	var createdAt int64
	var raw []byte
	var sourceDocuments int
	if err := destination.db.QueryRow(`SELECT created_at,raw_bytes,
		(SELECT COUNT(*) FROM song_lyrics_source_documents)
		FROM lyrics_source_index_evidence WHERE provider=? AND evidence_id=?`, parent.Provider, parent.EvidenceID).
		Scan(&createdAt, &raw, &sourceDocuments); err != nil {
		t.Fatal(err)
	}
	if createdAt != conflictingCreatedAt.UnixMilli() || !bytes.Equal(raw, parent.RawBytes) || sourceDocuments != 0 {
		t.Fatalf("conflict changed private parent createdAt=%d raw=%q sourceDocuments=%d", createdAt, raw, sourceDocuments)
	}
}

// Another artifact can link the same evidence id with its own bytes while a
// seed-style reference names other bytes. The backup keeps that reference
// unlinked, and the restored database exports the same content.
func TestContentBackupKeepsReferenceUnlinkedWhenSharedParentHasOtherBytes(t *testing.T) {
	s := setupLyricsStore(t)
	saveContentBackupEvidenceGraphSong(t, s, 10, nil)
	sharedID := ""
	otherDigest := sha256.Sum256([]byte("OTHER-LIST-OF-SONGS-BYTES"))
	otherSHA := hex.EncodeToString(otherDigest[:])
	saveContentBackupEvidenceGraphSong(t, s, 20, func(_ int, identity *model.LyricsSourceFixedIdentity) map[int]bool {
		if identity.Provider != model.LyricsSourceProviderSekaipedia {
			return nil
		}
		sharedID = identity.IndexEvidenceRefs[0].EvidenceID
		identity.IndexEvidenceRefs[0].SHA256 = otherSHA
		return map[int]bool{0: true}
	})
	if sharedID == "" {
		t.Fatal("fixture has no Sekaipedia artifact")
	}
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	parents := 0
	for _, parent := range exported.SourceIndexEvidence {
		if parent.EvidenceID == sharedID {
			parents++
		}
	}
	if parents != 1 {
		t.Fatalf("shared parent rows=%d, want the one linked by song 10", parents)
	}
	for _, link := range exported.SourceArtifactEvidence {
		if link.EvidenceID == sharedID && link.SHA256 == otherSHA {
			t.Fatalf("export linked a reference to a parent with other bytes: %+v", link)
		}
	}
	destination := setupLyricsStore(t)
	if err := destination.ImportTranslationContent(nil, EventContentExport{}, exported); err != nil {
		t.Fatalf("restore: %v", err)
	}
	again, err := destination.ExportLyricsContent()
	if err != nil {
		t.Fatalf("export restored content: %v", err)
	}
	firstJSON, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("restored export differs: first=%d bytes second=%d bytes", len(firstJSON), len(secondJSON))
	}
}

func setupContentBackupEvidenceGraph(t *testing.T) (*Store, LyricsContentExport) {
	t.Helper()
	s := setupLyricsStore(t)
	saveContentBackupEvidenceGraphSong(t, s, 10, nil)
	exported, err := s.ExportLyricsContent()
	if err != nil {
		t.Fatal(err)
	}
	return s, exported
}

// saveContentBackupEvidenceGraphSong saves and publishes one song whose
// artifacts carry linked raw evidence. unlink may rewrite an artifact's
// identity and returns the reference positions that get no evidence row or link.
func saveContentBackupEvidenceGraphSong(
	t *testing.T,
	s *Store,
	musicID int,
	unlink func(index int, identity *model.LyricsSourceFixedIdentity) map[int]bool,
) {
	t.Helper()
	input, document := publicLyricsV2VirtualSingerFixture(musicID)
	evidenceByArtifact := make([][]lyricssource.IndexEvidence, len(document.FixedIdentities))
	unlinked := make([]map[int]bool, len(document.FixedIdentities))
	for index, original := range document.FixedIdentities {
		identity, parents := contentBackupEvidenceForIdentity(t, original, index)
		if unlink != nil {
			unlinked[index] = unlink(index, &identity)
		}
		evidenceByArtifact[index] = parents
		document.FixedIdentities[index] = identity
	}
	fullIdentity, ok := publicLyricsFixedIdentity(document, document.Provenance.FullText.RenditionKey)
	if !ok {
		t.Fatal("content-backup fixture has no Full identity")
	}
	input.SourceURL = fullIdentity.CanonicalURL
	input.SourcePageID = fullIdentity.PageID
	input.SourceRevisionID = fullIdentity.RevisionID
	input.SourceSHA1 = fullIdentity.SHA1
	input.SourceFetchedAt = fullIdentity.FetchedAt
	saved := savePublicLyricsV2Fixture(t, s, input, document)

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var documentID int64
	if err := tx.QueryRowContext(context.Background(), `SELECT document_id FROM song_lyrics_source_documents WHERE music_id=?`, saved.MusicID).
		Scan(&documentID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	createdAt := time.Date(2026, time.July, 31, 15, 0, 0, 0, time.UTC)
	evidenceOffset := 0
	for index, parents := range evidenceByArtifact {
		identity := document.FixedIdentities[index]
		for position, item := range parents {
			if unlinked[index][position] {
				continue
			}
			if err := insertOrVerifyLyricsIndexEvidenceTx(context.Background(), tx, item,
				createdAt.Add(time.Duration(evidenceOffset)*time.Millisecond)); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			evidenceOffset++
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO song_lyrics_source_artifact_index_evidence
				(document_id,rendition_key,position,provider,evidence_id,sha256) VALUES (?,?,?,?,?,?)`,
				documentID, identity.RenditionKey, position, identity.Provider, item.EvidenceID, item.SHA256); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishLyrics(saved.MusicID, saved.Revision); err != nil {
		t.Fatal(err)
	}
}

func contentBackupEvidenceForIdentity(t *testing.T, original model.LyricsSourceFixedIdentity, index int) (model.LyricsSourceFixedIdentity, []lyricssource.IndexEvidence) {
	t.Helper()
	if original.Provider != model.LyricsSourceProviderSekaipedia {
		reason := original.VersionReason
		if reason == "" {
			reason = model.LyricsSourceVersionReasonUntaggedFullOnly
		}
		candidate := testRevisionCandidate(original.Provider, original.PageID, original.RevisionID,
			fmt.Sprintf("ContentBackup_%d", index+1), original.Categories, original.Section, original.RenditionKey,
			reason, []byte(fmt.Sprintf("CONTENT-BACKUP-EVIDENCE-%d", index+1)))
		parent := candidate.IndexEvidence[0]
		return model.LyricsSourceFixedIdentity{
			Provider: candidate.Provider, Origin: candidate.Origin, PageID: candidate.PageID, RevisionID: candidate.RevisionID,
			SHA1: candidate.SHA1, Title: candidate.Title, CanonicalURL: candidate.CanonicalURL,
			RevisionTimestamp: original.RevisionTimestamp, FetchedAt: parent.FetchedAt,
			Categories: append([]string(nil), candidate.Categories...), Section: candidate.Section,
			RenditionKey: original.RenditionKey, CompositionRenditionKey: original.CompositionRenditionKey,
			VersionReason:     original.VersionReason,
			IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef(nil), candidate.IndexEvidenceRefs...),
		}, []lyricssource.IndexEvidence{parent}
	}

	content := fmt.Sprintf("CONTENT-BACKUP-SEKAIPEDIA-%d", index+1)
	title := fmt.Sprintf("ContentBackup_%d", index+1)
	canonicalURL := fmt.Sprintf("https://www.sekaipedia.org/wiki/%s?oldid=%d", title, original.RevisionID)
	contentSHA1 := sha1.Sum([]byte(content))
	categories := append([]string(nil), original.Categories...)
	categoryRows := make([]map[string]string, len(categories))
	for categoryIndex, category := range categories {
		categoryRows[categoryIndex] = map[string]string{"title": "Category:" + category}
	}
	raw, err := json.Marshal(map[string]any{
		"query": map[string]any{"pages": map[string]any{
			fmt.Sprintf("%d", original.PageID): map[string]any{
				"pageid": original.PageID, "title": title, "categories": categoryRows,
				"revisions": []any{map[string]any{
					"revid": original.RevisionID, "timestamp": original.RevisionTimestamp,
					"sha1":  hex.EncodeToString(contentSHA1[:]),
					"slots": map[string]any{"main": map[string]any{"content": content}},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawSHA256 := sha256.Sum256(raw)
	rawSHA256Hex := hex.EncodeToString(rawSHA256[:])
	songEvidenceBaseID := fmt.Sprintf("revision:sekaipedia:%d:%d", original.PageID, original.RevisionID)
	songEvidenceID := contentBackupSekaipediaEvidenceID(songEvidenceBaseID, original.FetchedAt, rawSHA256Hex)
	songEvidence := lyricssource.IndexEvidence{
		EvidenceID: songEvidenceID, SHA256: rawSHA256Hex,
		Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
		PageID: original.PageID, RevisionID: original.RevisionID, RevisionTimestamp: original.RevisionTimestamp,
		MediaWikiSHA1: hex.EncodeToString(contentSHA1[:]), Title: title, CanonicalURL: canonicalURL,
		Categories: categories, FetchedAt: original.FetchedAt, Raw: raw, RawSHA256: rawSHA256Hex,
	}
	listEvidence := contentBackupSekaipediaListEvidence(t, original.FetchedAt)
	identity := original
	identity.SHA1 = songEvidence.MediaWikiSHA1
	identity.Title = title
	identity.CanonicalURL = canonicalURL
	identity.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{
		{EvidenceID: listEvidence.EvidenceID, SHA256: listEvidence.SHA256},
		{EvidenceID: songEvidence.EvidenceID, SHA256: songEvidence.SHA256},
	}
	return identity, []lyricssource.IndexEvidence{listEvidence, songEvidence}
}

func contentBackupSekaipediaListEvidence(t *testing.T, fetchedAt string) lyricssource.IndexEvidence {
	t.Helper()
	raw := mustContentBackupPackageFixture(t, "sekaipedia-list-335193.json")
	digest := sha256.Sum256(raw)
	rawSHA256 := hex.EncodeToString(digest[:])
	return lyricssource.IndexEvidence{
		EvidenceID: contentBackupSekaipediaEvidenceID(
			"authority:sekaipedia:list-of-songs:335193", fetchedAt, rawSHA256,
		),
		SHA256:   rawSHA256,
		Kind:     lyricssource.IndexEvidenceKindMediaWikiRevision,
		Provider: model.LyricsSourceProviderSekaipedia, Origin: model.LyricsSourceOriginSekaipedia,
		PageID: 268, RevisionID: 335193, RevisionTimestamp: "2026-07-27T16:29:13Z",
		MediaWikiSHA1: "b216a827f88c59f5e954a120027832fe9cd74413", Title: "List of songs",
		CanonicalURL: "https://www.sekaipedia.org/wiki/List_of_songs?oldid=335193",
		Categories:   []string{"Lists", "Project SEKAI"}, FetchedAt: fetchedAt,
		Raw: raw, RawSHA256: rawSHA256,
	}
}

func contentBackupSekaipediaEvidenceID(baseID, fetchedAt, rawSHA256 string) string {
	identity := strings.Join([]string{
		"lyrics-source-index-evidence-v1",
		string(lyricssource.IndexEvidenceKindMediaWikiRevision),
		string(model.LyricsSourceProviderSekaipedia),
		model.LyricsSourceOriginSekaipedia,
		baseID,
		fetchedAt,
		rawSHA256,
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s:%x", baseID, digest)
}

func rewriteContentBackupEvidenceIDAsLegacy(
	t *testing.T,
	content *LyricsContentExport,
	provider model.LyricsSourceProvider,
) (string, string) {
	t.Helper()
	for _, record := range content.SourceIndexEvidence {
		if record.Provider != string(provider) || record.Kind != string(lyricssource.IndexEvidenceKindMediaWikiRevision) {
			continue
		}
		legacyID := ""
		switch provider {
		case model.LyricsSourceProviderVocaloidFandom:
			legacyID = fmt.Sprintf("fetch:vocaloid-fandom:%d", record.PageID)
		case model.LyricsSourceProviderMoegirl:
			legacyID = fmt.Sprintf("search:moegirl:%d", record.PageID)
		default:
			t.Fatalf("unsupported legacy backup provider %q", provider)
		}
		rewriteContentBackupEvidenceID(t, content, provider, record.EvidenceID, legacyID)
		return record.EvidenceID, legacyID
	}
	t.Fatalf("content backup has no %s revision evidence", provider)
	return "", ""
}

func rewriteContentBackupEvidenceID(
	t *testing.T,
	content *LyricsContentExport,
	provider model.LyricsSourceProvider,
	oldID, newID string,
) {
	t.Helper()
	changedParents := 0
	for index := range content.SourceIndexEvidence {
		if content.SourceIndexEvidence[index].Provider == string(provider) && content.SourceIndexEvidence[index].EvidenceID == oldID {
			content.SourceIndexEvidence[index].EvidenceID = newID
			changedParents++
		}
	}
	changedLinks := 0
	for index := range content.SourceArtifactEvidence {
		if content.SourceArtifactEvidence[index].Provider == string(provider) && content.SourceArtifactEvidence[index].EvidenceID == oldID {
			content.SourceArtifactEvidence[index].EvidenceID = newID
			changedLinks++
		}
	}
	changedArtifacts := 0
	for index := range content.SourceArtifacts {
		artifact := &content.SourceArtifacts[index]
		if artifact.Provider != string(provider) {
			continue
		}
		identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(artifact.FixedIdentityJSON))
		if err != nil {
			t.Fatal(err)
		}
		changed := false
		for refIndex := range identity.IndexEvidenceRefs {
			if identity.IndexEvidenceRefs[refIndex].EvidenceID == oldID {
				identity.IndexEvidenceRefs[refIndex].EvidenceID = newID
				changed = true
			}
		}
		if !changed {
			continue
		}
		identityBody, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		refsBody, err := json.Marshal(identity.IndexEvidenceRefs)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(identityBody)
		artifact.FixedIdentityJSON = string(identityBody)
		artifact.FixedIdentitySHA256 = hex.EncodeToString(digest[:])
		artifact.IndexEvidenceRefsJSON = string(refsBody)
		changedArtifacts++
	}
	changedDocuments := 0
	changedDocumentSHA := map[int64]string{}
	for index := range content.SourceDocuments {
		documentRecord := &content.SourceDocuments[index]
		document, err := model.DecodeLyricsSourceDocument([]byte(documentRecord.DocumentJSON))
		if err != nil {
			t.Fatal(err)
		}
		changed := false
		for identityIndex := range document.FixedIdentities {
			identity := &document.FixedIdentities[identityIndex]
			if identity.Provider != provider {
				continue
			}
			for refIndex := range identity.IndexEvidenceRefs {
				if identity.IndexEvidenceRefs[refIndex].EvidenceID == oldID {
					identity.IndexEvidenceRefs[refIndex].EvidenceID = newID
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		documentBody, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(documentBody)
		documentRecord.DocumentJSON = string(documentBody)
		documentRecord.DocumentSHA256 = hex.EncodeToString(digest[:])
		changedDocumentSHA[documentRecord.DocumentID] = documentRecord.DocumentSHA256
		changedDocuments++
	}
	for index := range content.SourceContributions {
		contribution := &content.SourceContributions[index]
		documentSHA, changed := changedDocumentSHA[contribution.DocumentID]
		if !changed {
			continue
		}
		digest := sha256.Sum256([]byte(documentSHA + "\x00" + contribution.Component + "\x00" + contribution.RenditionKey))
		contribution.ContributionSHA256 = hex.EncodeToString(digest[:])
	}
	if changedParents != 1 || changedLinks != 1 || changedArtifacts != 1 || changedDocuments != 1 {
		t.Fatalf("evidence rewrite parents=%d links=%d artifacts=%d documents=%d", changedParents, changedLinks, changedArtifacts, changedDocuments)
	}
}

func contentBackupIndexEvidenceRecord(evidence lyricssource.IndexEvidence, createdAt int64) LyricsSourceIndexEvidenceBackupRecord {
	categoriesJSON, _ := json.Marshal(evidence.Categories)
	return LyricsSourceIndexEvidenceBackupRecord{
		Provider: string(evidence.Provider), EvidenceID: evidence.EvidenceID, SHA256: evidence.SHA256,
		Kind: string(evidence.Kind), Origin: evidence.Origin, PageID: evidence.PageID, RevisionID: evidence.RevisionID,
		RevisionTimestamp: evidence.RevisionTimestamp,
		MediaWikiSHA1:     evidence.MediaWikiSHA1, PageTitle: evidence.Title, CanonicalRevisionURL: evidence.CanonicalURL,
		CategoriesJSON: string(categoriesJSON), CanonicalRequestURL: evidence.CanonicalRequestURL,
		FetchedAt: evidence.FetchedAt, RawBytes: append([]byte(nil), evidence.Raw...), RawByteCount: len(evidence.Raw),
		RawSHA256: evidence.RawSHA256, CreatedAt: createdAt,
	}
}

func cloneLyricsContentExport(t *testing.T, input LyricsContentExport) LyricsContentExport {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var cloned LyricsContentExport
	if err := json.Unmarshal(body, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

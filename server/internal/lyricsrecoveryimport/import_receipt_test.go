package lyricsrecoveryimport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

func importReceiptFixture(t *testing.T) ImportReceipt {
	t.Helper()
	manifest := textFreeManifestFixture(t)
	evidence := emptyEvidenceReceiptFixture(t)
	items := make([]ImportReceiptItem, len(manifest.Items))
	for index, item := range manifest.Items {
		items[index] = ImportReceiptItem{
			MusicID: item.MusicID, State: item.State,
			AvailabilityDocumentSHA256: item.AvailabilityDocumentSHA256,
		}
	}
	receipt := ImportReceipt{
		SchemaVersion: ImportReceiptSchemaVersion, Kind: ImportReceiptKind,
		RuntimeSchemaVersion: ImportReceiptRuntimeSchemaVersion, DatabaseAuditAction: ImportReceiptAuditAction,
		CommitProtocol: ImportReceiptCommitProtocol, ReceiptAuditAction: ImportReceiptReceiptAuditAction,
		RootManifestFileSHA256: strings.Repeat("1", 64), RootID: manifest.Root.RootID,
		RootSHA256: manifest.Root.RootSHA256, ImportManifestFileSHA256: strings.Repeat("2", 64),
		BatchSHA256: manifest.BatchSHA256, EvidenceReceiptFileSHA256: strings.Repeat("3", 64),
		EvidenceReceiptSHA256: evidence.ReceiptSHA256, PackSHA256: evidence.PackSHA256,
		SelectionSHA256: evidence.SelectionSHA256, CatalogCount: manifest.Root.CatalogCount,
		MusicIDsSHA256: manifest.Root.MusicIDsSHA256, Coverage: manifest.Root.Coverage,
		BackupSHA256: strings.Repeat("4", 64), StateDigestVersion: ImportReceiptStateDigestVersion,
		BackupStateSHA256: strings.Repeat("5", 64), PreImportDatabaseSHA256: strings.Repeat("6", 64),
		PreImportStateSHA256: strings.Repeat("5", 64), PostImportStateSHA256: strings.Repeat("7", 64),
		ProtectedDigestVersion:   ImportReceiptProtectedDigestVersion,
		PreImportProtectedSHA256: strings.Repeat("8", 64), PostImportProtectedSHA256: strings.Repeat("8", 64),
		AuditBoundaryID: 17,
		DatabasePath:    "/tmp/recovery-import.db", RecoveryDatabasePath: "/tmp/.lyrics-recovery-import-fixture/database.sqlite",
		ReceiptPath: "/tmp/recovery-import-receipt.json", Actor: "offline-review",
		BatchCreatedAt: "2026-08-05T03:04:05Z", PreparedAt: "2026-08-05T03:05:06Z",
		Counts: ExpectedImportStorageCounts(manifest, evidence), Items: items,
	}
	digest, err := importReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ReceiptSHA256 = digest
	return receipt
}

func TestRecoveryImportReceiptCanonicalRoundTrip(t *testing.T) {
	receipt := importReceiptFixture(t)
	body, err := MarshalImportReceiptCanonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeImportReceiptCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalImportReceiptCanonical(decoded)
	if err != nil || !bytes.Equal(body, second) {
		t.Fatalf("canonical import receipt round trip err=%v", err)
	}
	lower := bytes.ToLower(body)
	for _, forbidden := range [][]byte{[]byte("romaji"), []byte("romanization"), []byte(`"lyrics"`), []byte(`"raw"`)} {
		if bytes.Contains(lower, forbidden) {
			t.Fatalf("import receipt leaked forbidden field %q", forbidden)
		}
	}
}

func TestRecoveryImportReceiptRejectsStateCountAndDigestDrift(t *testing.T) {
	for name, mutate := range map[string]func(*ImportReceipt){
		"state": func(receipt *ImportReceipt) {
			receipt.Items[0].State = receipt.Items[1].State
		},
		"prepared before batch": func(receipt *ImportReceipt) {
			receipt.PreparedAt = "2026-08-05T03:04:04Z"
		},
		"legacy committedAt in v2": func(receipt *ImportReceipt) {
			receipt.CommittedAt = receipt.BatchCreatedAt
		},
		"availability digest": func(receipt *ImportReceipt) {
			receipt.Items[0].AvailabilityDocumentSHA256 = ""
		},
		"counts": func(receipt *ImportReceipt) {
			receipt.Counts.AvailabilityDocuments--
		},
		"receipt digest": func(receipt *ImportReceipt) {
			receipt.ReceiptSHA256 = strings.Repeat("0", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := importReceiptFixture(t)
			mutate(&receipt)
			if err := ValidateImportReceipt(receipt); err == nil {
				t.Fatal("drifted recovery import receipt was accepted")
			}
		})
	}
}

func TestImportReceiptRevisionOwnershipDistinguishesEditableAndSourceOnlyItems(t *testing.T) {
	if !importReceiptItemOwnsEditableLyrics(Item{State: lyricscontract.CoverageComplete}) {
		t.Fatal("legacy complete item did not require an editable revision")
	}
	if importReceiptItemOwnsEditableLyrics(Item{State: lyricscontract.CoverageGameOnly}) {
		t.Fatal("availability-owned Game-only item unexpectedly required an editable revision")
	}
	sourceOnlyV3 := Item{
		State: lyricscontract.CoverageComplete,
		Draft: &lyricsstaging.Draft{Document: model.LyricsSourceDocument{
			SchemaVersion: model.LyricsSourceDocumentSchemaVersionV3,
		}},
	}
	if importReceiptItemOwnsEditableLyrics(sourceOnlyV3) {
		t.Fatal("non-compatible source-v3 item unexpectedly required an editable revision")
	}
}

func TestRecoveryImportReceiptDecodesHistoricalV1CanonicalReceipt(t *testing.T) {
	current := importReceiptFixture(t)
	legacy := importReceiptV1{
		SchemaVersion: ImportReceiptSchemaVersionV1, Kind: ImportReceiptKindV1,
		RuntimeSchemaVersion: current.RuntimeSchemaVersion, DatabaseAuditAction: current.DatabaseAuditAction,
		RootManifestFileSHA256: current.RootManifestFileSHA256, RootID: current.RootID, RootSHA256: current.RootSHA256,
		ImportManifestFileSHA256: current.ImportManifestFileSHA256, BatchSHA256: current.BatchSHA256,
		EvidenceReceiptFileSHA256: current.EvidenceReceiptFileSHA256, EvidenceReceiptSHA256: current.EvidenceReceiptSHA256,
		PackSHA256: current.PackSHA256, SelectionSHA256: current.SelectionSHA256,
		CatalogCount: current.CatalogCount, MusicIDsSHA256: current.MusicIDsSHA256, Coverage: current.Coverage,
		DatabasePath: current.DatabasePath, ReceiptPath: current.ReceiptPath, Actor: current.Actor,
		CommittedAt: current.BatchCreatedAt, Counts: current.Counts, Items: append([]ImportReceiptItem(nil), current.Items...),
	}
	legacy.ReceiptSHA256, _ = importReceiptV1Digest(legacy)
	body, err := marshalImportReceiptV1Canonical(legacy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeImportReceiptCanonical(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != ImportReceiptSchemaVersionV1 || decoded.Kind != ImportReceiptKindV1 ||
		decoded.CommitProtocol != "" || decoded.BackupSHA256 != "" || decoded.CommittedAt != legacy.CommittedAt ||
		decoded.BatchCreatedAt != "" || decoded.PreparedAt != "" || decoded.ReceiptSHA256 != legacy.ReceiptSHA256 {
		t.Fatalf("historical receipt projection=%+v", decoded)
	}
}

func TestRecoveryImportReceiptRejectsUnknownFieldsAndNoncanonicalJSON(t *testing.T) {
	body, err := MarshalImportReceiptCanonical(importReceiptFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(body, []byte(`"schemaVersion": 2,`), []byte("\"schemaVersion\": 2,\n  \"lyrics\": \"forbidden\","), 1)
	if _, err := DecodeImportReceiptCanonical(unknown); err == nil {
		t.Fatal("unknown content field was accepted")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeImportReceiptCanonical(compact.Bytes()); err == nil {
		t.Fatal("noncanonical recovery import receipt was accepted")
	}
}

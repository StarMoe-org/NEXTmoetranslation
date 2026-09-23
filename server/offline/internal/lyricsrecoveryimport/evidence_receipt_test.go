package lyricsrecoveryimport

import (
	"bytes"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
)

func emptyEvidenceReceiptFixture(t *testing.T) EvidenceReceipt {
	t.Helper()
	selectionSHA, err := lyricscontract.OrderedSelectionSHA256([]lyricscontract.EvidenceRef{})
	if err != nil {
		t.Fatal(err)
	}
	receipt := EvidenceReceipt{
		SchemaVersion: EvidenceReceiptSchemaVersion,
		RootID:        "recovery-root:test", RootSHA256: strings.Repeat("a", 64),
		PackSHA256: strings.Repeat("b", 64), SelectionSHA256: selectionSHA,
		Evidence: []lyricscontract.EvidenceRef{},
	}
	digest, err := evidenceReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ReceiptSHA256 = digest
	return receipt
}

func TestRecoveryImportEvidenceReceiptCanonicalRoundTrip(t *testing.T) {
	receipt := emptyEvidenceReceiptFixture(t)
	body, err := MarshalEvidenceReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeEvidenceReceipt(body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalEvidenceReceipt(decoded)
	if err != nil || !bytes.Equal(body, second) {
		t.Fatalf("canonical evidence receipt round trip err=%v", err)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("romaji")) {
		t.Fatal("evidence receipt leaked a romanization field")
	}
}

func TestRecoveryImportEvidenceReceiptRejectsDigestDrift(t *testing.T) {
	receipt := emptyEvidenceReceiptFixture(t)
	receipt.RawByteCount = 1
	if err := ValidateEvidenceReceipt(receipt); err == nil {
		t.Fatal("empty receipt pack accounting drift was accepted")
	}

	receipt = emptyEvidenceReceiptFixture(t)
	receipt.ReceiptSHA256 = strings.Repeat("0", 64)
	if err := ValidateEvidenceReceipt(receipt); err == nil {
		t.Fatal("receipt digest drift was accepted")
	}
}

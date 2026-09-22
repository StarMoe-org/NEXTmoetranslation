package lyricsevidencepack

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestConservativeShardAndTotalSafetyCeilings(t *testing.T) {
	const currentReceiptRawBytes = 32 << 20
	const currentReceiptEncodedBytes = 64 << 20
	if MaxShardRawBytes != 16<<20 || MaxShardEncodedBytes != 32<<20 ||
		MaxShardRawBytes > currentReceiptRawBytes || MaxShardEncodedBytes > currentReceiptEncodedBytes ||
		MaxPackRawBytes != 512<<20 || MaxPackEncodedBytes != 1<<30 || lyricscontract.MaxPackItems != 64<<10 {
		t.Fatalf("evidence pack bounds raw=%d encoded=%d totalRaw=%d totalEncoded=%d items=%d receiptRaw=%d receiptEncoded=%d",
			MaxShardRawBytes, MaxShardEncodedBytes, MaxPackRawBytes, MaxPackEncodedBytes, lyricscontract.MaxPackItems,
			currentReceiptRawBytes, currentReceiptEncodedBytes)
	}
}

func TestBuildRejectsPerItemRawBeyondTwoMiBBeforePublication(t *testing.T) {
	item := testEvidence(t, 1, 0)
	item.Raw = bytes.Repeat([]byte{'x'}, lyricssource.MaxIndexEvidenceRawBytes+1)
	rawDigest := sha256.Sum256(item.Raw)
	item.RawSHA256 = hex.EncodeToString(rawDigest[:])
	item.SHA256 = item.RawSHA256
	contentSHA1 := sha1.Sum(item.Raw)
	item.MediaWikiSHA1 = hex.EncodeToString(contentSHA1[:])
	item.EvidenceID = lyricssource.MediaWikiRevisionAcquisitionEvidenceID(
		model.LyricsSourceProviderVocaloidFandom, fmt.Sprintf("fetch:vocaloid-fandom:%d", item.PageID), item.FetchedAt, item.RawSHA256,
	)
	acquired := testAcquisition(item)
	ref := lyricscontract.EvidenceRef{
		Provider: item.Provider, AcquisitionID: string(acquired.AcquisitionID), EvidenceID: item.EvidenceID,
		SHA256: item.SHA256, EnvelopeSHA256: acquired.EvidenceEnvelopeSHA256,
	}
	output := filepath.Join(canonicalTestRoot(t), "pack")
	_, err := Build(context.Background(), output, []lyricscontract.EvidenceRef{ref}, sliceExactSource{items: []lyricssource.IndexEvidence{item}})
	if err == nil || !strings.Contains(err.Error(), "bytes or digest") {
		t.Fatalf("oversized item error=%v", err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized item created output: %v", statErr)
	}
}

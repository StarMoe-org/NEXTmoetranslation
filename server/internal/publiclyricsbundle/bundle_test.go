package publiclyricsbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"moesekai/server/internal/store"
)

func TestCatalogRuntimeMetadataSeparatesEmbeddedOverlayFromDatabaseState(t *testing.T) {
	metadata, err := CatalogRuntimeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) == 0 {
		t.Fatal("expected non-empty runtime metadata")
	}
	complete := metadata[307]
	if complete.ReleaseID != ReleaseID || !complete.ImmutableOverlay || complete.State != "complete" || !complete.HasDetail ||
		complete.Revision != 1 || complete.UpdatedAt != "2026-08-08T13:24:16Z" ||
		len(complete.AvailableVersions) != 2 || complete.AvailableVersions[0] != "full" || complete.AvailableVersions[1] != "game" ||
		complete.BatchSHA256 != BatchSHA256 || complete.RootSHA256 != RootSHA256 {
		t.Fatalf("complete runtime metadata=%+v", complete)
	}
	incomplete := metadata[682]
	if incomplete.ReleaseID != ReleaseID || !incomplete.ImmutableOverlay || incomplete.State != "incomplete" || incomplete.HasDetail ||
		incomplete.Revision != 1 || incomplete.UpdatedAt != "2026-08-08T13:24:16Z" || incomplete.AvailableVersions == nil ||
		len(incomplete.AvailableVersions) != 0 {
		t.Fatalf("incomplete runtime metadata=%+v", incomplete)
	}
	if _, ok := metadata[999999]; ok {
		t.Fatal("non-bundle music unexpectedly has runtime metadata")
	}
	complete.AvailableVersions[0] = "mutated"
	again, err := CatalogRuntimeMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if got := again[307].AvailableVersions[0]; got != "full" {
		t.Fatalf("runtime metadata cache leaked caller mutation: %q", got)
	}
}

func TestBundleIsClosedPublicV3Inventory(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) == 0 {
		t.Fatal("expected non-empty assets")
	}
	index := assets["translation/lyrics/index.json"]
	if len(index) == 0 {
		t.Fatal("missing embedded public lyrics index")
	}
	document, err := store.DecodePublicLyricsV3Index(index)
	if err != nil {
		t.Fatal(err)
	}
	if document.Version != 3 || len(document.Songs) == 0 {
		t.Fatalf("index version=%d songs=%d", document.Version, len(document.Songs))
	}
	for _, musicID := range []int{307, 728, 754, 765, 83, 750, 789} {
		if _, ok := assets["translation/lyrics/music_"+itoa(musicID)+".json"]; !ok {
			t.Fatalf("missing representative detail musicId=%d", musicID)
		}
	}
	if _, ok := assets["translation/lyrics/music_682.json"]; ok {
		t.Fatal("incomplete musicId=682 unexpectedly has a public detail")
	}
	if _, ok := assets["translation/lyrics/music_789.json"]; !ok {
		t.Fatal("restored musicId=789 missing public detail")
	}
	if got := sha256Hex(index); len(got) != 64 {
		t.Fatalf("invalid index hash=%s", got)
	}
	for key := range assets {
		if !bytes.HasPrefix([]byte(key), []byte("translation/lyrics/")) {
			t.Fatalf("asset escaped public lyrics prefix: %q", key)
		}
	}
}

func TestValidateDocumentsStructuralChecks(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fullIndex, err := store.DecodePublicLyricsV3Index(assets["translation/lyrics/index.json"])
	if err != nil {
		t.Fatal(err)
	}
	sampleSongs := fullIndex.Songs[:0]
	for _, song := range fullIndex.Songs {
		if song.MusicID == 307 || song.MusicID == 682 {
			sampleSongs = append(sampleSongs, song)
		}
	}
	fullIndex.Songs = sampleSongs
	compactIndex, err := json.Marshal(fullIndex)
	if err != nil {
		t.Fatal(err)
	}
	baseAssets := map[string][]byte{
		"index.json":     compactIndex,
		"music_307.json": bytes.Clone(assets["translation/lyrics/music_307.json"]),
	}

	cloneAssets := func(in map[string][]byte) map[string][]byte {
		out := make(map[string][]byte, len(in))
		for k, v := range in {
			out[k] = bytes.Clone(v)
		}
		return out
	}

	t.Run("valid documents pass", func(t *testing.T) {
		testAssets := cloneAssets(baseAssets)
		if err := validateDocuments(testAssets); err != nil {
			t.Fatalf("unexpected validation failure: %v", err)
		}
	})

	t.Run("missing index.json", func(t *testing.T) {
		testAssets := cloneAssets(baseAssets)
		delete(testAssets, "index.json")
		if err := validateDocuments(testAssets); err == nil || !strings.Contains(err.Error(), "missing index.json") {
			t.Fatalf("expected missing index.json error, got: %v", err)
		}
	})

	t.Run("invalid index version", func(t *testing.T) {
		testAssets := cloneAssets(baseAssets)
		replaced := bytes.Replace(testAssets["index.json"], []byte(`"version":3`), []byte(`"version":2`), 1)
		if bytes.Equal(replaced, testAssets["index.json"]) {
			replaced = bytes.Replace(testAssets["index.json"], []byte(`"version": 3`), []byte(`"version": 2`), 1)
		}
		testAssets["index.json"] = replaced
		if err := validateDocuments(testAssets); err == nil {
			t.Fatal("expected error for invalid index version")
		}
	})

	t.Run("missing detail file for complete song", func(t *testing.T) {
		testAssets := cloneAssets(baseAssets)
		delete(testAssets, "music_307.json")
		if err := validateDocuments(testAssets); err == nil || !strings.Contains(err.Error(), "detail inventory differs") {
			t.Fatalf("expected detail inventory differs error, got: %v", err)
		}
	})

	t.Run("extra unreferenced detail file", func(t *testing.T) {
		testAssets := cloneAssets(baseAssets)
		fake682 := bytes.Replace(testAssets["music_307.json"], []byte(`"musicId":307`), []byte(`"musicId":682`), 1)
		if bytes.Equal(fake682, testAssets["music_307.json"]) {
			fake682 = bytes.Replace(testAssets["music_307.json"], []byte(`"musicId": 307`), []byte(`"musicId": 682`), 1)
		}
		testAssets["music_682.json"] = fake682
		if err := validateDocuments(testAssets); err == nil || !strings.Contains(err.Error(), "unexpected public lyrics detail") {
			t.Fatalf("expected unexpected detail error, got: %v", err)
		}
	})

	t.Run("detail metadata mismatch", func(t *testing.T) {
		testAssets := cloneAssets(baseAssets)
		replaced := bytes.Replace(testAssets["music_307.json"], []byte(`"revision":1`), []byte(`"revision":999`), 1)
		if bytes.Equal(replaced, testAssets["music_307.json"]) {
			replaced = bytes.Replace(testAssets["music_307.json"], []byte(`"revision": 1`), []byte(`"revision": 999`), 1)
		}
		testAssets["music_307.json"] = replaced
		if err := validateDocuments(testAssets); err == nil || !strings.Contains(err.Error(), "detail/index metadata differs") {
			t.Fatalf("expected metadata differs error, got: %v", err)
		}
	})
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}

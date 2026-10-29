package embeddedlyricsseed

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbeddedLyricsEditorSeedLoads(t *testing.T) {
	bundle, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ArchiveSHA256 != ExpectedArchiveSHA256 || bundle.Manifest.CatalogCount != ExpectedCatalogCount ||
		len(bundle.Manifest.Items) != ExpectedCatalogCount || len(bundle.Documents) != ExpectedSourceV3 ||
		len(bundle.Artifacts) != ExpectedArtifacts || len(bundle.Contributions) != ExpectedContributions ||
		len(bundle.LegacyDocuments) != ExpectedLegacy || len(bundle.Availability) != ExpectedAvailability {
		t.Fatalf("embedded lyrics editor seed counts differ: archive=%s manifest=%+v docs=%d artifacts=%d contributions=%d legacy=%d availability=%d",
			bundle.ArchiveSHA256, bundle.Manifest, len(bundle.Documents), len(bundle.Artifacts), len(bundle.Contributions),
			len(bundle.LegacyDocuments), len(bundle.Availability))
	}
}

func TestDecodeClosedJSONArrayRejectsDuplicateAndUnknownFields(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "duplicate", body: `[{"musicId":1,"musicId":1}]`, want: "duplicate object key"},
		{name: "unknown", body: `[{"musicId":1,"unknown":true}]`, want: "unknown field"},
		{name: "trailing", body: `[{"musicId":1}] {}`, want: "trailing JSON value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var records []struct {
				MusicID int `json:"musicId"`
			}
			err := decodeClosedJSONArray(bytes.NewBufferString(test.body), &records, 1)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error=%v records=%+v", err, records)
			}
		})
	}
}

func TestDecodeArchiveRejectsTrailingGzipData(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte("canonical-payload")); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateCanonicalGzipNoTrailing(buf.Bytes()); err != nil {
		t.Fatalf("clean gzip rejected: %v", err)
	}
	trailing := append(append([]byte(nil), buf.Bytes()...), 0)
	if err := validateCanonicalGzipNoTrailing(trailing); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("archive with trailing gzip data error=%v", err)
	}
}

func TestValidateManifestIdentityRejectsUnexpectedFileCount(t *testing.T) {
	bundle, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	manifest := bundle.Manifest
	manifest.Files[0].Count++
	if err := validateManifestIdentity(manifest); err == nil || !strings.Contains(err.Error(), "file inventory") {
		t.Fatalf("unexpected inventory count accepted: %+v error=%v", manifest.Files[0], err)
	}
}

func TestValidateManifestIdentityRejectsOutOfOrderInventory(t *testing.T) {
	bundle, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	manifest := bundle.Manifest
	manifest.Files[0], manifest.Files[1] = manifest.Files[1], manifest.Files[0]
	if err := validateManifestIdentity(manifest); err == nil || !strings.Contains(err.Error(), "file inventory") {
		encoded, _ := json.Marshal(manifest.Files)
		t.Fatalf("out-of-order inventory accepted: %s error=%v", encoded, err)
	}
}

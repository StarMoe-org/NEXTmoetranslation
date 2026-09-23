package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"moesekai/server/offline/internal/lyricsstaging"
)

func TestRunDescribesManifestIntegrityWithoutOverstatingProvenance(t *testing.T) {
	manifest := validManifest(t)
	body, err := lyricsstaging.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "staging.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := run([]string{"-input", path}, &output); err != nil {
		t.Fatal(err)
	}
	want := "verified staging manifest self-consistency and integrity for 1 staged draft; did not independently verify fixed-revision provenance; batch sha256:" + manifest.BatchSHA256 + "\n"
	if got := output.String(); got != want {
		t.Fatalf("validator wording mismatch\n got: %q\nwant: %q", got, want)
	}
}

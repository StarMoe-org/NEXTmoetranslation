package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Offline-only packages (recovery, staging, evidence packs, provider policy and
// coordination) must never be linked into the production server binary. This
// test pins the boundary that the P2 split established.
func TestProductionBinaryDoesNotLinkOfflinePackages(t *testing.T) {
	forbidden := []string{
		"moesekai/server/internal/lyricsacquisition",
		"moesekai/server/internal/lyricscompose",
		"moesekai/server/internal/lyricsevidencepack",
		"moesekai/server/internal/lyricsextractionplan",
		"moesekai/server/internal/lyricsimportreceipt",
		"moesekai/server/internal/lyricsoutcomeartifact",
		"moesekai/server/internal/lyricsprovidercoord",
		"moesekai/server/internal/lyricsproviderpolicy",
		"moesekai/server/internal/lyricsrecovery",
		"moesekai/server/internal/lyricsrecoveryimport",
		"moesekai/server/internal/lyricsrecoverypublic",
		"moesekai/server/internal/lyricsreview",
		"moesekai/server/internal/lyricsrootmanifest",
		"moesekai/server/internal/lyricssourceoffline",
		"moesekai/server/internal/lyricsstaging",
		"moesekai/server/internal/offlineimport",
	}

	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := exec.LookPath(goBinary); err != nil {
		goBinary = "go"
	}
	cmd := exec.Command(goBinary, "list", "-deps", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, stderr.String())
	}
	linked := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		linked[strings.TrimSpace(line)] = true
	}
	if !linked["moesekai/server/internal/lyricssource"] {
		t.Fatalf("go list -deps output does not look like the production binary: %d packages", len(linked))
	}
	for _, pkg := range forbidden {
		if linked[pkg] {
			t.Errorf("offline package %s is linked into the production server binary", pkg)
		}
	}
}

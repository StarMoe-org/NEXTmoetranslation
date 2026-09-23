package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Offline-only code (recovery, staging, evidence packs, provider policy and
// coordination) lives in the nested module server/offline and must never be
// linked into the production server binary. This test pins that boundary: no
// dependency may come from the offline module, and none of the packages that
// moved there may reappear under the production internal tree.
func TestProductionBinaryDoesNotLinkOfflinePackages(t *testing.T) {
	const offlineModulePrefix = "moesekai/server/offline/"
	movedPackages := []string{
		"lyricsacquisition",
		"lyricscompose",
		"lyricsevidencepack",
		"lyricsextractionplan",
		"lyricsimportreceipt",
		"lyricsoutcomeartifact",
		"lyricsprovidercoord",
		"lyricsproviderpolicy",
		"lyricsrecovery",
		"lyricsrecoveryimport",
		"lyricsrecoverypublic",
		"lyricsreview",
		"lyricsrootmanifest",
		"lyricssourceoffline",
		"lyricsstaging",
		"offlineimport",
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
	for pkg := range linked {
		if strings.HasPrefix(pkg, offlineModulePrefix) {
			t.Errorf("offline module package %s is linked into the production server binary", pkg)
		}
	}
	for _, name := range movedPackages {
		if pkg := "moesekai/server/internal/" + name; linked[pkg] {
			t.Errorf("offline package %s reappeared in the production server binary", pkg)
		}
	}
}

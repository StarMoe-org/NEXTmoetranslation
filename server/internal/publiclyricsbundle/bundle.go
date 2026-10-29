// Package publiclyricsbundle exposes the accepted public-only Public Lyrics v3
// runtime projection. It contains no producer database, recovery evidence,
// manifest, receipt, or authenticated application state.
package publiclyricsbundle

import (
	"archive/tar"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

const (
	ReleaseID                = "runtime-ruby-restore-full-aug1617-20260818b"
	BatchSHA256              = "6559b9b21fff20418ec97e1a965cbff3f516f18205e122355e0e96cd19472bd7"
	RootSHA256               = "fe486efcd029659519b411a88dfe5688d2a67f351bc19bb61fa970784a39b3ad"
	maxPublicLyricsAssetSize = 2 << 20
)

//go:embed public-v3.tar.gz
var archiveFS embed.FS

var (
	validateOnce   sync.Once
	validateErr    error
	loadedMetadata map[int]model.RuntimeLyricsMetadata
	detailRE       = regexp.MustCompile(`^music_([1-9][0-9]*)\.json$`)
)

// Load verifies and returns the immutable runtime assets under their files
// service keys (translation/lyrics/index.json and music_<id>.json). After the
// initial validation pass, Load unpacks directly from the compiled-in archive
// so ~25 MiB of uncompressed JSON is not pinned permanently in package globals.
func Load() (map[string][]byte, error) {
	var firstAssets map[string][]byte
	validateOnce.Do(func() {
		firstAssets, validateErr = unpackAndValidate(true)
	})
	if validateErr != nil {
		return nil, validateErr
	}
	if firstAssets != nil {
		return firstAssets, nil
	}
	return unpackAndValidate(false)
}

// CatalogRuntimeMetadata returns the immutable Public v3 index projection used
// only to explain what the embedded runtime serves. These records do not
// represent authenticated editor drafts or database publication state.
func CatalogRuntimeMetadata() (map[int]model.RuntimeLyricsMetadata, error) {
	validateOnce.Do(func() {
		_, validateErr = unpackAndValidate(true)
	})
	if validateErr != nil {
		return nil, validateErr
	}
	metadata := make(map[int]model.RuntimeLyricsMetadata, len(loadedMetadata))
	for musicID, item := range loadedMetadata {
		item.AvailableVersions = append([]string{}, item.AvailableVersions...)
		metadata[musicID] = item
	}
	return metadata, nil
}

func buildCatalogRuntimeMetadata(index store.PublicLyricsV3IndexDocument) map[int]model.RuntimeLyricsMetadata {
	metadata := make(map[int]model.RuntimeLyricsMetadata, len(index.Songs))
	for _, song := range index.Songs {
		versions := append([]string(nil), song.AvailableVersions...)
		metadata[song.MusicID] = model.RuntimeLyricsMetadata{
			ReleaseID:         ReleaseID,
			ImmutableOverlay:  true,
			Source:            "bundle",
			State:             string(song.State),
			HasDetail:         song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly,
			AvailableVersions: versions,
			Revision:          song.Revision,
			UpdatedAt:         song.UpdatedAt,
			BatchSHA256:       BatchSHA256,
			RootSHA256:        RootSHA256,
		}
	}
	return metadata
}

func unpackAndValidate(validate bool) (map[string][]byte, error) {
	archiveFile, err := archiveFS.Open("public-v3.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("read embedded public lyrics archive: %w", err)
	}
	defer archiveFile.Close()

	compressed, err := gzip.NewReader(archiveFile)
	if err != nil {
		return nil, fmt.Errorf("open public lyrics archive: %w", err)
	}
	defer compressed.Close()

	assets := make(map[string][]byte, 720)
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read public lyrics archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg ||
			path.Clean(header.Name) != header.Name ||
			strings.Contains(header.Name, "/") ||
			strings.Contains(header.Name, "..") ||
			(header.Name != "index.json" && !detailRE.MatchString(header.Name)) {
			return nil, fmt.Errorf("invalid public lyrics archive entry %q", header.Name)
		}
		if header.Size < 1 || header.Size > maxPublicLyricsAssetSize {
			return nil, fmt.Errorf("invalid public lyrics asset size for %q", header.Name)
		}
		if _, exists := assets[header.Name]; exists {
			return nil, fmt.Errorf("duplicate public lyrics asset %q", header.Name)
		}
		body, err := io.ReadAll(io.LimitReader(reader, maxPublicLyricsAssetSize+1))
		if err != nil || int64(len(body)) != header.Size {
			return nil, fmt.Errorf("read public lyrics asset %q: size mismatch", header.Name)
		}
		if int64(len(body)) > maxPublicLyricsAssetSize {
			return nil, fmt.Errorf("public lyrics asset %q exceeds size limit", header.Name)
		}
		assets[header.Name] = body
	}

	if validate {
		index, err := store.DecodePublicLyricsV3Index(assets["index.json"])
		if err != nil {
			return nil, fmt.Errorf("decode public lyrics index: %w", err)
		}
		if err := validateArchiveInventory(assets, index); err != nil {
			return nil, err
		}
		loadedMetadata = buildCatalogRuntimeMetadata(index)
	}

	result := make(map[string][]byte, len(assets))
	for name, body := range assets {
		result["translation/lyrics/"+name] = body
	}
	return result, nil
}

func validateArchiveInventory(assets map[string][]byte, index store.PublicLyricsV3IndexDocument) error {
	expectedDetails := make(map[string]struct{}, len(index.Songs))
	for _, song := range index.Songs {
		if song.State == store.PublicLyricsStateComplete || song.State == store.PublicLyricsStateGameOnly {
			name := "music_" + strconv.Itoa(song.MusicID) + ".json"
			if _, ok := assets[name]; !ok {
				return fmt.Errorf("missing public lyrics detail %q", name)
			}
			expectedDetails[name] = struct{}{}
		}
	}
	for name := range assets {
		if name == "index.json" {
			continue
		}
		if _, ok := expectedDetails[name]; !ok {
			return fmt.Errorf("unexpected public lyrics detail %q", name)
		}
	}
	if len(assets) != len(expectedDetails)+1 {
		return fmt.Errorf("public lyrics detail inventory differs: assets=%d expected=%d", len(assets)-1, len(expectedDetails))
	}
	return nil
}

func validateDocuments(assets map[string][]byte) error {
	indexBody, ok := assets["index.json"]
	if !ok {
		return fmt.Errorf("public lyrics archive missing index.json")
	}
	index, err := store.DecodePublicLyricsV3Index(indexBody)
	if err != nil {
		return fmt.Errorf("decode public lyrics index: %w", err)
	}
	if index.Version != 3 {
		return fmt.Errorf("public lyrics index version is not 3")
	}

	expectedDetails := make(map[int]store.PublicLyricsIndexSong)
	seenMusic := make(map[int]struct{}, len(index.Songs))
	for _, song := range index.Songs {
		if song.MusicID <= 0 {
			return fmt.Errorf("public lyrics index contains invalid music ID %d", song.MusicID)
		}
		if _, exists := seenMusic[song.MusicID]; exists {
			return fmt.Errorf("public lyrics index contains duplicate music ID %d", song.MusicID)
		}
		seenMusic[song.MusicID] = struct{}{}
		switch song.State {
		case store.PublicLyricsStateComplete, store.PublicLyricsStateGameOnly:
			expectedDetails[song.MusicID] = song
		case store.PublicLyricsStateSatisfiedNoLyrics, store.PublicLyricsStateIncomplete:
		default:
			return fmt.Errorf("public lyrics index contains unexpected state %q", song.State)
		}
	}

	seenDetails := make(map[int]struct{}, len(expectedDetails))
	for name, body := range assets {
		if name == "index.json" {
			continue
		}
		match := detailRE.FindStringSubmatch(name)
		if match == nil {
			return fmt.Errorf("unexpected file %q in public lyrics archive", name)
		}
		musicID, _ := strconv.Atoi(match[1])
		detail, err := store.DecodePublicLyricsV3Detail(body)
		if err != nil {
			return fmt.Errorf("decode public lyrics detail %q: %w", name, err)
		}
		if detail.Version != 3 || detail.MusicID != musicID {
			return fmt.Errorf("public lyrics detail identity differs for %q", name)
		}
		song, expected := expectedDetails[musicID]
		if !expected {
			return fmt.Errorf("unexpected public lyrics detail %q", name)
		}
		if detail.Revision != song.Revision || detail.UpdatedAt != song.UpdatedAt || detail.State != song.State {
			return fmt.Errorf("public lyrics detail/index metadata differs for %q", name)
		}
		if !sameStringSlices(detailAvailableVersions(detail), song.AvailableVersions) {
			return fmt.Errorf("public lyrics detail/index versions differ for %q", name)
		}
		seenDetails[musicID] = struct{}{}
	}

	if len(seenDetails) != len(expectedDetails) {
		return fmt.Errorf("public lyrics detail inventory differs from index")
	}
	return nil
}

func detailAvailableVersions(detail store.PublicLyricsV3DetailDocument) []string {
	seen := map[string]bool{}
	for _, rendition := range detail.Renditions {
		for _, version := range rendition.AvailableVersions {
			seen[version] = true
		}
	}
	versions := make([]string, 0, 2)
	for _, version := range []string{"full", "game"} {
		if seen[version] {
			versions = append(versions, version)
		}
	}
	return versions
}

func sameStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

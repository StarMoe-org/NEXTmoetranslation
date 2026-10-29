package embeddedlyricsseed

import (
	"embed"
	"fmt"
	"sync"
)

const ExpectedArchiveSHA256 = "a8a2a7c841d0d73e448fd69f9adb236965b3b01a89d2ba58dcc921925e6ea479"

//go:embed editor-seed.tar.gz
var archiveFS embed.FS

var (
	loadMu   sync.Mutex
	isLoaded bool
	loaded   Bundle
	loadErr  error
)

func Load() (Bundle, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if !isLoaded {
		archive, err := archiveFS.ReadFile("editor-seed.tar.gz")
		if err != nil {
			loadErr = fmt.Errorf("read embedded lyrics editor seed: %w", err)
		} else {
			loaded, loadErr = DecodeArchive(archive)
			if loadErr == nil && loaded.ArchiveSHA256 != ExpectedArchiveSHA256 {
				loaded = Bundle{}
				loadErr = fmt.Errorf("embedded lyrics editor seed archive SHA-256 differs")
			}
		}
		isLoaded = true
	}
	if loadErr != nil {
		return Bundle{}, loadErr
	}
	return cloneBundle(loaded), nil
}

// Release drops the decoded startup seed cache so the runtime does not retain
// ~50 MiB of unpacked seed records after database seeding completes. Subsequent
// Load calls re-decode the embedded archive if needed.
func Release() {
	loadMu.Lock()
	loaded = Bundle{}
	loadErr = nil
	isLoaded = false
	loadMu.Unlock()
}

func cloneBundle(source Bundle) Bundle {
	copy := source
	copy.Manifest.Items = append([]CatalogItem(nil), source.Manifest.Items...)
	copy.Manifest.Files = append([]FileRecord(nil), source.Manifest.Files...)
	copy.Documents = append([]SourceDocumentRecord(nil), source.Documents...)
	copy.Artifacts = append([]SourceArtifactRecord(nil), source.Artifacts...)
	copy.Contributions = append([]SourceContributionRecord(nil), source.Contributions...)
	copy.LegacyDocuments = append([]LegacyDocumentRecord(nil), source.LegacyDocuments...)
	copy.LegacyLines = append([]LegacyLineRecord(nil), source.LegacyLines...)
	copy.LegacySegments = append([]LegacySegmentRecord(nil), source.LegacySegments...)
	copy.Availability = append([]AvailabilityRecord(nil), source.Availability...)
	return copy
}

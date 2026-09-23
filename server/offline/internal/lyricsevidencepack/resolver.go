package lyricsevidencepack

import (
	"errors"
	"fmt"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
)

type resolvedEvidence struct {
	ref  lyricscontract.EvidenceRef
	item lyricssource.IndexEvidence
}

// Resolver owns one validated in-memory copy of each exact envelope. Every
// shard is read and validated exactly once during OpenResolver; ID hydration is O(1).
type Resolver struct {
	manifest            Manifest
	byID                map[string]resolvedEvidence
	validatedShardCount int
}

// OpenResolver validates the manifest, directory closure, and every shard exactly once.
func OpenResolver(path string) (*Resolver, error) {
	directory, err := openPrivateDirectory(path, false)
	if err != nil {
		return nil, err
	}
	defer directory.close()
	if err := directory.lock(); err != nil {
		return nil, err
	}
	defer directory.unlock()
	manifestBody, _, err := readVerifiedFile(directory, ManifestFileName, "evidence pack manifest", "", -1, MaxManifestBytes, 1)
	if err != nil {
		return nil, err
	}
	manifest, err := DecodeManifest(manifestBody)
	if err != nil {
		return nil, err
	}
	if manifest.Totals.EncodedByteCount+int64(len(manifestBody)) > MaxPackEncodedBytes {
		return nil, errors.New("evidence pack publication exceeds the total private encoded-byte ceiling")
	}
	if err := verifyResolverEntries(directory, manifest); err != nil {
		return nil, err
	}
	resolver := &Resolver{manifest: manifest, byID: make(map[string]resolvedEvidence, len(manifest.Selected))}
	selectedIndex := 0
	for _, shardManifest := range manifest.Shards {
		name, _ := ShardFileName(shardManifest.Ordinal, shardManifest.SHA256)
		body, _, err := readVerifiedFile(directory, name, fmt.Sprintf("evidence shard %d", shardManifest.Ordinal),
			shardManifest.SHA256, shardManifest.EncodedByteCount, MaxShardEncodedBytes, 1)
		if err != nil {
			return nil, err
		}
		items, itemBodies, err := decodeShardItemsNoCopy(body, shardManifest.Ordinal)
		if err != nil {
			return nil, err
		}
		if len(items) != shardManifest.ItemCount || len(items) != len(shardManifest.Items) {
			return nil, fmt.Errorf("evidence shard %d envelope does not match its manifest", shardManifest.Ordinal)
		}
		rawBytes := 0
		lastID := ""
		for itemIndex, item := range items {
			if itemIndex > 0 && lastID >= item.EvidenceID {
				return nil, fmt.Errorf("evidence shard %d is not uniquely ordered", shardManifest.Ordinal)
			}
			lastID = item.EvidenceID
			ref := shardManifest.Items[itemIndex]
			if selectedIndex >= len(manifest.Selected) || ref != manifest.Selected[selectedIndex] ||
				ref.Provider != item.Provider || ref.EvidenceID != item.EvidenceID || ref.SHA256 != item.SHA256 ||
				item.RawSHA256 != ref.SHA256 {
				return nil, errors.New("evidence shard item does not equal its compact exact reference")
			}
			if sha256Hex(itemBodies[itemIndex]) != ref.EnvelopeSHA256 {
				return nil, errors.New("evidence shard item does not match its canonical envelope digest")
			}
			selectedIndex++
			if _, duplicate := resolver.byID[item.EvidenceID]; duplicate {
				return nil, errors.New("evidence pack resolves one EvidenceID more than once")
			}
			resolver.byID[item.EvidenceID] = resolvedEvidence{ref: ref, item: item}
			rawBytes += len(item.Raw)
		}
		if rawBytes != shardManifest.RawByteCount {
			return nil, fmt.Errorf("evidence shard %d raw-byte accounting does not match", shardManifest.Ordinal)
		}
		resolver.validatedShardCount++
	}
	if selectedIndex != len(manifest.Selected) || len(resolver.byID) != len(manifest.Selected) {
		return nil, errors.New("resolved evidence union does not equal selected references")
	}
	if err := verifyResolverEntries(directory, manifest); err != nil {
		return nil, err
	}
	return resolver, nil
}

func verifyResolverEntries(directory *privateDirectory, manifest Manifest) error {
	expected := make(map[string]struct{}, len(manifest.Shards)+1)
	expected[ManifestFileName] = struct{}{}
	for _, shard := range manifest.Shards {
		name, _ := ShardFileName(shard.Ordinal, shard.SHA256)
		expected[name] = struct{}{}
	}
	return verifyExactEvidenceEntries(directory, expected, "evidence pack directory")
}

// Manifest returns a defensive compact manifest copy.
func (resolver *Resolver) Manifest() Manifest {
	if resolver == nil {
		return Manifest{}
	}
	return cloneManifest(resolver.manifest)
}

// HydrateExact resolves one compact reference and checks its provider and digest.
func (resolver *Resolver) HydrateExact(ref lyricscontract.EvidenceRef) (lyricssource.IndexEvidence, error) {
	if resolver == nil || resolver.byID == nil {
		return lyricssource.IndexEvidence{}, errors.New("evidence resolver is required")
	}
	if err := lyricscontract.ValidateEvidenceRef(ref); err != nil {
		return lyricssource.IndexEvidence{}, err
	}
	resolved, found := resolver.byID[ref.EvidenceID]
	if !found || resolved.ref != ref || resolved.item.Provider != ref.Provider || resolved.item.SHA256 != ref.SHA256 ||
		resolved.item.RawSHA256 != ref.SHA256 {
		return lyricssource.IndexEvidence{}, errors.New("exact evidence reference does not match its hydrated envelope")
	}
	return cloneEvidence(resolved.item), nil
}

// ValidateSelected proves an independently supplied ordered union exactly
// equals the pack selection in one pass without copying the reference slice.
func (resolver *Resolver) ValidateSelected(refs []lyricscontract.EvidenceRef) error {
	if resolver == nil || resolver.byID == nil {
		return errors.New("evidence resolver is required")
	}
	if err := lyricscontract.ValidateOrderedSelection(refs); err != nil {
		return err
	}
	if len(refs) != len(resolver.manifest.Selected) {
		return errors.New("selected acquisition/evidence union does not equal the evidence pack")
	}
	for index := range refs {
		if refs[index] != resolver.manifest.Selected[index] {
			return errors.New("selected acquisition/evidence union does not equal the evidence pack")
		}
	}
	return nil
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Selected = append([]lyricscontract.EvidenceRef{}, manifest.Selected...)
	manifest.Shards = append([]ShardManifest{}, manifest.Shards...)
	for index := range manifest.Shards {
		manifest.Shards[index].Items = append([]lyricscontract.EvidenceRef(nil), manifest.Shards[index].Items...)
	}
	return manifest
}

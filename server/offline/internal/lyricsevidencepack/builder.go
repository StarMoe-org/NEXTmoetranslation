package lyricsevidencepack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricssource"
)

type buildLimits struct {
	shardRaw     int
	shardEncoded int
	totalRaw     int64
	totalEncoded int64
	items        int
}

var defaultBuildLimits = buildLimits{
	shardRaw: MaxShardRawBytes, shardEncoded: MaxShardEncodedBytes,
	totalRaw: MaxPackRawBytes, totalEncoded: MaxPackEncodedBytes, items: lyricscontract.MaxPackItems,
}

type plannedShard struct {
	manifest ShardManifest
	start    int
	end      int
}

var testHookBeforeManifest func() error

// Build replays every selected acquisition by exact content address,
// deterministically shards the resulting exact evidence union, and publishes a
// private create-exclusive pack in directory.
func Build(ctx context.Context, directory string, selected []lyricscontract.EvidenceRef, source ExactAcquisitionSource) (Manifest, error) {
	return buildWithLimits(ctx, directory, selected, source, defaultBuildLimits)
}

func buildWithLimits(ctx context.Context, directory string, selectedInput []lyricscontract.EvidenceRef, source ExactAcquisitionSource, limits buildLimits) (Manifest, error) {
	if ctx == nil || source == nil {
		return Manifest{}, errors.New("context and exact acquisition source are required")
	}
	if limits.shardRaw <= 0 || limits.shardRaw > MaxShardRawBytes || limits.shardEncoded <= 0 ||
		limits.shardEncoded > MaxShardEncodedBytes || limits.totalRaw <= 0 || limits.totalRaw > MaxPackRawBytes ||
		limits.totalEncoded <= 0 || limits.totalEncoded > MaxPackEncodedBytes || limits.items <= 0 || limits.items > lyricscontract.MaxPackItems {
		return Manifest{}, errors.New("evidence pack build limits are invalid")
	}
	selected, err := canonicalSelection(selectedInput)
	if err != nil {
		return Manifest{}, err
	}
	if len(selected) > limits.items {
		return Manifest{}, errors.New("selected evidence exceeds the private item ceiling")
	}
	evidence := make([]lyricssource.IndexEvidence, 0, len(selected))
	var rawTotal int64
	for _, ref := range selected {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		item, err := resolveExactAcquisition(ctx, source, ref)
		if err != nil {
			return Manifest{}, err
		}
		if int64(len(item.Raw)) > limits.totalRaw-rawTotal {
			return Manifest{}, errors.New("selected acquisition union exceeds the total private raw-byte ceiling")
		}
		rawTotal += int64(len(item.Raw))
		evidence = append(evidence, item)
	}
	plans, encodedTotal, err := planShards(evidence, selected, limits)
	if err != nil {
		return Manifest{}, err
	}
	selectionDigest, err := lyricscontract.OrderedSelectionSHA256(selected)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		SchemaVersion: SchemaVersionV1, CanonicalEncoding: CanonicalEncodingV1, DigestAlgorithm: DigestAlgorithmV1,
		SelectionSHA256: selectionDigest, Selected: append([]lyricscontract.EvidenceRef{}, selected...),
		Shards: make([]ShardManifest, len(plans)),
		Totals: Totals{ItemCount: len(selected), ShardCount: len(plans), RawByteCount: rawTotal, EncodedByteCount: encodedTotal},
	}
	for index, plan := range plans {
		manifest.Shards[index] = plan.manifest
	}
	manifest.PackSHA256, err = manifestDigest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifestBody, err := MarshalManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if encodedTotal+int64(len(manifestBody)) > limits.totalEncoded {
		return Manifest{}, errors.New("evidence pack exceeds the total private encoded-byte ceiling")
	}
	private, err := openPrivateDirectory(directory, true)
	if err != nil {
		return Manifest{}, err
	}
	defer private.close()
	if err := private.lock(); err != nil {
		return Manifest{}, err
	}
	defer private.unlock()
	if err := rejectUnexpectedBuildEntries(private, plans); err != nil {
		return Manifest{}, err
	}
	published := make(map[string]publishedEvidenceFile, len(plans)+1)
	for _, plan := range plans {
		name, _ := ShardFileName(plan.manifest.Ordinal, plan.manifest.SHA256)
		plan := plan
		identity, err := publishFile(private, name, fmt.Sprintf("evidence shard %d", plan.manifest.Ordinal),
			plan.manifest.EncodedByteCount, plan.manifest.SHA256, true,
			func(writer io.Writer) error {
				return writeShard(writer, plan.manifest.Ordinal, evidence[plan.start:plan.end])
			})
		if err != nil {
			return Manifest{}, err
		}
		published[name] = publishedEvidenceFile{
			label: fmt.Sprintf("evidence shard %d", plan.manifest.Ordinal), sha256: plan.manifest.SHA256,
			bytes: plan.manifest.EncodedByteCount, identity: identity,
		}
	}
	if testHookBeforeManifest != nil {
		if err := testHookBeforeManifest(); err != nil {
			return Manifest{}, err
		}
	}
	manifestPhysicalSHA := sha256Hex(manifestBody)
	manifestIdentity, err := publishFile(private, ManifestFileName, "evidence pack manifest", len(manifestBody), manifestPhysicalSHA, false,
		func(writer io.Writer) error {
			_, err := writer.Write(manifestBody)
			return err
		})
	if err != nil {
		return Manifest{}, err
	}
	published[ManifestFileName] = publishedEvidenceFile{
		label: "evidence pack manifest", sha256: manifestPhysicalSHA, bytes: len(manifestBody), identity: manifestIdentity,
	}
	if err := verifyCompletedEntries(private, published); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func planShards(evidence []lyricssource.IndexEvidence, selected []lyricscontract.EvidenceRef, limits buildLimits) ([]plannedShard, int64, error) {
	if len(evidence) == 0 {
		return []plannedShard{}, 0, nil
	}
	itemSizes := make([]int, len(evidence))
	for index, item := range evidence {
		body, err := json.Marshal(item)
		if err != nil {
			return nil, 0, err
		}
		if len(body) == 0 || len(body) > MaxEnvelopeEncodedBytes {
			return nil, 0, fmt.Errorf("evidence %q exceeds its encoded envelope bound", item.EvidenceID)
		}
		itemSizes[index] = len(body)
	}
	plans := make([]plannedShard, 0)
	start := 0
	var encodedTotal int64
	for start < len(evidence) {
		ordinal := len(plans)
		encoded := shardEnvelopeOverhead(ordinal)
		raw := 0
		end := start
		for end < len(evidence) {
			separator := 0
			if end > start {
				separator = 1
			}
			nextEncoded := encoded + separator + itemSizes[end]
			nextRaw := raw + len(evidence[end].Raw)
			if nextRaw > limits.shardRaw || nextEncoded > limits.shardEncoded {
				break
			}
			encoded = nextEncoded
			raw = nextRaw
			end++
		}
		if end == start {
			return nil, 0, fmt.Errorf("evidence %q cannot fit a bounded shard", evidence[start].EvidenceID)
		}
		measuredBytes, digest, err := measureShard(ordinal, evidence[start:end])
		if err != nil {
			return nil, 0, err
		}
		if measuredBytes != encoded {
			return nil, 0, errors.New("evidence shard encoded planning drifted")
		}
		items := append([]lyricscontract.EvidenceRef(nil), selected[start:end]...)
		manifest := ShardManifest{
			Ordinal: ordinal, SHA256: digest, EncodedByteCount: encoded, RawByteCount: raw, ItemCount: end - start,
			FirstEvidenceID: items[0].EvidenceID, LastEvidenceID: items[len(items)-1].EvidenceID, Items: items,
		}
		plans = append(plans, plannedShard{manifest: manifest, start: start, end: end})
		encodedTotal += int64(encoded)
		if encodedTotal > limits.totalEncoded {
			return nil, 0, errors.New("evidence shards exceed the total private encoded-byte ceiling")
		}
		start = end
	}
	return plans, encodedTotal, nil
}

func shardEnvelopeOverhead(ordinal int) int {
	return len(fmt.Sprintf(`{"schemaVersion":%d,"ordinal":%d,"items":[]}`, SchemaVersionV1, ordinal))
}

func measureShard(ordinal int, evidence []lyricssource.IndexEvidence) (int, string, error) {
	digest := sha256.New()
	counter := &countHashWriter{hash: digest}
	if err := writeShard(counter, ordinal, evidence); err != nil {
		return 0, "", err
	}
	return counter.count, hex.EncodeToString(digest.Sum(nil)), nil
}

func writeShard(writer io.Writer, ordinal int, evidence []lyricssource.IndexEvidence) error {
	if _, err := fmt.Fprintf(writer, `{"schemaVersion":%d,"ordinal":%d,"items":[`, SchemaVersionV1, ordinal); err != nil {
		return err
	}
	for index, item := range evidence {
		if index > 0 {
			if _, err := io.WriteString(writer, ","); err != nil {
				return err
			}
		}
		body, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := writer.Write(body); err != nil {
			return err
		}
	}
	_, err := io.WriteString(writer, "]}")
	return err
}

func rejectUnexpectedBuildEntries(directory *privateDirectory, plans []plannedShard) error {
	expected := map[string]struct{}{
		ManifestFileName: {}, "." + ManifestFileName + ".tmp": {},
	}
	for _, plan := range plans {
		name, _ := ShardFileName(plan.manifest.Ordinal, plan.manifest.SHA256)
		expected[name] = struct{}{}
		expected["."+name+".tmp"] = struct{}{}
	}
	entries, err := boundedEntries(directory, len(expected)+1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, allowed := expected[entry.Name()]; !allowed {
			return fmt.Errorf("private evidence pack directory contains unexpected entry %q", entry.Name())
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return fmt.Errorf("private evidence pack entry %q is not a direct regular file", entry.Name())
		}
	}
	if final, err := evidenceStatAt(directory.file, ManifestFileName); err == nil {
		temp, tempErr := evidenceStatAt(directory.file, "."+ManifestFileName+".tmp")
		if tempErr != nil || !sameEvidenceMetadata(final, temp) {
			return ErrAlreadyPublished
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type publishedEvidenceFile struct {
	label    string
	sha256   string
	bytes    int
	identity evidenceFileIdentity
}

func verifyCompletedEntries(directory *privateDirectory, expected map[string]publishedEvidenceFile) error {
	if err := verifyPublishedEvidenceFiles(directory, expected, "completed evidence pack directory"); err != nil {
		return err
	}
	if err := directory.sync(); err != nil {
		return err
	}
	return verifyPublishedEvidenceFiles(directory, expected, "completed evidence pack directory")
}

func verifyPublishedEvidenceFiles(directory *privateDirectory, expected map[string]publishedEvidenceFile, label string) error {
	names := make(map[string]struct{}, len(expected))
	for name := range expected {
		names[name] = struct{}{}
	}
	if err := verifyExactEvidenceEntries(directory, names, label); err != nil {
		return err
	}
	for name, publication := range expected {
		_, identity, err := readVerifiedFile(directory, name, publication.label, publication.sha256,
			publication.bytes, publication.bytes, 1)
		if err != nil {
			return err
		}
		if !sameEvidenceMetadata(publication.identity, identity) {
			return fmt.Errorf("%s entry %q changed inode after publication", label, name)
		}
	}
	return directory.verifyPath()
}

func verifyExactEvidenceEntries(directory *privateDirectory, expected map[string]struct{}, label string) error {
	entries, err := boundedEntries(directory, len(expected)+1)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("%s has missing or extra entries", label)
	}
	for _, entry := range entries {
		if _, found := expected[entry.Name()]; !found || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("%s has an invalid entry", label)
		}
		identity, err := evidenceStatAt(directory.file, entry.Name())
		if err != nil {
			return fmt.Errorf("%s has an invalid entry: %w", label, err)
		}
		if err := validateEvidenceRegularIdentity(identity, label+" entry "+entry.Name(), 1); err != nil {
			return err
		}
	}
	return directory.verifyPath()
}

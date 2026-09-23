package lyricsacquisition

import (
	"context"

	"errors"
	"fmt"
	"os"
	"reflect"

	"sort"
	"strings"
)

func (s *spool) recoverPending(ctx context.Context) error {
	if err := s.root.verifyDirectory(pendingDirectory); err != nil {
		return err
	}
	if err := s.validateReviewedHardlinkGraph(); err != nil {
		return fmt.Errorf("revalidate reviewed lyrics acquisition leaves before pending recovery: %w", err)
	}
	if err := s.recoverMarkerTemps(); err != nil {
		return err
	}
	entries, err := sortedDirectoryEntries(s.root, pendingDirectory, maxRetainedPendingEntries)
	if err != nil {
		return fmt.Errorf("list pending acquisition markers: %w", err)
	}
	markerNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		match := pendingEntryPattern.FindStringSubmatch(entry.Name())
		if match == nil {
			return fmt.Errorf("pending acquisition directory contains unknown entry %q", entry.Name())
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return fmt.Errorf("pending acquisition entry %q is not a direct regular file", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			markerNames = append(markerNames, entry.Name())
		}
	}
	if len(markerNames) > maxRetainedPendingMarkers {
		return errors.New("retained acquisition marker count exceeds the bounded recovery capacity")
	}
	sort.Strings(markerNames)
	for _, name := range markerNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.recoverMarker(ctx, strings.TrimSuffix(name, ".json")); err != nil {
			return err
		}
	}
	remaining, err := sortedDirectoryEntries(s.root, pendingDirectory, maxRetainedPendingEntries)
	if err != nil {
		return err
	}
	if len(remaining) > maxRetainedPendingEntries {
		return errors.New("retained acquisition recovery state exceeds its bounded capacity")
	}
	return s.validateReviewedHardlinkGraph()
}

func (s *spool) recoverMarkerTemps() error {
	entries, err := sortedDirectoryEntries(s.root, pendingDirectory, maxRetainedPendingEntries)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".marker.tmp") {
			continue
		}
		match := pendingEntryPattern.FindStringSubmatch(entry.Name())
		if match == nil || match[2] != "marker" {
			return errors.New("pending marker temporary name is invalid")
		}
		acquisitionID := match[1]
		body, _, err := readVerifiedFileAt(s.root, pendingDirectory, entry.Name(), "staged pending acquisition marker", "", -1, maxPendingMarkerBytes, 1, 2)
		if err != nil {
			return err
		}
		marker, err := decodePendingMarker(body)
		if err != nil || marker.AcquisitionID != acquisitionID {
			return errors.New("staged pending acquisition marker is not bound to its name")
		}
		if _, err := publishNoOverwrite(s.root, entry.Name(), pendingDirectory, acquisitionID+".json", "pending acquisition marker", sha256Hex(body), len(body), maxPendingMarkerBytes); err != nil {
			return err
		}
	}
	return nil
}

func (s *spool) recoverMarker(ctx context.Context, acquisitionID string) error {
	markerBody, markerInfo, err := readVerifiedFileAt(s.root, pendingDirectory, acquisitionID+".json", "pending acquisition marker", "", -1, maxPendingMarkerBytes, 1, 2)
	if err != nil {
		return err
	}
	marker, err := decodePendingMarker(markerBody)
	if err != nil || marker.AcquisitionID != acquisitionID {
		return errors.New("pending acquisition marker is not bound to its content-addressed name")
	}
	rawReady, err := s.recoverPublication(acquisitionID, "raw", blobsDirectory, marker.RawSHA256,
		"raw acquisition blob", marker.RawSHA256, marker.RawByteCount, maxRawResponseBytes)
	if err != nil {
		return err
	}
	evidenceReady := rawReady && marker.EvidenceSHA256 == marker.RawSHA256 && marker.EvidenceByteCount == marker.RawByteCount
	if marker.EvidenceSHA256 != marker.RawSHA256 {
		evidenceReady, err = s.recoverPublication(acquisitionID, "evidence", blobsDirectory, marker.EvidenceSHA256,
			"evidence acquisition blob", marker.EvidenceSHA256, marker.EvidenceByteCount, maxEvidenceProjectionBytes)
		if err != nil {
			return err
		}
	}
	envelopeReady, err := s.recoverPublication(acquisitionID, "envelope", blobsDirectory, marker.EnvelopeSHA256,
		"evidence envelope acquisition blob", marker.EnvelopeSHA256, marker.EnvelopeByteCount, maxEvidenceEnvelopeBytes)
	if err != nil {
		return err
	}
	manifestReady, err := s.recoverPublication(acquisitionID, "manifest", manifestsDirectory, acquisitionID+".json",
		"acquisition manifest", acquisitionID, marker.ManifestByteCount, maxManifestBytes)
	if err != nil {
		return err
	}
	if !rawReady || !evidenceReady || !envelopeReady || !manifestReady {
		return s.discardIncompleteMarker(acquisitionID, markerBody, markerInfo)
	}
	manifestBody, _, err := readVerifiedFileAt(s.root, manifestsDirectory, acquisitionID+".json",
		"acquisition manifest", acquisitionID, marker.ManifestByteCount, maxManifestBytes, 1, 2)
	if err != nil {
		return err
	}
	manifest, err := decodeManifest(manifestBody)
	if err != nil {
		return err
	}
	if err := validateMarkerManifest(marker, manifest, manifestBody); err != nil {
		return err
	}
	if err := s.insertMetadata(ctx, manifest, acquisitionID, marker.RequestKey, len(manifestBody)); err != nil {
		return err
	}
	return s.removeMarker(acquisitionID, markerBody, markerInfo)
}

func (s *spool) recoverPublication(acquisitionID, role, targetDirectory, targetName, label, expectedSHA string, expectedBytes, maximum int) (bool, error) {
	if err := s.root.verifyDirectory(pendingDirectory); err != nil {
		return false, err
	}
	if err := s.root.verifyDirectory(targetDirectory); err != nil {
		return false, err
	}
	stagedName := acquisitionID + "." + role + ".tmp"
	targetFile := s.root.directories[targetDirectory].file
	pendingFile := s.root.directories[pendingDirectory].file
	targetStat, targetErr := statAt(targetFile, targetName)
	stagedStat, stagedErr := statAt(pendingFile, stagedName)
	if targetErr == nil {
		if err := s.root.verifyKnownLeaf(targetDirectory, targetName, targetStat.identity); err != nil {
			return false, err
		}
	}
	if stagedErr == nil {
		if err := s.root.verifyKnownLeaf(pendingDirectory, stagedName, stagedStat.identity); err != nil {
			return false, err
		}
	}
	if errors.Is(targetErr, os.ErrNotExist) && errors.Is(stagedErr, os.ErrNotExist) {
		return false, nil
	}
	if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return false, targetErr
	}
	if stagedErr != nil && !errors.Is(stagedErr, os.ErrNotExist) {
		return false, stagedErr
	}
	if errors.Is(targetErr, os.ErrNotExist) {
		if _, err := publishNoOverwrite(s.root, stagedName, targetDirectory, targetName, label, expectedSHA, expectedBytes, maximum); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(stagedErr, os.ErrNotExist) {
		if _, err := publishNoOverwrite(s.root, stagedName, targetDirectory, targetName, label, expectedSHA, expectedBytes, maximum); err != nil {
			return false, err
		}
	}
	_, _, err := readVerifiedFileAt(s.root, targetDirectory, targetName, label, expectedSHA, expectedBytes, maximum, 1, 2)
	return err == nil, err
}

func (s *spool) discardIncompleteMarker(acquisitionID string, markerBody []byte, markerInfo os.FileInfo) error {
	pendingFile := s.root.directories[pendingDirectory].file
	maximums := map[string]int{
		"raw": maxRawResponseBytes, "evidence": maxEvidenceProjectionBytes,
		"envelope": maxEvidenceEnvelopeBytes, "manifest": maxManifestBytes,
	}
	for _, role := range []string{"raw", "evidence", "envelope", "manifest"} {
		name := acquisitionID + "." + role + ".tmp"
		if _, err := statAt(pendingFile, name); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		_, info, err := readVerifiedFileAt(s.root, pendingDirectory, name, "incomplete staged acquisition file", "", -1, maximums[role], 1, 2)
		if err != nil {
			return err
		}
		if err := retireOwnedFile(s.root, pendingDirectory, name, info, 1, 2); err != nil {
			return fmt.Errorf("quarantine incomplete %s acquisition stage: %w", role, err)
		}
	}
	return s.removeMarker(acquisitionID, markerBody, markerInfo)
}

func (s *spool) loadManifestArtifacts(manifest acquisitionManifest) (validatedProviderResponse, error) {
	raw, _, err := readVerifiedFileAt(s.root, blobsDirectory, manifest.RawResponse.SHA256,
		"raw acquisition blob", manifest.RawResponse.SHA256, manifest.RawResponse.ByteCount, maxRawResponseBytes, 1, 2)
	if err != nil {
		return validatedProviderResponse{}, err
	}
	evidence := raw
	if manifest.EvidenceProjection.SHA256 != manifest.RawResponse.SHA256 {
		evidence, _, err = readVerifiedFileAt(s.root, blobsDirectory, manifest.EvidenceProjection.SHA256,
			"evidence acquisition blob", manifest.EvidenceProjection.SHA256, manifest.EvidenceProjection.ByteCount, maxEvidenceProjectionBytes, 1, 2)
		if err != nil {
			return validatedProviderResponse{}, err
		}
	} else if manifest.EvidenceProjection.ByteCount != manifest.RawResponse.ByteCount {
		return validatedProviderResponse{}, errors.New("shared raw/evidence blob has conflicting byte counts")
	}
	envelope, _, err := readVerifiedFileAt(s.root, blobsDirectory, manifest.EvidenceEnvelope.SHA256,
		"evidence envelope acquisition blob", manifest.EvidenceEnvelope.SHA256, manifest.EvidenceEnvelope.ByteCount, maxEvidenceEnvelopeBytes, 1, 2)
	if err != nil {
		return validatedProviderResponse{}, err
	}
	response := manifest.response(raw, evidence, envelope)
	if err := validateResponse(response); err != nil {
		return validatedProviderResponse{}, err
	}
	return response, nil
}

func (s *spool) reconcileMetadataFromManifests(ctx context.Context) error {
	if err := s.root.verifyDirectory(manifestsDirectory); err != nil {
		return err
	}
	if err := s.root.verifyDirectory(blobsDirectory); err != nil {
		return err
	}
	entries, err := sortedDirectoryEntries(s.root, manifestsDirectory, maxAcquisitions+maxPendingAcquisitions)
	if err != nil {
		return err
	}
	metadataRows := make([]acquisitionMetadata, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return fmt.Errorf("manifest directory contains non-content-addressed entry %q", entry.Name())
		}
		acquisitionID := strings.TrimSuffix(entry.Name(), ".json")
		if !canonicalSHA256.MatchString(acquisitionID) {
			return fmt.Errorf("manifest directory contains non-content-addressed entry %q", entry.Name())
		}
		body, _, err := readVerifiedFileAt(s.root, manifestsDirectory, entry.Name(),
			"content-addressed acquisition manifest", acquisitionID, -1, maxManifestBytes, 1, 2)
		if err != nil {
			return err
		}
		manifest, err := decodeManifest(body)
		if err != nil {
			return err
		}
		if _, err := s.loadManifestArtifacts(manifest); err != nil {
			return fmt.Errorf("validate content-addressed acquisition %s: %w", acquisitionID, err)
		}
		key, err := requestKey(manifest.response(nil, nil, nil).request)
		if err != nil {
			return err
		}
		metadataRows = append(metadataRows, metadataFromManifest(manifest, acquisitionID, key, len(body)))
	}
	return s.insertMetadataBatch(ctx, metadataRows, "content-addressed manifest reconciliation")
}

func (s *spool) loadAcquisition(ctx context.Context, acquisitionID string, replayOnly bool) (acquiredProviderResponse, error) {
	if !canonicalSHA256.MatchString(acquisitionID) {
		return acquiredProviderResponse{}, errors.New("acquisition ID is invalid")
	}
	if err := s.root.verifyDirectory(manifestsDirectory); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.root.verifyDirectory(blobsDirectory); err != nil {
		return acquiredProviderResponse{}, err
	}
	metadata, err := s.metadataByID(ctx, acquisitionID)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	manifestBody, _, err := readVerifiedFileAt(s.root, manifestsDirectory, acquisitionID+".json", "acquisition manifest", acquisitionID, metadata.manifestByteCount, maxManifestBytes, 1, 2)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	manifest, err := decodeManifest(manifestBody)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	key, err := requestKey(manifest.response(nil, nil, nil).request)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	wantMetadata := metadataFromManifest(manifest, acquisitionID, key, len(manifestBody))
	if !reflect.DeepEqual(metadata, wantMetadata) {
		return acquiredProviderResponse{}, errors.New("acquisition manifest does not match its exact metadata row")
	}
	response, err := s.loadManifestArtifacts(manifest)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	return manifest.acquired(acquisitionID, response.rawResponse, response.evidence.raw, response.envelope.raw, replayOnly), nil
}

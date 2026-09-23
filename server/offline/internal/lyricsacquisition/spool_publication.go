package lyricsacquisition

import (
	"bytes"

	"errors"
	"fmt"
	"os"

	"strings"
)

type contentAddressedUsage struct {
	blobCount     int
	blobBytes     int64
	manifestCount int
	manifestBytes int64
}

func (s *spool) ensurePublicationCapacity(response validatedProviderResponse, acquisitionID string, manifestBody []byte) error {
	usage, err := s.readContentAddressedUsage()
	if err != nil {
		return err
	}
	type prospectiveBlob struct {
		body    []byte
		maximum int
	}
	blobs := map[string]prospectiveBlob{}
	for _, prospective := range []struct {
		digest  string
		body    []byte
		maximum int
	}{
		{digest: response.rawResponseSHA256, body: response.rawResponse, maximum: maxRawResponseBytes},
		{digest: response.evidence.rawSHA256, body: response.evidence.raw, maximum: maxEvidenceProjectionBytes},
		{digest: response.envelope.sha256, body: response.envelope.raw, maximum: maxEvidenceEnvelopeBytes},
	} {
		if existing, duplicate := blobs[prospective.digest]; duplicate {
			if !bytes.Equal(existing.body, prospective.body) {
				return errors.New("one content address cannot bind different acquisition bytes")
			}
			if prospective.maximum > existing.maximum {
				existing.maximum = prospective.maximum
				blobs[prospective.digest] = existing
			}
			continue
		}
		blobs[prospective.digest] = prospectiveBlob{body: prospective.body, maximum: prospective.maximum}
	}
	for digest, prospective := range blobs {
		body, _, readErr := readVerifiedFileAt(s.root, blobsDirectory, digest, "existing content-addressed acquisition blob", digest, len(prospective.body), prospective.maximum, 1, 2)
		switch {
		case readErr == nil:
			if !bytes.Equal(body, prospective.body) {
				return errors.New("existing content-addressed acquisition blob changed bytes")
			}
		case errors.Is(readErr, os.ErrNotExist):
			usage.blobCount++
			usage.blobBytes += int64(len(prospective.body))
		default:
			return readErr
		}
	}
	existingManifest, _, manifestErr := readVerifiedFileAt(s.root, manifestsDirectory, acquisitionID+".json", "existing content-addressed acquisition manifest",
		acquisitionID, len(manifestBody), maxManifestBytes, 1, 2)
	switch {
	case manifestErr == nil:
		if !bytes.Equal(existingManifest, manifestBody) {
			return errors.New("existing content-addressed acquisition manifest changed bytes")
		}
	case errors.Is(manifestErr, os.ErrNotExist):
		usage.manifestCount++
		usage.manifestBytes += int64(len(manifestBody))
	default:
		return manifestErr
	}
	if usage.blobCount > maxAcquisitions*3+maxPendingAcquisitions*3 ||
		usage.blobBytes > maxAggregateRawBytes+maxAggregateEvidence+maxAggregateEnvelope ||
		usage.manifestCount > maxAcquisitions+maxPendingAcquisitions || usage.manifestBytes > maxAggregateManifest {
		return errors.New("lyrics acquisition publication exceeds its bounded v2 physical capacity")
	}
	return nil
}

func (s *spool) readContentAddressedUsage() (contentAddressedUsage, error) {
	var usage contentAddressedUsage
	blobEntries, err := sortedDirectoryEntries(s.root, blobsDirectory, maxAcquisitions*3+maxPendingAcquisitions*3)
	if err != nil {
		return usage, err
	}
	blobDirectory := s.root.directories[blobsDirectory].file
	for _, entry := range blobEntries {
		if !canonicalSHA256.MatchString(entry.Name()) {
			return usage, fmt.Errorf("blob directory contains non-content-addressed entry %q", entry.Name())
		}
		stat, err := statAt(blobDirectory, entry.Name())
		if err != nil {
			return usage, err
		}
		if err := s.root.verifyKnownLeaf(blobsDirectory, entry.Name(), stat.identity); err != nil {
			return usage, err
		}
		if err := validatePrivateRegularStat(stat, "content-addressed acquisition blob", 1, 2); err != nil {
			return usage, err
		}
		if stat.size <= 0 || stat.size > maxEvidenceEnvelopeBytes {
			return usage, errors.New("content-addressed acquisition blob has an invalid byte count")
		}
		usage.blobCount++
		usage.blobBytes += stat.size
		if usage.blobBytes > maxAggregateRawBytes+maxAggregateEvidence+maxAggregateEnvelope {
			return usage, errors.New("physical acquisition blobs exceed the bounded v2 capacity")
		}
	}
	manifestEntries, err := sortedDirectoryEntries(s.root, manifestsDirectory, maxAcquisitions+maxPendingAcquisitions)
	if err != nil {
		return usage, err
	}
	manifestDirectory := s.root.directories[manifestsDirectory].file
	for _, entry := range manifestEntries {
		if !strings.HasSuffix(entry.Name(), ".json") || !canonicalSHA256.MatchString(strings.TrimSuffix(entry.Name(), ".json")) {
			return usage, fmt.Errorf("manifest directory contains non-content-addressed entry %q", entry.Name())
		}
		stat, err := statAt(manifestDirectory, entry.Name())
		if err != nil {
			return usage, err
		}
		if err := s.root.verifyKnownLeaf(manifestsDirectory, entry.Name(), stat.identity); err != nil {
			return usage, err
		}
		if err := validatePrivateRegularStat(stat, "content-addressed acquisition manifest", 1, 2); err != nil {
			return usage, err
		}
		if stat.size <= 0 || stat.size > maxManifestBytes {
			return usage, errors.New("content-addressed acquisition manifest has an invalid byte count")
		}
		usage.manifestCount++
		usage.manifestBytes += stat.size
		if usage.manifestBytes > maxAggregateManifest {
			return usage, errors.New("physical acquisition manifests exceed the bounded v2 capacity")
		}
	}
	if err := s.root.verifyDirectory(blobsDirectory); err != nil {
		return usage, err
	}
	if err := s.root.verifyDirectory(manifestsDirectory); err != nil {
		return usage, err
	}
	return usage, nil
}

func (s *spool) afterStage(stage string) error {
	if s.hooks.afterStage == nil {
		return nil
	}
	return s.hooks.afterStage(stage)
}

func (s *spool) publishMarker(acquisitionID string, body []byte) (os.FileInfo, error) {
	staged := acquisitionID + ".marker.tmp"
	target := acquisitionID + ".json"
	if _, err := writeStagedFile(s.root, staged, body); err != nil {
		return nil, err
	}
	return publishNoOverwrite(s.root, staged, pendingDirectory, target, "pending acquisition marker", sha256Hex(body), len(body), maxPendingMarkerBytes)
}

func (s *spool) publishBlob(acquisitionID, role string, body []byte, digest string, maximum int) error {
	ready, err := s.existingPublication(blobsDirectory, digest, role+" acquisition blob", digest, body, maximum)
	if err != nil || ready {
		return err
	}
	staged := acquisitionID + "." + role + ".tmp"
	if _, err := writeStagedFile(s.root, staged, body); err != nil {
		return err
	}
	_, err = publishNoOverwrite(s.root, staged, blobsDirectory, digest, role+" acquisition blob", digest, len(body), maximum)
	return err
}

func (s *spool) publishManifest(acquisitionID string, body []byte) error {
	ready, err := s.existingPublication(manifestsDirectory, acquisitionID+".json", "acquisition manifest", acquisitionID, body, maxManifestBytes)
	if err != nil || ready {
		return err
	}
	staged := acquisitionID + ".manifest.tmp"
	if _, err := writeStagedFile(s.root, staged, body); err != nil {
		return err
	}
	_, err = publishNoOverwrite(s.root, staged, manifestsDirectory, acquisitionID+".json", "acquisition manifest", acquisitionID, len(body), maxManifestBytes)
	return err
}

func (s *spool) existingPublication(directory, name, label, digest string, body []byte, maximum int) (bool, error) {
	existing, _, err := readVerifiedFileAt(s.root, directory, name, "existing "+label, digest, len(body), maximum, 1, 2)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(existing, body) {
		return false, fmt.Errorf("existing %s changed bytes", label)
	}
	return true, nil
}

func (s *spool) removeMarker(acquisitionID string, expectedBody []byte, expectedInfo os.FileInfo) error {
	if expectedInfo == nil || len(expectedBody) == 0 {
		return errors.New("exact pending acquisition marker identity is required for removal")
	}
	name := acquisitionID + ".json"
	body, info, err := readVerifiedFileAt(s.root, pendingDirectory, name, "pending acquisition marker", sha256Hex(expectedBody), len(expectedBody), maxPendingMarkerBytes, 1, 2)
	if err != nil {
		return err
	}
	infoIdentity, infoOK := fileIdentityFromFileInfo(info)
	expectedIdentity, expectedOK := fileIdentityFromFileInfo(expectedInfo)
	marker, err := decodePendingMarker(body)
	if err != nil || marker.AcquisitionID != acquisitionID || !bytes.Equal(body, expectedBody) || !infoOK || !expectedOK ||
		!sameFileIdentity(infoIdentity, expectedIdentity) {
		return errors.New("pending acquisition marker changed before owned removal")
	}
	return retireOwnedFile(s.root, pendingDirectory, name, info, 1, 2)
}

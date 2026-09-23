package lyricsacquisition

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"regexp"

	"strings"
	"sync"
)

const (
	maxRetainedPendingMarkers = maxAcquisitions + maxPendingAcquisitions
	maxRetainedPendingEntries = maxRetainedPendingMarkers * 6

	commitStageMarker   = "pending_marker"
	commitStageRaw      = "raw_blob"
	commitStageEvidence = "evidence_blob"
	commitStageEnvelope = "evidence_envelope_blob"
	commitStageManifest = "manifest"
	commitStageMetadata = "metadata"
)

var pendingEntryPattern = regexp.MustCompile(`^([0-9a-f]{64})(?:\.json|\.(marker|raw|evidence|envelope|manifest)\.tmp)$`)

type spoolHooks struct {
	// afterStage runs after one named immutable acquisition publication stage.
	afterStage func(string) error
	// metadataBoundary receives only validated, uniquely mapped persistence
	// boundaries immediately before or after the operation named by the boundary.
	metadataBoundary func(metadataPersistenceBoundary) error
	// metadataSnapshotWriteAt replaces only the snapshot's os.File.WriteAt call
	// so short and partial write behavior can be tested independently of boundaries.
	metadataSnapshotWriteAt func(*os.File, []byte, int64) (int, error)
}

type spool struct {
	mu sync.Mutex

	root *privateRoot

	metadataFile          *os.File
	metadataBinding       *pinnedMetadataBinding
	metadataStandbyFile   *os.File
	metadataStandbyStat   trustedStat
	metadataStandbyDigest string
	metadataStandbyPath   string
	metadataWriteFile     *os.File
	metadataWriteStat     trustedStat
	metadataWritePath     string
	metadataConnector     *ledgerSQLiteConnector
	metadataAnchor        *sql.Conn
	database              *sql.DB

	hooks   spoolHooks
	failure error
	closed  bool
}

func openSpool(ctx context.Context, rootPath string) (*spool, error) {
	return openSpoolWithHooks(ctx, rootPath, spoolHooks{})
}

func createSpool(ctx context.Context, rootPath string) (*spool, error) {
	return openSpoolMode(ctx, rootPath, spoolHooks{}, true, false)
}

func openExistingSpool(ctx context.Context, rootPath string) (*spool, error) {
	return openSpoolMode(ctx, rootPath, spoolHooks{}, false, true)
}

func openSpoolWithHooks(ctx context.Context, rootPath string, hooks spoolHooks) (*spool, error) {
	return openSpoolMode(ctx, rootPath, hooks, false, false)
}

func openSpoolMode(ctx context.Context, rootPath string, hooks spoolHooks, mustCreate, mustExist bool) (*spool, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireAtomicNamespacePublication(); err != nil {
		return nil, err
	}
	root, err := openPrivateRoot(rootPath, mustCreate, mustExist)
	if err != nil {
		return nil, err
	}
	opened := &spool{root: root, hooks: hooks}
	fail := func(cause error) (*spool, error) {
		_ = opened.closeResources()
		return nil, cause
	}
	initializeMissing := root.created
	if err := root.acquireLedgerLock(initializeMissing); err != nil {
		return fail(err)
	}
	if err := root.ensureDirectories(initializeMissing); err != nil {
		return fail(err)
	}
	if err := opened.validateRootEntries(initializeMissing, true); err != nil {
		return fail(err)
	}
	if err := opened.openMetadata(ctx, initializeMissing); err != nil {
		return fail(err)
	}
	if err := root.ensureMetadataStateDirectory(); err != nil {
		return fail(fmt.Errorf("prepare bounded lyrics acquisition metadata state: %w", err))
	}
	if err := root.ensureQuarantineDirectory(); err != nil {
		return fail(fmt.Errorf("prepare bounded lyrics acquisition quarantine: %w", err))
	}
	if err := opened.validateRootEntries(false, false); err != nil {
		return fail(fmt.Errorf("revalidate lyrics acquisition root before pending recovery: %w", err))
	}
	if err := opened.captureReviewedLeaves(); err != nil {
		return fail(fmt.Errorf("capture reviewed lyrics acquisition leaf identities: %w", err))
	}
	if err := opened.recoverPending(ctx); err != nil {
		return fail(fmt.Errorf("recover pending lyrics acquisition: %w", err))
	}
	if err := opened.reconcileMetadataFromManifests(ctx); err != nil {
		return fail(fmt.Errorf("reconcile acquisition metadata from content-addressed manifests: %w", err))
	}
	if err := opened.validateState(ctx); err != nil {
		return fail(fmt.Errorf("validate lyrics acquisition spool: %w", err))
	}
	return opened, nil
}

func (s *spool) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var result error
	if s.database != nil {
		result = errors.Join(result, s.syncMetadata("close"))
	}
	if s.metadataAnchor != nil {
		result = errors.Join(result, s.metadataAnchor.Close())
		s.metadataAnchor = nil
	}
	if s.database != nil {
		result = errors.Join(result, s.database.Close())
		s.database = nil
	}
	result = errors.Join(result, s.closeResources())
	return result
}

func (s *spool) closeResources() error {
	if s == nil {
		return nil
	}
	var result error
	if s.metadataAnchor != nil {
		result = errors.Join(result, s.metadataAnchor.Close())
		s.metadataAnchor = nil
	}
	if s.database != nil {
		result = errors.Join(result, s.database.Close())
		s.database = nil
	}
	if s.metadataFile != nil {
		result = errors.Join(result, s.metadataFile.Close())
		s.metadataFile = nil
	}
	if s.metadataStandbyFile != nil {
		result = errors.Join(result, s.metadataStandbyFile.Close())
		s.metadataStandbyFile = nil
	}
	s.metadataStandbyStat = trustedStat{}
	s.metadataStandbyDigest = ""
	s.metadataStandbyPath = ""
	if s.metadataWriteFile != nil {
		result = errors.Join(result, s.metadataWriteFile.Close())
		s.metadataWriteFile = nil
	}
	s.metadataWriteStat = trustedStat{}
	s.metadataWritePath = ""
	s.metadataBinding = nil
	s.metadataConnector = nil
	if s.root != nil {
		result = errors.Join(result, s.root.Close())
		s.root = nil
	}
	return result
}

func (s *spool) commit(ctx context.Context, response validatedProviderResponse) (acquiredProviderResponse, error) {
	if ctx == nil {
		return acquiredProviderResponse{}, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return acquiredProviderResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.database == nil || s.root == nil {
		return acquiredProviderResponse{}, errSpoolClosed
	}
	if s.failure != nil {
		return acquiredProviderResponse{}, s.failure
	}
	if err := s.ensureOperationalIdentity(ctx); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.ensureCommitReady(); err != nil {
		return acquiredProviderResponse{}, err
	}
	manifest, manifestBody, acquisitionID, key, err := buildManifest(response)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.ensureMetadataCapacity(ctx, manifest, acquisitionID, key, len(manifestBody)); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.ensurePublicationCapacity(response, acquisitionID, manifestBody); err != nil {
		return acquiredProviderResponse{}, err
	}
	if _, err := s.metadataByID(ctx, acquisitionID); err == nil {
		acquired, loadErr := s.loadAcquisition(ctx, acquisitionID, false)
		if loadErr != nil {
			return acquiredProviderResponse{}, loadErr
		}
		expected := manifest.acquired(acquisitionID, response.rawResponse, response.evidence.raw, response.envelope.raw, false)
		if acquired.replayOnly || !sameAcquisitionIdentity(acquired, expected) {
			return acquiredProviderResponse{}, errors.New("idempotent acquisition changed identity before return")
		}
		return acquired, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return acquiredProviderResponse{}, fmt.Errorf("inspect idempotent acquisition before publication: %w", err)
	}
	marker, markerBody, err := buildPendingMarker(manifest, acquisitionID, key, len(manifestBody))
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	markerInfo, err := s.publishMarker(acquisitionID, markerBody)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.afterStage(commitStageMarker); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.publishBlob(acquisitionID, "raw", response.rawResponse, response.rawResponseSHA256, maxRawResponseBytes); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.afterStage(commitStageRaw); err != nil {
		return acquiredProviderResponse{}, err
	}
	if response.evidence.rawSHA256 != response.rawResponseSHA256 {
		if err := s.publishBlob(acquisitionID, "evidence", response.evidence.raw, response.evidence.rawSHA256, maxEvidenceProjectionBytes); err != nil {
			return acquiredProviderResponse{}, err
		}
	} else if !bytes.Equal(response.evidence.raw, response.rawResponse) {
		return acquiredProviderResponse{}, errors.New("equal raw and evidence SHA-256 values have different bytes")
	}
	if err := s.afterStage(commitStageEvidence); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.publishBlob(acquisitionID, "envelope", response.envelope.raw, response.envelope.sha256, maxEvidenceEnvelopeBytes); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.afterStage(commitStageEnvelope); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.publishManifest(acquisitionID, manifestBody); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.afterStage(commitStageManifest); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := validateMarkerManifest(marker, manifest, manifestBody); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.insertMetadata(ctx, manifest, acquisitionID, key, len(manifestBody)); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.afterStage(commitStageMetadata); err != nil {
		return acquiredProviderResponse{}, err
	}
	if err := s.removeMarker(acquisitionID, markerBody, markerInfo); err != nil {
		return acquiredProviderResponse{}, err
	}
	acquired, err := s.loadAcquisition(ctx, acquisitionID, false)
	if err != nil {
		return acquiredProviderResponse{}, err
	}
	expected := manifest.acquired(acquisitionID, response.rawResponse, response.evidence.raw, response.envelope.raw, false)
	if acquired.replayOnly || !sameAcquisitionIdentity(acquired, expected) {
		return acquiredProviderResponse{}, errors.New("committed acquisition changed identity before return")
	}
	return acquired, nil
}

func (s *spool) replayByAcquisitionID(ctx context.Context, acquisitionID string) (acquiredProviderResponse, error) {
	if ctx == nil {
		return acquiredProviderResponse{}, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return acquiredProviderResponse{}, err
	}
	if !canonicalSHA256.MatchString(acquisitionID) {
		return acquiredProviderResponse{}, errors.New("acquisition ID is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.database == nil || s.root == nil {
		return acquiredProviderResponse{}, errSpoolClosed
	}
	if s.failure != nil {
		return acquiredProviderResponse{}, s.failure
	}
	if err := s.ensureOperationalIdentity(ctx); err != nil {
		return acquiredProviderResponse{}, err
	}
	return s.loadAcquisition(ctx, acquisitionID, true)
}

func (s *spool) captureReviewedLeaves() error {
	for _, reviewed := range []struct {
		directory string
		maximum   int
	}{
		{directory: pendingDirectory, maximum: maxRetainedPendingEntries},
		{directory: quarantineDirectory, maximum: maxQuarantineEntries},
		{directory: metadataStateDirectory, maximum: maxMetadataStateEntries},
		{directory: blobsDirectory, maximum: maxAcquisitions*3 + maxPendingAcquisitions*3},
		{directory: manifestsDirectory, maximum: maxAcquisitions + maxPendingAcquisitions},
	} {
		if err := s.root.captureDirectoryLeaves(reviewed.directory, reviewed.maximum); err != nil {
			return err
		}
	}
	return s.validateReviewedHardlinkGraph()
}

type reviewedLeafLocation struct {
	directory string
	name      string
	stat      trustedStat
}

func (s *spool) validateReviewedHardlinkGraph() error {
	groups := make(map[fileIdentity][]reviewedLeafLocation)
	for _, reviewed := range []struct {
		directory string
		maximum   int
	}{
		{directory: pendingDirectory, maximum: maxRetainedPendingEntries},
		{directory: quarantineDirectory, maximum: maxQuarantineEntries},
		{directory: metadataStateDirectory, maximum: maxMetadataStateEntries},
		{directory: blobsDirectory, maximum: maxAcquisitions*3 + maxPendingAcquisitions*3},
		{directory: manifestsDirectory, maximum: maxAcquisitions + maxPendingAcquisitions},
	} {
		entries, err := sortedDirectoryEntries(s.root, reviewed.directory, reviewed.maximum)
		if err != nil {
			return err
		}
		directoryFile := s.root.directories[reviewed.directory].file
		for _, entry := range entries {
			if reviewed.directory == quarantineDirectory && pendingEntryPattern.FindStringSubmatch(entry.Name()) == nil {
				return fmt.Errorf("lyrics acquisition quarantine contains unknown entry %q", entry.Name())
			}
			if reviewed.directory == metadataStateDirectory && !metadataSelectorNameAllowed(entry.Name()) {
				return fmt.Errorf("lyrics acquisition metadata state contains unknown entry %q", entry.Name())
			}
			stat, err := statAt(directoryFile, entry.Name())
			if err != nil {
				return err
			}
			if err := s.root.verifyKnownLeaf(reviewed.directory, entry.Name(), stat.identity); err != nil {
				return err
			}
			if stat.size < 0 || stat.size > maxEvidenceEnvelopeBytes {
				return errors.New("lyrics acquisition quarantine leaf exceeds its byte bound")
			}
			groups[stat.identity] = append(groups[stat.identity], reviewedLeafLocation{
				directory: reviewed.directory, name: entry.Name(), stat: stat,
			})
		}
	}
	for _, locations := range groups {
		links := locations[0].stat.links
		switch links {
		case 1:
			if len(locations) != 1 {
				return errors.New("reviewed lyrics acquisition leaf identity has an inconsistent single-link graph")
			}
		case 2:
			if len(locations) != 2 || locations[1].stat.links != 2 || !validReviewedPublicationPair(locations[0], locations[1]) {
				return errors.New("reviewed lyrics acquisition hard-link graph is not an exact interrupted publication pair")
			}
		default:
			return errors.New("reviewed lyrics acquisition leaf has an unsupported hard-link count")
		}
	}
	return nil
}

func validReviewedPublicationPair(left, right reviewedLeafLocation) bool {
	return validDirectedPublicationPair(left, right) || validDirectedPublicationPair(right, left)
}

func validDirectedPublicationPair(staged, target reviewedLeafLocation) bool {
	if staged.directory == metadataStateDirectory {
		return target.directory == metadataStateDirectory && strings.HasSuffix(staged.name, ".json.stage") &&
			target.name == strings.TrimSuffix(staged.name, ".stage") && metadataSelectorNamePattern.MatchString(target.name)
	}
	if staged.directory != pendingDirectory && staged.directory != quarantineDirectory {
		return false
	}
	match := pendingEntryPattern.FindStringSubmatch(staged.name)
	if match == nil || match[2] == "" {
		return false
	}
	acquisitionID, role := match[1], match[2]
	switch role {
	case "marker":
		return (target.directory == pendingDirectory || target.directory == quarantineDirectory) && target.name == acquisitionID+".json"
	case "raw", "evidence", "envelope":
		return target.directory == blobsDirectory && canonicalSHA256.MatchString(target.name)
	case "manifest":
		return target.directory == manifestsDirectory && target.name == acquisitionID+".json"
	default:
		return false
	}
}

func (s *spool) ensureCommitReady() error {
	if err := s.root.verifyDirectory(pendingDirectory); err != nil {
		return err
	}
	entries, err := sortedDirectoryEntries(s.root, pendingDirectory, maxRetainedPendingEntries)
	if err != nil {
		return fmt.Errorf("inspect retained acquisition stages before commit: %w", err)
	}
	if len(entries) >= maxRetainedPendingEntries {
		return errors.New("retained acquisition stage capacity is exhausted")
	}
	return s.validateReviewedHardlinkGraph()
}

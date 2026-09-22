package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"moesekai/server/internal/lyricsevidencepack"
	"moesekai/server/internal/lyricsrootmanifest"
	"moesekai/server/internal/model"
)

type recoveryBackupItemIdentity struct {
	batchSHA256 string
	musicID     int
}

type recoveryBackupArtifactIdentity struct {
	batchSHA256  string
	musicID      int
	renditionKey string
}

type recoveryBackupEvidenceIdentity struct {
	provider   string
	evidenceID string
}

func validateRestoredLyricsRecoveryProvenance(
	lyrics LyricsContentExport,
	documentIDs map[int]bool,
	musicIDs map[int]bool,
) error {
	if len(lyrics.RecoveryBatches) == 0 {
		if len(lyrics.RecoveryItems)+len(lyrics.RecoverySourceEvidence)+len(lyrics.RecoveryArtifacts)+
			len(lyrics.RecoveryArtifactEvidence)+len(lyrics.RecoveryContributions)+len(lyrics.AvailabilityDocuments) != 0 {
			return errors.New("lyrics recovery backup has graph rows without a batch")
		}
		return nil
	}

	batches := make(map[string]LyricsRecoveryBatchBackupRecord, len(lyrics.RecoveryBatches))
	coverageByBatch := make(map[string]lyricsrootmanifest.Coverage, len(lyrics.RecoveryBatches))
	for _, record := range lyrics.RecoveryBatches {
		if record.SchemaVersion != 1 || record.RootSchemaVersion != lyricsrootmanifest.SchemaVersionV2 ||
			!isCanonicalContentBackupSHA256(record.BatchSHA256) || !isCanonicalContentBackupSHA256(record.RootSHA256) ||
			!isCanonicalContentBackupSHA256(record.MusicIDsSHA256) ||
			!isCanonicalContentBackupSHA256(record.EvidenceReceiptSHA256) ||
			!isCanonicalContentBackupSHA256(record.PackSHA256) || !isCanonicalContentBackupSHA256(record.SelectionSHA256) ||
			record.RootID == "" || record.RootID != strings.TrimSpace(record.RootID) || record.CatalogCount <= 0 ||
			record.EvidenceCount < 0 || record.ShardCount < 0 || record.RawByteCount < 0 || record.EncodedByteCount < 0 ||
			record.Actor == "" || record.Actor != strings.TrimSpace(record.Actor) || record.CreatedAt <= 0 {
			return fmt.Errorf("lyrics recovery batch %s is invalid", record.BatchSHA256)
		}
		if _, duplicate := batches[record.BatchSHA256]; duplicate {
			return fmt.Errorf("lyrics recovery batch %s is duplicated", record.BatchSHA256)
		}
		var coverage lyricsrootmanifest.Coverage
		// The batch is historical: coverage is checked against the catalog size
		// the batch itself recorded, never against the catalog as it stands
		// today. Item validation below still requires every batch music ID to
		// exist in the exported catalog.
		if err := decodeCanonicalBackupJSON(record.CoverageJSON, &coverage); err != nil ||
			coverage.Total != record.CatalogCount ||
			coverage.UniqueEvidenceCount != record.EvidenceCount ||
			coverage.UniqueAcquisitionCount != record.EvidenceCount ||
			(record.EvidenceCount == 0) != (record.ShardCount == 0 && record.RawByteCount == 0 && record.EncodedByteCount == 0) ||
			(record.EvidenceCount > 0 && (record.ShardCount == 0 || record.RawByteCount == 0 || record.EncodedByteCount == 0)) {
			return fmt.Errorf("lyrics recovery batch %s coverage is invalid", record.BatchSHA256)
		}
		batches[record.BatchSHA256] = record
		coverageByBatch[record.BatchSHA256] = coverage
	}

	catalog := make(map[int]CatalogMusicBackupRecord, len(lyrics.Music))
	for _, record := range lyrics.Music {
		catalog[record.MusicID] = record
	}
	items := make(map[recoveryBackupItemIdentity]LyricsRecoveryItemBackupRecord, len(lyrics.RecoveryItems))
	stateCounts := make(map[string]map[lyricsrootmanifest.CoverageState]int, len(batches))
	for _, record := range lyrics.RecoveryItems {
		_, exists := batches[record.BatchSHA256]
		music := catalog[record.MusicID]
		identity := recoveryBackupItemIdentity{batchSHA256: record.BatchSHA256, musicID: record.MusicID}
		if !exists || !musicIDs[record.MusicID] || record.MusicID <= 0 || record.TargetMusicID != record.MusicID ||
			record.JapaneseTitle != music.TitleJA || record.CatalogFingerprint != music.LyricsCatalogFingerprint ||
			!isCanonicalContentBackupSHA256(record.CatalogFingerprint) ||
			!isCanonicalContentBackupSHA256(record.ResultSHA256) || record.CreatedAt <= 0 {
			return fmt.Errorf("lyrics recovery item %s/%d is invalid", record.BatchSHA256, record.MusicID)
		}
		if _, duplicate := items[identity]; duplicate {
			return fmt.Errorf("lyrics recovery item %s/%d is duplicated", record.BatchSHA256, record.MusicID)
		}
		var associations []int
		if err := decodeCanonicalBackupJSON(record.AssociationMusicIDsJSON, &associations); err != nil || associations == nil ||
			!strictlyIncreasingRecoveryBackupIDs(associations, record.MusicID) {
			return fmt.Errorf("lyrics recovery item %s/%d associations are invalid", record.BatchSHA256, record.MusicID)
		}
		for _, associationMusicID := range associations {
			if !musicIDs[associationMusicID] {
				return fmt.Errorf("lyrics recovery item %s/%d association %d is outside the catalog",
					record.BatchSHA256, record.MusicID, associationMusicID)
			}
		}
		state := lyricsrootmanifest.CoverageState(record.State)
		switch state {
		case lyricsrootmanifest.CoverageComplete:
			if !isCanonicalContentBackupSHA256(record.DraftSHA256) ||
				!isCanonicalContentBackupSHA256(record.DocumentSHA256) || record.AvailabilityDocumentSHA256 != "" {
				return fmt.Errorf("lyrics recovery complete item %s/%d is invalid", record.BatchSHA256, record.MusicID)
			}
		case lyricsrootmanifest.CoverageGameOnly:
			sourceOwned := isCanonicalContentBackupSHA256(record.DraftSHA256) &&
				isCanonicalContentBackupSHA256(record.DocumentSHA256) && record.AvailabilityDocumentSHA256 == ""
			availabilityOwned := record.DraftSHA256 == "" && record.DocumentSHA256 == "" &&
				isCanonicalContentBackupSHA256(record.AvailabilityDocumentSHA256)
			if !sourceOwned && !availabilityOwned {
				return fmt.Errorf("lyrics recovery Game-only item %s/%d is invalid", record.BatchSHA256, record.MusicID)
			}
		case lyricsrootmanifest.CoverageSatisfiedNoLyrics, lyricsrootmanifest.CoverageAmbiguous,
			lyricsrootmanifest.CoverageMissing, lyricsrootmanifest.CoverageIncomplete, lyricsrootmanifest.CoverageFailed:
			if record.DraftSHA256 != "" || record.DocumentSHA256 != "" ||
				!isCanonicalContentBackupSHA256(record.AvailabilityDocumentSHA256) {
				return fmt.Errorf("lyrics recovery availability item %s/%d is invalid", record.BatchSHA256, record.MusicID)
			}
		default:
			return fmt.Errorf("lyrics recovery item %s/%d state is invalid", record.BatchSHA256, record.MusicID)
		}
		if stateCounts[record.BatchSHA256] == nil {
			stateCounts[record.BatchSHA256] = map[lyricsrootmanifest.CoverageState]int{}
		}
		stateCounts[record.BatchSHA256][state]++
		items[identity] = record
	}
	for batchSHA, batch := range batches {
		counts := stateCounts[batchSHA]
		batchItems := itemsForRecoveryBatch(items, batchSHA)
		musicIDs := make([]int, len(batchItems))
		for index, item := range batchItems {
			musicIDs[index] = item.MusicID
		}
		sort.Ints(musicIDs)
		musicIDsSHA256, digestErr := lyricsrootmanifest.OrderedMusicIDsSHA256(musicIDs)
		if len(batchItems) != batch.CatalogCount || digestErr != nil || musicIDsSHA256 != batch.MusicIDsSHA256 ||
			!recoveryCoverageCountsMatch(coverageByBatch[batchSHA], counts, batch.CatalogCount) {
			return fmt.Errorf("lyrics recovery batch %s item coverage is incomplete", batchSHA)
		}
	}

	sourceDocuments := make(map[recoveryBackupItemIdentity]model.LyricsSourceDocument)
	for _, record := range lyrics.SourceDocuments {
		if _, recovery := batches[record.ManifestBatchSHA256]; !recovery {
			continue
		}
		identity := recoveryBackupItemIdentity{batchSHA256: record.ManifestBatchSHA256, musicID: record.MusicID}
		item, exists := items[identity]
		document, err := model.DecodeLyricsSourceDocument([]byte(record.DocumentJSON))
		if !exists || (item.State != string(lyricsrootmanifest.CoverageComplete) && item.State != string(lyricsrootmanifest.CoverageGameOnly)) ||
			err != nil || item.DocumentSHA256 != record.DocumentSHA256 || document.SchemaVersion != record.SchemaVersion ||
			item.DocumentSHA256 == "" || item.State == string(lyricsrootmanifest.CoverageGameOnly) && document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
			string(document.ReasonCode) != record.ReasonCode {
			return fmt.Errorf("lyrics recovery source document %s/%d is invalid", record.ManifestBatchSHA256, record.MusicID)
		}
		if document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 && documentIDs[record.MusicID] {
			return fmt.Errorf("lyrics recovery source document %s/%d has mixed source-v3 and legacy editable ownership",
				record.ManifestBatchSHA256, record.MusicID)
		}
		if !documentIDs[record.MusicID] && document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
			return fmt.Errorf("lyrics recovery source document %s/%d is invalid", record.ManifestBatchSHA256, record.MusicID)
		}
		if _, duplicate := sourceDocuments[identity]; duplicate {
			return fmt.Errorf("lyrics recovery source document %s/%d is duplicated", record.ManifestBatchSHA256, record.MusicID)
		}
		sourceDocuments[identity] = document
	}

	availability := make(map[recoveryBackupItemIdentity]model.LyricsAvailabilityDocument, len(lyrics.AvailabilityDocuments))
	availabilityIDs := make(map[int64]bool, len(lyrics.AvailabilityDocuments))
	for _, record := range lyrics.AvailabilityDocuments {
		identity := recoveryBackupItemIdentity{batchSHA256: record.BatchSHA256, musicID: record.MusicID}
		item, exists := items[identity]
		document, err := model.DecodeLyricsAvailabilityDocument([]byte(record.DocumentJSON))
		canonicalDocumentJSON, canonicalErr := json.Marshal(document)
		digest := sha256.Sum256([]byte(record.DocumentJSON))
		if record.AvailabilityDocumentID <= 0 || availabilityIDs[record.AvailabilityDocumentID] || !exists ||
			item.State == string(lyricsrootmanifest.CoverageComplete) || sourceDocuments[identity].SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 || err != nil || canonicalErr != nil ||
			string(canonicalDocumentJSON) != record.DocumentJSON ||
			record.SchemaVersion != document.SchemaVersion || record.State != string(document.State) ||
			record.ReasonCode != string(document.ReasonCode) || record.NoLyricsReason != document.NoLyricsReason ||
			record.DocumentSHA256 != hex.EncodeToString(digest[:]) || record.DocumentSHA256 != item.AvailabilityDocumentSHA256 ||
			record.ResultSHA256 != item.ResultSHA256 || record.CreatedAt <= 0 {
			return fmt.Errorf("lyrics availability document %s/%d is invalid", record.BatchSHA256, record.MusicID)
		}
		if _, duplicate := availability[identity]; duplicate {
			return fmt.Errorf("lyrics availability document %s/%d is duplicated", record.BatchSHA256, record.MusicID)
		}
		availabilityIDs[record.AvailabilityDocumentID] = true
		availability[identity] = document
	}
	for identity, item := range items {
		_, hasSource := sourceDocuments[identity]
		_, hasAvailability := availability[identity]
		switch {
		case (item.State == string(lyricsrootmanifest.CoverageComplete) || item.State == string(lyricsrootmanifest.CoverageGameOnly)) && hasSource && !hasAvailability:
			if item.State == string(lyricsrootmanifest.CoverageGameOnly) && sourceDocuments[identity].SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
				return fmt.Errorf("lyrics recovery Game-only item %s/%d has a non-v3 source document", identity.batchSHA256, identity.musicID)
			}
		case item.State == string(lyricsrootmanifest.CoverageComplete) && !hasSource:
			return fmt.Errorf("lyrics recovery complete item %s/%d has no source document", identity.batchSHA256, identity.musicID)
		case item.State == string(lyricsrootmanifest.CoverageGameOnly) && !hasSource && hasAvailability:
			// Legacy Game-only availability remains a valid v2 recovery shape.
		case item.State != string(lyricsrootmanifest.CoverageComplete) && item.State != string(lyricsrootmanifest.CoverageGameOnly) && hasAvailability && !hasSource:
		default:
			return fmt.Errorf("lyrics recovery item %s/%d has an invalid source/availability ownership shape", identity.batchSHA256, identity.musicID)
		}
	}

	expectedFixedIdentities := make(map[recoveryBackupArtifactIdentity]string, len(lyrics.RecoveryArtifacts))
	for itemIdentity, document := range sourceDocuments {
		if err := addRecoveryBackupFixedIdentities(expectedFixedIdentities, itemIdentity, document.FixedIdentities); err != nil {
			return err
		}
	}
	for itemIdentity, document := range availability {
		if err := addRecoveryBackupFixedIdentities(expectedFixedIdentities, itemIdentity, document.FixedIdentities); err != nil {
			return err
		}
	}

	evidence := make(map[recoveryBackupEvidenceIdentity]LyricsRecoverySourceEvidenceBackupRecord, len(lyrics.RecoverySourceEvidence))
	for _, record := range lyrics.RecoverySourceEvidence {
		identity := recoveryBackupEvidenceIdentity{provider: record.Provider, evidenceID: record.EvidenceID}
		rawDigest := sha256.Sum256(record.RawBytes)
		var categories []string
		if record.CreatedAt <= 0 || record.RawByteCount <= 0 || record.RawByteCount != len(record.RawBytes) ||
			!isCanonicalContentBackupSHA256(record.SHA256) || record.SHA256 != record.RawSHA256 ||
			record.RawSHA256 != hex.EncodeToString(rawDigest[:]) || !isCanonicalContentBackupSHA256(record.AcquisitionID) ||
			!isCanonicalContentBackupSHA256(record.EnvelopeSHA256) ||
			decodeCanonicalBackupJSON(record.CategoriesJSON, &categories) != nil || categories == nil ||
			!validRecoveryBackupEvidenceShape(record) {
			return fmt.Errorf("lyrics recovery evidence %s/%s is invalid", record.Provider, record.EvidenceID)
		}
		if _, duplicate := evidence[identity]; duplicate {
			return fmt.Errorf("lyrics recovery evidence %s/%s is duplicated", record.Provider, record.EvidenceID)
		}
		evidence[identity] = record
	}

	artifacts := make(map[recoveryBackupArtifactIdentity]model.LyricsSourceFixedIdentity, len(lyrics.RecoveryArtifacts))
	artifactRefs := make(map[recoveryBackupArtifactIdentity][]model.LyricsSourceIndexEvidenceRef, len(lyrics.RecoveryArtifacts))
	artifactsByItem := make(map[recoveryBackupItemIdentity]map[string]bool)
	artifactIndexByItem := make(map[recoveryBackupItemIdentity]int)
	for _, record := range lyrics.RecoveryArtifacts {
		itemIdentity := recoveryBackupItemIdentity{batchSHA256: record.BatchSHA256, musicID: record.MusicID}
		item, exists := items[itemIdentity]
		identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(record.FixedIdentityJSON))
		canonicalCategories, categoriesErr := json.Marshal(identity.Categories)
		canonicalRefs, refsErr := json.Marshal(identity.IndexEvidenceRefs)
		digest := sha256.Sum256([]byte(record.FixedIdentityJSON))
		artifactIdentity := recoveryBackupArtifactIdentity{
			batchSHA256: record.BatchSHA256, musicID: record.MusicID, renditionKey: record.RenditionKey,
		}
		if source, sourceExists := sourceDocuments[itemIdentity]; sourceExists && source.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			index := artifactIndexByItem[itemIdentity]
			if index >= len(source.FixedIdentities) || record.RenditionKey != source.FixedIdentities[index].RenditionKey {
				return fmt.Errorf("lyrics recovery artifacts for %s/%d are not in canonical fixed-identity order", record.BatchSHA256, record.MusicID)
			}
			artifactIndexByItem[itemIdentity] = index + 1
		}
		expectedIdentityJSON, expectedIdentity := expectedFixedIdentities[artifactIdentity]
		if !exists || !expectedIdentity || expectedIdentityJSON != record.FixedIdentityJSON ||
			item.State != string(lyricsrootmanifest.CoverageComplete) && item.State != string(lyricsrootmanifest.CoverageGameOnly) ||
			err != nil || string(identity.Provider) != record.Provider || identity.RenditionKey != record.RenditionKey ||
			identity.Origin != record.Origin || identity.PageID != record.PageID || identity.RevisionID != record.RevisionID ||
			identity.RevisionTimestamp != record.RevisionTimestamp || identity.SHA1 != record.MediaWikiSHA1 ||
			identity.Title != record.PageTitle || identity.CanonicalURL != record.CanonicalRevisionURL ||
			identity.FetchedAt != record.FetchedAt || identity.Section != record.Section ||
			identity.CompositionRenditionKey != record.CompositionRenditionKey || string(identity.VersionReason) != record.VersionReason ||
			categoriesErr != nil || string(canonicalCategories) != record.CategoriesJSON || refsErr != nil ||
			string(canonicalRefs) != record.IndexEvidenceRefsJSON || record.FixedIdentitySHA256 != hex.EncodeToString(digest[:]) ||
			record.RawByteCount <= 0 || !isCanonicalContentBackupSHA256(record.RawWikitextSHA256) ||
			!isCanonicalContentBackupSHA256(record.ArtifactSHA256) || record.CreatedAt <= 0 {
			return fmt.Errorf("lyrics recovery artifact %s/%d/%s is invalid", record.BatchSHA256, record.MusicID, record.RenditionKey)
		}
		if _, duplicate := artifacts[artifactIdentity]; duplicate {
			return fmt.Errorf("lyrics recovery artifact %s/%d/%s is duplicated", record.BatchSHA256, record.MusicID, record.RenditionKey)
		}
		artifacts[artifactIdentity] = identity
		artifactRefs[artifactIdentity] = append([]model.LyricsSourceIndexEvidenceRef{}, identity.IndexEvidenceRefs...)
		if artifactsByItem[itemIdentity] == nil {
			artifactsByItem[itemIdentity] = map[string]bool{}
		}
		artifactsByItem[itemIdentity][record.RenditionKey] = true
	}

	positions := make(map[recoveryBackupArtifactIdentity]map[int]bool)
	referencedEvidence := make(map[recoveryBackupEvidenceIdentity]bool)
	batchEvidence := make(map[string]map[recoveryBackupEvidenceIdentity]bool)
	for _, record := range lyrics.RecoveryArtifactEvidence {
		artifactIdentity := recoveryBackupArtifactIdentity{
			batchSHA256: record.BatchSHA256, musicID: record.MusicID, renditionKey: record.RenditionKey,
		}
		identity, exists := artifacts[artifactIdentity]
		refs := artifactRefs[artifactIdentity]
		if !exists || record.Position < 0 || record.Position >= len(refs) ||
			string(identity.Provider) != record.Provider || refs[record.Position].EvidenceID != record.EvidenceID ||
			refs[record.Position].SHA256 != record.SHA256 {
			return fmt.Errorf("lyrics recovery artifact evidence %s/%d/%s/%d is invalid",
				record.BatchSHA256, record.MusicID, record.RenditionKey, record.Position)
		}
		if positions[artifactIdentity] == nil {
			positions[artifactIdentity] = map[int]bool{}
		}
		if positions[artifactIdentity][record.Position] {
			return fmt.Errorf("lyrics recovery artifact evidence %s/%d/%s/%d is duplicated",
				record.BatchSHA256, record.MusicID, record.RenditionKey, record.Position)
		}
		evidenceIdentity := recoveryBackupEvidenceIdentity{provider: record.Provider, evidenceID: record.EvidenceID}
		parent, found := evidence[evidenceIdentity]
		if !found || parent.SHA256 != record.SHA256 {
			return fmt.Errorf("lyrics recovery artifact evidence %s/%d/%s/%d has no exact parent",
				record.BatchSHA256, record.MusicID, record.RenditionKey, record.Position)
		}
		positions[artifactIdentity][record.Position] = true
		referencedEvidence[evidenceIdentity] = true
		if batchEvidence[record.BatchSHA256] == nil {
			batchEvidence[record.BatchSHA256] = map[recoveryBackupEvidenceIdentity]bool{}
		}
		batchEvidence[record.BatchSHA256][evidenceIdentity] = true
	}
	for identity, refs := range artifactRefs {
		if len(positions[identity]) != len(refs) {
			return fmt.Errorf("lyrics recovery artifact %s/%d/%s has incomplete evidence links",
				identity.batchSHA256, identity.musicID, identity.renditionKey)
		}
	}
	if len(referencedEvidence) != len(evidence) {
		return errors.New("lyrics recovery backup contains orphan parent evidence")
	}
	for batchSHA, batch := range batches {
		refs := make([]lyricsevidencepack.EvidenceRef, 0, len(batchEvidence[batchSHA]))
		var rawBytes int64
		for identity := range batchEvidence[batchSHA] {
			parent := evidence[identity]
			refs = append(refs, lyricsevidencepack.EvidenceRef{
				Provider: model.LyricsSourceProvider(parent.Provider), AcquisitionID: parent.AcquisitionID,
				EvidenceID: parent.EvidenceID, SHA256: parent.SHA256, EnvelopeSHA256: parent.EnvelopeSHA256,
			})
			rawBytes += int64(parent.RawByteCount)
		}
		sort.Slice(refs, func(left, right int) bool { return refs[left].EvidenceID < refs[right].EvidenceID })
		selectionSHA, err := lyricsevidencepack.OrderedSelectionSHA256(refs)
		if err != nil || len(refs) != batch.EvidenceCount || rawBytes != batch.RawByteCount || selectionSHA != batch.SelectionSHA256 {
			return fmt.Errorf("lyrics recovery batch %s evidence selection is invalid", batchSHA)
		}
	}

	contributions := make(map[recoveryBackupItemIdentity]map[string]bool)
	expectedComponents := make(map[recoveryBackupItemIdentity]map[string]string, len(items))
	lastContributionByItem := make(map[recoveryBackupItemIdentity]string)
	for identity, item := range items {
		if source, exists := sourceDocuments[identity]; exists {
			expectedComponents[identity] = stagedLyricsComponentRefs(source)
			continue
		}
		if item.State == string(lyricsrootmanifest.CoverageGameOnly) {
			expectedComponents[identity] = recoveryAvailabilityComponentRefs(availability[identity])
			continue
		}
		expectedComponents[identity] = map[string]string{}
	}
	for _, record := range lyrics.RecoveryContributions {
		identity := recoveryBackupItemIdentity{batchSHA256: record.BatchSHA256, musicID: record.MusicID}
		item, exists := items[identity]
		if source, sourceExists := sourceDocuments[identity]; sourceExists && source.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			if previous := lastContributionByItem[identity]; previous != "" && record.Component <= previous {
				return fmt.Errorf("lyrics recovery contributions for %s/%d are not in canonical component order", record.BatchSHA256, record.MusicID)
			}
			lastContributionByItem[identity] = record.Component
		}
		expectedRendition, expected := expectedComponents[identity][record.Component]
		if !exists || !expected || expectedRendition != record.RenditionKey ||
			!artifactsByItem[identity][record.RenditionKey] {
			return fmt.Errorf("lyrics recovery contribution %s/%d/%s is invalid", record.BatchSHA256, record.MusicID, record.Component)
		}
		if contributions[identity] == nil {
			contributions[identity] = map[string]bool{}
		}
		if contributions[identity][record.Component] {
			return fmt.Errorf("lyrics recovery contribution %s/%d/%s is duplicated", record.BatchSHA256, record.MusicID, record.Component)
		}
		ownerSHA := item.DocumentSHA256
		if ownerSHA == "" {
			ownerSHA = item.AvailabilityDocumentSHA256
		}
		digest := sha256.Sum256([]byte(ownerSHA + "\x00" + record.Component + "\x00" + record.RenditionKey))
		if record.ContributionSHA256 != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("lyrics recovery contribution %s/%d/%s checksum is invalid", record.BatchSHA256, record.MusicID, record.Component)
		}
		contributions[identity][record.Component] = true
	}
	for identity, item := range items {
		expectedArtifacts := 0
		if source, exists := sourceDocuments[identity]; exists {
			expectedArtifacts = len(source.FixedIdentities)
		} else if item.State == string(lyricsrootmanifest.CoverageGameOnly) {
			expectedArtifacts = len(availability[identity].FixedIdentities)
		}
		if len(artifactsByItem[identity]) != expectedArtifacts ||
			len(contributions[identity]) != len(expectedComponents[identity]) {
			return fmt.Errorf("lyrics recovery item %s/%d has incomplete provenance", identity.batchSHA256, identity.musicID)
		}
	}
	return nil
}

func addRecoveryBackupFixedIdentities(
	target map[recoveryBackupArtifactIdentity]string,
	item recoveryBackupItemIdentity,
	identities []model.LyricsSourceFixedIdentity,
) error {
	for _, identity := range identities {
		body, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		key := recoveryBackupArtifactIdentity{
			batchSHA256:  item.batchSHA256,
			musicID:      item.musicID,
			renditionKey: identity.RenditionKey,
		}
		if _, duplicate := target[key]; duplicate {
			return fmt.Errorf("lyrics recovery item %s/%d repeats fixed identity %s",
				item.batchSHA256, item.musicID, identity.RenditionKey)
		}
		target[key] = string(body)
	}
	return nil
}

func decodeCanonicalBackupJSON(body string, target any) error {
	if err := json.Unmarshal([]byte(body), target); err != nil {
		return err
	}
	canonical, err := json.Marshal(target)
	if err != nil || string(canonical) != body {
		return errors.New("JSON is not canonical")
	}
	return nil
}

func strictlyIncreasingRecoveryBackupIDs(values []int, owner int) bool {
	last := 0
	for _, value := range values {
		if value <= last || value == owner {
			return false
		}
		last = value
	}
	return true
}

func itemsForRecoveryBatch(items map[recoveryBackupItemIdentity]LyricsRecoveryItemBackupRecord, batchSHA string) []LyricsRecoveryItemBackupRecord {
	result := []LyricsRecoveryItemBackupRecord{}
	for identity, item := range items {
		if identity.batchSHA256 == batchSHA {
			result = append(result, item)
		}
	}
	return result
}

func recoveryCoverageCountsMatch(coverage lyricsrootmanifest.Coverage, counts map[lyricsrootmanifest.CoverageState]int, total int) bool {
	return coverage.Total == total && coverage.Complete == counts[lyricsrootmanifest.CoverageComplete] &&
		coverage.GameOnly == counts[lyricsrootmanifest.CoverageGameOnly] &&
		coverage.SatisfiedNoLyrics == counts[lyricsrootmanifest.CoverageSatisfiedNoLyrics] &&
		coverage.CatalogReview == counts[lyricsrootmanifest.CoverageCatalogReview] &&
		coverage.GameSizeEvidence == counts[lyricsrootmanifest.CoverageGameSizeEvidence] &&
		coverage.Ambiguous == counts[lyricsrootmanifest.CoverageAmbiguous] &&
		coverage.Missing == counts[lyricsrootmanifest.CoverageMissing] &&
		coverage.Incomplete == counts[lyricsrootmanifest.CoverageIncomplete] &&
		coverage.Failed == counts[lyricsrootmanifest.CoverageFailed]
}

func validRecoveryBackupEvidenceShape(record LyricsRecoverySourceEvidenceBackupRecord) bool {
	if record.EvidenceID == "" || record.FetchedAt == "" || record.FetchedAt != strings.TrimSpace(record.FetchedAt) {
		return false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, record.FetchedAt); err != nil || parsed.Unix() <= 0 ||
		parsed.UTC().Format(time.RFC3339Nano) != record.FetchedAt {
		return false
	}
	provider := model.LyricsSourceProvider(record.Provider)
	expectedOrigin := ""
	switch provider {
	case model.LyricsSourceProviderVocaloidFandom:
		expectedOrigin = model.LyricsSourceOriginVocaloidFandom
	case model.LyricsSourceProviderMoegirl:
		expectedOrigin = model.LyricsSourceOriginMoegirl
	case model.LyricsSourceProviderMoegirlPublicExact:
		expectedOrigin = model.LyricsSourceOriginMoegirlPublicExact
	case model.LyricsSourceProviderSekaipedia:
		expectedOrigin = model.LyricsSourceOriginSekaipedia
	default:
		return false
	}
	if record.Origin != expectedOrigin {
		return false
	}
	switch record.Kind {
	case "mediawiki_revision":
		return provider != model.LyricsSourceProviderMoegirlPublicExact && record.PageID > 0 && record.RevisionID > 0 &&
			len(record.MediaWikiSHA1) == 40 && record.PageTitle != "" && record.CanonicalRevisionURL != "" &&
			record.CanonicalRequestURL == "" &&
			(provider == model.LyricsSourceProviderSekaipedia && record.RevisionTimestamp != "" ||
				provider != model.LyricsSourceProviderSekaipedia && record.RevisionTimestamp == "")
	case "mediawiki_search_response":
		return provider == model.LyricsSourceProviderVocaloidFandom && record.PageID == 0 && record.RevisionID == 0 &&
			record.RevisionTimestamp == "" && record.MediaWikiSHA1 == "" && record.PageTitle == "" &&
			record.CanonicalRevisionURL == "" && record.CategoriesJSON == "[]" && record.CanonicalRequestURL != ""
	case "exact_public_html":
		return provider == model.LyricsSourceProviderMoegirlPublicExact && record.PageID > 0 && record.RevisionID > 0 &&
			record.RevisionTimestamp == "" && record.MediaWikiSHA1 == "" && record.PageTitle != "" &&
			record.CanonicalRevisionURL != "" && record.CanonicalRequestURL == record.CanonicalRevisionURL
	default:
		return false
	}
}

func recoveryAvailabilityComponentRefs(document model.LyricsAvailabilityDocument) map[string]string {
	if document.State != model.LyricsAvailabilityStateGameOnly {
		return map[string]string{}
	}
	refs := map[string]string{
		"game_text":        document.Provenance.GameText.RenditionKey,
		"version_evidence": document.Provenance.VersionEvidence.RenditionKey,
	}
	if document.Provenance.PerformerSegmentation != nil {
		refs["performer_segmentation"] = document.Provenance.PerformerSegmentation.RenditionKey
	}
	if document.Provenance.Ruby != nil {
		refs["ruby"] = document.Provenance.Ruby.RenditionKey
	}
	return refs
}

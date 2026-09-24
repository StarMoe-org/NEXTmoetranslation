package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/model"
)

var ErrPublicLyricsV3Unavailable = errors.New("public lyrics v3 is unavailable for this recovery batch")

var errPublicLyricsV2CompatibilityUnrepresentable = errors.New("public lyrics v3 document is not losslessly representable as v2")

var (
	publicV3RenditionKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	publicV3ColorPattern        = regexp.MustCompile(`^#[0-9A-F]{6}$`)
)

type PublicLyricsV3TranslationCredits struct {
	Translation  string `json:"translation,omitempty"`
	Proofreading string `json:"proofreading,omitempty"`
}

type PublicLyricsV3RubySpan struct {
	Text    string `json:"text"`
	Reading string `json:"reading,omitempty"`
}

type PublicLyricsV3Segment struct {
	Text         string                   `json:"text"`
	PerformerIDs []string                 `json:"performerIds"`
	Ruby         []PublicLyricsV3RubySpan `json:"ruby"`
}

type PublicLyricsV3Line struct {
	ID                   string                  `json:"id"`
	Order                int                     `json:"order"`
	Japanese             string                  `json:"japanese"`
	Chinese              string                  `json:"zh-CN,omitempty"`
	English              string                  `json:"en-US,omitempty"`
	StanzaBreakBefore    bool                    `json:"stanzaBreakBefore,omitempty"`
	Segments             []PublicLyricsV3Segment `json:"segments"`
	TrailingPerformerIDs []string                `json:"trailingPerformerIds"`
}

type PublicLyricsV3Side struct {
	Version model.LyricsSourceVersion `json:"version"`
	Lines   []PublicLyricsV3Line      `json:"lines"`
}

type PublicLyricsV3Relation struct {
	Kind             model.LyricsSourceRenditionRelationKind `json:"kind"`
	FullRenditionKey string                                  `json:"fullRenditionKey,omitempty"`
	LineIDs          []string                                `json:"lineIds,omitempty"`
}

type PublicLyricsV3ComponentAttribution struct {
	Component   string                     `json:"component"`
	Provider    model.LyricsSourceProvider `json:"provider"`
	Title       string                     `json:"title"`
	RevisionID  int                        `json:"revisionId"`
	RevisionURL string                     `json:"revisionUrl"`
	LicenseName string                     `json:"licenseName"`
	LicenseURL  string                     `json:"licenseUrl"`
}

type PublicLyricsV3Rendition struct {
	Key                string                               `json:"key"`
	Kind               model.LyricsSourceRenditionKind      `json:"kind"`
	Label              string                               `json:"label"`
	AvailableVersions  []string                             `json:"availableVersions"`
	Performers         []model.LyricsSourcePerformer        `json:"performers"`
	Full               *PublicLyricsV3Side                  `json:"full,omitempty"`
	Game               *PublicLyricsV3Side                  `json:"game,omitempty"`
	Relation           PublicLyricsV3Relation               `json:"relation"`
	SourceTabPaths     []model.LyricsSourceTabPath          `json:"sourceTabPaths"`
	Provenance         []PublicLyricsV3ComponentAttribution `json:"provenance"`
	TranslationCredits *PublicLyricsV3TranslationCredits    `json:"translationCredits,omitempty"`
}

type PublicLyricsV3DetailDocument struct {
	Version    int                           `json:"version"`
	MusicID    int                           `json:"musicId"`
	Revision   int                           `json:"revision"`
	UpdatedAt  string                        `json:"updatedAt"`
	State      PublicLyricsAvailabilityState `json:"state,omitempty"`
	Renditions []PublicLyricsV3Rendition     `json:"renditions"`
}

type PublicLyricsV3IndexDocument struct {
	Version int                     `json:"version"`
	Songs   []PublicLyricsIndexSong `json:"songs"`
}

type RecoveryPublicLyricsV3Candidate struct {
	BatchSHA256 string
	RootSHA256  string
	Index       PublicLyricsV3IndexDocument
	Details     map[int]PublicLyricsV3DetailDocument
}

func EncodePublicLyricsV3Index(index PublicLyricsV3IndexDocument) ([]byte, error) {
	if err := validatePublicLyricsV3Index(index); err != nil {
		return nil, err
	}
	if err := validatePublicLyricsArtifactSize(index); err != nil {
		return nil, err
	}
	return json.Marshal(index)
}

func DecodePublicLyricsV3Index(body []byte) (PublicLyricsV3IndexDocument, error) {
	var index PublicLyricsV3IndexDocument
	if err := decodePublicLyricsV3JSON(body, &index); err != nil {
		return PublicLyricsV3IndexDocument{}, fmt.Errorf("decode public lyrics v3 index: %w", err)
	}
	if err := validatePublicLyricsV3Index(index); err != nil {
		return PublicLyricsV3IndexDocument{}, err
	}
	if err := validatePublicLyricsArtifactSize(index); err != nil {
		return PublicLyricsV3IndexDocument{}, err
	}
	return index, nil
}

func EncodePublicLyricsV3Detail(detail PublicLyricsV3DetailDocument) ([]byte, error) {
	if err := validatePublicLyricsV3Detail(detail); err != nil {
		return nil, err
	}
	if err := validatePublicLyricsArtifactSize(detail); err != nil {
		return nil, err
	}
	return json.Marshal(detail)
}

func DecodePublicLyricsV3Detail(body []byte) (PublicLyricsV3DetailDocument, error) {
	var detail PublicLyricsV3DetailDocument
	if err := decodePublicLyricsV3JSON(body, &detail); err != nil {
		return PublicLyricsV3DetailDocument{}, fmt.Errorf("decode public lyrics v3 detail: %w", err)
	}
	if err := validatePublicLyricsV3Detail(detail); err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	if err := validatePublicLyricsArtifactSize(detail); err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	return detail, nil
}

func decodePublicLyricsV3JSON(body []byte, target any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("public lyrics v3 JSON is empty")
	}
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return err
	}
	var raw any
	rawDecoder := json.NewDecoder(bytes.NewReader(body))
	rawDecoder.UseNumber()
	if err := rawDecoder.Decode(&raw); err != nil {
		return err
	}
	if publicLyricsV3ContainsNull(raw) {
		return errors.New("public lyrics v3 JSON contains null")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("public lyrics v3 contains trailing JSON")
		}
		return err
	}
	return nil
}

func publicLyricsV3ContainsNull(value any) bool {
	switch current := value.(type) {
	case nil:
		return true
	case []any:
		for _, item := range current {
			if publicLyricsV3ContainsNull(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range current {
			if publicLyricsV3ContainsNull(item) {
				return true
			}
		}
	}
	return false
}

func (s *Store) RecoveryPublicLyricsV3(batchSHA256 string) (RecoveryPublicLyricsV3Candidate, error) {
	return s.RecoveryPublicLyricsV3Context(context.Background(), batchSHA256)
}

func (s *Store) RecoveryPublicLyricsV3Context(ctx context.Context, batchSHA256 string) (RecoveryPublicLyricsV3Candidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !isCanonicalContentBackupSHA256(batchSHA256) {
		return RecoveryPublicLyricsV3Candidate{}, errors.New("recovery Public v3 candidate requires an exact lowercase batch SHA-256")
	}
	content, err := s.ExportLyricsContentContext(ctx)
	if err != nil {
		return RecoveryPublicLyricsV3Candidate{}, err
	}
	return buildRecoveryPublicLyricsV3Candidate(content, batchSHA256)
}

// RecoveryPublicLyricsV2Compatibility derives the separately addressed Public
// v2 compatibility candidate from the exact persisted v3 batch. It emits a
// detail only for one legacy-lossless rendition and marks every unrepresentable
// text-owning entry incomplete without flattening it.
func (s *Store) RecoveryPublicLyricsV2Compatibility(batchSHA256 string) (RecoveryPublicLyricsCandidate, error) {
	return s.RecoveryPublicLyricsV2CompatibilityContext(context.Background(), batchSHA256)
}

func (s *Store) RecoveryPublicLyricsV2CompatibilityContext(ctx context.Context, batchSHA256 string) (RecoveryPublicLyricsCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !isCanonicalContentBackupSHA256(batchSHA256) {
		return RecoveryPublicLyricsCandidate{}, errors.New("recovery Public v2 compatibility candidate requires an exact lowercase batch SHA-256")
	}
	content, err := s.ExportLyricsContentContext(ctx)
	if err != nil {
		return RecoveryPublicLyricsCandidate{}, err
	}
	v3Candidate, err := buildRecoveryPublicLyricsV3Candidate(content, batchSHA256)
	if err != nil {
		return RecoveryPublicLyricsCandidate{}, err
	}
	return buildRecoveryPublicLyricsV2CompatibilityCandidate(content, v3Candidate)
}

func buildRecoveryPublicLyricsV3Candidate(content LyricsContentExport, batchSHA256 string) (RecoveryPublicLyricsV3Candidate, error) {
	var batch *LyricsRecoveryBatchBackupRecord
	for index := range content.RecoveryBatches {
		if content.RecoveryBatches[index].BatchSHA256 == batchSHA256 {
			if batch != nil {
				return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery batch %s is duplicated", batchSHA256)
			}
			batch = &content.RecoveryBatches[index]
		}
	}
	if batch == nil {
		return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery batch %s was not found", batchSHA256)
	}
	items := make([]LyricsRecoveryItemBackupRecord, 0, batch.CatalogCount)
	for _, item := range content.RecoveryItems {
		if item.BatchSHA256 == batchSHA256 {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(left, right int) bool { return items[left].MusicID < items[right].MusicID })
	if len(items) != batch.CatalogCount {
		return RecoveryPublicLyricsV3Candidate{}, errors.New("lyrics recovery v3 batch does not cover its catalog")
	}
	itemMusicIDs := make(map[int]struct{}, len(items))
	for _, item := range items {
		if item.MusicID <= 0 {
			return RecoveryPublicLyricsV3Candidate{}, errors.New("lyrics recovery v3 item has an invalid music ID")
		}
		if _, duplicate := itemMusicIDs[item.MusicID]; duplicate {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 item %d is duplicated", item.MusicID)
		}
		itemMusicIDs[item.MusicID] = struct{}{}
	}
	catalog := make(map[int]CatalogMusicBackupRecord, len(content.Music))
	for _, music := range content.Music {
		if _, duplicate := catalog[music.MusicID]; duplicate {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 catalog music %d is duplicated", music.MusicID)
		}
		catalog[music.MusicID] = music
	}
	if len(catalog) != batch.CatalogCount {
		return RecoveryPublicLyricsV3Candidate{}, errors.New("lyrics recovery v3 catalog count changed")
	}
	legacyEditableDocuments := make(map[int]int, len(content.Documents))
	for _, document := range content.Documents {
		legacyEditableDocuments[document.MusicID]++
	}
	sourceRecords, superseded, err := recoveryBatchSourceRecords(content, batchSHA256)
	if err != nil {
		return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 %w", err)
	}
	for musicID := range sourceRecords {
		if _, found := itemMusicIDs[musicID]; !found {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 source document %d is outside the batch catalog", musicID)
		}
	}
	publicV3SourceCount := 0
	for _, record := range sourceRecords {
		document, err := model.DecodeLyricsSourceDocument([]byte(record.DocumentJSON))
		if err != nil {
			return RecoveryPublicLyricsV3Candidate{}, err
		}
		if superseded[record.MusicID] == nil && document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 &&
			legacyEditableDocuments[record.MusicID] != 0 {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf(
				"lyrics recovery v3 music %d has mixed source-v3 and legacy editable ownership", record.MusicID,
			)
		}
		if _, err := publicV3SourceDocument(document); err != nil {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf(
				"lyrics recovery v3 music %d source document: %w", record.MusicID, err,
			)
		}
		publicV3SourceCount++
	}
	if publicV3SourceCount == 0 {
		return RecoveryPublicLyricsV3Candidate{}, ErrPublicLyricsV3Unavailable
	}
	contributions := make(map[int]map[string]string)
	for _, record := range content.RecoveryContributions {
		if record.BatchSHA256 != batchSHA256 {
			continue
		}
		if _, found := itemMusicIDs[record.MusicID]; !found {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 contribution music %d is outside the batch catalog", record.MusicID)
		}
		if contributions[record.MusicID] == nil {
			contributions[record.MusicID] = map[string]string{}
		}
		if _, duplicate := contributions[record.MusicID][record.Component]; duplicate {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 contribution %d/%s is duplicated", record.MusicID, record.Component)
		}
		contributions[record.MusicID][record.Component] = record.RenditionKey
	}
	artifacts := make(map[int]map[string]LyricsRecoveryArtifactBackupRecord)
	for _, record := range content.RecoveryArtifacts {
		if record.BatchSHA256 != batchSHA256 {
			continue
		}
		if _, found := itemMusicIDs[record.MusicID]; !found {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 artifact music %d is outside the batch catalog", record.MusicID)
		}
		if artifacts[record.MusicID] == nil {
			artifacts[record.MusicID] = map[string]LyricsRecoveryArtifactBackupRecord{}
		}
		if _, duplicate := artifacts[record.MusicID][record.RenditionKey]; duplicate {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 artifact %d/%s is duplicated", record.MusicID, record.RenditionKey)
		}
		artifacts[record.MusicID][record.RenditionKey] = record
	}
	availabilityRecords := make(map[int]LyricsAvailabilityDocumentBackupRecord)
	for _, record := range content.AvailabilityDocuments {
		if record.BatchSHA256 != batchSHA256 {
			continue
		}
		if _, found := itemMusicIDs[record.MusicID]; !found {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 availability music %d is outside the batch catalog", record.MusicID)
		}
		if _, duplicate := availabilityRecords[record.MusicID]; duplicate {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 availability document %d is duplicated", record.MusicID)
		}
		availabilityRecords[record.MusicID] = record
	}
	candidate := RecoveryPublicLyricsV3Candidate{
		BatchSHA256: batchSHA256, RootSHA256: batch.RootSHA256,
		Index:   PublicLyricsV3IndexDocument{Version: 3, Songs: make([]PublicLyricsIndexSong, 0, len(items))},
		Details: map[int]PublicLyricsV3DetailDocument{},
	}
	lastMusicID := 0
	for _, item := range items {
		if item.MusicID <= lastMusicID {
			return RecoveryPublicLyricsV3Candidate{}, errors.New("lyrics recovery v3 items are not ordered")
		}
		lastMusicID = item.MusicID
		music, ok := catalog[item.MusicID]
		if !ok {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 music %d is outside catalog", item.MusicID)
		}
		state, err := publicLyricsRecoveryState(item.State)
		if err != nil {
			return RecoveryPublicLyricsV3Candidate{}, err
		}
		indexItem := PublicLyricsIndexSong{MusicID: item.MusicID, State: state, Title: model.LocalizedTitle{Japanese: music.TitleJA, Chinese: music.TitleZH, English: music.TitleEN}}
		if sourceRecord, exists := sourceRecords[item.MusicID]; exists {
			document, err := model.DecodeLyricsSourceDocument([]byte(sourceRecord.DocumentJSON))
			if err != nil {
				return RecoveryPublicLyricsV3Candidate{}, err
			}
			detail, err := buildRecoveryPublicLyricsV3SourceDetail(
				item, sourceRecord, document, contributions[item.MusicID],
				artifacts[item.MusicID], content, superseded[item.MusicID],
			)
			if err != nil {
				return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("build public v3 music %d: %w", item.MusicID, err)
			}
			indexItem.Revision, indexItem.UpdatedAt = detail.Revision, detail.UpdatedAt
			indexItem.AvailableVersions = publicV3AvailableVersions(detail)
			candidate.Details[item.MusicID] = detail
		} else if state == PublicLyricsStateComplete || state == PublicLyricsStateGameOnly {
			return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 music %d has no source v3 document", item.MusicID)
		} else {
			availability, found := availabilityRecords[item.MusicID]
			if !found {
				return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 music %d has no availability document", item.MusicID)
			}
			document, err := model.DecodeLyricsAvailabilityDocument([]byte(availability.DocumentJSON))
			if err != nil || string(document.State) != item.State || availability.State != item.State {
				return RecoveryPublicLyricsV3Candidate{}, fmt.Errorf("lyrics recovery v3 music %d availability state changed", item.MusicID)
			}
			indexItem.Revision = 1
			indexItem.UpdatedAt = formatTimestamp(availability.CreatedAt)
			if state == PublicLyricsStateSatisfiedNoLyrics {
				indexItem.NoLyricsReason = document.NoLyricsReason
			}
		}
		candidate.Index.Songs = append(candidate.Index.Songs, indexItem)
	}
	return candidate, validatePublicLyricsV3Candidate(candidate)
}

// recoveryBatchSourceRecords returns the batch's source documents by song. A
// recovery takeover deleted the document it superseded, so its verbatim copy
// stands in: a candidate represents the ledger, not the editor document that
// replaced it. The copy has document ID 0, and superseded maps its song to the
// localization and translation-edition rows the takeover kept, keyed to that
// ID; no row or editable document of the replacing document attaches to it.
func recoveryBatchSourceRecords(content LyricsContentExport, batchSHA256 string) (map[int]LyricsSourceDocumentBackupRecord, map[int]*LyricsContentExport, error) {
	records := make(map[int]LyricsSourceDocumentBackupRecord)
	for _, record := range content.SourceDocuments {
		if record.ManifestBatchSHA256 != batchSHA256 {
			continue
		}
		if _, duplicate := records[record.MusicID]; duplicate {
			return nil, nil, fmt.Errorf("source document %d is duplicated", record.MusicID)
		}
		records[record.MusicID] = record
	}
	superseded := make(map[int]*LyricsContentExport)
	for _, takeover := range content.RecoveryTakeovers {
		if takeover.BatchSHA256 != batchSHA256 || takeover.SupersededDocument == nil {
			continue
		}
		if _, duplicate := records[takeover.MusicID]; duplicate {
			return nil, nil, fmt.Errorf("source document %d is both current and superseded", takeover.MusicID)
		}
		document := takeover.SupersededDocument
		records[takeover.MusicID] = LyricsSourceDocumentBackupRecord{
			MusicID: takeover.MusicID, SchemaVersion: document.SchemaVersion, ReasonCode: document.ReasonCode,
			DocumentJSON: document.DocumentJSON, DocumentSHA256: document.DocumentSHA256,
			ManifestBatchSHA256: takeover.BatchSHA256, CreatedAt: document.CreatedAt,
		}
		rows := &LyricsContentExport{}
		if document.LocalizationsJSON != "" {
			localizations, err := decodeLyricsRecoverySupersededLocalizations(document.LocalizationsJSON)
			if err != nil {
				return nil, nil, fmt.Errorf("superseded localizations of music %d: %w", takeover.MusicID, err)
			}
			*rows = localizations.contentRows(0)
		}
		superseded[takeover.MusicID] = rows
	}
	return records, superseded, nil
}

func publicV3SourceDocument(document model.LyricsSourceDocument) (model.LyricsSourceDocument, error) {
	switch document.SchemaVersion {
	case model.LyricsSourceDocumentSchemaVersionV3:
		return document, nil
	case model.LyricsSourceDocumentSchemaVersionV2:
		upgraded, err := model.UpconvertLyricsSourceDocumentV2(document)
		if err != nil {
			return model.LyricsSourceDocument{}, fmt.Errorf("legacy source v2 is not losslessly upconvertible: %w", err)
		}
		return upgraded, nil
	default:
		return model.LyricsSourceDocument{}, fmt.Errorf(
			"unsupported source schema version %d", document.SchemaVersion,
		)
	}
}

func publicV3RecoveryContributions(
	raw, normalized model.LyricsSourceDocument,
	stored map[string]string,
) (map[string]string, error) {
	if raw.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
		return stored, nil
	}
	if err := validateRecoveryPublicContributions(
		publicLyricsSourceComponentRefs(raw), stored,
	); err != nil {
		return nil, err
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(normalized.Renditions)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		result[binding.ComponentKey] = binding.FixedIdentityKey
	}
	return result, nil
}

func buildRecoveryPublicLyricsV3SourceDetail(
	item LyricsRecoveryItemBackupRecord,
	sourceRecord LyricsSourceDocumentBackupRecord,
	rawDocument model.LyricsSourceDocument,
	storedContributions map[string]string,
	artifacts map[string]LyricsRecoveryArtifactBackupRecord,
	content LyricsContentExport,
	superseded *LyricsContentExport,
) (PublicLyricsV3DetailDocument, error) {
	document, err := publicV3SourceDocument(rawDocument)
	if err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	contributions, err := publicV3RecoveryContributions(rawDocument, document, storedContributions)
	if err != nil {
		return PublicLyricsV3DetailDocument{}, fmt.Errorf("source contributions: %w", err)
	}
	localizations := content.RenditionLocalizations
	translationLines := content.RenditionTranslationLines
	if superseded != nil {
		localizations, translationLines = superseded.RenditionLocalizations, superseded.RenditionTranslationLines
	}
	var legacyLines []LyricsLineBackupRecord
	// A takeover deleted the editable rows a superseded v2 document carried.
	if rawDocument.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV2 && superseded == nil {
		localization, lines, err := publicV3LegacyLocalization(
			content, sourceRecord, document,
		)
		if err != nil {
			return PublicLyricsV3DetailDocument{}, err
		}
		legacyLines = lines
		if localization != nil {
			localizations = append(
				append([]LyricsRenditionLocalizationBackupRecord(nil), localizations...),
				localization.LyricsRenditionLocalizationBackupRecord,
			)
			translationLines = append([]LyricsRenditionTranslationLineBackupRecord(nil), translationLines...)
			translationLines = append(translationLines, localization.translationLines...)
		}
	}
	detail, err := buildPublicLyricsV3Detail(
		item, sourceRecord, document, contributions, artifacts,
		localizations, translationLines,
	)
	if err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	if len(legacyLines) != 0 {
		if err := applyPublicV3LegacyEnglish(&detail, document, legacyLines); err != nil {
			return PublicLyricsV3DetailDocument{}, err
		}
	}
	return detail, nil
}

type publicV3LegacyLocalizationRecord struct {
	LyricsRenditionLocalizationBackupRecord
	translationLines []LyricsRenditionTranslationLineBackupRecord
}

func publicV3LegacyLocalization(
	content LyricsContentExport,
	sourceRecord LyricsSourceDocumentBackupRecord,
	document model.LyricsSourceDocument,
) (*publicV3LegacyLocalizationRecord, []LyricsLineBackupRecord, error) {
	if len(document.Renditions) != 1 {
		return nil, nil, errors.New("legacy source v2 Public v3 conversion requires one rendition")
	}
	var editable *LyricsDocumentBackupRecord
	for index := range content.Documents {
		candidate := &content.Documents[index]
		if candidate.MusicID != sourceRecord.MusicID {
			continue
		}
		if editable != nil {
			return nil, nil, fmt.Errorf("legacy source v2 music %d has duplicate editable documents", sourceRecord.MusicID)
		}
		editable = candidate
	}
	if editable == nil || editable.Revision <= 0 {
		return nil, nil, fmt.Errorf("legacy source v2 music %d has no editable lyrics document", sourceRecord.MusicID)
	}
	var sourceSide *model.LyricsSourceFull
	if document.Renditions[0].Full != nil {
		sourceSide = document.Renditions[0].Full
	} else {
		sourceSide = document.Renditions[0].Game
	}
	if sourceSide == nil {
		return nil, nil, errors.New("legacy source v2 Public v3 conversion has no authoritative side")
	}
	lines := make([]LyricsLineBackupRecord, 0, len(sourceSide.Lines))
	for _, line := range content.Lines {
		if line.MusicID == sourceRecord.MusicID {
			lines = append(lines, line)
		}
	}
	sort.Slice(lines, func(left, right int) bool { return lines[left].Position < lines[right].Position })
	if len(lines) != len(sourceSide.Lines) {
		return nil, nil, fmt.Errorf(
			"legacy source v2 music %d editable line count changed", sourceRecord.MusicID,
		)
	}
	for index, line := range lines {
		sourceLine := sourceSide.Lines[index]
		if line.Position != index || line.LineID != sourceLine.ID || line.Japanese != sourceLine.Text ||
			(line.StanzaBreakBefore != 0) != sourceLine.StanzaBreakBefore {
			return nil, nil, fmt.Errorf(
				"legacy source v2 music %d editable line %d changed authoritative source",
				sourceRecord.MusicID, index+1,
			)
		}
	}
	translationLines := make([]LyricsRenditionTranslationLineBackupRecord, len(lines))
	hasChinese := false
	for index, line := range lines {
		if line.Chinese != "" {
			hasChinese = true
		}
		translationLines[index] = LyricsRenditionTranslationLineBackupRecord{
			DocumentID: sourceRecord.DocumentID, RenditionKey: document.Renditions[0].RenditionKey,
			Locale: "zh-CN", Position: index, Text: line.Chinese,
		}
	}
	if !hasChinese && editable.TranslationCredit == "" && editable.ProofreadingCredit == "" {
		return nil, lines, nil
	}
	return &publicV3LegacyLocalizationRecord{
		LyricsRenditionLocalizationBackupRecord: LyricsRenditionLocalizationBackupRecord{
			DocumentID: sourceRecord.DocumentID, RenditionKey: document.Renditions[0].RenditionKey,
			Locale: "zh-CN", TranslationCredit: editable.TranslationCredit,
			ProofreadingCredit: editable.ProofreadingCredit, UpdatedAt: editable.UpdatedAt,
			UpdatedBy: editable.UpdatedBy, Revision: editable.Revision,
		},
		translationLines: translationLines,
	}, lines, nil
}

func applyPublicV3LegacyEnglish(
	detail *PublicLyricsV3DetailDocument,
	document model.LyricsSourceDocument,
	lines []LyricsLineBackupRecord,
) error {
	if detail == nil || len(document.Renditions) != 1 || len(detail.Renditions) != 1 {
		return errors.New("legacy source v2 English projection envelope is invalid")
	}
	rendition := &detail.Renditions[0]
	hasEnglish := false
	for _, line := range lines {
		if line.English != "" {
			hasEnglish = true
			break
		}
	}
	if !hasEnglish {
		return nil
	}
	setSide := func(side *PublicLyricsV3Side) error {
		if side == nil || len(side.Lines) != len(lines) {
			return errors.New("legacy source v2 English line count changed")
		}
		for index := range side.Lines {
			side.Lines[index].English = lines[index].English
		}
		return nil
	}
	if rendition.Full != nil {
		if err := setSide(rendition.Full); err != nil {
			return err
		}
		if rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
			if rendition.Game == nil {
				return errors.New("legacy source v2 English projection is missing Game")
			}
			byID := make(map[string]string, len(rendition.Full.Lines))
			for _, line := range rendition.Full.Lines {
				byID[line.ID] = line.English
			}
			if len(rendition.Relation.LineIDs) != len(rendition.Game.Lines) {
				return errors.New("legacy source v2 English projection relation changed")
			}
			for index, lineID := range rendition.Relation.LineIDs {
				value, found := byID[lineID]
				if !found {
					return fmt.Errorf("legacy source v2 English projection line %q is unknown", lineID)
				}
				rendition.Game.Lines[index].English = value
			}
		}
	} else if err := setSide(rendition.Game); err != nil {
		return err
	}
	return validatePublicLyricsV3Detail(*detail)
}

func buildRecoveryPublicLyricsV2CompatibilityCandidate(content LyricsContentExport, v3Candidate RecoveryPublicLyricsV3Candidate) (RecoveryPublicLyricsCandidate, error) {
	if err := validatePublicLyricsV3Candidate(v3Candidate); err != nil {
		return RecoveryPublicLyricsCandidate{}, err
	}
	catalogPerformers := newCatalogPerformerAliases()
	for _, performer := range content.Performers {
		addCatalogPerformerAliases(&catalogPerformers, performer.PerformerID,
			performer.NameJA, performer.NameZH, performer.NameEN)
	}
	addCanonicalLyricsSourcePerformerAliases(&catalogPerformers)
	if err := addAuditedExternalLyricsPerformerAliases(&catalogPerformers); err != nil {
		return RecoveryPublicLyricsCandidate{}, err
	}
	sourceRecords, _, err := recoveryBatchSourceRecords(content, v3Candidate.BatchSHA256)
	if err != nil {
		return RecoveryPublicLyricsCandidate{}, fmt.Errorf("public v2 compatibility %w", err)
	}
	candidate := RecoveryPublicLyricsCandidate{
		BatchSHA256: v3Candidate.BatchSHA256,
		RootSHA256:  v3Candidate.RootSHA256,
		Index: PublicLyricsIndexDocument{
			Version: 2,
			Songs:   make([]PublicLyricsIndexSong, 0, len(v3Candidate.Index.Songs)),
		},
		Details: make(map[int]PublicLyricsDetailDocument, len(v3Candidate.Details)),
	}
	for _, song := range v3Candidate.Index.Songs {
		indexSong := song
		indexSong.AvailableVersions = clonePublicV3Strings(song.AvailableVersions)
		v3Detail, ownsText := v3Candidate.Details[song.MusicID]
		if ownsText {
			record, found := sourceRecords[song.MusicID]
			if !found {
				return RecoveryPublicLyricsCandidate{}, fmt.Errorf("public v2 compatibility music %d has no exact source v3 document", song.MusicID)
			}
			document, err := model.DecodeLyricsSourceDocument([]byte(record.DocumentJSON))
			if err != nil {
				return RecoveryPublicLyricsCandidate{}, fmt.Errorf("public v2 compatibility music %d source document is invalid", song.MusicID)
			}
			document, err = publicV3SourceDocument(document)
			if err != nil {
				return RecoveryPublicLyricsCandidate{}, fmt.Errorf("public v2 compatibility music %d source document is invalid: %w", song.MusicID, err)
			}
			if err := validatePublicLyricsV2CompatibilitySource(document); err != nil {
				if errors.Is(err, errPublicLyricsV2CompatibilityUnrepresentable) {
					indexSong.State = PublicLyricsStateIncomplete
					indexSong.AvailableVersions = nil
					indexSong.NoLyricsReason = ""
					candidate.Index.Songs = append(candidate.Index.Songs, indexSong)
					continue
				}
				return RecoveryPublicLyricsCandidate{}, fmt.Errorf("public v2 compatibility music %d: %w", song.MusicID, err)
			}
			detail, err := buildPublicLyricsV2CompatibilityDetail(v3Detail, document, catalogPerformers)
			if err != nil {
				if errors.Is(err, errPublicLyricsV2CompatibilityUnrepresentable) {
					indexSong.State = PublicLyricsStateIncomplete
					indexSong.AvailableVersions = nil
					indexSong.NoLyricsReason = ""
					candidate.Index.Songs = append(candidate.Index.Songs, indexSong)
					continue
				}
				return RecoveryPublicLyricsCandidate{}, fmt.Errorf("public v2 compatibility music %d: %w", song.MusicID, err)
			}
			candidate.Details[song.MusicID] = detail
		}
		candidate.Index.Songs = append(candidate.Index.Songs, indexSong)
	}
	if err := validatePublicLyricsV2CompatibilityCandidate(candidate); err != nil {
		return RecoveryPublicLyricsCandidate{}, err
	}
	return candidate, nil
}

func validatePublicLyricsV2CompatibilitySource(document model.LyricsSourceDocument) error {
	if err := model.LyricsSourceDocumentV3ToV2Compatibility(document); err != nil {
		return fmt.Errorf("%w: %v", errPublicLyricsV2CompatibilityUnrepresentable, err)
	}
	if len(document.Renditions) != 1 {
		return fmt.Errorf("%w: peer rendition families", errPublicLyricsV2CompatibilityUnrepresentable)
	}
	rendition := document.Renditions[0]
	if rendition.SourceKind == model.LyricsSourceRenditionAlternate {
		return fmt.Errorf("%w: alternate rendition semantics", errPublicLyricsV2CompatibilityUnrepresentable)
	}
	for _, path := range rendition.SourceTabPaths {
		for _, label := range path {
			normalized := strings.ToLower(label)
			for _, marker := range []string{"alternate", "another vocal", "archive", "april fools"} {
				if strings.Contains(normalized, marker) {
					return fmt.Errorf("%w: alternate, archive, or April Fools semantics", errPublicLyricsV2CompatibilityUnrepresentable)
				}
			}
		}
	}
	return nil
}

func buildPublicLyricsV2CompatibilityDetail(v3Detail PublicLyricsV3DetailDocument, document model.LyricsSourceDocument, catalogPerformers catalogPerformerAliases) (PublicLyricsDetailDocument, error) {
	if err := validatePublicLyricsV3Detail(v3Detail); err != nil {
		return PublicLyricsDetailDocument{}, err
	}
	if len(document.Renditions) != 1 || len(v3Detail.Renditions) != 1 {
		return PublicLyricsDetailDocument{}, errors.New("public v2 compatibility requires exactly one rendition")
	}
	source := document.Renditions[0]
	public := v3Detail.Renditions[0]
	if source.RenditionKey != public.Key || source.SourceKind != public.Kind {
		return PublicLyricsDetailDocument{}, errors.New("public v2 compatibility rendition identity changed")
	}
	result := PublicLyricsDetailDocument{
		Version: 2, MusicID: v3Detail.MusicID, Revision: v3Detail.Revision, UpdatedAt: v3Detail.UpdatedAt,
		State: v3Detail.State, Attributions: publicLyricsV2CompatibilityAttributions(public.Provenance),
		AvailableVersions: clonePublicV3Strings(public.AvailableVersions),
	}
	if public.TranslationCredits != nil {
		result.TranslationCredits = &PublicLyricsTranslationCredits{
			Translation: public.TranslationCredits.Translation, Proofreading: public.TranslationCredits.Proofreading,
		}
	}
	var publicSide *PublicLyricsV3Side
	var sourceSide *model.LyricsSourceFull
	switch v3Detail.State {
	case PublicLyricsStateComplete:
		publicSide, sourceSide = public.Full, source.Full
	case PublicLyricsStateGameOnly:
		publicSide, sourceSide = public.Game, source.Game
	default:
		return PublicLyricsDetailDocument{}, errors.New("public v2 compatibility detail has no representable text state")
	}
	if publicSide == nil || sourceSide == nil {
		return PublicLyricsDetailDocument{}, errors.New("public v2 compatibility authoritative side is missing")
	}
	lines, err := publicLyricsV2CompatibilityLines(*publicSide, *sourceSide, catalogPerformers)
	if err != nil {
		return PublicLyricsDetailDocument{}, err
	}
	result.Lines = lines
	if source.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		if v3Detail.State != PublicLyricsStateComplete || public.Full == nil || public.Game == nil ||
			(source.ReasonCode != model.LyricsSourceVersionReasonTaggedFullAndGame &&
				source.ReasonCode != model.LyricsSourceVersionReasonUntaggedUncutIdentity) {
			return PublicLyricsDetailDocument{}, fmt.Errorf("%w: Full/Game relation", errPublicLyricsV2CompatibilityUnrepresentable)
		}
		result.GameProjection = &PublicLyricsGameProjection{
			ReasonCode: source.ReasonCode, LineIDs: clonePublicV3Strings(source.Relation.LineIDs),
		}
	}
	if err := validatePublicLyricsV2CompatibilityDetail(result); err != nil {
		return PublicLyricsDetailDocument{}, fmt.Errorf("%w: %v", errPublicLyricsV2CompatibilityUnrepresentable, err)
	}
	return result, nil
}

func publicLyricsV2CompatibilityLines(side PublicLyricsV3Side, source model.LyricsSourceFull, catalogPerformers catalogPerformerAliases) ([]PublicLyricsLine, error) {
	if side.Version != source.Version || len(side.Lines) != len(source.Lines) {
		return nil, errors.New("public v2 compatibility authoritative side changed")
	}
	sourceCatalog, err := newPublicLyricsSourcePerformerCatalogForPerformers(source.Performers, catalogPerformers)
	if err != nil {
		return nil, fmt.Errorf("%w: performer roster", errPublicLyricsV2CompatibilityUnrepresentable)
	}
	result := make([]PublicLyricsLine, len(side.Lines))
	for lineIndex, line := range side.Lines {
		sourceLine := source.Lines[lineIndex]
		if line.ID != sourceLine.ID || line.Order != lineIndex || line.Japanese != sourceLine.Text ||
			line.StanzaBreakBefore != sourceLine.StanzaBreakBefore || len(line.Segments) != len(sourceLine.Segments) ||
			!samePublicV3Strings(line.TrailingPerformerIDs, sourceLine.TrailingPerformerIDs) {
			return nil, fmt.Errorf("authoritative line %d changed during public v2 compatibility", lineIndex+1)
		}
		trailing, err := publicLyricsSourceSegmentPerformerIDs(line.TrailingPerformerIDs, sourceCatalog)
		if err != nil {
			return nil, fmt.Errorf("%w: authoritative line %d trailing performers", errPublicLyricsV2CompatibilityUnrepresentable, lineIndex+1)
		}
		publicLine := PublicLyricsLine{
			ID: line.ID, Order: line.Order, Japanese: line.Japanese, Chinese: line.Chinese, English: line.English,
			StanzaBreakBefore: line.StanzaBreakBefore, Segments: make([]model.LyricSegment, len(line.Segments)),
			TrailingPerformerIDs: append([]int{}, trailing...),
		}
		for segmentIndex, segment := range line.Segments {
			sourceSegment := sourceLine.Segments[segmentIndex]
			if segment.Text != sourceSegment.Text || !samePublicV3Strings(segment.PerformerIDs, sourceSegment.PerformerIDs) ||
				len(segment.Ruby) != len(sourceSegment.Ruby) {
				return nil, fmt.Errorf("authoritative line %d segment %d changed during public v2 compatibility", lineIndex+1, segmentIndex+1)
			}
			performers, err := publicLyricsSourceSegmentPerformerIDs(segment.PerformerIDs, sourceCatalog)
			if err != nil {
				return nil, fmt.Errorf("%w: authoritative line %d segment %d performers", errPublicLyricsV2CompatibilityUnrepresentable, lineIndex+1, segmentIndex+1)
			}
			ruby := make([]model.LyricRubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				sourceSpan := sourceSegment.Ruby[rubyIndex]
				if span.Text != sourceSpan.Text || span.Reading != sourceSpan.Reading {
					return nil, fmt.Errorf("authoritative line %d segment %d ruby changed during public v2 compatibility", lineIndex+1, segmentIndex+1)
				}
				ruby[rubyIndex] = model.LyricRubySpan{Text: span.Text, Reading: span.Reading}
			}
			publicLine.Segments[segmentIndex] = model.LyricSegment{
				Text: segment.Text, PerformerIDs: append([]int{}, performers...), Ruby: ruby,
			}
		}
		result[lineIndex] = publicLine
	}
	return result, nil
}

func publicLyricsV2CompatibilityAttributions(provenance []PublicLyricsV3ComponentAttribution) []PublicLyricsAttribution {
	result := make([]PublicLyricsAttribution, 0, len(provenance))
	seen := map[string]struct{}{}
	for _, source := range provenance {
		attribution := PublicLyricsAttribution{
			Provider: source.Provider, Title: source.Title, RevisionID: source.RevisionID,
			RevisionURL: source.RevisionURL, LicenseName: source.LicenseName, LicenseURL: source.LicenseURL,
		}
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", attribution.Provider, attribution.Title,
			attribution.RevisionID, attribution.RevisionURL, attribution.LicenseName, attribution.LicenseURL)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, attribution)
	}
	return result
}

func validatePublicLyricsV2CompatibilityCandidate(candidate RecoveryPublicLyricsCandidate) error {
	if candidate.Index.Version != 2 || len(candidate.Index.Songs) == 0 || candidate.Details == nil {
		return errors.New("public v2 compatibility candidate envelope is invalid")
	}
	lastMusicID := 0
	for _, song := range candidate.Index.Songs {
		if song.MusicID <= lastMusicID {
			return errors.New("public v2 compatibility index is not strictly ordered")
		}
		lastMusicID = song.MusicID
		detail := candidate.Details[song.MusicID]
		if err := validateRecoveryPublicIndexItem(song, detail); err != nil {
			return err
		}
		if detail.MusicID != 0 {
			if err := validatePublicLyricsV2CompatibilityDetail(detail); err != nil {
				return err
			}
		}
	}
	for musicID := range candidate.Details {
		found := false
		for _, song := range candidate.Index.Songs {
			if song.MusicID == musicID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("public v2 compatibility detail %d is outside its index", musicID)
		}
	}
	return validatePublicLyricsArtifactSize(candidate.Index)
}

func validatePublicLyricsV2CompatibilityDetail(detail PublicLyricsDetailDocument) error {
	if detail.Version != 2 || detail.MusicID <= 0 || detail.Revision <= 0 || detail.UpdatedAt == "" ||
		(detail.State != PublicLyricsStateComplete && detail.State != PublicLyricsStateGameOnly) ||
		detail.Attribution != "" || detail.NoLyricsReason != "" || len(detail.Attributions) == 0 ||
		len(detail.Attributions) > 16 || !validPublicLyricsTranslationCredits(detail.TranslationCredits) ||
		len(detail.Lines) == 0 || len(detail.Lines) > maxLyricsLines {
		return errors.New("public v2 compatibility detail envelope is invalid")
	}
	if detail.State == PublicLyricsStateComplete {
		if !samePublicLyricsVersions(detail.AvailableVersions, []string{publicLyricsFullVersion}) &&
			!samePublicLyricsVersions(detail.AvailableVersions, []string{publicLyricsFullVersion, publicLyricsGameVersion}) {
			return errors.New("public v2 compatibility complete versions are invalid")
		}
	} else if !samePublicLyricsVersions(detail.AvailableVersions, []string{publicLyricsGameVersion}) || detail.GameProjection != nil {
		return errors.New("public v2 compatibility Game-only shape is invalid")
	}
	if samePublicLyricsVersions(detail.AvailableVersions, []string{publicLyricsFullVersion, publicLyricsGameVersion}) {
		if detail.GameProjection == nil || len(detail.GameProjection.LineIDs) == 0 {
			return errors.New("public v2 compatibility Full/Game projection is missing")
		}
	} else if detail.GameProjection != nil {
		return errors.New("public v2 compatibility exposes an unexpected Game projection")
	}
	for _, attribution := range detail.Attributions {
		licenseName, licenseURL := publicLyricsProviderLicense(attribution.Provider)
		if licenseName == "" || attribution.LicenseName != licenseName || attribution.LicenseURL != licenseURL ||
			attribution.Title == "" || attribution.RevisionID <= 0 || attribution.RevisionURL == "" {
			return errors.New("public v2 compatibility attribution is invalid")
		}
	}
	body, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if err := validatePublicLyricsV2JSONShape(body); err != nil {
		return err
	}
	return validatePublicLyricsArtifactSize(detail)
}

func buildPublicLyricsV3Detail(item LyricsRecoveryItemBackupRecord, sourceRecord LyricsSourceDocumentBackupRecord, document model.LyricsSourceDocument, contributions map[string]string, artifacts map[string]LyricsRecoveryArtifactBackupRecord, localizations []LyricsRenditionLocalizationBackupRecord, translationLines []LyricsRenditionTranslationLineBackupRecord) (PublicLyricsV3DetailDocument, error) {
	if item.DocumentSHA256 != sourceRecord.DocumentSHA256 || sourceRecord.ManifestBatchSHA256 != item.BatchSHA256 {
		return PublicLyricsV3DetailDocument{}, errors.New("public v3 source ownership changed")
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	if len(contributions) != len(bindings) {
		return PublicLyricsV3DetailDocument{}, errors.New("public v3 contributions do not cover every rendition component")
	}
	identities := make(map[string]model.LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for _, identity := range document.FixedIdentities {
		if _, duplicate := identities[identity.RenditionKey]; duplicate {
			return PublicLyricsV3DetailDocument{}, errors.New("public v3 source document repeats a fixed identity")
		}
		identities[identity.RenditionKey] = identity
	}
	if len(artifacts) != len(identities) {
		return PublicLyricsV3DetailDocument{}, errors.New("public v3 artifacts do not cover every fixed identity")
	}
	for key, artifact := range artifacts {
		decoded, err := model.DecodeLyricsSourceFixedIdentity([]byte(artifact.FixedIdentityJSON))
		expected, found := identities[key]
		if err != nil || !found || decoded.RenditionKey != key || !reflect.DeepEqual(decoded, expected) {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 artifact identity %q differs from the source document", key)
		}
	}
	renditionKeys := make(map[string]struct{}, len(document.Renditions))
	for _, rendition := range document.Renditions {
		renditionKeys[rendition.RenditionKey] = struct{}{}
	}
	localizationByKey := make(map[string]LyricsRenditionLocalizationBackupRecord)
	linesByKey := make(map[string][]LyricsRenditionTranslationLineBackupRecord)
	for _, record := range localizations {
		if record.DocumentID != sourceRecord.DocumentID {
			continue
		}
		if _, found := renditionKeys[record.RenditionKey]; !found {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 localization %q is outside the source rendition set", record.RenditionKey)
		}
		if _, duplicate := localizationByKey[record.RenditionKey]; duplicate {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 localization %q is duplicated", record.RenditionKey)
		}
		if record.Locale != "zh-CN" || record.Revision <= 0 || record.UpdatedAt <= 0 {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 localization %q metadata is invalid", record.RenditionKey)
		}
		localizationByKey[record.RenditionKey] = record
	}
	localizationRevision := 1
	localizationUpdatedAt := sourceRecord.CreatedAt
	if len(localizationByKey) != 0 {
		if len(localizationByKey) != len(document.Renditions) {
			return PublicLyricsV3DetailDocument{}, errors.New("public v3 localizations do not cover every source rendition")
		}
		haveLocalizationMetadata := false
		for _, rendition := range document.Renditions {
			localization, found := localizationByKey[rendition.RenditionKey]
			if !found {
				return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 localization %q is missing", rendition.RenditionKey)
			}
			if !haveLocalizationMetadata {
				localizationRevision = localization.Revision
				localizationUpdatedAt = localization.UpdatedAt
				haveLocalizationMetadata = true
				continue
			}
			if localization.Revision != localizationRevision || localization.UpdatedAt != localizationUpdatedAt {
				return PublicLyricsV3DetailDocument{}, errors.New("public v3 localization metadata is inconsistent across renditions")
			}
		}
	}
	for _, record := range translationLines {
		if record.DocumentID != sourceRecord.DocumentID {
			continue
		}
		if _, found := renditionKeys[record.RenditionKey]; !found {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 translation line %q is outside the source rendition set", record.RenditionKey)
		}
		side, err := canonicalBackupTranslationSide(document, record.RenditionKey, record.Side)
		if err != nil {
			return PublicLyricsV3DetailDocument{}, err
		}
		if side == renditionPrimaryTranslationSide(document, record.RenditionKey) {
			linesByKey[record.RenditionKey] = append(linesByKey[record.RenditionKey], record)
		}
	}
	state, err := publicLyricsRecoveryState(item.State)
	if err != nil {
		return PublicLyricsV3DetailDocument{}, err
	}
	result := PublicLyricsV3DetailDocument{Version: 3, MusicID: item.MusicID, Revision: localizationRevision, UpdatedAt: formatTimestamp(localizationUpdatedAt), State: state, Renditions: make([]PublicLyricsV3Rendition, 0, len(document.Renditions))}
	orderedRenditions := append([]model.LyricsSourceRendition(nil), document.Renditions...)
	sort.Slice(orderedRenditions, func(left, right int) bool {
		return orderedRenditions[left].RenditionKey < orderedRenditions[right].RenditionKey
	})
	for _, rendition := range orderedRenditions {
		localization, localized := localizationByKey[rendition.RenditionKey]
		translated := append([]LyricsRenditionTranslationLineBackupRecord(nil), linesByKey[rendition.RenditionKey]...)
		sort.Slice(translated, func(left, right int) bool { return translated[left].Position < translated[right].Position })
		if !localized && len(translated) != 0 {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q has translation lines without localization", rendition.RenditionKey)
		}
		if localized && (localization.DocumentID != sourceRecord.DocumentID || localization.RenditionKey != rendition.RenditionKey || localization.Locale != "zh-CN") {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q localization identity changed", rendition.RenditionKey)
		}
		texts := make([]string, len(translated))
		for index, line := range translated {
			if line.Position != index || line.RenditionKey != rendition.RenditionKey || line.Locale != "zh-CN" || line.Side != "" && line.Side != renditionPrimaryTranslationSide(document, rendition.RenditionKey) {
				return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q translation lines are incomplete", rendition.RenditionKey)
			}
			texts[index] = line.Text
		}
		if len(texts) != 0 && len(texts) != renditionLineCountForStore(rendition) {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q translation lines do not cover the authoritative side", rendition.RenditionKey)
		}
		var localizationPtr *LyricsRenditionLocalizationBackupRecord
		if localized {
			localizationPtr = &localization
		}
		peerTranslations := map[string][]string{}
		for _, side := range []string{"full", "game"} {
			if side == renditionPrimaryTranslationSide(document, rendition.RenditionKey) {
				continue
			}
			var peer []LyricsRenditionTranslationLineBackupRecord
			for _, line := range translationLines {
				if line.DocumentID == sourceRecord.DocumentID && line.RenditionKey == rendition.RenditionKey && line.Locale == "zh-CN" && line.Side == side {
					peer = append(peer, line)
				}
			}
			sort.Slice(peer, func(left, right int) bool { return peer[left].Position < peer[right].Position })
			if len(peer) == 0 {
				continue
			}
			source, found := renditionPeerTranslationSide(document, rendition.RenditionKey, side)
			if !found || len(peer) != len(source.Lines) {
				return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q %s translation lines are incomplete", rendition.RenditionKey, side)
			}
			values := make([]string, len(peer))
			for index, line := range peer {
				if line.Position != index {
					return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q %s translation positions are incomplete", rendition.RenditionKey, side)
				}
				values[index] = line.Text
			}
			peerTranslations[side] = values
		}
		if len(texts) == 0 && len(peerTranslations) == 0 && localized &&
			(localization.TranslationCredit != "" || localization.ProofreadingCredit != "") {
			return PublicLyricsV3DetailDocument{}, fmt.Errorf("public v3 rendition %q credits exist without translations", rendition.RenditionKey)
		}
		publicRendition, err := buildPublicLyricsV3Rendition(rendition, texts, peerTranslations, localizationPtr, bindings, identities, contributions)
		if err != nil {
			return PublicLyricsV3DetailDocument{}, err
		}
		result.Renditions = append(result.Renditions, publicRendition)
	}
	return result, validatePublicLyricsV3Detail(result)
}

func buildPublicLyricsV3Rendition(rendition model.LyricsSourceRendition, translations []string, sideTranslations map[string][]string, localization *LyricsRenditionLocalizationBackupRecord, bindings []model.LyricsSourceRenditionComponentBinding, identities map[string]model.LyricsSourceFixedIdentity, contributions map[string]string) (PublicLyricsV3Rendition, error) {
	return buildLyricsV3Rendition(rendition, translations, sideTranslations, localization, bindings, identities, contributions, true)
}

func buildLyricsV3Rendition(rendition model.LyricsSourceRendition, translations []string, sideTranslations map[string][]string, localization *LyricsRenditionLocalizationBackupRecord, bindings []model.LyricsSourceRenditionComponentBinding, identities map[string]model.LyricsSourceFixedIdentity, contributions map[string]string, validatePublicCredits bool) (PublicLyricsV3Rendition, error) {
	result := PublicLyricsV3Rendition{Key: rendition.RenditionKey, Kind: rendition.SourceKind, SourceTabPaths: clonePublicV3TabPaths(rendition.SourceTabPaths), Relation: PublicLyricsV3Relation{Kind: rendition.Relation.Kind, FullRenditionKey: rendition.Relation.FullRenditionKey, LineIDs: append([]string(nil), rendition.Relation.LineIDs...)}, Performers: publicV3Performers(rendition.Full, rendition.Game)}
	if rendition.Full != nil {
		result.AvailableVersions = append(result.AvailableVersions, "full")
		result.Label = rendition.Full.Version.Label
		result.Full = publicV3Side(*rendition.Full, translations)
	}
	if rendition.Game != nil {
		result.AvailableVersions = append(result.AvailableVersions, "game")
		if result.Label == "" {
			result.Label = rendition.Game.Version.Label
		}
		gameTranslations := append([]string(nil), sideTranslations["game"]...)
		if rendition.Full == nil {
			gameTranslations = append([]string(nil), translations...)
		}
		if rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
			var err error
			gameTranslations, err = publicV3ProjectionTranslations(
				*rendition.Full, rendition.Relation.LineIDs, translations,
			)
			if err != nil {
				return PublicLyricsV3Rendition{}, err
			}
		}
		result.Game = publicV3Side(*rendition.Game, gameTranslations)
	} else if rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
		if rendition.Full == nil {
			return PublicLyricsV3Rendition{}, errors.New("public v3 exact projection has no Full side")
		}
		projected, err := publicV3ProjectedSide(
			*rendition.Full, rendition.Relation.LineIDs, translations,
		)
		if err != nil {
			return PublicLyricsV3Rendition{}, err
		}
		result.AvailableVersions = append(result.AvailableVersions, "game")
		result.Game = projected
	}
	if localization != nil && (localization.TranslationCredit != "" || localization.ProofreadingCredit != "") {
		result.TranslationCredits = &PublicLyricsV3TranslationCredits{Translation: localization.TranslationCredit, Proofreading: localization.ProofreadingCredit}
	}
	for _, binding := range bindings {
		if binding.RenditionKey != rendition.RenditionKey {
			continue
		}
		fixedIdentityKey, found := contributions[binding.ComponentKey]
		if !found || fixedIdentityKey != binding.FixedIdentityKey {
			return PublicLyricsV3Rendition{}, fmt.Errorf("public v3 component %s contribution identity is not canonical", binding.ComponentKey)
		}
		identity, ok := identities[fixedIdentityKey]
		if !ok {
			return PublicLyricsV3Rendition{}, fmt.Errorf("public v3 component %s identity is missing", binding.ComponentKey)
		}
		licenseName, licenseURL := publicLyricsProviderLicense(identity.Provider)
		if licenseName == "" || licenseURL == "" {
			return PublicLyricsV3Rendition{}, fmt.Errorf("public v3 component %s has no provider license", binding.ComponentKey)
		}
		result.Provenance = append(result.Provenance, PublicLyricsV3ComponentAttribution{Component: binding.ComponentKey, Provider: identity.Provider, Title: identity.Title, RevisionID: identity.RevisionID, RevisionURL: identity.CanonicalURL, LicenseName: licenseName, LicenseURL: licenseURL})
	}
	if validatePublicCredits {
		return result, validatePublicLyricsV3Rendition(result)
	}
	credits := result.TranslationCredits
	result.TranslationCredits = nil
	if err := validatePublicLyricsV3Rendition(result); err != nil {
		return PublicLyricsV3Rendition{}, err
	}
	result.TranslationCredits = credits
	return result, nil
}

func publicV3ProjectionTranslations(
	full model.LyricsSourceFull,
	lineIDs, translations []string,
) ([]string, error) {
	if len(translations) == 0 {
		return nil, nil
	}
	byLineID := make(map[string]string, len(full.Lines))
	for index, line := range full.Lines {
		if index < len(translations) {
			byLineID[line.ID] = translations[index]
		}
	}
	result := make([]string, len(lineIDs))
	for index, lineID := range lineIDs {
		value, found := byLineID[lineID]
		if !found {
			return nil, fmt.Errorf(
				"public v3 exact projection line %q has no Full translation", lineID,
			)
		}
		result[index] = value
	}
	return result, nil
}

func publicV3ProjectedSide(
	full model.LyricsSourceFull,
	lineIDs, translations []string,
) (*PublicLyricsV3Side, error) {
	projected := model.CloneLyricsSourceFull(&full)
	if projected == nil {
		return nil, errors.New("public v3 exact projection has no Full side")
	}
	fullLines := projected.Lines
	byLineID := make(map[string]model.LyricsSourceFullLine, len(fullLines))
	for _, line := range fullLines {
		byLineID[line.ID] = line
	}
	projected.Lines = make([]model.LyricsSourceFullLine, len(lineIDs))
	for index, lineID := range lineIDs {
		line, found := byLineID[lineID]
		if !found {
			return nil, fmt.Errorf(
				"public v3 exact projection references unknown Full line %q", lineID,
			)
		}
		line.ID = fmt.Sprintf("game-%06d", index+1)
		projected.Lines[index] = line
	}
	gameTranslations, err := publicV3ProjectionTranslations(full, lineIDs, translations)
	if err != nil {
		return nil, err
	}
	return publicV3Side(*projected, gameTranslations), nil
}

func publicV3Side(source model.LyricsSourceFull, translations []string) *PublicLyricsV3Side {
	result := &PublicLyricsV3Side{Version: source.Version, Lines: make([]PublicLyricsV3Line, len(source.Lines))}
	for index, line := range source.Lines {
		publicLine := PublicLyricsV3Line{ID: line.ID, Order: index, Japanese: line.Text, StanzaBreakBefore: line.StanzaBreakBefore, TrailingPerformerIDs: clonePublicV3Strings(line.TrailingPerformerIDs), Segments: make([]PublicLyricsV3Segment, len(line.Segments))}
		if index < len(translations) {
			publicLine.Chinese = translations[index]
		}
		for segmentIndex, segment := range line.Segments {
			publicRuby := make([]PublicLyricsV3RubySpan, len(segment.Ruby))
			for rubyIndex, span := range segment.Ruby {
				publicRuby[rubyIndex] = PublicLyricsV3RubySpan{Text: span.Text, Reading: span.Reading}
			}
			publicLine.Segments[segmentIndex] = PublicLyricsV3Segment{Text: segment.Text, PerformerIDs: clonePublicV3Strings(segment.PerformerIDs), Ruby: publicRuby}
		}
		result.Lines[index] = publicLine
	}
	return result
}

func publicV3Performers(full, game *model.LyricsSourceFull) []model.LyricsSourcePerformer {
	seen := map[string]model.LyricsSourcePerformer{}
	for _, source := range []*model.LyricsSourceFull{full, game} {
		if source == nil {
			continue
		}
		for _, performer := range source.Performers {
			seen[performer.PerformerID] = performer
		}
	}
	result := make([]model.LyricsSourcePerformer, 0, len(seen))
	for _, performer := range seen {
		result = append(result, performer)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].PerformerID < result[right].PerformerID })
	return result
}

func clonePublicV3Strings(input []string) []string {
	if input == nil {
		return nil
	}
	return append([]string{}, input...)
}

func clonePublicV3TabPaths(input []model.LyricsSourceTabPath) []model.LyricsSourceTabPath {
	result := make([]model.LyricsSourceTabPath, len(input))
	for index, path := range input {
		result[index] = append(model.LyricsSourceTabPath(nil), path...)
	}
	return result
}

func publicV3AvailableVersions(detail PublicLyricsV3DetailDocument) []string {
	seen := map[string]bool{}
	for _, rendition := range detail.Renditions {
		for _, version := range rendition.AvailableVersions {
			seen[version] = true
		}
	}
	result := []string{}
	for _, version := range []string{"full", "game"} {
		if seen[version] {
			result = append(result, version)
		}
	}
	return result
}

func validatePublicLyricsV3Index(index PublicLyricsV3IndexDocument) error {
	if index.Version != 3 || len(index.Songs) == 0 || len(index.Songs) > model.PublicLyricsMaxIndexEntries {
		return errors.New("public v3 index envelope is invalid")
	}
	last := 0
	for _, song := range index.Songs {
		if song.MusicID <= last || song.Revision <= 0 || !canonicalPublicV3Timestamp(song.UpdatedAt) ||
			song.Title.Japanese == "" || song.Title.Japanese != strings.TrimSpace(song.Title.Japanese) ||
			len(song.Title.Japanese) > model.PublicLyricsMaxTitleBytes || len(song.Title.Chinese) > model.PublicLyricsMaxTitleBytes ||
			len(song.Title.English) > model.PublicLyricsMaxTitleBytes || !utf8.ValidString(song.Title.Japanese) ||
			!utf8.ValidString(song.Title.Chinese) || !utf8.ValidString(song.Title.English) {
			return errors.New("public v3 index is not ordered or bounded")
		}
		last = song.MusicID
		switch song.State {
		case PublicLyricsStateComplete:
			if !samePublicV3Strings(song.AvailableVersions, []string{"full"}) &&
				!samePublicV3Strings(song.AvailableVersions, []string{"full", "game"}) || song.NoLyricsReason != "" {
				return fmt.Errorf("public v3 index music %d complete state is not lossless", song.MusicID)
			}
		case PublicLyricsStateGameOnly:
			if !samePublicV3Strings(song.AvailableVersions, []string{"game"}) || song.NoLyricsReason != "" {
				return fmt.Errorf("public v3 index music %d Game-only state is not lossless", song.MusicID)
			}
		case PublicLyricsStateSatisfiedNoLyrics:
			if song.AvailableVersions != nil || song.NoLyricsReason != model.LyricsAvailabilityNoLyricsCatalogInstrumental {
				return fmt.Errorf("public v3 index music %d no-lyrics state is invalid", song.MusicID)
			}
		case PublicLyricsStateAmbiguous, PublicLyricsStateMissing, PublicLyricsStateIncomplete, PublicLyricsStateFailed:
			if song.AvailableVersions != nil || song.NoLyricsReason != "" {
				return fmt.Errorf("public v3 index music %d unresolved state owns text", song.MusicID)
			}
		default:
			return fmt.Errorf("public v3 index music %d has unsupported state %q", song.MusicID, song.State)
		}
	}
	return nil
}

func validatePublicLyricsV3Candidate(candidate RecoveryPublicLyricsV3Candidate) error {
	if len(candidate.Index.Songs) == 0 || candidate.Details == nil {
		return errors.New("public v3 candidate envelope is invalid")
	}
	if err := validatePublicLyricsV3Index(candidate.Index); err != nil {
		return err
	}
	if err := validatePublicLyricsArtifactSize(candidate.Index); err != nil {
		return err
	}
	for _, song := range candidate.Index.Songs {
		detail, exists := candidate.Details[song.MusicID]
		expectsDetail := song.State == PublicLyricsStateComplete || song.State == PublicLyricsStateGameOnly
		if exists != expectsDetail {
			return fmt.Errorf("public v3 candidate music %d detail presence does not match state", song.MusicID)
		}
		if !exists {
			continue
		}
		if detail.Version != 3 || detail.MusicID != song.MusicID || detail.Revision != song.Revision || detail.UpdatedAt != song.UpdatedAt || detail.State != song.State ||
			!samePublicV3Strings(song.AvailableVersions, publicV3AvailableVersions(detail)) {
			return errors.New("public v3 candidate index/detail identity changed")
		}
		if err := validatePublicLyricsV3Detail(detail); err != nil {
			return fmt.Errorf("public v3 candidate music %d: %w", song.MusicID, err)
		}
		if err := validatePublicLyricsArtifactSize(detail); err != nil {
			return fmt.Errorf("public v3 candidate music %d: %w", song.MusicID, err)
		}
	}
	for musicID := range candidate.Details {
		found := false
		for _, song := range candidate.Index.Songs {
			if song.MusicID == musicID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("public v3 candidate detail %d is outside its index", musicID)
		}
	}
	return nil
}

func validatePublicLyricsV3Detail(detail PublicLyricsV3DetailDocument) error {
	if detail.Version != 3 || detail.MusicID <= 0 || detail.Revision <= 0 || !canonicalPublicV3Timestamp(detail.UpdatedAt) ||
		(detail.State != PublicLyricsStateComplete && detail.State != PublicLyricsStateGameOnly) || detail.Renditions == nil || len(detail.Renditions) == 0 || len(detail.Renditions) > 16 {
		return errors.New("public v3 detail envelope is invalid")
	}
	lastKey := ""
	seenPaths := map[string]string{}
	hasFull, hasGame := false, false
	for _, rendition := range detail.Renditions {
		if rendition.Key <= lastKey {
			return errors.New("public v3 renditions are not strictly ordered")
		}
		lastKey = rendition.Key
		if err := validatePublicLyricsV3Rendition(rendition); err != nil {
			return err
		}
		for _, path := range rendition.SourceTabPaths {
			key := strings.Join(path, "\x00")
			if owner, duplicate := seenPaths[key]; duplicate {
				return fmt.Errorf("public v3 source tab path is shared by renditions %q and %q", owner, rendition.Key)
			}
			seenPaths[key] = rendition.Key
		}
		hasFull = hasFull || rendition.Full != nil
		hasGame = hasGame || rendition.Game != nil
	}
	if !hasFull && !hasGame || detail.State == PublicLyricsStateComplete && !hasFull ||
		detail.State == PublicLyricsStateGameOnly && (hasFull || !hasGame) {
		return errors.New("public v3 detail state does not match rendition sides")
	}
	return nil
}

func validatePublicLyricsV3Rendition(rendition PublicLyricsV3Rendition) error {
	if !publicV3RenditionKeyPattern.MatchString(rendition.Key) || rendition.Label == "" || rendition.Label != strings.TrimSpace(rendition.Label) || len(rendition.Label) > 2048 || !utf8.ValidString(rendition.Label) ||
		!model.IsValidLyricsSourceRenditionKind(rendition.Kind) || rendition.SourceTabPaths == nil || len(rendition.SourceTabPaths) == 0 || len(rendition.SourceTabPaths) > 32 ||
		rendition.AvailableVersions == nil || len(rendition.AvailableVersions) == 0 || rendition.Performers == nil || len(rendition.Performers) > 256 || rendition.Provenance == nil || len(rendition.Provenance) == 0 || len(rendition.Provenance) > 128 {
		return errors.New("public v3 rendition header is invalid")
	}
	expectedVersions := []string{}
	if rendition.Full != nil {
		expectedVersions = append(expectedVersions, "full")
	}
	if rendition.Game != nil {
		expectedVersions = append(expectedVersions, "game")
	}
	if !samePublicV3Strings(rendition.AvailableVersions, expectedVersions) {
		return errors.New("public v3 rendition available versions are not lossless")
	}
	if !model.IsValidLyricsSourceRenditionRelationKind(rendition.Relation.Kind) {
		return errors.New("public v3 rendition relation is invalid")
	}
	if err := validatePublicV3TabPaths(rendition.SourceTabPaths); err != nil {
		return err
	}
	performers := make(map[string]struct{}, len(rendition.Performers))
	lastPerformer := ""
	for _, performer := range rendition.Performers {
		if performer.PerformerID == "" || performer.PerformerID <= lastPerformer || performer.PerformerID != strings.TrimSpace(performer.PerformerID) ||
			len(performer.PerformerID) > 128 || len(performer.Name) == 0 || len(performer.Name) > 2048 ||
			performer.Name != strings.TrimSpace(performer.Name) || !utf8.ValidString(performer.PerformerID) ||
			!utf8.ValidString(performer.Name) || strings.ContainsAny(performer.PerformerID+performer.Name, "\r\n\x00") ||
			performer.Color != "" && !publicV3ColorPattern.MatchString(performer.Color) {
			return errors.New("public v3 rendition performers are invalid")
		}
		lastPerformer = performer.PerformerID
		performers[performer.PerformerID] = struct{}{}
	}
	if err := validatePublicV3Side(rendition.Full, rendition.Kind, performers); err != nil {
		return err
	}
	if err := validatePublicV3Side(rendition.Game, rendition.Kind, performers); err != nil {
		return err
	}
	if rendition.Relation.Kind == model.LyricsSourceRenditionRelationNone {
		if rendition.Relation.FullRenditionKey != "" || rendition.Relation.LineIDs != nil {
			return errors.New("public v3 none relation contains projection data")
		}
	} else {
		if rendition.Full == nil || rendition.Game == nil || rendition.Relation.FullRenditionKey != rendition.Key || rendition.Relation.LineIDs == nil || len(rendition.Relation.LineIDs) != len(rendition.Game.Lines) {
			return errors.New("public v3 exact projection relation is incomplete")
		}
		fullLines := make(map[string]PublicLyricsV3Line, len(rendition.Full.Lines))
		for _, line := range rendition.Full.Lines {
			fullLines[line.ID] = line
		}
		seen := make(map[string]struct{}, len(rendition.Relation.LineIDs))
		previousOrder := -1
		for index, lineID := range rendition.Relation.LineIDs {
			fullLine, found := fullLines[lineID]
			if !found {
				return errors.New("public v3 exact projection references an unknown Full line")
			}
			if _, duplicate := seen[lineID]; duplicate {
				return errors.New("public v3 exact projection repeats a Full line")
			}
			if fullLine.Order <= previousOrder {
				return errors.New("public v3 exact projection Full references are out of order")
			}
			gameLine := rendition.Game.Lines[index]
			if fullLine.Japanese != gameLine.Japanese {
				return errors.New("public v3 exact projection Game text differs from Full")
			}
			if fullLine.Chinese != gameLine.Chinese || fullLine.English != gameLine.English {
				return errors.New("public v3 exact projection Game translations differ from Full")
			}
			seen[lineID] = struct{}{}
			previousOrder = fullLine.Order
		}
	}
	if rendition.TranslationCredits != nil {
		credits := rendition.TranslationCredits
		if credits.Translation != strings.TrimSpace(credits.Translation) || credits.Proofreading != strings.TrimSpace(credits.Proofreading) ||
			len(credits.Translation) > 2048 || len(credits.Proofreading) > 2048 || !utf8.ValidString(credits.Translation) || !utf8.ValidString(credits.Proofreading) || credits.Translation == "" && credits.Proofreading == "" {
			return errors.New("public v3 rendition translation credits are invalid")
		}
	}
	lastComponentRank := -1
	seenComponents := make(map[string]struct{}, len(rendition.Provenance))
	componentPrefix := "renditions/" + rendition.Key + "/"
	for _, attribution := range rendition.Provenance {
		if !strings.HasPrefix(attribution.Component, componentPrefix) || attribution.Component == componentPrefix ||
			len(attribution.Component) > 512 || !model.IsValidLyricsSourceProvider(attribution.Provider) ||
			attribution.Title == "" || attribution.Title != strings.TrimSpace(attribution.Title) || len(attribution.Title) > 2048 ||
			!validPublicV3RevisionURL(attribution) ||
			attribution.LicenseName == "" || len(attribution.LicenseName) > 512 ||
			!strings.HasPrefix(attribution.LicenseURL, "https://") || len(attribution.LicenseURL) > 4096 ||
			!utf8.ValidString(attribution.Component) || !utf8.ValidString(attribution.Title) ||
			!utf8.ValidString(attribution.RevisionURL) || !utf8.ValidString(attribution.LicenseName) ||
			!utf8.ValidString(attribution.LicenseURL) || strings.ContainsAny(attribution.Component+attribution.Title+
			attribution.RevisionURL+attribution.LicenseName+attribution.LicenseURL, "\r\n\x00") {
			return errors.New("public v3 rendition provenance is invalid")
		}
		licenseName, licenseURL := publicLyricsProviderLicense(attribution.Provider)
		if attribution.LicenseName != licenseName || attribution.LicenseURL != licenseURL {
			return errors.New("public v3 rendition source license does not match its provider")
		}
		if _, duplicate := seenComponents[attribution.Component]; duplicate {
			return errors.New("public v3 rendition provenance repeats a component")
		}
		seenComponents[attribution.Component] = struct{}{}
		component := model.LyricsSourceRenditionComponentKind(strings.TrimPrefix(attribution.Component, componentPrefix))
		rank := model.LyricsSourceRenditionComponentRank(component)
		if rank < 0 || rank <= lastComponentRank {
			return errors.New("public v3 rendition provenance is not in canonical component order")
		}
		lastComponentRank = rank
	}
	hasComponent := func(component string) bool {
		_, ok := seenComponents[componentPrefix+component]
		return ok
	}
	if !hasComponent("relation") || !hasComponent("version") {
		return errors.New("public v3 rendition provenance is missing relation or version evidence")
	}
	if rendition.Full != nil && !hasComponent("full_text") {
		return errors.New("public v3 rendition Full side is missing full_text provenance")
	}
	if rendition.Full == nil && (hasComponent("full_text") || hasComponent("full_performer_segmentation") || hasComponent("full_ruby")) {
		return errors.New("public v3 rendition has Full provenance without a Full side")
	}
	if rendition.Game != nil && rendition.Relation.Kind == model.LyricsSourceRenditionRelationNone && !hasComponent("game_text") {
		return errors.New("public v3 independent Game side is missing game_text provenance")
	}
	if rendition.Game == nil && (hasComponent("game_text") || hasComponent("game_performer_segmentation") || hasComponent("game_ruby")) {
		return errors.New("public v3 rendition has Game provenance without a Game side")
	}
	return nil
}

func validPublicV3RevisionURL(attribution PublicLyricsV3ComponentAttribution) bool {
	if attribution.RevisionID <= 0 || attribution.RevisionURL == "" ||
		len(attribution.RevisionURL) > 4096 || attribution.RevisionURL != strings.TrimSpace(attribution.RevisionURL) {
		return false
	}
	parsed, err := url.Parse(attribution.RevisionURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Opaque != "" || parsed.Host == "" || parsed.Path == "" || parsed.ForceQuery {
		return false
	}
	if !model.LyricsSourceProviderAcceptsOrigin(attribution.Provider, parsed.Scheme+"://"+parsed.Host) {
		return false
	}
	if attribution.Provider == model.LyricsSourceProviderMoegirlPublicExact {
		return parsed.RawQuery == "" && parsed.EscapedPath() != "/" &&
			strings.HasPrefix(parsed.EscapedPath(), "/") && parsed.String() == attribution.RevisionURL
	}
	query := parsed.Query()
	if len(query["oldid"]) != 1 || query.Get("oldid") != strconv.Itoa(attribution.RevisionID) ||
		parsed.RawQuery != query.Encode() {
		return false
	}
	switch attribution.Provider {
	case model.LyricsSourceProviderVocaloidFandom, model.LyricsSourceProviderSekaipedia:
		return strings.HasPrefix(parsed.EscapedPath(), "/wiki/") && parsed.EscapedPath() != "/wiki/" && len(query) == 1
	case model.LyricsSourceProviderMoegirl:
		if strings.HasPrefix(parsed.EscapedPath(), "/wiki/") && parsed.EscapedPath() != "/wiki/" {
			return len(query) == 1
		}
		return parsed.EscapedPath() == "/index.php" && len(query) == 2 &&
			len(query["title"]) == 1 && strings.TrimSpace(query.Get("title")) != ""
	default:
		return false
	}
}

func validatePublicV3Side(side *PublicLyricsV3Side, kind model.LyricsSourceRenditionKind, performers map[string]struct{}) error {
	if side == nil {
		return nil
	}
	if side.Version.Kind != string(kind) || side.Version.Label == "" || side.Version.Label != strings.TrimSpace(side.Version.Label) || len(side.Version.Label) > 2048 || side.Lines == nil || len(side.Lines) == 0 || len(side.Lines) > maxLyricsLines {
		return errors.New("public v3 rendition side is invalid")
	}
	seenLines := make(map[string]struct{}, len(side.Lines))
	for index, line := range side.Lines {
		if line.ID == "" || line.ID != strings.TrimSpace(line.ID) || len(line.ID) > 128 || line.Order != index ||
			line.Japanese == "" || len(line.Japanese) > maxLyricsLineTextBytes ||
			!utf8.ValidString(line.ID) || !utf8.ValidString(line.Japanese) || strings.ContainsAny(line.ID+line.Japanese, "\r\n\x00") ||
			line.Segments == nil || len(line.Segments) == 0 || len(line.Segments) > maxLyricsSegmentsPerLine || line.TrailingPerformerIDs == nil {
			return fmt.Errorf("public v3 side line %d is invalid", index+1)
		}
		if _, duplicate := seenLines[line.ID]; duplicate {
			return errors.New("public v3 side repeats a line ID")
		}
		seenLines[line.ID] = struct{}{}
		if len(line.Chinese) > maxLyricsLineTextBytes || len(line.English) > maxLyricsLineTextBytes || !utf8.ValidString(line.Chinese) || !utf8.ValidString(line.English) {
			return fmt.Errorf("public v3 side line %d translation is invalid", index+1)
		}
		if err := validatePublicV3PerformerIDs(line.TrailingPerformerIDs, performers); err != nil {
			return fmt.Errorf("public v3 side line %d trailing performers: %w", index+1, err)
		}
		var lineText strings.Builder
		for segmentIndex, segment := range line.Segments {
			if segment.Text == "" || len(segment.Text) > maxLyricsLineTextBytes || !utf8.ValidString(segment.Text) ||
				strings.ContainsAny(segment.Text, "\r\n\x00") || segment.PerformerIDs == nil ||
				len(segment.PerformerIDs) > maxLyricsPerformers || segment.Ruby == nil || len(segment.Ruby) == 0 || len(segment.Ruby) > 8192 {
				return fmt.Errorf("public v3 side line %d segment %d is invalid", index+1, segmentIndex+1)
			}
			if err := validatePublicV3PerformerIDs(segment.PerformerIDs, performers); err != nil {
				return fmt.Errorf("public v3 side line %d segment %d performers: %w", index+1, segmentIndex+1, err)
			}
			var rubyText strings.Builder
			for rubyIndex, span := range segment.Ruby {
				if span.Text == "" || len(span.Text) > maxLyricsLineTextBytes || len(span.Reading) > maxLyricsLineTextBytes ||
					!utf8.ValidString(span.Text) || !utf8.ValidString(span.Reading) ||
					strings.ContainsAny(span.Text+span.Reading, "\r\n\x00") {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d is invalid", index+1, segmentIndex+1, rubyIndex+1)
				}
				if publicV3TextContainsHan(span.Text) && span.Reading == "" {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d is missing a Han reading", index+1, segmentIndex+1, rubyIndex+1)
				}
				if span.Reading != "" && !publicV3ReadingBaseSpan(span.Text) {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d reading base contains a non-Han character", index+1, segmentIndex+1, rubyIndex+1)
				}
				if span.Reading != "" && !publicV3KanaReading(span.Reading) {
					return fmt.Errorf("public v3 side line %d segment %d ruby %d reading is not kana", index+1, segmentIndex+1, rubyIndex+1)
				}
				rubyText.WriteString(span.Text)
			}
			if rubyText.String() != segment.Text {
				return fmt.Errorf("public v3 side line %d segment %d ruby does not concatenate", index+1, segmentIndex+1)
			}
			lineText.WriteString(segment.Text)
		}
		if lineText.String() != line.Japanese {
			return fmt.Errorf("public v3 side line %d segments do not concatenate", index+1)
		}
	}
	return nil
}

func publicV3KanaReading(value string) bool {
	hasKana := false
	for _, current := range value {
		switch {
		case unicode.In(current, unicode.Hiragana, unicode.Katakana):
			hasKana = true
		case current == 'ー' || current == '・' || unicode.Is(unicode.Mn, current) || unicode.Is(unicode.Mc, current):
			if !hasKana {
				return false
			}
		default:
			return false
		}
	}
	return hasKana
}

func publicV3TextContainsHan(text string) bool {
	for _, current := range text {
		if model.LyricsSourceRubyBaseRune(current) {
			return true
		}
	}
	return false
}

func publicV3ReadingBaseSpan(text string) bool {
	for _, current := range text {
		if model.LyricsSourceRubyBaseRune(current) {
			continue
		}
		return false
	}
	return text != ""
}

func validatePublicV3PerformerIDs(ids []string, performers map[string]struct{}) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsAny(id, "\r\n\x00") {
			return errors.New("performer ID is invalid")
		}
		if _, found := performers[id]; !found {
			return errors.New("performer ID is outside the rendition roster")
		}
		if _, duplicate := seen[id]; duplicate {
			return errors.New("performer IDs contain a duplicate")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validatePublicV3TabPaths(paths []model.LyricsSourceTabPath) error {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == nil || len(path) == 0 || len(path) > 8 {
			return errors.New("public v3 source tab path is invalid")
		}
		key := strings.Join(path, "\x00")
		if _, duplicate := seen[key]; duplicate {
			return errors.New("public v3 source tab paths contain a duplicate")
		}
		seen[key] = struct{}{}
		for _, label := range path {
			if label == "" || label != strings.TrimSpace(label) || len(label) > 512 || !utf8.ValidString(label) || strings.ContainsAny(label, "\r\n\x00") {
				return errors.New("public v3 source tab path label is invalid")
			}
		}
	}
	return nil
}

func canonicalPublicV3Timestamp(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Unix() > 0 && strings.HasSuffix(value, "Z") && parsed.UTC().Format(time.RFC3339Nano) == value
}

func samePublicV3Strings(left, right []string) bool {
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

func publicV3LineIDs(side *PublicLyricsV3Side) []string {
	if side == nil {
		return nil
	}
	result := make([]string, len(side.Lines))
	for index, line := range side.Lines {
		result[index] = line.ID
	}
	return result
}

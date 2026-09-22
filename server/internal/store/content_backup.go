package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/legacy"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
)

type EntryLocalizationRecord struct {
	Category  string `json:"category"`
	Field     string `json:"field"`
	JPKey     string `json:"jpKey"`
	Locale    string `json:"locale"`
	Text      string `json:"text"`
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updatedAt"`
	UpdatedBy string `json:"updatedBy"`
	Revision  int    `json:"revision"`
}

type EventSegmentRecord struct {
	SegmentID  string `json:"segmentId"`
	EventID    int    `json:"eventId"`
	EpisodeNo  string `json:"episodeNo"`
	ScenarioID string `json:"scenarioId"`
	Kind       string `json:"kind"`
	Position   int    `json:"position"`
	JPKey      string `json:"jpKey"`
	SourceText string `json:"sourceText"`
	SourceHash string `json:"sourceHash"`
}

type EventLocalizationRecord struct {
	SegmentID string `json:"segmentId"`
	Locale    string `json:"locale"`
	Text      string `json:"text"`
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updatedAt"`
	UpdatedBy string `json:"updatedBy"`
	Revision  int    `json:"revision"`
}

type EventLocaleMetaRecord struct {
	EventID     int    `json:"eventId"`
	Locale      string `json:"locale"`
	LastUpdated int64  `json:"lastUpdated"`
}

type EventContentExport struct {
	Segments      []EventSegmentRecord      `json:"segments"`
	Localizations []EventLocalizationRecord `json:"localizations"`
	LocaleMeta    []EventLocaleMetaRecord   `json:"localeMeta"`
	Scenarios     []EventScenarioRecord     `json:"scenarios,omitempty"`
}

type CatalogMusicBackupRecord struct {
	MusicID                    int                           `json:"musicId"`
	TitleJA                    string                        `json:"titleJa"`
	TitleZH                    string                        `json:"titleZh"`
	TitleEN                    string                        `json:"titleEn"`
	JacketURL                  string                        `json:"jacketUrl"`
	NewlyWritten               int                           `json:"newlyWritten"`
	UpdatedAt                  int64                         `json:"updatedAt"`
	ProducerMetadata           string                        `json:"producerMetadata"`
	Lyricist                   string                        `json:"lyricist,omitempty"`
	Composer                   string                        `json:"composer,omitempty"`
	Arranger                   string                        `json:"arranger,omitempty"`
	AssetbundleName            string                        `json:"assetbundleName,omitempty"`
	VersionHint                string                        `json:"versionHint,omitempty"`
	LyricsVersion              string                        `json:"lyricsVersion,omitempty"`
	LyricsEvidencePresence     model.CatalogEvidencePresence `json:"lyricsEvidencePresence,omitempty"`
	Vocals                     []model.CatalogVocalSignal    `json:"vocals,omitempty"`
	LyricsCatalogFingerprint   string                        `json:"lyricsCatalogFingerprint,omitempty"`
	LyricsCatalogPolicyVersion string                        `json:"lyricsCatalogPolicyVersion,omitempty"`
}

type CatalogPerformerBackupRecord struct {
	PerformerID int    `json:"performerId"`
	NameJA      string `json:"nameJa"`
	NameZH      string `json:"nameZh"`
	NameEN      string `json:"nameEn"`
	UpdatedAt   int64  `json:"updatedAt"`
}

type LyricsDocumentBackupRecord struct {
	MusicID                int    `json:"musicId"`
	Revision               int    `json:"revision"`
	UpdatedAt              int64  `json:"updatedAt"`
	UpdatedBy              string `json:"updatedBy"`
	Attribution            string `json:"attribution"`
	TranslationCredit      string `json:"translationCredit,omitempty"`
	ProofreadingCredit     string `json:"proofreadingCredit,omitempty"`
	SourceNote             string `json:"sourceNote"`
	SourceURL              string `json:"sourceUrl"`
	LicenseNote            string `json:"licenseNote"`
	SourceHash             string `json:"sourceHash"`
	SourcePageID           int    `json:"sourcePageId"`
	SourceRevisionID       int    `json:"sourceRevisionId"`
	SourceSHA1             string `json:"sourceSha1"`
	SourceFetchedAt        int64  `json:"sourceFetchedAt"`
	SourceFetchedAtRFC3339 string `json:"sourceFetchedAtRfc3339,omitempty"`
}

type LyricsLineBackupRecord struct {
	MusicID           int    `json:"musicId"`
	LineID            string `json:"lineId"`
	Position          int    `json:"position"`
	Japanese          string `json:"japanese"`
	Chinese           string `json:"zh-CN"`
	English           string `json:"en-US"`
	StanzaBreakBefore int    `json:"stanzaBreakBefore"`
}

type LyricsSegmentBackupRecord struct {
	MusicID          int    `json:"musicId"`
	LineID           string `json:"lineId"`
	Position         int    `json:"position"`
	Text             string `json:"text"`
	PerformerIDsJSON string `json:"performerIdsJson"`
	RubyJSON         string `json:"rubyJson"`
}

type LyricsPublicationBackupRecord struct {
	MusicID     int    `json:"musicId"`
	Revision    int    `json:"revision"`
	UpdatedAt   int64  `json:"updatedAt"`
	PayloadJSON string `json:"payloadJson"`
}

type LyricsPublicWithdrawalBackupRecord struct {
	MusicID     int    `json:"musicId"`
	WithdrawnAt int64  `json:"withdrawnAt"`
	WithdrawnBy string `json:"withdrawnBy"`
}

type LyricsSourceDocumentBackupRecord struct {
	DocumentID                   int64  `json:"documentId"`
	MusicID                      int    `json:"musicId"`
	SchemaVersion                int    `json:"schemaVersion"`
	ReasonCode                   string `json:"reasonCode"`
	DocumentJSON                 string `json:"documentJson"`
	DocumentSHA256               string `json:"documentSha256"`
	ManifestBatchSHA256          string `json:"manifestBatchSha256"`
	RenditionLocalizationsSHA256 string `json:"renditionLocalizationsSha256,omitempty"`
	CreatedAt                    int64  `json:"createdAt"`
}

type LyricsSourceArtifactBackupRecord struct {
	DocumentID              int64  `json:"documentId"`
	Provider                string `json:"provider"`
	RenditionKey            string `json:"renditionKey"`
	Origin                  string `json:"origin"`
	PageID                  int    `json:"pageId"`
	RevisionID              int    `json:"revisionId"`
	RevisionTimestamp       string `json:"revisionTimestamp,omitempty"`
	MediaWikiSHA1           string `json:"mediawikiSha1"`
	PageTitle               string `json:"pageTitle"`
	CanonicalRevisionURL    string `json:"canonicalRevisionUrl"`
	FetchedAt               string `json:"fetchedAt"`
	CategoriesJSON          string `json:"categoriesJson"`
	Section                 string `json:"section"`
	CompositionRenditionKey string `json:"compositionRenditionKey,omitempty"`
	VersionReason           string `json:"versionReason,omitempty"`
	IndexEvidenceRefsJSON   string `json:"indexEvidenceRefsJson"`
	FixedIdentityJSON       string `json:"fixedIdentityJson"`
	FixedIdentitySHA256     string `json:"fixedIdentitySha256"`
	RawByteCount            int    `json:"rawByteCount"`
	RawWikitextSHA256       string `json:"rawWikitextSha256"`
	ArtifactSHA256          string `json:"artifactSha256"`
}

type LyricsSourceIndexEvidenceBackupRecord struct {
	Provider             string `json:"provider"`
	EvidenceID           string `json:"evidenceId"`
	SHA256               string `json:"sha256"`
	Kind                 string `json:"kind"`
	Origin               string `json:"origin"`
	PageID               int    `json:"pageId,omitempty"`
	RevisionID           int    `json:"revisionId,omitempty"`
	RevisionTimestamp    string `json:"revisionTimestamp,omitempty"`
	MediaWikiSHA1        string `json:"mediawikiSha1,omitempty"`
	PageTitle            string `json:"pageTitle,omitempty"`
	CanonicalRevisionURL string `json:"canonicalRevisionUrl,omitempty"`
	CategoriesJSON       string `json:"categoriesJson"`
	CanonicalRequestURL  string `json:"canonicalRequestUrl,omitempty"`
	FetchedAt            string `json:"fetchedAt"`
	RawBytes             []byte `json:"rawBytes"`
	RawByteCount         int    `json:"rawByteCount"`
	RawSHA256            string `json:"rawSha256"`
	CreatedAt            int64  `json:"createdAt"`
}

type LyricsSourceArtifactEvidenceBackupRecord struct {
	DocumentID   int64  `json:"documentId"`
	RenditionKey string `json:"renditionKey"`
	Position     int    `json:"position"`
	Provider     string `json:"provider"`
	EvidenceID   string `json:"evidenceId"`
	SHA256       string `json:"sha256"`
}

type LyricsSourceContributionBackupRecord struct {
	DocumentID         int64  `json:"documentId"`
	Component          string `json:"component"`
	RenditionKey       string `json:"renditionKey"`
	ContributionSHA256 string `json:"contributionSha256"`
}

type LyricsRenditionLocalizationBackupRecord struct {
	DocumentID         int64  `json:"documentId"`
	RenditionKey       string `json:"renditionKey"`
	Locale             string `json:"locale"`
	TranslationCredit  string `json:"translationCredit"`
	ProofreadingCredit string `json:"proofreadingCredit"`
	UpdatedAt          int64  `json:"updatedAt"`
	UpdatedBy          string `json:"updatedBy"`
	Revision           int    `json:"revision"`
}

type LyricsRenditionTranslationLineBackupRecord struct {
	DocumentID   int64  `json:"documentId"`
	RenditionKey string `json:"renditionKey"`
	// Side is omitted for the historical primary-side representation (Full when
	// present, otherwise Game). Independent peer-side translations use an
	// explicit "full" or "game" value so old content backups remain readable.
	Side     string `json:"side,omitempty"`
	Locale   string `json:"locale"`
	Position int    `json:"position"`
	Text     string `json:"text"`
}

type LyricsTranslationEditionStateBackupRecord struct {
	DocumentID        int64  `json:"documentId"`
	DefaultEditionKey string `json:"defaultEditionKey"`
	Revision          int    `json:"revision"`
	UpdatedAt         int64  `json:"updatedAt"`
	UpdatedBy         string `json:"updatedBy"`
}

type LyricsTranslationEditionBackupRecord struct {
	DocumentID int64  `json:"documentId"`
	EditionKey string `json:"editionKey"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"createdAt"`
	CreatedBy  string `json:"createdBy"`
}

type LyricsTranslationEditionLocalizationBackupRecord struct {
	DocumentID         int64  `json:"documentId"`
	EditionKey         string `json:"editionKey"`
	RenditionKey       string `json:"renditionKey"`
	Locale             string `json:"locale"`
	TranslationCredit  string `json:"translationCredit"`
	ProofreadingCredit string `json:"proofreadingCredit"`
	UpdatedAt          int64  `json:"updatedAt"`
	UpdatedBy          string `json:"updatedBy"`
}

type LyricsTranslationEditionLineBackupRecord struct {
	DocumentID   int64  `json:"documentId"`
	EditionKey   string `json:"editionKey"`
	RenditionKey string `json:"renditionKey"`
	Side         string `json:"side"`
	Locale       string `json:"locale"`
	Position     int    `json:"position"`
	Text         string `json:"text"`
}

type LyricsContentExport struct {
	Music                           []CatalogMusicBackupRecord                         `json:"music"`
	Performers                      []CatalogPerformerBackupRecord                     `json:"performers"`
	Documents                       []LyricsDocumentBackupRecord                       `json:"documents"`
	Lines                           []LyricsLineBackupRecord                           `json:"lines"`
	Segments                        []LyricsSegmentBackupRecord                        `json:"segments"`
	Publications                    []LyricsPublicationBackupRecord                    `json:"publications"`
	PublicWithdrawals               []LyricsPublicWithdrawalBackupRecord               `json:"publicWithdrawals,omitempty"`
	SourceDocuments                 []LyricsSourceDocumentBackupRecord                 `json:"sourceDocuments,omitempty"`
	SourceArtifacts                 []LyricsSourceArtifactBackupRecord                 `json:"sourceArtifacts,omitempty"`
	SourceIndexEvidence             []LyricsSourceIndexEvidenceBackupRecord            `json:"sourceIndexEvidence,omitempty"`
	SourceArtifactEvidence          []LyricsSourceArtifactEvidenceBackupRecord         `json:"sourceArtifactEvidence,omitempty"`
	SourceContributions             []LyricsSourceContributionBackupRecord             `json:"sourceContributions,omitempty"`
	RenditionLocalizations          []LyricsRenditionLocalizationBackupRecord          `json:"renditionLocalizations,omitempty"`
	RenditionTranslationLines       []LyricsRenditionTranslationLineBackupRecord       `json:"renditionTranslationLines,omitempty"`
	TranslationEditionStates        []LyricsTranslationEditionStateBackupRecord        `json:"translationEditionStates,omitempty"`
	TranslationEditions             []LyricsTranslationEditionBackupRecord             `json:"translationEditions,omitempty"`
	TranslationEditionLocalizations []LyricsTranslationEditionLocalizationBackupRecord `json:"translationEditionLocalizations,omitempty"`
	TranslationEditionLines         []LyricsTranslationEditionLineBackupRecord         `json:"translationEditionLines,omitempty"`
	RecoveryBatches                 []LyricsRecoveryBatchBackupRecord                  `json:"recoveryBatches,omitempty"`
	RecoveryItems                   []LyricsRecoveryItemBackupRecord                   `json:"recoveryItems,omitempty"`
	RecoverySourceEvidence          []LyricsRecoverySourceEvidenceBackupRecord         `json:"recoverySourceEvidence,omitempty"`
	RecoveryArtifacts               []LyricsRecoveryArtifactBackupRecord               `json:"recoveryArtifacts,omitempty"`
	RecoveryArtifactEvidence        []LyricsRecoveryArtifactEvidenceBackupRecord       `json:"recoveryArtifactEvidence,omitempty"`
	RecoveryContributions           []LyricsRecoveryContributionBackupRecord           `json:"recoveryContributions,omitempty"`
	AvailabilityDocuments           []LyricsAvailabilityDocumentBackupRecord           `json:"availabilityDocuments,omitempty"`
}

type LegacyEventRestore struct {
	EventID  int
	Meta     model.EventStoryMeta
	Episodes []OrderedEpisode
}

func (s *Store) ExportEntryLocalizations() ([]EntryLocalizationRecord, error) {
	return s.ExportEntryLocalizationsContext(context.Background())
}

func (s *Store) ExportEntryLocalizationsContext(ctx context.Context) ([]EntryLocalizationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT category, field, jp_key, locale, text, source, updated_at, updated_by, revision
		FROM entry_localizations ORDER BY category, field, jp_key, locale`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []EntryLocalizationRecord{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var record EntryLocalizationRecord
		if err := rows.Scan(&record.Category, &record.Field, &record.JPKey, &record.Locale, &record.Text,
			&record.Source, &record.UpdatedAt, &record.UpdatedBy, &record.Revision); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) ExportEventContent() (EventContentExport, error) {
	return s.ExportEventContentContext(context.Background())
}

func (s *Store) ExportEventContentContext(ctx context.Context) (EventContentExport, error) {
	result := EventContentExport{
		Segments: []EventSegmentRecord{}, Localizations: []EventLocalizationRecord{},
		LocaleMeta: []EventLocaleMetaRecord{}, Scenarios: []EventScenarioRecord{},
	}
	type episodeIdentity struct {
		eventID   int
		episodeNo string
	}
	canonicalSegmentIDs := map[episodeIdentity]map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT scenario.event_id, scenario.episode_no, scenario.scenario_id,
		scenario.canonical_json, scenario.sha256
		FROM event_story_scenarios scenario
		JOIN event_story_episodes episode
		ON episode.event_id=scenario.event_id AND episode.episode_no=scenario.episode_no
		AND episode.scenario_id=scenario.scenario_id
		ORDER BY scenario.event_id, scenario.episode_no`)
	if err != nil {
		return result, err
	}
	var scenarios []EventScenarioRecord
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return result, err
		}
		var record EventScenarioRecord
		if err := rows.Scan(&record.EventID, &record.EpisodeNo, &record.ScenarioID, &record.CanonicalJSON,
			&record.SHA256); err != nil {
			rows.Close()
			return result, err
		}
		if err := ValidateEventScenarioRecord(record); err != nil {
			rows.Close()
			return result, fmt.Errorf("event scenario %d/%s: %w", record.EventID, record.EpisodeNo, err)
		}
		scenarios = append(scenarios, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for _, record := range scenarios {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		expected, covered, err := eventEpisodeCanonicalSegments(s.db, record)
		if err != nil {
			return result, err
		}
		if !covered {
			return result, fmt.Errorf("event scenario %d/%s has incomplete segment coverage", record.EventID, record.EpisodeNo)
		}
		ids := make(map[string]bool, len(expected))
		for segmentID := range expected {
			ids[segmentID] = true
		}
		canonicalSegmentIDs[episodeIdentity{eventID: record.EventID, episodeNo: record.EpisodeNo}] = ids
		result.Scenarios = append(result.Scenarios, record)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT segment.segment_id, segment.event_id, segment.episode_no, segment.scenario_id,
		segment.kind, segment.position, segment.jp_key, segment.source_text, segment.source_hash
		FROM event_story_segments segment
		JOIN event_story_episodes episode
		ON episode.event_id=segment.event_id AND episode.episode_no=segment.episode_no
		AND episode.scenario_id=segment.scenario_id
		ORDER BY segment.event_id, segment.episode_no, segment.kind, segment.position`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return result, err
		}
		var record EventSegmentRecord
		if err := rows.Scan(&record.SegmentID, &record.EventID, &record.EpisodeNo, &record.ScenarioID,
			&record.Kind, &record.Position, &record.JPKey, &record.SourceText, &record.SourceHash); err != nil {
			rows.Close()
			return result, err
		}
		if canonicalIDs := canonicalSegmentIDs[episodeIdentity{eventID: record.EventID, episodeNo: record.EpisodeNo}]; canonicalIDs != nil && !canonicalIDs[record.SegmentID] {
			continue
		}
		result.Segments = append(result.Segments, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	exportedSegmentIDs := make(map[string]bool, len(result.Segments))
	for _, segment := range result.Segments {
		exportedSegmentIDs[segment.SegmentID] = true
	}
	rows, err = s.db.QueryContext(ctx, `SELECT localization.segment_id, localization.locale, localization.text,
		localization.source, localization.updated_at, localization.updated_by, localization.revision
		FROM event_story_segment_localizations localization
		JOIN event_story_segments segment ON segment.segment_id=localization.segment_id
		JOIN event_story_episodes episode
		ON episode.event_id=segment.event_id AND episode.episode_no=segment.episode_no
		AND episode.scenario_id=segment.scenario_id
		ORDER BY localization.segment_id, localization.locale`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return result, err
		}
		var record EventLocalizationRecord
		if err := rows.Scan(&record.SegmentID, &record.Locale, &record.Text, &record.Source,
			&record.UpdatedAt, &record.UpdatedBy, &record.Revision); err != nil {
			rows.Close()
			return result, err
		}
		if !exportedSegmentIDs[record.SegmentID] {
			continue
		}
		result.Localizations = append(result.Localizations, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT meta.event_id, meta.locale, meta.last_updated
		FROM event_story_locale_meta meta
		JOIN event_stories story ON story.event_id=meta.event_id
		ORDER BY meta.event_id, meta.locale`)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var record EventLocaleMetaRecord
		if err := rows.Scan(&record.EventID, &record.Locale, &record.LastUpdated); err != nil {
			return result, err
		}
		result.LocaleMeta = append(result.LocaleMeta, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) ExportLyricsContent() (LyricsContentExport, error) {
	return s.ExportLyricsContentContext(context.Background())
}

func (s *Store) ExportLyricsContentContext(ctx context.Context) (LyricsContentExport, error) {
	return s.exportLyricsContentSnapshot(ctx, nil)
}

func (s *Store) exportLyricsContentSnapshot(ctx context.Context, afterDocuments func()) (LyricsContentExport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LyricsContentExport{}, err
	}
	defer tx.Rollback()
	result := LyricsContentExport{
		Music: []CatalogMusicBackupRecord{}, Performers: []CatalogPerformerBackupRecord{},
		Documents: []LyricsDocumentBackupRecord{}, Lines: []LyricsLineBackupRecord{},
		Segments: []LyricsSegmentBackupRecord{}, Publications: []LyricsPublicationBackupRecord{},
		SourceDocuments: []LyricsSourceDocumentBackupRecord{}, SourceArtifacts: []LyricsSourceArtifactBackupRecord{},
		SourceIndexEvidence:       []LyricsSourceIndexEvidenceBackupRecord{},
		SourceArtifactEvidence:    []LyricsSourceArtifactEvidenceBackupRecord{},
		SourceContributions:       []LyricsSourceContributionBackupRecord{},
		RenditionLocalizations:    []LyricsRenditionLocalizationBackupRecord{},
		RenditionTranslationLines: []LyricsRenditionTranslationLineBackupRecord{},
		RecoveryBatches:           []LyricsRecoveryBatchBackupRecord{},
		RecoveryItems:             []LyricsRecoveryItemBackupRecord{},
		RecoverySourceEvidence:    []LyricsRecoverySourceEvidenceBackupRecord{},
		RecoveryArtifacts:         []LyricsRecoveryArtifactBackupRecord{},
		RecoveryArtifactEvidence:  []LyricsRecoveryArtifactEvidenceBackupRecord{},
		RecoveryContributions:     []LyricsRecoveryContributionBackupRecord{},
		AvailabilityDocuments:     []LyricsAvailabilityDocumentBackupRecord{},
	}
	translationLinesQuery := `SELECT document_id,rendition_key,'' AS side,locale,position,text
		FROM song_lyrics_rendition_translation_lines
		ORDER BY document_id,rendition_key,side,locale,position`
	var hasPeerTranslationSchema, hasTranslationEditionSchema int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=29)`).Scan(&hasPeerTranslationSchema); err != nil {
		return result, fmt.Errorf("inspect lyrics peer-translation schema: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=30)`).Scan(&hasTranslationEditionSchema); err != nil {
		return result, fmt.Errorf("inspect lyrics translation-edition schema: %w", err)
	}
	if hasPeerTranslationSchema == 1 {
		translationLinesQuery = `SELECT document_id,rendition_key,'' AS side,locale,position,text
			FROM song_lyrics_rendition_translation_lines
			UNION ALL
			SELECT document_id,rendition_key,side,locale,position,text
			FROM song_lyrics_rendition_side_translation_lines
			ORDER BY document_id,rendition_key,side,locale,position`
	}
	queries := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{`SELECT music_id, title_ja, title_zh, title_en, jacket_url, newly_written, updated_at, producer_metadata,
			lyricist, composer, arranger, assetbundle_name, version_hint, lyrics_version,
			lyrics_evidence_presence_json, vocal_signals_json, lyrics_catalog_fingerprint, lyrics_catalog_policy_version
			FROM catalog_music ORDER BY music_id`, func(rows *sql.Rows) error {
			var record CatalogMusicBackupRecord
			var presenceJSON, vocalsJSON string
			if err := rows.Scan(&record.MusicID, &record.TitleJA, &record.TitleZH, &record.TitleEN, &record.JacketURL,
				&record.NewlyWritten, &record.UpdatedAt, &record.ProducerMetadata, &record.Lyricist, &record.Composer,
				&record.Arranger, &record.AssetbundleName, &record.VersionHint, &record.LyricsVersion, &presenceJSON,
				&vocalsJSON, &record.LyricsCatalogFingerprint, &record.LyricsCatalogPolicyVersion); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(presenceJSON), &record.LyricsEvidencePresence); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(vocalsJSON), &record.Vocals); err != nil {
				return err
			}
			result.Music = append(result.Music, record)
			return nil
		}},
		{`SELECT performer_id, name_ja, name_zh, name_en, updated_at FROM catalog_performers ORDER BY performer_id`, func(rows *sql.Rows) error {
			var record CatalogPerformerBackupRecord
			if err := rows.Scan(&record.PerformerID, &record.NameJA, &record.NameZH, &record.NameEN, &record.UpdatedAt); err != nil {
				return err
			}
			result.Performers = append(result.Performers, record)
			return nil
		}},
		{`SELECT music_id, revision, updated_at, updated_by, attribution, translation_credit, proofreading_credit,
			source_note, source_url, license_note, source_hash, source_page_id, source_revision_id, source_sha1,
			source_fetched_at, source_fetched_at_rfc3339 FROM song_lyrics ORDER BY music_id`, func(rows *sql.Rows) error {
			var record LyricsDocumentBackupRecord
			if err := rows.Scan(&record.MusicID, &record.Revision, &record.UpdatedAt, &record.UpdatedBy,
				&record.Attribution, &record.TranslationCredit, &record.ProofreadingCredit,
				&record.SourceNote, &record.SourceURL, &record.LicenseNote, &record.SourceHash,
				&record.SourcePageID, &record.SourceRevisionID, &record.SourceSHA1, &record.SourceFetchedAt,
				&record.SourceFetchedAtRFC3339); err != nil {
				return err
			}
			result.Documents = append(result.Documents, record)
			return nil
		}},
		{`SELECT music_id, line_id, position, japanese, zh_cn, en_us, stanza_break_before FROM song_lyric_lines ORDER BY music_id, position`, func(rows *sql.Rows) error {
			var record LyricsLineBackupRecord
			if err := rows.Scan(&record.MusicID, &record.LineID, &record.Position, &record.Japanese, &record.Chinese, &record.English, &record.StanzaBreakBefore); err != nil {
				return err
			}
			result.Lines = append(result.Lines, record)
			return nil
		}},
		{`SELECT music_id, line_id, position, text, performer_ids_json, ruby_json FROM song_lyric_segments ORDER BY music_id, line_id, position`, func(rows *sql.Rows) error {
			var record LyricsSegmentBackupRecord
			if err := rows.Scan(&record.MusicID, &record.LineID, &record.Position, &record.Text, &record.PerformerIDsJSON, &record.RubyJSON); err != nil {
				return err
			}
			result.Segments = append(result.Segments, record)
			return nil
		}},
		{`SELECT music_id, revision, updated_at, payload_json FROM song_lyrics_publications ORDER BY music_id`, func(rows *sql.Rows) error {
			var record LyricsPublicationBackupRecord
			if err := rows.Scan(&record.MusicID, &record.Revision, &record.UpdatedAt, &record.PayloadJSON); err != nil {
				return err
			}
			result.Publications = append(result.Publications, record)
			return nil
		}},
		{`SELECT music_id, withdrawn_at, withdrawn_by FROM song_lyrics_public_withdrawals ORDER BY music_id`, func(rows *sql.Rows) error {
			var record LyricsPublicWithdrawalBackupRecord
			if err := rows.Scan(&record.MusicID, &record.WithdrawnAt, &record.WithdrawnBy); err != nil {
				return err
			}
			result.PublicWithdrawals = append(result.PublicWithdrawals, record)
			return nil
		}},
		{`SELECT document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at
			FROM song_lyrics_source_documents ORDER BY music_id`, func(rows *sql.Rows) error {
			var record LyricsSourceDocumentBackupRecord
			if err := rows.Scan(&record.DocumentID, &record.MusicID, &record.SchemaVersion, &record.ReasonCode,
				&record.DocumentJSON, &record.DocumentSHA256, &record.ManifestBatchSHA256, &record.CreatedAt); err != nil {
				return err
			}
			result.SourceDocuments = append(result.SourceDocuments, record)
			return nil
		}},
		{`SELECT document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,
			mediawiki_sha1,page_title,canonical_revision_url,fetched_at,categories_json,section,
			composition_rendition_key,version_reason,index_evidence_refs_json,fixed_identity_json,
			fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256
			FROM song_lyrics_source_artifacts ORDER BY document_id,rendition_key`, func(rows *sql.Rows) error {
			var record LyricsSourceArtifactBackupRecord
			if err := rows.Scan(&record.DocumentID, &record.Provider, &record.RenditionKey, &record.Origin,
				&record.PageID, &record.RevisionID, &record.RevisionTimestamp, &record.MediaWikiSHA1, &record.PageTitle,
				&record.CanonicalRevisionURL, &record.FetchedAt, &record.CategoriesJSON, &record.Section,
				&record.CompositionRenditionKey, &record.VersionReason, &record.IndexEvidenceRefsJSON,
				&record.FixedIdentityJSON, &record.FixedIdentitySHA256,
				&record.RawByteCount, &record.RawWikitextSHA256, &record.ArtifactSHA256); err != nil {
				return err
			}
			result.SourceArtifacts = append(result.SourceArtifacts, record)
			return nil
		}},
		{`SELECT evidence.provider,evidence.evidence_id,evidence.sha256,evidence.kind,evidence.origin,
			evidence.page_id,evidence.revision_id,evidence.revision_timestamp,evidence.mediawiki_sha1,evidence.page_title,
			evidence.canonical_revision_url,evidence.categories_json,evidence.canonical_request_url,
			evidence.fetched_at,evidence.raw_bytes,evidence.raw_byte_count,evidence.raw_sha256,evidence.created_at
			FROM lyrics_source_index_evidence evidence
			WHERE EXISTS (
				SELECT 1 FROM song_lyrics_source_artifact_index_evidence link
				WHERE link.provider=evidence.provider AND link.evidence_id=evidence.evidence_id
				  AND link.sha256=evidence.sha256
			)
			ORDER BY evidence.provider,evidence.evidence_id`, func(rows *sql.Rows) error {
			var record LyricsSourceIndexEvidenceBackupRecord
			var pageID, revisionID sql.NullInt64
			if err := rows.Scan(&record.Provider, &record.EvidenceID, &record.SHA256, &record.Kind, &record.Origin,
				&pageID, &revisionID, &record.RevisionTimestamp, &record.MediaWikiSHA1, &record.PageTitle,
				&record.CanonicalRevisionURL, &record.CategoriesJSON, &record.CanonicalRequestURL, &record.FetchedAt,
				&record.RawBytes, &record.RawByteCount, &record.RawSHA256, &record.CreatedAt); err != nil {
				return err
			}
			if pageID.Valid {
				record.PageID = int(pageID.Int64)
			}
			if revisionID.Valid {
				record.RevisionID = int(revisionID.Int64)
			}
			record.RawBytes = append([]byte(nil), record.RawBytes...)
			revisionTimestamp, err := contentBackupEvidenceRevisionTimestamp(record.Provider, record.Kind, record.RawBytes)
			if err != nil || record.RevisionTimestamp != revisionTimestamp {
				return fmt.Errorf("lyrics source index evidence %s/%s revision timestamp does not match exact raw evidence", record.Provider, record.EvidenceID)
			}
			result.SourceIndexEvidence = append(result.SourceIndexEvidence, record)
			return nil
		}},
		{`SELECT document_id,rendition_key,position,provider,evidence_id,sha256
			FROM song_lyrics_source_artifact_index_evidence ORDER BY document_id,rendition_key,position`, func(rows *sql.Rows) error {
			var record LyricsSourceArtifactEvidenceBackupRecord
			if err := rows.Scan(&record.DocumentID, &record.RenditionKey, &record.Position, &record.Provider,
				&record.EvidenceID, &record.SHA256); err != nil {
				return err
			}
			result.SourceArtifactEvidence = append(result.SourceArtifactEvidence, record)
			return nil
		}},
		{`SELECT document_id,component,rendition_key,contribution_sha256
			FROM song_lyrics_component_contributions ORDER BY document_id,component`, func(rows *sql.Rows) error {
			var record LyricsSourceContributionBackupRecord
			if err := rows.Scan(&record.DocumentID, &record.Component, &record.RenditionKey, &record.ContributionSHA256); err != nil {
				return err
			}
			result.SourceContributions = append(result.SourceContributions, record)
			return nil
		}},
		{`SELECT document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision
			FROM song_lyrics_rendition_localizations ORDER BY document_id,rendition_key,locale`, func(rows *sql.Rows) error {
			var record LyricsRenditionLocalizationBackupRecord
			if err := rows.Scan(&record.DocumentID, &record.RenditionKey, &record.Locale,
				&record.TranslationCredit, &record.ProofreadingCredit, &record.UpdatedAt, &record.UpdatedBy, &record.Revision); err != nil {
				return err
			}
			result.RenditionLocalizations = append(result.RenditionLocalizations, record)
			return nil
		}},
		{translationLinesQuery, func(rows *sql.Rows) error {
			var record LyricsRenditionTranslationLineBackupRecord
			if err := rows.Scan(&record.DocumentID, &record.RenditionKey, &record.Side, &record.Locale, &record.Position, &record.Text); err != nil {
				return err
			}
			result.RenditionTranslationLines = append(result.RenditionTranslationLines, record)
			return nil
		}},
	}
	if hasTranslationEditionSchema == 1 {
		queries = append(queries,
			struct {
				query string
				scan  func(*sql.Rows) error
			}{`SELECT document_id,default_edition_key,revision,updated_at,updated_by
				FROM song_lyrics_translation_edition_state ORDER BY document_id`, func(rows *sql.Rows) error {
				var record LyricsTranslationEditionStateBackupRecord
				if err := rows.Scan(&record.DocumentID, &record.DefaultEditionKey, &record.Revision, &record.UpdatedAt, &record.UpdatedBy); err != nil {
					return err
				}
				result.TranslationEditionStates = append(result.TranslationEditionStates, record)
				return nil
			}},
			struct {
				query string
				scan  func(*sql.Rows) error
			}{`SELECT document_id,edition_key,label,created_at,created_by
				FROM song_lyrics_translation_editions ORDER BY document_id,edition_key`, func(rows *sql.Rows) error {
				var record LyricsTranslationEditionBackupRecord
				if err := rows.Scan(&record.DocumentID, &record.EditionKey, &record.Label, &record.CreatedAt, &record.CreatedBy); err != nil {
					return err
				}
				result.TranslationEditions = append(result.TranslationEditions, record)
				return nil
			}},
			struct {
				query string
				scan  func(*sql.Rows) error
			}{`SELECT document_id,edition_key,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by
				FROM song_lyrics_translation_edition_localizations ORDER BY document_id,edition_key,rendition_key,locale`, func(rows *sql.Rows) error {
				var record LyricsTranslationEditionLocalizationBackupRecord
				if err := rows.Scan(&record.DocumentID, &record.EditionKey, &record.RenditionKey, &record.Locale,
					&record.TranslationCredit, &record.ProofreadingCredit, &record.UpdatedAt, &record.UpdatedBy); err != nil {
					return err
				}
				result.TranslationEditionLocalizations = append(result.TranslationEditionLocalizations, record)
				return nil
			}},
			struct {
				query string
				scan  func(*sql.Rows) error
			}{`SELECT document_id,edition_key,rendition_key,side,locale,position,text
				FROM song_lyrics_translation_edition_lines ORDER BY document_id,edition_key,rendition_key,side,locale,position`, func(rows *sql.Rows) error {
				var record LyricsTranslationEditionLineBackupRecord
				if err := rows.Scan(&record.DocumentID, &record.EditionKey, &record.RenditionKey, &record.Side,
					&record.Locale, &record.Position, &record.Text); err != nil {
					return err
				}
				result.TranslationEditionLines = append(result.TranslationEditionLines, record)
				return nil
			}},
		)
	}
	for queryIndex, item := range queries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		rows, err := tx.QueryContext(ctx, item.query)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return result, err
			}
			if err := item.scan(rows); err != nil {
				rows.Close()
				return result, err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return result, err
		}
		if err := rows.Close(); err != nil {
			return result, err
		}
		if queryIndex == 2 && afterDocuments != nil {
			afterDocuments()
		}
	}
	if err := exportRecoveryLyricsContentTx(ctx, tx, &result); err != nil {
		return result, err
	}
	if err := bindRenditionLocalizationBackupDigests(&result); err != nil {
		return result, err
	}
	documentIDs := make(map[int]bool, len(result.Documents))
	for _, document := range result.Documents {
		documentIDs[document.MusicID] = true
	}
	musicIDs := make(map[int]bool, len(result.Music))
	for _, music := range result.Music {
		musicIDs[music.MusicID] = true
	}
	if err := repairMissingExportArtifactEvidenceLinks(ctx, tx, &result); err != nil {
		return result, err
	}
	if err := validateRestoredLyricsSourceProvenance(result, documentIDs); err != nil {
		return result, fmt.Errorf("export lyrics source provenance: %w", err)
	}
	if err := validateRestoredLyricsRecoveryProvenance(result, documentIDs, musicIDs); err != nil {
		return result, fmt.Errorf("export lyrics recovery provenance: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) ImportTranslationContent(entries []EntryLocalizationRecord, events EventContentExport, lyrics LyricsContentExport) error {
	return s.ImportTranslationContentContext(context.Background(), entries, events, lyrics)
}

func (s *Store) ImportTranslationContentContext(ctx context.Context, entries []EntryLocalizationRecord, events EventContentExport, lyrics LyricsContentExport) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Fence every pre-restore Yjs room in the same transaction as the
	// authoritative replacement. The collaboration table intentionally has no
	// catalog foreign key: catalog replacement must not erase this generation
	// ledger and make an old epoch reusable.
	if _, err := tx.ExecContext(ctx, `UPDATE lyrics_collab_documents SET epoch=epoch+1, updated_at=?`, time.Now().UTC().Unix()); err != nil {
		return err
	}
	if err := importTranslationContentTx(ctx, tx, entries, events, lyrics); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateLocalizationProjectionCache()
	s.NotifyChange()
	return nil
}

func importTranslationContentTx(ctx context.Context, tx *sql.Tx, entries []EntryLocalizationRecord, events EventContentExport, lyrics LyricsContentExport) error {
	catalogPerformers := newCatalogPerformerAliases()
	sortedPerformers := append([]CatalogPerformerBackupRecord(nil), lyrics.Performers...)
	sort.Slice(sortedPerformers, func(left, right int) bool {
		return sortedPerformers[left].PerformerID < sortedPerformers[right].PerformerID
	})
	for _, performer := range sortedPerformers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if performer.PerformerID <= 0 || catalogPerformers.validIDs[performer.PerformerID] {
			return fmt.Errorf("lyrics performer %d is invalid or duplicated", performer.PerformerID)
		}
		addCatalogPerformerAliases(&catalogPerformers, performer.PerformerID,
			performer.NameJA, performer.NameZH, performer.NameEN)
	}
	addCanonicalLyricsSourcePerformerAliases(&catalogPerformers)
	if err := addAuditedExternalLyricsPerformerAliases(&catalogPerformers); err != nil {
		return err
	}
	performerIDs := catalogPerformers.validIDs
	musicIDs := make(map[int]bool, len(lyrics.Music))
	for index := range lyrics.Music {
		music := &lyrics.Music[index]
		if music.MusicID <= 0 || musicIDs[music.MusicID] {
			return fmt.Errorf("lyrics catalog music %d is invalid or duplicated", music.MusicID)
		}
		if err := canonicalizeCatalogMusicBackupRecord(music); err != nil {
			return fmt.Errorf("lyrics catalog music %d: %w", music.MusicID, err)
		}
		musicIDs[music.MusicID] = true
	}
	documentIDs := make(map[int]bool, len(lyrics.Documents))
	documentRevisions := make(map[int]int, len(lyrics.Documents))
	for _, document := range lyrics.Documents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !musicIDs[document.MusicID] || document.Revision <= 0 || document.UpdatedAt < 0 || documentIDs[document.MusicID] {
			return fmt.Errorf("lyrics document %d has invalid identity or metadata", document.MusicID)
		}
		documentIDs[document.MusicID] = true
		documentRevisions[document.MusicID] = document.Revision
	}
	if err := validateRestoredLyricsDocuments(lyrics, performerIDs, documentIDs); err != nil {
		return err
	}
	if err := validateRestoredLyricsSourceProvenance(lyrics, documentIDs); err != nil {
		return err
	}
	if err := validateRestoredLyricsRecoveryProvenance(lyrics, documentIDs, musicIDs); err != nil {
		return err
	}
	sourceBundles, err := restoredPublicLyricsSourceBundles(lyrics)
	if err != nil {
		return err
	}
	lyrics.Publications = append([]LyricsPublicationBackupRecord(nil), lyrics.Publications...)
	for publicationIndex := range lyrics.Publications {
		if err := ctx.Err(); err != nil {
			return err
		}
		publication := &lyrics.Publications[publicationIndex]
		if !documentIDs[publication.MusicID] {
			return fmt.Errorf("lyrics publication %d has no draft document", publication.MusicID)
		}
		if publication.Revision > documentRevisions[publication.MusicID] {
			return fmt.Errorf("lyrics publication %d revision exceeds its draft document", publication.MusicID)
		}
		if err := canonicalizeRestoredPublicationWithSource(
			publication, performerIDs, catalogPerformers, sourceBundles[publication.MusicID],
		); err != nil {
			return err
		}
	}
	// These side tables intentionally do not reference legacy event parents so
	// previous binaries can keep doing replace-imports without cascading away
	// new locale data. Restore still validates parent identity explicitly.
	for _, record := range events.Segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		var parentScenarioID string
		if err := tx.QueryRowContext(ctx, `SELECT scenario_id FROM event_story_episodes WHERE event_id=? AND episode_no=?`,
			record.EventID, record.EpisodeNo).Scan(&parentScenarioID); err == sql.ErrNoRows {
			return fmt.Errorf("event segment %s references a missing episode", record.SegmentID)
		} else if err != nil {
			return err
		}
		if parentScenarioID != record.ScenarioID {
			return fmt.Errorf("event segment %s scenario identity does not match its parent", record.SegmentID)
		}
	}
	for _, record := range events.LocaleMeta {
		if err := ctx.Err(); err != nil {
			return err
		}
		var parentCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_stories WHERE event_id=?`, record.EventID).Scan(&parentCount); err != nil {
			return err
		}
		if parentCount != 1 {
			return fmt.Errorf("event locale metadata references missing event %d", record.EventID)
		}
	}
	for _, record := range events.Scenarios {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ValidateEventScenarioRecord(record); err != nil {
			return fmt.Errorf("event scenario %d/%s: %w", record.EventID, record.EpisodeNo, err)
		}
		var parentScenarioID string
		if err := tx.QueryRowContext(ctx, `SELECT scenario_id FROM event_story_episodes WHERE event_id=? AND episode_no=?`,
			record.EventID, record.EpisodeNo).Scan(&parentScenarioID); err == sql.ErrNoRows {
			return fmt.Errorf("event scenario %d/%s references a missing episode", record.EventID, record.EpisodeNo)
		} else if err != nil {
			return err
		}
		if parentScenarioID != record.ScenarioID {
			return fmt.Errorf("event scenario %d/%s identity does not match its parent", record.EventID, record.EpisodeNo)
		}
	}
	if err := validateEventContentCanonicalCoverage(events); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := suspendLyricsSourceDocumentDeleteGuardsTx(ctx, tx); err != nil {
		return err
	}
	if err := suspendEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx, tx); err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM entry_localizations`,
		`DELETE FROM event_story_locale_meta`,
		`DELETE FROM event_story_segment_localizations`,
		`DELETE FROM event_story_segments`,
		`DELETE FROM event_story_scenarios`,
		`DELETE FROM song_lyrics_publications`,
		`DELETE FROM song_lyrics_public_withdrawals`,
		`DELETE FROM song_lyric_segments`,
		`DELETE FROM song_lyric_lines`,
		`DELETE FROM song_lyrics_translation_edition_lines`,
		`DELETE FROM song_lyrics_translation_edition_localizations`,
		`DELETE FROM song_lyrics_translation_edition_state`,
		`DELETE FROM song_lyrics_translation_editions`,
		`DELETE FROM song_lyrics_rendition_side_translation_lines`,
		`DELETE FROM song_lyrics_rendition_translation_lines`,
		`DELETE FROM song_lyrics_rendition_localizations`,
		`DELETE FROM song_lyrics`,
		`DELETE FROM catalog_performers`,
		// The seed ledger references catalog_music with ON DELETE RESTRICT; it is
		// replayed from the embedded bundle at the next startup.
		`DELETE FROM embedded_lyrics_editor_seed_items`,
		`DELETE FROM embedded_lyrics_editor_seed_batches`,
		`DELETE FROM catalog_music`,
		`DELETE FROM lyrics_recovery_import_batches`,
		`DELETE FROM lyrics_recovery_source_evidence`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := restoreEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx, tx); err != nil {
		return err
	}
	for _, record := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entry_localizations(category, field, jp_key, locale, text, source, updated_at, updated_by, revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Category, record.Field, record.JPKey, record.Locale,
			record.Text, record.Source, record.UpdatedAt, record.UpdatedBy, record.Revision); err != nil {
			return err
		}
	}
	for _, record := range events.Segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_story_segments(segment_id, event_id, episode_no, scenario_id, kind, position, jp_key, source_text, source_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.SegmentID, record.EventID, record.EpisodeNo, record.ScenarioID,
			record.Kind, record.Position, record.JPKey, record.SourceText, record.SourceHash); err != nil {
			return fmt.Errorf("event segment %s: %w", record.SegmentID, err)
		}
	}
	for _, record := range events.Localizations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_story_segment_localizations(segment_id, locale, text, source, updated_at, updated_by, revision)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, record.SegmentID, record.Locale, record.Text, record.Source,
			record.UpdatedAt, record.UpdatedBy, record.Revision); err != nil {
			return err
		}
	}
	for _, record := range events.LocaleMeta {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_story_locale_meta(event_id, locale, last_updated) VALUES (?, ?, ?)`,
			record.EventID, record.Locale, record.LastUpdated); err != nil {
			return err
		}
	}
	for _, record := range events.Scenarios {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_story_scenarios(event_id, episode_no, scenario_id, canonical_json, sha256)
			VALUES (?, ?, ?, ?, ?)`, record.EventID, record.EpisodeNo, record.ScenarioID, record.CanonicalJSON, record.SHA256); err != nil {
			return err
		}
	}
	for _, record := range lyrics.Music {
		if err := ctx.Err(); err != nil {
			return err
		}
		presenceJSON, _ := json.Marshal(record.LyricsEvidencePresence)
		vocalsJSON, _ := json.Marshal(record.Vocals)
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_music(music_id, title_ja, title_zh, title_en, jacket_url,
			newly_written, updated_at, producer_metadata, lyricist, composer, arranger, assetbundle_name, version_hint,
			lyrics_version, lyrics_evidence_presence_json, vocal_signals_json, lyrics_catalog_fingerprint,
			lyrics_catalog_policy_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.MusicID, record.TitleJA, record.TitleZH, record.TitleEN, record.JacketURL, record.NewlyWritten,
			record.UpdatedAt, record.ProducerMetadata, record.Lyricist, record.Composer, record.Arranger,
			record.AssetbundleName, record.VersionHint, record.LyricsVersion, string(presenceJSON), string(vocalsJSON),
			record.LyricsCatalogFingerprint, record.LyricsCatalogPolicyVersion); err != nil {
			return err
		}
	}
	for _, record := range lyrics.Performers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_performers(performer_id, name_ja, name_zh, name_en, updated_at)
			VALUES (?, ?, ?, ?, ?)`, record.PerformerID, record.NameJA, record.NameZH, record.NameEN, record.UpdatedAt); err != nil {
			return err
		}
	}
	for _, record := range lyrics.Documents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics(music_id, revision, updated_at, updated_by,
			attribution, translation_credit, proofreading_credit, source_note, source_url, license_note, source_hash,
			source_page_id, source_revision_id, source_sha1, source_fetched_at, source_fetched_at_rfc3339)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.MusicID, record.Revision, record.UpdatedAt, record.UpdatedBy,
			record.Attribution, record.TranslationCredit, record.ProofreadingCredit,
			record.SourceNote, record.SourceURL, record.LicenseNote, record.SourceHash,
			record.SourcePageID, record.SourceRevisionID, record.SourceSHA1, record.SourceFetchedAt,
			record.SourceFetchedAtRFC3339); err != nil {
			return err
		}
	}
	for _, record := range lyrics.SourceIndexEvidence {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := restoreLyricsSourceIndexEvidenceTx(ctx, tx, record); err != nil {
			return fmt.Errorf("restore lyrics source index evidence %s/%s: %w", record.Provider, record.EvidenceID, err)
		}
	}
	for _, record := range lyrics.SourceDocuments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_documents
			(document_id,music_id,schema_version,reason_code,document_json,document_sha256,manifest_batch_sha256,created_at)
			VALUES (?,?,?,?,?,?,?,?)`, record.DocumentID, record.MusicID, record.SchemaVersion, record.ReasonCode,
			record.DocumentJSON, record.DocumentSHA256, record.ManifestBatchSHA256, record.CreatedAt); err != nil {
			return err
		}
	}
	for _, record := range lyrics.RenditionLocalizations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
			VALUES (?,?,?,?,?,?,?,?)`, record.DocumentID, record.RenditionKey, record.Locale,
			record.TranslationCredit, record.ProofreadingCredit, record.UpdatedAt, record.UpdatedBy, record.Revision); err != nil {
			return err
		}
	}
	for _, record := range lyrics.RenditionTranslationLines {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Side == "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_translation_lines
				(document_id,rendition_key,locale,position,text) VALUES (?,?,?,?,?)`, record.DocumentID,
				record.RenditionKey, record.Locale, record.Position, record.Text); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_side_translation_lines
			(document_id,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?)`, record.DocumentID,
			record.RenditionKey, record.Side, record.Locale, record.Position, record.Text); err != nil {
			return err
		}
	}
	for _, record := range lyrics.TranslationEditions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_translation_editions
			(document_id,edition_key,label,created_at,created_by) VALUES (?,?,?,?,?)`, record.DocumentID,
			record.EditionKey, record.Label, record.CreatedAt, record.CreatedBy); err != nil {
			return err
		}
	}
	for _, record := range lyrics.TranslationEditionStates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_translation_edition_state
			(document_id,default_edition_key,revision,updated_at,updated_by) VALUES (?,?,?,?,?)`, record.DocumentID,
			record.DefaultEditionKey, record.Revision, record.UpdatedAt, record.UpdatedBy); err != nil {
			return err
		}
	}
	for _, record := range lyrics.TranslationEditionLocalizations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_translation_edition_localizations
			(document_id,edition_key,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by)
			VALUES (?,?,?,?,?,?,?,?)`, record.DocumentID, record.EditionKey, record.RenditionKey, record.Locale,
			record.TranslationCredit, record.ProofreadingCredit, record.UpdatedAt, record.UpdatedBy); err != nil {
			return err
		}
	}
	for _, record := range lyrics.TranslationEditionLines {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_translation_edition_lines
			(document_id,edition_key,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?,?)`, record.DocumentID,
			record.EditionKey, record.RenditionKey, record.Side, record.Locale, record.Position, record.Text); err != nil {
			return err
		}
	}
	for _, record := range lyrics.SourceArtifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_artifacts
			(document_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,mediawiki_sha1,page_title,
			 canonical_revision_url,fetched_at,categories_json,section,composition_rendition_key,version_reason,
			 index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.DocumentID, record.Provider, record.RenditionKey,
			record.Origin, record.PageID, record.RevisionID, record.RevisionTimestamp, record.MediaWikiSHA1, record.PageTitle,
			record.CanonicalRevisionURL, record.FetchedAt, record.CategoriesJSON, record.Section,
			record.CompositionRenditionKey, record.VersionReason, record.IndexEvidenceRefsJSON,
			record.FixedIdentityJSON, record.FixedIdentitySHA256, record.RawByteCount,
			record.RawWikitextSHA256, record.ArtifactSHA256); err != nil {
			return err
		}
	}
	for _, record := range lyrics.SourceArtifactEvidence {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_source_artifact_index_evidence
			(document_id,rendition_key,position,provider,evidence_id,sha256) VALUES (?,?,?,?,?,?)`,
			record.DocumentID, record.RenditionKey, record.Position, record.Provider, record.EvidenceID, record.SHA256); err != nil {
			return err
		}
	}
	for _, record := range lyrics.SourceContributions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_component_contributions
			(document_id,component,rendition_key,contribution_sha256) VALUES (?,?,?,?)`, record.DocumentID,
			record.Component, record.RenditionKey, record.ContributionSHA256); err != nil {
			return err
		}
	}
	if err := restoreLyricsRecoveryContentTx(ctx, tx, lyrics); err != nil {
		return err
	}
	for _, record := range lyrics.Lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_lines(music_id, line_id, position, japanese, zh_cn, en_us, stanza_break_before)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, record.MusicID, record.LineID, record.Position, record.Japanese,
			record.Chinese, record.English, record.StanzaBreakBefore); err != nil {
			return err
		}
	}
	for _, record := range lyrics.Segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		rubyJSON := record.RubyJSON
		if rubyJSON == "" {
			encoded, _ := json.Marshal([]model.LyricRubySpan{{Text: record.Text}})
			rubyJSON = string(encoded)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyric_segments(music_id, line_id, position, text, performer_ids_json, ruby_json)
			VALUES (?, ?, ?, ?, ?, ?)`, record.MusicID, record.LineID, record.Position, record.Text, record.PerformerIDsJSON, rubyJSON); err != nil {
			return err
		}
	}
	for _, record := range lyrics.Publications {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_publications(music_id, revision, updated_at, payload_json)
			VALUES (?, ?, ?, ?)`, record.MusicID, record.Revision, record.UpdatedAt, record.PayloadJSON); err != nil {
			return err
		}
	}
	for _, record := range lyrics.PublicWithdrawals {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_public_withdrawals(music_id, withdrawn_at, withdrawn_by)
			VALUES (?, ?, ?)`, record.MusicID, record.WithdrawnAt, record.WithdrawnBy); err != nil {
			return err
		}
	}
	if err := supersedeStalePendingLyricsSourceReviewsTx(ctx, tx, time.Now().UTC()); err != nil {
		return err
	}
	return restoreLyricsSourceDocumentDeleteGuardsTx(ctx, tx)
}

func suspendLyricsSourceDocumentDeleteGuardsTx(ctx context.Context, tx *sql.Tx) error {
	for _, name := range []string{"song_lyrics_source_v3_reject_delete", "song_lyrics_source_documents_immutable_delete"} {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name); err != nil {
			return fmt.Errorf("suspend lyrics source document delete guard %s: %w", name, err)
		}
	}
	return nil
}

// suspendEmbeddedLyricsEditorSeedDeleteGuardsTx lifts the ledger immutability
// guards for the restore transaction. The two guards reference each other
// (items refuse to go while their batch exists and vice versa), so a catalog
// replacement cannot clear the ledger any other way.
func suspendEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx context.Context, tx *sql.Tx) error {
	for _, name := range []string{
		"embedded_lyrics_editor_seed_items_immutable_delete",
		"embedded_lyrics_editor_seed_batches_immutable_delete",
	} {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name); err != nil {
			return fmt.Errorf("suspend embedded lyrics editor seed delete guard %s: %w", name, err)
		}
	}
	return nil
}

func restoreEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		// Keep these the migration v28 definitions.
		`CREATE TRIGGER embedded_lyrics_editor_seed_items_immutable_delete
		 BEFORE DELETE ON embedded_lyrics_editor_seed_items
		 WHEN EXISTS (SELECT 1 FROM embedded_lyrics_editor_seed_batches WHERE seed_sha256=OLD.seed_sha256)
		 BEGIN SELECT RAISE(ABORT, 'embedded lyrics editor seed items are immutable'); END`,
		`CREATE TRIGGER embedded_lyrics_editor_seed_batches_immutable_delete
		 BEFORE DELETE ON embedded_lyrics_editor_seed_batches
		 WHEN EXISTS (SELECT 1 FROM embedded_lyrics_editor_seed_items WHERE seed_sha256=OLD.seed_sha256)
		 BEGIN SELECT RAISE(ABORT, 'embedded lyrics editor seed batches are immutable while items exist'); END`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("restore embedded lyrics editor seed delete guard: %w", err)
		}
	}
	return nil
}

func restoreLyricsSourceDocumentDeleteGuardsTx(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		// Keep this the migration v27 definition; the v25 text it was copied from
		// lacked the rendition-localization clause.
		`CREATE TRIGGER song_lyrics_source_documents_immutable_delete BEFORE DELETE ON song_lyrics_source_documents
		 WHEN EXISTS (SELECT 1 FROM song_lyrics WHERE music_id=OLD.music_id) OR
		      EXISTS (SELECT 1 FROM song_lyrics_rendition_localizations WHERE document_id=OLD.document_id)
		 BEGIN SELECT RAISE(ABORT, 'song lyrics source documents are immutable'); END`,
		`CREATE TRIGGER song_lyrics_source_v3_reject_delete BEFORE DELETE ON song_lyrics_source_documents
		 WHEN OLD.schema_version=3
		 BEGIN SELECT RAISE(ABORT, 'source v3 documents are immutable'); END`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("restore lyrics source document delete guard: %w", err)
		}
	}
	return nil
}

func canonicalizeCatalogMusicBackupRecord(record *CatalogMusicBackupRecord) error {
	if record == nil {
		return errors.New("catalog record is required")
	}
	if record.LyricsCatalogFingerprint == "" {
		parts := strings.Split(record.ProducerMetadata, "|")
		credits := make([]string, 0, 3)
		for _, part := range parts {
			if value := strings.TrimSpace(part); value != "" && value != "-" {
				credits = append(credits, value)
			}
		}
		if record.Lyricist == "" && len(credits) > 0 {
			record.Lyricist = credits[0]
		}
		if record.Composer == "" && len(credits) > 1 {
			record.Composer = credits[1]
		}
		if record.Arranger == "" && len(credits) > 2 {
			record.Arranger = credits[2]
		}
		if record.LyricsVersion == "" {
			record.LyricsVersion = "unknown"
		}
		record.LyricsEvidencePresence.Lyricist = record.Lyricist != ""
		record.LyricsEvidencePresence.Composer = record.Composer != ""
		record.LyricsEvidencePresence.Arranger = record.Arranger != ""
		record.LyricsEvidencePresence.Assetbundle = record.AssetbundleName != ""
		record.LyricsEvidencePresence.VersionHint = record.VersionHint != ""
		record.LyricsCatalogPolicyVersion = model.LyricsCatalogIdentityPolicyVersion
	}
	if record.Vocals == nil {
		record.Vocals = []model.CatalogVocalSignal{}
	}
	evidence := model.CatalogLyricsEvidence{Title: record.TitleJA, Lyricist: record.Lyricist, Composer: record.Composer,
		Arranger: record.Arranger, Assetbundle: record.AssetbundleName, VersionHint: record.VersionHint,
		LyricsVersion: record.LyricsVersion, Vocals: record.Vocals, Presence: record.LyricsEvidencePresence}
	fingerprint, err := model.CatalogLyricsEvidenceFingerprint(evidence)
	if err != nil {
		return err
	}
	if record.LyricsCatalogFingerprint != "" && record.LyricsCatalogFingerprint != fingerprint {
		return errors.New("catalog fingerprint does not match evidence")
	}
	record.LyricsCatalogFingerprint = fingerprint
	record.LyricsCatalogPolicyVersion = model.LyricsCatalogIdentityPolicyVersion
	record.LyricsVersion = model.NormalizeCatalogLyricsEvidence(evidence).LyricsVersion
	return nil
}

func supersedeStalePendingLyricsSourceReviewsTx(ctx context.Context, tx *sql.Tx, completedAt time.Time) error {
	completedAt = canonicalLyricsDiscoveryTime(completedAt)
	_, err := tx.ExecContext(ctx, `UPDATE lyrics_source_review_items
		SET state='superseded', version=version+1,
			updated_at=CASE WHEN created_at>? THEN created_at ELSE ? END,
			completed_at=CASE WHEN created_at>? THEN created_at ELSE ? END
		WHERE state='pending' AND NOT EXISTS (
			SELECT 1 FROM catalog_music c WHERE c.music_id=lyrics_source_review_items.music_id
			AND c.lyrics_catalog_fingerprint=lyrics_source_review_items.catalog_fingerprint
		)`, completedAt.UnixMilli(), completedAt.UnixMilli(), completedAt.UnixMilli(), completedAt.UnixMilli())
	return err
}

func validateEventContentCanonicalCoverage(events EventContentExport) error {
	type scenarioIdentity struct {
		eventID    int
		episodeNo  string
		scenarioID string
	}
	segmentsByID := make(map[string]EventSegmentRecord, len(events.Segments))
	for _, segment := range events.Segments {
		if _, exists := segmentsByID[segment.SegmentID]; exists {
			return fmt.Errorf("event segment %s appears more than once", segment.SegmentID)
		}
		segmentsByID[segment.SegmentID] = segment
	}
	expectedByScenario := make(map[scenarioIdentity]map[string]bool, len(events.Scenarios))
	for _, scenario := range events.Scenarios {
		identity := scenarioIdentity{eventID: scenario.EventID, episodeNo: scenario.EpisodeNo, scenarioID: scenario.ScenarioID}
		if _, exists := expectedByScenario[identity]; exists {
			return fmt.Errorf("event scenario %d/%s appears more than once", scenario.EventID, scenario.EpisodeNo)
		}
		definitions, err := eventScenarioSegmentDefinitions(scenario, "")
		if err != nil {
			return err
		}
		expected := make(map[string]bool, len(definitions))
		for _, definition := range definitions {
			segment, exists := segmentsByID[definition.SegmentID]
			if !exists {
				return fmt.Errorf("event scenario %d/%s is missing canonical %s position %d",
					scenario.EventID, scenario.EpisodeNo, definition.Kind, definition.Position)
			}
			if definition.Kind == "title" {
				definition.SourceText = segment.SourceText
				definition.SourceHash = hashText(segment.SourceText)
			}
			if !eventSegmentMatchesDefinition(segment, definition) {
				return fmt.Errorf("event segment %s does not match its canonical definition", segment.SegmentID)
			}
			expected[definition.SegmentID] = true
		}
		expectedByScenario[identity] = expected
	}
	for _, segment := range events.Segments {
		identity := scenarioIdentity{eventID: segment.EventID, episodeNo: segment.EpisodeNo, scenarioID: segment.ScenarioID}
		if expected := expectedByScenario[identity]; expected != nil && !expected[segment.SegmentID] {
			return fmt.Errorf("event segment %s is non-canonical recovery data", segment.SegmentID)
		}
	}
	for _, localization := range events.Localizations {
		if _, exists := segmentsByID[localization.SegmentID]; !exists {
			return fmt.Errorf("event localization references missing segment %s", localization.SegmentID)
		}
	}
	return nil
}

type sourceEvidenceIdentity struct {
	provider   model.LyricsSourceProvider
	evidenceID string
}

type sourceArtifactIdentity struct {
	documentID   int64
	renditionKey string
}

// repairMissingExportArtifactEvidenceLinks completes evidence links that are
// derivable from each artifact's own fixed identity. It runs inside the
// read-only export transaction and therefore only repairs the exported result.
func repairMissingExportArtifactEvidenceLinks(ctx context.Context, tx *sql.Tx, lyrics *LyricsContentExport) error {
	existingLinks := make(map[sourceArtifactIdentity]map[int]bool)
	for _, record := range lyrics.SourceArtifactEvidence {
		id := sourceArtifactIdentity{documentID: record.DocumentID, renditionKey: record.RenditionKey}
		if existingLinks[id] == nil {
			existingLinks[id] = make(map[int]bool)
		}
		existingLinks[id][record.Position] = true
	}
	loadedEvidence := make(map[sourceEvidenceIdentity]bool)
	for _, ev := range lyrics.SourceIndexEvidence {
		loadedEvidence[sourceEvidenceIdentity{provider: model.LyricsSourceProvider(ev.Provider), evidenceID: ev.EvidenceID}] = true
	}
	for _, artifact := range lyrics.SourceArtifacts {
		identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(artifact.FixedIdentityJSON))
		if err != nil {
			continue
		}
		id := sourceArtifactIdentity{documentID: artifact.DocumentID, renditionKey: artifact.RenditionKey}
		for position, ref := range identity.IndexEvidenceRefs {
			if existingLinks[id] != nil && existingLinks[id][position] {
				continue
			}
			linkRecord := LyricsSourceArtifactEvidenceBackupRecord{
				DocumentID:   artifact.DocumentID,
				RenditionKey: artifact.RenditionKey,
				Position:     position,
				Provider:     string(identity.Provider),
				EvidenceID:   ref.EvidenceID,
				SHA256:       ref.SHA256,
			}
			lyrics.SourceArtifactEvidence = append(lyrics.SourceArtifactEvidence, linkRecord)
			if existingLinks[id] == nil {
				existingLinks[id] = make(map[int]bool)
			}
			existingLinks[id][position] = true

			evID := sourceEvidenceIdentity{provider: identity.Provider, evidenceID: ref.EvidenceID}
			if !loadedEvidence[evID] && tx != nil {
				var record LyricsSourceIndexEvidenceBackupRecord
				var pageID, revisionID sql.NullInt64
				err := tx.QueryRowContext(ctx, `SELECT provider,evidence_id,sha256,kind,origin,page_id,revision_id,
					revision_timestamp,mediawiki_sha1,page_title,canonical_revision_url,categories_json,
					canonical_request_url,fetched_at,raw_bytes,raw_byte_count,raw_sha256,created_at
					FROM lyrics_source_index_evidence
					WHERE provider=? AND evidence_id=? AND sha256=?`,
					linkRecord.Provider, linkRecord.EvidenceID, linkRecord.SHA256).Scan(
					&record.Provider, &record.EvidenceID, &record.SHA256, &record.Kind, &record.Origin,
					&pageID, &revisionID, &record.RevisionTimestamp, &record.MediaWikiSHA1, &record.PageTitle,
					&record.CanonicalRevisionURL, &record.CategoriesJSON, &record.CanonicalRequestURL, &record.FetchedAt,
					&record.RawBytes, &record.RawByteCount, &record.RawSHA256, &record.CreatedAt)
				if err == nil {
					if pageID.Valid {
						record.PageID = int(pageID.Int64)
					}
					if revisionID.Valid {
						record.RevisionID = int(revisionID.Int64)
					}
					record.RawBytes = append([]byte(nil), record.RawBytes...)
					lyrics.SourceIndexEvidence = append(lyrics.SourceIndexEvidence, record)
					loadedEvidence[evID] = true
				}
			}
		}
	}
	return nil
}

func validateRestoredLyricsSourceProvenance(lyrics LyricsContentExport, documentIDs map[int]bool) error {
	recoveryBatches := make(map[string]bool, len(lyrics.RecoveryBatches))
	for _, batch := range lyrics.RecoveryBatches {
		recoveryBatches[batch.BatchSHA256] = true
	}
	evidenceByIdentity := make(map[sourceEvidenceIdentity]lyricssource.IndexEvidence, len(lyrics.SourceIndexEvidence))
	evidenceCreatedAt := make(map[sourceEvidenceIdentity]int64, len(lyrics.SourceIndexEvidence))
	for _, record := range lyrics.SourceIndexEvidence {
		evidence, err := lyricsSourceIndexEvidenceFromBackupRecord(record)
		if err != nil {
			return err
		}
		identity := sourceEvidenceIdentity{provider: evidence.Provider, evidenceID: evidence.EvidenceID}
		if existing, duplicate := evidenceByIdentity[identity]; duplicate {
			if sameLyricsIndexEvidence(existing, evidence) && evidenceCreatedAt[identity] == record.CreatedAt {
				return fmt.Errorf("lyrics source index evidence %s/%s is duplicated", evidence.Provider, evidence.EvidenceID)
			}
			return fmt.Errorf("lyrics source index evidence %s/%s has conflicting immutable rows", evidence.Provider, evidence.EvidenceID)
		}
		evidenceByIdentity[identity] = evidence
		evidenceCreatedAt[identity] = record.CreatedAt
	}
	provenanceByID := make(map[int64]LyricsSourceDocumentBackupRecord, len(lyrics.SourceDocuments))
	sourceRecordsByID := make(map[int64]LyricsSourceDocumentBackupRecord, len(lyrics.SourceDocuments))
	sourceDocumentsByID := make(map[int64]model.LyricsSourceDocument, len(lyrics.SourceDocuments))
	allSourceDocumentsByID := make(map[int64]model.LyricsSourceDocument, len(lyrics.SourceDocuments))
	provenanceByMusic := make(map[int]bool, len(lyrics.SourceDocuments))
	allDocumentIDs := make(map[int64]bool, len(lyrics.SourceDocuments))
	allDocumentMusic := make(map[int]bool, len(lyrics.SourceDocuments))
	for _, record := range lyrics.SourceDocuments {
		if record.DocumentID <= 0 || allDocumentMusic[record.MusicID] {
			return fmt.Errorf("lyrics source document %d has invalid or duplicate identity", record.MusicID)
		}
		if allDocumentIDs[record.DocumentID] {
			return fmt.Errorf("lyrics source document id %d is duplicated", record.DocumentID)
		}
		allDocumentIDs[record.DocumentID] = true
		allDocumentMusic[record.MusicID] = true
		document, err := model.DecodeLyricsSourceDocument([]byte(record.DocumentJSON))
		if err != nil || document.SchemaVersion != record.SchemaVersion || string(document.ReasonCode) != record.ReasonCode {
			return fmt.Errorf("lyrics source document %d is invalid", record.MusicID)
		}
		if document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 && documentIDs[record.MusicID] {
			return fmt.Errorf("lyrics source document %d has mixed source-v3 and legacy editable ownership", record.MusicID)
		}
		if !documentIDs[record.MusicID] && document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
			return fmt.Errorf("lyrics source document %d is invalid", record.MusicID)
		}
		canonicalDocumentJSON, err := json.Marshal(document)
		if err != nil || string(canonicalDocumentJSON) != record.DocumentJSON {
			return fmt.Errorf("lyrics source document %d JSON is not canonical", record.MusicID)
		}
		if err := validateStoreV3DocumentGraph(document); err != nil {
			return fmt.Errorf("lyrics source document %d v3 graph: %w", record.MusicID, err)
		}
		digest := sha256.Sum256([]byte(record.DocumentJSON))
		if hex.EncodeToString(digest[:]) != record.DocumentSHA256 ||
			!isCanonicalContentBackupSHA256(record.ManifestBatchSHA256) || record.CreatedAt <= 0 {
			return fmt.Errorf("lyrics source document %d checksum is invalid", record.MusicID)
		}
		allSourceDocumentsByID[record.DocumentID] = document
		sourceRecordsByID[record.DocumentID] = record
		if recoveryBatches[record.ManifestBatchSHA256] {
			continue
		}
		provenanceByID[record.DocumentID] = record
		sourceDocumentsByID[record.DocumentID] = document
		provenanceByMusic[record.MusicID] = true
	}
	if err := validateRenditionLocalizationBackup(lyrics, sourceRecordsByID, allSourceDocumentsByID); err != nil {
		return err
	}
	if err := validateTranslationEditionBackup(lyrics, allSourceDocumentsByID); err != nil {
		return err
	}
	artifactsByDocument := make(map[int64]map[string]bool, len(provenanceByID))
	artifactIdentities := make(map[sourceArtifactIdentity]model.LyricsSourceFixedIdentity, len(lyrics.SourceArtifacts))
	artifactEvidenceRefs := make(map[sourceArtifactIdentity][]model.LyricsSourceIndexEvidenceRef, len(lyrics.SourceArtifacts))
	artifactIndexByDocument := make(map[int64]int)
	for _, record := range lyrics.SourceArtifacts {
		if _, exists := provenanceByID[record.DocumentID]; !exists {
			return fmt.Errorf("lyrics source artifact references unknown document %d", record.DocumentID)
		}
		if document := sourceDocumentsByID[record.DocumentID]; document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			index := artifactIndexByDocument[record.DocumentID]
			if index >= len(document.FixedIdentities) || record.RenditionKey != document.FixedIdentities[index].RenditionKey {
				return fmt.Errorf("lyrics source artifacts for document %d are not in canonical fixed-identity order", record.DocumentID)
			}
			artifactIndexByDocument[record.DocumentID] = index + 1
		}
		identity, err := model.DecodeLyricsSourceFixedIdentity([]byte(record.FixedIdentityJSON))
		if err != nil || string(identity.Provider) != record.Provider || identity.RenditionKey != record.RenditionKey ||
			identity.Origin != record.Origin || identity.PageID != record.PageID || identity.RevisionID != record.RevisionID ||
			identity.RevisionTimestamp != record.RevisionTimestamp || identity.SHA1 != record.MediaWikiSHA1 ||
			identity.Title != record.PageTitle || identity.CanonicalURL != record.CanonicalRevisionURL ||
			identity.Section != record.Section || identity.FetchedAt != record.FetchedAt ||
			identity.CompositionRenditionKey != record.CompositionRenditionKey ||
			string(identity.VersionReason) != record.VersionReason {
			return fmt.Errorf("lyrics source artifact %d/%s fixed identity is invalid", record.DocumentID, record.RenditionKey)
		}
		var expectedIdentity model.LyricsSourceFixedIdentity
		expectedIdentityFound := false
		for _, candidate := range sourceDocumentsByID[record.DocumentID].FixedIdentities {
			if candidate.RenditionKey == record.RenditionKey {
				expectedIdentity = candidate
				expectedIdentityFound = true
				break
			}
		}
		canonicalIdentityJSON, identityErr := json.Marshal(identity)
		canonicalExpectedIdentityJSON, expectedIdentityErr := json.Marshal(expectedIdentity)
		canonicalCategoriesJSON, categoriesErr := json.Marshal(identity.Categories)
		canonicalEvidenceRefsJSON, evidenceRefsErr := json.Marshal(identity.IndexEvidenceRefs)
		if !expectedIdentityFound || identityErr != nil || expectedIdentityErr != nil || categoriesErr != nil || evidenceRefsErr != nil ||
			string(canonicalIdentityJSON) != record.FixedIdentityJSON ||
			string(canonicalExpectedIdentityJSON) != record.FixedIdentityJSON ||
			string(canonicalCategoriesJSON) != record.CategoriesJSON ||
			string(canonicalEvidenceRefsJSON) != record.IndexEvidenceRefsJSON {
			return fmt.Errorf("lyrics source artifact %d/%s durable identity fields are not canonical", record.DocumentID, record.RenditionKey)
		}
		digest := sha256.Sum256([]byte(record.FixedIdentityJSON))
		if hex.EncodeToString(digest[:]) != record.FixedIdentitySHA256 || record.RawByteCount <= 0 ||
			record.RawByteCount > maxLyricsDiscoveryRawEvidenceBytes ||
			!isCanonicalContentBackupSHA256(record.RawWikitextSHA256) ||
			!isCanonicalContentBackupSHA256(record.ArtifactSHA256) {
			return fmt.Errorf("lyrics source artifact %d/%s checksum is invalid", record.DocumentID, record.RenditionKey)
		}
		if artifactsByDocument[record.DocumentID] == nil {
			artifactsByDocument[record.DocumentID] = map[string]bool{}
		}
		if artifactsByDocument[record.DocumentID][record.RenditionKey] {
			return fmt.Errorf("lyrics source artifact %d/%s is duplicated", record.DocumentID, record.RenditionKey)
		}
		artifactsByDocument[record.DocumentID][record.RenditionKey] = true
		artifactIdentity := sourceArtifactIdentity{documentID: record.DocumentID, renditionKey: record.RenditionKey}
		artifactIdentities[artifactIdentity] = identity
		artifactEvidenceRefs[artifactIdentity] = append([]model.LyricsSourceIndexEvidenceRef{}, identity.IndexEvidenceRefs...)
	}
	artifactEvidencePositions := make(map[sourceArtifactIdentity]map[int]bool, len(artifactEvidenceRefs))
	referencedEvidence := make(map[sourceEvidenceIdentity]bool, len(evidenceByIdentity))
	for _, record := range lyrics.SourceArtifactEvidence {
		artifactIdentity := sourceArtifactIdentity{documentID: record.DocumentID, renditionKey: record.RenditionKey}
		references, exists := artifactEvidenceRefs[artifactIdentity]
		identity, identityExists := artifactIdentities[artifactIdentity]
		if !exists || !identityExists || record.Position < 0 || record.Position >= len(references) ||
			record.Provider == "" || references[record.Position].EvidenceID != record.EvidenceID ||
			references[record.Position].SHA256 != record.SHA256 {
			return fmt.Errorf("lyrics source artifact evidence %d/%s/%d is invalid", record.DocumentID, record.RenditionKey, record.Position)
		}
		if artifactEvidencePositions[artifactIdentity] == nil {
			artifactEvidencePositions[artifactIdentity] = map[int]bool{}
		}
		if artifactEvidencePositions[artifactIdentity][record.Position] {
			return fmt.Errorf("lyrics source artifact evidence %d/%s/%d is duplicated", record.DocumentID, record.RenditionKey, record.Position)
		}
		if string(identity.Provider) != record.Provider {
			return fmt.Errorf("lyrics source artifact evidence %d/%s provider is invalid", record.DocumentID, record.RenditionKey)
		}
		evidenceIdentity := sourceEvidenceIdentity{provider: identity.Provider, evidenceID: record.EvidenceID}
		parent, exists := evidenceByIdentity[evidenceIdentity]
		if !exists || parent.SHA256 != record.SHA256 || parent.RawSHA256 != record.SHA256 {
			return fmt.Errorf("lyrics source artifact evidence %d/%s/%d has no exact parent evidence", record.DocumentID, record.RenditionKey, record.Position)
		}
		artifactEvidencePositions[artifactIdentity][record.Position] = true
		referencedEvidence[evidenceIdentity] = true
	}
	for artifactIdentity, references := range artifactEvidenceRefs {
		if len(artifactEvidencePositions[artifactIdentity]) != len(references) {
			return fmt.Errorf("lyrics source artifact %d/%s has incomplete evidence links", artifactIdentity.documentID, artifactIdentity.renditionKey)
		}
	}
	if len(referencedEvidence) != len(evidenceByIdentity) {
		return errors.New("lyrics source backup contains orphan parent evidence")
	}
	artifactOrder := make([]sourceArtifactIdentity, 0, len(artifactIdentities))
	for artifactIdentity := range artifactIdentities {
		artifactOrder = append(artifactOrder, artifactIdentity)
	}
	sort.Slice(artifactOrder, func(left, right int) bool {
		if artifactOrder[left].documentID != artifactOrder[right].documentID {
			return artifactOrder[left].documentID < artifactOrder[right].documentID
		}
		return artifactOrder[left].renditionKey < artifactOrder[right].renditionKey
	})
	hydratedCandidates := make([]lyricssource.Candidate, 0, len(artifactIdentities))
	for _, artifactIdentity := range artifactOrder {
		identity := artifactIdentities[artifactIdentity]
		versionReason := identity.VersionReason
		if versionReason == "" {
			versionReason = model.LyricsSourceVersionReasonCode(provenanceByID[artifactIdentity.documentID].ReasonCode)
		}
		candidate := lyricssource.Candidate{
			Provider: identity.Provider, Origin: identity.Origin, PageID: identity.PageID, RevisionID: identity.RevisionID,
			RevisionTimestamp: identity.RevisionTimestamp,
			SHA1:              identity.SHA1, Title: identity.Title, CanonicalURL: identity.CanonicalURL,
			FetchedAt:  identity.FetchedAt,
			Categories: append([]string{}, identity.Categories...), Section: identity.Section,
			RenditionKey: model.LyricsSourceCompositionRenditionKey(identity), VersionReason: versionReason,
			IndexEvidenceRefs: append([]model.LyricsSourceIndexEvidenceRef{}, identity.IndexEvidenceRefs...),
			IndexEvidence:     make([]lyricssource.IndexEvidence, len(identity.IndexEvidenceRefs)),
		}
		for position, reference := range identity.IndexEvidenceRefs {
			parent, exists := evidenceByIdentity[sourceEvidenceIdentity{provider: identity.Provider, evidenceID: reference.EvidenceID}]
			if !exists || parent.SHA256 != reference.SHA256 {
				return fmt.Errorf("lyrics source artifact %d/%s evidence reference is unresolved", artifactIdentity.documentID, artifactIdentity.renditionKey)
			}
			candidate.IndexEvidence[position] = cloneLyricsIndexEvidence(parent)
		}
		hydratedCandidates = append(hydratedCandidates, candidate)
	}
	if len(hydratedCandidates) > 0 {
		if err := validatePersistedBackupEvidenceCandidates(hydratedCandidates); err != nil {
			return fmt.Errorf("lyrics source parent evidence is invalid: %w", err)
		}
	}
	contributions := make(map[int64]map[string]string, len(provenanceByID))
	expectedContributions := make(map[int64]map[string]string, len(provenanceByID))
	lastContributionByDocument := make(map[int64]string)
	for documentID, document := range sourceDocumentsByID {
		expectedContributions[documentID] = LyricsSourceComponentRefs(document)
	}
	for _, record := range lyrics.SourceContributions {
		document, exists := provenanceByID[record.DocumentID]
		if exists && document.SchemaVersion == model.LyricsSourceDocumentSchemaVersionV3 {
			if previous := lastContributionByDocument[record.DocumentID]; previous != "" && record.Component <= previous {
				return fmt.Errorf("lyrics source contributions for document %d are not in canonical component order", record.DocumentID)
			}
			lastContributionByDocument[record.DocumentID] = record.Component
		}
		expectedRenditionKey, expectedComponent := expectedContributions[record.DocumentID][record.Component]
		if !exists || !expectedComponent || expectedRenditionKey != record.RenditionKey ||
			!artifactsByDocument[record.DocumentID][record.RenditionKey] {
			return fmt.Errorf("lyrics source contribution %d/%s does not match the canonical component graph", record.DocumentID, record.Component)
		}
		if contributions[record.DocumentID] == nil {
			contributions[record.DocumentID] = map[string]string{}
		}
		if _, duplicate := contributions[record.DocumentID][record.Component]; duplicate {
			return fmt.Errorf("lyrics source contribution %d/%s is duplicated", record.DocumentID, record.Component)
		}
		digest := sha256.Sum256([]byte(document.DocumentSHA256 + "\x00" + record.Component + "\x00" + record.RenditionKey))
		if record.ContributionSHA256 != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("lyrics source contribution %d/%s checksum is invalid", record.DocumentID, record.Component)
		}
		contributions[record.DocumentID][record.Component] = record.RenditionKey
	}
	for documentID, documentRecord := range provenanceByID {
		var document model.LyricsSourceDocument
		if err := json.Unmarshal([]byte(documentRecord.DocumentJSON), &document); err != nil {
			return err
		}
		if len(artifactsByDocument[documentID]) != len(document.FixedIdentities) ||
			len(contributions[documentID]) != len(expectedContributions[documentID]) {
			return fmt.Errorf("lyrics source document %d has incomplete provenance", documentRecord.MusicID)
		}
	}
	return nil
}

func restoredPublicLyricsSourceBundles(lyrics LyricsContentExport) (map[int]*publicLyricsSourceBundle, error) {
	byDocumentID := make(map[int64]*publicLyricsSourceBundle, len(lyrics.SourceDocuments))
	byMusicID := make(map[int]*publicLyricsSourceBundle, len(lyrics.SourceDocuments))
	for _, record := range lyrics.SourceDocuments {
		document, err := model.DecodeLyricsSourceDocument([]byte(record.DocumentJSON))
		if err != nil {
			return nil, fmt.Errorf("lyrics source document %d: %w", record.MusicID, err)
		}
		bundle := &publicLyricsSourceBundle{
			documentID: record.DocumentID, documentJSON: record.DocumentJSON, documentSHA: record.DocumentSHA256,
			document: document, contributions: map[string]string{},
		}
		byDocumentID[record.DocumentID] = bundle
		byMusicID[record.MusicID] = bundle
	}
	for _, record := range lyrics.SourceContributions {
		bundle := byDocumentID[record.DocumentID]
		if bundle == nil {
			return nil, fmt.Errorf("lyrics source contribution references unknown document %d", record.DocumentID)
		}
		bundle.contributions[record.Component] = record.RenditionKey
	}
	return byMusicID, nil
}

func renditionTranslationSide(document model.LyricsSourceDocument, renditionKey, side string) (*model.LyricsSourceFull, bool) {
	for index := range document.Renditions {
		rendition := &document.Renditions[index]
		if rendition.RenditionKey != renditionKey {
			continue
		}
		switch side {
		case "full":
			return rendition.Full, rendition.Full != nil
		case "game":
			return rendition.Game, rendition.Game != nil
		default:
			return nil, false
		}
	}
	return nil, false
}

func renditionPrimaryTranslationSide(document model.LyricsSourceDocument, renditionKey string) string {
	if _, found := renditionTranslationSide(document, renditionKey, "full"); found {
		return "full"
	}
	if _, found := renditionTranslationSide(document, renditionKey, "game"); found {
		return "game"
	}
	return ""
}

func renditionPeerTranslationSide(document model.LyricsSourceDocument, renditionKey, side string) (*model.LyricsSourceFull, bool) {
	for index := range document.Renditions {
		rendition := &document.Renditions[index]
		if rendition.RenditionKey != renditionKey || side == renditionPrimaryTranslationSide(document, renditionKey) ||
			rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
			continue
		}
		if side == "full" && rendition.Full != nil {
			return rendition.Full, true
		}
		if side == "game" && rendition.Game != nil {
			return rendition.Game, true
		}
	}
	return nil, false
}

func canonicalBackupTranslationSide(document model.LyricsSourceDocument, renditionKey, side string) (string, error) {
	primary := renditionPrimaryTranslationSide(document, renditionKey)
	if primary == "" {
		return "", fmt.Errorf("lyrics rendition %q has no authoritative side", renditionKey)
	}
	if side == "" {
		return primary, nil
	}
	if side != "full" && side != "game" {
		return "", fmt.Errorf("lyrics rendition %q translation side %q is invalid", renditionKey, side)
	}
	if side == primary {
		return "", fmt.Errorf("lyrics rendition %q primary translation side %q must use the historical representation", renditionKey, side)
	}
	if _, found := renditionPeerTranslationSide(document, renditionKey, side); !found {
		return "", fmt.Errorf("lyrics rendition %q has no independently persisted %s peer side", renditionKey, side)
	}
	return side, nil
}

func renditionLocalizationBackupDigest(
	lyrics LyricsContentExport,
	documentID int64,
	document model.LyricsSourceDocument,
) (string, error) {
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return "", nil
	}
	parents := make(map[string]LyricsRenditionLocalizationBackupRecord, len(document.Renditions))
	type sideKey struct {
		renditionKey string
		side         string
	}
	lines := make(map[sideKey]map[int]string, len(document.Renditions))
	for _, record := range lyrics.RenditionLocalizations {
		if record.DocumentID != documentID || record.Locale != "zh-CN" {
			continue
		}
		if _, duplicate := parents[record.RenditionKey]; duplicate {
			return "", fmt.Errorf("lyrics rendition localization %d/%s is duplicated", documentID, record.RenditionKey)
		}
		parents[record.RenditionKey] = record
	}
	for _, record := range lyrics.RenditionTranslationLines {
		if record.DocumentID != documentID || record.Locale != "zh-CN" {
			continue
		}
		side, err := canonicalBackupTranslationSide(document, record.RenditionKey, record.Side)
		if err != nil {
			return "", err
		}
		key := sideKey{renditionKey: record.RenditionKey, side: side}
		if lines[key] == nil {
			lines[key] = map[int]string{}
		}
		if _, duplicate := lines[key][record.Position]; duplicate {
			return "", fmt.Errorf("lyrics rendition translation line %d/%s/%s/%d is duplicated", documentID, record.RenditionKey, side, record.Position)
		}
		lines[key][record.Position] = record.Text
	}
	var translations []lyricsstaging.RenditionTranslation
	if len(parents) > 0 {
		translations = make([]lyricsstaging.RenditionTranslation, len(document.Renditions))
		for index, rendition := range document.Renditions {
			parent, found := parents[rendition.RenditionKey]
			if !found {
				return "", fmt.Errorf("lyrics rendition localization %d/%s is incomplete", documentID, rendition.RenditionKey)
			}
			item := lyricsstaging.RenditionTranslation{
				RenditionKey:       rendition.RenditionKey,
				TranslationCredit:  parent.TranslationCredit,
				ProofreadingCredit: parent.ProofreadingCredit,
			}
			primarySide := renditionPrimaryTranslationSide(document, rendition.RenditionKey)
			lineMap := lines[sideKey{renditionKey: rendition.RenditionKey, side: primarySide}]
			if len(lineMap) > 0 {
				item.Translations = make([]string, len(lineMap))
				for position := range item.Translations {
					text, exists := lineMap[position]
					if !exists {
						return "", fmt.Errorf("lyrics rendition translation lines %d/%s are not contiguous", documentID, rendition.RenditionKey)
					}
					item.Translations[position] = text
				}
			}
			translations[index] = item
		}
	}
	peerLines := make([]LyricsRenditionTranslationLineBackupRecord, 0)
	for key, positions := range lines {
		if key.side == renditionPrimaryTranslationSide(document, key.renditionKey) {
			continue
		}
		for position := 0; position < len(positions); position++ {
			text, found := positions[position]
			if !found {
				return "", fmt.Errorf("lyrics rendition translation lines %d/%s/%s are not contiguous", documentID, key.renditionKey, key.side)
			}
			peerLines = append(peerLines, LyricsRenditionTranslationLineBackupRecord{
				DocumentID: documentID, RenditionKey: key.renditionKey, Side: key.side,
				Locale: "zh-CN", Position: position, Text: text,
			})
		}
	}
	sort.Slice(peerLines, func(left, right int) bool {
		if peerLines[left].RenditionKey != peerLines[right].RenditionKey {
			return peerLines[left].RenditionKey < peerLines[right].RenditionKey
		}
		if peerLines[left].Side != peerLines[right].Side {
			return peerLines[left].Side < peerLines[right].Side
		}
		return peerLines[left].Position < peerLines[right].Position
	})
	var body []byte
	var err error
	if len(peerLines) == 0 {
		// Preserve the historical checksum for backups that only contain the
		// primary rendition side.
		body, err = json.Marshal(translations)
	} else {
		body, err = json.Marshal(struct {
			Version      int                                          `json:"version"`
			Translations []lyricsstaging.RenditionTranslation         `json:"translations"`
			PeerLines    []LyricsRenditionTranslationLineBackupRecord `json:"peerLines"`
		}{Version: 2, Translations: translations, PeerLines: peerLines})
	}
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func bindRenditionLocalizationBackupDigests(lyrics *LyricsContentExport) error {
	if lyrics == nil {
		return errors.New("lyrics content export is nil")
	}
	for index := range lyrics.SourceDocuments {
		record := &lyrics.SourceDocuments[index]
		document, err := model.DecodeLyricsSourceDocument([]byte(record.DocumentJSON))
		if err != nil {
			return err
		}
		record.RenditionLocalizationsSHA256, err = renditionLocalizationBackupDigest(*lyrics, record.DocumentID, document)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateRenditionLocalizationBackup(
	lyrics LyricsContentExport,
	sourceRecords map[int64]LyricsSourceDocumentBackupRecord,
	documents map[int64]model.LyricsSourceDocument,
) error {
	type localizationKey struct {
		documentID   int64
		renditionKey string
		side         string
		locale       string
	}
	parents := make(map[localizationKey]LyricsRenditionLocalizationBackupRecord, len(lyrics.RenditionLocalizations))
	parentCountByDocument := make(map[int64]int)
	type localizationMetadata struct {
		revision  int
		updatedAt int64
	}
	metadataByDocument := make(map[int64]localizationMetadata)
	var lastParentKey localizationKey
	haveParentKey := false
	for _, record := range lyrics.RenditionLocalizations {
		document, exists := documents[record.DocumentID]
		key := localizationKey{documentID: record.DocumentID, renditionKey: record.RenditionKey, locale: record.Locale}
		if !exists || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 ||
			record.RenditionKey == "" || record.Locale != "zh-CN" || record.TranslationCredit != strings.TrimSpace(record.TranslationCredit) ||
			record.ProofreadingCredit != strings.TrimSpace(record.ProofreadingCredit) || len(record.TranslationCredit) > 2048 ||
			len(record.ProofreadingCredit) > 2048 || !utf8.ValidString(record.TranslationCredit) ||
			!utf8.ValidString(record.ProofreadingCredit) || record.UpdatedAt <= 0 || record.Revision <= 0 {
			return fmt.Errorf("lyrics rendition localization %d/%s/%s is invalid", record.DocumentID, record.RenditionKey, record.Locale)
		}
		metadata := localizationMetadata{revision: record.Revision, updatedAt: record.UpdatedAt}
		if previous, found := metadataByDocument[record.DocumentID]; found && previous != metadata {
			return fmt.Errorf("lyrics rendition localizations for document %d have inconsistent revision metadata", record.DocumentID)
		}
		metadataByDocument[record.DocumentID] = metadata
		if _, duplicate := parents[key]; duplicate {
			return fmt.Errorf("lyrics rendition localization %d/%s/%s is duplicated", record.DocumentID, record.RenditionKey, record.Locale)
		}
		if haveParentKey && !lessRenditionLocalizationKey(lastParentKey, key) {
			return fmt.Errorf("lyrics rendition localizations are not canonically ordered at %d/%s/%s",
				record.DocumentID, record.RenditionKey, record.Locale)
		}
		lastParentKey, haveParentKey = key, true
		foundRendition := false
		for _, rendition := range document.Renditions {
			foundRendition = foundRendition || rendition.RenditionKey == record.RenditionKey
		}
		if !foundRendition {
			return fmt.Errorf("lyrics rendition localization %d/%s has no rendition", record.DocumentID, record.RenditionKey)
		}
		parents[key] = record
		parentCountByDocument[record.DocumentID]++
	}
	for documentID, count := range parentCountByDocument {
		if document := documents[documentID]; count != len(document.Renditions) {
			return fmt.Errorf("lyrics rendition localizations for document %d do not cover every rendition", documentID)
		}
	}
	positions := make(map[localizationKey]map[int]bool)
	var lastLineKey localizationKey
	lastLinePosition := -1
	haveLineKey := false
	for _, record := range lyrics.RenditionTranslationLines {
		document, found := documents[record.DocumentID]
		if !found {
			return fmt.Errorf("lyrics rendition translation line %d/%s/%s/%s/%d is invalid", record.DocumentID, record.RenditionKey, record.Side, record.Locale, record.Position)
		}
		side, sideErr := canonicalBackupTranslationSide(document, record.RenditionKey, record.Side)
		if sideErr != nil {
			return fmt.Errorf("lyrics rendition translation line %d/%s/%s/%s/%d: %w", record.DocumentID, record.RenditionKey, record.Side, record.Locale, record.Position, sideErr)
		}
		parentKey := localizationKey{documentID: record.DocumentID, renditionKey: record.RenditionKey, locale: record.Locale}
		_, parentFound := parents[parentKey]
		key := localizationKey{documentID: record.DocumentID, renditionKey: record.RenditionKey, side: side, locale: record.Locale}
		if record.Side != "" && record.Side != side || !parentFound || record.Position < 0 ||
			!utf8.ValidString(record.Text) || len(record.Text) > maxLyricsLineTextBytes {
			return fmt.Errorf("lyrics rendition translation line %d/%s/%s/%s/%d is invalid", record.DocumentID, record.RenditionKey, record.Side, record.Locale, record.Position)
		}
		if positions[key] == nil {
			positions[key] = map[int]bool{}
		}
		if positions[key][record.Position] {
			return fmt.Errorf("lyrics rendition translation line %d/%s/%s/%s/%d is duplicated", record.DocumentID, record.RenditionKey, side, record.Locale, record.Position)
		}
		if haveLineKey {
			if lessRenditionLocalizationKey(key, lastLineKey) || key == lastLineKey && record.Position <= lastLinePosition {
				return fmt.Errorf("lyrics rendition translation lines are not canonically ordered at %d/%s/%s/%s/%d",
					record.DocumentID, record.RenditionKey, side, record.Locale, record.Position)
			}
		}
		lastLineKey, lastLinePosition, haveLineKey = key, record.Position, true
		positions[key][record.Position] = true
	}
	for key, parent := range parents {
		document := documents[key.documentID]
		primarySide := renditionPrimaryTranslationSide(document, key.renditionKey)
		primaryKey := localizationKey{documentID: key.documentID, renditionKey: key.renditionKey, side: primarySide, locale: key.locale}
		primarySource, found := renditionTranslationSide(document, key.renditionKey, primarySide)
		if !found {
			return fmt.Errorf("lyrics rendition localization %d/%s has no primary side", key.documentID, key.renditionKey)
		}
		count := len(positions[primaryKey])
		if count != 0 && count != len(primarySource.Lines) {
			return fmt.Errorf("lyrics rendition localization %d/%s has incomplete translation lines for the primary side", key.documentID, key.renditionKey)
		}
		hasTranslationLines := count != 0
		if !hasTranslationLines {
			for positionKey := range positions {
				if positionKey.documentID == key.documentID && positionKey.renditionKey == key.renditionKey &&
					positionKey.locale == key.locale && positionKey.side != primarySide {
					hasTranslationLines = true
					break
				}
			}
		}
		if !hasTranslationLines && (parent.TranslationCredit != "" || parent.ProofreadingCredit != "") {
			return fmt.Errorf("lyrics rendition localization %d/%s has credits without translation lines", key.documentID, key.renditionKey)
		}
	}
	for key, present := range positions {
		source, found := renditionTranslationSide(documents[key.documentID], key.renditionKey, key.side)
		if !found || len(present) != len(source.Lines) {
			return fmt.Errorf("lyrics rendition localization %d/%s/%s has incomplete translation lines", key.documentID, key.renditionKey, key.side)
		}
		for position := 0; position < len(present); position++ {
			if !present[position] {
				return fmt.Errorf("lyrics rendition localization %d/%s/%s has a non-contiguous translation position", key.documentID, key.renditionKey, key.side)
			}
		}
	}
	for documentID, document := range documents {
		record, found := sourceRecords[documentID]
		if !found {
			return fmt.Errorf("lyrics source document %d has no backup record", documentID)
		}
		if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
			if record.RenditionLocalizationsSHA256 != "" {
				return fmt.Errorf("legacy lyrics source document %d has a rendition localization checksum", documentID)
			}
			continue
		}
		digest, err := renditionLocalizationBackupDigest(lyrics, documentID, document)
		if err != nil || !isCanonicalContentBackupSHA256(record.RenditionLocalizationsSHA256) ||
			digest != record.RenditionLocalizationsSHA256 {
			return fmt.Errorf("lyrics rendition localizations for document %d do not match their checksum", documentID)
		}
	}
	return nil
}

func validateTranslationEditionBackup(lyrics LyricsContentExport, documents map[int64]model.LyricsSourceDocument) error {
	type editionIdentity struct {
		documentID int64
		editionKey string
	}
	type localizationIdentity struct {
		documentID   int64
		editionKey   string
		renditionKey string
	}
	type sideIdentity struct {
		documentID   int64
		editionKey   string
		renditionKey string
		side         string
	}
	states := make(map[int64]LyricsTranslationEditionStateBackupRecord, len(lyrics.TranslationEditionStates))
	lastStateDocumentID := int64(0)
	for _, record := range lyrics.TranslationEditionStates {
		document, found := documents[record.DocumentID]
		if !found || document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 || record.DocumentID <= lastStateDocumentID ||
			!validLyricsTranslationEditionKey(record.DefaultEditionKey) || record.Revision <= 0 || record.UpdatedAt <= 0 ||
			len(record.UpdatedBy) > 128 || !utf8.ValidString(record.UpdatedBy) {
			return fmt.Errorf("lyrics translation edition state for document %d is invalid", record.DocumentID)
		}
		states[record.DocumentID] = record
		lastStateDocumentID = record.DocumentID
	}
	editions := make(map[int64]map[string]LyricsTranslationEditionBackupRecord)
	lastEditionKey := ""
	for _, record := range lyrics.TranslationEditions {
		identityKey := fmt.Sprintf("%020d\x00%s", record.DocumentID, record.EditionKey)
		if identityKey <= lastEditionKey || states[record.DocumentID].DocumentID == 0 ||
			!validLyricsTranslationEditionKey(record.EditionKey) || validateLyricsTranslationEditionLabel(record.Label) != nil ||
			record.CreatedAt <= 0 || len(record.CreatedBy) > 128 || !utf8.ValidString(record.CreatedBy) {
			return fmt.Errorf("lyrics translation edition %d/%s is invalid", record.DocumentID, record.EditionKey)
		}
		if editions[record.DocumentID] == nil {
			editions[record.DocumentID] = map[string]LyricsTranslationEditionBackupRecord{}
		}
		if _, duplicate := editions[record.DocumentID][record.EditionKey]; duplicate {
			return fmt.Errorf("lyrics translation edition %d/%s is duplicated", record.DocumentID, record.EditionKey)
		}
		editions[record.DocumentID][record.EditionKey] = record
		lastEditionKey = identityKey
	}
	parents := make(map[localizationIdentity]LyricsTranslationEditionLocalizationBackupRecord, len(lyrics.TranslationEditionLocalizations))
	lastLocalizationKey := ""
	for _, record := range lyrics.TranslationEditionLocalizations {
		identityKey := fmt.Sprintf("%020d\x00%s\x00%s\x00%s", record.DocumentID, record.EditionKey, record.RenditionKey, record.Locale)
		document, found := documents[record.DocumentID]
		_, editionFound := editions[record.DocumentID][record.EditionKey]
		if identityKey <= lastLocalizationKey || !found || !editionFound || record.Locale != "zh-CN" ||
			record.TranslationCredit != strings.TrimSpace(record.TranslationCredit) ||
			record.ProofreadingCredit != strings.TrimSpace(record.ProofreadingCredit) ||
			len(record.TranslationCredit) > 2048 || len(record.ProofreadingCredit) > 2048 ||
			!utf8.ValidString(record.TranslationCredit) || !utf8.ValidString(record.ProofreadingCredit) ||
			record.UpdatedAt <= 0 || len(record.UpdatedBy) > 128 || !utf8.ValidString(record.UpdatedBy) {
			return fmt.Errorf("lyrics translation edition localization %d/%s/%s is invalid", record.DocumentID, record.EditionKey, record.RenditionKey)
		}
		if _, editable := translationEditionEditableSide(document, record.RenditionKey, translationEditionEditableSidesForKey(document, record.RenditionKey)); !editable {
			return fmt.Errorf("lyrics translation edition localization %d/%s/%s has no source rendition", record.DocumentID, record.EditionKey, record.RenditionKey)
		}
		identity := localizationIdentity{documentID: record.DocumentID, editionKey: record.EditionKey, renditionKey: record.RenditionKey}
		if _, duplicate := parents[identity]; duplicate {
			return fmt.Errorf("lyrics translation edition localization %d/%s/%s is duplicated", record.DocumentID, record.EditionKey, record.RenditionKey)
		}
		parents[identity] = record
		lastLocalizationKey = identityKey
	}
	lines := make(map[sideIdentity][]string)
	lastLineKey := ""
	for _, record := range lyrics.TranslationEditionLines {
		identityKey := fmt.Sprintf("%020d\x00%s\x00%s\x00%s\x00%s\x00%020d", record.DocumentID, record.EditionKey,
			record.RenditionKey, record.Side, record.Locale, record.Position)
		document, found := documents[record.DocumentID]
		parent := localizationIdentity{documentID: record.DocumentID, editionKey: record.EditionKey, renditionKey: record.RenditionKey}
		source, editable := translationEditionEditableSide(document, record.RenditionKey, record.Side)
		key := sideIdentity{documentID: record.DocumentID, editionKey: record.EditionKey, renditionKey: record.RenditionKey, side: record.Side}
		if identityKey <= lastLineKey || !found || !editable || record.Locale != "zh-CN" || parents[parent].DocumentID == 0 ||
			record.Position != len(lines[key]) || record.Position >= len(source.Lines) || len(record.Text) > maxLyricsLineTextBytes || !utf8.ValidString(record.Text) {
			return fmt.Errorf("lyrics translation edition line %d/%s/%s/%s/%d is invalid", record.DocumentID, record.EditionKey,
				record.RenditionKey, record.Side, record.Position)
		}
		lines[key] = append(lines[key], record.Text)
		lastLineKey = identityKey
	}
	for documentID, state := range states {
		document := documents[documentID]
		set := editions[documentID]
		if len(set) == 0 || len(set) > maxLyricsTranslationEditions || set[state.DefaultEditionKey].DocumentID == 0 {
			return fmt.Errorf("lyrics translation edition set for document %d is invalid", documentID)
		}
		for editionKey := range set {
			for _, rendition := range document.Renditions {
				parentKey := localizationIdentity{documentID: documentID, editionKey: editionKey, renditionKey: rendition.RenditionKey}
				if parents[parentKey].DocumentID == 0 {
					return fmt.Errorf("lyrics translation edition %d/%s does not cover rendition %s", documentID, editionKey, rendition.RenditionKey)
				}
				for _, side := range translationEditionEditableSides(rendition) {
					source, _ := translationEditionEditableSide(document, rendition.RenditionKey, side)
					if len(lines[sideIdentity{documentID: documentID, editionKey: editionKey, renditionKey: rendition.RenditionKey, side: side}]) != len(source.Lines) {
						return fmt.Errorf("lyrics translation edition %d/%s does not cover %s/%s", documentID, editionKey, rendition.RenditionKey, side)
					}
				}
			}
		}
		legacyParents := make(map[string]LyricsRenditionLocalizationBackupRecord)
		for _, record := range lyrics.RenditionLocalizations {
			if record.DocumentID == documentID {
				legacyParents[record.RenditionKey] = record
			}
		}
		legacyLines := make(map[string]map[string][]string)
		for _, record := range lyrics.RenditionTranslationLines {
			if record.DocumentID != documentID {
				continue
			}
			side := record.Side
			if side == "" {
				side = renditionPrimaryTranslationSide(document, record.RenditionKey)
			}
			if legacyLines[record.RenditionKey] == nil {
				legacyLines[record.RenditionKey] = map[string][]string{}
			}
			values := legacyLines[record.RenditionKey][side]
			if record.Position != len(values) {
				return fmt.Errorf("lyrics default translation mirror %d/%s/%s is not contiguous", documentID, record.RenditionKey, side)
			}
			legacyLines[record.RenditionKey][side] = append(values, record.Text)
		}
		for _, rendition := range document.Renditions {
			legacy := legacyParents[rendition.RenditionKey]
			current := parents[localizationIdentity{documentID: documentID, editionKey: state.DefaultEditionKey, renditionKey: rendition.RenditionKey}]
			if legacy.DocumentID == 0 || legacy.Revision != state.Revision || legacy.UpdatedAt != state.UpdatedAt ||
				legacy.UpdatedBy != state.UpdatedBy || legacy.TranslationCredit != current.TranslationCredit ||
				legacy.ProofreadingCredit != current.ProofreadingCredit {
				return fmt.Errorf("lyrics default translation mirror metadata for document %d/%s does not match", documentID, rendition.RenditionKey)
			}
			for _, side := range translationEditionEditableSides(rendition) {
				editionValues := lines[sideIdentity{documentID: documentID, editionKey: state.DefaultEditionKey, renditionKey: rendition.RenditionKey, side: side}]
				legacyValues := legacyLines[rendition.RenditionKey][side]
				if len(editionValues) != len(legacyValues) {
					return fmt.Errorf("lyrics default translation mirror lines for document %d/%s/%s do not match", documentID, rendition.RenditionKey, side)
				}
				for index := range editionValues {
					if editionValues[index] != legacyValues[index] {
						return fmt.Errorf("lyrics default translation mirror line for document %d/%s/%s/%d does not match", documentID, rendition.RenditionKey, side, index)
					}
				}
			}
		}
	}
	if len(states) == 0 && (len(lyrics.TranslationEditions) != 0 || len(lyrics.TranslationEditionLocalizations) != 0 || len(lyrics.TranslationEditionLines) != 0) {
		return errors.New("lyrics translation edition rows exist without state")
	}
	return nil
}

func translationEditionEditableSidesForKey(document model.LyricsSourceDocument, renditionKey string) string {
	for _, rendition := range document.Renditions {
		if rendition.RenditionKey == renditionKey {
			sides := translationEditionEditableSides(rendition)
			if len(sides) != 0 {
				return sides[0]
			}
		}
	}
	return ""
}

func lessRenditionLocalizationKey(left, right struct {
	documentID   int64
	renditionKey string
	side         string
	locale       string
}) bool {
	if left.documentID != right.documentID {
		return left.documentID < right.documentID
	}
	if left.renditionKey != right.renditionKey {
		return left.renditionKey < right.renditionKey
	}
	if left.side != right.side {
		return left.side < right.side
	}
	return left.locale < right.locale
}

// validatePersistedBackupEvidenceCandidates keeps all live acquisition paths
// strict while allowing content backup/restore to read exact historical
// Fandom/Moegirl revision rows created before acquisition IDs gained their
// fetchedAt/raw-digest suffix. The persisted IDs are never rewritten: only the
// defensive validation copies are normalized and passed to the current strict
// validator.
func validatePersistedBackupEvidenceCandidates(candidates []lyricssource.Candidate) error {
	normalized := make([]lyricssource.Candidate, len(candidates))
	for candidateIndex, candidate := range candidates {
		candidate.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef(nil), candidate.IndexEvidenceRefs...)
		candidate.IndexEvidence = append([]lyricssource.IndexEvidence(nil), candidate.IndexEvidence...)
		for evidenceIndex, evidence := range candidate.IndexEvidence {
			if err := lyricssource.ValidateIndexEvidenceEnvelope(evidence); err == nil {
				continue
			}
			if candidate.FetchedAt == "" || evidence.FetchedAt != candidate.FetchedAt {
				return errors.New("legacy persisted MediaWiki revision fetchedAt does not match its fixed identity")
			}
			canonical, err := lyricssource.CanonicalizeLegacyPersistedMediaWikiRevisionEvidence(evidence)
			if err != nil {
				return err
			}
			candidate.IndexEvidence[evidenceIndex] = canonical
			candidate.IndexEvidenceRefs[evidenceIndex].EvidenceID = canonical.EvidenceID
		}
		normalized[candidateIndex] = candidate
	}
	return lyricssource.ValidateCandidatesIndexEvidence(normalized)
}

func restoreLyricsSourceIndexEvidenceTx(ctx context.Context, tx *sql.Tx, record LyricsSourceIndexEvidenceBackupRecord) error {
	evidence, err := lyricsSourceIndexEvidenceFromBackupRecord(record)
	if err != nil {
		return err
	}
	if err := insertOrVerifyLyricsIndexEvidenceTx(ctx, tx, evidence, time.UnixMilli(record.CreatedAt).UTC()); err != nil {
		return err
	}
	var storedCategoriesJSON string
	var storedRawByteCount int
	var storedCreatedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT categories_json,raw_byte_count,created_at
		FROM lyrics_source_index_evidence WHERE provider=? AND evidence_id=?`, record.Provider, record.EvidenceID).
		Scan(&storedCategoriesJSON, &storedRawByteCount, &storedCreatedAt); err != nil {
		return err
	}
	if storedCategoriesJSON != record.CategoriesJSON || storedRawByteCount != record.RawByteCount || storedCreatedAt != record.CreatedAt {
		return ErrLyricsSourceArtifactConflict
	}
	return nil
}

func contentBackupEvidenceRevisionTimestamp(provider, kind string, raw []byte) (string, error) {
	if provider != string(model.LyricsSourceProviderSekaipedia) {
		return "", nil
	}
	if kind != string(lyricssource.IndexEvidenceKindMediaWikiRevision) || len(raw) == 0 {
		return "", errors.New("Sekaipedia source evidence is not an exact MediaWiki revision response")
	}
	if err := legacy.ValidateUniqueJSON(raw); err != nil {
		return "", fmt.Errorf("Sekaipedia source evidence JSON: %w", err)
	}
	type revision struct {
		Timestamp string `json:"timestamp"`
	}
	type page struct {
		Revisions []revision `json:"revisions"`
	}
	var envelope struct {
		Query *struct {
			Pages json.RawMessage `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Query == nil || len(envelope.Query.Pages) == 0 {
		return "", errors.New("Sekaipedia source evidence has no exact revision page")
	}
	pages := []page{}
	trimmed := bytes.TrimSpace(envelope.Query.Pages)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &pages); err != nil {
			return "", fmt.Errorf("Sekaipedia source evidence pages: %w", err)
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		var pageMap map[string]page
		if err := json.Unmarshal(trimmed, &pageMap); err != nil {
			return "", fmt.Errorf("Sekaipedia source evidence pages: %w", err)
		}
		for _, item := range pageMap {
			pages = append(pages, item)
		}
	default:
		return "", errors.New("Sekaipedia source evidence pages are malformed")
	}
	if len(pages) != 1 || len(pages[0].Revisions) != 1 || pages[0].Revisions[0].Timestamp == "" {
		return "", errors.New("Sekaipedia source evidence does not contain one revisionTimestamp")
	}
	return pages[0].Revisions[0].Timestamp, nil
}

func isCanonicalContentBackupSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func lyricsSourceIndexEvidenceFromBackupRecord(record LyricsSourceIndexEvidenceBackupRecord) (lyricssource.IndexEvidence, error) {
	rawDigest := sha256.Sum256(record.RawBytes)
	revisionTimestamp, timestampErr := contentBackupEvidenceRevisionTimestamp(record.Provider, record.Kind, record.RawBytes)
	if record.CreatedAt <= 0 || record.RawByteCount <= 0 || record.RawByteCount != len(record.RawBytes) ||
		!isCanonicalContentBackupSHA256(record.SHA256) || !isCanonicalContentBackupSHA256(record.RawSHA256) ||
		record.SHA256 != record.RawSHA256 || record.RawSHA256 != hex.EncodeToString(rawDigest[:]) ||
		timestampErr != nil || record.RevisionTimestamp != revisionTimestamp {
		return lyricssource.IndexEvidence{}, fmt.Errorf("lyrics source index evidence %s/%s has invalid stored metadata", record.Provider, record.EvidenceID)
	}
	var categories []string
	if err := json.Unmarshal([]byte(record.CategoriesJSON), &categories); err != nil || categories == nil {
		return lyricssource.IndexEvidence{}, fmt.Errorf("lyrics source index evidence %s/%s has invalid categories", record.Provider, record.EvidenceID)
	}
	canonicalCategories, err := json.Marshal(categories)
	if err != nil || string(canonicalCategories) != record.CategoriesJSON {
		return lyricssource.IndexEvidence{}, fmt.Errorf("lyrics source index evidence %s/%s categories are not canonical", record.Provider, record.EvidenceID)
	}
	evidence := lyricssource.IndexEvidence{
		EvidenceID: record.EvidenceID, SHA256: record.SHA256,
		Kind: lyricssource.IndexEvidenceKind(record.Kind), Provider: model.LyricsSourceProvider(record.Provider),
		Origin: record.Origin, PageID: record.PageID, RevisionID: record.RevisionID,
		RevisionTimestamp: record.RevisionTimestamp, MediaWikiSHA1: record.MediaWikiSHA1,
		Title: record.PageTitle, CanonicalURL: record.CanonicalRevisionURL,
		Categories: categories, CanonicalRequestURL: record.CanonicalRequestURL, FetchedAt: record.FetchedAt,
		Raw: append([]byte(nil), record.RawBytes...), RawSHA256: record.RawSHA256,
	}
	return evidence, nil
}

func validateRestoredLyricsDocuments(lyrics LyricsContentExport, performerIDs, documentIDs map[int]bool) error {
	type lineIdentity struct {
		musicID int
		lineID  string
	}
	type positionedSegment struct {
		position int
		segment  model.LyricSegment
	}
	type restoredLine struct {
		line     model.LyricLine
		segments []positionedSegment
	}

	lines := make(map[lineIdentity]*restoredLine, len(lyrics.Lines))
	lineOrder := make(map[int][]lineIdentity, len(documentIDs))
	for _, record := range lyrics.Lines {
		identity := lineIdentity{musicID: record.MusicID, lineID: record.LineID}
		if !documentIDs[record.MusicID] || (record.StanzaBreakBefore != 0 && record.StanzaBreakBefore != 1) {
			return fmt.Errorf("lyrics line %d/%s references an invalid document or stanza flag", record.MusicID, record.LineID)
		}
		if strings.TrimSpace(record.LineID) == "" || lines[identity] != nil {
			return fmt.Errorf("lyrics line %d/%s is empty or duplicated", record.MusicID, record.LineID)
		}
		lines[identity] = &restoredLine{line: model.LyricLine{
			ID: record.LineID, Order: record.Position, Japanese: record.Japanese, Chinese: record.Chinese, English: record.English,
			StanzaBreakBefore: record.StanzaBreakBefore == 1, Segments: []model.LyricSegment{},
		}}
		lineOrder[record.MusicID] = append(lineOrder[record.MusicID], identity)
	}
	segmentPositions := make(map[lineIdentity]map[int]bool, len(lines))
	for _, record := range lyrics.Segments {
		identity := lineIdentity{musicID: record.MusicID, lineID: record.LineID}
		line := lines[identity]
		if line == nil || record.Position < 0 {
			return fmt.Errorf("lyrics segment %d/%s/%d references an invalid line or position", record.MusicID, record.LineID, record.Position)
		}
		if segmentPositions[identity] == nil {
			segmentPositions[identity] = map[int]bool{}
		}
		if segmentPositions[identity][record.Position] {
			return fmt.Errorf("lyrics segment %d/%s/%d is duplicated", record.MusicID, record.LineID, record.Position)
		}
		segmentPositions[identity][record.Position] = true
		var performerIDsJSON []int
		decoder := json.NewDecoder(bytes.NewBufferString(record.PerformerIDsJSON))
		if err := decoder.Decode(&performerIDsJSON); err != nil {
			return fmt.Errorf("lyrics segment %d/%s/%d performerIds: %w", record.MusicID, record.LineID, record.Position, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return fmt.Errorf("lyrics segment %d/%s/%d performerIds has trailing JSON", record.MusicID, record.LineID, record.Position)
		}
		var ruby []model.LyricRubySpan
		rubyJSON := record.RubyJSON
		if rubyJSON == "" {
			encoded, _ := json.Marshal([]model.LyricRubySpan{{Text: record.Text}})
			rubyJSON = string(encoded)
		}
		rubyDecoder := json.NewDecoder(bytes.NewBufferString(rubyJSON))
		if err := rubyDecoder.Decode(&ruby); err != nil {
			return fmt.Errorf("lyrics segment %d/%s/%d ruby: %w", record.MusicID, record.LineID, record.Position, err)
		}
		if err := rubyDecoder.Decode(&struct{}{}); err != io.EOF {
			return fmt.Errorf("lyrics segment %d/%s/%d ruby has trailing JSON", record.MusicID, record.LineID, record.Position)
		}
		line.segments = append(line.segments, positionedSegment{
			position: record.Position,
			segment:  model.LyricSegment{Text: record.Text, PerformerIDs: performerIDsJSON, Ruby: ruby},
		})
	}
	for _, document := range lyrics.Documents {
		documentLines := make([]model.LyricLine, 0, len(lineOrder[document.MusicID]))
		for _, identity := range lineOrder[document.MusicID] {
			line := lines[identity]
			sort.Slice(line.segments, func(left, right int) bool { return line.segments[left].position < line.segments[right].position })
			for _, segment := range line.segments {
				line.line.Segments = append(line.line.Segments, segment.segment)
			}
			documentLines = append(documentLines, line.line)
		}
		candidate := model.SongLyrics{
			MusicID: document.MusicID, Revision: document.Revision, UpdatedAt: formatTimestamp(document.UpdatedAt),
			Attribution: document.Attribution, TranslationCredit: document.TranslationCredit,
			ProofreadingCredit: document.ProofreadingCredit, SourceNote: document.SourceNote, SourceURL: document.SourceURL,
			LicenseNote: document.LicenseNote, SourcePageID: document.SourcePageID, SourceRevisionID: document.SourceRevisionID,
			SourceSHA1: document.SourceSHA1, Lines: documentLines,
		}
		if document.SourceFetchedAt > 0 {
			candidate.SourceFetchedAt = document.SourceFetchedAtRFC3339
			if candidate.SourceFetchedAt == "" {
				// Backups produced before schema v22 only contain compatibility seconds.
				candidate.SourceFetchedAt = formatTimestamp(document.SourceFetchedAt)
			} else {
				exactFetchedAt, err := parseCanonicalExactTimestamp(candidate.SourceFetchedAt)
				if err != nil || exactFetchedAt.Unix() != document.SourceFetchedAt {
					return fmt.Errorf("lyrics document %d exact fetched timestamp does not match compatibility seconds", document.MusicID)
				}
			}
		} else if document.SourceFetchedAtRFC3339 != "" {
			return fmt.Errorf("lyrics document %d has an exact fetched timestamp without compatibility seconds", document.MusicID)
		}
		if _, err := validateLyricsProvenance(candidate); err != nil {
			return fmt.Errorf("lyrics document %d provenance: %w", document.MusicID, err)
		}
		candidate = normalizeEditableLyricsRuby(candidate)
		code, details, sourceHash := validateLyrics(candidate, performerIDs, false)
		if code != "" {
			return fmt.Errorf("lyrics document %d violates %s: %s", document.MusicID, code, strings.Join(details, "; "))
		}
		if sourceHash != document.SourceHash {
			return fmt.Errorf("lyrics document %d source hash does not match its lines", document.MusicID)
		}
	}
	return nil
}

func canonicalizeRestoredPublication(record *LyricsPublicationBackupRecord, performerIDs map[int]bool) error {
	return canonicalizeRestoredPublicationWithSource(record, performerIDs, newCatalogPerformerAliases(), nil)
}

func canonicalizeRestoredPublicationWithSource(record *LyricsPublicationBackupRecord, performerIDs map[int]bool,
	catalogPerformers catalogPerformerAliases, bundle *publicLyricsSourceBundle,
) error {
	if err := legacy.ValidateUniqueJSON([]byte(record.PayloadJSON)); err != nil {
		return fmt.Errorf("lyrics publication %d: %w", record.MusicID, err)
	}
	version, err := publicLyricsPayloadVersion(record.PayloadJSON)
	if err != nil {
		return fmt.Errorf("lyrics publication %d: %w", record.MusicID, err)
	}
	switch version {
	case 1:
		if bundle != nil {
			return fmt.Errorf("lyrics publication %d has a source document but a stale v1 payload", record.MusicID)
		}
		decoder := json.NewDecoder(bytes.NewBufferString(record.PayloadJSON))
		decoder.DisallowUnknownFields()
		var public model.PublicSongLyrics
		if err := decoder.Decode(&public); err != nil {
			return fmt.Errorf("lyrics publication %d: %w", record.MusicID, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return fmt.Errorf("lyrics publication %d has trailing JSON", record.MusicID)
		}
		updatedAt, err := parseTimestamp(public.UpdatedAt)
		if err != nil {
			return fmt.Errorf("lyrics publication %d has invalid updatedAt", record.MusicID)
		}
		if public.MusicID != record.MusicID || public.Revision != record.Revision || updatedAt != record.UpdatedAt {
			return fmt.Errorf("lyrics publication %d identity does not match its manifest record", record.MusicID)
		}
		lyrics := normalizeEditableLyricsRuby(model.SongLyrics{MusicID: public.MusicID, Revision: public.Revision, UpdatedAt: public.UpdatedAt,
			Attribution: public.Attribution, SourceURL: public.SourceURL, SourcePageID: public.SourcePageID,
			SourceRevisionID: public.SourceRevisionID, SourceSHA1: public.SourceSHA1,
			SourceFetchedAt: public.SourceFetchedAt, Lines: public.Lines})
		if code, details, _ := validateLyrics(lyrics, performerIDs, true); code != "" {
			return fmt.Errorf("lyrics publication %d violates %s: %s", record.MusicID, code, strings.Join(details, "; "))
		}
		publicV1 := publicLyricsV1(lyrics)
		if err := validatePublicLyricsArtifactSize(publicV1); err != nil {
			return fmt.Errorf("lyrics publication %d: %w", record.MusicID, err)
		}
		payload, err := json.Marshal(publicV1)
		if err != nil {
			return fmt.Errorf("lyrics publication %d: %w", record.MusicID, err)
		}
		record.PayloadJSON = string(payload)
		return nil
	case 2:
		if bundle == nil {
			return fmt.Errorf("lyrics publication %d has a v2 payload without its source document", record.MusicID)
		}
		public, err := decodePublicLyricsV2Detail(
			record.PayloadJSON, record.MusicID, record.Revision, record.UpdatedAt, bundle, catalogPerformers,
		)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(public)
		if err != nil {
			return fmt.Errorf("lyrics publication %d: %w", record.MusicID, err)
		}
		record.PayloadJSON = string(payload)
		return nil
	default:
		return fmt.Errorf("lyrics publication %d has unsupported public version %d", record.MusicID, version)
	}
}

// RestoreBackup commits the legacy public projection and optional additive
// content in one transaction. A missing additive manifest is an old backup: it
// deliberately clears multilingual and lyrics-only state instead of retaining
// unrelated data from the database being replaced.
func (s *Store) RestoreBackup(categories map[string]model.Category, events []LegacyEventRestore,
	entries []EntryLocalizationRecord, eventContent EventContentExport, lyrics LyricsContentExport,
	additivePresent bool, actor string) error {
	return s.RestoreBackupContext(context.Background(), categories, events, entries, eventContent, lyrics, additivePresent, actor)
}

func (s *Store) RestoreBackupContext(ctx context.Context, categories map[string]model.Category, events []LegacyEventRestore,
	entries []EntryLocalizationRecord, eventContent EventContentExport, lyrics LyricsContentExport,
	additivePresent bool, actor string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Fence pre-restore collaboration rooms in the same transaction as the
	// authoritative replacement. Waiting persistence writers resume only after
	// commit and then fail their epoch predicate.
	if _, err := tx.ExecContext(ctx, `UPDATE lyrics_collab_documents SET epoch=epoch+1, updated_at=?`, time.Now().UTC().Unix()); err != nil {
		return err
	}
	for _, category := range model.SupportedCategories {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := categories[category]; !ok {
			return fmt.Errorf("restore is missing category %s", category)
		}
	}
	for _, statement := range []string{
		`DELETE FROM event_story_locale_meta`,
		`DELETE FROM event_story_scenarios`,
		`DELETE FROM event_story_segments`,
		`DELETE FROM event_stories`,
		`DELETE FROM entries`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, category := range model.SupportedCategories {
		if err := ctx.Err(); err != nil {
			return err
		}
		content := categories[category]
		if err := importCategoryTx(ctx, tx, category, content); err != nil {
			return fmt.Errorf("import %s: %w", category, err)
		}
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := importOrderedTx(ctx, tx, event.EventID, event.Meta, event.Episodes, false); err != nil {
			return fmt.Errorf("import event %d: %w", event.EventID, err)
		}
	}
	if additivePresent {
		if err := importTranslationContentTx(ctx, tx, entries, eventContent, lyrics); err != nil {
			return err
		}
	} else {
		if err := suspendLyricsSourceDocumentDeleteGuardsTx(ctx, tx); err != nil {
			return err
		}
		if err := suspendEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx, tx); err != nil {
			return err
		}
		for _, statement := range []string{
			`DELETE FROM entry_localizations`,
			`DELETE FROM event_story_segment_localizations WHERE locale<>'zh-CN'`,
			`DELETE FROM song_lyrics_publications`,
			`DELETE FROM song_lyrics_public_withdrawals`,
			`DELETE FROM song_lyric_segments`,
			`DELETE FROM song_lyric_lines`,
			`DELETE FROM song_lyrics`,
			`DELETE FROM catalog_performers`,
			`DELETE FROM embedded_lyrics_editor_seed_items`,
			`DELETE FROM embedded_lyrics_editor_seed_batches`,
			`DELETE FROM catalog_music`,
			// An old-format payload carries no recovery graph, so leaving one behind
			// would describe a catalog that no longer exists.
			`DELETE FROM lyrics_recovery_import_batches`,
			`DELETE FROM lyrics_recovery_source_evidence`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if err := restoreEmbeddedLyricsEditorSeedDeleteGuardsTx(ctx, tx); err != nil {
			return err
		}
		if err := restoreLyricsSourceDocumentDeleteGuardsTx(ctx, tx); err != nil {
			return err
		}
		if err := supersedeStalePendingLyricsSourceReviewsTx(ctx, tx, time.Now().UTC()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(ts, user, action, detail) VALUES (?, ?, 'backup.restore', ?)`,
		time.Now().Unix(), actor, fmt.Sprintf("additive=%t categories=%d events=%d", additivePresent, len(categories), len(events))); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateLocalizationProjectionCache()
	s.NotifyChange()
	return nil
}

func importCategoryTx(ctx context.Context, tx *sql.Tx, category string, content model.Category) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE category=?`, category); err != nil {
		return err
	}
	now := time.Now().Unix()
	for field, entries := range content {
		for jpKey, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			idsJSON := ""
			if len(entry.Ids) > 0 {
				encoded, err := json.Marshal(entry.Ids)
				if err != nil {
					return err
				}
				idsJSON = string(encoded)
			}
			source := entry.Source
			if source == "" {
				source = model.SourceUnknown
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO entries
				(category, field, jp_key, cn_text, source, ids_json, updated_at, updated_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, 'restore')`, category, field, jpKey, entry.Text, source, idsJSON, now); err != nil {
				return err
			}
		}
	}
	return nil
}

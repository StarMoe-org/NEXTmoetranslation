package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"moesekai/server/internal/model"
)

type LyricsRecoveryBatchBackupRecord struct {
	BatchSHA256           string `json:"batchSha256"`
	SchemaVersion         int    `json:"schemaVersion"`
	RootSchemaVersion     int    `json:"rootSchemaVersion"`
	RootID                string `json:"rootId"`
	RootSHA256            string `json:"rootSha256"`
	CatalogCount          int    `json:"catalogCount"`
	MusicIDsSHA256        string `json:"musicIdsSha256"`
	CoverageJSON          string `json:"coverageJson"`
	EvidenceReceiptSHA256 string `json:"evidenceReceiptSha256"`
	PackSHA256            string `json:"packSha256"`
	SelectionSHA256       string `json:"selectionSha256"`
	EvidenceCount         int    `json:"evidenceCount"`
	ShardCount            int    `json:"shardCount"`
	RawByteCount          int64  `json:"rawByteCount"`
	EncodedByteCount      int64  `json:"encodedByteCount"`
	Actor                 string `json:"actor"`
	CreatedAt             int64  `json:"createdAt"`
}

type LyricsRecoveryItemBackupRecord struct {
	BatchSHA256                string `json:"batchSha256"`
	MusicID                    int    `json:"musicId"`
	JapaneseTitle              string `json:"japaneseTitle"`
	CatalogFingerprint         string `json:"catalogFingerprint"`
	TargetMusicID              int    `json:"targetMusicId"`
	AssociationMusicIDsJSON    string `json:"associationMusicIdsJson"`
	State                      string `json:"state"`
	ResultSHA256               string `json:"resultSha256"`
	DraftSHA256                string `json:"draftSha256"`
	DocumentSHA256             string `json:"documentSha256"`
	AvailabilityDocumentSHA256 string `json:"availabilityDocumentSha256"`
	CreatedAt                  int64  `json:"createdAt"`
}

type LyricsRecoverySourceEvidenceBackupRecord struct {
	Provider             string `json:"provider"`
	EvidenceID           string `json:"evidenceId"`
	SHA256               string `json:"sha256"`
	AcquisitionID        string `json:"acquisitionId"`
	EnvelopeSHA256       string `json:"envelopeSha256"`
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

type LyricsRecoveryArtifactBackupRecord struct {
	BatchSHA256             string `json:"batchSha256"`
	MusicID                 int    `json:"musicId"`
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
	CreatedAt               int64  `json:"createdAt"`
}

type LyricsRecoveryArtifactEvidenceBackupRecord struct {
	BatchSHA256  string `json:"batchSha256"`
	MusicID      int    `json:"musicId"`
	RenditionKey string `json:"renditionKey"`
	Position     int    `json:"position"`
	Provider     string `json:"provider"`
	EvidenceID   string `json:"evidenceId"`
	SHA256       string `json:"sha256"`
}

type LyricsRecoveryContributionBackupRecord struct {
	BatchSHA256        string `json:"batchSha256"`
	MusicID            int    `json:"musicId"`
	Component          string `json:"component"`
	RenditionKey       string `json:"renditionKey"`
	ContributionSHA256 string `json:"contributionSha256"`
}

type LyricsAvailabilityDocumentBackupRecord struct {
	AvailabilityDocumentID int64  `json:"availabilityDocumentId"`
	BatchSHA256            string `json:"batchSha256"`
	MusicID                int    `json:"musicId"`
	SchemaVersion          int    `json:"schemaVersion"`
	State                  string `json:"state"`
	ReasonCode             string `json:"reasonCode"`
	NoLyricsReason         string `json:"noLyricsReason"`
	DocumentJSON           string `json:"documentJson"`
	DocumentSHA256         string `json:"documentSha256"`
	ResultSHA256           string `json:"resultSha256"`
	CreatedAt              int64  `json:"createdAt"`
}

// LyricsRecoveryTakeoverBackupRecord is a lyrics_recovery_takeovers row.
// SupersededDocument is nil when the item owned no source document.
type LyricsRecoveryTakeoverBackupRecord struct {
	MusicID            int                                         `json:"musicId"`
	BatchSHA256        string                                      `json:"batchSha256"`
	ItemState          string                                      `json:"itemState"`
	SupersededDocument *LyricsRecoveryTakeoverDocumentBackupRecord `json:"supersededDocument,omitempty"`
	TakenOverAt        int64                                       `json:"takenOverAt"`
	TakenOverBy        string                                      `json:"takenOverBy"`
}

// LyricsRecoveryTakeoverDocumentBackupRecord is the recovery source document a
// takeover superseded, as its song_lyrics_source_documents row stored it.
// LocalizationsJSON is the takeover's localizations_json, empty when NULL.
type LyricsRecoveryTakeoverDocumentBackupRecord struct {
	SchemaVersion     int    `json:"schemaVersion"`
	ReasonCode        string `json:"reasonCode"`
	DocumentJSON      string `json:"documentJson"`
	DocumentSHA256    string `json:"documentSha256"`
	CreatedAt         int64  `json:"createdAt"`
	LocalizationsJSON string `json:"localizationsJson,omitempty"`
}

// lyricsRecoverySupersededLocalizations is a takeover's localizations_json:
// every row the superseded source document owned in the zh-CN localization
// and translation-edition tables, in primary-key order, without document IDs.
type lyricsRecoverySupersededLocalizations struct {
	Localizations        []lyricsRecoverySupersededLocalization        `json:"localizations"`
	TranslationLines     []lyricsRecoverySupersededTranslationLine     `json:"translationLines,omitempty"`
	SideTranslationLines []lyricsRecoverySupersededSideTranslationLine `json:"sideTranslationLines,omitempty"`
	EditionState         *lyricsRecoverySupersededEditionState         `json:"editionState,omitempty"`
	Editions             []lyricsRecoverySupersededEdition             `json:"editions,omitempty"`
	EditionLocalizations []lyricsRecoverySupersededEditionLocalization `json:"editionLocalizations,omitempty"`
	EditionLines         []lyricsRecoverySupersededEditionLine         `json:"editionLines,omitempty"`
}

type lyricsRecoverySupersededLocalization struct {
	RenditionKey       string `json:"renditionKey"`
	Locale             string `json:"locale"`
	TranslationCredit  string `json:"translationCredit"`
	ProofreadingCredit string `json:"proofreadingCredit"`
	UpdatedAt          int64  `json:"updatedAt"`
	UpdatedBy          string `json:"updatedBy"`
	Revision           int    `json:"revision"`
}

type lyricsRecoverySupersededTranslationLine struct {
	RenditionKey string `json:"renditionKey"`
	Locale       string `json:"locale"`
	Position     int    `json:"position"`
	Text         string `json:"text"`
}

type lyricsRecoverySupersededSideTranslationLine struct {
	RenditionKey string `json:"renditionKey"`
	Side         string `json:"side"`
	Locale       string `json:"locale"`
	Position     int    `json:"position"`
	Text         string `json:"text"`
}

type lyricsRecoverySupersededEditionState struct {
	DefaultEditionKey string `json:"defaultEditionKey"`
	Revision          int    `json:"revision"`
	UpdatedAt         int64  `json:"updatedAt"`
	UpdatedBy         string `json:"updatedBy"`
}

type lyricsRecoverySupersededEdition struct {
	EditionKey string `json:"editionKey"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"createdAt"`
	CreatedBy  string `json:"createdBy"`
}

type lyricsRecoverySupersededEditionLocalization struct {
	EditionKey         string `json:"editionKey"`
	RenditionKey       string `json:"renditionKey"`
	Locale             string `json:"locale"`
	TranslationCredit  string `json:"translationCredit"`
	ProofreadingCredit string `json:"proofreadingCredit"`
	UpdatedAt          int64  `json:"updatedAt"`
	UpdatedBy          string `json:"updatedBy"`
}

type lyricsRecoverySupersededEditionLine struct {
	EditionKey   string `json:"editionKey"`
	RenditionKey string `json:"renditionKey"`
	Side         string `json:"side"`
	Locale       string `json:"locale"`
	Position     int    `json:"position"`
	Text         string `json:"text"`
}

// supersededLyricsRecoveryLocalizationsTx reads the rows the recovery source
// document documentID owns in the localization and translation-edition tables
// as a takeover's localizations_json, NULL when it owns none. The JSON is
// validated like a backup would validate it, because the takeover row is
// immutable and every later export checks it.
func supersededLyricsRecoveryLocalizationsTx(ctx context.Context, tx *sql.Tx, documentID int64, documentJSON string) (sql.NullString, error) {
	var value lyricsRecoverySupersededLocalizations
	queries := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{`SELECT rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision
			FROM song_lyrics_rendition_localizations WHERE document_id=? ORDER BY rendition_key,locale`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededLocalization
			if err := rows.Scan(&row.RenditionKey, &row.Locale, &row.TranslationCredit, &row.ProofreadingCredit,
				&row.UpdatedAt, &row.UpdatedBy, &row.Revision); err != nil {
				return err
			}
			value.Localizations = append(value.Localizations, row)
			return nil
		}},
		{`SELECT rendition_key,locale,position,text FROM song_lyrics_rendition_translation_lines
			WHERE document_id=? ORDER BY rendition_key,locale,position`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededTranslationLine
			if err := rows.Scan(&row.RenditionKey, &row.Locale, &row.Position, &row.Text); err != nil {
				return err
			}
			value.TranslationLines = append(value.TranslationLines, row)
			return nil
		}},
		{`SELECT rendition_key,side,locale,position,text FROM song_lyrics_rendition_side_translation_lines
			WHERE document_id=? ORDER BY rendition_key,side,locale,position`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededSideTranslationLine
			if err := rows.Scan(&row.RenditionKey, &row.Side, &row.Locale, &row.Position, &row.Text); err != nil {
				return err
			}
			value.SideTranslationLines = append(value.SideTranslationLines, row)
			return nil
		}},
		{`SELECT default_edition_key,revision,updated_at,updated_by FROM song_lyrics_translation_edition_state
			WHERE document_id=?`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededEditionState
			if err := rows.Scan(&row.DefaultEditionKey, &row.Revision, &row.UpdatedAt, &row.UpdatedBy); err != nil {
				return err
			}
			value.EditionState = &row
			return nil
		}},
		{`SELECT edition_key,label,created_at,created_by FROM song_lyrics_translation_editions
			WHERE document_id=? ORDER BY edition_key`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededEdition
			if err := rows.Scan(&row.EditionKey, &row.Label, &row.CreatedAt, &row.CreatedBy); err != nil {
				return err
			}
			value.Editions = append(value.Editions, row)
			return nil
		}},
		{`SELECT edition_key,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by
			FROM song_lyrics_translation_edition_localizations WHERE document_id=?
			ORDER BY edition_key,rendition_key,locale`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededEditionLocalization
			if err := rows.Scan(&row.EditionKey, &row.RenditionKey, &row.Locale, &row.TranslationCredit,
				&row.ProofreadingCredit, &row.UpdatedAt, &row.UpdatedBy); err != nil {
				return err
			}
			value.EditionLocalizations = append(value.EditionLocalizations, row)
			return nil
		}},
		{`SELECT edition_key,rendition_key,side,locale,position,text FROM song_lyrics_translation_edition_lines
			WHERE document_id=? ORDER BY edition_key,rendition_key,side,locale,position`, func(rows *sql.Rows) error {
			var row lyricsRecoverySupersededEditionLine
			if err := rows.Scan(&row.EditionKey, &row.RenditionKey, &row.Side, &row.Locale, &row.Position, &row.Text); err != nil {
				return err
			}
			value.EditionLines = append(value.EditionLines, row)
			return nil
		}},
	}
	for _, item := range queries {
		rows, err := tx.QueryContext(ctx, item.query, documentID)
		if err != nil {
			return sql.NullString{}, err
		}
		for rows.Next() {
			if err := item.scan(rows); err != nil {
				rows.Close()
				return sql.NullString{}, err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return sql.NullString{}, err
		}
		if err := rows.Close(); err != nil {
			return sql.NullString{}, err
		}
	}
	if len(value.Localizations)+len(value.TranslationLines)+len(value.SideTranslationLines)+len(value.Editions)+
		len(value.EditionLocalizations)+len(value.EditionLines) == 0 && value.EditionState == nil {
		return sql.NullString{}, nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return sql.NullString{}, err
	}
	document, err := model.DecodeLyricsSourceDocument([]byte(documentJSON))
	if err != nil {
		return sql.NullString{}, err
	}
	if err := validateLyricsRecoverySupersededLocalizations(string(body), document); err != nil {
		return sql.NullString{}, fmt.Errorf("superseded localizations of document %d: %w", documentID, err)
	}
	return sql.NullString{String: string(body), Valid: true}, nil
}

func decodeLyricsRecoverySupersededLocalizations(body string) (lyricsRecoverySupersededLocalizations, error) {
	var value lyricsRecoverySupersededLocalizations
	if err := decodeCanonicalBackupJSON(body, &value); err != nil {
		return lyricsRecoverySupersededLocalizations{}, err
	}
	if len(value.Localizations) == 0 {
		return lyricsRecoverySupersededLocalizations{}, errors.New("superseded localizations have no localization rows")
	}
	return value, nil
}

// contentRows returns the rows in content-backup form, keyed to documentID.
func (value lyricsRecoverySupersededLocalizations) contentRows(documentID int64) LyricsContentExport {
	var rows LyricsContentExport
	for _, row := range value.Localizations {
		rows.RenditionLocalizations = append(rows.RenditionLocalizations, LyricsRenditionLocalizationBackupRecord{
			DocumentID: documentID, RenditionKey: row.RenditionKey, Locale: row.Locale,
			TranslationCredit: row.TranslationCredit, ProofreadingCredit: row.ProofreadingCredit,
			UpdatedAt: row.UpdatedAt, UpdatedBy: row.UpdatedBy, Revision: row.Revision,
		})
	}
	for _, row := range value.TranslationLines {
		rows.RenditionTranslationLines = append(rows.RenditionTranslationLines, LyricsRenditionTranslationLineBackupRecord{
			DocumentID: documentID, RenditionKey: row.RenditionKey, Locale: row.Locale, Position: row.Position, Text: row.Text,
		})
	}
	for _, row := range value.SideTranslationLines {
		rows.RenditionTranslationLines = append(rows.RenditionTranslationLines, LyricsRenditionTranslationLineBackupRecord{
			DocumentID: documentID, RenditionKey: row.RenditionKey, Side: row.Side, Locale: row.Locale,
			Position: row.Position, Text: row.Text,
		})
	}
	// The content export reads both line tables in one ordered query.
	lines := rows.RenditionTranslationLines
	sort.SliceStable(lines, func(left, right int) bool {
		a, b := lines[left], lines[right]
		if a.RenditionKey != b.RenditionKey {
			return a.RenditionKey < b.RenditionKey
		}
		if a.Side != b.Side {
			return a.Side < b.Side
		}
		if a.Locale != b.Locale {
			return a.Locale < b.Locale
		}
		return a.Position < b.Position
	})
	if state := value.EditionState; state != nil {
		rows.TranslationEditionStates = []LyricsTranslationEditionStateBackupRecord{{
			DocumentID: documentID, DefaultEditionKey: state.DefaultEditionKey, Revision: state.Revision,
			UpdatedAt: state.UpdatedAt, UpdatedBy: state.UpdatedBy,
		}}
	}
	for _, row := range value.Editions {
		rows.TranslationEditions = append(rows.TranslationEditions, LyricsTranslationEditionBackupRecord{
			DocumentID: documentID, EditionKey: row.EditionKey, Label: row.Label, CreatedAt: row.CreatedAt, CreatedBy: row.CreatedBy,
		})
	}
	for _, row := range value.EditionLocalizations {
		rows.TranslationEditionLocalizations = append(rows.TranslationEditionLocalizations, LyricsTranslationEditionLocalizationBackupRecord{
			DocumentID: documentID, EditionKey: row.EditionKey, RenditionKey: row.RenditionKey, Locale: row.Locale,
			TranslationCredit: row.TranslationCredit, ProofreadingCredit: row.ProofreadingCredit,
			UpdatedAt: row.UpdatedAt, UpdatedBy: row.UpdatedBy,
		})
	}
	for _, row := range value.EditionLines {
		rows.TranslationEditionLines = append(rows.TranslationEditionLines, LyricsTranslationEditionLineBackupRecord{
			DocumentID: documentID, EditionKey: row.EditionKey, RenditionKey: row.RenditionKey, Side: row.Side,
			Locale: row.Locale, Position: row.Position, Text: row.Text,
		})
	}
	return rows
}

// inPrimaryKeyOrder reports whether every row list is strictly ordered by its
// table's primary key, which is the order supersededLyricsRecoveryLocalizationsTx
// reads them in.
func (value lyricsRecoverySupersededLocalizations) inPrimaryKeyOrder() bool {
	// Keys hold no NUL, so NUL-joined strings order like the key tuples.
	position := func(number int) string { return fmt.Sprintf("\x00%020d", number) }
	increasing := func(count int, keyAt func(int) string) bool {
		for index := 1; index < count; index++ {
			if keyAt(index-1) >= keyAt(index) {
				return false
			}
		}
		return true
	}
	return increasing(len(value.Localizations), func(index int) string {
		row := value.Localizations[index]
		return row.RenditionKey + "\x00" + row.Locale
	}) && increasing(len(value.TranslationLines), func(index int) string {
		row := value.TranslationLines[index]
		return row.RenditionKey + "\x00" + row.Locale + position(row.Position)
	}) && increasing(len(value.SideTranslationLines), func(index int) string {
		row := value.SideTranslationLines[index]
		return row.RenditionKey + "\x00" + row.Side + "\x00" + row.Locale + position(row.Position)
	}) && increasing(len(value.Editions), func(index int) string {
		return value.Editions[index].EditionKey
	}) && increasing(len(value.EditionLocalizations), func(index int) string {
		row := value.EditionLocalizations[index]
		return row.EditionKey + "\x00" + row.RenditionKey + "\x00" + row.Locale
	}) && increasing(len(value.EditionLines), func(index int) string {
		row := value.EditionLines[index]
		return row.EditionKey + "\x00" + row.RenditionKey + "\x00" + row.Side + "\x00" + row.Locale + position(row.Position)
	})
}

// lyricsRecoveryTakeoverSchemaVersion is the migration that created
// lyrics_recovery_takeovers; read-only snapshots of older databases lack it.
const lyricsRecoveryTakeoverSchemaVersion = 38

func exportRecoveryLyricsContentTx(ctx context.Context, tx *sql.Tx, result *LyricsContentExport) error {
	var hasTakeoverSchema int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)`,
		lyricsRecoveryTakeoverSchemaVersion).Scan(&hasTakeoverSchema); err != nil {
		return fmt.Errorf("inspect lyrics recovery takeover schema: %w", err)
	}
	type recoveryExportQuery struct {
		query string
		scan  func(*sql.Rows) error
	}
	queries := []recoveryExportQuery{
		{`SELECT batch_sha256,schema_version,root_schema_version,root_id,root_sha256,catalog_count,
			music_ids_sha256,coverage_json,evidence_receipt_sha256,pack_sha256,selection_sha256,evidence_count,
			shard_count,raw_byte_count,encoded_byte_count,actor,created_at
			FROM lyrics_recovery_import_batches ORDER BY batch_sha256`, func(rows *sql.Rows) error {
			var record LyricsRecoveryBatchBackupRecord
			if err := rows.Scan(&record.BatchSHA256, &record.SchemaVersion, &record.RootSchemaVersion, &record.RootID,
				&record.RootSHA256, &record.CatalogCount, &record.MusicIDsSHA256, &record.CoverageJSON,
				&record.EvidenceReceiptSHA256, &record.PackSHA256, &record.SelectionSHA256, &record.EvidenceCount,
				&record.ShardCount, &record.RawByteCount, &record.EncodedByteCount, &record.Actor, &record.CreatedAt); err != nil {
				return err
			}
			result.RecoveryBatches = append(result.RecoveryBatches, record)
			return nil
		}},
		{`SELECT batch_sha256,music_id,japanese_title,catalog_fingerprint,target_music_id,
			association_music_ids_json,state,result_sha256,draft_sha256,document_sha256,
			availability_document_sha256,created_at
			FROM lyrics_recovery_import_items ORDER BY batch_sha256,music_id`, func(rows *sql.Rows) error {
			var record LyricsRecoveryItemBackupRecord
			if err := rows.Scan(&record.BatchSHA256, &record.MusicID, &record.JapaneseTitle,
				&record.CatalogFingerprint, &record.TargetMusicID, &record.AssociationMusicIDsJSON,
				&record.State, &record.ResultSHA256, &record.DraftSHA256, &record.DocumentSHA256,
				&record.AvailabilityDocumentSHA256, &record.CreatedAt); err != nil {
				return err
			}
			result.RecoveryItems = append(result.RecoveryItems, record)
			return nil
		}},
		{`SELECT evidence.provider,evidence.evidence_id,evidence.sha256,evidence.acquisition_id,evidence.envelope_sha256,
			evidence.kind,evidence.origin,evidence.page_id,evidence.revision_id,evidence.revision_timestamp,
			evidence.mediawiki_sha1,evidence.page_title,evidence.canonical_revision_url,evidence.categories_json,
			evidence.canonical_request_url,evidence.fetched_at,evidence.raw_bytes,evidence.raw_byte_count,
			evidence.raw_sha256,evidence.created_at
			FROM lyrics_recovery_source_evidence AS evidence
			WHERE EXISTS (SELECT 1 FROM lyrics_recovery_import_artifact_evidence AS link
			 WHERE link.provider=evidence.provider AND link.evidence_id=evidence.evidence_id AND link.sha256=evidence.sha256)
			ORDER BY evidence.provider,evidence.evidence_id`, func(rows *sql.Rows) error {
			var record LyricsRecoverySourceEvidenceBackupRecord
			var pageID, revisionID sql.NullInt64
			if err := rows.Scan(&record.Provider, &record.EvidenceID, &record.SHA256, &record.AcquisitionID,
				&record.EnvelopeSHA256, &record.Kind, &record.Origin, &pageID, &revisionID,
				&record.RevisionTimestamp, &record.MediaWikiSHA1, &record.PageTitle, &record.CanonicalRevisionURL,
				&record.CategoriesJSON, &record.CanonicalRequestURL, &record.FetchedAt, &record.RawBytes,
				&record.RawByteCount, &record.RawSHA256, &record.CreatedAt); err != nil {
				return err
			}
			if pageID.Valid {
				record.PageID = int(pageID.Int64)
			}
			if revisionID.Valid {
				record.RevisionID = int(revisionID.Int64)
			}
			record.RawBytes = append([]byte(nil), record.RawBytes...)
			result.RecoverySourceEvidence = append(result.RecoverySourceEvidence, record)
			return nil
		}},
		{`SELECT batch_sha256,music_id,provider,rendition_key,origin,page_id,revision_id,revision_timestamp,
			mediawiki_sha1,page_title,canonical_revision_url,fetched_at,categories_json,section,
			composition_rendition_key,version_reason,index_evidence_refs_json,fixed_identity_json,
			fixed_identity_sha256,raw_byte_count,raw_wikitext_sha256,artifact_sha256,created_at
			FROM lyrics_recovery_import_artifacts ORDER BY batch_sha256,music_id,rendition_key`, func(rows *sql.Rows) error {
			var record LyricsRecoveryArtifactBackupRecord
			if err := rows.Scan(&record.BatchSHA256, &record.MusicID, &record.Provider, &record.RenditionKey,
				&record.Origin, &record.PageID, &record.RevisionID, &record.RevisionTimestamp, &record.MediaWikiSHA1,
				&record.PageTitle, &record.CanonicalRevisionURL, &record.FetchedAt, &record.CategoriesJSON,
				&record.Section, &record.CompositionRenditionKey, &record.VersionReason, &record.IndexEvidenceRefsJSON,
				&record.FixedIdentityJSON, &record.FixedIdentitySHA256, &record.RawByteCount,
				&record.RawWikitextSHA256, &record.ArtifactSHA256, &record.CreatedAt); err != nil {
				return err
			}
			result.RecoveryArtifacts = append(result.RecoveryArtifacts, record)
			return nil
		}},
		{`SELECT batch_sha256,music_id,rendition_key,position,provider,evidence_id,sha256
			FROM lyrics_recovery_import_artifact_evidence ORDER BY batch_sha256,music_id,rendition_key,position`, func(rows *sql.Rows) error {
			var record LyricsRecoveryArtifactEvidenceBackupRecord
			if err := rows.Scan(&record.BatchSHA256, &record.MusicID, &record.RenditionKey, &record.Position,
				&record.Provider, &record.EvidenceID, &record.SHA256); err != nil {
				return err
			}
			result.RecoveryArtifactEvidence = append(result.RecoveryArtifactEvidence, record)
			return nil
		}},
		{`SELECT batch_sha256,music_id,component,rendition_key,contribution_sha256
			FROM lyrics_recovery_import_component_contributions ORDER BY batch_sha256,music_id,component`, func(rows *sql.Rows) error {
			var record LyricsRecoveryContributionBackupRecord
			if err := rows.Scan(&record.BatchSHA256, &record.MusicID, &record.Component,
				&record.RenditionKey, &record.ContributionSHA256); err != nil {
				return err
			}
			result.RecoveryContributions = append(result.RecoveryContributions, record)
			return nil
		}},
		{`SELECT availability_document_id,batch_sha256,music_id,schema_version,state,reason_code,
			no_lyrics_reason,document_json,document_sha256,result_sha256,created_at
			FROM song_lyrics_availability_documents ORDER BY batch_sha256,music_id`, func(rows *sql.Rows) error {
			var record LyricsAvailabilityDocumentBackupRecord
			if err := rows.Scan(&record.AvailabilityDocumentID, &record.BatchSHA256, &record.MusicID,
				&record.SchemaVersion, &record.State, &record.ReasonCode, &record.NoLyricsReason,
				&record.DocumentJSON, &record.DocumentSHA256, &record.ResultSHA256, &record.CreatedAt); err != nil {
				return err
			}
			result.AvailabilityDocuments = append(result.AvailabilityDocuments, record)
			return nil
		}},
	}
	if hasTakeoverSchema == 1 {
		queries = append(queries, recoveryExportQuery{`SELECT music_id,batch_sha256,item_state,schema_version,reason_code,document_json,document_sha256,
			document_created_at,localizations_json,taken_over_at,taken_over_by
			FROM lyrics_recovery_takeovers ORDER BY music_id`, func(rows *sql.Rows) error {
			var record LyricsRecoveryTakeoverBackupRecord
			var schemaVersion, createdAt sql.NullInt64
			var reasonCode, documentJSON, documentSHA, localizationsJSON sql.NullString
			if err := rows.Scan(&record.MusicID, &record.BatchSHA256, &record.ItemState, &schemaVersion, &reasonCode,
				&documentJSON, &documentSHA, &createdAt, &localizationsJSON, &record.TakenOverAt, &record.TakenOverBy); err != nil {
				return err
			}
			if documentJSON.Valid {
				record.SupersededDocument = &LyricsRecoveryTakeoverDocumentBackupRecord{
					SchemaVersion: int(schemaVersion.Int64), ReasonCode: reasonCode.String,
					DocumentJSON: documentJSON.String, DocumentSHA256: documentSHA.String, CreatedAt: createdAt.Int64,
					LocalizationsJSON: localizationsJSON.String,
				}
			}
			result.RecoveryTakeovers = append(result.RecoveryTakeovers, record)
			return nil
		}})
	}
	for _, item := range queries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, item.query)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := item.scan(rows); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

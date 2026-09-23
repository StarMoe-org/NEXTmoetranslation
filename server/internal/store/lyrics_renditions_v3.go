package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/model"
)

func storeV3DocumentComponentRefs(document model.LyricsSourceDocument) (map[string]string, error) {
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		refs[binding.ComponentKey] = binding.FixedIdentityKey
	}
	return refs, nil
}

// validateStoreV3DocumentGraph adds the persistence boundary that is stricter
// than the wire contract: staged and backed-up v3 documents must have one
// canonical rendition/fixed-identity order, and every component reference must
// stay inside its owning logical rendition family.
func validateStoreV3DocumentGraph(document model.LyricsSourceDocument) error {
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return nil
	}
	lastRenditionKey := ""
	for _, rendition := range document.Renditions {
		if lastRenditionKey != "" && rendition.RenditionKey <= lastRenditionKey {
			return errors.New("source v3 renditions are not strictly ordered")
		}
		lastRenditionKey = rendition.RenditionKey
	}
	identities := make(map[string]model.LyricsSourceFixedIdentity, len(document.FixedIdentities))
	lastIdentityKey := ""
	for _, identity := range document.FixedIdentities {
		if lastIdentityKey != "" && identity.RenditionKey <= lastIdentityKey {
			return errors.New("source v3 fixed identities are not strictly ordered")
		}
		lastIdentityKey = identity.RenditionKey
		identities[identity.RenditionKey] = identity
	}
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		identity, found := identities[binding.FixedIdentityKey]
		if !found {
			return fmt.Errorf("source v3 component %q references unknown fixed identity %q", binding.ComponentKey, binding.FixedIdentityKey)
		}
		if model.LyricsSourceCompositionRenditionKey(identity) != binding.RenditionKey {
			return fmt.Errorf("source v3 component %q crosses logical rendition families", binding.ComponentKey)
		}
	}
	return nil
}

func cloneSourceDocumentPtr(document model.LyricsSourceDocument) *model.LyricsSourceDocument {
	copy := document
	copy.FixedIdentities = append([]model.LyricsSourceFixedIdentity(nil), document.FixedIdentities...)
	copy.Renditions = model.CloneLyricsSourceRenditions(document.Renditions)
	return &copy
}

func sourceV3ComponentCount(document model.LyricsSourceDocument) int {
	bindings, err := model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return 0
	}
	return len(bindings)
}

func insertLyricsRenditionLocalizationsTx(ctx context.Context, tx *sql.Tx, documentID int64, document model.LyricsSourceDocument, translations []lyricscontract.RenditionTranslation, actor string, now int64) error {
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return nil
	}
	if err := validateStoreV3DocumentGraph(document); err != nil {
		return err
	}
	if translations == nil {
		return nil
	}
	if len(translations) != len(document.Renditions) {
		return errors.New("v3 localization array does not cover every rendition")
	}
	for index, rendition := range document.Renditions {
		item := translations[index]
		if item.RenditionKey != rendition.RenditionKey {
			return fmt.Errorf("v3 localization record %d is out of canonical rendition order: got %q want %q",
				index, item.RenditionKey, rendition.RenditionKey)
		}
		if item.Translations == nil && len(item.PeerTranslations) == 0 &&
			(item.TranslationCredit != "" || item.ProofreadingCredit != "") {
			return errors.New("v3 localization credits exist without translations")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
			VALUES (?,?,?,?,?,?,?,?)`, documentID, item.RenditionKey, "zh-CN", item.TranslationCredit,
			item.ProofreadingCredit, now, actor, 1); err != nil {
			return err
		}
		for position, text := range item.Translations {
			if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_translation_lines
				(document_id,rendition_key,locale,position,text) VALUES (?,?,?,?,?)`, documentID,
				item.RenditionKey, "zh-CN", position, text); err != nil {
				return err
			}
		}
		lastPeerKey := ""
		for _, peer := range item.PeerTranslations {
			peerKey := peer.Side + "\x00" + peer.Locale
			source, allowed := renditionPeerTranslationSide(document, item.RenditionKey, peer.Side)
			if peer.Locale != "zh-CN" || peerKey <= lastPeerKey || !allowed ||
				len(peer.Translations) == 0 || len(peer.Translations) != len(source.Lines) {
				return fmt.Errorf("v3 localization peer record %q/%s/%s is invalid", item.RenditionKey, peer.Side, peer.Locale)
			}
			lastPeerKey = peerKey
			for position, text := range peer.Translations {
				if _, err := tx.ExecContext(ctx, `INSERT INTO song_lyrics_rendition_side_translation_lines
					(document_id,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?)`, documentID,
					item.RenditionKey, peer.Side, peer.Locale, position, text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func exportLyricsRenditionLocalizationsTx(ctx context.Context, tx *sql.Tx, documentID int64, document model.LyricsSourceDocument) ([]lyricscontract.RenditionTranslation, error) {
	if document.SchemaVersion != model.LyricsSourceDocumentSchemaVersionV3 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT rendition_key,translation_credit,proofreading_credit
		FROM song_lyrics_rendition_localizations WHERE document_id=? AND locale='zh-CN' ORDER BY rendition_key`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]lyricscontract.RenditionTranslation{}
	expectedKeys := make(map[string]struct{}, len(document.Renditions))
	for _, rendition := range document.Renditions {
		expectedKeys[rendition.RenditionKey] = struct{}{}
	}
	for rows.Next() {
		var item lyricscontract.RenditionTranslation
		if err := rows.Scan(&item.RenditionKey, &item.TranslationCredit, &item.ProofreadingCredit); err != nil {
			return nil, err
		}
		if _, expected := expectedKeys[item.RenditionKey]; !expected {
			return nil, fmt.Errorf("stored v3 localization has unknown rendition %q", item.RenditionKey)
		}
		if _, duplicate := byKey[item.RenditionKey]; duplicate {
			return nil, fmt.Errorf("stored v3 localization repeats rendition %q", item.RenditionKey)
		}
		byKey[item.RenditionKey] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byKey) == 0 {
		return nil, nil
	}
	lineRows, err := tx.QueryContext(ctx, `SELECT rendition_key,position,text
		FROM song_lyrics_rendition_translation_lines WHERE document_id=? AND locale='zh-CN' ORDER BY rendition_key,position`, documentID)
	if err != nil {
		return nil, err
	}
	hasLines := make(map[string]bool, len(byKey))
	for lineRows.Next() {
		var key, text string
		var position int
		if err := lineRows.Scan(&key, &position, &text); err != nil {
			lineRows.Close()
			return nil, err
		}
		item, found := byKey[key]
		if !found || position != len(item.Translations) {
			lineRows.Close()
			return nil, errors.New("stored v3 localization lines are incomplete")
		}
		item.Translations = append(item.Translations, text)
		hasLines[key] = true
		byKey[key] = item
	}
	if err := lineRows.Err(); err != nil {
		lineRows.Close()
		return nil, err
	}
	if err := lineRows.Close(); err != nil {
		return nil, err
	}
	peerByKey := make(map[string][]lyricscontract.RenditionPeerTranslation, len(byKey))
	var hasPeerTable int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=29)`).Scan(&hasPeerTable); err != nil {
		return nil, err
	}
	if hasPeerTable == 1 {
		peerRows, err := tx.QueryContext(ctx, `SELECT rendition_key,side,locale,position,text
			FROM song_lyrics_rendition_side_translation_lines WHERE document_id=?
			ORDER BY rendition_key,side,locale,position`, documentID)
		if err != nil {
			return nil, err
		}
		lastPeerKey := ""
		for peerRows.Next() {
			var renditionKey, side, locale, text string
			var position int
			if err := peerRows.Scan(&renditionKey, &side, &locale, &position, &text); err != nil {
				peerRows.Close()
				return nil, err
			}
			if _, found := byKey[renditionKey]; !found {
				peerRows.Close()
				return nil, fmt.Errorf("stored v3 peer localization has unknown rendition %q", renditionKey)
			}
			source, allowed := renditionPeerTranslationSide(document, renditionKey, side)
			peerKey := renditionKey + "\x00" + side + "\x00" + locale
			if locale != "zh-CN" || !allowed || peerKey < lastPeerKey {
				peerRows.Close()
				return nil, errors.New("stored v3 peer localization is invalid")
			}
			peers := peerByKey[renditionKey]
			if peerKey != lastPeerKey {
				peers = append(peers, lyricscontract.RenditionPeerTranslation{Side: side, Locale: locale})
			}
			if len(peers) == 0 || position != len(peers[len(peers)-1].Translations) {
				peerRows.Close()
				return nil, errors.New("stored v3 peer localization lines are incomplete")
			}
			peers[len(peers)-1].Translations = append(peers[len(peers)-1].Translations, text)
			if len(peers[len(peers)-1].Translations) > len(source.Lines) {
				peerRows.Close()
				return nil, errors.New("stored v3 peer localization has too many lines")
			}
			peerByKey[renditionKey] = peers
			lastPeerKey = peerKey
		}
		if err := peerRows.Err(); err != nil {
			peerRows.Close()
			return nil, err
		}
		if err := peerRows.Close(); err != nil {
			return nil, err
		}
		for renditionKey, peers := range peerByKey {
			for _, peer := range peers {
				source, _ := renditionPeerTranslationSide(document, renditionKey, peer.Side)
				if len(peer.Translations) != len(source.Lines) {
					return nil, fmt.Errorf("stored v3 peer localization for rendition %q/%s is incomplete", renditionKey, peer.Side)
				}
			}
		}
	}
	result := make([]lyricscontract.RenditionTranslation, 0, len(document.Renditions))
	for _, rendition := range document.Renditions {
		item, found := byKey[rendition.RenditionKey]
		if !found {
			return nil, fmt.Errorf("stored v3 localization for rendition %q is incomplete", rendition.RenditionKey)
		}
		if hasLines[rendition.RenditionKey] {
			if len(item.Translations) != renditionLineCountForStore(rendition) {
				return nil, fmt.Errorf("stored v3 localization for rendition %q is incomplete", rendition.RenditionKey)
			}
		} else {
			item.Translations = nil
		}
		item.PeerTranslations = peerByKey[rendition.RenditionKey]
		result = append(result, item)
	}
	return result, nil
}

func renditionLineCountForStore(rendition model.LyricsSourceRendition) int {
	if rendition.Full != nil {
		return len(rendition.Full.Lines)
	}
	if rendition.Game != nil {
		return len(rendition.Game.Lines)
	}
	return 0
}

func v3TranslationsJSON(translations []lyricscontract.RenditionTranslation) (string, error) {
	if translations == nil {
		return "", nil
	}
	body, err := json.Marshal(translations)
	return string(body), err
}

func v3TranslationsDigest(translations []lyricscontract.RenditionTranslation) (string, error) {
	body, err := v3TranslationsJSON(translations)
	if err != nil {
		return "", err
	}
	if body == "" {
		return "", nil
	}
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:]), nil
}

func sourceV3RenditionKeys(document model.LyricsSourceDocument) []string {
	keys := make([]string, len(document.Renditions))
	for index, rendition := range document.Renditions {
		keys[index] = rendition.RenditionKey
	}
	sort.Strings(keys)
	return keys
}

func sourceV3TranslationCreditPair(item lyricscontract.RenditionTranslation) (string, string) {
	return strings.TrimSpace(item.TranslationCredit), strings.TrimSpace(item.ProofreadingCredit)
}

// LyricsRenditionDocument is the authenticated editor envelope for every
// source-v3 document. It deliberately has no manual-publication state:
// recovery Public v3 publication remains a separate, batch-bound operation.
type LyricsRenditionDocument struct {
	MusicID                      int                               `json:"musicId"`
	Status                       string                            `json:"status"`
	PublishedRevision            int                               `json:"publishedRevision,omitempty"`
	Revision                     int                               `json:"revision"`
	UpdatedAt                    string                            `json:"updatedAt"`
	TranslationEditionKey        string                            `json:"translationEditionKey"`
	DefaultTranslationEditionKey string                            `json:"defaultTranslationEditionKey"`
	TranslationEditions          []LyricsTranslationEditionSummary `json:"translationEditions"`
	Renditions                   []PublicLyricsV3Rendition         `json:"renditions"`
}

// LyricsRenditionMutationTarget identifies one editable localization bucket.
// The editor still commits one document-level revision, but collaboration
// events and audit records use these targets to describe the actual changed
// rendition side(s) without trusting the client's currently visible tab.
type LyricsRenditionMutationTarget struct {
	RenditionKey string `json:"renditionKey"`
	Side         string `json:"side"`
	Locale       string `json:"locale"`
}

type LyricsRenditionContractError struct {
	Code    string
	Details []string
	Current *LyricsRenditionDocument
}

func (e *LyricsRenditionContractError) Error() string { return e.Code }

type lyricsRenditionLocalizationState struct {
	HasRows      bool
	Revision     int
	UpdatedAt    int64
	Translations []lyricscontract.RenditionTranslation
	// SideTranslations contains only explicitly persisted non-primary peers.
	// The historical primary side remains in Translations for backup and import
	// compatibility (Full when present, otherwise Game).
	SideTranslations map[string]map[string][]string
}

type lyricsRenditionEditorBundle struct {
	musicID             int
	documentID          int64
	documentSHA         string
	manifestBatchSHA256 string
	recoveryProvenance  bool
	createdAt           int64
	document            model.LyricsSourceDocument
	bindings            []model.LyricsSourceRenditionComponentBinding
	identities          map[string]model.LyricsSourceFixedIdentity
	contributions       map[string]string
	localization        lyricsRenditionLocalizationState
}

// GetLyricsDocument keeps the legacy response shape for legacy source
// documents and uses the authenticated plural envelope for every source-v3
// document. A source-v3 document must never silently fall back to a second
// mutable SongLyrics store.
func (s *Store) GetLyricsDocument(musicID int) (any, error) {
	return s.GetLyricsDocumentWithEdition(musicID, "", false)
}

func (s *Store) GetLyricsDocumentWithEdition(musicID int, editionKey string, explicit bool) (any, error) {
	if explicit && !validLyricsTranslationEditionKey(editionKey) {
		return nil, &LyricsRenditionContractError{Code: "invalid_translation_edition", Details: []string{"translationEditionKey is invalid"}}
	}
	plural, pluralErr := s.GetLyricsRenditionDocumentEdition(musicID, editionKey, explicit)
	if pluralErr == nil {
		return plural, nil
	}
	if !errors.Is(pluralErr, ErrLyricsNotFound) {
		return nil, pluralErr
	}
	if explicit {
		// A translation-edition selector is valid only for source-v3 documents.
		// Never accept the parameter and silently return the legacy shape.
		return nil, ErrLyricsTranslationEditionNotFound
	}
	legacy, legacyErr := s.GetLyrics(musicID)
	if legacyErr != nil {
		return nil, legacyErr
	}
	return legacy, nil
}

func (s *Store) GetLyricsRenditionDocument(musicID int) (LyricsRenditionDocument, error) {
	return s.GetLyricsRenditionDocumentEdition(musicID, "", false)
}

func (s *Store) GetLyricsRenditionDocumentEdition(musicID int, editionKey string, explicit bool) (LyricsRenditionDocument, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	defer tx.Rollback()
	bundle, err := loadLyricsRenditionEditorBundle(tx, musicID)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	selection, err := loadLyricsTranslationEditionSelection(tx, bundle, editionKey, explicit)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	result, err := buildLyricsTranslationEditionDocument(bundle, selection)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	if err := tx.Commit(); err != nil {
		return LyricsRenditionDocument{}, err
	}
	return result, nil
}

// resolveLyricsRenditionEditorProvenance selects exactly one provenance graph.
// A source document whose batch is a recovery batch must have an exact
// recovery item/document owner and no legacy rows; a non-recovery batch must
// not be claimed by any recovery graph before the legacy tables are read.
func resolveLyricsRenditionEditorProvenance(q queryRower, bundle lyricsRenditionEditorBundle) (bool, error) {
	var recoveryBatchCount int
	if err := q.QueryRow(`SELECT COUNT(*) FROM lyrics_recovery_import_batches WHERE batch_sha256=?`,
		bundle.manifestBatchSHA256).Scan(&recoveryBatchCount); err != nil {
		return false, err
	}
	if recoveryBatchCount > 1 {
		return false, errors.New("source v3 editor recovery batch identity is duplicated")
	}
	if recoveryBatchCount == 1 {
		var state, draftSHA, documentSHA string
		if err := q.QueryRow(`SELECT state,draft_sha256,document_sha256
			FROM lyrics_recovery_import_items WHERE batch_sha256=? AND music_id=?`,
			bundle.manifestBatchSHA256, bundle.musicID).Scan(&state, &draftSHA, &documentSHA); err == sql.ErrNoRows {
			return false, errors.New("source v3 editor recovery batch does not own the document music")
		} else if err != nil {
			return false, err
		}
		if state != "complete" && state != "game_only" || draftSHA == "" || documentSHA != bundle.documentSHA {
			return false, errors.New("source v3 editor recovery item does not own the exact document")
		}
		var legacyGraphRows int
		if err := q.QueryRow(`SELECT
			(SELECT COUNT(*) FROM song_lyrics_source_artifacts WHERE document_id=?)+
			(SELECT COUNT(*) FROM song_lyrics_component_contributions WHERE document_id=?)`,
			bundle.documentID, bundle.documentID).Scan(&legacyGraphRows); err != nil {
			return false, err
		}
		if legacyGraphRows != 0 {
			return false, errors.New("source v3 editor document mixes recovery and legacy provenance graphs")
		}
		return true, nil
	}

	var recoveryOwnershipRows int
	if err := q.QueryRow(`SELECT
		(SELECT COUNT(*) FROM lyrics_recovery_import_items WHERE music_id=?)+
		(SELECT COUNT(*) FROM lyrics_recovery_import_artifacts WHERE music_id=?)+
		(SELECT COUNT(*) FROM lyrics_recovery_import_component_contributions WHERE music_id=?)`,
		bundle.musicID, bundle.musicID, bundle.musicID).Scan(&recoveryOwnershipRows); err != nil {
		return false, err
	}
	if recoveryOwnershipRows != 0 {
		return false, errors.New("source v3 editor legacy document is claimed by a recovery provenance graph")
	}
	return false, nil
}

func loadLyricsRenditionEditorBundle(q queryRower, musicID int) (lyricsRenditionEditorBundle, error) {
	bundle := lyricsRenditionEditorBundle{musicID: musicID}
	var schemaVersion int
	var reasonCode, documentJSON string
	if err := q.QueryRow(`SELECT document_id,schema_version,reason_code,document_json,document_sha256,
		manifest_batch_sha256,created_at
		FROM song_lyrics_source_documents AS source
		WHERE source.music_id=? AND source.schema_version=?`,
		musicID, model.LyricsSourceDocumentSchemaVersionV3).Scan(
		&bundle.documentID, &schemaVersion, &reasonCode, &documentJSON, &bundle.documentSHA,
		&bundle.manifestBatchSHA256, &bundle.createdAt,
	); err == sql.ErrNoRows {
		return lyricsRenditionEditorBundle{}, ErrLyricsNotFound
	} else if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	if musicID <= 0 || bundle.documentID <= 0 || bundle.createdAt <= 0 {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor document has invalid persisted identity")
	}
	var legacyRows int
	if err := q.QueryRow(`SELECT COUNT(*) FROM song_lyrics WHERE music_id=?`, musicID).Scan(&legacyRows); err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	if legacyRows != 0 {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor document mixes plural localizations with legacy editable lyrics")
	}
	digest := sha256.Sum256([]byte(documentJSON))
	if hex.EncodeToString(digest[:]) != bundle.documentSHA {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor document checksum changed")
	}
	document, err := model.DecodeLyricsSourceDocument([]byte(documentJSON))
	if err != nil || document.SchemaVersion != schemaVersion || string(document.ReasonCode) != reasonCode {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor document is invalid or stale")
	}
	if err := validateStoreV3DocumentGraph(document); err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	canonicalJSON, err := json.Marshal(document)
	if err != nil || string(canonicalJSON) != documentJSON {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor document JSON is not canonical")
	}
	bundle.document = document
	bundle.recoveryProvenance, err = resolveLyricsRenditionEditorProvenance(q, bundle)
	if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	bundle.bindings, err = model.EnumerateLyricsSourceRenditionComponents(document.Renditions)
	if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	bundle.identities = make(map[string]model.LyricsSourceFixedIdentity, len(document.FixedIdentities))
	for _, identity := range document.FixedIdentities {
		if _, duplicate := bundle.identities[identity.RenditionKey]; duplicate {
			return lyricsRenditionEditorBundle{}, errors.New("source v3 editor document repeats a fixed identity")
		}
		bundle.identities[identity.RenditionKey] = identity
	}
	artifactQuery := `SELECT provider,rendition_key,origin,page_id,revision_id,revision_timestamp,
		mediawiki_sha1,page_title,canonical_revision_url,fetched_at,categories_json,section,
		composition_rendition_key,version_reason,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256
		FROM song_lyrics_source_artifacts WHERE document_id=? ORDER BY rendition_key`
	artifactArgs := []any{bundle.documentID}
	if bundle.recoveryProvenance {
		artifactQuery = `SELECT provider,rendition_key,origin,page_id,revision_id,revision_timestamp,
			mediawiki_sha1,page_title,canonical_revision_url,fetched_at,categories_json,section,
			composition_rendition_key,version_reason,index_evidence_refs_json,fixed_identity_json,fixed_identity_sha256
			FROM lyrics_recovery_import_artifacts WHERE batch_sha256=? AND music_id=? ORDER BY rendition_key`
		artifactArgs = []any{bundle.manifestBatchSHA256, bundle.musicID}
	}
	rows, err := q.Query(artifactQuery, artifactArgs...)
	if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	seenArtifacts := make(map[string]struct{}, len(bundle.identities))
	for rows.Next() {
		var stored model.LyricsSourceFixedIdentity
		var categoriesJSON, evidenceJSON, identityJSON, identitySHA string
		if err := rows.Scan(&stored.Provider, &stored.RenditionKey, &stored.Origin, &stored.PageID, &stored.RevisionID,
			&stored.RevisionTimestamp, &stored.SHA1, &stored.Title, &stored.CanonicalURL, &stored.FetchedAt,
			&categoriesJSON, &stored.Section, &stored.CompositionRenditionKey, &stored.VersionReason,
			&evidenceJSON, &identityJSON, &identitySHA); err != nil {
			rows.Close()
			return lyricsRenditionEditorBundle{}, err
		}
		if err := json.Unmarshal([]byte(categoriesJSON), &stored.Categories); err != nil {
			rows.Close()
			return lyricsRenditionEditorBundle{}, err
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &stored.IndexEvidenceRefs); err != nil {
			rows.Close()
			return lyricsRenditionEditorBundle{}, err
		}
		decoded, err := model.DecodeLyricsSourceFixedIdentity([]byte(identityJSON))
		if err != nil {
			rows.Close()
			return lyricsRenditionEditorBundle{}, err
		}
		canonicalIdentityJSON, err := json.Marshal(decoded)
		identityDigest := sha256.Sum256([]byte(identityJSON))
		expected, found := bundle.identities[stored.RenditionKey]
		if err != nil || !found || !publicLyricsFixedIdentityScalarsMatch(stored, decoded) ||
			!reflect.DeepEqual(decoded, expected) || string(canonicalIdentityJSON) != identityJSON ||
			hex.EncodeToString(identityDigest[:]) != identitySHA {
			rows.Close()
			return lyricsRenditionEditorBundle{}, fmt.Errorf("source v3 editor artifact %q changed", stored.RenditionKey)
		}
		if _, duplicate := seenArtifacts[stored.RenditionKey]; duplicate {
			rows.Close()
			return lyricsRenditionEditorBundle{}, fmt.Errorf("source v3 editor artifact %q is duplicated", stored.RenditionKey)
		}
		seenArtifacts[stored.RenditionKey] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return lyricsRenditionEditorBundle{}, err
	}
	if err := rows.Close(); err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	if len(seenArtifacts) != len(bundle.identities) {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor artifacts do not cover every fixed identity")
	}
	expectedRefs, err := storeV3DocumentComponentRefs(document)
	if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	bundle.contributions = make(map[string]string, len(expectedRefs))
	contributionQuery := `SELECT component,rendition_key,contribution_sha256
		FROM song_lyrics_component_contributions WHERE document_id=? ORDER BY component`
	contributionArgs := []any{bundle.documentID}
	if bundle.recoveryProvenance {
		contributionQuery = `SELECT component,rendition_key,contribution_sha256
			FROM lyrics_recovery_import_component_contributions
			WHERE batch_sha256=? AND music_id=? ORDER BY component`
		contributionArgs = []any{bundle.manifestBatchSHA256, bundle.musicID}
	}
	rows, err = q.Query(contributionQuery, contributionArgs...)
	if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	for rows.Next() {
		var component, identityKey, contributionSHA string
		if err := rows.Scan(&component, &identityKey, &contributionSHA); err != nil {
			rows.Close()
			return lyricsRenditionEditorBundle{}, err
		}
		expected, found := expectedRefs[component]
		contributionDigest := sha256.Sum256([]byte(bundle.documentSHA + "\x00" + component + "\x00" + identityKey))
		if !found || expected != identityKey || bundle.contributions[component] != "" ||
			hex.EncodeToString(contributionDigest[:]) != contributionSHA {
			rows.Close()
			return lyricsRenditionEditorBundle{}, fmt.Errorf("source v3 editor contribution %q changed", component)
		}
		bundle.contributions[component] = identityKey
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return lyricsRenditionEditorBundle{}, err
	}
	if err := rows.Close(); err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	if len(bundle.contributions) != len(expectedRefs) {
		return lyricsRenditionEditorBundle{}, errors.New("source v3 editor contributions are incomplete")
	}
	localization, err := loadLyricsRenditionLocalizationState(q, bundle.documentID, document)
	if err != nil {
		return lyricsRenditionEditorBundle{}, err
	}
	bundle.localization = localization
	return bundle, nil
}

func loadLyricsRenditionLocalizationState(q queryRower, documentID int64, document model.LyricsSourceDocument) (lyricsRenditionLocalizationState, error) {
	rows, err := q.Query(`SELECT rendition_key,locale,translation_credit,proofreading_credit,updated_at,revision
		FROM song_lyrics_rendition_localizations WHERE document_id=? ORDER BY rendition_key,locale`, documentID)
	if err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	defer rows.Close()
	byKey := make(map[string]lyricscontract.RenditionTranslation, len(document.Renditions))
	state := lyricsRenditionLocalizationState{}
	for rows.Next() {
		var item lyricscontract.RenditionTranslation
		var locale string
		var updatedAt int64
		var revision int
		if err := rows.Scan(&item.RenditionKey, &locale, &item.TranslationCredit, &item.ProofreadingCredit, &updatedAt, &revision); err != nil {
			return lyricsRenditionLocalizationState{}, err
		}
		if locale != "zh-CN" || revision <= 0 || updatedAt <= 0 {
			return lyricsRenditionLocalizationState{}, errors.New("source v3 editor localization metadata is unsupported")
		}
		if _, duplicate := byKey[item.RenditionKey]; duplicate {
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor localization %q is duplicated", item.RenditionKey)
		}
		if !state.HasRows {
			state.HasRows, state.Revision, state.UpdatedAt = true, revision, updatedAt
		} else if revision != state.Revision || updatedAt != state.UpdatedAt {
			return lyricsRenditionLocalizationState{}, errors.New("source v3 editor localization revisions are inconsistent")
		}
		byKey[item.RenditionKey] = item
	}
	if err := rows.Err(); err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	if !state.HasRows {
		state.Revision = 1
		return state, nil
	}
	if len(byKey) != len(document.Renditions) {
		return lyricsRenditionLocalizationState{}, errors.New("source v3 editor localizations do not cover every rendition")
	}
	lineRows, err := q.Query(`SELECT rendition_key,locale,position,text
		FROM song_lyrics_rendition_translation_lines WHERE document_id=? ORDER BY rendition_key,locale,position`, documentID)
	if err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	linesByKey := make(map[string][]string, len(byKey))
	for lineRows.Next() {
		var key, locale, text string
		var position int
		if err := lineRows.Scan(&key, &locale, &position, &text); err != nil {
			lineRows.Close()
			return lyricsRenditionLocalizationState{}, err
		}
		if locale != "zh-CN" || position != len(linesByKey[key]) {
			lineRows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 editor translation lines are incomplete")
		}
		if _, found := byKey[key]; !found {
			lineRows.Close()
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor translation line has unknown rendition %q", key)
		}
		linesByKey[key] = append(linesByKey[key], text)
	}
	if err := lineRows.Err(); err != nil {
		lineRows.Close()
		return lyricsRenditionLocalizationState{}, err
	}
	if err := lineRows.Close(); err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	state.Translations = make([]lyricscontract.RenditionTranslation, 0, len(document.Renditions))
	for _, rendition := range document.Renditions {
		item, found := byKey[rendition.RenditionKey]
		if !found {
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor localization %q is missing", rendition.RenditionKey)
		}
		lines := linesByKey[rendition.RenditionKey]
		if len(lines) != 0 && len(lines) != renditionLineCountForStore(rendition) {
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor localization %q has incomplete translation lines", rendition.RenditionKey)
		}
		item.Translations = append([]string(nil), lines...)
		state.Translations = append(state.Translations, item)
	}
	peerRows, err := q.Query(`SELECT rendition_key,side,locale,position,text
		FROM song_lyrics_rendition_side_translation_lines
		WHERE document_id=? ORDER BY rendition_key,side,locale,position`, documentID)
	if err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	state.SideTranslations = map[string]map[string][]string{}
	for peerRows.Next() {
		var key, side, locale, text string
		var position int
		if err := peerRows.Scan(&key, &side, &locale, &position, &text); err != nil {
			peerRows.Close()
			return lyricsRenditionLocalizationState{}, err
		}
		if locale != "zh-CN" || side != "full" && side != "game" || position < 0 {
			peerRows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 editor peer translation identity is unsupported")
		}
		source, _ := renditionPeerTranslationSide(document, key, side)
		if source == nil {
			peerRows.Close()
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor peer translation %q/%s has no authoritative side", key, side)
		}
		if state.SideTranslations[key] == nil {
			state.SideTranslations[key] = map[string][]string{}
		}
		if position != len(state.SideTranslations[key][side]) {
			peerRows.Close()
			return lyricsRenditionLocalizationState{}, errors.New("source v3 editor peer translation lines are incomplete")
		}
		state.SideTranslations[key][side] = append(state.SideTranslations[key][side], text)
	}
	if err := peerRows.Err(); err != nil {
		peerRows.Close()
		return lyricsRenditionLocalizationState{}, err
	}
	if err := peerRows.Close(); err != nil {
		return lyricsRenditionLocalizationState{}, err
	}
	for key, sides := range state.SideTranslations {
		for side, lines := range sides {
			source, found := renditionTranslationSide(document, key, side)
			if !found || len(lines) != len(source.Lines) {
				return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor peer translation %q/%s is incomplete", key, side)
			}
		}
	}
	for _, item := range state.Translations {
		if len(item.Translations) != 0 || len(state.SideTranslations[item.RenditionKey]) != 0 {
			continue
		}
		if item.TranslationCredit != "" || item.ProofreadingCredit != "" {
			return lyricsRenditionLocalizationState{}, fmt.Errorf("source v3 editor localization %q has credits without translation rows", item.RenditionKey)
		}
	}
	return state, nil
}

func lyricsRenditionEditorUpdatedAt(bundle lyricsRenditionEditorBundle, localization lyricsRenditionLocalizationState) int64 {
	if localization.HasRows {
		return localization.UpdatedAt
	}
	return bundle.createdAt
}

func buildLyricsRenditionEditorDocument(bundle lyricsRenditionEditorBundle, localization lyricsRenditionLocalizationState) (LyricsRenditionDocument, error) {
	updatedAt := lyricsRenditionEditorUpdatedAt(bundle, localization)
	result := LyricsRenditionDocument{
		MusicID: bundle.musicID, Status: "draft", Revision: localization.Revision,
		UpdatedAt: formatTimestamp(updatedAt), Renditions: make([]PublicLyricsV3Rendition, 0, len(bundle.document.Renditions)),
	}
	localizedByKey := make(map[string]lyricscontract.RenditionTranslation, len(localization.Translations))
	for _, item := range localization.Translations {
		localizedByKey[item.RenditionKey] = item
	}
	for _, rendition := range bundle.document.Renditions {
		item, localized := localizedByKey[rendition.RenditionKey]
		var localizationRecord *LyricsRenditionLocalizationBackupRecord
		if localization.HasRows {
			if !localized {
				return LyricsRenditionDocument{}, fmt.Errorf("source v3 editor localization %q is missing", rendition.RenditionKey)
			}
			localizationRecord = &LyricsRenditionLocalizationBackupRecord{
				DocumentID: bundle.documentID, RenditionKey: rendition.RenditionKey, Locale: "zh-CN",
				TranslationCredit: item.TranslationCredit, ProofreadingCredit: item.ProofreadingCredit,
				UpdatedAt: localization.UpdatedAt, Revision: localization.Revision,
			}
		}
		peerTranslations := map[string][]string{}
		if sides := localization.SideTranslations[rendition.RenditionKey]; sides != nil {
			for side, translations := range sides {
				peerTranslations[side] = append([]string(nil), translations...)
			}
		}
		publicRendition, err := buildLyricsV3Rendition(
			rendition, item.Translations, peerTranslations, localizationRecord,
			bundle.bindings, bundle.identities, bundle.contributions, false,
		)
		if err != nil {
			return LyricsRenditionDocument{}, err
		}
		result.Renditions = append(result.Renditions, publicRendition)
	}
	if result.MusicID <= 0 || result.Revision <= 0 || len(result.Renditions) == 0 {
		return LyricsRenditionDocument{}, errors.New("source v3 editor envelope is incomplete")
	}
	return result, nil
}

func (s *Store) SaveLyricsRenditionMutation(input LyricsRenditionDocument, user string) (LyricsRenditionDocument, bool, error) {
	result, changed, _, err := s.saveLyricsRenditionMutation(input, user, nil)
	return result, changed, err
}

func (s *Store) SaveLyricsRenditionMutationWithTargets(input LyricsRenditionDocument, user string) (LyricsRenditionDocument, bool, []LyricsRenditionMutationTarget, error) {
	return s.saveLyricsRenditionMutation(input, user, nil)
}

// SaveLyricsRenditionMutationWithBeforeCommit runs beforeCommit inside the
// authoritative rendition save transaction. Callback errors roll back both
// the rendition mutation and any callback writes.
func (s *Store) SaveLyricsRenditionMutationWithBeforeCommit(
	input LyricsRenditionDocument,
	user string,
	beforeCommit func(*sql.Tx, LyricsRenditionDocument, bool) error,
) (LyricsRenditionDocument, bool, error) {
	result, changed, _, err := s.saveLyricsRenditionMutation(input, user, beforeCommit)
	return result, changed, err
}

// lyricsRenditionEditorDiff holds the requested and stored translation state
// one save compares before it writes.
type lyricsRenditionEditorDiff struct {
	requested, stored           []lyricscontract.RenditionTranslation
	requestedSides, storedSides map[string]map[string][]string
}

func (s *Store) saveLyricsRenditionMutation(
	input LyricsRenditionDocument,
	user string,
	beforeCommit func(*sql.Tx, LyricsRenditionDocument, bool) error,
) (LyricsRenditionDocument, bool, []LyricsRenditionMutationTarget, error) {
	unlock := s.lockLyrics(input.MusicID)
	defer unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	defer tx.Rollback()
	bundle, selection, current, err := loadLyricsRenditionMutationTx(tx, input)
	if err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	if err := validateLyricsRenditionMutationInput(&input, current); err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	diff, err := diffLyricsRenditionEditorTranslations(input, current, selection)
	if err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	sourceChanged, err := persistLyricsRenditionSourceDocumentTx(tx, &bundle, input)
	if err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	if !sourceChanged && reflect.DeepEqual(diff.requested, diff.stored) && equalLyricsRenditionSideTranslations(diff.requestedSides, diff.storedSides) {
		if beforeCommit != nil {
			if err := beforeCommit(tx, current, false); err != nil {
				return LyricsRenditionDocument{}, false, nil, err
			}
			if err := tx.Commit(); err != nil {
				return LyricsRenditionDocument{}, false, nil, err
			}
		}
		return current, false, nil, nil
	}
	mutationTargets := lyricsRenditionMutationTargets(input, current)
	if len(mutationTargets) == 0 {
		if len(input.Renditions) > 0 {
			mutationTargets = append(mutationTargets, LyricsRenditionMutationTarget{
				RenditionKey: input.Renditions[0].Key, Side: "full", Locale: "ja-JP",
			})
		} else {
			return LyricsRenditionDocument{}, false, nil, errors.New("source v3 editor changed without a mutation target")
		}
	}
	nextRevision := current.Revision + 1
	now := time.Now().Unix()
	if currentUpdatedAt := lyricsRenditionEditorUpdatedAt(bundle, selection.localization); now < currentUpdatedAt {
		now = currentUpdatedAt
	}
	if selection.authoritative {
		result, err := writeAuthoritativeLyricsTranslationEditionTx(tx, bundle, selection, input, user, now, nextRevision, mutationTargets)
		if err != nil {
			return LyricsRenditionDocument{}, false, nil, err
		}
		if beforeCommit != nil {
			if err := beforeCommit(tx, result, true); err != nil {
				return LyricsRenditionDocument{}, false, nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return LyricsRenditionDocument{}, false, nil, err
		}
		s.NotifyChange()
		return result, true, mutationTargets, nil
	}
	if err := writeLyricsRenditionTranslationsTx(tx, bundle, diff, input, user, now, nextRevision, mutationTargets); err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	nextState := lyricsRenditionLocalizationState{
		HasRows: true, Revision: nextRevision, UpdatedAt: now, Translations: diff.requested,
		SideTranslations: diff.requestedSides,
	}
	selection.localization = nextState
	result, err := buildLyricsTranslationEditionDocument(bundle, selection)
	if err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	if beforeCommit != nil {
		if err := beforeCommit(tx, result, true); err != nil {
			return LyricsRenditionDocument{}, false, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return LyricsRenditionDocument{}, false, nil, err
	}
	s.NotifyChange()
	return result, true, mutationTargets, nil
}

// loadLyricsRenditionMutationTx reads the stored editor bundle and the
// translation edition the request addresses.
func loadLyricsRenditionMutationTx(tx *sql.Tx, input LyricsRenditionDocument) (
	lyricsRenditionEditorBundle, lyricsTranslationEditionSelection, LyricsRenditionDocument, error,
) {
	bundle, err := loadLyricsRenditionEditorBundle(tx, input.MusicID)
	if err != nil {
		return lyricsRenditionEditorBundle{}, lyricsTranslationEditionSelection{}, LyricsRenditionDocument{}, err
	}
	selection, err := loadLyricsTranslationEditionSelection(tx, bundle, input.TranslationEditionKey, input.TranslationEditionKey != "")
	if errors.Is(err, ErrLyricsTranslationEditionNotFound) {
		return lyricsRenditionEditorBundle{}, lyricsTranslationEditionSelection{}, LyricsRenditionDocument{}, &LyricsRenditionContractError{Code: "translation_edition_not_found"}
	}
	if err != nil {
		return lyricsRenditionEditorBundle{}, lyricsTranslationEditionSelection{}, LyricsRenditionDocument{}, err
	}
	current, err := buildLyricsTranslationEditionDocument(bundle, selection)
	if err != nil {
		return lyricsRenditionEditorBundle{}, lyricsTranslationEditionSelection{}, LyricsRenditionDocument{}, err
	}
	return bundle, selection, current, nil
}

func validateLyricsRenditionMutationInput(input *LyricsRenditionDocument, current LyricsRenditionDocument) error {
	if err := normalizeLyricsTranslationEditionEnvelope(input, current); err != nil {
		return err
	}
	if input.Revision != current.Revision {
		copy := current
		return &LyricsRenditionContractError{Code: "revision_conflict", Current: &copy}
	}
	if err := validateLyricsRenditionImmutableEnvelope(*input, current); err != nil {
		return err
	}
	return nil
}

func diffLyricsRenditionEditorTranslations(input, current LyricsRenditionDocument,
	selection lyricsTranslationEditionSelection,
) (lyricsRenditionEditorDiff, error) {
	requestedSides, requestedPeerBytes, err := lyricsRenditionEditorSideTranslations(input)
	if err != nil {
		return lyricsRenditionEditorDiff{}, err
	}
	storedSides, storedPeerBytes, err := lyricsRenditionEditorSideTranslations(current)
	if err != nil {
		return lyricsRenditionEditorDiff{}, err
	}
	requested, err := lyricsRenditionEditorTranslations(
		input, current, selection.localization.HasRows || len(requestedSides) > 0, requestedPeerBytes, true,
	)
	if err != nil {
		return lyricsRenditionEditorDiff{}, err
	}
	stored, err := lyricsRenditionEditorTranslations(
		current, current, selection.localization.HasRows || len(storedSides) > 0, storedPeerBytes, false,
	)
	if err != nil {
		return lyricsRenditionEditorDiff{}, err
	}
	return lyricsRenditionEditorDiff{
		requested: requested, stored: stored, requestedSides: requestedSides, storedSides: storedSides,
	}, nil
}

// persistLyricsRenditionSourceDocumentTx rewrites the source layer when the
// editor changed it, and reports whether it did.
func persistLyricsRenditionSourceDocumentTx(tx *sql.Tx, bundle *lyricsRenditionEditorBundle,
	input LyricsRenditionDocument,
) (bool, error) {
	sourceChanged, err := updateLyricsSourceDocumentFromEditor(&bundle.document, input)
	if err != nil {
		return false, err
	}
	// A recovery-imported document is owned by the immutable recovery ledger:
	// its contributions live in lyrics_recovery_import_component_contributions
	// and its recovery item pins document_sha256. Rewriting the source layer
	// below would leave the ledger pointing at a document that no longer
	// exists, so only the translation layer stays editable here.
	if sourceChanged && bundle.recoveryProvenance {
		return false, &LyricsRenditionContractError{
			Code:    "source_drift",
			Details: []string{"recovery-imported source documents are immutable; only translations are editable"},
		}
	}
	if sourceChanged {
		newDocumentJSON, err := json.Marshal(bundle.document)
		if err != nil {
			return false, err
		}
		newDocumentDigest := sha256.Sum256(newDocumentJSON)
		newDocumentSHA := hex.EncodeToString(newDocumentDigest[:])
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS song_lyrics_source_documents_immutable_update`); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS song_lyrics_component_contributions_immutable_update`); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`UPDATE song_lyrics_source_documents
			SET document_json=?, document_sha256=? WHERE document_id=?`,
			string(newDocumentJSON), newDocumentSHA, bundle.documentID); err != nil {
			return false, err
		}
		expectedRefs, err := storeV3DocumentComponentRefs(bundle.document)
		if err != nil {
			return false, err
		}
		for component, identityKey := range expectedRefs {
			digest := sha256.Sum256([]byte(newDocumentSHA + "\x00" + component + "\x00" + identityKey))
			sha := hex.EncodeToString(digest[:])
			if _, err := tx.Exec(`UPDATE song_lyrics_component_contributions
				SET contribution_sha256=? WHERE document_id=? AND component=?`, sha, bundle.documentID, component); err != nil {
				return false, err
			}
		}
		// Reinstate the migration v27 immutability guards dropped above, in the
		// same transaction, so the editor write is the only update they allow.
		for _, statement := range []string{
			`CREATE TRIGGER song_lyrics_source_documents_immutable_update BEFORE UPDATE ON song_lyrics_source_documents
			BEGIN SELECT RAISE(ABORT, 'song lyrics source documents are immutable'); END`,
			`CREATE TRIGGER song_lyrics_component_contributions_immutable_update
			BEFORE UPDATE ON song_lyrics_component_contributions
			BEGIN SELECT RAISE(ABORT, 'song lyrics component contributions are immutable'); END`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return false, err
			}
		}
		bundle.documentSHA = newDocumentSHA
	}
	return sourceChanged, nil
}

func writeAuthoritativeLyricsTranslationEditionTx(tx *sql.Tx, bundle lyricsRenditionEditorBundle,
	selection lyricsTranslationEditionSelection, input LyricsRenditionDocument, user string,
	now int64, nextRevision int, mutationTargets []LyricsRenditionMutationTarget,
) (LyricsRenditionDocument, error) {
	if err := replaceMaterializedLyricsTranslationEditionTx(tx, bundle, selection.key, input, user, now); err != nil {
		return LyricsRenditionDocument{}, err
	}
	if _, err := tx.Exec(`UPDATE song_lyrics_translation_edition_state
			SET revision=?,updated_at=?,updated_by=? WHERE document_id=?`, nextRevision, now, user, bundle.documentID); err != nil {
		return LyricsRenditionDocument{}, err
	}
	if err := rewriteLegacyLyricsTranslationMirrorTx(tx, bundle, selection.defaultKey, nextRevision, now, user); err != nil {
		return LyricsRenditionDocument{}, err
	}
	targetsJSON, err := json.Marshal(mutationTargets)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.rendition.save',?)`,
		now, user, fmt.Sprintf("musicId=%d revision=%d editionKey=%s targets=%s", input.MusicID, nextRevision, selection.key, targetsJSON)); err != nil {
		return LyricsRenditionDocument{}, err
	}
	nextSelection, err := loadLyricsTranslationEditionSelection(tx, bundle, selection.key, true)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	result, err := buildLyricsTranslationEditionDocument(bundle, nextSelection)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	return result, nil
}

func writeLyricsRenditionTranslationsTx(tx *sql.Tx, bundle lyricsRenditionEditorBundle,
	diff lyricsRenditionEditorDiff, input LyricsRenditionDocument, user string,
	now int64, nextRevision int, mutationTargets []LyricsRenditionMutationTarget,
) error {
	if _, err := tx.Exec(`DELETE FROM song_lyrics_rendition_translation_lines WHERE document_id=?`, bundle.documentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyrics_rendition_side_translation_lines WHERE document_id=?`, bundle.documentID); err != nil {
		return err
	}
	for _, item := range diff.requested {
		if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_localizations
			(document_id,rendition_key,locale,translation_credit,proofreading_credit,updated_at,updated_by,revision)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(document_id,rendition_key,locale) DO UPDATE SET
			translation_credit=excluded.translation_credit,proofreading_credit=excluded.proofreading_credit,
			updated_at=excluded.updated_at,updated_by=excluded.updated_by,revision=excluded.revision`,
			bundle.documentID, item.RenditionKey, "zh-CN", item.TranslationCredit, item.ProofreadingCredit,
			now, user, nextRevision); err != nil {
			return err
		}
		for position, text := range item.Translations {
			if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_translation_lines
				(document_id,rendition_key,locale,position,text) VALUES (?,?,?,?,?)`,
				bundle.documentID, item.RenditionKey, "zh-CN", position, text); err != nil {
				return err
			}
		}
	}
	for renditionKey, sides := range diff.requestedSides {
		for side, translations := range sides {
			for position, text := range translations {
				if _, err := tx.Exec(`INSERT INTO song_lyrics_rendition_side_translation_lines
					(document_id,rendition_key,side,locale,position,text) VALUES (?,?,?,?,?,?)`,
					bundle.documentID, renditionKey, side, "zh-CN", position, text); err != nil {
					return err
				}
			}
		}
	}
	targetsJSON, err := json.Marshal(mutationTargets)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.rendition.save',?)`,
		now, user, fmt.Sprintf("musicId=%d revision=%d targets=%s", input.MusicID, nextRevision, targetsJSON)); err != nil {
		return err
	}
	return nil
}

func validateLyricsRenditionImmutableEnvelope(input, current LyricsRenditionDocument) error {
	if input.MusicID <= 0 || input.Status != "draft" || input.PublishedRevision != 0 || input.UpdatedAt != current.UpdatedAt {
		return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"plural rendition status and source envelope are immutable"}}
	}
	if len(input.Renditions) != len(current.Renditions) {
		return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"plural rendition set changed"}}
	}
	for i := range input.Renditions {
		inRend := input.Renditions[i]
		curRend := current.Renditions[i]
		if inRend.Key != curRend.Key || inRend.Kind != curRend.Kind {
			return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"rendition key or kind changed"}}
		}
		if (inRend.Full == nil) != (curRend.Full == nil) || (inRend.Game == nil) != (curRend.Game == nil) {
			return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"rendition version sides changed"}}
		}
		if inRend.Full != nil && curRend.Full != nil {
			if err := validateLyricsRenditionSideImmutableFacts(*inRend.Full, *curRend.Full); err != nil {
				return err
			}
		}
		if inRend.Game != nil && curRend.Game != nil && curRend.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection {
			if err := validateLyricsRenditionSideImmutableFacts(*inRend.Game, *curRend.Game); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateLyricsRenditionSideImmutableFacts(inputSide, currentSide PublicLyricsV3Side) error {
	if len(inputSide.Lines) != len(currentSide.Lines) {
		return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"rendition line count changed"}}
	}
	for i := range inputSide.Lines {
		inLine := inputSide.Lines[i]
		curLine := currentSide.Lines[i]
		if inLine.ID != curLine.ID || inLine.Order != curLine.Order {
			return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"line IDs or order changed"}}
		}
		if inLine.Japanese != curLine.Japanese {
			return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"Japanese source text is immutable"}}
		}
		var segConcat strings.Builder
		for _, seg := range inLine.Segments {
			segConcat.WriteString(seg.Text)
			var rubyConcat strings.Builder
			for _, r := range seg.Ruby {
				rubyConcat.WriteString(r.Text)
			}
			if rubyConcat.String() != seg.Text {
				return &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"ruby text must concatenate to segment text"}}
			}
		}
		if segConcat.String() != inLine.Japanese {
			return &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"segment text must concatenate to Japanese line text"}}
		}
		curSpans := lineRubySpans(curLine)
		inSpans := lineRubySpans(inLine)
		if !rubySpansMatch(inSpans, curSpans) {
			return &LyricsRenditionContractError{Code: "source_drift", Details: []string{"Japanese ruby pronunciation and readings are immutable"}}
		}
	}
	return nil
}

func lineRubySpans(line PublicLyricsV3Line) []model.LyricRubySpan {
	var spans []model.LyricRubySpan
	for _, seg := range line.Segments {
		for _, r := range seg.Ruby {
			spans = append(spans, model.LyricRubySpan{
				Text:    r.Text,
				Reading: r.Reading,
			})
		}
	}
	return spans
}

func rubySpansMatch(left, right []model.LyricRubySpan) bool {
	if len(left) == len(right) {
		match := true
		for i := range left {
			if left[i].Text != right[i].Text || left[i].Reading != right[i].Reading {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	var leftText, rightText, leftReading, rightReading strings.Builder
	for _, s := range left {
		leftText.WriteString(s.Text)
		leftReading.WriteString(s.Reading)
	}
	for _, s := range right {
		rightText.WriteString(s.Text)
		rightReading.WriteString(s.Reading)
	}
	return leftText.String() == rightText.String() && leftReading.String() == rightReading.String()
}

func sortedUniquePerformerIDs(performers []model.LyricsSourcePerformer) []string {
	seen := make(map[string]struct{}, len(performers))
	var result []string
	for _, p := range performers {
		if _, exists := seen[p.PerformerID]; !exists {
			seen[p.PerformerID] = struct{}{}
			result = append(result, p.PerformerID)
		}
	}
	sort.Strings(result)
	return result
}

func updateLyricsSourceDocumentFromEditor(
	doc *model.LyricsSourceDocument,
	input LyricsRenditionDocument,
) (bool, error) {
	changed := false
	for rIdx := range doc.Renditions {
		sourceRendition := &doc.Renditions[rIdx]
		var inputRendition *PublicLyricsV3Rendition
		for i := range input.Renditions {
			if input.Renditions[i].Key == sourceRendition.RenditionKey {
				inputRendition = &input.Renditions[i]
				break
			}
		}
		if inputRendition == nil {
			continue
		}
		if sourceRendition.Full != nil && inputRendition.Full != nil {
			newPerformers := make([]model.LyricsSourcePerformer, 0, len(inputRendition.Performers))
			for _, p := range inputRendition.Performers {
				newPerformers = append(newPerformers, model.LyricsSourcePerformer{
					PerformerID: p.PerformerID,
					Name:        p.Name,
					Color:       p.Color,
				})
			}
			if !reflect.DeepEqual(sourceRendition.Full.Performers, newPerformers) {
				sourceRendition.Full.Performers = newPerformers
				sourceRendition.SourcePerformerIDs = sortedUniquePerformerIDs(newPerformers)
				changed = true
			}
			for lineIdx := range sourceRendition.Full.Lines {
				srcLine := &sourceRendition.Full.Lines[lineIdx]
				if lineIdx >= len(inputRendition.Full.Lines) {
					continue
				}
				inLine := inputRendition.Full.Lines[lineIdx]
				if srcLine.StanzaBreakBefore != inLine.StanzaBreakBefore {
					srcLine.StanzaBreakBefore = inLine.StanzaBreakBefore
					changed = true
				}
				newTrailing := make([]string, 0, len(inLine.TrailingPerformerIDs))
				newTrailing = append(newTrailing, inLine.TrailingPerformerIDs...)
				if !reflect.DeepEqual(srcLine.TrailingPerformerIDs, newTrailing) {
					srcLine.TrailingPerformerIDs = newTrailing
					changed = true
				}
				var existingSpans []model.LyricsSourceRubySpan
				for _, seg := range srcLine.Segments {
					for _, span := range seg.Ruby {
						existingSpans = append(existingSpans, span)
					}
				}
				spanCursor := 0
				newSegments := make([]model.LyricsSourceSegment, 0, len(inLine.Segments))
				for _, seg := range inLine.Segments {
					newRuby := make([]model.LyricsSourceRubySpan, 0, len(seg.Ruby))
					for _, r := range seg.Ruby {
						var evidence *model.LyricsSourceReadingEvidence
						if spanCursor < len(existingSpans) && existingSpans[spanCursor].Text == r.Text && existingSpans[spanCursor].Reading == r.Reading {
							evidence = existingSpans[spanCursor].ReadingEvidence
							spanCursor++
						} else {
							for _, ex := range existingSpans {
								if ex.Text == r.Text && ex.Reading == r.Reading && ex.ReadingEvidence != nil {
									evidence = ex.ReadingEvidence
									break
								}
							}
							if evidence == nil && r.Reading != "" {
								evidence = &model.LyricsSourceReadingEvidence{
									Kind:             model.LyricsSourceReadingEvidenceFixedReviewedToken,
									GeneratorVersion: "kagome-v1",
								}
							}
						}
						newRuby = append(newRuby, model.LyricsSourceRubySpan{
							Text:            r.Text,
							Reading:         r.Reading,
							ReadingEvidence: evidence,
						})
					}
					segPerformerIDs := make([]string, 0, len(seg.PerformerIDs))
					segPerformerIDs = append(segPerformerIDs, seg.PerformerIDs...)
					newSegments = append(newSegments, model.LyricsSourceSegment{
						Text:         seg.Text,
						PerformerIDs: segPerformerIDs,
						Ruby:         newRuby,
					})
				}
				if !reflect.DeepEqual(srcLine.Segments, newSegments) {
					srcLine.Segments = newSegments
					changed = true
				}
			}
			if len(sourceRendition.SourcePerformerIDs) > 0 {
				if model.LyricsSourceFullHasCompletePerformerEvidence(*sourceRendition.Full, sourceRendition.SourcePerformerIDs) {
					sourceRendition.FullPerformerEvidence = model.LyricsSourcePerformerEvidenceSourceComplete
				} else if model.LyricsSourceFullHasPerformerSegmentation(*sourceRendition.Full) {
					sourceRendition.FullPerformerEvidence = model.LyricsSourcePerformerEvidenceSourcePartial
				} else {
					sourceRendition.FullPerformerEvidence = model.LyricsSourcePerformerEvidenceNone
				}
			}
		}
		if sourceRendition.Game != nil {
			if sourceRendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection && sourceRendition.Full != nil {
				sourceRendition.Game.Performers = sourceRendition.Full.Performers
			}
			if len(sourceRendition.SourcePerformerIDs) > 0 {
				if model.LyricsSourceFullHasCompletePerformerEvidence(*sourceRendition.Game, sourceRendition.SourcePerformerIDs) {
					sourceRendition.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceSourceComplete
				} else if model.LyricsSourceFullHasPerformerSegmentation(*sourceRendition.Game) {
					sourceRendition.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceSourcePartial
				} else {
					sourceRendition.GamePerformerEvidence = model.LyricsSourcePerformerEvidenceNone
				}
			}
		}
	}
	return changed, nil
}

func lyricsRenditionMutationTargets(input, current LyricsRenditionDocument) []LyricsRenditionMutationTarget {
	if len(input.Renditions) != len(current.Renditions) {
		return nil
	}
	targets := make([]LyricsRenditionMutationTarget, 0, len(input.Renditions))
	for index := range input.Renditions {
		requested := input.Renditions[index]
		stored := current.Renditions[index]
		if requested.Key != stored.Key {
			return nil
		}
		if lyricsRenditionSideTranslationChanged(requested.Full, stored.Full) {
			targets = append(targets, LyricsRenditionMutationTarget{
				RenditionKey: requested.Key, Side: "full", Locale: "zh-CN",
			})
		}
		// An exact-projection Game has no independently persisted localization;
		// its Chinese text follows the Full line-ID mapping and is therefore not
		// reported as a second mutation target.
		if requested.Relation.Kind != model.LyricsSourceRenditionRelationExactProjection &&
			lyricsRenditionSideTranslationChanged(requested.Game, stored.Game) {
			targets = append(targets, LyricsRenditionMutationTarget{
				RenditionKey: requested.Key, Side: "game", Locale: "zh-CN",
			})
		}
		if !reflect.DeepEqual(requested.TranslationCredits, stored.TranslationCredits) {
			targets = append(targets, LyricsRenditionMutationTarget{
				RenditionKey: requested.Key, Side: "credits", Locale: "zh-CN",
			})
		}
	}
	return targets
}

func lyricsRenditionSideTranslationChanged(requested, stored *PublicLyricsV3Side) bool {
	if requested == nil || stored == nil {
		return requested != stored
	}
	if len(requested.Lines) != len(stored.Lines) {
		return true
	}
	for index := range requested.Lines {
		if requested.Lines[index].Chinese != stored.Lines[index].Chinese {
			return true
		}
	}
	return false
}

func lyricsRenditionEditorTranslations(
	input, current LyricsRenditionDocument,
	preserveRows bool,
	peerBytes int,
	validateRequestedLocalization bool,
) ([]lyricscontract.RenditionTranslation, error) {
	if len(input.Renditions) != len(current.Renditions) || len(input.Renditions) == 0 {
		return nil, &LyricsRenditionContractError{Code: "source_drift", Details: []string{"plural rendition set changed"}}
	}
	result := make([]lyricscontract.RenditionTranslation, len(input.Renditions))
	hasLocalization := preserveRows
	totalBytes := peerBytes
	for index := range input.Renditions {
		rendition := input.Renditions[index]
		currentRendition := current.Renditions[index]
		if rendition.Key != currentRendition.Key {
			return nil, &LyricsRenditionContractError{Code: "source_drift", Details: []string{"plural rendition order changed"}}
		}
		item := lyricscontract.RenditionTranslation{RenditionKey: rendition.Key}
		if rendition.TranslationCredits != nil {
			item.TranslationCredit = rendition.TranslationCredits.Translation
			item.ProofreadingCredit = rendition.TranslationCredits.Proofreading
			if item.TranslationCredit == "" && item.ProofreadingCredit == "" {
				return nil, &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"translationCredits must not be empty"}}
			}
		}
		if !utf8.ValidString(item.TranslationCredit) || !utf8.ValidString(item.ProofreadingCredit) ||
			validateRequestedLocalization && (item.TranslationCredit != strings.TrimSpace(item.TranslationCredit) ||
				item.ProofreadingCredit != strings.TrimSpace(item.ProofreadingCredit) ||
				len(item.TranslationCredit) > maxLyricsRenditionCreditBytes ||
				len(item.ProofreadingCredit) > maxLyricsRenditionCreditBytes) {
			return nil, &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"rendition credits must be trim-stable and within the 2048-byte public limit"}}
		}
		totalBytes += len(item.TranslationCredit) + len(item.ProofreadingCredit)
		var target, currentTarget *PublicLyricsV3Side
		if rendition.Full != nil {
			target, currentTarget = rendition.Full, currentRendition.Full
		} else {
			target, currentTarget = rendition.Game, currentRendition.Game
		}
		if target == nil || currentTarget == nil || len(target.Lines) != len(currentTarget.Lines) {
			return nil, &LyricsRenditionContractError{Code: "source_drift", Details: []string{"authoritative rendition side changed"}}
		}
		item.Translations = make([]string, len(target.Lines))
		for lineIndex, line := range target.Lines {
			if len(line.Chinese) > maxLyricsLineTextBytes || !utf8.ValidString(line.Chinese) {
				return nil, &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"rendition translation exceeds the safe per-line limit"}}
			}
			item.Translations[lineIndex] = line.Chinese
			totalBytes += len(line.Chinese)
			if line.Chinese != "" {
				hasLocalization = true
			}
		}
		if item.TranslationCredit != "" || item.ProofreadingCredit != "" {
			hasLocalization = true
		}
		if rendition.Full != nil && rendition.Game != nil && rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
			fullByID := make(map[string]string, len(rendition.Full.Lines))
			for _, line := range rendition.Full.Lines {
				fullByID[line.ID] = line.Chinese
			}
			if len(rendition.Relation.LineIDs) != len(rendition.Game.Lines) {
				return nil, &LyricsRenditionContractError{Code: "invalid_game_projection", Details: []string{"exact projection line count changed"}}
			}
			for lineIndex, lineID := range rendition.Relation.LineIDs {
				translation, found := fullByID[lineID]
				if !found || rendition.Game.Lines[lineIndex].Chinese != translation {
					return nil, &LyricsRenditionContractError{Code: "invalid_game_projection", Details: []string{"exact projection Game translation must follow its Full line IDs"}}
				}
			}
		}
		result[index] = item
	}
	if validateRequestedLocalization && totalBytes > maxLyricsDocumentBytes {
		return nil, &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"rendition localizations exceed the safe document size"}}
	}
	if !hasLocalization {
		return nil, nil
	}
	return result, nil
}

func lyricsRenditionEditorSideTranslations(input LyricsRenditionDocument) (map[string]map[string][]string, int, error) {
	result := map[string]map[string][]string{}
	totalBytes := 0
	for _, rendition := range input.Renditions {
		if rendition.Full == nil || rendition.Game == nil || rendition.Relation.Kind == model.LyricsSourceRenditionRelationExactProjection {
			continue
		}
		lines := make([]string, len(rendition.Game.Lines))
		hasTranslation := false
		for index, line := range rendition.Game.Lines {
			if len(line.Chinese) > maxLyricsLineTextBytes || !utf8.ValidString(line.Chinese) {
				return nil, 0, &LyricsRenditionContractError{Code: "segment_mismatch", Details: []string{"rendition Game translation exceeds the safe per-line limit"}}
			}
			lines[index] = line.Chinese
			totalBytes += len(line.Chinese)
			hasTranslation = hasTranslation || line.Chinese != ""
		}
		if hasTranslation {
			result[rendition.Key] = map[string][]string{"game": lines}
		}
	}
	return result, totalBytes, nil
}

func equalLyricsRenditionSideTranslations(left, right map[string]map[string][]string) bool {
	return reflect.DeepEqual(left, right)
}

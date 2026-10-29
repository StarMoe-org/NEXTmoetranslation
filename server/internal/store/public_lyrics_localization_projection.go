package store

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"

	"moesekai/server/internal/model"
)

// PublishedLyricsLocalizationProjection returns the public index entries and
// validated v3 detail documents for source-v3 songs whose rendition
// localizations were edited after the recovery batch (localization revision >
// 1) and carry a translation credit. Songs with a legacy publication row are
// excluded: the legacy publication snapshot owns them. Individual songs that
// cannot be materialized or validated are skipped and logged; the caller keeps
// serving the reviewed bundle for those songs. A systemic query failure is
// returned so the caller can fail closed to the bundle.
//
// Edited rendition documents are cached per music ID and revision so frequent
// public rebuilds do not reload the full source bundle for every untouched
// song: only songs edited since the previous rebuild pay the document build.
type projectedLocalizationCandidate struct {
	musicID    int
	documentID int64
	revision   int
	updatedAt  int64
	title      model.LocalizedTitle
}

type projectedLocalizationEntry struct {
	revision          int
	skip              bool
	detail            PublicLyricsV3DetailDocument
	hasV4             bool
	v4Detail          PublicLyricsV4DetailDocument
	availableVersions []string
}

func (s *Store) PublishedLyricsLocalizationProjection() ([]PublicLyricsIndexSong, map[int]PublicLyricsV3DetailDocument, map[int]PublicLyricsV4DetailDocument, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`WITH edited AS (
		SELECT document_id,
			MAX(revision) AS localization_revision,
			MAX(updated_at) AS localization_updated_at
		FROM song_lyrics_rendition_localizations
		GROUP BY document_id
		HAVING MAX(revision) > 1
	)
	SELECT d.music_id, d.document_id,
		edited.localization_revision, edited.localization_updated_at,
		m.title_ja, COALESCE(NULLIF(zh.cn_text,''), m.title_zh), COALESCE(NULLIF(en.text,''), m.title_en)
		FROM edited
		JOIN song_lyrics_source_documents d ON d.document_id=edited.document_id
		JOIN catalog_music m ON m.music_id=d.music_id
		LEFT JOIN entries zh ON zh.category='music' AND zh.field='title' AND zh.jp_key=m.title_ja
		LEFT JOIN entry_localizations en ON en.category='music' AND en.field='title'
		 AND en.jp_key=m.title_ja AND en.locale='en-US'
		LEFT JOIN song_lyrics_publications p ON p.music_id=d.music_id
		WHERE p.music_id IS NULL
		ORDER BY d.music_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	var candidates []projectedLocalizationCandidate
	for rows.Next() {
		var musicID, documentID, localizationRevision, localizationUpdatedAt int64
		var title model.LocalizedTitle
		if err := rows.Scan(&musicID, &documentID, &localizationRevision, &localizationUpdatedAt,
			&title.Japanese, &title.Chinese, &title.English); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		if localizationRevision <= 1 {
			continue
		}
		candidates = append(candidates, projectedLocalizationCandidate{
			musicID:    int(musicID),
			documentID: documentID,
			revision:   int(localizationRevision),
			updatedAt:  localizationUpdatedAt,
			title:      title,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, err
	}

	index := make([]PublicLyricsIndexSong, 0, len(candidates))
	details := make(map[int]PublicLyricsV3DetailDocument, len(candidates))
	v4Details := make(map[int]PublicLyricsV4DetailDocument)
	for _, candidate := range candidates {
		projected, ok := s.projectionEntry(tx, candidate.musicID, candidate.revision)
		if !ok || projected.skip {
			continue
		}
		entry := PublicLyricsIndexSong{
			MusicID:           candidate.musicID,
			Revision:          projected.detail.Revision,
			UpdatedAt:         projected.detail.UpdatedAt,
			State:             projected.detail.State,
			Title:             candidate.title,
			AvailableVersions: projected.availableVersions,
		}
		if !validLocalizationIndexEntry(entry) {
			log.Printf("[projection] lyrics localization %d index entry invalid; skipping", candidate.musicID)
			continue
		}
		index = append(index, entry)
		details[candidate.musicID] = projected.detail
		if projected.hasV4 {
			v4Details[candidate.musicID] = projected.v4Detail
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, err
	}
	return index, details, v4Details, nil
}

func (s *Store) buildProjectedV4Detail(tx *sql.Tx, musicID int, v3 PublicLyricsV3DetailDocument) (PublicLyricsV4DetailDocument, bool, error) {
	bundle, err := loadLyricsRenditionEditorBundle(tx, musicID)
	if err != nil {
		return PublicLyricsV4DetailDocument{}, false, err
	}
	selection, err := loadLyricsTranslationEditionSelection(tx, bundle, "", false)
	if err != nil {
		return PublicLyricsV4DetailDocument{}, false, err
	}
	return s.buildProjectedV4DetailFromBundle(tx, bundle, selection, v3)
}

func (s *Store) buildProjectedV4DetailFromBundle(
	tx *sql.Tx,
	bundle lyricsRenditionEditorBundle,
	selection lyricsTranslationEditionSelection,
	v3 PublicLyricsV3DetailDocument,
) (PublicLyricsV4DetailDocument, bool, error) {
	if len(selection.editions) <= 1 {
		return PublicLyricsV4DetailDocument{}, false, nil
	}
	result := PublicLyricsV4DetailDocument{
		Version:                      4,
		MusicID:                      v3.MusicID,
		Revision:                     v3.Revision,
		UpdatedAt:                    v3.UpdatedAt,
		State:                        v3.State,
		DefaultTranslationEditionKey: selection.defaultKey,
		TranslationEditions:          make([]PublicLyricsV4TranslationEdition, len(selection.editions)),
		Renditions:                   make([]PublicLyricsV4Rendition, len(v3.Renditions)),
	}
	for index, rendition := range v3.Renditions {
		result.Renditions[index] = publicLyricsV4SourceRendition(rendition)
	}
	for editionIndex, summary := range selection.editions {
		var edSelection lyricsTranslationEditionSelection
		if summary.Key == selection.key {
			edSelection = selection
		} else {
			var err error
			edSelection, err = loadLyricsTranslationEditionSelection(tx, bundle, summary.Key, true)
			if err != nil {
				return PublicLyricsV4DetailDocument{}, false, err
			}
		}
		edDoc, err := buildLyricsTranslationEditionDocument(bundle, edSelection)
		if err != nil {
			return PublicLyricsV4DetailDocument{}, false, err
		}
		editionItem := PublicLyricsV4TranslationEdition{
			Key:        summary.Key,
			Label:      summary.Label,
			Renditions: make([]PublicLyricsV4EditionRendition, len(edDoc.Renditions)),
		}
		for renditionIndex, rendition := range edDoc.Renditions {
			editionRendition := PublicLyricsV4EditionRendition{
				RenditionKey: rendition.Key,
			}
			if rendition.TranslationCredits != nil &&
				(rendition.TranslationCredits.Translation != "" || rendition.TranslationCredits.Proofreading != "") {
				editionRendition.TranslationCredits = rendition.TranslationCredits
			}
			if rendition.Full != nil {
				editionRendition.Full = &PublicLyricsV4TranslationSide{
					Translations: publicLyricsV4SideTranslations(rendition.Full),
				}
			}
			if rendition.Game != nil && rendition.Relation.Kind != "exact_projection" {
				editionRendition.Game = &PublicLyricsV4TranslationSide{
					Translations: publicLyricsV4SideTranslations(rendition.Game),
				}
			}
			editionItem.Renditions[renditionIndex] = editionRendition
		}
		result.TranslationEditions[editionIndex] = editionItem
	}
	if err := validatePublicLyricsV4Detail(result); err != nil {
		return PublicLyricsV4DetailDocument{}, false, err
	}
	return result, true, nil
}

// projectionEntry returns the validated projected localization entry for the
// given localization revision inside tx, reusing the cached entry when the
// revision is unchanged since the previous public rebuild.
func (s *Store) projectionEntry(tx *sql.Tx, musicID, revision int) (projectedLocalizationEntry, bool) {
	s.localizationProjectionMu.RLock()
	cached, ok := s.localizationProjectionCache[musicID]
	s.localizationProjectionMu.RUnlock()
	if ok && cached.revision == revision {
		return cached, true
	}

	bundle, err := loadLyricsRenditionEditorBundle(tx, musicID)
	if err != nil {
		if !errors.Is(err, ErrLyricsNotFound) {
			log.Printf("[projection] lyrics localization %d unavailable; skipping: %v", musicID, err)
		}
		return projectedLocalizationEntry{}, false
	}
	selection, err := loadLyricsTranslationEditionSelection(tx, bundle, "", false)
	if err != nil {
		log.Printf("[projection] lyrics localization %d unavailable; skipping: %v", musicID, err)
		return projectedLocalizationEntry{}, false
	}
	document, err := buildLyricsTranslationEditionDocument(bundle, selection)
	if err != nil {
		log.Printf("[projection] lyrics localization %d unavailable; skipping: %v", musicID, err)
		return projectedLocalizationEntry{}, false
	}
	if document.Revision <= 1 || !lyricsRenditionHasLocalizationCredit(document) {
		entry := projectedLocalizationEntry{revision: document.Revision, skip: true}
		s.storeProjectionEntry(musicID, entry)
		return entry, true
	}
	state := PublicLyricsStateGameOnly
	for _, rendition := range document.Renditions {
		if rendition.Full != nil {
			state = PublicLyricsStateComplete
			break
		}
	}
	detail := PublicLyricsV3DetailDocument{
		Version: 3, MusicID: musicID, Revision: document.Revision,
		UpdatedAt: document.UpdatedAt, State: state,
		Renditions: document.Renditions,
	}
	if err := validatePublicLyricsV3Detail(detail); err != nil {
		log.Printf("[projection] lyrics localization %d invalid; skipping: %v", musicID, err)
		return projectedLocalizationEntry{}, false
	}
	built := projectedLocalizationEntry{
		revision:          document.Revision,
		detail:            detail,
		availableVersions: publicV3AvailableVersions(detail),
	}
	if len(selection.editions) > 1 {
		v4Detail, v4OK, v4Err := s.buildProjectedV4DetailFromBundle(tx, bundle, selection, detail)
		if v4Err != nil {
			log.Printf("[projection] lyrics localization %d v4 detail build failed: %v", musicID, v4Err)
		} else if v4OK {
			built.hasV4 = true
			built.v4Detail = v4Detail
		}
	}
	s.storeProjectionEntry(musicID, built)
	return built, true
}

func (s *Store) storeProjectionEntry(musicID int, entry projectedLocalizationEntry) {
	const maxLocalizationProjectionCacheEntries = 1024
	s.localizationProjectionMu.Lock()
	if s.localizationProjectionCache == nil || len(s.localizationProjectionCache) >= maxLocalizationProjectionCacheEntries {
		s.localizationProjectionCache = make(map[int]projectedLocalizationEntry)
	}
	s.localizationProjectionCache[musicID] = entry
	s.localizationProjectionMu.Unlock()
}

// invalidateLocalizationProjectionCache drops every cached rendition document.
// A restore can replace the text of a song while keeping its localization
// revision, which the revision-keyed cache alone cannot detect.
func (s *Store) invalidateLocalizationProjectionCache() {
	s.localizationProjectionMu.Lock()
	s.localizationProjectionCache = nil
	s.localizationProjectionMu.Unlock()
}

// lyricsRenditionHasLocalizationCredit reports whether any rendition of an
// edited source-v3 document declares a translation or proofreading credit.
func lyricsRenditionHasLocalizationCredit(document LyricsRenditionDocument) bool {
	for _, rendition := range document.Renditions {
		if rendition.TranslationCredits != nil &&
			(rendition.TranslationCredits.Translation != "" || rendition.TranslationCredits.Proofreading != "") {
			return true
		}
	}
	return false
}

// validLocalizationIndexEntry applies the index-level contract checks for a
// projected localization entry without requiring a whole index document.
func validLocalizationIndexEntry(song PublicLyricsIndexSong) bool {
	return song.MusicID > 0 && song.Revision > 0 && canonicalPublicV3Timestamp(song.UpdatedAt) &&
		song.Title.Japanese != "" && song.Title.Japanese == strings.TrimSpace(song.Title.Japanese) &&
		(song.State == PublicLyricsStateComplete || song.State == PublicLyricsStateGameOnly) &&
		len(song.AvailableVersions) > 0 && len(song.AvailableVersions) <= 2
}

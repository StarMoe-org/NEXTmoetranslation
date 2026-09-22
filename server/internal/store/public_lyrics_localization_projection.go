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
func (s *Store) PublishedLyricsLocalizationProjection() ([]PublicLyricsIndexSong, map[int]PublicLyricsV3DetailDocument, map[int]PublicLyricsV4DetailDocument, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT d.music_id, d.document_id,
		MAX(l.revision) AS localization_revision, MAX(l.updated_at) AS localization_updated_at,
		m.title_ja, COALESCE(NULLIF(zh.cn_text,''), m.title_zh), COALESCE(NULLIF(en.text,''), m.title_en)
		FROM song_lyrics_source_documents d
		JOIN song_lyrics_rendition_localizations l ON l.document_id=d.document_id
		JOIN catalog_music m ON m.music_id=d.music_id
		LEFT JOIN entries zh ON zh.category='music' AND zh.field='title' AND zh.jp_key=m.title_ja
		LEFT JOIN entry_localizations en ON en.category='music' AND en.field='title'
		 AND en.jp_key=m.title_ja AND en.locale='en-US'
		LEFT JOIN song_lyrics_publications p ON p.music_id=d.music_id
		WHERE p.music_id IS NULL
		GROUP BY d.document_id, d.music_id, m.title_ja, zh.cn_text, m.title_zh, en.text, m.title_en
		HAVING MAX(l.revision) > 1
		ORDER BY d.music_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	index := []PublicLyricsIndexSong{}
	details := make(map[int]PublicLyricsV3DetailDocument)
	v4Details := make(map[int]PublicLyricsV4DetailDocument)
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
		document, err := s.projectionRenditionDocument(int(musicID), int(localizationRevision))
		if err != nil {
			if !errors.Is(err, ErrLyricsNotFound) {
				log.Printf("[projection] lyrics localization %d unavailable; skipping: %v", musicID, err)
			}
			continue
		}
		if document.Revision <= 1 || !lyricsRenditionHasLocalizationCredit(document) {
			continue
		}
		state := PublicLyricsStateGameOnly
		for _, rendition := range document.Renditions {
			if rendition.Full != nil {
				state = PublicLyricsStateComplete
				break
			}
		}
		detail := PublicLyricsV3DetailDocument{
			Version: 3, MusicID: int(musicID), Revision: document.Revision,
			UpdatedAt: document.UpdatedAt, State: state,
			Renditions: document.Renditions,
		}
		if err := validatePublicLyricsV3Detail(detail); err != nil {
			log.Printf("[projection] lyrics localization %d invalid; skipping: %v", musicID, err)
			continue
		}
		if len(document.TranslationEditions) > 1 {
			v4Detail, ok, v4Err := s.buildProjectedV4Detail(tx, int(musicID), detail)
			if v4Err != nil {
				log.Printf("[projection] lyrics localization %d v4 detail build failed: %v", musicID, v4Err)
			} else if ok {
				v4Details[int(musicID)] = v4Detail
			}
		}
		entry := PublicLyricsIndexSong{
			MusicID: int(musicID), Revision: document.Revision, UpdatedAt: document.UpdatedAt,
			State: state, Title: title, AvailableVersions: publicV3AvailableVersions(detail),
		}
		if !validLocalizationIndexEntry(entry) {
			log.Printf("[projection] lyrics localization %d index entry invalid; skipping", musicID)
			continue
		}
		index = append(index, entry)
		details[int(musicID)] = detail
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, nil, err
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
		edSelection, err := loadLyricsTranslationEditionSelection(tx, bundle, summary.Key, true)
		if err != nil {
			return PublicLyricsV4DetailDocument{}, false, err
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

// projectionRenditionDocument returns the rendition document for the given
// localization revision, reusing the cached document when the revision is
// unchanged since the previous public rebuild.
func (s *Store) projectionRenditionDocument(musicID, revision int) (LyricsRenditionDocument, error) {
	s.localizationProjectionMu.RLock()
	cached, ok := s.localizationProjectionCache[musicID]
	s.localizationProjectionMu.RUnlock()
	if ok && cached.Revision == revision {
		return cached, nil
	}
	document, err := s.GetLyricsRenditionDocument(musicID)
	if err != nil {
		return LyricsRenditionDocument{}, err
	}
	s.localizationProjectionMu.Lock()
	if s.localizationProjectionCache == nil {
		s.localizationProjectionCache = make(map[int]LyricsRenditionDocument)
	}
	s.localizationProjectionCache[musicID] = document
	s.localizationProjectionMu.Unlock()
	return document, nil
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

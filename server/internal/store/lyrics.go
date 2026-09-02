package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"moesekai/server/internal/model"
)

var (
	ErrLyricsNotFound     = errors.New("lyrics not found")
	ErrLyricsAlreadySaved = errors.New("lyrics already saved")
)

const (
	maxLyricsLines                = 5000
	maxLyricsSegmentsPerLine      = 100
	maxLyricsRubyPerSegment       = 256
	maxLyricsPerformers           = 64
	maxLyricsLineTextBytes        = 16 << 10
	maxLyricsMetadataBytes        = 16 << 10
	maxLyricsRenditionCreditBytes = 2 << 10
	maxLyricsURLBytes             = 2 << 10
	maxLyricsDocumentBytes        = 4 << 20
)

var managedLyricsSourceHosts = map[string]struct{}{
	"vocaloid.fandom.com": {},
	"vocaloid.wikia.com":  {},
	"www.sekaipedia.org":  {},
}

type lyricsSaveMode uint8

const (
	lyricsSaveOrdinary lyricsSaveMode = iota
	lyricsSaveVerifiedImport
)

type LyricsContractError struct {
	Code    string
	Details []string
	Current *model.SongLyrics
}

func (e *LyricsContractError) Error() string { return e.Code }

type storedLyrics struct {
	lyrics     model.SongLyrics
	sourceHash string
}

func (s *Store) GetLyrics(musicID int) (model.SongLyrics, error) {
	return s.getLyricsSnapshot(musicID, nil)
}

// getLyricsSnapshot assembles every part of a lyrics document from one
// read-only SQLite transaction. The optional hook exists for deterministic
// concurrency tests and runs after the document header establishes the
// snapshot but before line and segment reads.
func (s *Store) getLyricsSnapshot(musicID int, afterHeader func()) (model.SongLyrics, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.SongLyrics{}, err
	}
	defer tx.Rollback()
	stored, err := s.loadLyricsWithHook(tx, musicID, afterHeader)
	if err != nil {
		return model.SongLyrics{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.SongLyrics{}, err
	}
	return stored.lyrics, nil
}

// WithLyricsFirstSaveEligibility serializes a fast callback with every
// save/publish/unpublish mutation for the same music. It is used to issue a
// first-save import capability without a check-to-issue race.
func (s *Store) WithLyricsFirstSaveEligibility(musicID int, issue func() error) error {
	unlock := s.lockLyrics(musicID)
	defer unlock()
	var count int
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM song_lyrics WHERE music_id=?)+
		(SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=?)`, musicID, musicID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrLyricsAlreadySaved
	}
	return issue()
}

func (s *Store) ListLyrics(limit, cursor int) (model.LyricsListResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT music_id,revision,updated_at,published_revision FROM (
		SELECT l.music_id AS music_id,l.revision AS revision,l.updated_at AS updated_at,p.revision AS published_revision
		FROM song_lyrics AS l LEFT JOIN song_lyrics_publications AS p ON p.music_id=l.music_id
		UNION ALL
		SELECT source.music_id AS music_id,
			COALESCE(MAX(localization.revision),1) AS revision,
			COALESCE(MAX(localization.updated_at),source.created_at) AS updated_at,
			NULL AS published_revision
		FROM song_lyrics_source_documents AS source
		LEFT JOIN song_lyrics_rendition_localizations AS localization ON localization.document_id=source.document_id
		WHERE source.schema_version=? AND NOT EXISTS (
			SELECT 1 FROM song_lyrics AS editable WHERE editable.music_id=source.music_id
		)
		GROUP BY source.document_id,source.music_id,source.created_at
	) WHERE music_id>? ORDER BY music_id LIMIT ?`, model.LyricsSourceDocumentSchemaVersionV3, cursor, limit+1)
	if err != nil {
		return model.LyricsListResponse{}, err
	}
	defer rows.Close()
	response := model.LyricsListResponse{Items: []model.LyricsListItem{}}
	for rows.Next() {
		var item model.LyricsListItem
		var updatedAt int64
		var published sql.NullInt64
		if err := rows.Scan(&item.MusicID, &item.Revision, &updatedAt, &published); err != nil {
			return model.LyricsListResponse{}, err
		}
		item.Status = "draft"
		if published.Valid {
			item.PublishedRevision = int(published.Int64)
			item.Status = "draft-published"
			if published.Int64 == int64(item.Revision) {
				item.Status = "published"
			}
		}
		item.UpdatedAt = formatTimestamp(updatedAt)
		response.Items = append(response.Items, item)
	}
	if err := rows.Err(); err != nil {
		return model.LyricsListResponse{}, err
	}
	if len(response.Items) > limit {
		response.NextCursor = itoa(response.Items[limit-1].MusicID)
		response.Items = response.Items[:limit]
	}
	return response, nil
}

func (s *Store) SaveLyrics(input model.SongLyrics, user string) (model.SongLyrics, error) {
	lyrics, _, err := s.SaveLyricsMutation(input, user)
	return lyrics, err
}

// SaveLyricsMutation reports whether the successful call committed a change.
func (s *Store) SaveLyricsMutation(input model.SongLyrics, user string) (model.SongLyrics, bool, error) {
	return s.saveLyricsMutation(input, user, lyricsSaveOrdinary, nil, nil)
}

// SaveLyricsMutationWithBeforeCommit runs beforeCommit inside the same SQLite
// transaction as the authoritative lyrics mutation. The callback receives the
// final document that will be returned and whether authoritative content
// changed. Returning an error rolls back both the save and callback writes.
func (s *Store) SaveLyricsMutationWithBeforeCommit(
	input model.SongLyrics,
	user string,
	beforeCommit func(*sql.Tx, model.SongLyrics, bool) error,
) (model.SongLyrics, bool, error) {
	return s.saveLyricsMutation(input, user, lyricsSaveOrdinary, nil, beforeCommit)
}

// SaveImportedLyricsMutation permits complete source provenance on the first
// save for trusted internal callers and compatibility tests. All
// provenance-bearing saves require the canonical lowercase 40-hex MediaWiki
// SHA1 representation.
func (s *Store) SaveImportedLyricsMutation(input model.SongLyrics, user string) (model.SongLyrics, bool, error) {
	return s.saveLyricsMutation(input, user, lyricsSaveVerifiedImport, nil, nil)
}

// SaveImportedLyricsMutationWithCommit runs afterCommit immediately after the
// first-save transaction commits and before synchronous change notifications.
// The callback must be fast and must not call back into lyrics mutations.
func (s *Store) SaveImportedLyricsMutationWithCommit(input model.SongLyrics, user string, afterCommit func()) (model.SongLyrics, bool, error) {
	return s.saveLyricsMutation(input, user, lyricsSaveVerifiedImport, afterCommit, nil)
}

func (s *Store) saveLyricsMutation(
	input model.SongLyrics,
	user string,
	mode lyricsSaveMode,
	afterCommit func(),
	beforeCommit func(*sql.Tx, model.SongLyrics, bool) error,
) (model.SongLyrics, bool, error) {
	unlock := s.lockLyrics(input.MusicID)
	defer unlock()
	return s.saveLyricsMutationLocked(input, user, mode, afterCommit, beforeCommit, nil)
}

func prepareLyricsMutationInput(input model.SongLyrics) (model.SongLyrics, model.SongLyrics, int64, error) {
	requested := cloneEditableLyricsRuby(input)
	sort.SliceStable(requested.Lines, func(i, j int) bool { return requested.Lines[i].Order < requested.Lines[j].Order })
	normalized := normalizeEditableLyricsRuby(requested)
	sourceFetchedAt, err := validateLyricsProvenance(normalized)
	if err != nil {
		return model.SongLyrics{}, model.SongLyrics{}, 0, err
	}
	return requested, normalized, sourceFetchedAt, nil
}

func rejectLegacyLyricsMutationForSourceV3(q queryRower, musicID int) error {
	var count int
	if err := q.QueryRow(`SELECT COUNT(*) FROM song_lyrics_source_documents WHERE music_id=? AND schema_version=?`,
		musicID, model.LyricsSourceDocumentSchemaVersionV3).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return &LyricsContractError{Code: "source_drift", Details: []string{
			"source-v3 lyrics are owned by the plural rendition editor and cannot use the legacy mutation route",
		}}
	}
	return nil
}

func (s *Store) saveLyricsMutationLocked(
	input model.SongLyrics,
	user string,
	mode lyricsSaveMode,
	afterCommit func(),
	beforeCommit func(*sql.Tx, model.SongLyrics, bool) error,
	prepareFirstSave func(*sql.Tx, *model.SongLyrics) error,
) (model.SongLyrics, bool, error) {
	var requested, normalized model.SongLyrics
	var sourceFetchedAt int64
	var err error
	if prepareFirstSave == nil {
		requested, normalized, sourceFetchedAt, err = prepareLyricsMutationInput(input)
		if err != nil {
			return model.SongLyrics{}, false, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	defer tx.Rollback()
	if prepareFirstSave != nil {
		if err := prepareFirstSave(tx, &input); err != nil {
			return model.SongLyrics{}, false, err
		}
		requested, normalized, sourceFetchedAt, err = prepareLyricsMutationInput(input)
		if err != nil {
			return model.SongLyrics{}, false, err
		}
	}
	if err := rejectLegacyLyricsMutationForSourceV3(tx, normalized.MusicID); err != nil {
		return model.SongLyrics{}, false, err
	}
	exists, err := s.catalogMusicExists(tx, normalized.MusicID)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	if !exists {
		return model.SongLyrics{}, false, &LyricsContractError{Code: "source_drift", Details: []string{"musicId is not present in the server catalog"}}
	}
	validPerformers, err := s.performerIDs(tx)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	code, details, sourceHash := validateLyrics(normalized, validPerformers, false)
	if code != "" {
		return model.SongLyrics{}, false, &LyricsContractError{Code: code, Details: details}
	}

	current, loadErr := s.loadLyrics(tx, normalized.MusicID)
	if loadErr != nil && loadErr != ErrLyricsNotFound {
		return model.SongLyrics{}, false, loadErr
	}
	if loadErr == ErrLyricsNotFound {
		if normalized.Revision != 0 {
			return model.SongLyrics{}, false, &LyricsContractError{Code: "revision_conflict"}
		}
		if mode == lyricsSaveOrdinary {
			if sourceFetchedAt > 0 {
				return model.SongLyrics{}, false, &LyricsContractError{
					Code: "source_drift", Details: []string{"new source provenance requires a verified server preview"},
				}
			}
			if isManagedLyricsSourceURL(normalized.SourceURL) {
				return model.SongLyrics{}, false, &LyricsContractError{
					Code: "source_drift", Details: []string{"the managed lyrics source requires a verified server preview"},
				}
			}
		}
	} else {
		if mode == lyricsSaveVerifiedImport && (current.lyrics.Status != "draft" || current.lyrics.SourceURL != "") {
			return model.SongLyrics{}, false, &LyricsContractError{
				Code: "source_drift", Details: []string{"verified source previews may only be used for the first save of a new lyrics document"},
			}
		}
		if normalized.Revision != current.lyrics.Revision {
			copy := current.lyrics
			return model.SongLyrics{}, false, &LyricsContractError{Code: "revision_conflict", Current: &copy}
		}
		if lyricsProvenanceChanged(normalized, current.lyrics) {
			return model.SongLyrics{}, false, &LyricsContractError{
				Code: "source_drift", Details: []string{"source page, revision, SHA1, fetched timestamp, and URL are immutable after first save"},
			}
		}
		if mode == lyricsSaveOrdinary && (lyricsSourceStructureChanged(normalized.Lines, current.lyrics.Lines) || sourceHash != current.sourceHash) {
			if !(current.lyrics.Status == "draft" && current.lyrics.SourceURL == "") {
				return model.SongLyrics{}, false, &LyricsContractError{
					Code: "source_drift", Details: []string{"ordered line IDs, numeric order values, or Japanese source text changed"},
				}
			}
		}
		inheritedRuby, missingRubyDetails := preserveOmittedLyricsRuby(&normalized, requested, current.lyrics)
		if len(missingRubyDetails) > 0 {
			return model.SongLyrics{}, false, &LyricsContractError{Code: "segment_mismatch", Details: missingRubyDetails}
		}
		if inheritedRuby {
			if code, details, _ := validateLyrics(normalized, validPerformers, false); code != "" {
				return model.SongLyrics{}, false, &LyricsContractError{Code: code, Details: details}
			}
		}
		if sameLyricsContent(normalized, current.lyrics) {
			if beforeCommit != nil {
				if err := beforeCommit(tx, current.lyrics, false); err != nil {
					return model.SongLyrics{}, false, err
				}
				if err := tx.Commit(); err != nil {
					return model.SongLyrics{}, false, err
				}
			}
			return current.lyrics, false, nil
		}
	}

	nextRevision := 1
	if loadErr == nil {
		nextRevision = current.lyrics.Revision + 1
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO song_lyrics
		(music_id, revision, updated_at, updated_by, attribution, translation_credit, proofreading_credit,
		 source_note, source_url, license_note, source_hash, source_page_id, source_revision_id, source_sha1,
		 source_fetched_at, source_fetched_at_rfc3339)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(music_id) DO UPDATE SET revision=excluded.revision, updated_at=excluded.updated_at,
		updated_by=excluded.updated_by, attribution=excluded.attribution,
		translation_credit=excluded.translation_credit, proofreading_credit=excluded.proofreading_credit,
		source_note=excluded.source_note, source_url=excluded.source_url,
		license_note=excluded.license_note, source_hash=excluded.source_hash,
		 source_page_id=excluded.source_page_id, source_revision_id=excluded.source_revision_id,
		 source_sha1=excluded.source_sha1, source_fetched_at=excluded.source_fetched_at,
		 source_fetched_at_rfc3339=excluded.source_fetched_at_rfc3339`,
		normalized.MusicID, nextRevision, now, user, normalized.Attribution, normalized.TranslationCredit,
		normalized.ProofreadingCredit, normalized.SourceNote, normalized.SourceURL, normalized.LicenseNote,
		sourceHash, normalized.SourcePageID, normalized.SourceRevisionID, normalized.SourceSHA1,
		sourceFetchedAt, normalized.SourceFetchedAt); err != nil {
		return model.SongLyrics{}, false, err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyric_lines WHERE music_id=?`, normalized.MusicID); err != nil {
		return model.SongLyrics{}, false, err
	}
	for _, line := range normalized.Lines {
		stanzaBreak := 0
		if line.StanzaBreakBefore {
			stanzaBreak = 1
		}
		if _, err := tx.Exec(`INSERT INTO song_lyric_lines
			(music_id, line_id, position, japanese, zh_cn, en_us, stanza_break_before) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			normalized.MusicID, line.ID, line.Order, line.Japanese, line.Chinese, line.English, stanzaBreak); err != nil {
			return model.SongLyrics{}, false, err
		}
		for position, segment := range line.Segments {
			performersJSON, _ := json.Marshal(segment.PerformerIDs)
			rubyJSON, _ := json.Marshal(segment.Ruby)
			if _, err := tx.Exec(`INSERT INTO song_lyric_segments
				(music_id, line_id, position, text, performer_ids_json, ruby_json) VALUES (?, ?, ?, ?, ?, ?)`,
				normalized.MusicID, line.ID, position, segment.Text, string(performersJSON), string(rubyJSON)); err != nil {
				return model.SongLyrics{}, false, err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts, user, action, detail) VALUES (?, ?, 'lyrics.save', ?)`,
		now, user, fmt.Sprintf("musicId=%d revision=%d", normalized.MusicID, nextRevision)); err != nil {
		return model.SongLyrics{}, false, err
	}
	normalized.Status = "draft"
	if loadErr == nil && current.lyrics.PublishedRevision > 0 {
		normalized.Status = "draft-published"
		normalized.PublishedRevision = current.lyrics.PublishedRevision
	}
	normalized.Revision = nextRevision
	normalized.UpdatedAt = formatTimestamp(now)
	if beforeCommit != nil {
		if err := beforeCommit(tx, normalized, true); err != nil {
			return model.SongLyrics{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.SongLyrics{}, false, err
	}
	if afterCommit != nil {
		afterCommit()
	}
	s.NotifyChange()
	return normalized, true, nil
}

func (s *Store) PublishLyrics(musicID, revision int, users ...string) (model.SongLyrics, error) {
	lyrics, _, err := s.PublishLyricsMutation(musicID, revision, users...)
	return lyrics, err
}

// PublishLyricsMutation reports whether the successful call committed a change.
func (s *Store) PublishLyricsMutation(musicID, revision int, users ...string) (model.SongLyrics, bool, error) {
	unlock := s.lockLyrics(musicID)
	defer unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	defer tx.Rollback()
	if err := rejectLegacyLyricsMutationForSourceV3(tx, musicID); err != nil {
		return model.SongLyrics{}, false, err
	}
	current, err := s.loadLyrics(tx, musicID)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	if revision != current.lyrics.Revision {
		copy := current.lyrics
		return model.SongLyrics{}, false, &LyricsContractError{Code: "revision_conflict", Current: &copy}
	}
	validPerformers, err := s.performerIDs(tx)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	public, err := s.publicLyricsPublication(tx, current.lyrics, validPerformers)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	payload, err := json.Marshal(public)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	var existingRevision int
	var existingPayload string
	err = tx.QueryRow(`SELECT revision,payload_json FROM song_lyrics_publications WHERE music_id=?`, musicID).
		Scan(&existingRevision, &existingPayload)
	if err == nil && existingRevision == revision {
		if existingPayload != string(payload) {
			return model.SongLyrics{}, false, &LyricsContractError{Code: "incomplete_publication", Details: []string{
				"existing publication payload does not match the source-backed public contract",
			}}
		}
		current.lyrics.Status = "published"
		current.lyrics.PublishedRevision = revision
		return current.lyrics, false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return model.SongLyrics{}, false, err
	}
	updatedAt, err := parseTimestamp(current.lyrics.UpdatedAt)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO song_lyrics_publications(music_id, revision, updated_at, payload_json)
		VALUES (?, ?, ?, ?) ON CONFLICT(music_id) DO UPDATE SET revision=excluded.revision,
		updated_at=excluded.updated_at, payload_json=excluded.payload_json`,
		musicID, revision, updatedAt, string(payload)); err != nil {
		return model.SongLyrics{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts, user, action, detail) VALUES (?, ?, 'lyrics.publish', ?)`,
		time.Now().Unix(), optionalActor(users), fmt.Sprintf("musicId=%d revision=%d", musicID, revision)); err != nil {
		return model.SongLyrics{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.SongLyrics{}, false, err
	}
	current.lyrics.Status = "published"
	current.lyrics.PublishedRevision = revision
	s.NotifyChange()
	return current.lyrics, true, nil
}

func (s *Store) UnpublishLyrics(musicID, revision int, users ...string) (model.SongLyrics, error) {
	lyrics, _, err := s.UnpublishLyricsMutation(musicID, revision, users...)
	return lyrics, err
}

// UnpublishLyricsMutation reports whether the successful call committed a change.
func (s *Store) UnpublishLyricsMutation(musicID, revision int, users ...string) (model.SongLyrics, bool, error) {
	unlock := s.lockLyrics(musicID)
	defer unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	defer tx.Rollback()
	if err := rejectLegacyLyricsMutationForSourceV3(tx, musicID); err != nil {
		return model.SongLyrics{}, false, err
	}
	current, err := s.loadLyrics(tx, musicID)
	if err != nil {
		return model.SongLyrics{}, false, err
	}
	if revision != current.lyrics.Revision {
		copy := current.lyrics
		return model.SongLyrics{}, false, &LyricsContractError{Code: "revision_conflict", Current: &copy}
	}
	var publishedRevision int
	if err := tx.QueryRow(`SELECT revision FROM song_lyrics_publications WHERE music_id=?`, musicID).Scan(&publishedRevision); err == sql.ErrNoRows {
		current.lyrics.Status = "draft"
		current.lyrics.PublishedRevision = 0
		return current.lyrics, false, nil
	} else if err != nil {
		return model.SongLyrics{}, false, err
	}
	if _, err := tx.Exec(`DELETE FROM song_lyrics_publications WHERE music_id=?`, musicID); err != nil {
		return model.SongLyrics{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts, user, action, detail) VALUES (?, ?, 'lyrics.unpublish', ?)`,
		time.Now().Unix(), optionalActor(users), fmt.Sprintf("musicId=%d revision=%d", musicID, revision)); err != nil {
		return model.SongLyrics{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.SongLyrics{}, false, err
	}
	current.lyrics.Status = "draft"
	current.lyrics.PublishedRevision = 0
	s.NotifyChange()
	return current.lyrics, true, nil
}

type publicLyricSegmentV1 struct {
	Text         string `json:"text"`
	PerformerIDs []int  `json:"performerIds"`
}

type publicLyricLineV1 struct {
	ID                string                 `json:"id"`
	Order             int                    `json:"order"`
	Japanese          string                 `json:"japanese"`
	Chinese           string                 `json:"zh-CN"`
	English           string                 `json:"en-US"`
	StanzaBreakBefore bool                   `json:"stanzaBreakBefore,omitempty"`
	Segments          []publicLyricSegmentV1 `json:"segments"`
}

type publicSongLyricsV1 struct {
	Version          int                 `json:"version"`
	MusicID          int                 `json:"musicId"`
	Revision         int                 `json:"revision"`
	UpdatedAt        string              `json:"updatedAt"`
	Attribution      string              `json:"attribution"`
	SourceURL        string              `json:"sourceUrl,omitempty"`
	SourcePageID     int                 `json:"sourcePageId,omitempty"`
	SourceRevisionID int                 `json:"sourceRevisionId,omitempty"`
	SourceSHA1       string              `json:"sourceSha1,omitempty"`
	SourceFetchedAt  string              `json:"sourceFetchedAt,omitempty"`
	LicenseName      string              `json:"licenseName,omitempty"`
	LicenseURL       string              `json:"licenseUrl,omitempty"`
	Lines            []publicLyricLineV1 `json:"lines"`
}

// publicLyricsV1SourceLicense derives the stable public license pair for the
// imported extraction source. Sekaipedia pages are published under CC BY-SA
// 4.0; Fandom pages under CC BY-SA 3.0; other legacy sources keep no
// public license claim.
func publicLyricsV1SourceLicense(sourceURL string) (string, string) {
	if strings.Contains(sourceURL, "sekaipedia.org/") {
		return "CC BY-SA 4.0", "https://creativecommons.org/licenses/by-sa/4.0/"
	}
	if strings.Contains(sourceURL, "fandom.com/") || strings.Contains(sourceURL, "wikia.com/") {
		return "CC BY-SA 3.0", "https://creativecommons.org/licenses/by-sa/3.0/"
	}
	return "", ""
}

func publicLyricsV1(lyrics model.SongLyrics) publicSongLyricsV1 {
	attribution := lyrics.Attribution
	if translation := strings.TrimSpace(lyrics.TranslationCredit); translation != "" {
		attribution = translation
	}
	licenseName, licenseURL := publicLyricsV1SourceLicense(lyrics.SourceURL)
	public := publicSongLyricsV1{
		Version: 1, MusicID: lyrics.MusicID, Revision: lyrics.Revision,
		UpdatedAt: lyrics.UpdatedAt, Attribution: attribution,
		Lines: make([]publicLyricLineV1, len(lyrics.Lines)),
	}
	if licenseName != "" {
		public.SourceURL = lyrics.SourceURL
		public.SourcePageID = lyrics.SourcePageID
		public.SourceRevisionID = lyrics.SourceRevisionID
		public.SourceSHA1 = lyrics.SourceSHA1
		public.SourceFetchedAt = lyrics.SourceFetchedAt
		public.LicenseName = licenseName
		public.LicenseURL = licenseURL
	}
	for index, line := range lyrics.Lines {
		public.Lines[index] = publicLyricLineV1{
			ID: fmt.Sprintf("line-%d", line.Order+1), Order: line.Order, Japanese: line.Japanese,
			Chinese: line.Chinese, English: line.English, StanzaBreakBefore: line.StanzaBreakBefore,
			Segments: make([]publicLyricSegmentV1, len(line.Segments)),
		}
		for segmentIndex, segment := range line.Segments {
			public.Lines[index].Segments[segmentIndex] = publicLyricSegmentV1{
				Text: segment.Text, PerformerIDs: append([]int(nil), segment.PerformerIDs...),
			}
		}
	}
	return public
}

type byteCounter struct {
	n int64
}

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func validatePublicLyricsArtifactSize(public any) error {
	var counter byteCounter
	enc := json.NewEncoder(&counter)
	enc.SetIndent("", "  ")
	if err := enc.Encode(public); err != nil {
		return fmt.Errorf("encode public lyrics document: %w", err)
	}
	if counter.n > int64(model.PublicLyricsMaxArtifactBytes) {
		return errors.New("encoded public lyrics document exceeds the public artifact size limit")
	}
	return nil
}

func (s *Store) PublishedLyrics() (PublicLyricsIndexDocument, map[int]PublicLyricsDetailDocument, error) {
	return s.publishedLyricsSnapshot()
}

func (s *Store) loadLyrics(q queryRower, musicID int) (storedLyrics, error) {
	return s.loadLyricsWithHook(q, musicID, nil)
}

func (s *Store) loadLyricsWithHook(q queryRower, musicID int, afterHeader func()) (storedLyrics, error) {
	var result storedLyrics
	var updatedAt int64
	var publishedRevision sql.NullInt64
	var sourceFetchedAt int64
	var exactSourceFetchedAt string
	err := q.QueryRow(`SELECT l.music_id, l.revision, l.updated_at, l.attribution,
		l.translation_credit, l.proofreading_credit, l.source_note, l.source_url,
		l.license_note, l.source_hash, l.source_page_id, l.source_revision_id, l.source_sha1,
		l.source_fetched_at, l.source_fetched_at_rfc3339, p.revision
		FROM song_lyrics l LEFT JOIN song_lyrics_publications p ON p.music_id=l.music_id
		WHERE l.music_id=?`, musicID).Scan(
		&result.lyrics.MusicID, &result.lyrics.Revision, &updatedAt, &result.lyrics.Attribution,
		&result.lyrics.TranslationCredit, &result.lyrics.ProofreadingCredit, &result.lyrics.SourceNote,
		&result.lyrics.SourceURL, &result.lyrics.LicenseNote, &result.sourceHash,
		&result.lyrics.SourcePageID, &result.lyrics.SourceRevisionID, &result.lyrics.SourceSHA1,
		&sourceFetchedAt, &exactSourceFetchedAt, &publishedRevision)
	if err == sql.ErrNoRows {
		return result, ErrLyricsNotFound
	}
	if err != nil {
		return result, err
	}
	result.lyrics.Status = "draft"
	if publishedRevision.Valid {
		result.lyrics.PublishedRevision = int(publishedRevision.Int64)
		result.lyrics.Status = "draft-published"
		if publishedRevision.Int64 == int64(result.lyrics.Revision) {
			result.lyrics.Status = "published"
		}
	}
	result.lyrics.UpdatedAt = formatTimestamp(updatedAt)
	if exactSourceFetchedAt != "" {
		result.lyrics.SourceFetchedAt = exactSourceFetchedAt
	} else if sourceFetchedAt > 0 {
		result.lyrics.SourceFetchedAt = formatTimestamp(sourceFetchedAt)
	}
	if afterHeader != nil {
		afterHeader()
	}
	result.lyrics.Lines = []model.LyricLine{}
	lineRows, err := q.Query(`SELECT line_id, position, japanese, zh_cn, en_us, stanza_break_before
		FROM song_lyric_lines WHERE music_id=? ORDER BY position`, musicID)
	if err != nil {
		return storedLyrics{}, err
	}
	for lineRows.Next() {
		var line model.LyricLine
		var stanzaBreak int
		if err := lineRows.Scan(&line.ID, &line.Order, &line.Japanese, &line.Chinese, &line.English, &stanzaBreak); err != nil {
			lineRows.Close()
			return storedLyrics{}, err
		}
		line.StanzaBreakBefore = stanzaBreak == 1
		line.Segments = []model.LyricSegment{}
		result.lyrics.Lines = append(result.lyrics.Lines, line)
	}
	if err := lineRows.Close(); err != nil {
		return storedLyrics{}, err
	}
	lineIndex := map[string]int{}
	for index, line := range result.lyrics.Lines {
		lineIndex[line.ID] = index
	}
	segmentRows, err := q.Query(`SELECT line_id, text, performer_ids_json, ruby_json FROM song_lyric_segments
		WHERE music_id=? ORDER BY line_id, position`, musicID)
	if err != nil {
		return storedLyrics{}, err
	}
	defer segmentRows.Close()
	for segmentRows.Next() {
		var lineID, performerJSON, rubyJSON string
		var segment model.LyricSegment
		if err := segmentRows.Scan(&lineID, &segment.Text, &performerJSON, &rubyJSON); err != nil {
			return storedLyrics{}, err
		}
		if err := json.Unmarshal([]byte(performerJSON), &segment.PerformerIDs); err != nil {
			return storedLyrics{}, fmt.Errorf("lyrics segment performers for musicId=%d lineId=%q: %w", musicID, lineID, err)
		}
		if err := json.Unmarshal([]byte(rubyJSON), &segment.Ruby); err != nil {
			return storedLyrics{}, fmt.Errorf("lyrics segment ruby for musicId=%d lineId=%q: %w", musicID, lineID, err)
		}
		if segment.PerformerIDs == nil {
			segment.PerformerIDs = []int{}
		}
		if segment.Ruby == nil {
			segment.Ruby = []model.LyricRubySpan{}
		}
		if index, ok := lineIndex[lineID]; ok {
			result.lyrics.Lines[index].Segments = append(result.lyrics.Lines[index].Segments, segment)
		}
	}
	return result, segmentRows.Err()
}

func cloneEditableLyricsRuby(lyrics model.SongLyrics) model.SongLyrics {
	lyrics.Lines = append([]model.LyricLine(nil), lyrics.Lines...)
	for lineIndex := range lyrics.Lines {
		lyrics.Lines[lineIndex].Segments = append([]model.LyricSegment(nil), lyrics.Lines[lineIndex].Segments...)
		for segmentIndex := range lyrics.Lines[lineIndex].Segments {
			segment := &lyrics.Lines[lineIndex].Segments[segmentIndex]
			segment.PerformerIDs = append([]int{}, segment.PerformerIDs...)
			segment.Ruby = append([]model.LyricRubySpan{}, segment.Ruby...)
		}
	}
	return lyrics
}

// normalizeEditableLyricsRuby preserves compatibility with new manual drafts
// while guaranteeing that every persisted segment has a concrete, editable
// ruby contract. Machine-generated source imports already carry detailed
// spans; legacy/manual segments receive one exact-text span.
func normalizeEditableLyricsRuby(lyrics model.SongLyrics) model.SongLyrics {
	lyrics = cloneEditableLyricsRuby(lyrics)
	for lineIndex := range lyrics.Lines {
		for segmentIndex := range lyrics.Lines[lineIndex].Segments {
			segment := &lyrics.Lines[lineIndex].Segments[segmentIndex]
			if len(segment.Ruby) == 0 && segment.Text != "" {
				segment.Ruby = []model.LyricRubySpan{{Text: segment.Text}}
			}
		}
	}
	return lyrics
}

// preserveOmittedLyricsRuby accepts the pre-ruby private payload only when its
// complete persisted source and performer structure is unchanged. In that
// narrow compatibility case, omitted spans inherit the stored editable ruby;
// otherwise the caller must supply ruby explicitly for every segment.
func preserveOmittedLyricsRuby(normalized *model.SongLyrics, requested, current model.SongLyrics) (bool, []string) {
	var omitted [][2]int
	for lineIndex := range requested.Lines {
		for segmentIndex := range requested.Lines[lineIndex].Segments {
			if len(requested.Lines[lineIndex].Segments[segmentIndex].Ruby) == 0 {
				omitted = append(omitted, [2]int{lineIndex, segmentIndex})
			}
		}
	}
	if len(omitted) == 0 {
		return false, nil
	}
	if !sameLyricsRubySourceStructure(requested.Lines, current.Lines) {
		details := make([]string, 0, len(omitted))
		for _, position := range omitted {
			details = append(details, fmt.Sprintf(
				"lines[%d].segments[%d].ruby must be supplied when the Japanese, segment, or performer structure changes",
				position[0], position[1],
			))
		}
		return false, details
	}
	for _, position := range omitted {
		lineIndex, segmentIndex := position[0], position[1]
		currentRuby := current.Lines[lineIndex].Segments[segmentIndex].Ruby
		if len(currentRuby) > 0 {
			normalized.Lines[lineIndex].Segments[segmentIndex].Ruby = append([]model.LyricRubySpan(nil), currentRuby...)
		}
	}
	return true, nil
}

func sameLyricsRubySourceStructure(left, right []model.LyricLine) bool {
	if len(left) != len(right) {
		return false
	}
	for lineIndex := range left {
		leftLine, rightLine := left[lineIndex], right[lineIndex]
		if leftLine.ID != rightLine.ID || leftLine.Order != rightLine.Order || leftLine.Japanese != rightLine.Japanese ||
			leftLine.StanzaBreakBefore != rightLine.StanzaBreakBefore || len(leftLine.Segments) != len(rightLine.Segments) {
			return false
		}
		for segmentIndex := range leftLine.Segments {
			leftSegment, rightSegment := leftLine.Segments[segmentIndex], rightLine.Segments[segmentIndex]
			if leftSegment.Text != rightSegment.Text || !sameLyricsPerformerIDs(leftSegment.PerformerIDs, rightSegment.PerformerIDs) {
				return false
			}
		}
	}
	return true
}

func sameLyricsPerformerIDs(left, right []int) bool {
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

func lyricsHasTranslationCredit(lyrics model.SongLyrics) bool {
	return strings.TrimSpace(lyrics.TranslationCredit) != "" || strings.TrimSpace(lyrics.Attribution) != ""
}

// lyricsHasSourceAttribution reports whether the extraction source of a
// legacy document is attributed well enough to publish without a translation
// credit (source-only publications render the source card with no credit).
func lyricsHasSourceAttribution(lyrics model.SongLyrics) bool {
	return strings.TrimSpace(lyrics.SourceURL) != "" && lyrics.SourcePageID > 0 &&
		strings.TrimSpace(lyrics.SourceSHA1) != ""
}

// lyricsSourceOnlyPublicationAllowed permits source-only publications when no
// credit field is filled at all and the extraction source is attributed.
// Proofreading-only documents keep requiring a translation credit.
func lyricsSourceOnlyPublicationAllowed(lyrics model.SongLyrics) bool {
	return strings.TrimSpace(lyrics.ProofreadingCredit) == "" && lyricsHasSourceAttribution(lyrics)
}

func validateLyrics(lyrics model.SongLyrics, performers map[int]bool, publishing bool) (string, []string, string) {
	if lyrics.MusicID <= 0 {
		return "source_drift", []string{"musicId must be positive"}, ""
	}
	if len(lyrics.Lines) == 0 || len(lyrics.Lines) > maxLyricsLines {
		return "segment_mismatch", []string{"lines must contain between 1 and 5000 items"}, ""
	}
	if len(lyrics.Attribution) > maxLyricsMetadataBytes || len(lyrics.TranslationCredit) > maxLyricsMetadataBytes ||
		len(lyrics.ProofreadingCredit) > maxLyricsMetadataBytes || len(lyrics.SourceNote) > maxLyricsMetadataBytes ||
		len(lyrics.LicenseNote) > maxLyricsMetadataBytes || len(lyrics.SourceURL) > maxLyricsURLBytes ||
		len(lyrics.SourceSHA1) > 256 || !utf8.ValidString(lyrics.Attribution) ||
		!utf8.ValidString(lyrics.TranslationCredit) || !utf8.ValidString(lyrics.ProofreadingCredit) {
		return "segment_mismatch", []string{"lyrics metadata exceeds safe size limits"}, ""
	}
	totalBytes := len(lyrics.Attribution) + len(lyrics.TranslationCredit) + len(lyrics.ProofreadingCredit) +
		len(lyrics.SourceNote) + len(lyrics.LicenseNote) + len(lyrics.SourceURL) + len(lyrics.SourceSHA1)
	var segmentDetails, performerDetails, publicationDetails []string
	if publishing && !lyricsHasTranslationCredit(lyrics) && !lyricsSourceOnlyPublicationAllowed(lyrics) {
		publicationDetails = append(publicationDetails, "translation credit is required for publication")
	}
	lineIDs := map[string]bool{}
	orders := map[int]bool{}
	for lineIndex, line := range lyrics.Lines {
		path := fmt.Sprintf("lines[%d]", lineIndex)
		if strings.TrimSpace(line.ID) == "" || len(line.ID) > 128 || lineIDs[line.ID] {
			segmentDetails = append(segmentDetails, path+".id must be unique and 1-128 characters")
		}
		lineIDs[line.ID] = true
		if line.Order < 0 || orders[line.Order] {
			segmentDetails = append(segmentDetails, path+".order must be unique and non-negative")
		}
		orders[line.Order] = true
		if len(line.Segments) == 0 || len(line.Segments) > maxLyricsSegmentsPerLine {
			segmentDetails = append(segmentDetails, path+".segments must contain between 1 and 100 items")
		}
		if strings.TrimSpace(line.Japanese) == "" {
			segmentDetails = append(segmentDetails, path+".japanese must not be empty")
		}
		if len(line.Japanese) > maxLyricsLineTextBytes || len(line.Chinese) > maxLyricsLineTextBytes || len(line.English) > maxLyricsLineTextBytes {
			segmentDetails = append(segmentDetails, path+" text exceeds the safe per-line size limit")
		}
		totalBytes += len(line.ID) + len(line.Japanese) + len(line.Chinese) + len(line.English)
		var concatenated strings.Builder
		for segmentIndex, segment := range line.Segments {
			if len(segment.Text) > maxLyricsLineTextBytes {
				segmentDetails = append(segmentDetails, fmt.Sprintf("%s.segments[%d].text exceeds the safe size limit", path, segmentIndex))
			}
			totalBytes += len(segment.Text) + len(segment.PerformerIDs)*8
			concatenated.WriteString(segment.Text)
			var rubyText strings.Builder
			if len(segment.Ruby) == 0 || len(segment.Ruby) > maxLyricsRubyPerSegment {
				segmentDetails = append(segmentDetails, fmt.Sprintf("%s.segments[%d].ruby must contain between 1 and %d editable spans", path, segmentIndex, maxLyricsRubyPerSegment))
			}
			if len(segment.PerformerIDs) > maxLyricsPerformers {
				performerDetails = append(performerDetails, fmt.Sprintf("%s.segments[%d] must contain at most %d performerIds", path, segmentIndex, maxLyricsPerformers))
			}
			for rubyIndex, span := range segment.Ruby {
				if span.Text == "" || len(span.Text) > maxLyricsLineTextBytes || len(span.Reading) > maxLyricsLineTextBytes {
					segmentDetails = append(segmentDetails, fmt.Sprintf("%s.segments[%d].ruby[%d] has invalid text or reading", path, segmentIndex, rubyIndex))
				}
				totalBytes += len(span.Text) + len(span.Reading)
				rubyText.WriteString(span.Text)
			}
			if rubyText.String() != segment.Text {
				segmentDetails = append(segmentDetails, fmt.Sprintf("%s.segments[%d].ruby text must equal segment text", path, segmentIndex))
			}
			seenPerformers := map[int]bool{}
			for _, performerID := range segment.PerformerIDs {
				if seenPerformers[performerID] {
					performerDetails = append(performerDetails,
						fmt.Sprintf("%s.segments[%d] has duplicate performerId %d", path, segmentIndex, performerID))
					continue
				}
				seenPerformers[performerID] = true
				if !performers[performerID] {
					performerDetails = append(performerDetails,
						fmt.Sprintf("%s.segments[%d] has invalid performerId %d", path, segmentIndex, performerID))
				}
			}
		}
		if concatenated.String() != line.Japanese {
			segmentDetails = append(segmentDetails, path+".japanese must equal concatenated segment text")
		}
	}
	if totalBytes > maxLyricsDocumentBytes {
		segmentDetails = append(segmentDetails, "lyrics document exceeds the safe total size limit")
	}
	if len(segmentDetails) > 0 {
		return "segment_mismatch", segmentDetails, ""
	}
	if len(performerDetails) > 0 {
		return "invalid_performer", performerDetails, ""
	}
	if len(publicationDetails) > 0 {
		return "incomplete_publication", publicationDetails, ""
	}
	return "", nil, lyricsSourceHash(lyrics.Lines)
}

func lyricsProvenanceChanged(left, right model.SongLyrics) bool {
	if right.SourceURL == "" && right.SourcePageID == 0 && right.SourceRevisionID == 0 && right.SourceSHA1 == "" {
		return false
	}
	return left.SourcePageID != right.SourcePageID || left.SourceRevisionID != right.SourceRevisionID ||
		left.SourceSHA1 != right.SourceSHA1 || left.SourceFetchedAt != right.SourceFetchedAt ||
		left.SourceURL != right.SourceURL
}

func lyricsSourceStructureChanged(left, right []model.LyricLine) bool {
	if len(left) != len(right) {
		return true
	}
	for index := range left {
		if left[index].ID != right[index].ID || left[index].Order != right[index].Order || left[index].Japanese != right[index].Japanese {
			return true
		}
	}
	return false
}

func validateLyricsProvenance(lyrics model.SongLyrics) (int64, error) {
	provenanceSet := lyrics.SourcePageID != 0 || lyrics.SourceRevisionID != 0 ||
		strings.TrimSpace(lyrics.SourceSHA1) != "" || strings.TrimSpace(lyrics.SourceFetchedAt) != ""
	if !provenanceSet {
		if sourceURL := strings.TrimSpace(lyrics.SourceURL); sourceURL != "" {
			if err := ValidateLyricsSourceURL(sourceURL); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	if lyrics.SourcePageID <= 0 || lyrics.SourceRevisionID <= 0 ||
		strings.TrimSpace(lyrics.SourceSHA1) == "" || strings.TrimSpace(lyrics.SourceFetchedAt) == "" ||
		strings.TrimSpace(lyrics.SourceURL) == "" {
		return 0, &LyricsContractError{Code: "source_drift", Details: []string{
			"sourcePageId, sourceRevisionId, sourceSha1, sourceFetchedAt, and sourceUrl must be supplied together",
		}}
	}
	if !hasCanonicalLyricsSourceSHA1(lyrics.SourceSHA1) {
		return 0, &LyricsContractError{Code: "source_drift", Details: []string{
			"sourceSha1 must be exactly 40 lowercase hexadecimal characters",
		}}
	}
	if err := ValidateLyricsSourceURL(lyrics.SourceURL); err != nil {
		return 0, err
	}
	if isManagedLyricsSourceURL(lyrics.SourceURL) {
		if err := ValidateLyricsSourceRevisionURL(lyrics.SourceURL, lyrics.SourceRevisionID); err != nil {
			return 0, err
		}
	}
	fetchedAt, err := parseCanonicalExactTimestamp(lyrics.SourceFetchedAt)
	if err != nil {
		return 0, &LyricsContractError{Code: "source_drift", Details: []string{
			"sourceFetchedAt must be a canonical UTC RFC3339Nano timestamp",
		}}
	}
	if fetchedAt.Unix() <= 0 {
		return 0, &LyricsContractError{Code: "source_drift", Details: []string{"sourceFetchedAt must be after 1970-01-01T00:00:00Z"}}
	}
	return fetchedAt.Unix(), nil
}

func hasCanonicalLyricsSourceSHA1(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// ValidateLyricsSourceURL applies the transport policy for every persisted
// lyrics source URL. External absolute HTTP(S) references remain supported,
// while managed Wiki hostnames require HTTPS and the default port. A trailing
// DNS root dot is classified as managed here so it cannot bypass ordinary-save
// protection, but it is not accepted as a canonical verified revision URL.
func ValidateLyricsSourceURL(value string) error {
	parsed, err := parseLyricsSourceURL(value)
	if err != nil {
		return err
	}
	if !isManagedLyricsSource(parsed) {
		return nil
	}
	if !strings.EqualFold(parsed.Scheme, "https") || (parsed.Port() != "" && parsed.Port() != "443") {
		return &LyricsContractError{Code: "source_drift", Details: []string{
			"managed sourceUrl must use HTTPS with the default port",
		}}
	}
	return nil
}

// ValidateLyricsSourceRevisionURL is the strict boundary for the managed Wiki
// source clients (Vocaloid Wiki and Sekaipedia). It accepts only an exact
// managed hostname, the /wiki/ revision path, and a single oldid query value
// matching the verified MediaWiki revision. Other callers may keep using
// non-managed references, but they cannot receive a Wiki preview grant.
func ValidateLyricsSourceRevisionURL(value string, revisionID int) error {
	if value != strings.TrimSpace(value) {
		return &LyricsContractError{Code: "source_drift", Details: []string{
			"verified managed sourceUrl must be canonical",
		}}
	}
	parsed, err := parseLyricsSourceURL(value)
	if err != nil {
		return err
	}
	if revisionID <= 0 || !isExactManagedLyricsSource(parsed) || !strings.EqualFold(parsed.Scheme, "https") ||
		(parsed.Port() != "" && parsed.Port() != "443") || parsed.Fragment != "" ||
		!strings.HasPrefix(parsed.EscapedPath(), "/wiki/") || len(strings.TrimPrefix(parsed.EscapedPath(), "/wiki/")) == 0 {
		return &LyricsContractError{Code: "source_drift", Details: []string{
			"verified managed sourceUrl must use an exact HTTPS managed Wiki revision",
		}}
	}
	expectedQuery := "oldid=" + strconv.Itoa(revisionID)
	if parsed.RawQuery != expectedQuery || parsed.ForceQuery {
		return &LyricsContractError{Code: "source_drift", Details: []string{
			"verified managed sourceUrl oldid must match sourceRevisionId",
		}}
	}
	canonicalHost := strings.ToLower(parsed.Hostname())
	if parsed.Port() == "443" {
		canonicalHost += ":443"
	}
	canonicalURL := (&url.URL{
		Scheme:   "https",
		Host:     canonicalHost,
		Path:     parsed.Path,
		RawQuery: expectedQuery,
	}).String()
	if strings.Contains(parsed.Path, " ") || value != canonicalURL {
		return &LyricsContractError{Code: "source_drift", Details: []string{
			"verified managed sourceUrl must use the canonical encoded Wiki revision URL",
		}}
	}
	return nil
}

func parseLyricsSourceURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, &LyricsContractError{Code: "source_drift", Details: []string{
			"sourceUrl must be an absolute http(s) URL without credentials",
		}}
	}
	return parsed, nil
}

func isManagedLyricsSourceURL(value string) bool {
	parsed, err := parseLyricsSourceURL(value)
	return err == nil && isManagedLyricsSource(parsed)
}

func isManagedLyricsSource(parsed *url.URL) bool {
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	_, managed := managedLyricsSourceHosts[hostname]
	return managed
}

func isExactManagedLyricsSource(parsed *url.URL) bool {
	hostname := strings.ToLower(parsed.Hostname())
	_, managed := managedLyricsSourceHosts[hostname]
	return managed
}

func lyricsSourceHash(lines []model.LyricLine) string {
	ordered := append([]model.LyricLine(nil), lines...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	hash := sha256.New()
	for _, line := range ordered {
		hash.Write([]byte(line.ID))
		hash.Write([]byte{0})
		hash.Write([]byte(line.Japanese))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func lyricsMutexStripe(musicID int) int {
	return int(uint(musicID) & uint(lyricsMutexStripeCount-1))
}

func (s *Store) lockLyrics(musicID int) func() {
	mutex := &s.lyricsMutexes[lyricsMutexStripe(musicID)]
	mutex.Lock()
	return mutex.Unlock
}

func sameLyricsContent(left, right model.SongLyrics) bool {
	canonicalize := func(lyrics model.SongLyrics) model.SongLyrics {
		lyrics = normalizeEditableLyricsRuby(lyrics)
		for lineIndex := range lyrics.Lines {
			for segmentIndex := range lyrics.Lines[lineIndex].Segments {
				if len(lyrics.Lines[lineIndex].Segments[segmentIndex].PerformerIDs) == 0 {
					lyrics.Lines[lineIndex].Segments[segmentIndex].PerformerIDs = nil
				}
			}
		}
		return lyrics
	}
	left = canonicalize(left)
	right = canonicalize(right)
	left.Status, left.Revision, left.PublishedRevision, left.UpdatedAt = "", 0, 0, ""
	right.Status, right.Revision, right.PublishedRevision, right.UpdatedAt = "", 0, 0, ""
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func formatTimestamp(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func parseTimestamp(value string) (int64, error) {
	timestamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, err
	}
	return timestamp.Unix(), nil
}

func parseCanonicalExactTimestamp(value string) (time.Time, error) {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || value == "" || !strings.HasSuffix(value, "Z") || timestamp.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("timestamp is not canonical UTC RFC3339Nano")
	}
	return timestamp.UTC(), nil
}

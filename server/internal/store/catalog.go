package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/model"
)

type MusicCatalogRecord struct {
	MusicID             int
	JapaneseTitle       string
	ChineseTitle        string
	EnglishTitle        string
	JacketURL           string
	IsNewlyWrittenMusic bool
	ProducerMetadata    string
	Lyricist            string
	Composer            string
	Arranger            string
	AssetbundleName     string
	VersionHint         string
	LyricsVersion       string
	LyricsVersionKnown  bool
	Vocals              []model.CatalogVocalSignal
}

type PerformerCatalogRecord struct {
	PerformerID  int
	JapaneseName string
	ChineseName  string
	EnglishName  string
}

func (s *Store) UpsertMusicCatalog(records []MusicCatalogRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := upsertMusicCatalogTx(tx, records, time.Now().Unix())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed > 0 {
		s.NotifyChange()
	}
	return nil
}

func upsertMusicCatalogTx(tx txExecer, records []MusicCatalogRecord, now int64) (int, error) {
	changed := 0
	for _, record := range records {
		if record.MusicID <= 0 || strings.TrimSpace(record.JapaneseTitle) == "" {
			continue
		}
		if !musicCatalogEvidenceSpecified(record) {
			var presenceJSON, vocalsJSON string
			var lyricsVersion string
			err := tx.QueryRow(`SELECT lyricist, composer, arranger, producer_metadata, assetbundle_name,
				version_hint, lyrics_version, lyrics_evidence_presence_json, vocal_signals_json
				FROM catalog_music WHERE music_id=?`, record.MusicID).Scan(
				&record.Lyricist, &record.Composer, &record.Arranger, &record.ProducerMetadata,
				&record.AssetbundleName, &record.VersionHint, &lyricsVersion, &presenceJSON, &vocalsJSON)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return changed, err
			}
			if err == nil {
				record.LyricsVersion = lyricsVersion
				if strings.TrimSpace(presenceJSON) == "" {
					presenceJSON = "{}"
				}
				var presence model.CatalogEvidencePresence
				if err := json.Unmarshal([]byte(presenceJSON), &presence); err != nil {
					return changed, fmt.Errorf("catalog music %d evidence presence: %w", record.MusicID, err)
				}
				record.LyricsVersionKnown = presence.LyricsVersion
				if strings.TrimSpace(vocalsJSON) == "" {
					vocalsJSON = "[]"
				}
				if err := json.Unmarshal([]byte(vocalsJSON), &record.Vocals); err != nil {
					return changed, fmt.Errorf("catalog music %d vocal signals: %w", record.MusicID, err)
				}
			}
		}
		evidence := musicCatalogEvidence(record)
		fingerprint, err := model.CatalogLyricsEvidenceFingerprint(evidence)
		if err != nil {
			return changed, fmt.Errorf("catalog music %d fingerprint: %w", record.MusicID, err)
		}
		presenceJSON, err := json.Marshal(evidence.Presence)
		if err != nil {
			return changed, err
		}
		vocalsJSON, err := json.Marshal(evidence.Vocals)
		if err != nil {
			return changed, err
		}
		producerMetadata := musicProducerMetadata(evidence.Lyricist, evidence.Composer, evidence.Arranger)
		if producerMetadata == "" {
			producerMetadata = strings.TrimSpace(record.ProducerMetadata)
		}
		newlyWritten := 0
		if record.IsNewlyWrittenMusic {
			newlyWritten = 1
		}
		result, err := tx.Exec(`INSERT INTO catalog_music
			(music_id, title_ja, title_zh, title_en, jacket_url, newly_written, updated_at, producer_metadata,
			 lyricist, composer, arranger, assetbundle_name, version_hint, lyrics_version,
			 lyrics_evidence_presence_json, vocal_signals_json, lyrics_catalog_fingerprint, lyrics_catalog_policy_version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(music_id) DO UPDATE SET title_ja=excluded.title_ja,
			title_zh=CASE WHEN excluded.title_zh<>'' THEN excluded.title_zh ELSE catalog_music.title_zh END,
			title_en=CASE WHEN excluded.title_en<>'' THEN excluded.title_en ELSE catalog_music.title_en END,
			jacket_url=CASE WHEN excluded.jacket_url<>'' THEN excluded.jacket_url ELSE catalog_music.jacket_url END,
			newly_written=excluded.newly_written, updated_at=excluded.updated_at,
			producer_metadata=excluded.producer_metadata, lyricist=excluded.lyricist, composer=excluded.composer,
			arranger=excluded.arranger, assetbundle_name=excluded.assetbundle_name, version_hint=excluded.version_hint,
			lyrics_version=excluded.lyrics_version, lyrics_evidence_presence_json=excluded.lyrics_evidence_presence_json,
			vocal_signals_json=excluded.vocal_signals_json, lyrics_catalog_fingerprint=excluded.lyrics_catalog_fingerprint,
			lyrics_catalog_policy_version=excluded.lyrics_catalog_policy_version
			WHERE catalog_music.title_ja<>excluded.title_ja
			 OR (excluded.title_zh<>'' AND catalog_music.title_zh<>excluded.title_zh)
			 OR (excluded.title_en<>'' AND catalog_music.title_en<>excluded.title_en)
			 OR (excluded.jacket_url<>'' AND catalog_music.jacket_url<>excluded.jacket_url)
			 OR catalog_music.newly_written<>excluded.newly_written
			 OR catalog_music.producer_metadata<>excluded.producer_metadata
			 OR catalog_music.lyricist<>excluded.lyricist OR catalog_music.composer<>excluded.composer
			 OR catalog_music.arranger<>excluded.arranger OR catalog_music.assetbundle_name<>excluded.assetbundle_name
			 OR catalog_music.version_hint<>excluded.version_hint OR catalog_music.lyrics_version<>excluded.lyrics_version
			 OR catalog_music.lyrics_evidence_presence_json<>excluded.lyrics_evidence_presence_json
			 OR catalog_music.vocal_signals_json<>excluded.vocal_signals_json
			 OR catalog_music.lyrics_catalog_fingerprint<>excluded.lyrics_catalog_fingerprint
			 OR catalog_music.lyrics_catalog_policy_version<>excluded.lyrics_catalog_policy_version`,
			record.MusicID, evidence.Title, record.ChineseTitle, record.EnglishTitle, record.JacketURL,
			newlyWritten, now, producerMetadata, evidence.Lyricist, evidence.Composer, evidence.Arranger,
			evidence.Assetbundle, evidence.VersionHint, evidence.LyricsVersion, string(presenceJSON), string(vocalsJSON),
			fingerprint, model.LyricsCatalogIdentityPolicyVersion)
		if err != nil {
			return changed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return changed, err
		}
		if affected > 0 {
			completedAt := now * 1000
			if _, err := tx.Exec(`UPDATE lyrics_source_review_items
				SET state='superseded', version=version+1,
					updated_at=CASE WHEN created_at>? THEN created_at ELSE ? END,
					completed_at=CASE WHEN created_at>? THEN created_at ELSE ? END
				WHERE music_id=? AND state='pending' AND catalog_fingerprint<>?`, completedAt, completedAt,
				completedAt, completedAt, record.MusicID, fingerprint); err != nil {
				return changed, err
			}
		}
		changed += int(affected)
	}
	return changed, nil
}

func (s *Store) UpsertPerformerCatalog(records []PerformerCatalogRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := upsertPerformerCatalogTx(tx, records, time.Now().Unix())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed > 0 {
		s.NotifyChange()
	}
	return nil
}

func upsertPerformerCatalogTx(tx txExecer, records []PerformerCatalogRecord, now int64) (int, error) {
	changed := 0
	for _, record := range records {
		if record.PerformerID <= 0 || strings.TrimSpace(record.JapaneseName) == "" {
			continue
		}
		result, err := tx.Exec(`INSERT INTO catalog_performers
			(performer_id, name_ja, name_zh, name_en, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(performer_id) DO UPDATE SET name_ja=excluded.name_ja,
			name_zh=CASE WHEN excluded.name_zh<>'' THEN excluded.name_zh ELSE catalog_performers.name_zh END,
			name_en=CASE WHEN excluded.name_en<>'' THEN excluded.name_en ELSE catalog_performers.name_en END,
			updated_at=excluded.updated_at
			WHERE catalog_performers.name_ja<>excluded.name_ja
			 OR (excluded.name_zh<>'' AND catalog_performers.name_zh<>excluded.name_zh)
			 OR (excluded.name_en<>'' AND catalog_performers.name_en<>excluded.name_en)`,
			record.PerformerID, record.JapaneseName, record.ChineseName, record.EnglishName, now)
		if err != nil {
			return changed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return changed, err
		}
		changed += int(affected)
	}
	return changed, nil
}

func (s *Store) CatalogMusic(query string, newlyWrittenOnly bool, limit, cursor int) (model.CatalogMusicResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	// A recovery takeover supersedes the song's ledger availability state.
	sqlQuery := `SELECT m.music_id, m.title_ja,
		COALESCE(NULLIF(zh.cn_text, ''), m.title_zh), COALESCE(NULLIF(en.text, ''), m.title_en), m.jacket_url,
		m.newly_written, l.revision, p.revision, d.document_id, COALESCE(d.schema_version=3, 0),
		COALESCE(
			(SELECT availability.state FROM song_lyrics_availability_documents AS availability
			 WHERE availability.music_id=m.music_id
			 AND NOT EXISTS (SELECT 1 FROM lyrics_recovery_takeovers AS takeover WHERE takeover.music_id=m.music_id)
			 ORDER BY availability.created_at DESC,availability.batch_sha256 DESC LIMIT 1),
			(SELECT seed.state FROM embedded_lyrics_editor_seed_items AS seed
			 WHERE seed.music_id=m.music_id AND seed.seed_kind='availability' AND seed.apply_status='inserted'
			 ORDER BY seed.created_at DESC,seed.seed_sha256 DESC LIMIT 1)
		),
		EXISTS (SELECT 1 FROM song_lyrics_public_withdrawals AS withdrawal WHERE withdrawal.music_id=m.music_id)
		FROM catalog_music m
		LEFT JOIN entries zh ON zh.category='music' AND zh.field='title' AND zh.jp_key=m.title_ja
		LEFT JOIN entry_localizations en ON en.category='music' AND en.field='title'
		 AND en.jp_key=m.title_ja AND en.locale='en-US'
		LEFT JOIN song_lyrics l ON l.music_id=m.music_id
		LEFT JOIN song_lyrics_publications p ON p.music_id=m.music_id
		LEFT JOIN song_lyrics_source_documents d ON d.music_id=m.music_id
		WHERE m.music_id>?`
	args := []any{cursor}
	if newlyWrittenOnly {
		sqlQuery += ` AND m.newly_written=1`
	}
	query = strings.TrimSpace(query)
	if query != "" {
		sqlQuery += ` AND (m.title_ja LIKE ? OR m.title_zh LIKE ? OR m.title_en LIKE ? OR CAST(m.music_id AS TEXT)=?)`
		like := "%" + query + "%"
		args = append(args, like, like, like, query)
	}
	sqlQuery += ` ORDER BY m.music_id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return model.CatalogMusicResponse{}, err
	}
	defer rows.Close()
	response := model.CatalogMusicResponse{Items: []model.CatalogMusicItem{}}
	for rows.Next() {
		var item model.CatalogMusicItem
		var newlyWritten int
		var revision, publishedRevision, sourceDocumentID sql.NullInt64
		var availabilityState sql.NullString
		if err := rows.Scan(&item.MusicID, &item.Title.Japanese, &item.Title.Chinese, &item.Title.English,
			&item.JacketURL, &newlyWritten, &revision, &publishedRevision, &sourceDocumentID, &item.LyricsSourceV3, &availabilityState,
			&item.LyricsWithdrawn); err != nil {
			return model.CatalogMusicResponse{}, err
		}
		item.IsNewlyWrittenMusic = newlyWritten == 1
		if revision.Valid || sourceDocumentID.Valid {
			item.LyricsStatus = "draft"
			if revision.Valid && publishedRevision.Valid {
				item.LyricsStatus = "draft-published"
				if publishedRevision.Int64 == revision.Int64 {
					item.LyricsStatus = "published"
				}
			}
		}
		if availabilityState.Valid {
			item.LyricsAvailabilityState = availabilityState.String
		}
		response.Items = append(response.Items, item)
	}
	if err := rows.Err(); err != nil {
		return model.CatalogMusicResponse{}, err
	}
	if len(response.Items) > limit {
		response.NextCursor = itoa(response.Items[limit-1].MusicID)
		response.Items = response.Items[:limit]
	}
	return response, nil
}

func (s *Store) CatalogPerformers() (model.CatalogPerformerResponse, error) {
	rows, err := s.db.Query(`SELECT performer_id, name_ja, name_zh, name_en FROM catalog_performers ORDER BY performer_id`)
	if err != nil {
		return model.CatalogPerformerResponse{}, err
	}
	defer rows.Close()
	response := model.CatalogPerformerResponse{Items: []model.CatalogPerformerItem{}}
	for rows.Next() {
		var item model.CatalogPerformerItem
		if err := rows.Scan(&item.PerformerID, &item.Name.Japanese, &item.Name.Chinese, &item.Name.English); err != nil {
			return model.CatalogPerformerResponse{}, err
		}
		response.Items = append(response.Items, item)
	}
	return response, rows.Err()
}

type CatalogMusicIdentity struct {
	MusicID              int
	JapaneseTitle        string
	ProducerMetadata     string
	Lyricist             string
	Composer             string
	Arranger             string
	AssetbundleName      string
	VersionHint          string
	LyricsVersion        string
	LyricsVersionKnown   bool
	Vocals               []model.CatalogVocalSignal
	CatalogFingerprint   string
	CatalogPolicyVersion string
}

type LyricsDiscoveryCatalogItem struct {
	MusicID            int
	JapaneseTitle      string
	ProducerMetadata   string
	Evidence           model.CatalogLyricsEvidence
	CatalogFingerprint string
}

func (s *Store) LyricsDiscoveryCatalog() ([]LyricsDiscoveryCatalogItem, error) {
	return s.LyricsDiscoveryCatalogContext(context.Background())
}

func (s *Store) LyricsDiscoveryCatalogContext(ctx context.Context) ([]LyricsDiscoveryCatalogItem, error) {
	if ctx == nil {
		return nil, errors.New("lyrics discovery catalog requires context")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT music_id, title_ja, producer_metadata, lyricist, composer, arranger,
		assetbundle_name, version_hint, lyrics_version, lyrics_evidence_presence_json, vocal_signals_json,
		lyrics_catalog_fingerprint FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []LyricsDiscoveryCatalogItem{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var item LyricsDiscoveryCatalogItem
		var presenceJSON, vocalsJSON string
		if err := rows.Scan(&item.MusicID, &item.JapaneseTitle, &item.ProducerMetadata,
			&item.Evidence.Lyricist, &item.Evidence.Composer, &item.Evidence.Arranger,
			&item.Evidence.Assetbundle, &item.Evidence.VersionHint, &item.Evidence.LyricsVersion,
			&presenceJSON, &vocalsJSON, &item.CatalogFingerprint); err != nil {
			return nil, err
		}
		item.Evidence.Title = item.JapaneseTitle
		if strings.TrimSpace(presenceJSON) == "" {
			presenceJSON = "{}"
		}
		if err := json.Unmarshal([]byte(presenceJSON), &item.Evidence.Presence); err != nil {
			return nil, fmt.Errorf("catalog music %d evidence presence: %w", item.MusicID, err)
		}
		if strings.TrimSpace(vocalsJSON) == "" {
			vocalsJSON = "[]"
		}
		if err := json.Unmarshal([]byte(vocalsJSON), &item.Evidence.Vocals); err != nil {
			return nil, fmt.Errorf("catalog music %d vocal signals: %w", item.MusicID, err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func lyricsDiscoveryCatalogFingerprint(item LyricsDiscoveryCatalogItem) string {
	if item.CatalogFingerprint != "" {
		return item.CatalogFingerprint
	}
	fingerprint, _ := model.CatalogLyricsEvidenceFingerprint(item.Evidence)
	return fingerprint
}

func (s *Store) CatalogMusicIdentity(musicID int) (CatalogMusicIdentity, error) {
	return s.CatalogMusicIdentityContext(context.Background(), musicID)
}

func (s *Store) CatalogMusicIdentityContext(ctx context.Context, musicID int) (CatalogMusicIdentity, error) {
	if ctx == nil {
		return CatalogMusicIdentity{}, errors.New("catalog music identity requires context")
	}
	return loadCatalogMusicIdentityContext(ctx, s.db, musicID)
}

type catalogMusicIdentityQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadCatalogMusicIdentityContext(ctx context.Context, query catalogMusicIdentityQuery, musicID int) (CatalogMusicIdentity, error) {
	var identity CatalogMusicIdentity
	var presenceJSON, vocalsJSON string
	err := query.QueryRowContext(ctx, `SELECT music_id, title_ja, producer_metadata, lyricist, composer, arranger,
		assetbundle_name, version_hint, lyrics_version, lyrics_evidence_presence_json, vocal_signals_json,
		lyrics_catalog_fingerprint, lyrics_catalog_policy_version FROM catalog_music WHERE music_id=?`, musicID).
		Scan(&identity.MusicID, &identity.JapaneseTitle, &identity.ProducerMetadata, &identity.Lyricist,
			&identity.Composer, &identity.Arranger, &identity.AssetbundleName, &identity.VersionHint,
			&identity.LyricsVersion, &presenceJSON, &vocalsJSON, &identity.CatalogFingerprint, &identity.CatalogPolicyVersion)
	if err != nil {
		return identity, err
	}
	if strings.TrimSpace(presenceJSON) == "" {
		presenceJSON = "{}"
	}
	var presence model.CatalogEvidencePresence
	if err := json.Unmarshal([]byte(presenceJSON), &presence); err != nil {
		return identity, err
	}
	identity.LyricsVersionKnown = presence.LyricsVersion
	if strings.TrimSpace(vocalsJSON) == "" {
		vocalsJSON = "[]"
	}
	if err := json.Unmarshal([]byte(vocalsJSON), &identity.Vocals); err != nil {
		return identity, err
	}
	return identity, nil
}

func (s *Store) CatalogLyricsTargets() ([]model.CatalogLyricsTarget, error) {
	items, err := s.LyricsDiscoveryCatalog()
	if err != nil {
		return nil, err
	}
	records := make([]model.CatalogLyricsGroupingRecord, 0, len(items))
	for _, item := range items {
		records = append(records, model.CatalogLyricsGroupingRecord{
			MusicID: item.MusicID, Fingerprint: item.CatalogFingerprint, Evidence: item.Evidence,
		})
	}
	return model.ClassifyCatalogLyricsTargets(records), nil
}

func musicCatalogEvidence(record MusicCatalogRecord) model.CatalogLyricsEvidence {
	lyricsVersion := strings.TrimSpace(strings.ToLower(record.LyricsVersion))
	if !record.LyricsVersionKnown {
		lyricsVersion = "unknown"
	}
	return model.NormalizeCatalogLyricsEvidence(model.CatalogLyricsEvidence{
		Title: record.JapaneseTitle, Lyricist: record.Lyricist, Composer: record.Composer, Arranger: record.Arranger,
		Assetbundle: record.AssetbundleName, VersionHint: record.VersionHint, LyricsVersion: lyricsVersion,
		Vocals: append([]model.CatalogVocalSignal(nil), record.Vocals...), Presence: model.CatalogEvidencePresence{
			Lyricist: strings.TrimSpace(record.Lyricist) != "", Composer: strings.TrimSpace(record.Composer) != "",
			Arranger: strings.TrimSpace(record.Arranger) != "", Assetbundle: strings.TrimSpace(record.AssetbundleName) != "",
			VersionHint: strings.TrimSpace(record.VersionHint) != "", LyricsVersion: record.LyricsVersionKnown,
			Vocals: len(record.Vocals) > 0,
		},
	})
}

func musicCatalogEvidenceSpecified(record MusicCatalogRecord) bool {
	return strings.TrimSpace(record.Lyricist) != "" || strings.TrimSpace(record.Composer) != "" ||
		strings.TrimSpace(record.Arranger) != "" || strings.TrimSpace(record.AssetbundleName) != "" ||
		strings.TrimSpace(record.VersionHint) != "" || record.LyricsVersionKnown || len(record.Vocals) > 0
}

func musicProducerMetadata(lyricist, composer, arranger string) string {
	values := make([]string, 0, 3)
	for _, value := range []string{lyricist, composer, arranger} {
		if value = strings.TrimSpace(value); value != "" && value != "-" {
			values = append(values, value)
		}
	}
	return strings.Join(values, " | ")
}

func (s *Store) catalogMusicExists(q queryRower, musicID int) (bool, error) {
	var count int
	err := q.QueryRow(`SELECT COUNT(*) FROM catalog_music WHERE music_id=?`, musicID).Scan(&count)
	return count == 1, err
}

func (s *Store) performerIDs(q queryRower) (map[int]bool, error) {
	rows, err := q.Query(`SELECT performer_id FROM catalog_performers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := addAuditedExternalLyricsPerformerIDs(result); err != nil {
		return nil, err
	}
	return result, nil
}

type queryRower interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

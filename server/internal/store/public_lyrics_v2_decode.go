package store

import (
	"bytes"
	"context"

	"database/sql"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"

	"moesekai/server/internal/legacy"

	"moesekai/server/internal/model"
	"strconv"
	"strings"
)

func (s *Store) publishedLyricsSnapshot() (PublicLyricsIndexDocument, map[int]PublicLyricsDetailDocument, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}
	defer tx.Rollback()
	performerIDs, err := s.performerIDs(tx)
	if err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}
	catalogPerformers, err := loadCatalogPerformerAliases(tx)
	if err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}
	rows, err := tx.Query(`SELECT p.music_id,p.revision,p.updated_at,p.payload_json,
		m.title_ja,COALESCE(NULLIF(zh.cn_text,''),m.title_zh),COALESCE(NULLIF(en.text,''),m.title_en),
		d.document_id
		FROM song_lyrics_publications p
		JOIN catalog_music m ON m.music_id=p.music_id
		LEFT JOIN entries zh ON zh.category='music' AND zh.field='title' AND zh.jp_key=m.title_ja
		LEFT JOIN entry_localizations en ON en.category='music' AND en.field='title'
		 AND en.jp_key=m.title_ja AND en.locale='en-US'
		LEFT JOIN song_lyrics_source_documents d ON d.music_id=p.music_id
		ORDER BY p.music_id`)
	if err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}
	records := []publicLyricsPublishedRecord{}
	for rows.Next() {
		var record publicLyricsPublishedRecord
		var documentID sql.NullInt64
		if err := rows.Scan(&record.musicID, &record.revision, &record.updatedAt, &record.payload,
			&record.title.Japanese, &record.title.Chinese, &record.title.English, &documentID); err != nil {
			rows.Close()
			return PublicLyricsIndexDocument{}, nil, err
		}
		record.hasSourceDoc = documentID.Valid
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return PublicLyricsIndexDocument{}, nil, err
	}
	if err := rows.Close(); err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}

	index := PublicLyricsIndexDocument{Songs: make([]PublicLyricsIndexSong, 0, len(records))}
	details := make(map[int]PublicLyricsDetailDocument, len(records))
	outputVersion := 0
	for _, record := range records {
		if len(record.title.Japanese) == 0 || len(record.title.Japanese) > model.PublicLyricsMaxTitleBytes ||
			len(record.title.Chinese) > model.PublicLyricsMaxTitleBytes || len(record.title.English) > model.PublicLyricsMaxTitleBytes {
			return PublicLyricsIndexDocument{}, nil, fmt.Errorf("lyrics publication %d title exceeds the public index contract", record.musicID)
		}
		version, err := publicLyricsPayloadVersion(record.payload)
		if err != nil {
			return PublicLyricsIndexDocument{}, nil, fmt.Errorf("lyrics publication %d: %w", record.musicID, err)
		}
		if outputVersion != 0 && outputVersion != version {
			return PublicLyricsIndexDocument{}, nil, errors.New("published lyrics contain a mixed v1/v2 rollout")
		}
		outputVersion = version
		var detail PublicLyricsDetailDocument
		switch version {
		case 1:
			if record.hasSourceDoc {
				return PublicLyricsIndexDocument{}, nil, fmt.Errorf("lyrics publication %d has a source document but a stale v1 payload", record.musicID)
			}
			backupRecord := LyricsPublicationBackupRecord{
				MusicID: record.musicID, Revision: record.revision, UpdatedAt: record.updatedAt, PayloadJSON: record.payload,
			}
			if err := canonicalizeRestoredPublication(&backupRecord, performerIDs); err != nil {
				return PublicLyricsIndexDocument{}, nil, err
			}
			var legacyDetail model.PublicSongLyrics
			if err := json.Unmarshal([]byte(backupRecord.PayloadJSON), &legacyDetail); err != nil {
				return PublicLyricsIndexDocument{}, nil, err
			}
			detail = publicLyricsDetailFromV1(legacyDetail)
		case 2:
			if !record.hasSourceDoc {
				return PublicLyricsIndexDocument{}, nil, fmt.Errorf("lyrics publication %d has a v2 payload without its source document", record.musicID)
			}
			bundle, err := s.loadPublicLyricsSourceBundle(tx, record.musicID)
			if err != nil {
				return PublicLyricsIndexDocument{}, nil, err
			}
			detail, err = decodePublicLyricsV2Detail(
				record.payload, record.musicID, record.revision, record.updatedAt, bundle, catalogPerformers,
			)
			if err != nil {
				return PublicLyricsIndexDocument{}, nil, err
			}
		default:
			return PublicLyricsIndexDocument{}, nil, fmt.Errorf("lyrics publication %d has unsupported public version %d", record.musicID, version)
		}
		item := PublicLyricsIndexSong{
			MusicID: record.musicID, Revision: record.revision, UpdatedAt: formatTimestamp(record.updatedAt), Title: record.title,
		}
		if version == 2 {
			item.State = detail.State
			item.AvailableVersions = append([]string{}, detail.AvailableVersions...)
		}
		index.Songs = append(index.Songs, item)
		details[record.musicID] = detail
	}
	if outputVersion == 0 {
		var sourceDocuments int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM song_lyrics_source_documents`).Scan(&sourceDocuments); err != nil {
			return PublicLyricsIndexDocument{}, nil, err
		}
		outputVersion = 1
		if sourceDocuments > 0 {
			outputVersion = 2
		}
	}
	index.Version = outputVersion
	if len(index.Songs) > model.PublicLyricsMaxIndexEntries {
		return PublicLyricsIndexDocument{}, nil, errors.New("published lyrics index exceeds the maximum song count")
	}
	if err := validatePublicLyricsArtifactSize(index); err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return PublicLyricsIndexDocument{}, nil, err
	}
	return index, details, nil
}

func publicLyricsPayloadVersion(payload string) (int, error) {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return 0, err
	}
	return envelope.Version, nil
}

// publicLyricsV1Attributions derives the public source card from a legacy v1
// payload that carries extraction-source fields. Sekaipedia sources receive
// their canonical CC BY-SA 4.0 license pair; unrecognized sources stay
// unattributed instead of manufacturing provider metadata. pjsk.moe rejects
// the whole song for a non-canonical revision URL, so a source that fails the
// public v3 revision URL rules keeps only the plain attribution.
func publicLyricsV1Attributions(public model.PublicSongLyrics) []PublicLyricsAttribution {
	provider := model.LyricsSourceProvider("")
	if strings.Contains(public.SourceURL, "sekaipedia.org/") {
		provider = model.LyricsSourceProviderSekaipedia
	} else if strings.Contains(public.SourceURL, "fandom.com/") || strings.Contains(public.SourceURL, "wikia.com/") {
		provider = model.LyricsSourceProviderVocaloidFandom
	}
	if provider == "" || public.SourceURL == "" || public.SourcePageID <= 0 || public.LicenseName == "" {
		return nil
	}
	title := ""
	if path, err := url.Parse(public.SourceURL); err == nil && path.Path != "" {
		segment := path.Path
		if index := strings.LastIndex(segment, "/"); index >= 0 {
			segment = segment[index+1:]
		}
		if decoded, err := url.PathUnescape(segment); err == nil {
			title = strings.TrimSpace(strings.ReplaceAll(decoded, "_", " "))
		}
	}
	revisionURL := public.SourceURL
	if u, err := url.Parse(public.SourceURL); err == nil && public.SourceRevisionID > 0 {
		q := u.Query()
		if q.Get("oldid") == "" {
			q.Set("oldid", strconv.Itoa(public.SourceRevisionID))
			u.RawQuery = q.Encode()
		}
		revisionURL = u.String()
	}
	if title == "" || !validPublicV3RevisionURL(PublicLyricsV3ComponentAttribution{
		Provider: provider, RevisionID: public.SourceRevisionID, RevisionURL: revisionURL,
	}) {
		return nil
	}
	return []PublicLyricsAttribution{{
		Provider:    provider,
		Title:       title,
		RevisionID:  public.SourceRevisionID,
		RevisionURL: revisionURL,
		LicenseName: public.LicenseName,
		LicenseURL:  public.LicenseURL,
	}}
}

func publicLyricsDetailFromV1(public model.PublicSongLyrics) PublicLyricsDetailDocument {
	lines := make([]PublicLyricsLine, len(public.Lines))
	for lineIndex, source := range public.Lines {
		line := PublicLyricsLine{
			ID: source.ID, Order: source.Order, Japanese: source.Japanese,
			Chinese: source.Chinese, English: source.English,
			StanzaBreakBefore: source.StanzaBreakBefore,
			Segments:          append([]model.LyricSegment(nil), source.Segments...),
		}
		for segmentIndex := range line.Segments {
			line.Segments[segmentIndex].PerformerIDs = append([]int{}, source.Segments[segmentIndex].PerformerIDs...)
			line.Segments[segmentIndex].Ruby = nil
		}
		lines[lineIndex] = line
	}
	return PublicLyricsDetailDocument{
		Version: public.Version, MusicID: public.MusicID, Revision: public.Revision,
		UpdatedAt: public.UpdatedAt, Attribution: public.Attribution, Lines: lines,
		Attributions: publicLyricsV1Attributions(public),
	}
}

func decodePublicLyricsV2Detail(payload string, musicID, revision int, updatedAt int64, bundle *publicLyricsSourceBundle,
	catalogPerformers catalogPerformerAliases,
) (PublicLyricsDetailDocument, error) {
	body := []byte(payload)
	if err := legacy.ValidateUniqueJSON(body); err != nil {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d: %w", musicID, err)
	}
	if err := validatePublicLyricsV2JSONShape(body); err != nil {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d: %w", musicID, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var public PublicLyricsDetailDocument
	if err := decoder.Decode(&public); err != nil {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d: %w", musicID, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d has trailing JSON", musicID)
	}
	parsedUpdatedAt, err := parseTimestamp(public.UpdatedAt)
	if err != nil || public.Version != 2 || public.MusicID != musicID || public.Revision != revision || parsedUpdatedAt != updatedAt {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d identity does not match its publication row", musicID)
	}
	if err := validatePublicLyricsV2Detail(public, bundle, catalogPerformers); err != nil {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d: %w", musicID, err)
	}
	if err := validatePublicLyricsArtifactSize(public); err != nil {
		return PublicLyricsDetailDocument{}, fmt.Errorf("lyrics publication %d: %w", musicID, err)
	}
	return public, nil
}

func validatePublicLyricsV2JSONShape(body []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return err
	}
	if err := publicLyricsJSONObjectFields(top,
		[]string{"version", "musicId", "revision", "updatedAt", "attributions", "availableVersions", "lines"},
		[]string{"state", "noLyricsReason", "translationCredits", "gameProjection"}); err != nil {
		return err
	}
	for _, field := range []string{"version", "musicId", "revision"} {
		if !publicLyricsJSONKind(top[field], 'n') {
			return fmt.Errorf("public lyrics v2 field %s must be an integer", field)
		}
	}
	if !publicLyricsJSONKind(top["updatedAt"], 's') || !publicLyricsJSONKind(top["attributions"], 'a') ||
		!publicLyricsJSONKind(top["availableVersions"], 'a') || !publicLyricsJSONKind(top["lines"], 'a') {
		return errors.New("public lyrics v2 header has invalid JSON types")
	}
	for _, field := range []string{"state", "noLyricsReason"} {
		if value, exists := top[field]; exists && !publicLyricsJSONKind(value, 's') {
			return fmt.Errorf("public lyrics v2 field %s must be a string", field)
		}
	}
	if rawCredits, exists := top["translationCredits"]; exists {
		if !publicLyricsJSONKind(rawCredits, 'o') {
			return errors.New("public lyrics v2 translationCredits must be an object")
		}
		var credits map[string]json.RawMessage
		if err := json.Unmarshal(rawCredits, &credits); err != nil {
			return err
		}
		if len(credits) == 0 {
			return errors.New("public lyrics v2 translationCredits must not be empty")
		}
		if err := publicLyricsJSONObjectFields(credits, nil, []string{"translation", "proofreading"}); err != nil {
			return err
		}
		for field, value := range credits {
			if !publicLyricsJSONKind(value, 's') {
				return fmt.Errorf("public lyrics v2 translationCredits field %s must be a string", field)
			}
		}
	}
	var attributions []map[string]json.RawMessage
	if err := json.Unmarshal(top["attributions"], &attributions); err != nil {
		return err
	}
	for _, attribution := range attributions {
		if err := publicLyricsJSONObjectFields(attribution,
			[]string{"provider", "title", "revisionId", "revisionUrl", "licenseName", "licenseUrl"}, nil); err != nil {
			return err
		}
		for _, field := range []string{"provider", "title", "revisionUrl", "licenseName", "licenseUrl"} {
			if !publicLyricsJSONKind(attribution[field], 's') {
				return fmt.Errorf("public lyrics v2 attribution field %s must be a string", field)
			}
		}
		if !publicLyricsJSONKind(attribution["revisionId"], 'n') {
			return errors.New("public lyrics v2 attribution revisionId must be an integer")
		}
	}
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(top["lines"], &lines); err != nil {
		return err
	}
	for _, line := range lines {
		if err := publicLyricsJSONObjectFields(line,
			[]string{"id", "order", "japanese", "zh-CN", "en-US", "segments"},
			[]string{"stanzaBreakBefore", "trailingPerformerIds"}); err != nil {
			return err
		}
		for _, field := range []string{"id", "japanese", "zh-CN", "en-US"} {
			if !publicLyricsJSONKind(line[field], 's') {
				return fmt.Errorf("public lyrics v2 line field %s must be a string", field)
			}
		}
		if !publicLyricsJSONKind(line["order"], 'n') || !publicLyricsJSONKind(line["segments"], 'a') {
			return errors.New("public lyrics v2 line has invalid order or segments type")
		}
		if stanza, exists := line["stanzaBreakBefore"]; exists && !publicLyricsJSONKind(stanza, 'b') {
			return errors.New("public lyrics v2 stanzaBreakBefore must be a boolean")
		}
		if trailing, exists := line["trailingPerformerIds"]; exists && !publicLyricsJSONKind(trailing, 'a') {
			return errors.New("public lyrics v2 trailingPerformerIds must be an array")
		}
		var segments []map[string]json.RawMessage
		if err := json.Unmarshal(line["segments"], &segments); err != nil {
			return err
		}
		for _, segment := range segments {
			if err := publicLyricsJSONObjectFields(segment, []string{"text", "performerIds", "ruby"}, nil); err != nil {
				return err
			}
			if !publicLyricsJSONKind(segment["text"], 's') || !publicLyricsJSONKind(segment["performerIds"], 'a') ||
				!publicLyricsJSONKind(segment["ruby"], 'a') {
				return errors.New("public lyrics v2 segment has invalid JSON types")
			}
			var ruby []map[string]json.RawMessage
			if err := json.Unmarshal(segment["ruby"], &ruby); err != nil {
				return err
			}
			for _, span := range ruby {
				if err := publicLyricsJSONObjectFields(span, []string{"text"}, []string{"reading"}); err != nil {
					return err
				}
				if !publicLyricsJSONKind(span["text"], 's') {
					return errors.New("public lyrics v2 ruby text must be a string")
				}
				if reading, exists := span["reading"]; exists && !publicLyricsJSONKind(reading, 's') {
					return errors.New("public lyrics v2 ruby reading must be a string")
				}
			}
		}
	}
	if projection, exists := top["gameProjection"]; exists {
		if !publicLyricsJSONKind(projection, 'o') {
			return errors.New("public lyrics v2 gameProjection must be an object")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(projection, &object); err != nil {
			return err
		}
		if err := publicLyricsJSONObjectFields(object, []string{"reasonCode", "lineIds"}, nil); err != nil {
			return err
		}
		if !publicLyricsJSONKind(object["reasonCode"], 's') || !publicLyricsJSONKind(object["lineIds"], 'a') {
			return errors.New("public lyrics v2 gameProjection has invalid JSON types")
		}
	}
	return nil
}

func publicLyricsJSONObjectFields(object map[string]json.RawMessage, required, optional []string) error {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = true
		if _, exists := object[field]; !exists {
			return fmt.Errorf("public lyrics v2 is missing required field %s", field)
		}
	}
	for _, field := range optional {
		allowed[field] = true
	}
	for field := range object {
		if !allowed[field] {
			return fmt.Errorf("public lyrics v2 contains unknown field %s", field)
		}
	}
	return nil
}

func publicLyricsJSONKind(raw json.RawMessage, kind byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch kind {
	case 'a':
		return raw[0] == '['
	case 'b':
		return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
	case 'n':
		return raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9'
	case 'o':
		return raw[0] == '{'
	case 's':
		return raw[0] == '"'
	default:
		return false
	}
}

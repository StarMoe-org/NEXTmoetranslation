package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"moesekai/server/internal/textfonts"
	"sort"
	"strings"
	"time"
)

type MysekaiObject struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type MysekaiOverlayEntry struct {
	ID         string `json:"id"`
	SourceHash string `json:"sourceHash"`
	Text       string `json:"text"`
	Source     string `json:"source"`
}
type MysekaiOverlay struct {
	Schema         string                `json:"schema"`
	SourceRegion   string                `json:"sourceRegion"`
	SourceRevision string                `json:"sourceRevision"`
	Locale         string                `json:"locale"`
	Revision       string                `json:"revision"`
	Entries        []MysekaiOverlayEntry `json:"entries"`
}
type MysekaiPublication struct {
	Schema        string                   `json:"schema"`
	Region        string                   `json:"region"`
	Locale        string                   `json:"locale"`
	Revision      string                   `json:"revision"`
	Routing       string                   `json:"routing"`
	Buckets       map[string]MysekaiObject `json:"buckets"`
	SearchBuckets map[string]MysekaiObject `json:"searchBuckets,omitempty"`
}

func validateMysekaiPublication(region, locale, revision string, files map[string]json.RawMessage) error {
	if err := mysekaiScope(region, locale); err != nil {
		return err
	}
	var root MysekaiPublication
	if err := json.Unmarshal(files["latest.json"], &root); err != nil {
		return err
	}
	if root.Schema != "moly-text-publication/1" || root.Region != region || root.Locale != locale || root.Revision != revision || root.Routing != "sha256-utf8-prefix-2" || len(root.Buckets) > 256 {
		return fmt.Errorf("invalid MYSEKAI publication root")
	}
	original := root.Revision
	root.Revision = ""
	b, _ := json.Marshal(root)
	if mysekaiDigest(b) != original {
		return fmt.Errorf("MYSEKAI publication revision mismatch")
	}
	root.Revision = original
	if string(files["latest.json"]) != string(files["revisions/"+revision+".json"]) {
		return fmt.Errorf("MYSEKAI revision manifest mismatch")
	}
	used := map[string]bool{"latest.json": true, "revisions/" + revision + ".json": true}
	get := func(ref MysekaiObject) ([]byte, error) {
		body, ok := files[ref.Path]
		if !ok || !mysekaiHash.MatchString(ref.SHA256) || ref.Path != "objects/"+ref.SHA256+".json" || len(body) != ref.Bytes || mysekaiDigest(body) != ref.SHA256 {
			return nil, fmt.Errorf("invalid MYSEKAI object reference")
		}
		used[ref.Path] = true
		return body, nil
	}
	for prefix, ref := range root.Buckets {
		if len(prefix) != 2 || strings.Trim(prefix, "0123456789abcdef") != "" {
			return fmt.Errorf("invalid MYSEKAI routing prefix")
		}
		body, err := get(ref)
		if err != nil {
			return err
		}
		var bucket struct {
			Schema  string                   `json:"schema"`
			Scripts map[string]MysekaiObject `json:"scripts"`
		}
		if err = json.Unmarshal(body, &bucket); err != nil {
			return err
		}
		if bucket.Schema != "moly-text-translation-routes/1" {
			return fmt.Errorf("invalid MYSEKAI routes")
		}
		for key, scriptRef := range bucket.Scripts {
			if mysekaiDigest([]byte(key))[:2] != prefix {
				return fmt.Errorf("MYSEKAI route in wrong bucket")
			}
			body, err = get(scriptRef)
			if err != nil {
				return err
			}
			var overlay MysekaiOverlay
			if err = json.Unmarshal(body, &overlay); err != nil {
				return err
			}
			if overlay.Schema != "moly-text-overlay/1" || overlay.SourceRegion != region || overlay.Locale != locale || !mysekaiHash.MatchString(overlay.SourceRevision) {
				return fmt.Errorf("invalid MYSEKAI overlay")
			}
			b, _ = json.Marshal(overlay.Entries)
			if mysekaiDigest(b) != overlay.Revision {
				return fmt.Errorf("MYSEKAI overlay revision mismatch")
			}
			seen := map[string]bool{}
			for _, entry := range overlay.Entries {
				if entry.ID == "" || seen[entry.ID] || !mysekaiHash.MatchString(entry.SourceHash) || entry.Source != "human" || !mysekaiText(entry.Text) || strings.TrimSpace(entry.Text) == "" {
					return fmt.Errorf("invalid MYSEKAI overlay entry")
				}
				seen[entry.ID] = true
			}
		}
	}
	for prefix, ref := range root.SearchBuckets {
		if len(prefix) != 1 || strings.Trim(prefix, "0123456789abcdef") != "" {
			return fmt.Errorf("invalid MYSEKAI search prefix")
		}
		body, err := get(ref)
		if err != nil {
			return err
		}
		var search struct {
			Schema  string                 `json:"schema"`
			Scripts map[string][][4]string `json:"scripts"`
		}
		if err = json.Unmarshal(body, &search); err != nil {
			return err
		}
		if search.Schema != "moly-text-translation-search/1" {
			return fmt.Errorf("invalid MYSEKAI search index")
		}
		for key, entries := range search.Scripts {
			if mysekaiDigest([]byte(key))[:1] != prefix {
				return fmt.Errorf("MYSEKAI search in wrong bucket")
			}
			seen := map[string]bool{}
			for _, entry := range entries {
				if entry[0] == "" || len(entry[0]) > 160 || seen[entry[0]] || !mysekaiHash.MatchString(entry[1]) || !mysekaiText(entry[2]) || entry[3] != "human" {
					return fmt.Errorf("invalid MYSEKAI search entry")
				}
				seen[entry[0]] = true
			}
		}
	}
	if len(used) != len(files) {
		return fmt.Errorf("unreferenced MYSEKAI publication object")
	}
	return nil
}

func mysekaiBuildPublication(ctx context.Context, tx *sql.Tx, region, locale, expectedSource string, fonts textfonts.Catalog) (string, map[string]json.RawMessage, error) {
	var revision, catalogRaw string
	err := tx.QueryRowContext(ctx, `SELECT c.revision,s.catalog FROM mysekai_current_sources c JOIN mysekai_sources s ON s.region=c.region AND s.revision=c.revision WHERE c.region=?`, region).Scan(&revision, &catalogRaw)
	if err != nil {
		return "", nil, err
	}
	if revision != expectedSource {
		return "", nil, ErrMysekaiConflict
	}
	var catalog MysekaiCatalog
	if err = json.Unmarshal([]byte(catalogRaw), &catalog); err != nil {
		return "", nil, err
	}
	drafts := map[string]MysekaiDraft{}
	rows, err := tx.QueryContext(ctx, `SELECT line_id,source_hash,text FROM mysekai_drafts WHERE region=? AND locale=? ORDER BY line_id`, region, locale)
	if err != nil {
		return "", nil, err
	}
	for rows.Next() {
		var d MysekaiDraft
		if err = rows.Scan(&d.ID, &d.SourceHash, &d.Text); err != nil {
			rows.Close()
			return "", nil, err
		}
		drafts[d.ID] = d
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", nil, err
	}
	scripts := map[string]MysekaiScript{}
	for _, script := range catalog.Scripts {
		scripts[script.Key] = script
	}
	files := map[string]json.RawMessage{}
	put := func(value any) MysekaiObject {
		b, _ := json.Marshal(value)
		hash := mysekaiDigest(b)
		path := "objects/" + hash + ".json"
		files[path] = b
		return MysekaiObject{path, hash, len(b)}
	}
	buckets := map[string]map[string]MysekaiObject{}
	searchBuckets := map[string]map[string][][4]string{}
	for _, script := range catalog.Scripts {
		entries := []MysekaiOverlayEntry{}
		parts := []MysekaiScript{script}
		for _, key := range mysekaiTweetKeys(script) {
			parts = append(parts, scripts[key])
		}
		for _, part := range parts {
			for _, l := range part.Lines {
				d := drafts[l.ID]
				if d.SourceHash == l.SourceHash && strings.TrimSpace(d.Text) != "" {
					if missing := fonts.Missing(d.Text); len(missing) > 0 {
						return "", nil, fmt.Errorf("译文字体不支持 %s（%s）；草稿已保留，请修改文字或联系管理员更新字体", strings.Join(missing, " "), l.ID)
					}
					entries = append(entries, MysekaiOverlayEntry{l.ID, l.SourceHash, d.Text, "human"})
				}
			}
		}
		if len(entries) == 0 {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		b, _ := json.Marshal(entries)
		sourceLines := []MysekaiLine{}
		for _, part := range parts {
			sourceLines = append(sourceLines, part.Lines...)
		}
		sourceBytes, _ := json.Marshal(sourceLines)
		overlay := MysekaiOverlay{"moly-text-overlay/1", region, mysekaiDigest(sourceBytes), locale, mysekaiDigest(b), entries}
		prefix := mysekaiDigest([]byte(script.Key))[:2]
		if buckets[prefix] == nil {
			buckets[prefix] = map[string]MysekaiObject{}
		}
		buckets[prefix][script.Key] = put(overlay)
		ownEntries := [][4]string{}
		for _, line := range script.Lines {
			d := drafts[line.ID]
			if d.SourceHash == line.SourceHash && strings.TrimSpace(d.Text) != "" {
				ownEntries = append(ownEntries, [4]string{line.ID, line.SourceHash, d.Text, "human"})
			}
		}
		if len(ownEntries) > 0 {
			if searchBuckets[prefix[:1]] == nil {
				searchBuckets[prefix[:1]] = map[string][][4]string{}
			}
			searchBuckets[prefix[:1]][script.Key] = ownEntries
		}
	}
	root := MysekaiPublication{Schema: "moly-text-publication/1", Region: region, Locale: locale, Routing: "sha256-utf8-prefix-2", Buckets: map[string]MysekaiObject{}}
	for prefix, scripts := range buckets {
		root.Buckets[prefix] = put(struct {
			Schema  string                   `json:"schema"`
			Scripts map[string]MysekaiObject `json:"scripts"`
		}{"moly-text-translation-routes/1", scripts})
	}
	if len(searchBuckets) > 0 {
		root.SearchBuckets = map[string]MysekaiObject{}
	}
	for prefix, scripts := range searchBuckets {
		root.SearchBuckets[prefix] = put(struct {
			Schema  string                 `json:"schema"`
			Scripts map[string][][4]string `json:"scripts"`
		}{"moly-text-translation-search/1", scripts})
	}
	b, _ := json.Marshal(root)
	root.Revision = mysekaiDigest(b)
	b, _ = json.Marshal(root)
	files["latest.json"] = b
	files["revisions/"+root.Revision+".json"] = b
	return root.Revision, files, nil
}

// SetMysekaiPublication switches only the public pointer. Previous immutable
// files and every human draft remain available for pinned sessions and rollback.
func (s *Store) SetMysekaiPublication(ctx context.Context, r MysekaiPublicationRequest, action, actor string) (MysekaiChannel, error) {
	var fonts textfonts.Catalog
	if action == "publish" {
		var err error
		fonts, err = s.MysekaiFontCoverage(ctx)
		if err != nil {
			return MysekaiChannel{}, err
		}
	}
	if err := mysekaiScope(r.Region, r.Locale); err != nil {
		return MysekaiChannel{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MysekaiChannel{}, err
	}
	defer tx.Rollback()
	channel, err := mysekaiChannel(ctx, tx, r.Region, r.Locale)
	if err != nil {
		return channel, err
	}
	if channel.Revision != r.ExpectedRevision {
		return channel, ErrMysekaiConflict
	}
	publication := ""
	now := time.Now().Unix()
	switch action {
	case "publish":
		draftRevision, e := mysekaiDraftRevision(ctx, tx, r.Region, r.Locale)
		if e != nil {
			return channel, e
		}
		if draftRevision != r.DraftRevision {
			return channel, ErrMysekaiConflict
		}
		var files map[string]json.RawMessage
		publication, files, err = mysekaiBuildPublication(ctx, tx, r.Region, r.Locale, r.SourceRevision, fonts)
		if err != nil {
			return channel, err
		}
		for path, body := range files {
			if !strings.HasPrefix(path, "objects/") {
				continue
			}
			hash := mysekaiDigest(body)
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO mysekai_objects VALUES(?,?)`, hash, string(body)); err != nil {
				return channel, err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO mysekai_publications VALUES(?,?,?,?,?,?)`, r.Region, r.Locale, publication, string(files["latest.json"]), now, actor); err != nil {
			return channel, err
		}
	case "rollback":
		if !mysekaiHash.MatchString(r.Publication) {
			return channel, fmt.Errorf("invalid publication")
		}
		var found string
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM mysekai_publications WHERE region=? AND locale=? AND revision=?`, r.Region, r.Locale, r.Publication).Scan(&found); err != nil {
			return channel, err
		}
		publication = found
	case "unpublish":
	default:
		return channel, fmt.Errorf("invalid publication action")
	}
	channel.Revision++
	channel.Publication = publication
	if _, err = tx.ExecContext(ctx, `INSERT INTO mysekai_channels VALUES(?,?,?,?) ON CONFLICT(region,locale) DO UPDATE SET revision=excluded.revision,publication=excluded.publication`, r.Region, r.Locale, channel.Revision, publication); err != nil {
		return channel, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO mysekai_publication_history VALUES(?,?,?,?,?,?,?)`, r.Region, r.Locale, channel.Revision, publication, action, now, actor); err != nil {
		return channel, err
	}
	err = tx.Commit()
	if err == nil {
		s.NotifyChange()
	}
	return channel, err
}

type MysekaiPublicationHistory struct {
	Revision    int    `json:"revision"`
	Publication string `json:"publication"`
	Action      string `json:"action"`
	UpdatedAt   int64  `json:"updatedAt"`
	UpdatedBy   string `json:"updatedBy"`
}

func (s *Store) MysekaiPublicationHistory(ctx context.Context, region, locale string) ([]MysekaiPublicationHistory, error) {
	if err := mysekaiScope(region, locale); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision,publication,action,updated_at,updated_by FROM mysekai_publication_history WHERE region=? AND locale=? ORDER BY revision DESC LIMIT 100`, region, locale)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MysekaiPublicationHistory{}
	for rows.Next() {
		var h MysekaiPublicationHistory
		if err = rows.Scan(&h.Revision, &h.Publication, &h.Action, &h.UpdatedAt, &h.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *Store) MysekaiPublicFiles(ctx context.Context) (map[string][]byte, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := map[string][]byte{}
	rows, err := tx.QueryContext(ctx, `SELECT p.region,p.locale,p.revision,p.assets,COALESCE(c.publication,'') FROM mysekai_publications p LEFT JOIN mysekai_channels c ON c.region=p.region AND c.locale=p.locale ORDER BY p.region,p.locale,p.revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var region, locale, rev, raw, current string
		if err = rows.Scan(&region, &locale, &rev, &raw, &current); err != nil {
			return nil, err
		}
		files, resolveErr := mysekaiResolvePublication(ctx, tx, []byte(raw))
		if resolveErr != nil {
			return nil, resolveErr
		}
		base := "mysekai/" + region + "/" + locale + "/"
		for path, b := range files {
			if path == "latest.json" && rev != current {
				continue
			}
			out[base+path] = b
		}
	}
	return out, rows.Err()
}

// Stored publications contain only their root. Leaf and bucket bytes live once
// in the SHA-256 object table, shared by every historical publication.
func mysekaiResolvePublication(ctx context.Context, q mysekaiReader, rootBytes []byte) (map[string]json.RawMessage, error) {
	var root MysekaiPublication
	if err := json.Unmarshal(rootBytes, &root); err != nil {
		return nil, err
	}
	if !mysekaiHash.MatchString(root.Revision) {
		return nil, fmt.Errorf("invalid MYSEKAI stored root")
	}
	files := map[string]json.RawMessage{"latest.json": rootBytes, "revisions/" + root.Revision + ".json": rootBytes}
	get := func(ref MysekaiObject) ([]byte, error) {
		if !mysekaiHash.MatchString(ref.SHA256) || ref.Path != "objects/"+ref.SHA256+".json" {
			return nil, fmt.Errorf("invalid MYSEKAI object path")
		}
		if b, ok := files[ref.Path]; ok {
			return b, nil
		}
		var body string
		if err := q.QueryRowContext(ctx, `SELECT body FROM mysekai_objects WHERE sha256=?`, ref.SHA256).Scan(&body); err != nil {
			return nil, err
		}
		if len(body) != ref.Bytes || mysekaiDigest([]byte(body)) != ref.SHA256 {
			return nil, fmt.Errorf("corrupt MYSEKAI object")
		}
		files[ref.Path] = []byte(body)
		return []byte(body), nil
	}
	for _, ref := range root.Buckets {
		body, err := get(ref)
		if err != nil {
			return nil, err
		}
		var bucket struct {
			Scripts map[string]MysekaiObject `json:"scripts"`
		}
		if err = json.Unmarshal(body, &bucket); err != nil {
			return nil, err
		}
		for _, ref := range bucket.Scripts {
			if _, err = get(ref); err != nil {
				return nil, err
			}
		}
	}
	for _, ref := range root.SearchBuckets {
		if _, err := get(ref); err != nil {
			return nil, err
		}
	}
	return files, nil
}

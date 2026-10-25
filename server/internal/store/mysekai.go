package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrMysekaiConflict = errors.New("MYSEKAI source or edit revision changed; reload before saving")
var mysekaiHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var mysekaiRegion = regexp.MustCompile(`^(jp|cn|tw|en|kr)$`)
var mysekaiLocale = regexp.MustCompile(`^(zh-CN|zh-TW|en-US|ko-KR|ja-JP)$`)

type MysekaiLine struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Step        *int     `json:"step"`
	Text        string   `json:"text"`
	SourceHash  string   `json:"sourceHash"`
	Speaker     string   `json:"speaker,omitempty"`
	SpeakerUnit *int     `json:"speakerUnit,omitempty"`
	Cues        []string `json:"cues,omitempty"`
}
type MysekaiScript struct {
	Key          string            `json:"key"`
	ScriptHash   string            `json:"scriptHash"`
	Contents     []json.RawMessage `json:"contents"`
	Lines        []MysekaiLine     `json:"lines"`
	RetiredLines []json.RawMessage `json:"retiredLines,omitempty"`
}
type MysekaiCatalog struct {
	Schema         string            `json:"schema"`
	Region         string            `json:"region"`
	Revision       string            `json:"revision"`
	SourceRevision string            `json:"sourceRevision"`
	Scripts        []MysekaiScript   `json:"scripts"`
	RetiredScripts []json.RawMessage `json:"retiredScripts,omitempty"`
}
type MysekaiDraft struct {
	ID         string `json:"id"`
	SourceHash string `json:"sourceHash"`
	Text       string `json:"text"`
	Revision   int    `json:"revision"`
	UpdatedAt  int64  `json:"updatedAt"`
	UpdatedBy  string `json:"updatedBy"`
}
type MysekaiDetail struct {
	Region         string                  `json:"region"`
	Locale         string                  `json:"locale"`
	SourceRevision string                  `json:"sourceRevision"`
	Script         MysekaiScript           `json:"script"`
	Related        []MysekaiScript         `json:"related"`
	Drafts         map[string]MysekaiDraft `json:"drafts"`
}
type MysekaiEdit struct {
	ID               string `json:"id"`
	SourceHash       string `json:"sourceHash"`
	Text             string `json:"text"`
	ExpectedRevision int    `json:"expectedRevision"`
}
type MysekaiSave struct {
	Region         string        `json:"region"`
	Locale         string        `json:"locale"`
	ScriptKey      string        `json:"scriptKey"`
	SourceRevision string        `json:"sourceRevision"`
	Edits          []MysekaiEdit `json:"edits"`
}
type MysekaiChannel struct {
	Region      string `json:"region"`
	Locale      string `json:"locale"`
	Revision    int    `json:"revision"`
	Publication string `json:"publication"`
}
type MysekaiPublicationRequest struct {
	Region           string `json:"region"`
	Locale           string `json:"locale"`
	ExpectedRevision int    `json:"expectedRevision"`
	SourceRevision   string `json:"sourceRevision"`
	Publication      string `json:"publication,omitempty"`
	DraftRevision    string `json:"draftRevision"`
}

func mysekaiScope(region, locale string) error {
	if !mysekaiRegion.MatchString(region) || !mysekaiLocale.MatchString(locale) {
		return fmt.Errorf("unsupported region or locale")
	}
	return nil
}
func mysekaiText(text string) bool {
	if !utf8.ValidString(text) || len(text) > 16*1024 {
		return false
	}
	for _, r := range text {
		if (unicode.IsControl(r) && r != '\n') || r == 0x00ad ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
			(r >= 0xfdd0 && r <= 0xfdef) || (r&0xffff) >= 0xfffe {
			return false
		}
	}
	return true
}
func validateMysekaiCatalog(c MysekaiCatalog) error {
	if c.Schema != "moly-text-source/1" || !mysekaiRegion.MatchString(c.Region) || !mysekaiHash.MatchString(c.Revision) || len(c.Scripts) == 0 || len(c.Scripts) > 100000 {
		return fmt.Errorf("invalid source catalog")
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, s := range c.Scripts {
		if s.Key == "" || len(s.Key) > 512 || keys[s.Key] || len(s.Lines) > 10000 {
			return fmt.Errorf("invalid or duplicate script key")
		}
		keys[s.Key] = true
		for _, l := range s.Lines {
			if l.ID == "" || len(l.ID) > 160 || ids[l.ID] || !mysekaiHash.MatchString(l.SourceHash) || !mysekaiSourceText(l.Text) || (l.Kind != "text" && l.Kind != "speaker" && l.Kind != "tweet") || (l.Step != nil && *l.Step < 0) || ((l.Kind == "tweet") != (l.Step == nil)) {
				return fmt.Errorf("invalid or duplicate source line")
			}
			ids[l.ID] = true
		}
	}
	for _, s := range c.Scripts {
		for _, key := range mysekaiTweetKeys(s) {
			if !keys[key] {
				return fmt.Errorf("missing linked tweet %s", key)
			}
		}
	}
	return nil
}
func mysekaiTweetKeys(s MysekaiScript) []string {
	set := map[string]bool{}
	for _, raw := range s.Contents {
		var c struct {
			TweetKey string `json:"tweetKey"`
		}
		if json.Unmarshal(raw, &c) == nil && c.TweetKey != "" {
			set[c.TweetKey] = true
		}
	}
	keys := []string{}
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func (s *Store) ImportMysekaiSource(ctx context.Context, raw []byte, expected, actor string) (MysekaiCatalog, error) {
	var c MysekaiCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if err := validateMysekaiCatalog(c); err != nil {
		return c, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT revision FROM mysekai_current_sources WHERE region=?`, c.Region).Scan(&current)
	if err != nil && err != sql.ErrNoRows {
		return c, err
	}
	if current != expected {
		return c, ErrMysekaiConflict
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT catalog FROM mysekai_sources WHERE region=? AND revision=?`, c.Region, c.Revision).Scan(&stored)
	if err == nil {
		aa := mysekaiSourceIdentity([]byte(stored))
		bb := mysekaiSourceIdentity(raw)
		if string(aa) != string(bb) {
			return c, fmt.Errorf("source revision reused with different content")
		}
	} else if err != sql.ErrNoRows {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO mysekai_sources VALUES(?,?,?,?,?)`, c.Region, c.Revision, string(raw), time.Now().Unix(), actor); err != nil {
		return c, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mysekai_scripts SET active=0 WHERE region=?`, c.Region); err != nil {
		return c, err
	}
	for _, script := range c.Scripts {
		b, _ := json.Marshal(script)
		if _, err = tx.ExecContext(ctx, `INSERT INTO mysekai_scripts VALUES(?,?,?,1) ON CONFLICT(region,script_key) DO UPDATE SET document=excluded.document,active=1`, c.Region, script.Key, string(b)); err != nil {
			return c, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO mysekai_current_sources VALUES(?,?) ON CONFLICT(region) DO UPDATE SET revision=excluded.revision`, c.Region, c.Revision); err != nil {
		return c, err
	}
	if err = tx.Commit(); err == nil {
		s.NotifyChange()
	}
	return c, err
}

type mysekaiReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func mysekaiDetail(ctx context.Context, q mysekaiReader, region, locale, key string) (MysekaiDetail, error) {
	d := MysekaiDetail{Region: region, Locale: locale, Drafts: map[string]MysekaiDraft{}, Related: []MysekaiScript{}}
	if err := mysekaiScope(region, locale); err != nil {
		return d, err
	}
	if err := q.QueryRowContext(ctx, `SELECT revision FROM mysekai_current_sources WHERE region=?`, region).Scan(&d.SourceRevision); err != nil {
		return d, err
	}
	read := func(key string) (MysekaiScript, error) {
		var raw string
		var script MysekaiScript
		err := q.QueryRowContext(ctx, `SELECT document FROM mysekai_scripts WHERE region=? AND script_key=? AND active=1`, region, key).Scan(&raw)
		if err == nil {
			err = json.Unmarshal([]byte(raw), &script)
		}
		return script, err
	}
	var err error
	d.Script, err = read(key)
	if err != nil {
		return d, err
	}
	for _, key := range mysekaiTweetKeys(d.Script) {
		r, e := read(key)
		if e != nil {
			return d, e
		}
		d.Related = append(d.Related, r)
	}
	for _, script := range append([]MysekaiScript{d.Script}, d.Related...) {
		for _, line := range script.Lines {
			var v MysekaiDraft
			v.ID = line.ID
			err = q.QueryRowContext(ctx, `SELECT source_hash,text,revision,updated_at,updated_by FROM mysekai_drafts WHERE region=? AND line_id=? AND locale=?`, region, line.ID, locale).Scan(&v.SourceHash, &v.Text, &v.Revision, &v.UpdatedAt, &v.UpdatedBy)
			if err != nil && err != sql.ErrNoRows {
				return d, err
			}
			if err == nil {
				d.Drafts[v.ID] = v
			}
		}
	}
	return d, nil
}
func (s *Store) MysekaiDetail(ctx context.Context, region, locale, key string) (MysekaiDetail, error) {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return MysekaiDetail{}, e
	}
	defer tx.Rollback()
	return mysekaiDetail(ctx, tx, region, locale, key)
}
func (s *Store) SaveMysekai(ctx context.Context, r MysekaiSave, actor string) (MysekaiDetail, error) {
	if len(r.Edits) == 0 || len(r.Edits) > 10000 {
		return MysekaiDetail{}, fmt.Errorf("batch must contain 1 to 10000 edits")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MysekaiDetail{}, err
	}
	defer tx.Rollback()
	d, err := mysekaiDetail(ctx, tx, r.Region, r.Locale, r.ScriptKey)
	if err != nil {
		return d, err
	}
	if d.SourceRevision != r.SourceRevision {
		return d, ErrMysekaiConflict
	}
	lines := map[string]MysekaiLine{}
	for _, script := range append([]MysekaiScript{d.Script}, d.Related...) {
		for _, line := range script.Lines {
			lines[line.ID] = line
		}
	}
	seen := map[string]bool{}
	for _, e := range r.Edits {
		line, ok := lines[e.ID]
		if !ok || line.SourceHash != e.SourceHash || d.Drafts[e.ID].Revision != e.ExpectedRevision {
			return d, ErrMysekaiConflict
		}
		if seen[e.ID] || !mysekaiText(e.Text) {
			return d, fmt.Errorf("invalid or duplicate translation")
		}
		seen[e.ID] = true
	}
	now := time.Now().Unix()
	for _, e := range r.Edits {
		rev := e.ExpectedRevision + 1
		if _, err = tx.ExecContext(ctx, `INSERT INTO mysekai_drafts VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(region,line_id,locale) DO UPDATE SET source_hash=excluded.source_hash,text=excluded.text,revision=excluded.revision,updated_at=excluded.updated_at,updated_by=excluded.updated_by`, r.Region, e.ID, r.Locale, e.SourceHash, e.Text, rev, now, actor); err != nil {
			return d, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO mysekai_draft_history VALUES(?,?,?,?,?,?,?,?)`, r.Region, e.ID, r.Locale, e.SourceHash, e.Text, rev, now, actor); err != nil {
			return d, err
		}
		d.Drafts[e.ID] = MysekaiDraft{e.ID, e.SourceHash, e.Text, rev, now, actor}
	}
	err = tx.Commit()
	if err == nil {
		s.NotifyChange()
	}
	return d, err
}

type MysekaiSummary struct {
	Key        string            `json:"key"`
	Contents   []json.RawMessage `json:"contents"`
	Lines      int               `json:"lines"`
	Translated int               `json:"translated"`
	Review     int               `json:"review"`
}
type MysekaiPage struct {
	Items          []MysekaiSummary `json:"items"`
	NextCursor     string           `json:"nextCursor,omitempty"`
	SourceRevision string           `json:"sourceRevision"`
	DraftRevision  string           `json:"draftRevision"`
	Channel        MysekaiChannel   `json:"channel"`
}

func (s *Store) MysekaiList(ctx context.Context, region, locale, query, state, cursor string, limit int) (MysekaiPage, error) {
	p := MysekaiPage{Items: []MysekaiSummary{}}
	if err := mysekaiScope(region, locale); err != nil {
		return p, err
	}
	if limit < 1 || limit > 100 || len(query) > 200 {
		return p, fmt.Errorf("invalid query")
	}
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return p, e
	}
	defer tx.Rollback()
	e = tx.QueryRowContext(ctx, `SELECT revision FROM mysekai_current_sources WHERE region=?`, region).Scan(&p.SourceRevision)
	if e != nil && e != sql.ErrNoRows {
		return p, e
	}
	p.Channel, e = mysekaiChannel(ctx, tx, region, locale)
	if e != nil {
		return p, e
	}
	p.DraftRevision, e = mysekaiDraftRevision(ctx, tx, region, locale)
	if e != nil {
		return p, e
	}
	drafts := map[string]MysekaiDraft{}
	rows, e := tx.QueryContext(ctx, `SELECT line_id,source_hash,text FROM mysekai_drafts WHERE region=? AND locale=?`, region, locale)
	if e != nil {
		return p, e
	}
	for rows.Next() {
		var d MysekaiDraft
		if e = rows.Scan(&d.ID, &d.SourceHash, &d.Text); e != nil {
			rows.Close()
			return p, e
		}
		drafts[d.ID] = d
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return p, e
	}
	rows, e = tx.QueryContext(ctx, `SELECT document FROM mysekai_scripts WHERE region=? AND active=1 AND script_key>? ORDER BY script_key`, region, cursor)
	if e != nil {
		return p, e
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return p, e
		}
		var script MysekaiScript
		if e = json.Unmarshal([]byte(raw), &script); e != nil {
			return p, e
		}
		if query != "" && !strings.Contains(strings.ToLower(raw), strings.ToLower(query)) {
			continue
		}
		v := MysekaiSummary{Key: script.Key, Contents: script.Contents, Lines: len(script.Lines)}
		for _, l := range script.Lines {
			d := drafts[l.ID]
			if strings.TrimSpace(d.Text) != "" {
				if d.SourceHash == l.SourceHash {
					v.Translated++
				} else {
					v.Review++
				}
			}
		}
		if (state == "untranslated" && v.Translated == v.Lines) || (state == "review" && v.Review == 0) {
			continue
		}
		if len(p.Items) == limit {
			p.NextCursor = p.Items[len(p.Items)-1].Key
			break
		}
		p.Items = append(p.Items, v)
	}
	return p, rows.Err()
}
func mysekaiChannel(ctx context.Context, q mysekaiReader, region, locale string) (MysekaiChannel, error) {
	c := MysekaiChannel{Region: region, Locale: locale}
	err := q.QueryRowContext(ctx, `SELECT revision,publication FROM mysekai_channels WHERE region=? AND locale=?`, region, locale).Scan(&c.Revision, &c.Publication)
	if err == sql.ErrNoRows {
		err = nil
	}
	return c, err
}
func mysekaiDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func mysekaiDraftRevision(ctx context.Context, q mysekaiReader, region, locale string) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT line_id,revision FROM mysekai_drafts WHERE region=? AND locale=? ORDER BY line_id`, region, locale)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	values := []any{}
	for rows.Next() {
		var id string
		var revision int
		if err = rows.Scan(&id, &revision); err != nil {
			return "", err
		}
		values = append(values, []any{id, revision})
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	b, _ := json.Marshal(values)
	return mysekaiDigest(b), nil
}

// Extraction diagnostics can change on an idempotent reimport; they are not
// part of the source identity. All execution/display context still is.
func mysekaiSourceIdentity(raw []byte) []byte {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	delete(value, "retiredScripts")
	if scripts, ok := value["scripts"].([]any); ok {
		for _, v := range scripts {
			if script, ok := v.(map[string]any); ok {
				delete(script, "retiredLines")
				if lines, ok := script["lines"].([]any); ok {
					for _, v := range lines {
						if line, ok := v.(map[string]any); ok {
							delete(line, "change")
						}
					}
				}
			}
		}
	}
	out, _ := json.Marshal(value)
	return out
}

func mysekaiSourceText(text string) bool {
	if !utf8.ValidString(text) || len(text) > 32768 {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

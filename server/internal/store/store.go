package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
)

const (
	contentGateWeight      int64 = 1 << 20
	lyricsMutexStripeCount       = 256
)

// Snapshot writes a point-in-time SQLite copy including committed WAL content.
// The destination must not already exist.
func (s *Store) Snapshot(path string) error {
	return s.SnapshotContext(context.Background(), path)
}

func (s *Store) SnapshotContext(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// Store provides CRUD over translation entries backed by SQLite.
type Store struct {
	db *db.DB

	mu            sync.RWMutex
	changeHooks   []func()
	contentGate   *semaphore.Weighted
	lyricsMutexes [lyricsMutexStripeCount]sync.Mutex

	localizationProjectionMu    sync.RWMutex
	localizationProjectionCache map[int]projectedLocalizationEntry
}

func New(database *db.DB) *Store {
	return &Store{db: database, contentGate: semaphore.NewWeighted(contentGateWeight)}
}

// LockContentShared prevents a restore or public rebuild from crossing a
// multi-step editor/translator operation. The returned function must be called
// exactly once when the operation finishes.
func (s *Store) LockContentShared() func() {
	release, err := s.LockContentSharedContext(context.Background())
	if err != nil {
		panic(err)
	}
	return release
}

// LockContentExclusive creates a complete replacement/snapshot boundary.
func (s *Store) LockContentExclusive() func() {
	release, err := s.LockContentExclusiveContext(context.Background())
	if err != nil {
		panic(err)
	}
	return release

}

// LockContentSharedContext acquires shared content access while allowing a
// request or shutdown cancellation to interrupt the wait.
func (s *Store) LockContentSharedContext(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.contentGate.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { s.contentGate.Release(1) }) }, nil
}

// LockContentExclusiveContext acquires the complete replacement/snapshot
// boundary while allowing cancellation before the critical section starts.
func (s *Store) LockContentExclusiveContext(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.contentGate.Acquire(ctx, contentGateWeight); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { s.contentGate.Release(contentGateWeight) }) }, nil
}

// OnChange registers a callback invoked after any write that changes data.
// Hooks run synchronously after the write commits; keep them fast (e.g. just
// signal a debounced rebuild). Safe to call from multiple goroutines.
func (s *Store) OnChange(fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	s.changeHooks = append(s.changeHooks, fn)
	s.mu.Unlock()
}

// NotifyChange runs all registered change hooks. Exported so callers that write
// through other stores (e.g. event stories) can trigger file regeneration too.
func (s *Store) NotifyChange() {
	s.mu.RLock()
	hooks := append([]func(){}, s.changeHooks...)
	s.mu.RUnlock()
	for _, h := range hooks {
		h()
	}
}

// ImportCategory replaces all rows for a category with the given full-format data.
// Used by the migration tool. Runs in a single transaction.
func (s *Store) ImportCategory(category string, cat model.Category) (int, error) {
	return s.ImportCategoryContext(context.Background(), category, cat)
}

func (s *Store) ImportCategoryContext(ctx context.Context, category string, cat model.Category) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE category = ?`, category); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO entries
		(category, field, jp_key, cn_text, source, ids_json, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	count := 0
	for field, entries := range cat {
		for jpKey, e := range entries {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			idsJSON := ""
			if len(e.Ids) > 0 {
				b, _ := json.Marshal(e.Ids)
				idsJSON = string(b)
			}
			source := e.Source
			if source == "" {
				source = model.SourceUnknown
			}
			if _, err := stmt.ExecContext(ctx, category, field, jpKey, e.Text, source, idsJSON, now, "migrate"); err != nil {
				return 0, fmt.Errorf("insert %s/%s: %w", category, field, err)
			}
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// CategoryData reconstructs the full-format Category map from the DB,
// matching the on-disk X.full.json structure exactly.
func (s *Store) CategoryData(category string) (model.Category, error) {
	rows, err := s.db.Query(
		`SELECT field, jp_key, cn_text, source, ids_json FROM entries WHERE category = ?`,
		category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cat := make(model.Category)
	for rows.Next() {
		var field, jpKey, text, source, idsJSON string
		if err := rows.Scan(&field, &jpKey, &text, &source, &idsJSON); err != nil {
			return nil, err
		}
		if cat[field] == nil {
			cat[field] = make(map[string]model.Entry)
		}
		e := model.Entry{Text: text, Source: source}
		if idsJSON != "" {
			_ = json.Unmarshal([]byte(idsJSON), &e.Ids)
		}
		cat[field][jpKey] = e
	}
	return cat, rows.Err()
}

// FlatData reconstructs the flat-format map (field -> jpKey -> cnText) for a
// category, matching X.json on disk.
func (s *Store) FlatData(category string) (map[string]map[string]string, error) {
	cat, err := s.CategoryData(category)
	if err != nil {
		return nil, err
	}
	flat := make(map[string]map[string]string, len(cat))
	for field, entries := range cat {
		flat[field] = make(map[string]string, len(entries))
		for k, e := range entries {
			flat[field][k] = e.Text
		}
	}
	return flat, nil
}

// GetCategories returns per-field counts for all categories, in canonical order.
func (s *Store) GetCategories() ([]model.CategoryInfo, error) {
	rows, err := s.db.Query(`SELECT category, field, source, COUNT(*)
		FROM entries GROUP BY category, field, source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// category -> field -> FieldInfo
	acc := make(map[string]map[string]*model.FieldInfo)
	for rows.Next() {
		var category, field, source string
		var n int
		if err := rows.Scan(&category, &field, &source, &n); err != nil {
			return nil, err
		}
		if acc[category] == nil {
			acc[category] = make(map[string]*model.FieldInfo)
		}
		fi := acc[category][field]
		if fi == nil {
			fi = &model.FieldInfo{Name: field}
			acc[category][field] = fi
		}
		fi.Total += n
		switch source {
		case model.SourceCN:
			fi.CnCount += n
		case model.SourceHuman:
			fi.HumanCount += n
		case model.SourcePinned:
			fi.PinnedCount += n
		case model.SourceLLM:
			fi.LlmCount += n
		default:
			fi.UnknownCount += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var result []model.CategoryInfo
	for _, cat := range model.SupportedCategories {
		fieldsMap, ok := acc[cat]
		if !ok {
			continue
		}
		fields := make([]model.FieldInfo, 0, len(fieldsMap))
		for _, fi := range fieldsMap {
			fields = append(fields, *fi)
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
		result = append(result, model.CategoryInfo{Name: cat, Fields: fields})
	}
	return result, nil
}

// GetEntries returns entries for a category/field with optional source filter.
func (s *Store) GetEntries(category, field, source string) ([]model.EntryWithKey, error) {
	query := `SELECT jp_key, cn_text, source, ids_json, updated_at FROM entries WHERE category = ? AND field = ?`
	args := []any{category, field}
	if source != "" {
		query += ` AND source = ?`
		args = append(args, source)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []model.EntryWithKey
	for rows.Next() {
		var key, text, src, idsJSON string
		var updatedAt int64
		if err := rows.Scan(&key, &text, &src, &idsJSON, &updatedAt); err != nil {
			return nil, err
		}
		ewk := model.EntryWithKey{Key: key, Text: text, Source: src, UpdatedAt: updatedAt}
		if idsJSON != "" {
			_ = json.Unmarshal([]byte(idsJSON), &ewk.Ids)
		}
		result = append(result, ewk)
	}
	return result, rows.Err()
}

// UpdateEntry sets text/source for one existing source entry. Unknown identities
// are rejected so a stale client cannot create a key in the wrong field.
func (s *Store) UpdateEntry(category, field, key, text, source, user string) (string, error) {
	if !model.IsValidSource(source) {
		return "", fmt.Errorf("invalid translation source: %q", source)
	}
	var curText, curSource string
	err := s.db.QueryRow(
		`SELECT cn_text, source FROM entries WHERE category = ? AND field = ? AND jp_key = ?`,
		category, field, key).Scan(&curText, &curSource)

	now := time.Now().Unix()
	if err == sql.ErrNoRows {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", err
	}
	if curText == text && curSource == source {
		return "noop", nil
	}
	_, err = s.db.Exec(
		`UPDATE entries SET cn_text = ?, source = ?, updated_at = ?, updated_by = ?
		 WHERE category = ? AND field = ? AND jp_key = ?`,
		text, source, now, user, category, field, key)
	if err != nil {
		return "", err
	}
	s.NotifyChange()
	return "ok", nil
}

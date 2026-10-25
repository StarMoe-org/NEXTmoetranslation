package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type MysekaiBackupRecord struct {
	Table  string `json:"table"`
	Values []any  `json:"values"`
}
type MysekaiContentExport struct {
	Records []MysekaiBackupRecord `json:"records"`
}

// The allowlist fixes the backup format independently of unrelated DB tables.
var mysekaiBackupTables = []struct {
	name, columns string
	count         int
}{
	{"mysekai_sources", "region,revision,catalog,imported_at,imported_by", 5},
	{"mysekai_current_sources", "region,revision", 2},
	{"mysekai_scripts", "region,script_key,document,active", 4},
	{"mysekai_drafts", "region,line_id,locale,source_hash,text,revision,updated_at,updated_by", 8},
	{"mysekai_draft_history", "region,line_id,locale,source_hash,text,revision,updated_at,updated_by", 8},
	{"mysekai_channels", "region,locale,revision,publication", 4},
	{"mysekai_publications", "region,locale,revision,assets,created_at,created_by", 6},
	{"mysekai_objects", "sha256,body", 2},
	{"mysekai_publication_history", "region,locale,revision,publication,action,updated_at,updated_by", 7},
}

func (s *Store) ExportMysekaiContentContext(ctx context.Context) (MysekaiContentExport, error) {
	out := MysekaiContentExport{Records: []MysekaiBackupRecord{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, table := range mysekaiBackupTables {
		rows, e := tx.QueryContext(ctx, `SELECT `+table.columns+` FROM `+table.name+` ORDER BY `+table.columns)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			v := make([]any, table.count)
			p := make([]any, table.count)
			for i := range v {
				p[i] = &v[i]
			}
			if e = rows.Scan(p...); e != nil {
				rows.Close()
				return out, e
			}
			out.Records = append(out.Records, MysekaiBackupRecord{table.name, v})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	return out, nil
}
func importMysekaiContentTx(ctx context.Context, tx *sql.Tx, content MysekaiContentExport) error {
	for _, record := range content.Records {
		known := false
		for _, t := range mysekaiBackupTables {
			if record.Table == t.name && len(record.Values) == t.count {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("invalid MYSEKAI backup table or record")
		}
		for _, v := range record.Values {
			switch v.(type) {
			case string, float64, int64:
				// SQLite scalar values only.
			default:
				return fmt.Errorf("invalid MYSEKAI backup value")
			}
		}
	}
	for i := len(mysekaiBackupTables) - 1; i >= 0; i-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+mysekaiBackupTables[i].name); err != nil {
			return err
		}
	}
	for _, table := range mysekaiBackupTables {
		for _, record := range content.Records {
			if record.Table != table.name {
				continue
			}
			query := `INSERT INTO ` + table.name + ` (` + table.columns + `) VALUES (` + strings.TrimSuffix(strings.Repeat("?,", table.count), ",") + `)`
			if _, err := tx.ExecContext(ctx, query, record.Values...); err != nil {
				return err
			}
		}
	}
	// Restores are untrusted input. Validate source and published object shapes
	// before committing them to the same public projection as live edits.
	rows, err := tx.QueryContext(ctx, `SELECT catalog FROM mysekai_sources`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var c MysekaiCatalog
		if err = json.Unmarshal([]byte(raw), &c); err == nil {
			err = validateMysekaiCatalog(c)
		}
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT region,locale,revision,assets FROM mysekai_publications`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var region, locale, revision, raw string
		if err = rows.Scan(&region, &locale, &revision, &raw); err != nil {
			rows.Close()
			return err
		}
		files, resolveErr := mysekaiResolvePublication(ctx, tx, []byte(raw))
		err = resolveErr
		if err == nil {
			err = validateMysekaiPublication(region, locale, revision, files)
		}
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT region,locale,line_id,source_hash,text,revision FROM mysekai_drafts UNION ALL SELECT region,locale,line_id,source_hash,text,revision FROM mysekai_draft_history`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var region, locale, id, hash, text string
		var revision int
		if err = rows.Scan(&region, &locale, &id, &hash, &text, &revision); err != nil {
			rows.Close()
			return err
		}
		if mysekaiScope(region, locale) != nil || id == "" || len(id) > 160 || !mysekaiHash.MatchString(hash) || !mysekaiText(text) || revision < 1 {
			rows.Close()
			return fmt.Errorf("invalid MYSEKAI draft backup")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var dangling int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mysekai_channels c WHERE c.publication<>'' AND NOT EXISTS(SELECT 1 FROM mysekai_publications p WHERE p.region=c.region AND p.locale=c.locale AND p.revision=c.publication)`).Scan(&dangling)
	if err != nil {
		return err
	}
	if dangling != 0 {
		return fmt.Errorf("MYSEKAI channel points to missing publication")
	}
	return nil
}

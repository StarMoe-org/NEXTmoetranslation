package sqlitedriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("sqlite", &pragmaSQLiteDriver{})
}

type pragmaSQLiteDriver struct {
	inner sqlite3.SQLiteDriver
}

type sqliteConn struct {
	*sqlite3.SQLiteConn
}

type deferredTx struct {
	conn *sqlite3.SQLiteConn
}

func (tx *deferredTx) Commit() error {
	_, err := tx.conn.ExecContext(context.Background(), "COMMIT", nil)
	if err != nil {
		_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK", nil)
	}
	return err
}

func (tx *deferredTx) Rollback() error {
	_, err := tx.conn.ExecContext(context.Background(), "ROLLBACK", nil)
	return err
}

// BeginTx honors ReadOnly: true by starting a DEFERRED read snapshot even when
// `_txlock=immediate` is configured on the DSN, matching modernc.org/sqlite's
// behavior so read-only snapshot transactions never contend on SQLite's single
// writer lock in WAL mode. Write transactions still delegate to SQLiteConn and
// acquire BEGIN IMMEDIATE immediately.
func (c *sqliteConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		if _, err := c.SQLiteConn.ExecContext(ctx, "BEGIN DEFERRED", nil); err != nil {
			return nil, err
		}
		return &deferredTx{conn: c.SQLiteConn}, nil
	}
	return c.SQLiteConn.BeginTx(ctx, opts)
}

func (d *pragmaSQLiteDriver) Open(dsn string) (driver.Conn, error) {
	cleanDSN, pragmas, err := splitPragmaParams(dsn)
	if err != nil {
		return nil, err
	}
	rawConn, err := d.inner.Open(cleanDSN)
	if err != nil {
		return nil, err
	}
	sc, ok := rawConn.(*sqlite3.SQLiteConn)
	if !ok {
		_ = rawConn.Close()
		return nil, fmt.Errorf("unexpected sqlite conn type %T", rawConn)
	}
	if len(pragmas) > 0 {
		ctx := context.Background()
		for _, stmt := range pragmas {
			if _, err := sc.ExecContext(ctx, stmt, nil); err != nil {
				_ = sc.Close()
				return nil, fmt.Errorf("apply sqlite pragma %q: %w", stmt, err)
			}
		}
	}
	return &sqliteConn{SQLiteConn: sc}, nil
}

// splitPragmaParams extracts `_pragma=name(value)` query parameters (originally
// used with modernc.org/sqlite) from a SQLite DSN so the underlying CGO
// github.com/mattn/go-sqlite3 driver receives standard SQLite URI / driver
// parameters (`mode=ro`, `immutable=1`, `_txlock=immediate`, etc.) while every
// connection still applies each requested PRAGMA in order upon open.
func splitPragmaParams(dsn string) (string, []string, error) {
	qIdx := strings.IndexByte(dsn, '?')
	if qIdx < 0 {
		return dsn, nil, nil
	}
	base := dsn[:qIdx]
	rawQuery := dsn[qIdx+1:]
	if rawQuery == "" || !strings.Contains(rawQuery, "_pragma") {
		return dsn, nil, nil
	}

	parts := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(parts))
	pragmas := make([]string, 0, 4)
	hasBusyTimeoutParam := false

	for _, part := range parts {
		if part == "" {
			continue
		}
		key, rawVal, hasEq := strings.Cut(part, "=")
		unescapedKey, err := url.QueryUnescape(key)
		if err != nil {
			return "", nil, fmt.Errorf("unescape sqlite dsn key %q: %w", key, err)
		}
		if unescapedKey == "_busy_timeout" {
			hasBusyTimeoutParam = true
		}
		if unescapedKey != "_pragma" {
			kept = append(kept, part)
			continue
		}
		if !hasEq {
			return "", nil, fmt.Errorf("sqlite _pragma parameter missing value")
		}
		val, err := url.QueryUnescape(rawVal)
		if err != nil {
			return "", nil, fmt.Errorf("unescape sqlite _pragma value %q: %w", rawVal, err)
		}
		name, arg, err := parsePragmaSpec(val)
		if err != nil {
			return "", nil, err
		}
		pragmas = append(pragmas, "PRAGMA "+name+" = "+arg)
		if strings.EqualFold(name, "busy_timeout") && !hasBusyTimeoutParam {
			kept = append(kept, "_busy_timeout="+url.QueryEscape(arg))
			hasBusyTimeoutParam = true
		}
	}

	if len(kept) == 0 {
		return base, pragmas, nil
	}
	return base + "?" + strings.Join(kept, "&"), pragmas, nil
}

func parsePragmaSpec(spec string) (string, string, error) {
	spec = strings.TrimSpace(spec)
	openIdx := strings.IndexByte(spec, '(')
	if openIdx <= 0 || !strings.HasSuffix(spec, ")") {
		return "", "", fmt.Errorf("invalid sqlite _pragma specification %q", spec)
	}
	name := strings.TrimSpace(spec[:openIdx])
	arg := strings.TrimSpace(spec[openIdx+1 : len(spec)-1])
	if !isSafeIdentifier(name) || !isSafePragmaValue(arg) {
		return "", "", fmt.Errorf("unsafe sqlite _pragma specification %q", spec)
	}
	return name, arg, nil
}

func isSafeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func isSafePragmaValue(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '+' {
			continue
		}
		return false
	}
	return true
}

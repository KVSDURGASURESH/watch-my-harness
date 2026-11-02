// Package store owns the SQLite cache. The schema is byte-for-byte the one
// token_dashboard/db.py creates, so the Python and Go builds read each other's file.
//
// Query helpers panic on database errors (the Python code raised); the HTTP
// layer and the scan loop recover from them.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"
)

const Schema = `
CREATE TABLE IF NOT EXISTS files (
  path        TEXT PRIMARY KEY,
  mtime       REAL    NOT NULL,
  bytes_read  INTEGER NOT NULL,
  scanned_at  REAL    NOT NULL,
  state       TEXT
);

CREATE TABLE IF NOT EXISTS messages (
  uuid                    TEXT PRIMARY KEY,
  parent_uuid             TEXT,
  session_id              TEXT NOT NULL,
  project_slug            TEXT NOT NULL,
  cwd                     TEXT,
  git_branch              TEXT,
  cc_version              TEXT,
  entrypoint              TEXT,
  type                    TEXT NOT NULL,
  is_sidechain            INTEGER NOT NULL DEFAULT 0,
  agent_id                TEXT,
  timestamp               TEXT NOT NULL,
  model                   TEXT,
  stop_reason             TEXT,
  prompt_id               TEXT,
  message_id              TEXT,
  input_tokens            INTEGER NOT NULL DEFAULT 0,
  output_tokens           INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
  cache_create_5m_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_create_1h_tokens  INTEGER NOT NULL DEFAULT 0,
  prompt_text             TEXT,
  prompt_chars            INTEGER,
  tool_calls_json         TEXT,
  source                  TEXT NOT NULL DEFAULT 'claude'
);
CREATE INDEX IF NOT EXISTS idx_messages_session   ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_messages_project   ON messages(project_slug);
CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_model     ON messages(model);
CREATE INDEX IF NOT EXISTS idx_messages_msgid     ON messages(session_id, message_id);
CREATE INDEX IF NOT EXISTS idx_messages_source    ON messages(source);

CREATE TABLE IF NOT EXISTS tool_calls (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  message_uuid  TEXT    NOT NULL,
  session_id    TEXT    NOT NULL,
  project_slug  TEXT    NOT NULL,
  tool_name     TEXT    NOT NULL,
  target        TEXT,
  result_tokens INTEGER,
  is_error      INTEGER NOT NULL DEFAULT 0,
  timestamp     TEXT    NOT NULL,
  source        TEXT    NOT NULL DEFAULT 'claude'
);
CREATE INDEX IF NOT EXISTS idx_tools_session ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tools_name    ON tool_calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_tools_target  ON tool_calls(target);
CREATE INDEX IF NOT EXISTS idx_tools_source  ON tool_calls(source);

CREATE TABLE IF NOT EXISTS plan (
  k TEXT PRIMARY KEY,
  v TEXT
);

CREATE TABLE IF NOT EXISTS dismissed_tips (
  tip_key       TEXT PRIMARY KEY,
  dismissed_at  REAL NOT NULL
);
`

// Row is one result row; SQL NULL is nil, INTEGER int64, REAL float64, TEXT string.
type Row = map[string]any

// DB wraps the pool plus the file path (the scanner and /readyz need it).
type DB struct {
	*sql.DB
	Path string
}

func DefaultPath() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude", "token-dashboard.db")
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "cache_size(-8192)")
	q.Add("_pragma", "journal_size_limit(67108864)")
	if readOnly {
		q.Set("mode", "ro")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: strings.ReplaceAll(q.Encode(), "+", "%20")}
	if !strings.HasPrefix(u.Path, "/") { // windows drive paths: file:///C:/...
		u.Path = "/" + u.Path
	}
	return u.String()
}

// Open creates/migrates the database exactly like init_db() and returns a pool.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	sdb, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(8)
	d := &DB{DB: sdb, Path: path}
	_, _ = d.Exec("PRAGMA journal_mode = WAL") // persistent once set; ignore failure (network FS)
	if err := d.migrate(); err != nil {
		sdb.Close()
		return nil, err
	}
	if _, err := d.Exec(Schema); err != nil {
		sdb.Close()
		return nil, err
	}
	// Go-only extra index (harmless to the Python build): the scanner deletes tool_calls by
	// message_uuid for every ingested message, which is a full table scan without it.
	_, _ = d.Exec("CREATE INDEX IF NOT EXISTS idx_tools_msguuid ON tool_calls(message_uuid)")
	return d, nil
}

func (d *DB) cols(table string) map[string]bool {
	out := map[string]bool{}
	rows, err := d.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil {
			out[name] = true
		}
	}
	return out
}

func (d *DB) migrate() error {
	// message_id: clear everything so the next scan replays JSONL cleanly.
	if c := d.cols("messages"); len(c) > 0 && !c["message_id"] {
		for _, q := range []string{
			"ALTER TABLE messages ADD COLUMN message_id TEXT",
			"DELETE FROM messages", "DELETE FROM tool_calls", "DELETE FROM files"} {
			if _, err := d.Exec(q); err != nil {
				return err
			}
		}
	}
	// source / state: non-destructive.
	if c := d.cols("messages"); len(c) > 0 && !c["source"] {
		if _, err := d.Exec("ALTER TABLE messages ADD COLUMN source TEXT NOT NULL DEFAULT 'claude'"); err != nil {
			return err
		}
	}
	if c := d.cols("files"); len(c) > 0 && !c["state"] {
		if _, err := d.Exec("ALTER TABLE files ADD COLUMN state TEXT"); err != nil {
			return err
		}
	}
	if c := d.cols("tool_calls"); len(c) > 0 && !c["source"] {
		if _, err := d.Exec("ALTER TABLE tool_calls ADD COLUMN source TEXT NOT NULL DEFAULT 'claude'"); err != nil {
			return err
		}
		_, _ = d.Exec(`UPDATE tool_calls SET source = (
                         SELECT m.source FROM messages m WHERE m.session_id = tool_calls.session_id LIMIT 1)
                       WHERE session_id IN (SELECT session_id FROM messages WHERE source != 'claude')`)
	}
	return nil
}

// ---------------------------------------------------------------- row helpers

// Rows runs a query and returns every row as a map (panics on error).
func (d *DB) Rows(q string, args ...any) []Row {
	return ScanRows(d.DB, q, args...)
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func ScanRows(d queryer, q string, args ...any) []Row {
	rows, err := d.Query(q, args...)
	if err != nil {
		panic(fmt.Errorf("sql: %w", err))
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []Row{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			panic(fmt.Errorf("sql scan: %w", err))
		}
		r := Row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				r[c] = string(b)
			} else {
				r[c] = vals[i]
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		panic(fmt.Errorf("sql rows: %w", err))
	}
	return out
}

// One returns the first row or nil.
func (d *DB) One(q string, args ...any) Row {
	r := d.Rows(q, args...)
	if len(r) == 0 {
		return nil
	}
	return r[0]
}

// Int coerces a SQL value to int64 (nil -> 0).
func Int(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case bool:
		if x {
			return 1
		}
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	}
	return 0
}

func Float(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// RangeClause mirrors _range_clause: optional source filter and [since, until).
func RangeClause(since, until, source string) (string, []any) {
	var where []string
	var args []any
	if source != "" {
		where = append(where, "source = ?")
		args = append(args, source)
	}
	if since != "" {
		where = append(where, "timestamp >= ?")
		args = append(args, since)
	}
	if until != "" {
		where = append(where, "timestamp < ?")
		args = append(args, until)
	}
	if len(where) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(where, " AND "), args
}

// ---------------------------------------------------------------- project names

var slugRe = regexp.MustCompile(`[:\\/ ]`)

// EncodeSlug is Claude Code's project-slug encoding: each of ':', '\', '/', ' ' becomes '-'.
func EncodeSlug(path string) string { return slugRe.ReplaceAllString(path, "-") }

func splitSep(cwd string) (string, []string) {
	trimmed := strings.TrimRight(cwd, `/\`)
	sep := "/"
	if strings.Contains(trimmed, `\`) {
		sep = `\`
	}
	return sep, strings.Split(trimmed, sep)
}

func walkToRoot(cwd, slug string) string {
	if cwd == "" || slug == "" {
		return ""
	}
	sep, parts := splitSep(cwd)
	for i := len(parts); i > 0; i-- {
		if EncodeSlug(strings.Join(parts[:i], sep)) == slug {
			if parts[i-1] != "" {
				return parts[i-1]
			}
		}
	}
	return ""
}

var dashRe = regexp.MustCompile(`-+`)

func ProjectNameFor(cwd, slug string) string {
	if n := walkToRoot(cwd, slug); n != "" {
		return n
	}
	if cwd != "" {
		_, parts := splitSep(cwd)
		if tail := parts[len(parts)-1]; tail != "" {
			return tail
		}
	}
	if slug != "" {
		var parts []string
		for _, p := range dashRe.Split(slug, -1) {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return slug
}

func BestProjectName(cwds []string, slug string) string {
	var clean []string
	for _, c := range cwds {
		if c != "" {
			clean = append(clean, c)
		}
	}
	for _, c := range clean {
		if n := walkToRoot(c, slug); n != "" {
			return n
		}
	}
	first := ""
	if len(clean) > 0 {
		first = clean[0]
	}
	return ProjectNameFor(first, slug)
}

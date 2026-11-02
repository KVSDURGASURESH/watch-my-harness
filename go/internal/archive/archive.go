// Package archive keeps long-term copies of the usage database: gzip snapshots, JSON/CSV
// export, import/merge of a snapshot, and optional pruning of old rows.
package archive

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

type Settings struct {
	Dir        string `json:"dir"`
	Auto       bool   `json:"auto"`
	KeepMonths int    `json:"keep_months"` // 0 = keep everything in the live database
	LastAuto   string `json:"last_auto"`   // YYYY-MM of the last automatic snapshot
}

type Snapshot struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	MTime string `json:"mtime"`
}

func DefaultDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".watch-my-harness", "archive")
}

func Load(d *store.DB) Settings {
	s := Settings{Dir: DefaultDir(), Auto: true}
	if v, ok := d.GetKV("archive_dir"); ok && v != "" {
		s.Dir = v
	}
	if v, ok := d.GetKV("archive_auto"); ok {
		s.Auto = v == "1"
	}
	if v, ok := d.GetKV("archive_keep_months"); ok {
		s.KeepMonths, _ = strconv.Atoi(v)
	}
	s.LastAuto, _ = d.GetKV("archive_last_auto")
	return s
}

func Save(d *store.DB, s Settings) error {
	if s.KeepMonths < 0 {
		return fmt.Errorf("keep_months must be 0 or more")
	}
	if s.Dir == "" {
		s.Dir = DefaultDir()
	}
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("archive folder must be an absolute path")
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("cannot use that folder: %v", err)
	}
	auto := "0"
	if s.Auto {
		auto = "1"
	}
	d.SetKV("archive_dir", s.Dir)
	d.SetKV("archive_auto", auto)
	d.SetKV("archive_keep_months", strconv.Itoa(s.KeepMonths))
	return nil
}

func List(dir string) []Snapshot {
	out := []Snapshot{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db.gz") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, Snapshot{e.Name(), fi.Size(), fi.ModTime().Format(time.RFC3339)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// Take writes a consistent copy (VACUUM INTO) gzipped into dir and returns its file name.
func Take(d *store.DB, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".snapshot-tmp.db")
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	if _, err := d.Exec("VACUUM INTO ?", tmp); err != nil {
		return "", err
	}
	name := "watch-my-harness-" + time.Now().Format("2006-01-02-150405") + ".db.gz"
	final := filepath.Join(dir, name)
	in, err := os.Open(tmp)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(final + ".part")
	if err != nil {
		return "", err
	}
	zw := gzip.NewWriter(out)
	if _, err = io.Copy(zw, in); err == nil {
		err = zw.Close()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(final + ".part")
		return "", err
	}
	return name, os.Rename(final+".part", final)
}

// AutoIfDue takes one snapshot per calendar month when automatic archiving is on.
func AutoIfDue(d *store.DB) {
	defer func() { recover() }()
	s := Load(d)
	month := time.Now().Format("2006-01")
	if !s.Auto || s.LastAuto == month {
		return
	}
	if store.Int(d.One("SELECT COUNT(*) AS n FROM messages")["n"]) == 0 {
		return
	}
	if _, err := Take(d, s.Dir); err == nil {
		d.SetKV("archive_last_auto", month)
	}
}

// Import merges a snapshot (a .db.gz made by Take) into the live database; existing rows win.
func Import(d *store.DB, dir, name string) (int64, error) {
	name = filepath.Base(name)
	if !strings.HasSuffix(name, ".db.gz") {
		return 0, fmt.Errorf("not a snapshot file")
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	tmp := filepath.Join(dir, ".import-tmp.db")
	defer os.Remove(tmp)
	t, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	if _, err = io.Copy(t, zr); err == nil {
		err = t.Close()
	}
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS snap", tmp); err != nil {
		return 0, err
	}
	defer conn.ExecContext(ctx, "DETACH DATABASE snap")
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	msgCols, err := sharedCols(ctx, tx, "messages", nil)
	if err != nil {
		return 0, err
	}
	toolCols, err := sharedCols(ctx, tx, "tool_calls", map[string]bool{"id": true})
	if err != nil {
		return 0, err
	}
	// tool_calls first, while "new message" is still decidable.
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO main.tool_calls (%[1]s) SELECT %[1]s FROM snap.tool_calls WHERE message_uuid NOT IN (SELECT uuid FROM main.messages)", toolCols)); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, fmt.Sprintf(
		"INSERT OR IGNORE INTO main.messages (%[1]s) SELECT %[1]s FROM snap.messages", msgCols))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

func sharedCols(ctx context.Context, tx *sql.Tx, table string, skip map[string]bool) (string, error) {
	get := func(schema string) (map[string]bool, []string, error) {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.table_info(%s)", schema, table))
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		m, order := map[string]bool{}, []string{}
		for rows.Next() {
			var cid int
			var name, typ string
			var nn, pk int
			var dflt any
			if err := rows.Scan(&cid, &name, &typ, &nn, &dflt, &pk); err != nil {
				return nil, nil, err
			}
			m[name] = true
			order = append(order, name)
		}
		return m, order, rows.Err()
	}
	snap, _, err := get("snap")
	if err != nil {
		return "", err
	}
	_, mainOrder, err := get("main")
	if err != nil {
		return "", err
	}
	var cols []string
	for _, c := range mainOrder {
		if snap[c] && !skip[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return "", fmt.Errorf("snapshot has no %s table", table)
	}
	return strings.Join(cols, ","), nil
}

// Prune removes rows older than keepMonths from the live database. Callers take a snapshot first.
func Prune(d *store.DB, keepMonths int) (int64, error) {
	if keepMonths <= 0 {
		return 0, fmt.Errorf("keep_months must be at least 1 to prune")
	}
	cut := time.Now().AddDate(0, -keepMonths, 0).UTC().Format("2006-01-02T15:04:05")
	if _, err := d.Exec("DELETE FROM tool_calls WHERE timestamp < ?", cut); err != nil {
		return 0, err
	}
	res, err := d.Exec("DELETE FROM messages WHERE timestamp < ?", cut)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Export streams messages as csv, or as a single JSON array.
func Export(d *store.DB, w io.Writer, format string) error {
	rows, err := d.Query(`SELECT timestamp, source, session_id, project_slug, cwd, model, type,
		input_tokens, output_tokens, cache_read_tokens, cache_create_5m_tokens, cache_create_1h_tokens
		FROM messages ORDER BY timestamp`)
	if err != nil {
		return err
	}
	defer rows.Close()
	head := []string{"timestamp", "source", "session_id", "project", "cwd", "model", "type",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_create_5m_tokens", "cache_create_1h_tokens"}
	vals := make([]any, len(head))
	ptrs := make([]any, len(head))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	cell := func(v any) any {
		if b, ok := v.([]byte); ok {
			return string(b)
		}
		return v
	}
	if format == "json" {
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		first := true
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			o := map[string]any{}
			for i, h := range head {
				o[h] = cell(vals[i])
			}
			b, _ := json.Marshal(o)
			if !first {
				io.WriteString(w, ",\n")
			}
			first = false
			w.Write(b)
		}
		_, err = io.WriteString(w, "]\n")
		return err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write(head)
	rec := make([]string, len(head))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i := range vals {
			if vals[i] == nil {
				rec[i] = ""
			} else {
				rec[i] = fmt.Sprint(cell(vals[i]))
			}
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
	return cw.Error()
}

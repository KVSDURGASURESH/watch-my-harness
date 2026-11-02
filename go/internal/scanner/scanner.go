// Package scanner turns agent session logs (Claude Code / Codex JSONL, Hermes
// SQLite, normalised "generic" JSONL) into the shared messages/tool_calls rows.
package scanner

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

// Kinds a source can have.
var Kinds = []string{"claude", "codex", "generic", "hermes"}

func ValidKind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Source is one configured log location.
type Source struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Path       string            `json:"path"`
	ModelAlias map[string]string `json:"model_alias,omitempty"`
}

type Totals struct {
	Messages int `json:"messages"`
	Tools    int `json:"tools"`
	Files    int `json:"files"`
}

// Progress is shared with the server so the setup screen can show a live bar.
type Progress struct {
	Running  atomic.Bool
	Total    atomic.Int64
	Done     atomic.Int64
	Messages atomic.Int64
	Tools    atomic.Int64
	current  atomic.Value // string
	Started  atomic.Int64 // unix ms
	Touched  atomic.Int64 // unix ms of the last file finished (liveness for /healthz)
	Finished atomic.Int64
	lastErr  atomic.Value // string
}

func (p *Progress) Snapshot() map[string]any {
	cur, _ := p.current.Load().(string)
	le, _ := p.lastErr.Load().(string)
	return map[string]any{"running": p.Running.Load(), "files_total": p.Total.Load(), "files_done": p.Done.Load(),
		"messages": p.Messages.Load(), "tools": p.Tools.Load(), "current": cur,
		"started_ms": p.Started.Load(), "finished_ms": p.Finished.Load(), "error": le}
}

// Tick marks one file done and refreshes the liveness stamp.
func (p *Progress) Tick() {
	if p != nil {
		p.Done.Add(1)
		p.Touched.Store(time.Now().UnixMilli())
	}
}

func (p *Progress) SetError(e string) {
	if p != nil {
		p.lastErr.Store(e)
	}
}

func (p *Progress) begin() {
	if p == nil {
		return
	}
	p.Running.Store(true)
	p.Total.Store(0)
	p.Done.Store(0)
	p.Messages.Store(0)
	p.Tools.Store(0)
	p.lastErr.Store("")
	p.Started.Store(time.Now().UnixMilli())
	p.Finished.Store(0)
}

func (p *Progress) end() {
	if p == nil {
		return
	}
	p.Running.Store(false)
	p.Finished.Store(time.Now().UnixMilli())
}

// ExpandHome handles a leading ~.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// ---------------------------------------------------------------- writer

const insertMsg = `INSERT OR REPLACE INTO messages (
  uuid, parent_uuid, session_id, project_slug, cwd, git_branch, cc_version, entrypoint,
  type, is_sidechain, agent_id, timestamp, model, stop_reason, prompt_id, message_id,
  input_tokens, output_tokens, cache_read_tokens, cache_create_5m_tokens, cache_create_1h_tokens,
  prompt_text, prompt_chars, tool_calls_json, source
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const insertTool = `INSERT INTO tool_calls (message_uuid, session_id, project_slug, tool_name, target, result_tokens, is_error, timestamp, source)
VALUES (?,?,?,?,?,?,?,?,?)`

var msgCols = []string{"uuid", "parent_uuid", "session_id", "project_slug", "cwd", "git_branch", "cc_version", "entrypoint",
	"type", "is_sidechain", "agent_id", "timestamp", "model", "stop_reason", "prompt_id", "message_id",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_create_5m_tokens", "cache_create_1h_tokens",
	"prompt_text", "prompt_chars", "tool_calls_json", "source"}

type msgRow = map[string]any

func blankMsg(source string) msgRow {
	return msgRow{
		"uuid": nil, "parent_uuid": nil, "session_id": nil, "project_slug": "",
		"cwd": nil, "git_branch": nil, "cc_version": nil, "entrypoint": nil,
		"type": "assistant", "is_sidechain": int64(0), "agent_id": nil, "timestamp": nil,
		"model": nil, "stop_reason": nil, "prompt_id": nil, "message_id": nil,
		"input_tokens": int64(0), "output_tokens": int64(0), "cache_read_tokens": int64(0),
		"cache_create_5m_tokens": int64(0), "cache_create_1h_tokens": int64(0),
		"prompt_text": nil, "prompt_chars": nil, "tool_calls_json": nil, "source": source,
	}
}

type toolRow struct {
	MessageUUID, SessionID, Slug, Name any
	Target                             any
	ResultTokens                       any
	IsError                            int64
	Timestamp                          any
	Source                             string
}

// sqlArg keeps unsupported JSON shapes (objects, arrays) from aborting the whole scan.
func sqlArg(v any) any {
	switch v.(type) {
	case map[string]any, []any:
		return nil
	case float64:
		f := v.(float64)
		if f == float64(int64(f)) {
			return int64(f)
		}
	}
	return v
}

type writer struct {
	tx       *sql.Tx
	msg      *sql.Stmt
	tool     *sql.Stmt
	delTools *sql.Stmt
	msgs     int
	tools    int
}

func newWriter(db *store.DB) (*writer, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	w := &writer{tx: tx}
	if w.msg, err = tx.Prepare(insertMsg); err != nil {
		tx.Rollback()
		return nil, err
	}
	if w.tool, err = tx.Prepare(insertTool); err != nil {
		tx.Rollback()
		return nil, err
	}
	if w.delTools, err = tx.Prepare("DELETE FROM tool_calls WHERE message_uuid=?"); err != nil {
		tx.Rollback()
		return nil, err
	}
	return w, nil
}

func (w *writer) putMsg(m msgRow) {
	args := make([]any, len(msgCols))
	for i, c := range msgCols {
		args[i] = sqlArg(m[c])
	}
	if _, err := w.msg.Exec(args...); err != nil {
		panic(fmt.Errorf("insert message: %w", err))
	}
}

func (w *writer) putTool(t toolRow) {
	if _, err := w.tool.Exec(sqlArg(t.MessageUUID), sqlArg(t.SessionID), t.Slug, t.Name, sqlArg(t.Target), sqlArg(t.ResultTokens),
		t.IsError, sqlArg(t.Timestamp), t.Source); err != nil {
		panic(fmt.Errorf("insert tool call: %w", err))
	}
	w.tools++
}

func (w *writer) commit() error { return w.tx.Commit() }
func (w *writer) rollback()     { _ = w.tx.Rollback() }

// ---------------------------------------------------------------- line reader

// iterLines yields (lineStart, lineEnd, record|nil) for each complete line. A partial
// trailing line is left unread so it is re-read once the writer finishes it.
func iterLines(path string, start int64, fn func(lineStart, lineEnd int64, rec obj)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
	}
	br := bufio.NewReaderSize(f, 1<<20)
	pos := start
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			return nil // EOF or partial line
		}
		lineStart := pos
		pos += int64(len(raw))
		var rec obj
		var v any
		if json.Unmarshal(raw, &v) == nil {
			rec, _ = v.(map[string]any)
		}
		fn(lineStart, pos, rec)
		if err != nil {
			return nil
		}
	}
}

// ---------------------------------------------------------------- orchestration

type fileParser func(w *writer, path, base string, start int64, state obj, source string) (end int64, newState obj, err error)

func parserFor(kind string) fileParser {
	switch kind {
	case "claude":
		return scanClaudeFile
	case "codex":
		return scanCodexFile
	case "generic":
		return scanGenericFile
	}
	return nil
}

// listFiles returns the candidate files of a source in lexical order, plus the base dir.
func listFiles(s Source) (files []string, base string) {
	root := ExpandHome(s.Path)
	st, err := os.Stat(root)
	if err != nil {
		return nil, ""
	}
	if s.Kind == "hermes" {
		for _, d := range hermesDBs(root) {
			files = append(files, d.path)
		}
		return files, root
	}
	if !st.IsDir() {
		return []string{root}, filepath.Dir(root)
	}
	walkRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		walkRoot = r // WalkDir does not descend into a symlinked root
	}
	_ = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			if rel, e := filepath.Rel(walkRoot, p); e == nil {
				p = filepath.Join(root, rel)
			}
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files, root
}

// ScanSource ingests one source. prog may be nil.
func ScanSource(db *store.DB, s Source, prog *Progress) (Totals, error) {
	var tot Totals
	if !ValidKind(s.Kind) {
		return tot, fmt.Errorf("unknown source kind %q; expected one of %v", s.Kind, Kinds)
	}
	files, base := listFiles(s)
	if prog != nil {
		prog.Total.Add(int64(len(files)))
		prog.current.Store(s.Name)
	}
	return scanListed(db, s, files, base, prog)
}

func scanListed(db *store.DB, s Source, files []string, base string, prog *Progress) (Totals, error) {
	var tot Totals
	if s.Kind == "hermes" {
		return scanHermes(db, s, prog)
	}
	parse := parserFor(s.Kind)
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		mtime := float64(st.ModTime().Unix()) + float64(st.ModTime().Nanosecond())*1e-9
		row := db.One("SELECT mtime, bytes_read, state FROM files WHERE path=?", p)
		var offset int64
		var state obj
		if row != nil && store.Float(row["mtime"]) == mtime && store.Int(row["bytes_read"]) == st.Size() {
			prog.Tick()
			continue
		}
		if row != nil && st.Size() > store.Int(row["bytes_read"]) {
			offset = store.Int(row["bytes_read"])
			if js := store.Str(row["state"]); js != "" {
				_ = json.Unmarshal([]byte(js), &state)
			}
		}
		w, err := newWriter(db)
		if err != nil {
			return tot, err
		}
		end, ns, err := func() (end int64, ns obj, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			return parse(w, p, base, offset, state, s.Name)
		}()
		if err != nil {
			w.rollback()
			return tot, fmt.Errorf("%s: %w", p, err)
		}
		if ns == nil {
			ns = obj{}
		}
		sj, _ := json.Marshal(ns)
		if _, err := w.tx.Exec("INSERT OR REPLACE INTO files (path, mtime, bytes_read, scanned_at, state) VALUES (?, ?, ?, ?, ?)",
			p, mtime, end, float64(time.Now().UnixNano())/1e9, string(sj)); err != nil {
			w.rollback()
			return tot, err
		}
		if err := w.commit(); err != nil {
			return tot, err
		}
		tot.Messages += w.msgs
		tot.Tools += w.tools
		tot.Files++
		if prog != nil {
			prog.Messages.Add(int64(w.msgs))
			prog.Tools.Add(int64(w.tools))
			prog.Tick()
		}
	}
	return tot, nil
}

// ScanSources scans every enabled source, in order.
func ScanSources(db *store.DB, sources []Source, prog *Progress) (Totals, error) {
	var tot Totals
	prog.begin()
	defer prog.end()
	type listed struct {
		s     Source
		files []string
		base  string
	}
	var all []listed
	for _, s := range sources {
		if !ValidKind(s.Kind) {
			prog.SetError(fmt.Sprintf("source %q: unknown kind %q", s.Name, s.Kind))
			continue
		}
		f, b := listFiles(s)
		all = append(all, listed{s, f, b})
		if prog != nil {
			prog.Total.Add(int64(len(f)))
		}
	}
	var firstErr error
	for _, l := range all {
		if prog != nil {
			prog.current.Store(l.s.Name)
		}
		t, err := scanListed(db, l.s, l.files, l.base, prog)
		tot.Messages += t.Messages
		tot.Tools += t.Tools
		tot.Files += t.Files
		if err != nil {
			prog.SetError(err.Error())
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return tot, firstErr
}

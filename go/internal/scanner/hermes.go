package scanner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

// Hermes keeps SQLite, not JSONL: ~/.hermes/state.db plus one state.db per profile.

var hermesToolClass = map[string]string{
	"terminal": "Bash", "read_file": "Read", "write_file": "Write", "patch": "Edit",
	"search_files": "Grep", "skill_view": "Skill", "delegate_task": "Task",
	"web_search": "WebSearch", "web_extract": "WebFetch",
}

var hermesTargetKeys = []string{"command", "path", "file_path", "pattern", "name", "query", "url", "goal", "task"}

type hdb struct{ label, path string }

func hermesDBs(root string) []hdb {
	st, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		return []hdb{{"main", root}}
	}
	var out []hdb
	if f, err := os.Stat(filepath.Join(root, "state.db")); err == nil && !f.IsDir() {
		out = append(out, hdb{"main", filepath.Join(root, "state.db")})
	}
	entries, _ := os.ReadDir(filepath.Join(root, "profiles"))
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(root, "profiles", n, "state.db")
		if f, err := os.Stat(p); err == nil && !f.IsDir() {
			out = append(out, hdb{n, p})
		}
	}
	return out
}

// isoMillis is datetime.fromtimestamp(ts, utc).isoformat(timespec="milliseconds") + "Z".
func isoMillis(ts float64) string {
	us := int64(math.RoundToEven(ts * 1e6))
	t := time.Unix(0, us*1000).UTC()
	return t.Format("2006-01-02T15:04:05") + fmt.Sprintf(".%03dZ", (us%1_000_000+1_000_000)%1_000_000/1000)
}

func hermesToolCalls(raw any) []call {
	s, ok := raw.(string)
	if !ok || s == "" {
		return nil
	}
	var calls []any
	if json.Unmarshal([]byte(s), &calls) != nil {
		return nil
	}
	var out []call
	for _, c := range calls {
		fn := asObj(asObj(c)["function"])
		name := strOr(fn["name"], "unknown")
		var target *string
		var args any
		as, _ := fn["arguments"].(string)
		if as == "" {
			as = "{}"
		}
		if json.Unmarshal([]byte(as), &args) == nil {
			if ao, _ := args.(map[string]any); ao != nil {
				for _, k := range hermesTargetKeys {
					if v, ok := ao[k].(string); ok {
						t := trunc(v, 500)
						target = &t
						break
					}
				}
			}
		}
		cls := name
		if c, ok := hermesToolClass[name]; ok {
			cls = c
		}
		out = append(out, call{cls, target})
	}
	return out
}

func hermesResult(content any) (int64, int64) {
	body, _ := content.(string)
	var isErr int64
	var j any
	if json.Unmarshal([]byte(body), &j) == nil {
		if jo, ok := j.(map[string]any); ok {
			ec, has := jo["exit_code"]
			if jo["status"] == "error" || truthy(jo["error"]) || (has && ec != nil && toInt(ec) != 0) {
				isErr = 1
			}
		}
	}
	return runeLen(body) / 4, isErr
}

func ingestHermesSession(w *writer, label string, ses store.Row, msgs []store.Row, alias map[string]string, name string) (int, int) {
	sid := fmt.Sprintf("%s:%s:%v", name, label, ses["id"])
	cwd, _ := ses["cwd"].(string)
	slug := name + "-unknown"
	if cwd != "" {
		slug = store.EncodeSlug(cwd)
	}
	model := ses["model"]
	if ms, ok := model.(string); ok {
		if a, ok := alias[ms]; ok {
			model = a
		}
	}
	var agent any
	if label != "main" {
		agent = label
	}
	common := func(m msgRow) {
		m["session_id"], m["project_slug"], m["cwd"], m["git_branch"] = ses["id"], slug, ses["cwd"], ses["git_branch"]
		m["entrypoint"], m["agent_id"] = ses["source"], agent
	}
	var asst []store.Row
	for _, m := range msgs {
		if m["role"] == "assistant" {
			asst = append(asst, m)
		}
	}
	totalKeys := []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_create_5m_tokens"}
	totals := map[string]int64{"input_tokens": store.Int(ses["input_tokens"]), "output_tokens": store.Int(ses["output_tokens"]),
		"cache_read_tokens": store.Int(ses["cache_read_tokens"]), "cache_create_5m_tokens": store.Int(ses["cache_write_tokens"])}
	any_ := false
	for _, v := range totals {
		if v != 0 {
			any_ = true
		}
	}
	n := int64(len(asst))
	if any_ && n < 1 {
		n = 1
	}
	ordered := append([]store.Row{}, msgs...)
	if len(asst) == 0 && any_ {
		ordered = append(ordered, store.Row{"id": int64(0), "role": "assistant", "content": nil, "tool_calls": nil,
			"tool_name": nil, "tool_call_id": nil, "timestamp": ses["started_at"]})
	}
	var nmsg, ntool int
	var lastUser any
	seen := int64(0)
	for _, m := range ordered {
		key := fmt.Sprintf("%s:%v", sid, m["id"])
		ts := isoMillis(store.Float(m["timestamp"]))
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		switch {
		case role == "user" && strings.TrimSpace(content) != "":
			row := blankMsg(name)
			common(row)
			row["uuid"], row["timestamp"], row["type"] = key, ts, "user"
			row["prompt_text"], row["prompt_chars"] = content, runeLen(content)
			w.putMsg(row)
			lastUser = key
			nmsg++
		case role == "assistant":
			seen++
			calls := hermesToolCalls(m["tool_calls"])
			row := blankMsg(name)
			common(row)
			row["uuid"], row["parent_uuid"], row["timestamp"], row["model"] = key, lastUser, ts, model
			for _, k := range totalKeys {
				v := totals[k]
				part := v / n
				if seen == n {
					part += v % n
				}
				row[k] = part
			}
			if len(calls) > 0 {
				row["tool_calls_json"] = dumpCalls(calls)
			}
			w.putMsg(row)
			nmsg++
			for _, c := range calls {
				var tg any
				if c.Target != nil {
					tg = *c.Target
				}
				w.putTool(toolRow{MessageUUID: key, SessionID: ses["id"], Slug: slug, Name: c.Name, Target: tg, Timestamp: ts, Source: name})
				ntool++
			}
		case role == "tool":
			toks, e := hermesResult(m["content"])
			w.putTool(toolRow{MessageUUID: key, SessionID: ses["id"], Slug: slug, Name: "_tool_result", Target: m["tool_call_id"],
				ResultTokens: toks, IsError: e, Timestamp: ts, Source: name})
			ntool++
		}
	}
	return nmsg, ntool
}

// scanHermes re-ingests a session whenever its totals or message count change
// (its old rows are replaced), so live sessions stay current.
func scanHermes(db *store.DB, s Source, prog *Progress) (Totals, error) {
	var tot Totals
	alias := s.ModelAlias
	if alias == nil {
		alias = map[string]string{}
	}
	ab, _ := json.Marshal(alias) // map keys are sorted by encoding/json
	aliasSig := string(ab)
	for _, d := range hermesDBs(ExpandHome(s.Path)) {
		ro, err := sql.Open("sqlite", "file:"+filepath.ToSlash(d.path)+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			continue
		}
		var sessions []store.Row
		func() {
			defer func() { recover() }()
			sessions = store.ScanRows(ro, "SELECT * FROM sessions")
		}()
		if sessions == nil { // locked or mid-migration: pick it up next scan
			ro.Close()
			continue
		}
		prev := obj{}
		if row := db.One("SELECT state FROM files WHERE path=?", d.path); row != nil {
			_ = json.Unmarshal([]byte(store.Str(row["state"])), &prev)
		}
		sigs := obj{}
		if prev["alias"] == aliasSig {
			sigs = asObj(prev["sigs"])
		}
		newSigs := obj{}
		w, err := newWriter(db)
		if err != nil {
			ro.Close()
			return tot, err
		}
		fail := func(e error) (Totals, error) { w.rollback(); ro.Close(); return tot, e }
		var ierr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					ierr = fmt.Errorf("%v", r)
				}
			}()
			for _, ses := range sessions {
				sig := []any{ses["message_count"], ses["input_tokens"], ses["output_tokens"], ses["cache_read_tokens"], ses["ended_at"], ses["api_call_count"]}
				id := fmt.Sprint(ses["id"])
				newSigs[id] = sig
				if old, ok := sigs[id]; ok {
					ob, _ := json.Marshal(old)
					nb, _ := json.Marshal(sig)
					if string(ob) == string(nb) || reflect.DeepEqual(old, sig) {
						continue
					}
				}
				like := strings.ReplaceAll(fmt.Sprintf("%s:%s:%s:", s.Name, d.label, id), "_", `\_`) + "%"
				w.tx.Exec(`DELETE FROM tool_calls WHERE message_uuid LIKE ? ESCAPE '\'`, like)
				w.tx.Exec(`DELETE FROM messages WHERE uuid LIKE ? ESCAPE '\'`, like)
				msgs := store.ScanRows(ro, "SELECT id, role, content, tool_calls, tool_name, tool_call_id, timestamp FROM messages WHERE session_id=? ORDER BY timestamp, id", ses["id"])
				m, t := ingestHermesSession(w, d.label, ses, msgs, alias, s.Name)
				tot.Messages += m
				tot.Tools += t
				w.msgs += m
			}
		}()
		ro.Close()
		if ierr != nil {
			return fail(ierr)
		}
		st, _ := os.Stat(d.path)
		mt := float64(0)
		if st != nil {
			mt = float64(st.ModTime().Unix()) + float64(st.ModTime().Nanosecond())*1e-9
		}
		sj, _ := json.Marshal(obj{"sigs": newSigs, "alias": aliasSig})
		if _, err := w.tx.Exec("INSERT OR REPLACE INTO files (path, mtime, bytes_read, scanned_at, state) VALUES (?,?,?,?,?)",
			d.path, mt, 0, float64(time.Now().UnixNano())/1e9, string(sj)); err != nil {
			return fail(err)
		}
		if err := w.commit(); err != nil {
			ro.Close()
			return tot, err
		}
		tot.Files++
		if prog != nil {
			prog.Tick()
		}
	}
	return tot, nil
}

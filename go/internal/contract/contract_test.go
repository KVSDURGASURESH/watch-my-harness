// Package contract replays contract/golden/*.json (frozen from the Python server over
// contract/fixtures) against the Go server. Point CONTRACT_BASE_URL at any server that was
// started over the same fixtures (e.g. the Python one) to run the identical checks there:
//
//	CONTRACT_BASE_URL=http://127.0.0.1:8080 go test ./go/internal/contract -run Golden
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	tokendashboard "github.com/KVSDURGASURESH/watch-my-harness"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/pricing"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/scanner"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/server"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/sources"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/tips"
)

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

var tagRe = regexp.MustCompile(`(generic|openclaw):[0-9a-f]{8}:`)

// norm makes golden and actual comparable: generic row keys embed a hash of the absolute path.
func norm(v any) any {
	switch x := v.(type) {
	case string:
		return tagRe.ReplaceAllString(x, "${1}:TAG:")
	case []any:
		for i := range x {
			x[i] = norm(x[i])
		}
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[tagRe.ReplaceAllString(k, "${1}:TAG:")] = norm(e)
		}
		return out
	}
	return v
}

func diff(want, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object, got %T", path, got)
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			wv, wok := w[k]
			gv, gok := g[k]
			if wok != gok {
				return fmt.Sprintf("%s.%s: key present want=%v got=%v", path, k, wok, gok)
			}
			if d := diff(wv, gv, path+"."+k); d != "" {
				return d
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: want array, got %T", path, got)
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s: want %d items, got %d", path, len(w), len(g))
		}
		for i := range w {
			if d := diff(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
	case float64:
		g, ok := got.(float64)
		if !ok {
			return fmt.Sprintf("%s: want number %v, got %T %v", path, w, got, got)
		}
		if math.Abs(w-g) > 1e-9*math.Max(1, math.Abs(w)) {
			return fmt.Sprintf("%s: want %v, got %v", path, w, g)
		}
	default:
		if fmt.Sprint(want) != fmt.Sprint(got) {
			return fmt.Sprintf("%s: want %v, got %v", path, want, got)
		}
	}
	return ""
}

type fixture struct {
	db  *store.DB
	srv *server.Server
	url string
}

var shared *fixture

func setup(t *testing.T) *fixture {
	if shared != nil {
		return shared
	}
	root := repoRoot()
	fx := filepath.Join(root, "contract", "fixtures")
	t.Setenv("TOKEN_DASHBOARD_SKILL_ROOTS", filepath.Join(fx, "skills"))
	dir, _ := os.MkdirTemp("", "td-contract")
	db, err := store.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sources.LoadFile(filepath.Join(root, "contract", "sources.fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var srcs []scanner.Source
	for _, e := range entries {
		srcs = append(srcs, scanner.Source{Name: e.Name, Kind: e.Kind, Path: filepath.Join(fx, e.Path), ModelAlias: e.ModelAlias})
	}
	if _, err := scanner.ScanSources(db, srcs, nil); err != nil {
		t.Fatal(err)
	}
	pricing.SetPlan(db, "pro", "codex")
	tips.Dismiss(db, "claude:repeat-bash:make test")
	cfg := filepath.Join(dir, "sources.json")
	if err := sources.SaveFile(cfg, func() []sources.Entry {
		for i := range entries {
			entries[i].Path = filepath.Join(fx, entries[i].Path)
		}
		return entries
	}()); err != nil {
		t.Fatal(err)
	}
	mgr := sources.NewManager(filepath.Join(fx, "claude"), false, nil, cfg)
	srv, err := server.New(server.Options{DB: db, Assets: tokendashboard.Assets, Sources: mgr})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	shared = &fixture{db: db, srv: srv, url: ts.URL}
	return shared
}

func baseURL(t *testing.T) (string, *fixture) {
	if u := os.Getenv("CONTRACT_BASE_URL"); u != "" {
		return strings.TrimRight(u, "/"), nil
	}
	f := setup(t)
	return f.url, f
}

func TestGolden(t *testing.T) {
	base, _ := baseURL(t)
	files, _ := filepath.Glob(filepath.Join(repoRoot(), "contract", "golden", "*.json"))
	n := 0
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "_") {
			continue
		}
		b, _ := os.ReadFile(f)
		var g struct {
			Name    string
			Request struct {
				Method  string
				Path    string
				Headers map[string]string
				Body    *string
			}
			Status int
			Body   any
		}
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		n++
		t.Run(g.Name, func(t *testing.T) {
			var rd io.Reader
			if g.Request.Body != nil {
				rd = bytes.NewReader([]byte(*g.Request.Body))
			}
			req, _ := http.NewRequest(g.Request.Method, base+g.Request.Path, rd)
			for k, v := range g.Request.Headers {
				if strings.EqualFold(k, "Host") {
					req.Host = v
				} else {
					req.Header.Set(k, v)
				}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != g.Status {
				t.Fatalf("status want %d got %d body %.200s", g.Status, resp.StatusCode, raw)
			}
			var got any
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &got); err != nil {
					got = map[string]any{"__raw__": string(raw)}
				}
			}
			if d := diff(norm(g.Body), norm(got), "$"); d != "" {
				t.Fatalf("%s", d)
			}
		})
	}
	if n < 30 {
		t.Fatalf("only %d goldens found", n)
	}
}

func readJSON(t *testing.T, name string) map[string]any {
	b, err := os.ReadFile(filepath.Join(repoRoot(), "contract", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Tips with a pinned clock (the HTTP endpoint uses "now" and so cannot be frozen).
func TestTipsFixedClock(t *testing.T) {
	f := setup(t)
	g := readJSON(t, "_tips_fixed_clock.json")
	today := g["today_iso"].(string)
	for name, src := range map[string]string{"all": "", "codex": "codex", "claude": "claude"} {
		got, _ := json.Marshal(tips.All(f.db, today, src))
		var gv any
		json.Unmarshal(got, &gv)
		if d := diff(g[name], gv, "$."+name); d != "" {
			t.Errorf("%s", d)
		}
	}
}

// Differential check straight on the database: row counts, per-session token totals, keys.
func TestDBMatchesPython(t *testing.T) {
	f := setup(t)
	g := readJSON(t, "_db_summary.json")
	grp := func(q string) map[string]any {
		out := map[string]any{}
		for _, r := range f.db.Rows(q) {
			out[store.Str(r["k"])] = float64(store.Int(r["n"]))
		}
		return out
	}
	for name, q := range map[string]string{
		"messages_by_source":   "SELECT source k, COUNT(*) n FROM messages GROUP BY source",
		"tool_calls_by_source": "SELECT source k, COUNT(*) n FROM tool_calls GROUP BY source",
		"tool_calls_by_name":   "SELECT tool_name k, COUNT(*) n FROM tool_calls GROUP BY tool_name",
	} {
		if d := diff(g[name], grp(q), "$."+name); d != "" {
			t.Errorf("%s", d)
		}
	}
	if n := store.Int(f.db.One("SELECT COUNT(*) n FROM files")["n"]); float64(n) != g["files"].(float64) {
		t.Errorf("files table: want %v got %d", g["files"], n)
	}
	sess := map[string]any{}
	for _, r := range f.db.Rows(`SELECT session_id, source, COUNT(*) msgs, SUM(input_tokens) i, SUM(output_tokens) o, SUM(cache_read_tokens) cr,
	  SUM(cache_create_5m_tokens) c5, SUM(cache_create_1h_tokens) c1 FROM messages GROUP BY session_id, source`) {
		m := map[string]any{}
		for k, v := range r {
			if s, ok := v.(string); ok {
				m[k] = s
			} else {
				m[k] = float64(store.Int(v))
			}
		}
		sess[store.Str(r["session_id"])] = m
	}
	if d := diff(norm(g["sessions"]), norm(sess), "$.sessions"); d != "" {
		t.Errorf("%s", d)
	}
	var uuids []any
	for _, r := range f.db.Rows("SELECT uuid FROM messages ORDER BY uuid") {
		uuids = append(uuids, store.Str(r["uuid"]))
	}
	want := g["message_uuids"].([]any)
	sortAny := func(a []any) {
		sort.Slice(a, func(i, j int) bool { return a[i].(string) < a[j].(string) })
	}
	nw, ng := norm(want).([]any), norm(uuids).([]any)
	sortAny(nw)
	sortAny(ng)
	if d := diff(nw, ng, "$.message_uuids"); d != "" {
		t.Errorf("%s", d)
	}
}

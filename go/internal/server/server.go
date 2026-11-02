// Package server is the HTTP layer: static UI, the JSON API (same shapes as the Python
// server), the SSE diff stream, health probes, and the Go-only sources/setup endpoints.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/archive"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/pricing"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/profile"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/scanner"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/skills"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/sources"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/tips"
)

const (
	maxPostBytes = 1_000_000
	maxLimit     = 1000
	cacheMax     = 400
	staleFactor  = 6
)

// Version is set by the linker (-ldflags "-X .../server.Version=...").
var Version = "dev"

type Options struct {
	DB      *store.DB
	Assets  fs.FS // root holding web/ and pricing.json
	Sources *sources.Manager
	// ScanInterval is the background scan period; 0 disables the loop (tests, --no-scan).
	ScanInterval time.Duration
	// RequireSetup holds the background scanner back until the first-run setup is
	// completed (or the database already has data), so nothing is read before the user chose sources.
	RequireSetup bool
}

type Server struct {
	db      *store.DB
	web     fs.FS
	pricing *pricing.Table
	src     *sources.Manager
	prog    *scanner.Progress
	scanMu  sync.Mutex

	cacheMu sync.Mutex
	cache   map[string][]byte
	order   []string
	gen     int64

	subMu sync.Mutex
	subs  map[chan []byte]struct{}

	hbMu      sync.Mutex
	hbTS      time.Time
	hbScan    bool
	hbEvery   time.Duration
	hbLastErr string
	gate      bool
}

func New(o Options) (*Server, error) {
	raw, err := fs.ReadFile(o.Assets, "pricing.json")
	if err != nil {
		return nil, err
	}
	pt, err := pricing.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	web, err := fs.Sub(o.Assets, "web")
	if err != nil {
		return nil, err
	}
	s := &Server{db: o.DB, web: web, pricing: pt, src: o.Sources, prog: &scanner.Progress{},
		cache: map[string][]byte{}, subs: map[chan []byte]struct{}{}, hbTS: time.Now(), hbEvery: 30 * time.Second}
	s.gate = o.RequireSetup
	if o.ScanInterval > 0 {
		s.hbScan, s.hbEvery = true, o.ScanInterval
		go s.scanLoop(o.ScanInterval)
	}
	go s.warm()
	return s, nil
}

// ---------------------------------------------------------------- cache / SSE

func (s *Server) bump() {
	s.cacheMu.Lock()
	s.gen++
	s.cache = map[string][]byte{}
	s.order = nil
	s.cacheMu.Unlock()
}

func (s *Server) cacheGet(k string) ([]byte, int64, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	b, ok := s.cache[k]
	return b, s.gen, ok
}

func (s *Server) cachePut(k string, gen int64, b []byte) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if gen != s.gen {
		return // data changed while this response was being computed
	}
	if len(s.order) >= cacheMax {
		delete(s.cache, s.order[0])
		s.order = s.order[1:]
	}
	if _, ok := s.cache[k]; !ok {
		s.order = append(s.order, k)
	}
	s.cache[k] = b
}

var noCache = map[string]bool{"/api/stream": true, "/api/scan": true, "/api/plan": true, "/api/sources": true,
	"/api/scan-status": true, "/api/config": true, "/api/setup/complete": true}

func cacheKey(p string, q map[string][]string) string {
	if !strings.HasPrefix(p, "/api/") || noCache[p] {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k, v := range q {
		if len(v) > 0 && v[0] == "" { // parse_qs drops blank values
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(p)
	for _, k := range keys {
		v := q[k][0]
		if (k == "since" || k == "until") && len(v) > 16 {
			v = v[:16] // rolling windows send "now - N" stamps; reuse within the same minute
		}
		b.WriteString("&" + k + "=" + v)
	}
	return b.String()
}

func (s *Server) publish(evt map[string]any) {
	b, _ := json.Marshal(evt)
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // a stalled client must not block the scanner
		}
	}
}

// warm precomputes tips (~100 queries per agent) off the request path.
func (s *Server) warm() {
	s.cacheMu.Lock()
	gen := s.gen
	s.cacheMu.Unlock()
	defer func() { recover() }()
	b, err := encode(tips.All(s.db, "", ""))
	if err == nil {
		s.cachePut("/api/tips", gen, b)
	}
}

// ---------------------------------------------------------------- scanning

// ScanNow runs one synchronous scan over the current sources; returns the totals.
func (s *Server) ScanNow() (scanner.Totals, error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	srcs, err := s.src.Effective()
	if err != nil {
		return scanner.Totals{}, err
	}
	n, err := scanner.ScanSources(s.db, srcs, s.prog)
	if n.Messages > 0 || n.Tools > 0 {
		s.bump()
		s.warm()
		s.publish(map[string]any{"type": "scan", "n": n, "ts": float64(time.Now().UnixNano()) / 1e9})
	}
	return n, err
}

// StartScan kicks a background scan; false when one is already running.
func (s *Server) StartScan() bool {
	if !s.scanMu.TryLock() {
		return false
	}
	s.scanMu.Unlock()
	go func() {
		defer func() { recover() }()
		_, _ = s.ScanNow()
		archive.AutoIfDue(s.db)
		s.publish(map[string]any{"type": "scan-done", "ts": float64(time.Now().UnixNano()) / 1e9})
	}()
	return true
}

func (s *Server) scanLoop(every time.Duration) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.setErr(fmt.Sprint(r))
					s.publish(map[string]any{"type": "error", "message": fmt.Sprint(r)})
				}
			}()
			if s.gate && !s.setupComplete() && store.Int(s.db.One("SELECT COUNT(*) AS n FROM messages")["n"]) == 0 {
				return // first run: wait for the setup screen
			}
			_, err := s.ScanNow()
			archive.AutoIfDue(s.db)
			if err != nil {
				s.setErr(err.Error())
				s.publish(map[string]any{"type": "error", "message": err.Error()})
			} else {
				s.setErr("")
			}
		}()
		s.hbMu.Lock()
		s.hbTS = time.Now()
		s.hbMu.Unlock()
		time.Sleep(every)
	}
}

func (s *Server) setErr(e string) { s.hbMu.Lock(); s.hbLastErr = e; s.hbMu.Unlock() }

func (s *Server) scanAge() float64 {
	s.hbMu.Lock()
	ts := s.hbTS
	s.hbMu.Unlock()
	if t := s.prog.Touched.Load(); t > 0 { // a long first scan counts as alive
		if pt := time.UnixMilli(t); pt.After(ts) {
			ts = pt
		}
	}
	return time.Since(ts).Seconds()
}

func (s *Server) scanStale() bool {
	floor := 120.0
	if v := s.hbEvery.Seconds() * staleFactor; v > floor {
		floor = v
	}
	return s.hbScan && s.scanAge() > floor && !s.prog.Running.Load()
}

// ---------------------------------------------------------------- helpers

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeJSON(w http.ResponseWriter, body []byte, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(body)
}

type reqCtx struct {
	w    http.ResponseWriter
	ckey string
	gen  int64
	s    *Server
	path string
}

func (c *reqCtx) send(v any, status int) {
	b, err := encode(v)
	if err != nil {
		writeJSON(c.w, []byte(`{"error":"encode failed"}`), 500)
		return
	}
	if c.ckey != "" && status == 200 {
		c.s.cachePut(c.ckey, c.gen, b)
	}
	writeJSON(c.w, b, status)
}

func (c *reqCtx) fail(status int, msg string) { c.send(map[string]any{"error": msg}, status) }

func clampLimit(raw string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if v < 1 {
		v = 1
	}
	if v > maxLimit {
		v = maxLimit
	}
	return v
}

var loopback = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true, "::1": true}

// hostAllowed defends against DNS rebinding / cross-site requests: only loopback Host names
// (plus TOKEN_DASHBOARD_ALLOWED_HOSTS, comma-separated) are answered.
func hostAllowed(h string) bool {
	host := strings.ToLower(strings.TrimSpace(h))
	if strings.HasPrefix(host, "[") {
		host = strings.SplitN(host, "]", 2)[0] + "]"
	} else if strings.Count(host, ":") == 1 {
		host = host[:strings.LastIndex(host, ":")]
	}
	if loopback[host] {
		return true
	}
	for _, e := range strings.Split(os.Getenv("TOKEN_DASHBOARD_ALLOWED_HOSTS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" && e == host {
			return true
		}
	}
	return false
}

var mimeTypes = map[string]string{
	".html": "text/html", ".js": "text/javascript", ".mjs": "text/javascript", ".css": "text/css",
	".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon",
	".woff2": "font/woff2", ".woff": "font/woff", ".txt": "text/plain", ".map": "application/json",
}

func (s *Server) static(w http.ResponseWriter, rel string) {
	rel = strings.TrimLeft(rel, "/")
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/") // rooted Clean can never climb out of web/
	if clean == "" {
		w.WriteHeader(404)
		return
	}
	b, err := fs.ReadFile(s.web, clean)
	if err != nil {
		w.WriteHeader(404)
		return
	}
	ct := mimeTypes[strings.ToLower(path.Ext(clean))]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(200)
	w.Write(b)
}

// ---------------------------------------------------------------- routing

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			writeJSON(w, []byte(fmt.Sprintf(`{"error":%q}`, fmt.Sprint(rec))), 500)
		}
	}()
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.doGet(w, r)
	case http.MethodPost:
		s.doPost(w, r)
	default:
		w.WriteHeader(501)
	}
}

func (s *Server) doGet(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	c := &reqCtx{w: w, s: s}
	if p != "/healthz" && p != "/readyz" && !hostAllowed(r.Host) {
		c.fail(403, "host not allowed (set TOKEN_DASHBOARD_ALLOWED_HOSTS)")
		return
	}
	if strings.HasPrefix(p, "/api/archive") {
		s.archiveGet(c, w, r)
		return
	}
	q := r.URL.Query()
	since, until, source := q.Get("since"), q.Get("until"), q.Get("source")
	if ck := cacheKey(p, q); ck != "" {
		if b, gen, ok := s.cacheGet(ck); ok {
			writeJSON(w, b, 200)
			return
		} else {
			c.ckey, c.gen = ck, gen
		}
	}
	db := s.db
	switch {
	case p == "/healthz": // liveness: serving HTTP and the scanner is advancing
		stale := s.scanStale()
		s.hbMu.Lock()
		le := s.hbLastErr
		s.hbMu.Unlock()
		var lastErr any
		if le != "" {
			lastErr = le
		}
		st := 200
		if stale {
			st = 503
		}
		c.send(map[string]any{"ok": !stale, "scan_age_s": pricing.Round(s.scanAge(), 1), "last_error": lastErr}, st)
	case p == "/readyz": // readiness: the database is queryable
		if err := s.ready(); err != nil {
			c.send(map[string]any{"ready": false, "error": err.Error()}, 503)
			return
		}
		c.send(map[string]any{"ready": true}, 200)
	case p == "/" || p == "/index.html":
		s.static(w, "index.html")
	case strings.HasPrefix(p, "/web/"):
		s.static(w, p[5:])
	case p == "/api/overview":
		totals := store.OverviewTotals(db, since, until, source)
		cost, priced, models := 0.0, false, 0
		for _, m := range store.ModelBreakdown(db, since, until, source) {
			models++
			if cst := s.pricing.CostFor(store.Str(m["model"]), pricing.UsageOf(m)); cst.USD != nil {
				cost += *cst.USD
				priced = true
			}
		}
		// nil, not 0: an agent with no priced models isn't free, it's unknown.
		if priced || models == 0 {
			totals["cost_usd"] = pricing.Round(cost, 4)
		} else {
			totals["cost_usd"] = nil
		}
		c.send(totals, 200)
	case p == "/api/prompts":
		sortBy := "tokens"
		if v, ok := q["sort"]; ok && len(v) > 0 {
			sortBy = v[0]
		}
		limit := 50
		if v := q.Get("limit"); v != "" {
			limit = clampLimit(v, 50)
		}
		rows := store.ExpensivePrompts(db, limit, sortBy, source)
		for _, r := range rows {
			cst := s.pricing.CostFor(store.Str(r["model"]), pricing.Usage{CacheRead: store.Int(r["cache_read_tokens"])})
			if cst.USD != nil {
				r["estimated_cost_usd"] = *cst.USD
			} else {
				r["estimated_cost_usd"] = nil
			}
		}
		c.send(rows, 200)
	case p == "/api/projects":
		c.send(store.ProjectSummary(db, since, until, source), 200)
	case p == "/api/profile":
		c.send(profile.Build(db, since, until, source), 200)
	case p == "/api/tools":
		c.send(store.ToolTokenBreakdown(db, since, until, source), 200)
	case p == "/api/sessions":
		limit := 20
		if v := q.Get("limit"); v != "" {
			limit = clampLimit(v, 20)
		}
		c.send(store.RecentSessions(db, limit, since, until, source), 200)
	case p == "/api/daily":
		c.send(store.DailyTokenBreakdown(db, since, until, source), 200)
	case p == "/api/skills":
		rows := store.SkillBreakdown(db, since, until, source)
		cat := skills.Cached()
		for _, r := range rows {
			r["tokens_per_call"] = skills.TokensFor(store.Str(r["skill"]), cat)
		}
		c.send(rows, 200)
	case p == "/api/by-source":
		rows := store.SourceBreakdown(db, since, until)
		for _, r := range rows {
			src := store.Str(r["source"])
			total, n, priced := 0.0, 0, 0
			for _, m := range store.ModelBreakdown(db, since, until, src) {
				n++
				if cst := s.pricing.CostFor(store.Str(m["model"]), pricing.UsageOf(m)); cst.USD != nil {
					total += *cst.USD
					priced++
				}
			}
			// nil = no model of this agent is in pricing.json; a $0 would read as "free".
			if priced > 0 {
				r["cost_usd"] = pricing.Round(total, 4)
			} else {
				r["cost_usd"] = nil
			}
			r["unpriced_models"] = int64(n - priced)
			plan := pricing.GetPlan(db, "api", src)
			pl, ok := s.pricing.Plans[plan]
			if !ok {
				pl = s.pricing.Plans["api"]
			}
			r["plan"], r["plan_label"], r["plan_monthly"] = plan, pl.Label, pl.Monthly
		}
		c.send(rows, 200)
	case p == "/api/by-model":
		rows := store.ModelBreakdown(db, since, until, source)
		for _, r := range rows {
			cst := s.pricing.CostFor(store.Str(r["model"]), pricing.UsageOf(r))
			if cst.USD != nil {
				r["cost_usd"] = *cst.USD
			} else {
				r["cost_usd"] = nil
			}
			r["cost_estimated"] = cst.Estimated
		}
		c.send(rows, 200)
	case strings.HasPrefix(p, "/api/sessions/"):
		c.send(store.SessionTurns(db, p[strings.LastIndex(p, "/")+1:]), 200)
	case p == "/api/tips":
		c.send(tips.All(db, "", source), 200)
	case p == "/api/recommendations":
		c.send(map[string]any{"markdown": tips.RecommendationsMarkdown(db, "", source)}, 200)
	case p == "/api/plan":
		c.send(map[string]any{"plan": pricing.GetPlan(db, "api", ""), "configured": db.HasKey("plan"),
			"agent_plans": pricing.AgentPlans(db), "pricing": s.pricing.Raw}, 200)
	case p == "/api/scan":
		n, err := s.ScanNow()
		if err != nil {
			c.fail(500, err.Error())
			return
		}
		c.send(n, 200)
	case p == "/api/scan-status":
		c.send(s.prog.Snapshot(), 200)
	case p == "/api/sources":
		s.getSources(c)
	case p == "/api/config":
		c.send(map[string]any{"licence": sources.LoadAppConfig().Licence, "version": Version}, 200)
	case p == "/api/stream":
		s.stream(w, r)
	default:
		w.WriteHeader(404)
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	fl.Flush()
	ch := make(chan []byte, 100)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() { s.subMu.Lock(); delete(s.subs, ch); s.subMu.Unlock() }()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			t.Reset(15 * time.Second)
		case <-t.C:
			io.WriteString(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		fl.Flush()
	}
}

func (s *Server) doPost(w http.ResponseWriter, r *http.Request) {
	c := &reqCtx{w: w, s: s}
	if !hostAllowed(r.Host) {
		c.fail(403, "host not allowed (set TOKEN_DASHBOARD_ALLOWED_HOSTS)")
		return
	}
	// JSON-only: a cross-site <form> can only send urlencoded/multipart/text, and a fetch with
	// application/json triggers a CORS preflight this server never approves.
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "application/json" {
		c.fail(415, "Content-Type must be application/json")
		return
	}
	length := 0
	if h := r.Header.Get("Content-Length"); h != "" {
		n, err := strconv.Atoi(strings.TrimSpace(h))
		if err != nil {
			c.fail(400, "invalid Content-Length")
			return
		}
		length = n
	}
	if length == 0 && r.ContentLength > 0 {
		length = int(r.ContentLength)
	}
	if length < 0 || length > maxPostBytes {
		c.fail(413, fmt.Sprintf("body too large (max %d bytes)", maxPostBytes))
		return
	}
	body := map[string]any{}
	if length > 0 {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, int64(length)))
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			c.fail(400, "invalid JSON")
			return
		}
		o, ok := v.(map[string]any)
		if !ok {
			c.fail(400, "body must be a JSON object")
			return
		}
		body = o
	}
	switch r.URL.Path {
	case "/api/plan":
		plan := "api"
		if v, ok := body["plan"]; ok {
			plan = fmt.Sprint(v)
			if v == nil {
				plan = "None"
			}
		}
		src, _ := body["source"].(string)
		pricing.SetPlan(s.db, plan, src)
		s.bump()
		c.send(map[string]any{"ok": true}, 200)
	case "/api/tips/dismiss":
		key, _ := body["key"].(string)
		tips.Dismiss(s.db, key)
		s.bump()
		go s.warm()
		c.send(map[string]any{"ok": true}, 200)
	case "/api/archive/settings", "/api/archive/snapshot", "/api/archive/import", "/api/archive/prune":
		c.path = r.URL.Path
		s.archivePost(c, body)
	case "/api/sources":
		s.postSources(c, body)
	case "/api/setup/complete":
		s.postSetupComplete(c, body)
	case "/api/scan":
		c.send(map[string]any{"ok": true, "started": s.StartScan()}, 200)
	default:
		w.WriteHeader(404)
	}
}

// Handler returns the server as an http.Handler.
func (s *Server) Handler() http.Handler { return s }

func (s *Server) ready() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	s.db.Rows("SELECT 1 FROM messages LIMIT 1")
	return nil
}

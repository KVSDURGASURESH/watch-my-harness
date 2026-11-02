package server

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/sources"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

func newTestServer(t *testing.T) (*Server, string) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { time.Sleep(50 * time.Millisecond); db.Close() }) // let the warm-up goroutine finish; open files block TempDir removal on Windows
	fsys := fstest.MapFS{
		"pricing.json":   {Data: []byte(`{"models":{},"tier_fallback":{},"plans":{"api":{"monthly":0,"label":"API"}}}`)},
		"web/index.html": {Data: []byte("TOKEN DASHBOARD")},
	}
	s, err := New(Options{DB: db, Assets: fsys, Sources: sources.NewManager(dir, false, nil, filepath.Join(dir, "s.json"))})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func do(s *Server, method, path, host, ctype, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestHardening(t *testing.T) {
	s, _ := newTestServer(t)
	if c := do(s, "GET", "/api/overview", "evil.com", "", "").Code; c != 403 {
		t.Fatalf("foreign host: %d", c)
	}
	if c := do(s, "GET", "/healthz", "10.0.0.5:80", "", "").Code; c != 200 {
		t.Fatalf("probe exempt: %d", c)
	}
	if c := do(s, "GET", "/api/overview", "[::1]:8191", "", "").Code; c != 200 {
		t.Fatalf("ipv6 loopback: %d", c)
	}
	if c := do(s, "POST", "/api/plan", "localhost", "text/plain", "x").Code; c != 415 {
		t.Fatalf("ctype: %d", c)
	}
	if c := do(s, "POST", "/api/plan", "localhost", "application/json; charset=utf-8", "{bad").Code; c != 400 {
		t.Fatalf("bad json: %d", c)
	}
	if c := do(s, "GET", "/web/../../etc/passwd", "localhost", "", "").Code; c != 404 {
		t.Fatalf("traversal: %d", c)
	}
	if c := do(s, "GET", "/", "localhost", "", "").Code; c != 200 {
		t.Fatalf("index: %d", c)
	}
}

func TestSetupAndSourcesAPI(t *testing.T) {
	s, _ := newTestServer(t)
	if !strings.Contains(do(s, "GET", "/api/sources", "localhost", "", "").Body.String(), `"setup_complete":false`) {
		t.Fatal("fresh install must report setup incomplete")
	}
	if c := do(s, "POST", "/api/sources", "localhost", "application/json", `{"sources":[{"name":"a b","kind":"claude","path":"/x"}]}`).Code; c != 400 {
		t.Fatalf("invalid source accepted: %d", c)
	}
	body := `{"sources":[{"name":"oc","kind":"generic","path":"/nonexistent"}],"plan":"api"}`
	if c := do(s, "POST", "/api/setup/complete", "localhost", "application/json", body).Code; c != 200 {
		t.Fatalf("setup: %d", c)
	}
	out := do(s, "GET", "/api/sources", "localhost", "", "").Body.String()
	if !strings.Contains(out, `"setup_complete":true`) || !strings.Contains(out, `"oc"`) {
		t.Fatalf("not persisted: %s", out)
	}
}

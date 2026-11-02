package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

func TestPyDumpsMatchesPython(t *testing.T) {
	tg := "é\U0001F680\"\\"
	got := dumpCalls([]call{{"Read", &tg}, {"Bash", nil}})
	want := `[{"name": "Read", "target": "\u00e9\ud83d\ude80\"\\"}, {"name": "Bash", "target": null}]`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if dumpCalls(nil) != "[]" {
		t.Fatal("empty list")
	}
}

func TestClaudeDedupPartialLineAndRescan(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "-x-y")
	os.MkdirAll(proj, 0o755)
	f := filepath.Join(proj, "s.jsonl")
	line := func(uuid, mid string, out int) string {
		return `{"type":"assistant","uuid":"` + uuid + `","sessionId":"s","timestamp":"2026-01-01T00:00:00Z","message":{"id":"` + mid + `","model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":` + string(rune('0'+out)) + `}}}` + "\n"
	}
	os.WriteFile(f, []byte(line("a", "m", 1)+line("b", "m", 5)+`{"type":"user","uuid":"partial"`), 0o644)
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	src := Source{Name: "claude", Kind: "claude", Path: filepath.Join(dir, "projects")}
	n, err := ScanSource(db, src, nil)
	if err != nil || n.Messages != 2 {
		t.Fatalf("scan: %+v %v", n, err)
	}
	r := db.One("SELECT COUNT(*) c, SUM(output_tokens) o FROM messages")
	if store.Int(r["c"]) != 1 || store.Int(r["o"]) != 5 {
		t.Fatalf("snapshot dedupe failed: %v", r)
	}
	// finish the partial line: only it is ingested on the next scan
	os.WriteFile(f, []byte(line("a", "m", 1)+line("b", "m", 5)+`{"type":"user","uuid":"partial","sessionId":"s","timestamp":"2026-01-01T00:00:01Z","message":{"content":"hi"}}`+"\n"), 0o644)
	n, _ = ScanSource(db, src, nil)
	if n.Messages != 1 {
		t.Fatalf("incremental: %+v", n)
	}
	n, _ = ScanSource(db, src, nil)
	if n.Messages != 0 || n.Files != 0 {
		t.Fatalf("unchanged file must be skipped: %+v", n)
	}
}

func TestGenericAndUnknownKind(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(`{"session_id":"s1","timestamp":"2026-01-01T00:00:00Z","model":"m","input_tokens":3,"output_tokens":4,"prompt":"p","tools":["x"]}`+"\n"+`{"timestamp":"t"}`+"\n"), 0o644)
	db, _ := store.Open(filepath.Join(dir, "t.db"))
	t.Cleanup(func() { db.Close() })
	n, err := ScanSource(db, Source{Name: "oc", Kind: "generic", Path: dir}, nil)
	if err != nil || n.Messages != 2 || n.Tools != 1 {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := ScanSource(db, Source{Name: "x", Kind: "nope", Path: dir}, nil); err == nil {
		t.Fatal("unknown kind must error")
	}
}

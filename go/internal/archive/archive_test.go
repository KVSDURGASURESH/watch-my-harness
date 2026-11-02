package archive

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

func seed(t *testing.T, d *store.DB, uuid, ts string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO messages (uuid,session_id,project_slug,type,timestamp,model,input_tokens,output_tokens)
		VALUES (?,?,?,?,?,?,?,?)`, uuid, "s1", "p", "assistant", ts, "m", 10, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO tool_calls (message_uuid,session_id,project_slug,tool_name,timestamp) VALUES (?,?,?,?,?)`,
		uuid, "s1", "p", "Read", ts); err != nil {
		t.Fatal(err)
	}
}

func count(d *store.DB, table string) int64 {
	return store.Int(d.One("SELECT COUNT(*) AS n FROM " + table)["n"])
}

func TestSnapshotImportPruneExport(t *testing.T) {
	tmp := t.TempDir()
	a, err := store.Open(filepath.Join(tmp, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	old := time.Now().AddDate(0, -8, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	seed(t, a, "old1", old)
	seed(t, a, "new1", now)

	dir := filepath.Join(tmp, "arch")
	name, err := Take(a, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := List(dir); len(got) != 1 || got[0].Name != name {
		t.Fatalf("list = %v", got)
	}

	// Prune keeps 3 months: the 8-month-old row goes.
	if n, err := Prune(a, 3); err != nil || n != 1 {
		t.Fatalf("prune n=%d err=%v", n, err)
	}
	if count(a, "messages") != 1 || count(a, "tool_calls") != 1 {
		t.Fatal("prune left wrong counts")
	}

	// Import restores it; a second import is a no-op (no duplicate tool calls).
	if n, err := Import(a, dir, name); err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}
	if _, err := Import(a, dir, name); err != nil {
		t.Fatal(err)
	}
	if count(a, "messages") != 2 || count(a, "tool_calls") != 2 {
		t.Fatalf("after import: %d msgs %d tools", count(a, "messages"), count(a, "tool_calls"))
	}

	// Import into a brand-new database (new machine).
	b, _ := store.Open(filepath.Join(tmp, "b.db"))
	t.Cleanup(func() { b.Close() })
	if n, err := Import(b, dir, name); err != nil || n != 2 {
		t.Fatalf("fresh import n=%d err=%v", n, err)
	}

	var csvBuf, jsonBuf bytes.Buffer
	if err := Export(a, &csvBuf, "csv"); err != nil || strings.Count(csvBuf.String(), "\n") != 3 {
		t.Fatalf("csv: %v %q", err, csvBuf.String())
	}
	if err := Export(a, &jsonBuf, "json"); err != nil || !strings.HasPrefix(jsonBuf.String(), "[") {
		t.Fatalf("json: %v", err)
	}

	// Path traversal is flattened to a file name inside dir.
	if _, err := Import(a, dir, "../../etc/passwd"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSettingsAndAuto(t *testing.T) {
	d, _ := store.Open(filepath.Join(t.TempDir(), "x.db"))
	t.Cleanup(func() { d.Close() })
	s := Load(d)
	if !s.Auto || s.KeepMonths != 0 {
		t.Fatalf("defaults: %+v", s)
	}
	dir := filepath.Join(t.TempDir(), "arch")
	if err := Save(d, Settings{Dir: dir, Auto: true, KeepMonths: 6}); err != nil {
		t.Fatal(err)
	}
	if err := Save(d, Settings{Dir: "relative"}); err == nil {
		t.Fatal("relative dir accepted")
	}
	seed(t, d, "u", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	AutoIfDue(d)
	AutoIfDue(d) // second call in the same month must not add another
	if len(List(dir)) != 1 {
		t.Fatalf("auto snapshots = %d", len(List(dir)))
	}
}

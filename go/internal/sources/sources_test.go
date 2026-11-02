package sources

import (
	"path/filepath"
	"testing"
)

func TestParseArg(t *testing.T) {
	e, err := ParseArg("codex=codex:~/.codex/sessions")
	if err != nil || e.Kind != "codex" || e.Path != "~/.codex/sessions" {
		t.Fatalf("%+v %v", e, err)
	}
	e, _ = ParseArg("hermes=/x/y") // bare path: name is the kind
	if e.Kind != "hermes" || e.Path != "/x/y" {
		t.Fatalf("%+v", e)
	}
	e, _ = ParseArg(`generic=C:\logs`) // Windows drive colon is not a kind separator
	if e.Kind != "generic" || e.Path != `C:\logs` {
		t.Fatalf("%+v", e)
	}
	for _, bad := range []string{"nokind", "x=y:z", "=codex:/p", "x="} {
		if _, err := ParseArg(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestResolveOrderOverrideDisableAndPin(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "s.json")
	f := false
	if err := SaveFile(cfg, []Entry{{Name: "codex", Kind: "codex", Path: "/cfg"}, {Name: "claude", Kind: "claude", Path: "/mine", Enabled: &f}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager("/claude", false, []string{"codex=codex:/flag"}, cfg)
	got, _ := m.Effective()
	if len(got) != 1 || got[0].Name != "codex" || got[0].Path != "/flag" {
		t.Fatalf("%+v", got)
	}
	got, _ = NewManager("/claude", true, nil, cfg).Effective()
	if len(got) != 1 || got[0].Name != "claude" || got[0].Path != "/claude" {
		t.Fatalf("pinned: %+v", got)
	}
}

func TestPythonCompatibleFile(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "s.json")
	_ = SaveFile(cfg, []Entry{{Name: "oc", Kind: "generic", Path: "/p"}})
	es, err := LoadFile(cfg)
	if err != nil || len(es) != 1 || es[0].On() != true {
		t.Fatalf("%+v %v", es, err)
	}
}

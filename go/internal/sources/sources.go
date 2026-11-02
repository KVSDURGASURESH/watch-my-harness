// Package sources decides which agents' session logs get scanned: Claude Code's
// default folder, entries from the editable JSON file (the same file the Python
// build reads), then repeatable --source name=kind:path flags. It also detects
// the well-known harness folders for the first-run setup screen.
package sources

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/scanner"
)

const CodexDefault = "~/.codex/sessions"

func home() string { h, _ := os.UserHomeDir(); return h }

func DefaultConfigPath() string {
	if p := os.Getenv("TOKEN_DASHBOARD_SOURCES"); p != "" {
		return p
	}
	return filepath.Join(home(), ".claude", "token-dashboard-sources.json")
}

// Entry is one row of the sources JSON. Python reads name/kind/path/model_alias and
// ignores the rest, so "enabled" is a Go-side addition that stays backward compatible.
type Entry struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Path       string            `json:"path"`
	ModelAlias map[string]string `json:"model_alias,omitempty"`
	Enabled    *bool             `json:"enabled,omitempty"`
}

func (e Entry) On() bool { return e.Enabled == nil || *e.Enabled }

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)

func Validate(es []Entry) error {
	seen := map[string]bool{}
	for _, e := range es {
		if !nameRe.MatchString(e.Name) {
			return fmt.Errorf("bad source name %q (letters, digits, _ . - ; max 40)", e.Name)
		}
		if !scanner.ValidKind(e.Kind) {
			return fmt.Errorf("source %q: kind must be one of %v", e.Name, scanner.Kinds)
		}
		if strings.TrimSpace(e.Path) == "" {
			return fmt.Errorf("source %q: path is required", e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("duplicate source name %q", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

// ParseArg parses "name=kind:path" (or "name=path" when name is itself a kind).
func ParseArg(spec string) (Entry, error) {
	name, rest, ok := strings.Cut(spec, "=")
	if !ok || name == "" || rest == "" {
		return Entry{}, fmt.Errorf("bad --source %q; expected name=kind:path, e.g. codex=codex:~/.codex/sessions", spec)
	}
	kind, path, found := strings.Cut(rest, ":")
	// "kind:path" vs a bare path (which may itself contain ':' on Windows)
	if !found || !scanner.ValidKind(kind) {
		kind, path = name, rest
	}
	if !scanner.ValidKind(kind) {
		return Entry{}, fmt.Errorf("unknown kind %q in --source %q; expected one of %v", kind, spec, scanner.Kinds)
	}
	return Entry{Name: name, Kind: kind, Path: path}, nil
}

func LoadFile(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var es []Entry
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range es {
		if !scanner.ValidKind(e.Kind) || e.Name == "" || e.Path == "" {
			return nil, fmt.Errorf("%s: each entry needs name, kind (%v), path; got %+v", path, scanner.Kinds, e)
		}
	}
	return es, nil
}

func SaveFile(path string, es []Entry) error {
	if err := Validate(es); err != nil {
		return err
	}
	if es == nil {
		es = []Entry{}
	}
	b, _ := json.MarshalIndent(es, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Manager resolves the effective source list on demand, so edits made in the UI
// take effect on the next scan without a restart.
type Manager struct {
	mu         sync.Mutex
	ClaudeDir  string
	Pinned     bool     // --projects-dir given: the config file is ignored
	Extra      []string // --source flags
	ConfigPath string
}

func NewManager(claudeDir string, pinned bool, extra []string, configPath string) *Manager {
	if configPath == "" {
		configPath = DefaultConfigPath()
	}
	return &Manager{ClaudeDir: claudeDir, Pinned: pinned, Extra: extra, ConfigPath: configPath}
}

// Entries are the editable file entries.
func (m *Manager) Entries() ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return LoadFile(m.ConfigPath)
}

func (m *Manager) SetEntries(es []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return SaveFile(m.ConfigPath, es)
}

func (m *Manager) Effective() ([]scanner.Source, error) {
	by := []Entry{{Name: "claude", Kind: "claude", Path: m.ClaudeDir}}
	put := func(e Entry) {
		for i := range by {
			if by[i].Name == e.Name { // later replaces earlier, keeping its position
				by[i] = e
				return
			}
		}
		by = append(by, e)
	}
	if !m.Pinned {
		es, err := m.Entries()
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			put(e)
		}
	}
	for _, spec := range m.Extra {
		e, err := ParseArg(spec)
		if err != nil {
			return nil, err
		}
		put(e)
	}
	var out []scanner.Source
	for _, e := range by {
		if e.On() {
			out = append(out, scanner.Source{Name: e.Name, Kind: e.Kind, Path: e.Path, ModelAlias: e.ModelAlias})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- detection

type Detected struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Files  int    `json:"files"`
}

func countFiles(s scanner.Source) int {
	root := scanner.ExpandHome(s.Path)
	n := 0
	if s.Kind == "hermes" {
		for _, p := range []string{filepath.Join(root, "state.db")} {
			if _, err := os.Stat(p); err == nil {
				n++
			}
		}
		if es, err := os.ReadDir(filepath.Join(root, "profiles")); err == nil {
			for _, e := range es {
				if _, err := os.Stat(filepath.Join(root, "profiles", e.Name(), "state.db")); err == nil {
					n++
				}
			}
		}
		return n
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			n++
		}
		return nil
	})
	return n
}

// Detect looks for the known harness folders. claudeDir is the (possibly overridden) Claude path.
func Detect(claudeDir string) []Detected {
	hermes := os.Getenv("HERMES_HOME")
	if hermes == "" {
		hermes = "~/.hermes"
	}
	cands := []scanner.Source{
		{Name: "claude", Kind: "claude", Path: claudeDir},
		{Name: "codex", Kind: "codex", Path: CodexDefault},
		{Name: "hermes", Kind: "hermes", Path: hermes},
	}
	out := []Detected{}
	for _, c := range cands {
		d := Detected{Name: c.Name, Kind: c.Kind, Path: c.Path}
		if st, err := os.Stat(scanner.ExpandHome(c.Path)); err == nil && (st.IsDir() || c.Kind != "hermes") {
			if d.Files = countFiles(c); d.Files > 0 {
				d.Exists = true
			}
		}
		out = append(out, d)
	}
	return out
}

// Package skills locates SKILL.md files so the Skills tab can show tokens-per-call.
package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Info struct {
	Path   string
	Chars  int64
	Tokens int64
}

var versionRe = regexp.MustCompile(`^\d+\.\d+`)
var structure = map[string]bool{"skills": true, "plugins": true, "marketplaces": true, "cache": true, ".claude": true}

func defaultRoots() []string {
	if raw := os.Getenv("TOKEN_DASHBOARD_SKILL_ROOTS"); raw != "" {
		var out []string
		for _, p := range filepath.SplitList(raw) {
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	h, _ := os.UserHomeDir()
	return []string{
		filepath.Join(h, ".claude", "skills"), filepath.Join(h, ".claude", "scheduled-tasks"), filepath.Join(h, ".claude", "plugins"),
		filepath.Join(h, ".codex", "skills"), filepath.Join(h, ".codex", "plugins"), filepath.Join(h, ".agents", "skills"),
	}
}

// pathParts mirrors pathlib.Path.parts (absolute posix paths start with "/").
func pathParts(p string) []string {
	p = filepath.ToSlash(p)
	var parts []string
	if strings.HasPrefix(p, "/") {
		parts = append(parts, "/")
	}
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}

// slugsFor returns the slug(s) a Skill tool call could use to load this file: the bare
// skill name plus <ancestor>:<skill> for every plausible plugin-name segment above skills/.
func slugsFor(skillMD string) []string {
	if filepath.Base(skillMD) != "SKILL.md" {
		return nil
	}
	parts := pathParts(skillMD)
	name := filepath.Base(filepath.Dir(skillMD))
	slugs := map[string]bool{name: true}
	idx := -1
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "skills" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{name}
	}
	for _, seg := range parts[:idx] {
		if seg == "" || structure[seg] || versionRe.MatchString(seg) || strings.HasPrefix(seg, "temp_git_") || strings.Contains(seg, ":") {
			continue
		}
		slugs[seg+":"+name] = true
	}
	out := make([]string, 0, len(slugs))
	for s := range slugs {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ScanCatalog maps slug -> info; for duplicate slugs the shallowest path wins.
func ScanCatalog(roots []string) map[string]Info {
	cat := map[string]Info{}
	for _, root := range roots {
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			continue
		}
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			st, err := d.Info()
			if err != nil {
				return nil
			}
			e := Info{Path: p, Chars: st.Size(), Tokens: st.Size() / 4}
			for _, slug := range slugsFor(p) {
				if prev, ok := cat[slug]; !ok || len(pathParts(p)) < len(pathParts(prev.Path)) {
					cat[slug] = e
				}
			}
			return nil
		})
	}
	return cat
}

var (
	mu    sync.Mutex
	at    time.Time
	cache map[string]Info
)

// Cached is ScanCatalog(default roots) with a 60 s TTL.
func Cached() map[string]Info {
	mu.Lock()
	defer mu.Unlock()
	if cache == nil || time.Since(at) > 60*time.Second {
		cache, at = ScanCatalog(defaultRoots()), time.Now()
	}
	return cache
}

// TokensFor is nil when the slug is unknown.
func TokensFor(slug string, cat map[string]Info) any {
	if i, ok := cat[slug]; ok {
		return i.Tokens
	}
	return nil
}

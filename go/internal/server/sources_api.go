package server

import (
	"encoding/json"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/pricing"
	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/sources"
)

// Go-only endpoints behind the Settings > Sources editor and the first-run setup screen.
// The Python server has none of these; the UI feature-detects them (404 => hide).

func (s *Server) setupComplete() bool {
	_, ok := s.db.GetKV("setup_complete")
	return ok
}

func (s *Server) getSources(c *reqCtx) {
	entries, err := s.src.Entries()
	if err != nil {
		c.fail(500, err.Error())
		return
	}
	if entries == nil {
		entries = []sources.Entry{}
	}
	eff, err := s.src.Effective()
	if err != nil {
		c.fail(500, err.Error())
		return
	}
	c.send(map[string]any{
		"sources":        entries,
		"effective":      eff,
		"detected":       sources.Detect(s.src.ClaudeDir),
		"setup_complete": s.setupComplete(),
		"config_path":    s.src.ConfigPath,
		"pinned":         s.src.Pinned,
		"kinds":          []string{"claude", "codex", "hermes", "generic"},
	}, 200)
}

func parseEntries(body map[string]any) ([]sources.Entry, error) {
	raw, _ := json.Marshal(body["sources"])
	var es []sources.Entry
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, err
	}
	if err := sources.Validate(es); err != nil {
		return nil, err
	}
	return es, nil
}

func (s *Server) postSources(c *reqCtx, body map[string]any) {
	if s.src.Pinned {
		c.fail(409, "sources are pinned by --projects-dir / CLAUDE_PROJECTS_DIR")
		return
	}
	es, err := parseEntries(body)
	if err != nil {
		c.fail(400, err.Error())
		return
	}
	if err := s.src.SetEntries(es); err != nil {
		c.fail(500, err.Error())
		return
	}
	s.bump()
	s.StartScan()
	c.send(map[string]any{"ok": true}, 200)
}

func (s *Server) postSetupComplete(c *reqCtx, body map[string]any) {
	if _, has := body["sources"]; has && !s.src.Pinned {
		es, err := parseEntries(body)
		if err != nil {
			c.fail(400, err.Error())
			return
		}
		if err := s.src.SetEntries(es); err != nil {
			c.fail(500, err.Error())
			return
		}
	}
	if plan, ok := body["plan"].(string); ok && plan != "" {
		pricing.SetPlan(s.db, plan, "")
	}
	s.db.SetKV("setup_complete", "1")
	s.bump()
	s.StartScan()
	c.send(map[string]any{"ok": true}, 200)
}

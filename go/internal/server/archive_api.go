package server

import (
	"net/http"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/archive"
)

// Go-only archive endpoints behind the Settings > Archive card.

func (s *Server) archiveGet(c *reqCtx, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/archive":
		st := archive.Load(s.db)
		c.send(map[string]any{"settings": st, "snapshots": archive.List(st.Dir), "default_dir": archive.DefaultDir()}, 200)
	case "/api/archive/export":
		f := r.URL.Query().Get("format")
		if f != "json" {
			f = "csv"
		}
		w.Header().Set("Content-Type", map[string]string{"csv": "text/csv", "json": "application/json"}[f])
		w.Header().Set("Content-Disposition", `attachment; filename="watch-my-harness-export.`+f+`"`)
		_ = archive.Export(s.db, w, f)
	default:
		w.WriteHeader(404)
	}
}

func (s *Server) archivePost(c *reqCtx, body map[string]any) {
	st := archive.Load(s.db)
	switch c.path {
	case "/api/archive/settings":
		if v, ok := body["dir"].(string); ok {
			st.Dir = v
		}
		if v, ok := body["auto"].(bool); ok {
			st.Auto = v
		}
		if v, ok := body["keep_months"].(float64); ok {
			st.KeepMonths = int(v)
		}
		if err := archive.Save(s.db, st); err != nil {
			c.fail(400, err.Error())
			return
		}
		c.send(map[string]any{"ok": true}, 200)
	case "/api/archive/snapshot":
		name, err := archive.Take(s.db, st.Dir)
		if err != nil {
			c.fail(500, err.Error())
			return
		}
		c.send(map[string]any{"ok": true, "name": name}, 200)
	case "/api/archive/import":
		name, _ := body["name"].(string)
		n, err := archive.Import(s.db, st.Dir, name)
		if err != nil {
			c.fail(400, err.Error())
			return
		}
		s.bump()
		c.send(map[string]any{"ok": true, "added": n}, 200)
	case "/api/archive/prune":
		if st.KeepMonths <= 0 {
			c.fail(400, "set 'keep months' above 0 first")
			return
		}
		name, err := archive.Take(s.db, st.Dir) // always snapshot before deleting
		if err != nil {
			c.fail(500, "snapshot failed, nothing deleted: "+err.Error())
			return
		}
		n, err := archive.Prune(s.db, st.KeepMonths)
		if err != nil {
			c.fail(500, err.Error())
			return
		}
		s.bump()
		c.send(map[string]any{"ok": true, "removed": n, "snapshot": name}, 200)
	default:
		c.w.WriteHeader(404)
	}
}

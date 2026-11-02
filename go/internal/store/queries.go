package store

import (
	"fmt"
	"sort"
	"strings"
)

// foldAgents collapses (key, source) rows into one row per key with a by_agent map
// and an agents list (db.py _fold_agents).
func foldAgents(rows []Row, key string, sums []string, primary string, maxes []string) []Row {
	order := []string{}
	out := map[string]Row{}
	isSpecial := map[string]bool{"source": true, key: true}
	for _, f := range sums {
		isSpecial[f] = true
	}
	for _, f := range maxes {
		isSpecial[f] = true
	}
	// keys of by_agent in insertion order, per key
	byOrder := map[string][]string{}
	for _, r := range rows {
		kv := fmt.Sprint(r[key])
		o, ok := out[kv]
		if !ok {
			o = Row{key: r[key], "by_agent": map[string]any{}}
			for _, f := range sums {
				o[f] = int64(0)
			}
			out[kv] = o
			order = append(order, kv)
		}
		for _, f := range sums {
			o[f] = Int(o[f]) + Int(r[f])
		}
		for _, f := range maxes {
			cur, _ := o[f].(string)
			nv, _ := r[f].(string)
			switch {
			case nv != "" && (cur == "" || nv > cur):
				o[f] = nv
			case cur == "" && nv == "":
				o[f] = nil
			}
		}
		for f, v := range r {
			if !isSpecial[f] {
				if _, has := o[f]; !has {
					o[f] = v
				}
			}
		}
		src := Str(r["source"])
		ba := o["by_agent"].(map[string]any)
		if _, seen := ba[src]; !seen {
			byOrder[kv] = append(byOrder[kv], src)
		}
		prev := int64(0)
		if p, ok := ba[src]; ok {
			prev = p.(int64)
		}
		ba[src] = prev + Int(r[primary])
	}
	res := make([]Row, 0, len(order))
	for _, kv := range order {
		o := out[kv]
		agents := append([]string{}, byOrder[kv]...)
		ba := o["by_agent"].(map[string]any)
		sort.SliceStable(agents, func(i, j int) bool { return ba[agents[i]].(int64) > ba[agents[j]].(int64) })
		al := make([]any, len(agents))
		for i, a := range agents {
			al[i] = a
		}
		o["agents"] = al
		res = append(res, o)
	}
	return res
}

func OverviewTotals(d *DB, since, until, source string) Row {
	rng, args := RangeClause(since, until, source)
	return d.One(fmt.Sprintf(`
      SELECT COUNT(DISTINCT session_id) AS sessions,
             SUM(CASE WHEN type='user' THEN 1 ELSE 0 END) AS turns,
             COALESCE(SUM(input_tokens),0)            AS input_tokens,
             COALESCE(SUM(output_tokens),0)           AS output_tokens,
             COALESCE(SUM(cache_read_tokens),0)       AS cache_read_tokens,
             COALESCE(SUM(cache_create_5m_tokens),0)  AS cache_create_5m_tokens,
             COALESCE(SUM(cache_create_1h_tokens),0)  AS cache_create_1h_tokens
        FROM messages WHERE 1=1 %s`, rng), args...)
}

func ExpensivePrompts(d *DB, limit int, sort, source string) []Row {
	order := "billable_tokens DESC"
	if sort == "recent" {
		order = "u.timestamp DESC"
	}
	src, args := "", []any{}
	if source != "" {
		src, args = "AND u.source = ?", []any{source}
	}
	args = append(args, limit)
	return d.Rows(fmt.Sprintf(`
      SELECT u.uuid AS user_uuid, u.session_id, u.project_slug, u.timestamp, u.source,
             u.prompt_text, u.prompt_chars,
             a.uuid AS assistant_uuid, a.model,
             COALESCE(a.input_tokens,0)+COALESCE(a.output_tokens,0)
               +COALESCE(a.cache_create_5m_tokens,0)+COALESCE(a.cache_create_1h_tokens,0) AS billable_tokens,
             COALESCE(a.cache_read_tokens,0) AS cache_read_tokens
        FROM messages u
        JOIN messages a ON a.parent_uuid = u.uuid AND a.type='assistant'
       WHERE u.type='user' AND u.prompt_text IS NOT NULL %s
       ORDER BY %s
       LIMIT ?`, src, order), args...)
}

func (d *DB) cwdsFor(slug string) []string {
	var out []string
	for _, r := range d.Rows("SELECT DISTINCT cwd FROM messages WHERE project_slug=? AND cwd IS NOT NULL", slug) {
		out = append(out, Str(r["cwd"]))
	}
	return out
}

func ProjectSummary(d *DB, since, until, source string) []Row {
	rng, args := RangeClause(since, until, source)
	raw := d.Rows(fmt.Sprintf(`
      SELECT project_slug, source,
             COUNT(DISTINCT session_id) AS sessions,
             SUM(CASE WHEN type='user' THEN 1 ELSE 0 END) AS turns,
             COALESCE(SUM(input_tokens), 0)  AS input_tokens,
             COALESCE(SUM(output_tokens), 0) AS output_tokens,
             SUM(input_tokens)+SUM(output_tokens)
               +SUM(cache_create_5m_tokens)+SUM(cache_create_1h_tokens) AS billable_tokens,
             SUM(cache_read_tokens) AS cache_read_tokens
        FROM messages m
       WHERE 1=1 %s
       GROUP BY project_slug, source`, rng), args...)
	rows := foldAgents(raw, "project_slug",
		[]string{"sessions", "turns", "input_tokens", "output_tokens", "billable_tokens", "cache_read_tokens"},
		"billable_tokens", nil)
	sort.SliceStable(rows, func(i, j int) bool { return Int(rows[i]["billable_tokens"]) > Int(rows[j]["billable_tokens"]) })
	for _, r := range rows {
		slug := Str(r["project_slug"])
		r["project_name"] = BestProjectName(d.cwdsFor(slug), slug)
	}
	return rows
}

func ToolTokenBreakdown(d *DB, since, until, source string) []Row {
	rng, args := RangeClause(since, until, source)
	raw := d.Rows(fmt.Sprintf(`
      SELECT tool_name, source,
             COUNT(*) AS calls,
             COALESCE(SUM(result_tokens),0) AS result_tokens
        FROM tool_calls
       WHERE tool_name != '_tool_result' %s
       GROUP BY tool_name, source`, rng), args...)
	rows := foldAgents(raw, "tool_name", []string{"calls", "result_tokens"}, "calls", nil)
	sort.SliceStable(rows, func(i, j int) bool { return Int(rows[i]["calls"]) > Int(rows[j]["calls"]) })
	return rows
}

func RecentSessions(d *DB, limit int, since, until, source string) []Row {
	rng, args := RangeClause(since, until, source)
	args = append(args, limit)
	rows := d.Rows(fmt.Sprintf(`
      SELECT session_id, project_slug, MIN(source) AS source,
             MIN(timestamp) AS started, MAX(timestamp) AS ended,
             SUM(CASE WHEN type='user' THEN 1 ELSE 0 END) AS turns,
             SUM(input_tokens)+SUM(output_tokens) AS tokens
        FROM messages m
       WHERE 1=1 %s
       GROUP BY session_id
       ORDER BY ended DESC
       LIMIT ?`, rng), args...)
	cache := map[string]string{}
	for _, r := range rows {
		slug := Str(r["project_slug"])
		n, ok := cache[slug]
		if !ok {
			n = BestProjectName(d.cwdsFor(slug), slug)
			cache[slug] = n
		}
		r["project_name"] = n
	}
	return rows
}

func SessionTurns(d *DB, sessionID string) []Row {
	return d.Rows(`
      SELECT uuid, parent_uuid, type, timestamp, model, is_sidechain, agent_id,
             input_tokens, output_tokens, cache_read_tokens,
             cache_create_5m_tokens, cache_create_1h_tokens,
             prompt_text, prompt_chars, tool_calls_json, project_slug, cwd, source
        FROM messages
       WHERE session_id = ?
       ORDER BY timestamp ASC`, sessionID)
}

func DailyTokenBreakdown(d *DB, since, until, source string) []Row {
	rng, args := RangeClause(since, until, source)
	return d.Rows(fmt.Sprintf(`
      SELECT substr(timestamp, 1, 10) AS day,
             COALESCE(SUM(input_tokens),0)      AS input_tokens,
             COALESCE(SUM(output_tokens),0)     AS output_tokens,
             COALESCE(SUM(cache_read_tokens),0) AS cache_read_tokens,
             COALESCE(SUM(cache_create_5m_tokens),0)
               + COALESCE(SUM(cache_create_1h_tokens),0) AS cache_create_tokens
        FROM messages
       WHERE timestamp IS NOT NULL %s
       GROUP BY day
       ORDER BY day ASC`, rng), args...)
}

func SkillBreakdown(d *DB, since, until, source string) []Row {
	rng, args := RangeClause(since, until, source)
	raw := d.Rows(fmt.Sprintf(`
      SELECT target AS skill, source,
             COUNT(*) AS invocations,
             COUNT(DISTINCT session_id) AS sessions,
             MAX(timestamp) AS last_used
        FROM tool_calls
       WHERE tool_name = 'Skill' AND target IS NOT NULL AND target != '' %s
       GROUP BY target, source`, rng), args...)
	rows := foldAgents(raw, "skill", []string{"invocations", "sessions"}, "invocations", []string{"last_used"})
	sort.SliceStable(rows, func(i, j int) bool { return Int(rows[i]["invocations"]) > Int(rows[j]["invocations"]) })
	return rows
}

var modelSums = []string{"turns", "input_tokens", "output_tokens", "cache_read_tokens", "cache_create_5m_tokens", "cache_create_1h_tokens"}

func ModelBreakdown(d *DB, since, until, source string) []Row {
	rng, args := RangeClause(since, until, source)
	raw := d.Rows(fmt.Sprintf(`
      SELECT COALESCE(model, 'unknown') AS model, source,
             COUNT(*) AS turns,
             COALESCE(SUM(input_tokens),0)            AS input_tokens,
             COALESCE(SUM(output_tokens),0)           AS output_tokens,
             COALESCE(SUM(cache_read_tokens),0)       AS cache_read_tokens,
             COALESCE(SUM(cache_create_5m_tokens),0)  AS cache_create_5m_tokens,
             COALESCE(SUM(cache_create_1h_tokens),0)  AS cache_create_1h_tokens
        FROM messages
       WHERE type = 'assistant' %s
       GROUP BY model, source`, rng), args...)
	rows := foldAgents(raw, "model", modelSums, "turns", nil)
	billable := func(r Row) int64 {
		return Int(r["input_tokens"]) + Int(r["output_tokens"]) + Int(r["cache_create_5m_tokens"]) + Int(r["cache_create_1h_tokens"])
	}
	sort.SliceStable(rows, func(i, j int) bool { return billable(rows[i]) > billable(rows[j]) })
	return rows
}

func SourceBreakdown(d *DB, since, until string) []Row {
	rng, args := RangeClause(since, until, "")
	return d.Rows(fmt.Sprintf(`
      SELECT source,
             COUNT(DISTINCT session_id) AS sessions,
             SUM(CASE WHEN type='user' THEN 1 ELSE 0 END) AS turns,
             COALESCE(SUM(input_tokens),0)            AS input_tokens,
             COALESCE(SUM(output_tokens),0)           AS output_tokens,
             COALESCE(SUM(cache_read_tokens),0)       AS cache_read_tokens,
             COALESCE(SUM(cache_create_5m_tokens),0)  AS cache_create_5m_tokens,
             COALESCE(SUM(cache_create_1h_tokens),0)  AS cache_create_1h_tokens
        FROM messages
       WHERE 1=1 %s
       GROUP BY source
       ORDER BY (input_tokens + output_tokens) DESC`, rng), args...)
}

// Distinct sources present in messages, sorted (tips).
func Agents(d *DB) []string {
	var out []string
	for _, r := range d.Rows("SELECT DISTINCT source FROM messages ORDER BY source") {
		out = append(out, Str(r["source"]))
	}
	return out
}

// HasPlanKey reports whether a plan row exists (used by /api/plan "configured").
func (d *DB) HasKey(k string) bool {
	return d.One("SELECT 1 AS x FROM plan WHERE k=?", k) != nil
}

func (d *DB) GetKV(k string) (string, bool) {
	r := d.One("SELECT v FROM plan WHERE k=?", k)
	if r == nil {
		return "", false
	}
	return Str(r["v"]), true
}

func (d *DB) SetKV(k, v string) {
	if _, err := d.Exec("INSERT OR REPLACE INTO plan (k, v) VALUES (?, ?)", k, v); err != nil {
		panic(err)
	}
}

// Placeholders builds "?,?,?" for IN lists.
func Placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

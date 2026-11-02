// Package tips ports token_dashboard/tips.py and recommendations.py.
package tips

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

type Tip = store.Row

func srcClause(source string, toolTable bool) (string, []any) {
	if source == "" {
		return "", nil
	}
	if toolTable {
		return " AND session_id IN (SELECT session_id FROM messages WHERE source = ?)", []any{source}
	}
	return " AND source = ?", []any{source}
}

// isoformat mimics datetime.isoformat(): fractional part only when microseconds != 0.
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func nowISO() string { return isoformat(time.Now().UTC()) }

func isoDaysAgo(todayISO string, n int) string {
	s := strings.TrimSuffix(todayISO, "Z")
	var t time.Time
	var err error
	for _, layout := range []string{"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err = time.Parse(layout, s); err == nil {
			break
		}
	}
	if err != nil {
		t = time.Now().UTC()
	}
	return isoformat(t.AddDate(0, 0, -n))
}

func key(category, scope string) string { return category + ":" + scope }

func isDismissed(d *store.DB, k string) bool {
	r := d.One("SELECT dismissed_at FROM dismissed_tips WHERE tip_key=?", k)
	if r == nil {
		return false
	}
	return float64(time.Now().UnixNano())/1e9-store.Float(r["dismissed_at"]) < 14*86400
}

func Dismiss(d *store.DB, k string) {
	if _, err := d.Exec("INSERT OR REPLACE INTO dismissed_tips (tip_key, dismissed_at) VALUES (?, ?)", k, float64(time.Now().UnixNano())/1e9); err != nil {
		panic(err)
	}
}

// comma is Python's f"{n:,}".
func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func cacheDisciplineTips(d *store.DB, today, source string) []Tip {
	since := isoDaysAgo(today, 7)
	src, sargs := srcClause(source, false)
	var out []Tip
	for _, row := range d.Rows(fmt.Sprintf(`
      SELECT project_slug,
             SUM(cache_read_tokens) AS cr,
             SUM(input_tokens + cache_create_5m_tokens + cache_create_1h_tokens) AS rebuild
        FROM messages
       WHERE type='assistant' AND timestamp >= ? %s
       GROUP BY project_slug
       HAVING (cr + rebuild) > 100000`, src), append([]any{since}, sargs...)...) {
		cr, rebuild := store.Int(row["cr"]), store.Int(row["rebuild"])
		total := cr + rebuild
		hit := 0.0
		if total != 0 {
			hit = float64(cr) / float64(total)
		}
		if hit < 0.40 {
			slug := store.Str(row["project_slug"])
			k := key("cache", slug)
			if isDismissed(d, k) {
				continue
			}
			out = append(out, Tip{
				"key": k, "category": "cache",
				"title": fmt.Sprintf("Low cache hit rate in %s", slug),
				"body":  fmt.Sprintf("Cache hit rate is %.0f%% over the last 7 days. Sessions that restart context frequently rebuild cache. Consider longer-lived sessions or fewer context resets.", hit*100),
				"scope": slug,
			})
		}
	}
	return out
}

func repeatedTargetTips(d *store.DB, today, source string) []Tip {
	since := isoDaysAgo(today, 7)
	src, sargs := srcClause(source, true)
	args := append([]any{since}, sargs...)
	var out []Tip
	for _, row := range d.Rows(fmt.Sprintf(`
          SELECT target, COUNT(*) AS n, COUNT(DISTINCT session_id) AS sessions
            FROM tool_calls
           WHERE tool_name IN ('Read','Edit','Write') AND target IS NOT NULL AND timestamp >= ? %s
           GROUP BY target HAVING n > 10
           ORDER BY n DESC LIMIT 10`, src), args...) {
		t := store.Str(row["target"])
		k := key("repeat-file", t)
		if isDismissed(d, k) {
			continue
		}
		n, s := store.Int(row["n"]), store.Int(row["sessions"])
		out = append(out, Tip{"key": k, "category": "repeat-file",
			"title": fmt.Sprintf("%s read %d times", t, n),
			"body":  fmt.Sprintf("This file was opened %d times across %d sessions in the past 7 days. A summary in CLAUDE.md or one read per session would avoid repeats.", n, s),
			"scope": t})
	}
	for _, row := range d.Rows(fmt.Sprintf(`
          SELECT target, COUNT(*) AS n
            FROM tool_calls
           WHERE tool_name='Bash' AND target IS NOT NULL AND timestamp >= ? %s
           GROUP BY target HAVING n > 15
           ORDER BY n DESC LIMIT 10`, src), args...) {
		t := store.Str(row["target"])
		k := key("repeat-bash", t)
		if isDismissed(d, k) {
			continue
		}
		n := store.Int(row["n"])
		out = append(out, Tip{"key": k, "category": "repeat-bash",
			"title": fmt.Sprintf("`%s` ran %d times", t, n),
			"body":  fmt.Sprintf("This bash command ran %d times in the past 7 days. Consider a watch flag or shell alias.", n),
			"scope": t})
	}
	return out
}

func rightSizeTips(d *store.DB, today, source string) []Tip {
	since := isoDaysAgo(today, 7)
	src, sargs := srcClause(source, false)
	row := d.One(fmt.Sprintf(`
      SELECT COUNT(*) AS n,
             SUM(input_tokens+cache_create_5m_tokens+cache_create_1h_tokens) AS in_tok,
             SUM(output_tokens) AS out_tok
        FROM messages
       WHERE type='assistant' AND model LIKE '%%opus%%'
         AND output_tokens < 500 AND is_sidechain = 0
         AND timestamp >= ? %s`, src), append([]any{since}, sargs...)...)
	if row == nil || store.Int(row["n"]) < 10 {
		return nil
	}
	in, out := float64(store.Int(row["in_tok"])), float64(store.Int(row["out_tok"]))
	apiOpus := (in*15 + out*75) / 1_000_000
	apiSonnet := (in*3 + out*15) / 1_000_000
	savings := apiOpus - apiSonnet
	if savings < 1.0 {
		return nil
	}
	k := key("right-size", "opus-short-turns-7d")
	if isDismissed(d, k) {
		return nil
	}
	n := store.Int(row["n"])
	return []Tip{{"key": k, "category": "right-size",
		"title": fmt.Sprintf("%d short Opus turns might fit on Sonnet", n),
		"body":  fmt.Sprintf("Opus turns under 500 output tokens cost ~$%.2f in the last 7 days. Sonnet would have cost ~$%.2f (savings ~$%.2f).", apiOpus, apiSonnet, savings),
		"scope": "opus-short-turns-7d"}}
}

func outlierTips(d *store.DB, today, source string) []Tip {
	since := isoDaysAgo(today, 7)
	var out []Tip
	tsrc, targs := srcClause(source, true)
	if big := d.One(fmt.Sprintf(`
          SELECT COUNT(*) AS n, AVG(result_tokens) AS avg_t
            FROM tool_calls
           WHERE tool_name='_tool_result' AND result_tokens > 50000 AND timestamp >= ? %s`, tsrc), append([]any{since}, targs...)...); big != nil && store.Int(big["n"]) >= 5 {
		k := key("tool-bloat", "result-50k+")
		if !isDismissed(d, k) {
			out = append(out, Tip{"key": k, "category": "tool-bloat",
				"title": fmt.Sprintf("%d tool results over 50k tokens this week", store.Int(big["n"])),
				"body":  fmt.Sprintf("Average size is %s tokens. Pipe long Bash output to head/tail and ask for narrower file reads.", comma(int64(store.Float(big["avg_t"])))),
				"scope": "result-50k+"})
		}
	}
	msrc, margs := srcClause(source, false)
	for _, row := range d.Rows(fmt.Sprintf(`
          SELECT agent_id, COUNT(*) AS n,
                 AVG(input_tokens+output_tokens) AS mean_t,
                 MAX(input_tokens+output_tokens) AS max_t
            FROM messages
           WHERE is_sidechain=1 AND agent_id IS NOT NULL AND timestamp >= ? %s
           GROUP BY agent_id HAVING n >= 10`, msrc), append([]any{since}, margs...)...) {
		mx, mean := store.Float(row["max_t"]), store.Float(row["mean_t"])
		if mean == 0 {
			mean = 1
		}
		if mx > 6*mean && mx > 50_000 {
			id := store.Str(row["agent_id"])
			k := key("subagent-outlier", id)
			if isDismissed(d, k) {
				continue
			}
			out = append(out, Tip{"key": k, "category": "subagent-outlier",
				"title": fmt.Sprintf("Subagent %s has cost outliers", id),
				"body":  fmt.Sprintf("Largest invocation used %s tokens vs mean %s. Worth checking what those did differently.", comma(int64(mx)), comma(int64(store.Float(row["mean_t"])))),
				"scope": id})
		}
	}
	return out
}

func allTipsFor(d *store.DB, today, source string) []Tip {
	var out []Tip
	out = append(out, cacheDisciplineTips(d, today, source)...)
	out = append(out, repeatedTargetTips(d, today, source)...)
	out = append(out, rightSizeTips(d, today, source)...)
	out = append(out, outlierTips(d, today, source)...)
	return out
}

func with(t Tip, extra Tip) Tip {
	n := Tip{}
	for k, v := range t {
		n[k] = v
	}
	for k, v := range extra {
		n[k] = v
	}
	return n
}

// All tags every tip with the agent whose sessions produced it. With no agent picked
// the rules run once per agent and keys get an agent prefix so each can be dismissed
// on its own. todayISO "" means now (UTC).
func All(d *store.DB, todayISO, source string) []Tip {
	if todayISO == "" {
		todayISO = nowISO()
	}
	out := []Tip{}
	if source != "" {
		for _, t := range allTipsFor(d, todayISO, source) {
			out = append(out, with(t, Tip{"source": source}))
		}
		return out
	}
	agents := store.Agents(d)
	if len(agents) <= 1 {
		a := "claude"
		if len(agents) == 1 {
			a = agents[0]
		}
		for _, t := range allTipsFor(d, todayISO, "") {
			out = append(out, with(t, Tip{"source": a}))
		}
		return out
	}
	for _, a := range agents {
		for _, t := range allTipsFor(d, todayISO, a) {
			k := a + ":" + store.Str(t["key"])
			if !isDismissed(d, k) {
				out = append(out, with(t, Tip{"key": k, "source": a}))
			}
		}
	}
	return out
}

const header = "## From TOKEN DASHBOARD — draft harness recommendations"
const intro = "Generated from rule-based patterns in your own agent session history. Review before merging into ~/.claude/CLAUDE.md (or the matching agent's AGENTS.md) — nothing here is applied automatically."

// RecommendationsMarkdown is build_recommendations_markdown().
func RecommendationsMarkdown(d *store.DB, todayISO, source string) string {
	tips := All(d, todayISO, source)
	if len(tips) == 0 {
		return ""
	}
	lines := []string{header, "", intro, ""}
	for _, t := range tips {
		agent := ""
		if s := store.Str(t["source"]); s != "" {
			agent = fmt.Sprintf(" _(%s)_", s)
		}
		lines = append(lines, fmt.Sprintf("- **%s**%s", t["title"], agent), "  "+store.Str(t["body"]), "")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \n\t") + "\n"
}

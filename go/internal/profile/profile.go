// Package profile ports the local "Builder Profile" scoring engine.
package profile

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

const MinUserTurns = 10
const balanceThreshold = 10

var dimensions = []string{"steering", "execution", "engineering", "planning"}
var planningTools = []string{"Task", "Skill", "TodoWrite"}

var archetypes = map[string]string{
	"engineering|planning":  "The Architect",
	"engineering|steering":  "The Precision Editor",
	"execution|steering":    "The Sprinter",
	"execution|planning":    "The Operator",
	"engineering|execution": "The Craftsman",
	"planning|steering":     "The Strategist",
}

func clamp(value, worst, best float64) float64 {
	if worst == best {
		return 100
	}
	frac := (value - worst) / (best - worst)
	return math.Max(0, math.Min(1, frac)) * 100
}

func round(x float64) int64 { return int64(math.RoundToEven(x)) }

func steering(d *store.DB, since, until, source string) int64 {
	rng, args := store.RangeClause(since, until, source)
	pr := d.One(fmt.Sprintf(`
            SELECT COUNT(*) AS n,
                   SUM(CASE WHEN prompt_chars >= 40 THEN 1 ELSE 0 END) AS substantial
              FROM messages
             WHERE type='user' AND prompt_chars IS NOT NULL %s`, rng), args...)
	tr := d.One(fmt.Sprintf(`
            SELECT AVG(turns) AS avg_turns FROM (
                SELECT session_id, SUM(CASE WHEN type='user' THEN 1 ELSE 0 END) AS turns
                  FROM messages WHERE 1=1 %s
                 GROUP BY session_id
                HAVING turns > 0
            )`, rng), args...)
	n := store.Int(pr["n"])
	pct := 0.0
	if n != 0 {
		pct = float64(store.Int(pr["substantial"])) / float64(n)
	}
	return round((clamp(pct, 0, 0.8) + clamp(store.Float(tr["avg_turns"]), 30, 8)) / 2)
}

func execution(d *store.DB, since, until, source string) int64 {
	rng, args := store.RangeClause(since, until, source)
	n := store.Int(d.One(fmt.Sprintf(`SELECT COUNT(*) AS n FROM tool_calls WHERE tool_name != '_tool_result' %s`, rng), args...)["n"])
	errs := store.Int(d.One(fmt.Sprintf(`SELECT COUNT(*) AS n FROM tool_calls WHERE tool_name = '_tool_result' AND is_error = 1 %s`, rng), args...)["n"])
	div := store.Int(d.One(fmt.Sprintf(`SELECT COUNT(DISTINCT tool_name) AS n FROM tool_calls WHERE tool_name != '_tool_result' %s`, rng), args...)["n"])
	rate := 0.0
	if n != 0 {
		rate = float64(errs) / float64(n)
	}
	return round((clamp(rate, 0.10, 0) + clamp(float64(div), 1, 8)) / 2)
}

func engineering(d *store.DB, since, until, source string) int64 {
	rng, args := store.RangeClause(since, until, source)
	rt := d.One(fmt.Sprintf(`
            SELECT COUNT(*) AS files,
                   SUM(CASE WHEN touches > 2 THEN 1 ELSE 0 END) AS retouched
              FROM (
                SELECT session_id, target, COUNT(*) AS touches
                  FROM tool_calls
                 WHERE tool_name IN ('Read','Edit','Write') AND target IS NOT NULL %s
                 GROUP BY session_id, target
              )`, rng), args...)
	br := d.One(fmt.Sprintf(`
            SELECT AVG(files) AS avg_files FROM (
                SELECT session_id, COUNT(DISTINCT target) AS files
                  FROM tool_calls
                 WHERE tool_name IN ('Read','Edit','Write') AND target IS NOT NULL %s
                 GROUP BY session_id
            )`, rng), args...)
	total := store.Int(rt["files"])
	rate := 0.0
	if total != 0 {
		rate = float64(store.Int(rt["retouched"])) / float64(total)
	}
	return round((clamp(rate, 0.5, 0) + clamp(store.Float(br["avg_files"]), 1, 10)) / 2)
}

func planning(d *store.DB, since, until, source string) int64 {
	rng, args := store.RangeClause(since, until, source)
	total := store.Int(d.One(fmt.Sprintf(`SELECT COUNT(DISTINCT session_id) AS n FROM messages WHERE 1=1 %s`, rng), args...)["n"])
	qargs := []any{}
	for _, t := range planningTools {
		qargs = append(qargs, t)
	}
	qargs = append(qargs, args...)
	planned := store.Int(d.One(fmt.Sprintf(`SELECT COUNT(DISTINCT session_id) AS n FROM tool_calls WHERE tool_name IN (%s) %s`,
		store.Placeholders(len(planningTools)), rng), qargs...)["n"])
	pct := 0.0
	if total != 0 {
		pct = float64(planned) / float64(total)
	}
	return round(clamp(pct, 0, 0.5))
}

func archetype(scores map[string]int64) string {
	mx, mn := int64(math.MinInt64), int64(math.MaxInt64)
	for _, v := range scores {
		if v > mx {
			mx = v
		}
		if v < mn {
			mn = v
		}
	}
	if mx-mn <= balanceThreshold {
		return "The Generalist"
	}
	ranked := append([]string{}, dimensions...)
	sort.Slice(ranked, func(i, j int) bool {
		if scores[ranked[i]] != scores[ranked[j]] {
			return scores[ranked[i]] > scores[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})
	pair := []string{ranked[0], ranked[1]}
	sort.Strings(pair)
	if a, ok := archetypes[strings.Join(pair, "|")]; ok {
		return a
	}
	return "The Generalist"
}

func Build(d *store.DB, since, until, source string) store.Row {
	rng, args := store.RangeClause(since, until, source)
	turns := store.Int(d.One(fmt.Sprintf(`SELECT COUNT(*) AS n FROM messages WHERE type='user' %s`, rng), args...)["n"])
	if turns < MinUserTurns {
		return store.Row{"insufficient_data": true, "turns": turns, "minimum": int64(MinUserTurns)}
	}
	scores := map[string]int64{
		"steering": steering(d, since, until, source), "execution": execution(d, since, until, source),
		"engineering": engineering(d, since, until, source), "planning": planning(d, since, until, source),
	}
	sc := store.Row{}
	for k, v := range scores {
		sc[k] = v
	}
	return store.Row{"insufficient_data": false, "turns": turns, "scores": sc, "archetype": archetype(scores)}
}

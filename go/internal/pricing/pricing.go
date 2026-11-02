// Package pricing is a port of token_dashboard/pricing.py. The default table is
// pricing.json embedded in the binary; TOKEN_DASHBOARD_PRICING (or
// ~/.claude/token-dashboard-pricing.json) overrides it without a rebuild.
package pricing

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

type Rates struct {
	Tier          string  `json:"tier,omitempty"`
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreate5m float64 `json:"cache_create_5m"`
	CacheCreate1h float64 `json:"cache_create_1h"`
}

type Plan struct {
	Monthly float64 `json:"monthly"`
	Label   string  `json:"label"`
}

// Table keeps the raw JSON (served verbatim at /api/plan) and a typed view.
type Table struct {
	Raw          map[string]any
	Models       map[string]Rates
	TierFallback map[string]Rates
	Plans        map[string]Plan
}

func Parse(b []byte) (*Table, error) {
	t := &Table{}
	if err := json.Unmarshal(b, &t.Raw); err != nil {
		return nil, err
	}
	var typed struct {
		Models       map[string]Rates `json:"models"`
		TierFallback map[string]Rates `json:"tier_fallback"`
		Plans        map[string]Plan  `json:"plans"`
	}
	if err := json.Unmarshal(b, &typed); err != nil {
		return nil, err
	}
	t.Models, t.TierFallback, t.Plans = typed.Models, typed.TierFallback, typed.Plans
	return t, nil
}

// Load returns the override file if one exists, else the embedded default.
func Load(embedded []byte) (*Table, error) {
	cands := []string{os.Getenv("TOKEN_DASHBOARD_PRICING")}
	if h, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(h, ".claude", "token-dashboard-pricing.json"))
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if b, err := os.ReadFile(c); err == nil {
			if t, err := Parse(b); err == nil {
				return t, nil
			}
		}
	}
	return Parse(embedded)
}

type Usage struct {
	Input, Output, CacheRead, CacheCreate5m, CacheCreate1h int64
}

func UsageOf(r store.Row) Usage {
	return Usage{store.Int(r["input_tokens"]), store.Int(r["output_tokens"]), store.Int(r["cache_read_tokens"]),
		store.Int(r["cache_create_5m_tokens"]), store.Int(r["cache_create_1h_tokens"])}
}

// Cost is cost_for(): USD is nil when no tier matches.
type Cost struct {
	USD       *float64
	Estimated bool
}

func tierFromName(model string) string {
	m := strings.ToLower(model)
	for _, t := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(m, t) {
			return t
		}
	}
	return ""
}

// Round mimics Python's round(x, n) (correctly rounded decimal repr).
func Round(x float64, n int) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return f
}

func (t *Table) CostFor(model string, u Usage) Cost {
	rates, ok := t.Models[model]
	estimated := false
	if !ok {
		tier := tierFromName(model)
		fb, has := t.TierFallback[tier]
		if tier == "" || !has {
			return Cost{nil, true}
		}
		rates, estimated = fb, true
	}
	m := 1_000_000.0
	// Python 3.12+ sum() of floats is Neumaier-compensated; match it so the 6th decimal agrees.
	sum := neumaier(float64(u.Input)*rates.Input/m, float64(u.Output)*rates.Output/m,
		float64(u.CacheRead)*rates.CacheRead/m, float64(u.CacheCreate5m)*rates.CacheCreate5m/m,
		float64(u.CacheCreate1h)*rates.CacheCreate1h/m)
	v := Round(sum, 6)
	return Cost{&v, estimated}
}

// ---------------------------------------------------------------- plans

func planKey(source string) string {
	if source != "" {
		return "plan:" + source
	}
	return "plan"
}

// GetPlan is the agent's plan, else the global plan, else def.
func GetPlan(d *store.DB, def, source string) string {
	keys := []string{}
	if source != "" {
		keys = append(keys, planKey(source))
	}
	keys = append(keys, "plan")
	for _, k := range keys {
		if v, ok := d.GetKV(k); ok {
			return v
		}
	}
	return def
}

func AgentPlans(d *store.DB) map[string]any {
	out := map[string]any{}
	for _, r := range d.Rows("SELECT k, v FROM plan WHERE k LIKE 'plan:%'") {
		out[store.Str(r["k"])[5:]] = r["v"]
	}
	return out
}

func SetPlan(d *store.DB, plan, source string) { d.SetKV(planKey(source), plan) }

func neumaier(xs ...float64) float64 {
	var sum, c float64
	for _, x := range xs {
		t := sum + x
		if math.Abs(sum) >= math.Abs(x) {
			c += (sum - t) + x
		} else {
			c += (x - t) + sum
		}
		sum = t
	}
	if math.IsInf(c, 0) || math.IsNaN(c) {
		return sum
	}
	return sum + c
}

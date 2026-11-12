// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Live ReviewGuard metrics for Admin → Reviews: decisions by action, LLM share,
// tokens and cost per review, p95 latency per path against its SLO, overturn rate
// (how often people disagree with the engine) and live precision per rule from
// moderator decisions. Offline precision/recall per rule comes from the evals.

package reviews

import (
	"math"
	"os"
	"strconv"

	"asksauby/internal/model"
)

type Metrics struct {
	Reviews       int                           `json:"reviews"`
	ByStatus      map[string]int                `json:"by_status"`
	ByAction      map[string]int                `json:"by_action"`
	ByTagger      map[string]int                `json:"by_tagger"`
	LLMShare      float64                       `json:"llm_share"`
	Tokens        int                           `json:"tokens"`
	CostPerReview float64                       `json:"cost_per_review_usd"`
	CostPerM      float64                       `json:"cost_per_m_tokens_usd"`
	Latency       map[string]map[string]float64 `json:"latency_ms"`
	SLOMet        bool                          `json:"slo_met"`
	OpenCases     int                           `json:"open_cases"`
	OpenAppeals   int                           `json:"open_appeals"`
	CasesDecided  int                           `json:"cases_decided"`
	Dismissed     int                           `json:"cases_dismissed"`
	Appeals       int                           `json:"appeals_decided"`
	Overturned    int                           `json:"appeals_overturned"`
	OverturnRate  float64                       `json:"overturn_rate"` // (dismissed cases + overturned appeals) / decisions
	LivePrecision map[string]map[string]float64 `json:"live_precision"`
	Synthetic     int                           `json:"synthetic"`
}

var sloTarget = map[string]float64{"clear": 1000, "rules": 1000, "llm": 5000}

func (g *Guard) Metrics() Metrics {
	m := Metrics{ByStatus: map[string]int{}, ByAction: map[string]int{}, ByTagger: map[string]int{}, Latency: map[string]map[string]float64{},
		LivePrecision: map[string]map[string]float64{}, SLOMet: true}
	m.CostPerM, _ = strconv.ParseFloat(os.Getenv("LLM_COST_PER_M_TOKENS"), 64)
	if m.CostPerM == 0 {
		m.CostPerM = 1
	}
	lat := map[string][]float64{}
	byID := map[string]model.Review{}
	for _, r := range g.S.Reviews(nil) {
		byID[r.ID] = r
		m.Reviews++
		m.ByStatus[r.Status]++
		if r.Status == "pending" {
			continue
		}
		m.ByAction[r.Action]++
		m.ByTagger[r.Tagger]++
		m.Tokens += r.Tokens
		if r.Synthetic {
			m.Synthetic++
		}
		if r.Path != "" {
			lat[r.Path] = append(lat[r.Path], r.LatencyMS)
		}
	}
	decided := m.Reviews - m.ByStatus["pending"]
	if decided > 0 {
		m.LLMShare = round4(float64(m.ByTagger["llm"]) / float64(decided))
		m.CostPerReview = math.Round(float64(m.Tokens)/1e6*m.CostPerM/float64(decided)*1e6) / 1e6
	}
	for path, v := range lat {
		p95 := pctl(v, 95)
		m.Latency[path] = map[string]float64{"n": float64(len(v)), "p50": pctl(v, 50), "p95": p95, "target_p95": sloTarget[path]}
		m.SLOMet = m.SLOMet && p95 <= sloTarget[path]
	}
	tp, fp := map[string]int{}, map[string]int{}
	for _, c := range g.S.ReviewCases() {
		if c.Status == "open" {
			m.OpenCases++
			continue
		}
		if c.Kind == "report" || c.Kind == "burst" {
			continue
		}
		m.CasesDecided++
		if c.Decision == "dismiss" {
			m.Dismissed++
		}
		for _, rid := range c.ReviewIDs {
			for _, rule := range byID[rid].Rules {
				if c.Decision == "approve" {
					tp[rule]++
				} else {
					fp[rule]++
				}
			}
		}
	}
	for _, a := range g.S.Appeals() {
		switch a.Status {
		case "open":
			m.OpenAppeals++
		case "overturned":
			m.Appeals++
			m.Overturned++
			for _, rid := range a.ReviewIDs {
				for _, rule := range byID[rid].Rules {
					fp[rule]++
				}
			}
		default:
			m.Appeals++
		}
	}
	if n := m.CasesDecided + m.Appeals; n > 0 {
		m.OverturnRate = round4(float64(m.Dismissed+m.Overturned) / float64(n))
	}
	for rule := range unionKeys(tp, fp) {
		m.LivePrecision[rule] = map[string]float64{"precision": round4(ratioOr1(tp[rule], tp[rule]+fp[rule])), "decisions": float64(tp[rule] + fp[rule])}
	}
	return m
}

func unionKeys(a, b map[string]int) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

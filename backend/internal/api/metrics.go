// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Admin metrics: SLOs per path, availability, and the support KPIs (deflection,
// resolution, handoff, escalation, recontact, CSAT) per agent version.

package api

import (
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/model"
)

// SLO targets from the design: FAQ p95<1s p99<5s; order/refund p95<5s p99<30s; 99.99% availability.
var sloTargets = map[string][2]float64{"faq": {1000, 5000}, "transaction": {5000, 30000}}

const availabilityTarget = 0.9999

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return math.Round(s[i]*10) / 10
}

func rate(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(a/b*10000) / 10000
}

type sessAgg struct {
	cid, variant                           string
	start                                  time.Time
	handoff, escalated, resolved, up, down bool
	lat                                    []float64
}

// business turns per-session flags into the support KPIs:
//
//	handoff rate     sessions where the customer asked for a human
//	escalation rate  sessions a guardrail sent to a human (e.g. refund > limit)
//	deflection rate  sessions with no human involvement at all
//	resolution rate  sessions whose task got done (committed action or thumbs-up) and no thumbs-down/human
//	recontact rate   resolved/escalated sessions followed by another session from the same customer within 72h
//	CSAT             thumbs-up / all feedback
func business(sessions map[string]*sessAgg, fbUp, fbDown int) map[string]any {
	n := float64(len(sessions))
	var hand, esc, res, defl float64
	byCust := map[string][]*sessAgg{}
	for _, s := range sessions {
		if s.handoff {
			hand++
		}
		if s.escalated {
			esc++
		}
		if !s.handoff && !s.escalated {
			defl++
			if (s.resolved || s.up) && !s.down {
				res++
			}
		}
		byCust[s.cid] = append(byCust[s.cid], s)
	}
	closed, recontact := 0.0, 0.0
	for _, list := range byCust {
		sort.Slice(list, func(i, j int) bool { return list[i].start.Before(list[j].start) })
		for i, s := range list {
			if !(s.resolved || s.escalated || s.handoff) {
				continue
			}
			closed++
			if i+1 < len(list) && list[i+1].start.Sub(s.start) < 72*time.Hour {
				recontact++
			}
		}
	}
	return map[string]any{"sessions": len(sessions), "handoff_rate": rate(hand, n), "escalation_rate": rate(esc, n),
		"deflection_rate": rate(defl, n), "resolution_rate": rate(res, n), "recontact_rate": rate(recontact, closed),
		"csat": rate(float64(fbUp), float64(fbUp+fbDown)), "feedback": fbUp + fbDown,
		"feedback_response_rate": rate(float64(fbUp+fbDown), closed)}
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	var ev []model.Event
	for _, e := range s.store.Events(since) {
		if e.Source == "chat" {
			ev = append(ev, e)
		}
	}
	costPerM, _ := strconv.ParseFloat(os.Getenv("LLM_COST_PER_M_TOKENS"), 64)
	if costPerM == 0 {
		costPerM = 1
	}
	sessions := map[string]*sessAgg{}
	byPath, ttft := map[string][]float64{}, map[string][]float64{}
	tools, routers, models, guards := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	var all []float64
	served, failed5xx, limited, degraded, failover, inj, notOK, tokens, zero, searches := 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	perHour := map[string]int{}
	for _, e := range ev {
		ss := sessions[e.SessionID]
		if ss == nil {
			ss = &sessAgg{cid: e.CustomerID, variant: e.Variant, start: e.TS}
			sessions[e.SessionID] = ss
		}
		perHour[e.TS.Local().Format("15")]++
		if e.Status == 429 {
			limited++
			continue
		}
		if e.Status >= 500 {
			failed5xx++
		}
		served++
		ss.handoff, ss.escalated, ss.resolved = ss.handoff || e.Handoff, ss.escalated || e.Escalated, ss.resolved || e.Resolved
		ss.lat = append(ss.lat, e.LatencyMS)
		all = append(all, e.LatencyMS)
		byPath[e.Path] = append(byPath[e.Path], e.LatencyMS)
		ttft[e.Path] = append(ttft[e.Path], e.TTFTMS)
		tools[e.Tool]++
		routers[e.Router]++
		if e.Model != "" {
			models[e.Model]++
		}
		tokens += e.Tokens
		if !e.OK {
			notOK++
		}
		if e.Tool == "search_products" || e.Tool == "price_search" || e.Tool == "policy_search" {
			searches++
			if e.NResults == 0 {
				zero++
			}
		}
		for _, f := range e.Flags {
			switch {
			case f == "degraded":
				degraded++
			case f == "failover":
				failover++
			case f == "injection_suspect":
				inj++
			case strings.HasPrefix(f, "guardrail:"):
				guards[strings.TrimPrefix(f, "guardrail:")]++
			}
		}
	}
	fbUp, fbDown := 0, 0
	vfb := map[string][2]int{}
	for _, f := range s.store.Feedback(since) {
		if ss := sessions[f.SessionID]; ss != nil {
			ss.up, ss.down = ss.up || f.Helpful, ss.down || !f.Helpful
		}
		c := vfb[f.Variant]
		if f.Helpful {
			fbUp++
			c[0]++
		} else {
			fbDown++
			c[1]++
		}
		vfb[f.Variant] = c
	}
	// per agent version (control vs canary) - the numbers a rollout decision is made on
	variants := map[string]any{}
	vs := map[string]map[string]*sessAgg{}
	for id, ss := range sessions {
		v := ss.variant
		if v == "" {
			v = "control"
		}
		if vs[v] == nil {
			vs[v] = map[string]*sessAgg{}
		}
		vs[v][id] = ss
	}
	for v, m := range vs {
		var lat []float64
		for _, ss := range m {
			lat = append(lat, ss.lat...)
		}
		b := business(m, vfb[v][0], vfb[v][1])
		b["p95_ms"] = pct(lat, 95)
		variants[v] = b
	}
	slos := []map[string]any{}
	for _, p := range []string{"faq", "transaction"} {
		v := byPath[p]
		p95, p99 := pct(v, 95), pct(v, 99)
		t := sloTargets[p]
		slos = append(slos, map[string]any{"path": p, "n": len(v), "p50": pct(v, 50), "p95": p95, "p99": p99,
			"ttft_p95": pct(ttft[p], 95), "target_p95": t[0], "target_p99": t[1],
			"met": len(v) == 0 || (p95 <= t[0] && p99 <= t[1])})
	}
	avail := 1.0
	if served > 0 {
		avail = 1 - float64(failed5xx)/float64(served)
	}
	hoursOut := []map[string]any{}
	for i := min(hours, 24) - 1; i >= 0; i-- {
		h := time.Now().Add(-time.Duration(i) * time.Hour).Format("15")
		hoursOut = append(hoursOut, map[string]any{"hour": h + ":00", "n": perHour[h]})
	}
	recent := ev
	if len(recent) > 25 {
		recent = recent[len(recent)-25:]
	}
	rev := make([]model.Event, len(recent))
	for i := range recent {
		rev[i] = recent[len(recent)-1-i]
	}
	pending := 0
	for _, a := range s.store.Approvals() {
		if a.Status == "pending" {
			pending++
		}
	}
	writeJSON(w, 200, map[string]any{
		"window_hours": hours, "requests": len(ev), "served": served, "sessions": len(sessions),
		"latency": map[string]any{"p50": pct(all, 50), "p95": pct(all, 95), "p99": pct(all, 99)},
		"slos":    slos,
		"availability": map[string]any{"value": avail, "target": availabilityTarget, "failed": failed5xx,
			"budget_minutes_per_year": 52.6, "met": avail >= availabilityTarget},
		"rate_limited": limited, "tool_not_ok_rate": rate(float64(notOK), float64(served)),
		"zero_result_rate": rate(float64(zero), float64(searches)), "degraded": degraded, "failovers": failover,
		"injection_suspects": inj, "guardrails": guards, "pending_approvals": pending,
		"business": business(sessions, fbUp, fbDown), "variants": variants,
		"tokens": tokens, "est_cost_usd": math.Round(float64(tokens)/1e6*costPerM*100) / 100, "cost_per_m": costPerM,
		"tools": tools, "routers": routers, "models": models, "per_hour": hoursOut, "recent": rev,
		"tickets": len(s.store.Tickets()), "refunds": len(s.store.RefundsList()), "health": s.health(),
	})
}

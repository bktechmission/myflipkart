// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package evals scores Ask Sauby offline against the golden set.
//
//	retrieval   precision@k, recall@k, hit@k, MRR, nDCG@10 per retriever config and query type
//	router      tool-choice confusion matrix, per-tool precision/recall/F1, argument accuracy
//	e2e         full turns on a fresh store: tool, outcome, confirm-before-commit, authZ (no
//	            cross-customer leaks), groundedness, hit@k, latency vs SLO, plus consistency
//	            checks (refund idempotency, 20 concurrent refunds -> exactly one)
package evals

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"asksauby/internal/agent"
	"asksauby/internal/config"
	"asksauby/internal/llm"
	"asksauby/internal/search"
	"asksauby/internal/seed"
	"asksauby/internal/store"
)

var AllSuites = []string{"retrieval", "router", "e2e"}

const K = 5

type Options struct {
	Cfg       config.Config
	Suites    []string
	Router    string // "" = cfg.Router
	Label     string
	Retriever *search.Retriever
	GW        *llm.Gateway
	Log       func(string)
}

type Result struct {
	RunID          string                   `json:"run_id"`
	Label          string                   `json:"label"`
	K              int                      `json:"k"`
	Config         map[string]any           `json:"config"`
	NGolden        int                      `json:"n_golden"`
	Retrieval      map[string]*RetrievalCfg `json:"retrieval,omitempty"`
	RetrievalOrder []string                 `json:"retrieval_order,omitempty"`
	Router         *RouterResult            `json:"router,omitempty"`
	E2E            *E2EResult               `json:"e2e,omitempty"`
	Gate           *GateResult              `json:"gate,omitempty"`
	DurationS      float64                  `json:"duration_s"`
}

// ---------------- retrieval ----------------

type QueryScore struct {
	Query     string             `json:"query"`
	Type      string             `json:"type"`
	Relevant  []string           `json:"relevant"`
	Retrieved []string           `json:"retrieved"`
	M         map[string]float64 `json:"m"`
}

type RetrievalCfg struct {
	Overall  map[string]float64            `json:"overall"`
	ByType   map[string]map[string]float64 `json:"by_type"`
	PerQuery []QueryScore                  `json:"per_query"`
}

var metricKeys = []string{"precision@5", "recall@5", "hit@5", "mrr", "ndcg@10"}

func rankingMetrics(ranked, relevant []string) map[string]float64 {
	rel := map[string]bool{}
	for _, r := range relevant {
		rel[r] = true
	}
	hits := 0
	for i, d := range ranked {
		if i < K && rel[d] {
			hits++
		}
	}
	rr := 0.0
	for i, d := range ranked {
		if rel[d] {
			rr = 1 / float64(i+1)
			break
		}
	}
	dcg, idcg := 0.0, 0.0
	for i, d := range ranked {
		if i < 10 && rel[d] {
			dcg += 1 / math.Log2(float64(i+2))
		}
	}
	for i := 0; i < len(relevant) && i < 10; i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}
	hit := 0.0
	if hits > 0 {
		hit = 1
	}
	return map[string]float64{"precision@5": float64(hits) / K, "recall@5": float64(hits) / float64(len(relevant)),
		"hit@5": hit, "mrr": rr, "ndcg@10": dcg / math.Max(idcg, 1e-9)}
}

func mean(rows []map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, k := range metricKeys {
		s := 0.0
		for _, r := range rows {
			s += r[k]
		}
		out[k] = round4(s / math.Max(1, float64(len(rows))))
	}
	return out
}

func runRetrieval(r *search.Retriever, golden []seed.GoldenRow) (map[string]*RetrievalCfg, []string) {
	type cfgT struct {
		name, mode     string
		filter, rerank bool
	}
	cfgs := []cfgT{{"bm25", "bm25", false, false}, {"vector", "vector", false, false}, {"hybrid", "hybrid", false, false},
		{"hybrid+filter", "hybrid", true, false}, {"hybrid+filter+rerank", "hybrid", true, true}}
	out := map[string]*RetrievalCfg{}
	var order []string
	for _, c := range cfgs {
		rc := &RetrievalCfg{ByType: map[string]map[string]float64{}}
		byType := map[string][]map[string]float64{}
		var all []map[string]float64
		for _, g := range golden {
			if g.Index == "" {
				continue
			}
			var ranked []string
			if g.Index == "products" {
				f, rr := c.filter, c.rerank
				q := search.ProductQuery{Query: g.Query, K: 20, Mode: c.mode, CategoryFilter: &f, Rerank: &rr}
				if v, ok := g.Args["min_price"].(float64); ok {
					q.MinPrice = &v
				}
				if v, ok := g.Args["max_price"].(float64); ok {
					q.MaxPrice = &v
				}
				hits, _, _ := r.SearchProducts(q)
				for _, h := range hits {
					ranked = append(ranked, h.ID)
				}
			} else {
				hits, _, _ := r.SearchPolicies(g.Query, 20, c.mode)
				seen := map[string]bool{}
				for _, h := range hits { // chunks -> documents
					d := r.Chunk(h.ID).Doc
					if !seen[d] {
						seen[d] = true
						ranked = append(ranked, d)
					}
				}
			}
			m := rankingMetrics(ranked, g.Relevant)
			top := ranked
			if len(top) > K {
				top = top[:K]
			}
			rc.PerQuery = append(rc.PerQuery, QueryScore{g.Query, g.Type, g.Relevant, top, m})
			byType[g.Type] = append(byType[g.Type], m)
			all = append(all, m)
		}
		rc.Overall = mean(all)
		for t, rows := range byType {
			rc.ByType[t] = mean(rows)
		}
		out[c.name] = rc
		order = append(order, c.name)
	}
	return out, order
}

// ---------------- router ----------------

type ClassScore struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`
}

type RouterResult struct {
	Router      string                `json:"router"`
	Accuracy    float64               `json:"accuracy"`
	MacroF1     float64               `json:"macro_f1"`
	ArgAccuracy float64               `json:"arg_accuracy"`
	Labels      []string              `json:"labels"`
	Matrix      [][]int               `json:"matrix"`
	PerClass    map[string]ClassScore `json:"per_class"`
	Mistakes    []map[string]any      `json:"mistakes"`
	LatencyMS   map[string]float64    `json:"latency_ms"`
}

func pickRouter(mode string, gw *llm.Gateway) agent.Router {
	if mode == "rules" || gw == nil || (mode != "llm" && !gw.Available()) {
		return agent.RuleRouter{}
	}
	return agent.LLMRouter{GW: gw}
}

func runRouter(rt agent.Router, golden []seed.GoldenRow) *RouterResult {
	cm := map[string]map[string]int{}
	for _, t := range agent.ToolNames {
		cm[t] = map[string]int{}
	}
	res := &RouterResult{Router: rt.Name(), PerClass: map[string]ClassScore{}, Mistakes: []map[string]any{}}
	var lat []float64
	argOK, argN := 0, 0
	for _, g := range golden {
		t0 := time.Now()
		r := rt.Route(context.Background(), g.Query, nil)
		lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
		pred := r.Tool
		if cm[pred] == nil {
			pred = "chitchat"
		}
		cm[g.Tool][pred]++
		if pred != g.Tool {
			res.Mistakes = append(res.Mistakes, map[string]any{"query": g.Query, "expected": g.Tool, "predicted": pred, "args": r.Args})
			continue
		}
		if len(g.Args) > 0 {
			argN++
			ok := true
			for k, want := range g.Args {
				if !argEq(r.Args[k], want) {
					ok = false
				}
			}
			if ok {
				argOK++
			} else {
				res.Mistakes = append(res.Mistakes, map[string]any{"query": g.Query, "expected": fmt.Sprint(g.Tool, " ", g.Args),
					"predicted": fmt.Sprint(pred, " ", r.Args), "args": r.Args})
			}
		}
	}
	correct, total := 0, 0
	f1s := []float64{}
	for _, t := range agent.ToolNames {
		tp, fp, fn := cm[t][t], 0, 0
		for _, o := range agent.ToolNames {
			if o != t {
				fp += cm[o][t]
				fn += cm[t][o]
			}
			total += cm[t][o]
		}
		correct += tp
		if tp+fn == 0 && fp == 0 {
			continue
		}
		p, rc := ratio(tp, tp+fp), ratio(tp, tp+fn)
		f1 := 0.0
		if p+rc > 0 {
			f1 = 2 * p * rc / (p + rc)
		}
		res.PerClass[t] = ClassScore{round4(p), round4(rc), round4(f1), tp + fn}
		res.Labels = append(res.Labels, t)
		f1s = append(f1s, f1)
	}
	for _, a := range res.Labels {
		row := []int{}
		for _, b := range res.Labels {
			row = append(row, cm[a][b])
		}
		res.Matrix = append(res.Matrix, row)
	}
	res.Accuracy = round4(ratio(correct, total))
	s := 0.0
	for _, f := range f1s {
		s += f
	}
	res.MacroF1 = round4(s / math.Max(1, float64(len(f1s))))
	res.ArgAccuracy = round4(ratio(argOK, argN))
	res.LatencyMS = map[string]float64{"p50": pctl(lat, 50), "p95": pctl(lat, 95)}
	return res
}

// ---------------- end to end ----------------

type E2ERow struct {
	Query        string          `json:"query"`
	Type         string          `json:"type"`
	As           string          `json:"as"`
	Tool         string          `json:"tool"`
	ExpectedTool string          `json:"expected_tool"`
	Answer       string          `json:"answer"`
	LatencyMS    float64         `json:"latency_ms"`
	Path         string          `json:"path"`
	Checks       map[string]bool `json:"checks"`
	Pass         bool            `json:"pass"`
}

type E2EResult struct {
	Router      string             `json:"router"`
	Rates       map[string]float64 `json:"rates"`
	SLO         []map[string]any   `json:"slo"`
	Consistency map[string]any     `json:"consistency"`
	Failures    []E2ERow           `json:"failures"`
	Rows        []E2ERow           `json:"rows"`
}

func runE2E(o Options, golden []seed.GoldenRow) (*E2EResult, error) {
	st, err := store.Open("", "") // fresh, memory-only: cancels/refunds are repeatable
	if err != nil {
		return nil, err
	}
	tools := &agent.Tools{S: st, R: o.Retriever, ReturnWindow: o.Cfg.ReturnWindowDays,
		AutoRefundLimit: o.Cfg.AutoRefundLimit, MaxAutoRefunds: o.Cfg.MaxAutoRefunds}
	mode := o.Router
	if mode == "" {
		mode = o.Cfg.Router
	}
	ag := agent.New(tools, st, o.GW, mode, "eval")
	res := &E2EResult{Router: ag.RouterName(), Rates: map[string]float64{}}
	byPath := map[string][]float64{}
	for i, g := range golden {
		cid := g.As
		if cid == "" {
			cid = "C1001"
		}
		sid := fmt.Sprintf("%016x", i+1)
		first := ag.Handle(context.Background(), cid, sid, g.Query, nil)
		final := first
		if first.NeedsConfirm {
			final = ag.Handle(context.Background(), cid, sid, "yes", nil)
		}
		lat := first.Trace.LatencyMS
		if final.Trace.LatencyMS > lat {
			lat = final.Trace.LatencyMS
		}
		byPath[first.Trace.Path] = append(byPath[first.Trace.Path], lat)
		allowed := map[string]bool{}
		for _, p := range final.Result.Products {
			allowed[p.ID] = true
		}
		if final.Result.Order != nil {
			allowed[final.Result.Order.ID] = true
			for _, it := range final.Result.Order.Items {
				allowed[it] = true
			}
		}
		if id, _ := g.Args["order_id"].(string); id != "" {
			allowed[id] = true
		}
		grounded := true
		for _, id := range append(first.CitedIDs, final.CitedIDs...) {
			grounded = grounded && allowed[id]
		}
		c := map[string]bool{"tool_ok": first.Tool == g.Tool, "grounded": grounded}
		if g.ExpectOK != nil {
			c["outcome_ok"] = final.OK == *g.ExpectOK
			if agent.ConfirmTools[g.Tool] && *g.ExpectOK {
				// money path must ask first, and only "yes" commits (or escalates)
				c["confirm_before_commit"] = first.NeedsConfirm && !first.Result.Committed &&
					(final.Result.Committed || final.Result.Escalated)
			}
			if g.Tool == "request_refund" && *g.ExpectOK { // guardrail: big refunds must go to a human
				c["escalation_ok"] = final.Result.Escalated == g.Escalate && final.Result.Committed == !g.Escalate
			}
			if final.AskFeedback == "" && (final.Result.Committed || final.Result.Escalated) {
				c["asks_feedback"] = false
			} else if final.Result.Committed || final.Result.Escalated {
				c["asks_feedback"] = true
			}
			if g.Tool == "handoff" { // a live ticket with the conversation's cid+sid must be queued
				c["handoff_queued"] = final.Result.Ticket != nil && final.Result.Ticket.Kind == "live" &&
					final.Result.Ticket.SessionID == sid && final.Result.Ticket.CustomerID == cid
			}
		}
		if g.Type == "authz" { // nothing about another customer's order may come back
			c["no_leak"] = final.Result.Order == nil && !final.Result.Committed && !strings.Contains(final.Answer, "$")
		}
		if len(g.Relevant) > 0 {
			var got []string
			for _, p := range final.Result.Products {
				got = append(got, p.ID)
			}
			for _, ch := range final.Result.Chunks {
				got = append(got, ch.Doc)
			}
			hit := false
			for j, x := range got {
				if j < K && contains(g.Relevant, x) {
					hit = true
				}
			}
			c["hit@5"] = hit
		}
		pass := true
		for _, v := range c {
			pass = pass && v
		}
		row := E2ERow{g.Query, g.Type, cid, first.Tool, g.Tool, final.Answer, lat, first.Trace.Path, c, pass}
		res.Rows = append(res.Rows, row)
		if !pass {
			res.Failures = append(res.Failures, row)
		}
	}
	for _, k := range []string{"tool_ok", "outcome_ok", "grounded", "hit@5", "confirm_before_commit", "escalation_ok", "asks_feedback", "handoff_queued", "no_leak"} {
		n, ok := 0, 0
		for _, r := range res.Rows {
			if v, has := r.Checks[k]; has {
				n++
				if v {
					ok++
				}
			}
		}
		res.Rates[k] = round4(ratio(ok, n))
	}
	pass := 0
	for _, r := range res.Rows {
		if r.Pass {
			pass++
		}
	}
	res.Rates["pass"] = round4(ratio(pass, len(res.Rows)))
	targets := map[string][2]float64{"faq": {1000, 5000}, "transaction": {5000, 30000}}
	for _, p := range []string{"faq", "transaction"} {
		v := byPath[p]
		p95, p99 := pctl(v, 95), pctl(v, 99)
		res.SLO = append(res.SLO, map[string]any{"path": p, "n": len(v), "p95": p95, "p99": p99,
			"target_p95": targets[p][0], "target_p99": targets[p][1], "met": p95 <= targets[p][0] && p99 <= targets[p][1]})
	}
	res.Consistency = consistency(o)
	return res, nil
}

// consistency proves the strong-consistency rules on the refund path.
func consistency(o Options) map[string]any {
	st, _ := store.Open("", "")
	auto := &agent.Tools{S: st, R: o.Retriever, ReturnWindow: o.Cfg.ReturnWindowDays} // no amount limit: test raw money movement
	// 1) idempotency: refunding twice returns the same refund
	a := auto.Run("request_refund", map[string]any{"order_id": "O5008"}, "C1001", "x", true)
	b := auto.Run("request_refund", map[string]any{"order_id": "O5008"}, "C1001", "x", true)
	idem := a.Refund != nil && b.Refund != nil && a.Refund.ID == b.Refund.ID && !b.Committed
	// 2) race: 20 concurrent refund commits on one order -> exactly one money movement
	var wg sync.WaitGroup
	var mu sync.Mutex
	commits := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := auto.Run("request_refund", map[string]any{"order_id": "O5005"}, "C1001", "x", true)
			if r.Committed {
				mu.Lock()
				commits++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	n := 0
	for _, r := range st.RefundsList() {
		if r.OrderID == "O5005" {
			n++
		}
	}
	// 3) cross-customer refund through the tool layer directly (bypassing the router)
	x := auto.Run("request_refund", map[string]any{"order_id": "O5101"}, "C1001", "x", true)
	// 4) guardrail: a $299 refund with the $30 limit is held, and asking again doesn't create a second approval
	st2, _ := store.Open("", "")
	guarded := &agent.Tools{S: st2, R: o.Retriever, ReturnWindow: o.Cfg.ReturnWindowDays, AutoRefundLimit: 30, MaxAutoRefunds: 3}
	g1 := guarded.Run("request_refund", map[string]any{"order_id": "O5105"}, "C1002", "x", true)
	g2 := guarded.Run("request_refund", map[string]any{"order_id": "O5105"}, "C1002", "x", true)
	held := g1.Escalated && !g1.Committed && g2.Escalated && len(st2.Approvals()) == 1 && len(st2.RefundsList()) == 0
	return map[string]any{"idempotent_refund": idem, "concurrent_commits": commits, "refund_records": n,
		"exactly_once": commits == 1 && n == 1, "cross_customer_blocked": !x.OK && !x.Committed,
		"large_refund_held_once": held, "pass": idem && commits == 1 && n == 1 && !x.OK && held}
}

// ---------------- orchestration & persistence ----------------

func Run(o Options) (*Result, error) {
	if o.Log == nil {
		o.Log = func(string) {}
	}
	t0 := time.Now()
	golden := append(seed.Golden(seed.Products()), Mined(o.Cfg.DataDir)...)
	mode := o.Router
	if mode == "" {
		mode = o.Cfg.Router
	}
	rt := pickRouter(mode, o.GW)
	res := &Result{RunID: time.Now().Format("20060102-150405"), Label: o.Label, K: K, NGolden: len(golden),
		Config: map[string]any{"router": rt.Name(), "embeddings": o.Retriever.EmbedLabel, "retriever": o.Cfg.Retriever,
			"rrf_k": o.Cfg.RRFK, "bm25_weight": o.Cfg.BM25Weight, "vector_weight": o.Cfg.VectorWeight,
			"top_k": o.Cfg.TopK, "policy_k": o.Cfg.PolicyK, "category_filter": o.Cfg.CategoryFilter}}
	if rt.Name() == "llm" && o.GW.Internal != nil {
		res.Config["llm"] = o.GW.Internal.Model
	}
	for _, s := range o.Suites {
		o.Log("running " + s + " suite…")
		switch s {
		case "retrieval":
			res.Retrieval, res.RetrievalOrder = runRetrieval(o.Retriever, golden)
		case "router":
			res.Router = runRouter(rt, golden)
		case "e2e":
			e, err := runE2E(o, golden)
			if err != nil {
				return nil, err
			}
			res.E2E = e
		}
	}
	res.Gate = CheckGates(o.Cfg.EvalGates, res)
	res.DurationS = math.Round(time.Since(t0).Seconds()*10) / 10
	return res, save(o.Cfg.DataDir, res)
}

func dir(dataDir string) string { return filepath.Join(dataDir, "evals") }

func save(dataDir string, r *Result) error {
	d := dir(dataDir)
	os.MkdirAll(d, 0o700)
	b, _ := json.MarshalIndent(r, "", " ")
	os.WriteFile(filepath.Join(d, "run_"+r.RunID+".json"), b, 0o600)
	latest := *r
	if prev := Latest(dataDir); prev != nil { // keep other suites' last results on the dashboard
		if latest.Retrieval == nil {
			latest.Retrieval, latest.RetrievalOrder = prev.Retrieval, prev.RetrievalOrder
		}
		if latest.Router == nil {
			latest.Router = prev.Router
		}
		if latest.E2E == nil {
			latest.E2E = prev.E2E
		}
	}
	lb, _ := json.MarshalIndent(latest, "", " ")
	if err := os.WriteFile(filepath.Join(d, "latest.json"), lb, 0o600); err != nil {
		return err
	}
	sum := map[string]any{"run_id": r.RunID, "label": r.Label, "config": r.Config}
	if r.Retrieval != nil {
		ov := map[string]any{}
		for n, v := range r.Retrieval {
			ov[n] = v.Overall
		}
		sum["retrieval"] = ov
	}
	if r.Router != nil {
		sum["router"] = map[string]float64{"accuracy": r.Router.Accuracy, "macro_f1": r.Router.MacroF1, "arg_accuracy": r.Router.ArgAccuracy}
	}
	if r.E2E != nil {
		sum["e2e"] = r.E2E.Rates
	}
	if r.Gate != nil {
		sum["gate_pass"] = r.Gate.Pass
	}
	f, err := os.OpenFile(filepath.Join(d, "history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(sum)
	_, err = f.Write(append(line, '\n'))
	return err
}

func Latest(dataDir string) *Result {
	b, err := os.ReadFile(filepath.Join(dir(dataDir), "latest.json"))
	if err != nil {
		return nil
	}
	var r Result
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func History(dataDir string) []map[string]any {
	out := []map[string]any{}
	f, err := os.Open(filepath.Join(dir(dataDir), "history.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// ---------------- helpers ----------------

func argEq(got, want any) bool {
	if want == nil {
		return got == nil || got == "" || got == 0.0
	}
	if w, ok := want.(float64); ok {
		g, ok := got.(float64)
		return ok && math.Abs(g-w) < 0.01
	}
	return strings.EqualFold(fmt.Sprint(got), fmt.Sprint(want))
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

func pctl(v []float64, p float64) float64 {
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

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

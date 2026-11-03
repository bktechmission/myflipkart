// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Command evals runs the offline eval suites and prints a report.
//
//	go run ./cmd/evals                         # all suites
//	go run ./cmd/evals -suite retrieval
//	go run ./cmd/evals -router llm -label "qwen2.5:7b"
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"asksauby/internal/config"
	"asksauby/internal/evals"
	"asksauby/internal/llm"
	"asksauby/internal/search"
	"asksauby/internal/seed"
)

func main() {
	suite := flag.String("suite", "all", "all | retrieval | router | e2e")
	router := flag.String("router", "", "rules | llm | auto (default: ROUTER env)")
	label := flag.String("label", "cli", "label shown in run history")
	gate := flag.Bool("gate", false, "exit non-zero if any release gate fails (use in CI before rollout)")
	flag.Parse()
	cfg := config.Load()
	r, err := search.NewRetriever(cfg, seed.Products(), seed.Policies)
	if err != nil {
		log.Fatal(err)
	}
	gw := &llm.Gateway{Internal: llm.NewProvider("internal", cfg.InternalLLMURL, cfg.InternalLLMModel, cfg.InternalLLMKey, 60*time.Second)}
	if cfg.ExternalLLMURL != "" {
		gw.External = llm.NewProvider("external", cfg.ExternalLLMURL, cfg.ExternalLLMModel, cfg.ExternalLLMKey, 30*time.Second)
		gw.External.Probe()
	}
	gw.Internal.Probe()
	suites := evals.AllSuites
	if *suite != "all" {
		suites = []string{*suite}
	}
	res, err := evals.Run(evals.Options{Cfg: cfg, Suites: suites, Router: *router, Label: *label, Retriever: r, GW: gw,
		Log: func(m string) { fmt.Println(m) }})
	if err != nil {
		log.Fatal(err)
	}
	report(res)
	if res.Gate != nil {
		fmt.Printf("\nRELEASE GATES (%s): pass=%v\n", cfg.EvalGates, res.Gate.Pass)
		for _, c := range res.Gate.Checks {
			mark := "ok  "
			if !c.Pass {
				mark = "FAIL"
			}
			fmt.Printf("  %s %-38s %.3f >= %.3f %s\n", mark, c.Metric, c.Value, c.Min, c.Note)
		}
		if *gate && !res.Gate.Pass {
			os.Exit(1)
		}
	}
}

func report(r *evals.Result) {
	fmt.Printf("\n=== Ask Sauby eval %s  [%s] ===\nconfig: %v\n", r.RunID, r.Label, r.Config)
	if r.Retrieval != nil {
		cols := []string{"precision@5", "recall@5", "hit@5", "mrr", "ndcg@10"}
		fmt.Printf("\nRETRIEVAL\n%-15s", "config")
		for _, c := range cols {
			fmt.Printf("%13s", c)
		}
		fmt.Println()
		for _, n := range r.RetrievalOrder {
			fmt.Printf("%-15s", n)
			for _, c := range cols {
				fmt.Printf("%13.3f", r.Retrieval[n].Overall[c])
			}
			fmt.Println()
		}
		var types []string
		for t := range r.Retrieval["hybrid"].ByType {
			types = append(types, t)
		}
		sort.Strings(types)
		fmt.Printf("\nRECALL@5 BY TYPE\n%-15s", "config")
		for _, t := range types {
			fmt.Printf("%11s", t)
		}
		fmt.Println()
		for _, n := range r.RetrievalOrder {
			fmt.Printf("%-15s", n)
			for _, t := range types {
				fmt.Printf("%11.3f", r.Retrieval[n].ByType[t]["recall@5"])
			}
			fmt.Println()
		}
		fmt.Println("\nmisses (hybrid+filter+rerank):")
		for _, q := range r.Retrieval["hybrid+filter+rerank"].PerQuery {
			if q.M["hit@5"] == 0 {
				fmt.Printf("  - [%s] %q want %v got %v\n", q.Type, q.Query, q.Relevant, q.Retrieved)
			}
		}
	}
	if rt := r.Router; rt != nil {
		fmt.Printf("\nROUTER (%s) accuracy=%.3f macro_f1=%.3f arg_accuracy=%.3f p95=%.1fms\n", rt.Router, rt.Accuracy, rt.MacroF1, rt.ArgAccuracy, rt.LatencyMS["p95"])
		fmt.Printf("%-16s", "expected\\pred")
		for _, l := range rt.Labels {
			fmt.Printf("%9s", trunc(l, 8))
		}
		fmt.Println()
		for i, row := range rt.Matrix {
			fmt.Printf("%-16s", rt.Labels[i])
			for _, v := range row {
				fmt.Printf("%9d", v)
			}
			fmt.Println()
		}
		fmt.Printf("\n%-16s%10s%10s%10s%9s\n", "tool", "precision", "recall", "f1", "support")
		for _, l := range rt.Labels {
			c := rt.PerClass[l]
			fmt.Printf("%-16s%10.3f%10.3f%10.3f%9d\n", l, c.Precision, c.Recall, c.F1, c.Support)
		}
		for _, m := range rt.Mistakes {
			fmt.Printf("  x %q expected %v got %v\n", m["query"], m["expected"], m["predicted"])
		}
	}
	if e := r.E2E; e != nil {
		fmt.Printf("\nE2E (%s)", e.Router)
		keys := []string{"pass", "tool_ok", "outcome_ok", "confirm_before_commit", "escalation_ok", "asks_feedback", "handoff_queued", "no_leak", "grounded", "hit@5"}
		for _, k := range keys {
			fmt.Printf("  %s=%.3f", k, e.Rates[k])
		}
		fmt.Println()
		for _, s := range e.SLO {
			fmt.Printf("  SLO %-11s n=%-3v p95=%vms (target %v)  p99=%vms (target %v)  met=%v\n", s["path"], s["n"], s["p95"], s["target_p95"], s["p99"], s["target_p99"], s["met"])
		}
		fmt.Printf("  consistency: %v\n", e.Consistency)
		for _, f := range e.Failures {
			var bad []string
			for k, v := range f.Checks {
				if !v {
					bad = append(bad, k)
				}
			}
			fmt.Printf("  x [%s as %s] %q failed %v -> %q\n", f.Type, f.As, f.Query, bad, trunc(f.Answer, 90))
		}
	}
	fmt.Printf("\nsaved data/evals/latest.json (%.1fs)\n", r.DurationS)
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}

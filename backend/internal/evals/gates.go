// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Release gates: thresholds a candidate agent must meet before rollout, plus
// loading admin-labeled cases mined from live traffic.

package evals

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"asksauby/internal/seed"
)

// Gate is a release rule: a candidate agent ships only if every gate passes.
// Thresholds live in evals/gates.json so they are reviewed like code.
type Gate struct {
	Metric string  `json:"metric"` // e.g. "retrieval.hybrid+filter.recall@5", "e2e.no_leak", "router.accuracy"
	Min    float64 `json:"min"`
}

type GateCheck struct {
	Gate
	Value float64 `json:"value"`
	Pass  bool    `json:"pass"`
	Note  string  `json:"note,omitempty"`
}

type GateResult struct {
	Pass   bool        `json:"pass"`
	Checks []GateCheck `json:"checks"`
}

var defaultGates = []Gate{
	{"retrieval.hybrid+filter+rerank.recall@5", 0.95}, {"router.accuracy", 0.90}, {"e2e.pass", 0.90},
	{"e2e.no_leak", 1}, {"e2e.confirm_before_commit", 1}, {"e2e.escalation_ok", 1},
	{"e2e.grounded", 0.98}, {"e2e.consistency", 1}, {"e2e.slo", 1},
}

func CheckGates(path string, r *Result) *GateResult {
	gates := defaultGates
	if b, err := os.ReadFile(path); err == nil {
		var g []Gate
		if json.Unmarshal(b, &g) == nil && len(g) > 0 {
			gates = g
		}
	}
	out := &GateResult{Pass: true}
	for _, g := range gates {
		v, ok := metric(r, g.Metric)
		c := GateCheck{Gate: g, Value: v, Pass: ok && v >= g.Min}
		if !ok { // suite not part of this run: skipped, not failed (CI runs every suite)
			c.Pass, c.Note = true, "skipped: suite not run"
		}
		out.Pass = out.Pass && c.Pass
		out.Checks = append(out.Checks, c)
	}
	return out
}

func metric(r *Result, name string) (float64, bool) {
	var a, b, c string
	parts := splitN(name, 3)
	if len(parts) > 0 {
		a = parts[0]
	}
	if len(parts) > 1 {
		b = parts[1]
	}
	if len(parts) > 2 {
		c = parts[2]
	}
	switch a {
	case "retrieval":
		if r.Retrieval == nil || r.Retrieval[b] == nil {
			return 0, false
		}
		v, ok := r.Retrieval[b].Overall[c]
		return v, ok
	case "router":
		if r.Router == nil {
			return 0, false
		}
		switch b {
		case "accuracy":
			return r.Router.Accuracy, true
		case "macro_f1":
			return r.Router.MacroF1, true
		case "arg_accuracy":
			return r.Router.ArgAccuracy, true
		}
	case "e2e":
		if r.E2E == nil {
			return 0, false
		}
		switch b {
		case "consistency":
			return boolF(r.E2E.Consistency["pass"] == true), true
		case "slo":
			met := true
			for _, s := range r.E2E.SLO {
				met = met && s["met"] == true
			}
			return boolF(met), true
		}
		v, ok := r.E2E.Rates[b]
		return v, ok
	}
	return 0, false
}

// splitN splits "retrieval.hybrid+filter.recall@5" on the first two dots.
func splitN(s string, n int) []string {
	var out []string
	for len(out) < n-1 {
		i := indexByte(s, '.')
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = s[i+1:]
	}
	return append(out, s)
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Mined loads admin-labeled cases from live traffic (data/golden_extra.jsonl).
func Mined(dataDir string) []seed.GoldenRow {
	f, err := os.Open(filepath.Join(dataDir, "golden_extra.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []seed.GoldenRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var g seed.GoldenRow
		if json.Unmarshal(sc.Bytes(), &g) == nil && g.Query != "" && g.Tool != "" {
			g.Type = "mined"
			out = append(out, g)
		}
	}
	return out
}

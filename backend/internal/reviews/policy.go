// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package reviews is ReviewGuard: real reviews on MyFlipKart.com, guarded end to end.
//
//	submit (WAL first) -> normalize -> content rules -> authenticity -> tagger (rules or LLM)
//	-> verifier -> decision engine -> action -> moderator queue -> appeals -> audit
//
// The policy library (policy/policy.json) is versioned data: rule ids, definitions,
// examples and counter-examples. The LLM prompt contains only the clauses that match
// a review, and every label must name a rule id from the current version.
package reviews

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed policy/policy.json
var policyJSON []byte

type Rule struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`   // content | authenticity
	Action          string   `json:"action"` // mask | reject | hold | not_counted | note
	Severities      []string `json:"severities"`
	DefaultSeverity string   `json:"default_severity"`
	Triggers        []string `json:"triggers,omitempty"` // content signal names that match this clause
	Definition      string   `json:"definition"`
	Examples        []string `json:"examples"`
	CounterExamples []string `json:"counter_examples"`
}

type Policy struct {
	Version    string   `json:"version"`
	Title      string   `json:"title"`
	Note       string   `json:"note"`
	Principles []string `json:"principles"`
	Rules      []Rule   `json:"rules"`
	byID       map[string]Rule
	byName     map[string]Rule
}

// LoadPolicy parses and validates a policy library.
func LoadPolicy(b []byte) (*Policy, error) {
	p := &Policy{}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if p.Version == "" || len(p.Rules) == 0 {
		return nil, fmt.Errorf("policy: version and rules required")
	}
	p.byID, p.byName = map[string]Rule{}, map[string]Rule{}
	for _, r := range p.Rules {
		if _, dup := p.byID[r.ID]; dup || r.ID == "" || r.Name == "" {
			return nil, fmt.Errorf("policy: bad or duplicate rule %q", r.ID)
		}
		if !contains(r.Severities, r.DefaultSeverity) {
			return nil, fmt.Errorf("policy: %s default severity %q not allowed", r.ID, r.DefaultSeverity)
		}
		for _, s := range r.Severities {
			if !contains(Severities, s) {
				return nil, fmt.Errorf("policy: %s unknown severity %q", r.ID, s)
			}
		}
		if r.Kind != "content" && r.Kind != "authenticity" {
			return nil, fmt.Errorf("policy: %s unknown kind %q", r.ID, r.Kind)
		}
		p.byID[r.ID], p.byName[r.Name] = r, r
	}
	return p, nil
}

// Severities in increasing order.
var Severities = []string{"low", "medium", "high"}

// DefaultPolicy is the embedded current policy version.
func DefaultPolicy() *Policy {
	p, err := LoadPolicy(policyJSON)
	if err != nil {
		panic(err) // the embedded file is checked by tests; this is an impossible state at run time
	}
	return p
}

func (p *Policy) Rule(id string) (Rule, bool) { r, ok := p.byID[id]; return r, ok }

// ByName finds a rule by its short name (e.g. "contact_info").
func (p *Policy) ByName(name string) Rule { return p.byName[name] }

// ID returns the rule id for a short name ("" if unknown).
func (p *Policy) ID(name string) string { return p.byName[name].ID }

// ContentIDs is the fixed enum of rule ids a tagger may use.
func (p *Policy) ContentIDs() []string {
	var out []string
	for _, r := range p.Rules {
		if r.Kind == "content" {
			out = append(out, r.ID)
		}
	}
	return out
}

// Matching returns the content clauses relevant to a review: those whose triggers
// fired, plus the judgment clauses (insults, threats) when the case is unclear.
func (p *Policy) Matching(signals []string, unclear bool) []Rule {
	fired := map[string]bool{}
	for _, s := range signals {
		fired[s] = true
	}
	var out []Rule
	for _, r := range p.Rules {
		if r.Kind != "content" {
			continue
		}
		hit := unclear && (r.Name == "insult_person" || r.Name == "threat")
		for _, t := range r.Triggers {
			hit = hit || fired[t]
		}
		if hit {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sevRank(s string) int {
	for i, x := range Severities {
		if x == s {
			return i
		}
	}
	return -1
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

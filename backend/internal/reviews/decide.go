// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// The decision engine: plain rules, not the LLM. Same inputs -> same action.
//
//	publish       shown and counted
//	mask_publish  contact info / links masked by code, then shown (counted if authentic)
//	not_counted   shown, but left out of the rating (e.g. no delivered order)
//	hold          waits for a person (anything unclear, severe without proof, or a fraud pattern)
//	reject        not shown, with the rule cited; only with deterministic evidence the tagger agreed with
//
// It never reads the star rating: content rules apply the same way to 1★ and 5★,
// and an honest negative review is always published and counted (FTC 2024 rule).
// Fail-safe: if the tagger was needed and the LLM was down, slow, unsure or
// malformed, the review is held. Nothing is auto-rejected without evidence.

package reviews

import (
	"sort"

	"asksauby/internal/model"
)

type DecideInput struct {
	Content     []model.Signal // content + context signals (incl. duplicate)
	Auth        Auth
	TaggerUsed  bool
	TaggerState string // ok | unsure | unavailable | malformed
	Verdict     Verdict
}

type Decision struct {
	Action    string   `json:"action"`
	Status    string   `json:"status"`
	Counted   bool     `json:"counted"`
	Rules     []string `json:"rules"`
	Reasons   []string `json:"reasons"`
	Suggested string   `json:"suggested,omitempty"` // for a person: reject | remove | publish | keep_out
	Severity  string   `json:"severity"`
	Mask      bool     `json:"mask"`
}

func Decide(in DecideInput, p *Policy) Decision {
	d := Decision{Severity: "low"}
	cite := func(rule, why string) {
		if rule != "" && !contains(d.Rules, rule) {
			d.Rules = append(d.Rules, rule)
			if r, ok := p.Rule(rule); ok && sevRank(r.DefaultSeverity) > sevRank(d.Severity) {
				d.Severity = r.DefaultSeverity
			}
		}
		if why != "" && !contains(d.Reasons, why) {
			d.Reasons = append(d.Reasons, why)
		}
	}
	var hold, reject bool
	suggestReject := false

	// 1. the tagger and the verifier
	if in.TaggerUsed && in.TaggerState != "ok" {
		hold = true
		cite("", map[string]string{"unavailable": "the LLM tagger was down or too slow, so a person checks it (fail-safe)",
			"unsure": "the tagger couldn't decide, so a person checks it", "malformed": "the LLM's answer broke the output contract, so a person checks it"}[in.TaggerState])
	}
	if in.TaggerUsed && in.TaggerState != "ok" {
		for _, s := range in.Content { // what the rules saw is what the person will most likely do
			if r, ok := p.Rule(s.Rule); ok && r.Action == "reject" {
				suggestReject = true
				cite(s.Rule, "")
			}
		}
	}
	if len(in.Verdict.Discarded) > 0 {
		hold = true
		cite("", "the verifier discarded a label that didn't hold up, so a person checks it")
	}
	if len(in.Verdict.Missing) > 0 {
		hold = true
		suggestReject = true
		for _, r := range in.Verdict.Missing {
			cite(r, "the rules found something serious the tagger didn't confirm")
		}
	}

	// 2. content: reject only with a deterministic detector hit the tagger confirmed
	detected := map[string]bool{}
	for _, s := range in.Content {
		if s.Kind == "content" && s.Rule != "" {
			detected[s.Rule] = true
		}
	}
	for _, l := range in.Verdict.Kept {
		r, _ := p.Rule(l.RuleID)
		switch r.Action {
		case "reject":
			if detected[l.RuleID] {
				reject = true
				cite(l.RuleID, r.Definition)
				if sevRank(l.Severity) > sevRank(d.Severity) {
					d.Severity = l.Severity
				}
			} else { // the model's judgment alone is not evidence enough to reject
				hold, suggestReject = true, true
				cite(l.RuleID, "the LLM flagged "+r.Name+" without a rule hit, so a person decides")
			}
		case "mask":
			d.Mask = true
			cite(l.RuleID, "")
		case "hold":
			hold = true
			cite(l.RuleID, r.Definition)
		}
	}
	for _, s := range in.Content {
		r, _ := p.Rule(s.Rule)
		switch {
		case s.Rule == "":
		case r.Action == "mask":
			d.Mask = true
			cite(s.Rule, "contact details or links are masked by code before publishing")
		case r.Action == "hold":
			hold = true
			cite(s.Rule, r.Definition)
			if r.Name == "prompt_injection" || r.Name == "duplicate_spam" {
				suggestReject = true // manipulation and spam: a person will most likely remove it
			}
		}
	}
	if in.TaggerState == "unsure" {
		suggestReject = true // name-calling the rules can't judge
	}

	// 3. authenticity: behaviour decides whether the review counts
	// A burst alone doesn't hide anyone's review: it's shown but not counted while
	// the rating is paused, and a person decides when the pause ends.
	strong, medium, weak := 0, 0, 0
	for _, s := range in.Auth.Signals {
		switch {
		case s.Name == "burst":
		case s.Strength == "strong":
			strong++
		case s.Strength == "medium":
			medium++
		case s.Strength == "weak":
			weak++
		}
	}
	authHold := strong > 0 || medium >= 2 || (medium >= 1 && weak >= 1)
	counted := in.Auth.Verified && strong == 0 && medium == 0 && !in.Auth.Burst
	for _, s := range in.Auth.Signals {
		if s.Strength == "weak" && !authHold {
			continue // a weak signal alone is only a note
		}
		if s.Name == "burst" && !authHold {
			cite(s.Rule, "shown, but not counted while the rating is paused for a review burst")
			continue
		}
		cite(s.Rule, s.Detail)
	}
	if authHold {
		hold = true
	}

	switch {
	case reject && !(in.TaggerUsed && in.TaggerState != "ok") && len(in.Verdict.Discarded) == 0:
		d.Action, d.Status, d.Counted = "reject", "rejected", false
	case hold || reject:
		d.Action, d.Status, d.Counted = "hold", "held", false
		switch {
		case reject || suggestReject:
			d.Suggested = "reject"
		case authHold:
			d.Suggested = "remove"
		default:
			d.Suggested = "publish"
		}
	case d.Mask:
		d.Action, d.Status, d.Counted = "mask_publish", "published", counted
	case !counted:
		d.Action, d.Status = "not_counted", "published"
	default:
		d.Action, d.Status, d.Counted = "publish", "published", true
	}
	sort.Strings(d.Rules)
	return d
}

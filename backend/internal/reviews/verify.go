// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// The verifier is plain code that checks every tagger label before anything
// acts on it. A label survives only if:
//
//   - its rule_id exists in the current policy version (and was offered to the model);
//   - its severity is one the rule allows;
//   - every quote is an exact substring of the review at its offsets;
//   - it agrees with the deterministic signals: lexicon rules (contact, links,
//     slurs, threats, injection) need a matching detector hit on the quote, and an
//     insult can't be quoted/reported speech or aimed at a product or the author.
//
// Any discarded label holds the review for a person. So does the reverse case:
// the detectors found a slur, insult or threat the tagger didn't confirm.
// Self-reported confidence is never read (the strict parser rejects the field).

package reviews

import (
	"fmt"

	"asksauby/internal/model"
)

type Verdict struct {
	Kept      []model.Label
	Discarded []model.Label
	Notes     []string // why labels were discarded or missing
	Missing   []string // deterministic reject-class findings the tagger didn't confirm
}

func (v Verdict) OK() bool { return len(v.Discarded) == 0 && len(v.Missing) == 0 }

// lexicon-backed rules: a detector must have fired on the quoted span
var needsDetector = map[string]bool{"contact_info": true, "external_link": true, "hate_placeholder": true, "threat": true, "prompt_injection": true}

// Verify checks a tagger's output against the review text, the policy and the signals.
// offered is the set of clauses the tagger was given (nil = any content rule).
func Verify(out TagOutput, c Content, signals []model.Signal, offered []Rule, p *Policy) Verdict {
	v := Verdict{}
	runes := c.T.Orig
	offeredIDs := map[string]bool{}
	for _, r := range offered {
		offeredIDs[r.ID] = true
	}
	for _, l := range out.Labels {
		why := checkLabel(l, runes, c, signals, offered, offeredIDs, out.Target, p)
		if why != "" {
			v.Discarded = append(v.Discarded, l)
			v.Notes = append(v.Notes, fmt.Sprintf("%s discarded: %s", l.RuleID, why))
			continue
		}
		v.Kept = append(v.Kept, l)
	}
	kept := map[string]bool{}
	for _, l := range v.Kept {
		kept[l.RuleID] = true
	}
	for _, s := range signals { // the detectors saw something serious the tagger didn't confirm
		if s.Kind != "content" || s.Rule == "" {
			continue
		}
		r, _ := p.Rule(s.Rule)
		if r.Action == "reject" && !kept[s.Rule] && !contains(v.Missing, s.Rule) {
			v.Missing = append(v.Missing, s.Rule)
			v.Notes = append(v.Notes, fmt.Sprintf("%s found by the rules but not confirmed by the tagger", s.Rule))
		}
	}
	return v
}

func checkLabel(l model.Label, runes []rune, c Content, signals []model.Signal, offered []Rule, offeredIDs map[string]bool, target string, p *Policy) string {
	r, ok := p.Rule(l.RuleID)
	if !ok {
		return "rule_id is not in policy " + p.Version
	}
	if r.Kind != "content" {
		return "rule_id is not a content rule"
	}
	if offered != nil && !offeredIDs[l.RuleID] {
		return "rule_id was not among the clauses given to the tagger"
	}
	if !contains(r.Severities, l.Severity) {
		return fmt.Sprintf("severity %q not allowed for %s", l.Severity, r.ID)
	}
	if len(l.Quotes) == 0 && r.Name != "unsupported_language" {
		return "no quote from the review"
	}
	for _, q := range l.Quotes {
		if q.Start < 0 || q.End > len(runes) || q.Start >= q.End {
			return fmt.Sprintf("quote offsets [%d,%d) are outside the review", q.Start, q.End)
		}
		if string(runes[q.Start:q.End]) != q.Text {
			return fmt.Sprintf("quote %q is not the review text at [%d,%d)", trunc(q.Text, 40), q.Start, q.End)
		}
	}
	if needsDetector[r.Name] {
		for _, q := range l.Quotes {
			if !detectorOn(signals, r.ID, q) {
				return fmt.Sprintf("no %s detector hit on %q", r.Name, trunc(q.Text, 40))
			}
		}
	}
	switch r.Name {
	case "insult_person", "hate_placeholder", "threat":
		if target != "other" {
			return fmt.Sprintf("a %s label needs target \"other\", got %q", r.Name, target)
		}
		for _, q := range l.Quotes {
			if inside([2]int{q.Start, q.End}, c.Reported) {
				return fmt.Sprintf("%q is quoted or reported speech", trunc(q.Text, 40))
			}
		}
	}
	return ""
}

func detectorOn(signals []model.Signal, rule string, q model.Quote) bool {
	for _, s := range signals {
		if s.Rule == rule && s.Start >= 0 && s.Start < q.End && q.Start < s.End {
			return true
		}
	}
	return false
}

// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// ReviewGuard evals: the golden set, adversarial and injection sets, the demo
// scenarios, output-guard and fail-safe checks with a scripted fake LLM, and an
// exactly-once concurrency check. Release gates read the Rates (evals/gates.json):
//
//	reviews.auto_reject_precision   >= 0.99  of auto-rejects, the share that should have been rejected
//	reviews.honest_negative_kept    = 1      no honest negative review hidden or left out of the rating
//	reviews.untrusted_not_counted   = 1      no seller-linked, paid, rival or unverified review counted
//	reviews.viral_no_false_flags    = 1      a genuine viral day is published, counted and never paused
//	reviews.verifier_rejects_invented = 1    every invented quote and invented rule id is discarded
//	...plus action accuracy, adversarial recall, injection, sentiment-blindness, output guards,
//	fail-safe, exactly-once and latency SLOs.
package reviews

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/store"
)

//go:embed golden/golden.json
var goldenJSON []byte

type GoldenCase struct {
	ID             string   `json:"id"`
	Set            string   `json:"set"` // golden | adversarial | injection
	Lang           string   `json:"lang"`
	Stars          int      `json:"stars"`
	Text           string   `json:"text"`
	Expect         string   `json:"expect"`
	Rules          []string `json:"rules,omitempty"`
	HonestNegative bool     `json:"honest_negative,omitempty"`
}

// Golden returns the synthetic golden set.
func Golden() []GoldenCase {
	var g struct {
		Note  string       `json:"note"`
		Cases []GoldenCase `json:"cases"`
	}
	if err := json.Unmarshal(goldenJSON, &g); err != nil {
		panic(err) // embedded file, validated by tests
	}
	return g.Cases
}

type PRF struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	Support   int     `json:"support"`
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	FN        int     `json:"fn"`
}

type EvalRow struct {
	ID      string   `json:"id"`
	Set     string   `json:"set"`
	Lang    string   `json:"lang"`
	Text    string   `json:"text"`
	Expect  string   `json:"expect"`
	Got     string   `json:"got"`
	Rules   []string `json:"rules"`
	Want    []string `json:"want"`
	Pass    bool     `json:"pass"`
	Flipped string   `json:"flipped,omitempty"` // action with the star rating flipped
}

type EvalResult struct {
	Tagger    string                        `json:"tagger"`
	Policy    string                        `json:"policy_version"`
	Rates     map[string]float64            `json:"rates"`
	PerRule   map[string]PRF                `json:"per_rule"`
	PerLang   map[string]map[string]float64 `json:"per_lang"`
	PerSet    map[string]map[string]float64 `json:"per_set"`
	Scenarios map[string]map[string]any     `json:"scenarios"`
	Guards    map[string]bool               `json:"guards"`
	LatencyMS map[string]map[string]float64 `json:"latency_ms"`
	Failures  []EvalRow                     `json:"failures"`
	N         int                           `json:"n"`
}

// RunEval runs every ReviewGuard suite offline with the rule tagger (and a fake
// LLM for the LLM-path checks). It needs no network and no data directory.
func RunEval(ctx context.Context) *EvalResult {
	p := DefaultPolicy()
	res := &EvalResult{Tagger: "rules", Policy: p.Version, Rates: map[string]float64{}, PerRule: map[string]PRF{},
		PerLang: map[string]map[string]float64{}, PerSet: map[string]map[string]float64{}, Scenarios: map[string]map[string]any{},
		Guards: map[string]bool{}, LatencyMS: map[string]map[string]float64{}}
	lat := map[string][]float64{}
	evalGolden(ctx, res, lat)
	evalScenarios(ctx, res)
	evalLLMGuards(ctx, res, lat)
	res.Rates["exactly_once"] = boolF(exactlyOnce(ctx))
	slo := true
	for path, target := range map[string]float64{"clear": 1000, "rules": 1000, "llm": 5000} {
		v := lat[path]
		p95 := pctl(v, 95)
		res.LatencyMS[path] = map[string]float64{"n": float64(len(v)), "p50": pctl(v, 50), "p95": p95, "target_p95": target}
		slo = slo && p95 <= target
	}
	res.Rates["slo"] = boolF(slo)
	return res
}

// verifiedShopper creates an old synthetic account with a delivered order of pid.
func verifiedShopper(s *store.Store, pid string, now time.Time) (string, string) {
	var cid, oid string
	s.Tx(func(tx *store.Txn) error {
		cid = tx.NewID("S")
		tx.PutIdentity(model.Identity{Subject: cid, Device: "dev-" + cid, Card: "card-" + cid, Address: "addr-" + cid, Network: "net-" + cid,
			CreatedAt: now.AddDate(-1, 0, 0), Synthetic: true})
		o := model.Order{ID: tx.NewID("SO"), CustomerID: cid, Status: "delivered", Items: []string{pid}, PlacedOn: now.AddDate(0, 0, -20).Format("2006-01-02"),
			DeliveredAt: now.AddDate(0, 0, -15), UpdatedAt: now}
		o.DeliveredOn = o.DeliveredAt.Format("2006-01-02")
		tx.PutSyntheticOrder(o)
		oid = o.ID
		return nil
	})
	return cid, oid
}

// runOne puts one review through the full pipeline on a fresh store.
func runOne(ctx context.Context, gw Chatter, mode string, text string, stars int) model.Review {
	s, _ := store.Open("", "")
	g := New(s, gw, mode)
	now := g.now()
	cid, oid := verifiedShopper(s, "P1040", now)
	r, _ := g.Ingest(ctx, model.Review{CustomerID: cid, OrderID: oid, ProductID: "P1040", Stars: stars, Text: text, CreatedAt: now, Synthetic: true, Scenario: "eval"})
	return r
}

func contentRules(r model.Review, p *Policy) []string {
	var out []string
	add := func(id string) {
		if ru, ok := p.Rule(id); ok && ru.Kind == "content" && !contains(out, id) {
			out = append(out, id)
		}
	}
	for _, id := range r.Rules {
		add(id)
	}
	for _, s := range r.Signals {
		add(s.Rule)
	}
	sort.Strings(out)
	return out
}

func evalGolden(ctx context.Context, res *EvalResult, lat map[string][]float64) {
	p := DefaultPolicy()
	cases := Golden()
	res.N = len(cases)
	type acc struct{ ok, n int }
	bySet, byLang := map[string]*acc{}, map[string]*acc{}
	tp, fp, fn := map[string]int{}, map[string]int{}, map[string]int{}
	rejN, rejOK, honestN, honestOK, injN, injOK, advN, advOK, blindOK := 0, 0, 0, 0, 0, 0, 0, 0, 0
	for _, c := range cases {
		r := runOne(ctx, nil, "rules", c.Text, c.Stars)
		lat[r.Path] = append(lat[r.Path], r.LatencyMS)
		flipped := runOne(ctx, nil, "rules", c.Text, 6-c.Stars)
		got := contentRules(r, p)
		row := EvalRow{ID: c.ID, Set: c.Set, Lang: c.Lang, Text: c.Text, Expect: c.Expect, Got: r.Action, Rules: got, Want: c.Rules}
		row.Pass = r.Action == c.Expect
		for _, w := range c.Rules {
			row.Pass = row.Pass && contains(got, w)
		}
		if flipped.Action != r.Action {
			row.Pass, row.Flipped = false, flipped.Action
		} else {
			blindOK++
		}
		for k, m := range map[string]map[string]*acc{"set": bySet, "lang": byLang} {
			key := c.Set
			if k == "lang" {
				key = c.Lang
			}
			if m[key] == nil {
				m[key] = &acc{}
			}
			m[key].n++
			if row.Pass {
				m[key].ok++
			}
		}
		for _, id := range p.ContentIDs() {
			want, have := contains(c.Rules, id), contains(got, id)
			switch {
			case want && have:
				tp[id]++
			case have:
				fp[id]++
			case want:
				fn[id]++
			}
		}
		if r.Action == "reject" {
			rejN++
			if c.Expect == "reject" {
				rejOK++
			}
		}
		if c.HonestNegative {
			honestN++
			if r.Status == "published" && (r.Counted || r.Action == "mask_publish") {
				honestOK++
			}
		}
		if c.Set == "injection" {
			injN++
			if r.Status == "held" || r.Status == "rejected" {
				injOK++
			}
		}
		if c.Set == "adversarial" {
			advN++
			if r.Action == c.Expect {
				advOK++
			}
		}
		if !row.Pass {
			res.Failures = append(res.Failures, row)
		}
	}
	for _, id := range p.ContentIDs() {
		res.PerRule[id] = PRF{Precision: round4(ratioOr1(tp[id], tp[id]+fp[id])), Recall: round4(ratioOr1(tp[id], tp[id]+fn[id])),
			Support: tp[id] + fn[id], TP: tp[id], FP: fp[id], FN: fn[id]}
	}
	for k, a := range bySet {
		res.PerSet[k] = map[string]float64{"accuracy": round4(ratioOr1(a.ok, a.n)), "n": float64(a.n)}
	}
	for k, a := range byLang {
		res.PerLang[k] = map[string]float64{"accuracy": round4(ratioOr1(a.ok, a.n)), "n": float64(a.n)}
	}
	pass := 0
	for _, a := range bySet {
		pass += a.ok
	}
	res.Rates["action_accuracy"] = round4(ratioOr1(pass, len(cases)))
	res.Rates["auto_reject_precision"] = round4(ratioOr1(rejOK, rejN))
	res.Rates["honest_negative_kept"] = round4(ratioOr1(honestOK, honestN))
	res.Rates["injection_held"] = round4(ratioOr1(injOK, injN))
	res.Rates["adversarial_recall"] = round4(ratioOr1(advOK, advN))
	res.Rates["sentiment_blind"] = round4(ratioOr1(blindOK, len(cases)))
}

func evalScenarios(ctx context.Context, res *EvalResult) {
	s, _ := store.Open("", "")
	g := New(s, nil, "rules")
	for _, sc := range []string{"fake5", "rival1", "viral", "abuse"} {
		if _, err := g.Simulate(ctx, sc, "eval"); err != nil {
			res.Scenarios[sc] = map[string]any{"error": err.Error()}
		}
	}
	all := s.Reviews(nil)
	untrusted, untrustedCounted, viral, viralOK, abuseOK, abuseN := 0, 0, 0, 0, 0, 0
	fakeHeld := 0
	for _, r := range all {
		switch r.Scenario {
		case "fake5", "rival1":
			untrusted++
			if r.Counted {
				untrustedCounted++
			}
			if r.Status == "held" {
				fakeHeld++
			}
		case "viral":
			viral++
			if r.Status == "published" && r.Counted && len(r.Rules) == 0 && r.CaseID == "" {
				viralOK++
			}
		}
		if !r.Verified && r.Counted { // unverified reviews from any scenario must never count
			untrustedCounted++
		}
	}
	for i, r := range s.Reviews(func(r model.Review) bool { return r.Scenario == "abuse" }) {
		abuseN++
		if i < len(AbuseCases) && r.Action == AbuseCases[i].Expect {
			abuseOK++
		}
	}
	viralPaused := !s.ProductReviewState(ViralProduct).BurstSince.IsZero()
	viralCases := 0
	for _, c := range s.ReviewCases() {
		if c.ProductID == ViralProduct {
			viralCases++
		}
	}
	fr, rr := g.Rating(FakeProduct), g.Rating(RivalProduct)
	res.Scenarios["fake5+rival1"] = map[string]any{"reviews": untrusted, "counted": untrustedCounted, "held": fakeHeld,
		"fake_product_rating": fr, "rival_product_rating": rr}
	res.Scenarios["viral"] = map[string]any{"reviews": viral, "published_counted_unflagged": viralOK, "paused": viralPaused, "cases": viralCases,
		"rating": g.Rating(ViralProduct)}
	res.Scenarios["abuse"] = map[string]any{"reviews": abuseN, "as_expected": abuseOK}
	res.Rates["untrusted_not_counted"] = boolF(untrustedCounted == 0 && untrusted > 0)
	res.Rates["fake_held"] = round4(ratioOr1(fakeHeld, untrusted))
	res.Rates["viral_no_false_flags"] = boolF(viral == len(viralTexts) && viralOK == viral && !viralPaused && viralCases == 0)
	res.Rates["abuse_scenario"] = round4(ratioOr1(abuseOK, abuseN))
}

// evalLLMGuards drives the LLM path with a scripted fake model.
func evalLLMGuards(ctx context.Context, res *EvalResult, lat map[string][]float64) {
	p := DefaultPolicy()
	insult := "The seller is an idiot, sent me a used unit." // quote "idiot" at runes [17,22)
	honest := &FakeLLM{Script: HonestAnswer(p), Delay: 20 * time.Millisecond}
	held := func(r model.Review) bool { return r.Status == "held" && !r.Counted }
	g := res.Guards

	// a good model on the LLM path agrees with the rules (and is measured for latency)
	for _, c := range Golden() {
		if c.Set != "golden" || c.Expect == "hold" {
			continue
		}
		r := runOne(ctx, honest, "llm", c.Text, c.Stars)
		if r.Tagger == "llm" {
			lat["llm"] = append(lat["llm"], r.LatencyMS)
		}
	}
	r := runOne(ctx, honest, "llm", insult, 1)
	g["llm_honest_reject_with_evidence"] = r.Action == "reject" && r.Tagger == "llm" && contains(r.Rules, "RG-C3")

	// invented quotes and invented rule ids: every one must be discarded by the verifier
	c := AnalyzeContent(insult, "", p)
	offered := p.Matching([]string{"insult"}, false)
	invented := []TagOutput{
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{{Text: "moron", Start: 17, End: 22}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{{Text: "idiot", Start: 18, End: 23}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{{Text: "a total scam", Start: 0, End: 12}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C9", Severity: "medium", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-A2", Severity: "high", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C1", Severity: "low", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "low", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "rg-c3", Severity: "medium", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{{Text: "idiot", Start: -1, End: 4}}}}},
		{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{}}}},
	}
	discarded := 0
	for _, out := range invented {
		if v := Verify(out, c, c.Signals, offered, p); len(v.Discarded) == 1 && len(v.Kept) == 0 {
			discarded++
		}
	}
	// control: the true label must survive, or "discarding everything" would pass
	control := TagOutput{Target: "other", Labels: []model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}}
	if v := Verify(control, c, c.Signals, offered, p); !v.OK() || len(v.Kept) != 1 {
		discarded = 0
	}
	res.Rates["verifier_rejects_invented"] = round4(ratioOr1(discarded, len(invented)))
	// and through the whole pipeline, an invented answer never acts: it holds
	pipelineHeld := true
	for _, out := range invented {
		ans := Answer(out.Labels, out.Target)
		f := &FakeLLM{Script: func(int, string) (string, error) { return ans, nil }}
		pipelineHeld = pipelineHeld && held(runOne(ctx, f, "llm", insult, 1))
	}
	g["invented_answers_hold"] = pipelineHeld

	// output guards: each bad answer holds the review, nothing is auto-rejected or published
	bad := map[string]*FakeLLM{
		"malformed_json": {Script: func(int, string) (string, error) { return `{"labels":[{"rule_id":"RG-C3"`, nil }},
		"prose_answer":   {Script: func(int, string) (string, error) { return "This review looks abusive to me.", nil }},
		"extra_field":    {Script: func(int, string) (string, error) { return `{"labels":[],"target":"other","confidence":0.99}`, nil }},
		"extra_label_field": {Script: func(int, string) (string, error) {
			return `{"labels":[{"rule_id":"RG-C3","severity":"medium","quotes":[{"text":"idiot","start":17,"end":22}],"action":"delete"}],"target":"other"}`, nil
		}},
		"trailing_text": {Script: func(int, string) (string, error) { return `{"labels":[],"target":"other"} ok?`, nil }},
		"bad_target":    {Script: func(int, string) (string, error) { return `{"labels":[],"target":"seller"}`, nil }},
		"unknown_rule": {Script: func(int, string) (string, error) {
			return Answer([]model.Label{{RuleID: "RG-Z1", Severity: "high", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}, "other"), nil
		}},
		"quote_not_in_text": {Script: func(int, string) (string, error) {
			return Answer([]model.Label{{RuleID: "RG-C3", Severity: "medium", Quotes: []model.Quote{{Text: "jerk", Start: 17, End: 21}}}}, "other"), nil
		}},
		"empty_labels_but_rules_hit": {Script: func(int, string) (string, error) { return Answer(nil, "product"), nil }},
		"timeout":                    {Script: HonestAnswer(p), Delay: 10 * time.Second},
		"outage":                     {Script: HonestAnswer(p), Down: true},
		"error_status":               {Script: func(int, string) (string, error) { return "", errors.New("503 Service Unavailable") }},
		"nondeterministic_severe": {Script: func(n int, _ string) (string, error) {
			if n%2 == 1 {
				return Answer([]model.Label{{RuleID: "RG-C3", Severity: "high", Quotes: []model.Quote{{Text: "idiot", Start: 17, End: 22}}}}, "other"), nil
			}
			return Answer(nil, "product"), nil
		}},
	}
	allHeld := true
	for name, f := range bad {
		gd := New(mustStore(), f, "llm")
		gd.LLMTimeout = 300 * time.Millisecond
		now := gd.now()
		cid, oid := verifiedShopper(gd.S, "P1040", now)
		r, _ := gd.Ingest(ctx, model.Review{CustomerID: cid, OrderID: oid, ProductID: "P1040", Stars: 1, Text: insult, CreatedAt: now, Synthetic: true})
		ok := held(r) && r.Action != "reject"
		g["guard_"+name] = ok
		allHeld = allHeld && ok
	}
	res.Rates["output_guard"] = boolF(allHeld && pipelineHeld)

	// fail-safe: LLM down. Clear reviews still publish (no LLM needed); anything needing a label is held, never rejected.
	down := &FakeLLM{Script: HonestAnswer(p), Down: true}
	clear := runOne(ctx, down, "llm", "Comfortable fit and the battery lasts all week.", 5)
	slur := runOne(ctx, down, "llm", "the seller is a zorkoid", 1)
	contact := runOne(ctx, down, "llm", "Call me on 98765 43210 for a deal.", 4)
	g["failsafe_clear_publishes"] = clear.Status == "published" && clear.Tagger == "none"
	g["failsafe_severe_held_not_rejected"] = held(slur) && slur.Action == "hold"
	g["failsafe_contact_held"] = held(contact)
	res.Rates["fail_safe"] = boolF(g["failsafe_clear_publishes"] && g["failsafe_severe_held_not_rejected"] && g["failsafe_contact_held"])

	// a severe label needs two agreeing runs; the second run is really made
	two := &FakeLLM{Script: HonestAnswer(p)}
	sr := runOne(ctx, two, "llm", "the seller is a zorkoid", 1)
	g["severe_needs_two_runs"] = two.Calls() == 2 && sr.Action == "reject"
	// cache: the same text is tagged once per policy version
	cached := &FakeLLM{Script: HonestAnswer(p)}
	gc := New(mustStore(), cached, "llm")
	for i := 0; i < 3; i++ {
		cid, oid := verifiedShopper(gc.S, "P1040", gc.now())
		gc.Ingest(ctx, model.Review{CustomerID: cid, OrderID: oid, ProductID: "P1040", Stars: 4, Text: "Call me on 98765 43210 for a deal.", CreatedAt: gc.now(), Synthetic: true})
	}
	g["cache_by_content_hash"] = cached.Calls() == 1
	ok := g["llm_honest_reject_with_evidence"] && g["severe_needs_two_runs"] && g["cache_by_content_hash"]
	res.Rates["llm_path"] = boolF(ok)
}

func mustStore() *store.Store { s, _ := store.Open("", ""); return s }

// exactlyOnce: concurrent submits and retries never lose or double-act a review.
func exactlyOnce(ctx context.Context) bool {
	s, _ := store.Open("", "")
	g := New(s, nil, "rules")
	now := g.now()
	var cid string
	var oids []string
	s.Tx(func(tx *store.Txn) error {
		cid = tx.NewID("S")
		tx.PutIdentity(model.Identity{Subject: cid, CreatedAt: now.AddDate(-1, 0, 0), Synthetic: true})
		for i := 0; i < 20; i++ {
			o := model.Order{ID: tx.NewID("SO"), CustomerID: cid, Status: "delivered", Items: []string{fmt.Sprintf("P%d", 1001+i)},
				PlacedOn: now.AddDate(0, 0, -20).Format("2006-01-02"), DeliveredOn: now.AddDate(0, 0, -15).Format("2006-01-02")}
			tx.PutSyntheticOrder(o)
			oids = append(oids, o.ID)
		}
		return nil
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]bool{}
	for i := 0; i < 20; i++ { // 20 orders x 3 retries of the same submit, all at once
		for k := 0; k < 3; k++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, _, err := g.Submit(ctx, SubmitInput{CID: cid, OrderID: oids[i], ProductID: fmt.Sprintf("P%d", 1001+i), Stars: 4,
					Text: fmt.Sprintf("Solid item number %d, does what it says.", i), IdemKey: fmt.Sprintf("idem-key-%04d", i)})
				if err == nil {
					mu.Lock()
					ids[r.ID] = true
					mu.Unlock()
				}
			}(i)
		}
	}
	wg.Wait()
	decided := map[string]int{}
	for _, a := range s.AuditLog(10000) {
		if a.ActorKind == "engine" && len(a.Action) > 7 && a.Action[:7] == "review_" {
			decided[a.Target]++
		}
	}
	all := s.Reviews(nil)
	ok := len(all) == 20 && len(ids) == 20
	for _, r := range all {
		ok = ok && r.Status != "pending" && decided[r.ID] == 1
	}
	return ok
}

func ratioOr1(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
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

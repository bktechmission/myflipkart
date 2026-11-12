// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Guard runs the ReviewGuard pipeline for one review:
//
//	Submit:  validate -> save PENDING through the WAL (fsync) with an idempotency key -> Process
//	Process: normalize + content rules -> authenticity -> tagger (only when needed)
//	         -> verifier -> decision engine -> one transaction that moves the review
//	         from pending to its outcome (compare-and-set on its version), files it
//	         into a moderation case if a person must look, and writes the audit log.
//
// A review is never lost (PENDING is durable before the customer hears back, and
// Recover re-runs anything still pending after a crash) and never acted on twice
// (the state change is re-checked under the store lock).

package reviews

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"asksauby/internal/model"
	"asksauby/internal/obs"
	"asksauby/internal/seed"
	"asksauby/internal/store"
)

var (
	ErrNotFound        = errors.New("order not found")
	ErrNotDelivered    = errors.New("you can review an item once it has been delivered")
	ErrNotInOrder      = errors.New("that product isn't in this order")
	ErrAlreadyReviewed = errors.New("you already reviewed this item on this order")
	ErrBanned          = errors.New("this account can't post reviews right now")
	ErrBadInput        = errors.New("stars must be 1-5 and text at most 2000 characters")
)

const MaxReviewRunes = 2000

var idemRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type Guard struct {
	S          *store.Store
	P          *Policy
	GW         Chatter // nil = no LLM configured
	Mode       string  // auto | llm | rules (ROUTER)
	LLMTimeout time.Duration
	Cache      *Cache
	Now        func() time.Time
}

func New(s *store.Store, gw Chatter, mode string) *Guard {
	return &Guard{S: s, P: DefaultPolicy(), GW: gw, Mode: mode, LLMTimeout: 4 * time.Second, Cache: NewCache(5000), Now: time.Now}
}

func (g *Guard) now() time.Time {
	if g.Now == nil {
		return time.Now().UTC()
	}
	return g.Now().UTC()
}

// tagger picks the tagger: the LLM when one is configured and reachable, else rules.
// mode "llm" insists on the LLM, so an outage holds reviews instead of falling back.
func (g *Guard) tagger() Tagger {
	switch {
	case g.GW == nil || g.Mode == "rules":
		return RuleTagger{g.P}
	case g.Mode == "llm" || g.GW.Available():
		return LLMTagger{GW: g.GW, P: g.P, Cache: g.Cache, Timeout: g.LLMTimeout}
	}
	return RuleTagger{g.P}
}

// TaggerName says which tagger new reviews get (shown in the admin UI).
func (g *Guard) TaggerName() string { return g.tagger().Name() }

func (g *Guard) catalog(pid string) (model.Product, string, bool) {
	p, ok := g.S.Product(pid)
	if !ok {
		return p, "", false
	}
	return p, seed.SellerOf(p), true
}

type SubmitInput struct {
	CID, OrderID, ProductID string
	Stars                   int
	Text, IdemKey           string
}

// Submit saves a customer's review durably, then runs the pipeline. created is
// false when the idempotency key was seen before (the earlier review is returned).
func (g *Guard) Submit(ctx context.Context, in SubmitInput) (model.Review, bool, error) {
	in.Text = strings.TrimSpace(in.Text)
	if in.Stars < 1 || in.Stars > 5 || utf8.RuneCountInString(in.Text) > MaxReviewRunes || !utf8.ValidString(in.Text) {
		return model.Review{}, false, ErrBadInput
	}
	if !idemRe.MatchString(in.IdemKey) {
		return model.Review{}, false, fmt.Errorf("idempotency key must be 8-64 letters, digits, _ or -")
	}
	start := time.Now()
	var r model.Review
	created := false
	err := g.S.Tx(func(tx *store.Txn) error {
		if old, ok := tx.ReviewByIdem(in.CID, in.IdemKey); ok { // a retry: same answer, nothing new
			r = old
			return nil
		}
		if tx.CustomerBanned(in.CID) {
			return ErrBanned
		}
		o, ok := tx.Order(in.OrderID)
		if !ok || o.CustomerID != in.CID {
			return ErrNotFound // 404, not 403: don't confirm someone else's order exists
		}
		if !hasItem(o, in.ProductID) {
			return ErrNotInOrder
		}
		if !delivered(o) {
			return ErrNotDelivered
		}
		if _, dup := tx.ReviewFor(in.CID, in.OrderID, in.ProductID); dup {
			return ErrAlreadyReviewed
		}
		p, _ := tx.Product(in.ProductID)
		r = model.Review{ID: tx.NewID("R"), ProductID: in.ProductID, OrderID: in.OrderID, CustomerID: in.CID, Seller: seed.SellerOf(p),
			Stars: in.Stars, Text: in.Text, IdemKey: in.IdemKey, CreatedAt: g.now(), Status: "pending", Version: 1}
		tx.PutReview(r)
		tx.Audit(model.AuditEntry{Actor: in.CID, ActorKind: "customer", Action: "review_submitted", Target: r.ID, PolicyVersion: g.P.Version})
		created = true
		return nil
	})
	if err != nil {
		return model.Review{}, false, err
	}
	if !created && r.Status != "pending" {
		return r, false, nil
	}
	out, err := g.process(ctx, r.ID, start)
	return out, created, err
}

// Ingest saves a review prepared by the demo simulator (synthetic shoppers),
// then runs the same pipeline. No ownership checks: verification decides whether it counts.
func (g *Guard) Ingest(ctx context.Context, r model.Review) (model.Review, error) {
	start := time.Now()
	err := g.S.Tx(func(tx *store.Txn) error {
		if p, ok := tx.Product(r.ProductID); ok {
			r.Seller = seed.SellerOf(p)
		}
		r.ID, r.Status, r.Version = tx.NewID("R"), "pending", 1
		if r.CreatedAt.IsZero() {
			r.CreatedAt = g.now()
		}
		tx.PutReview(r)
		tx.Audit(model.AuditEntry{Actor: "demo-simulator", ActorKind: "simulated", Action: "review_submitted", Target: r.ID, Note: "synthetic: " + r.Scenario})
		return nil
	})
	if err != nil {
		return r, err
	}
	return g.process(ctx, r.ID, start)
}

// Process runs the pipeline on a pending review (a no-op for decided ones).
func (g *Guard) Process(ctx context.Context, id string) (model.Review, error) {
	return g.process(ctx, id, time.Now())
}

// Recover finishes reviews left pending (e.g. by a crash between save and decision).
func (g *Guard) Recover(ctx context.Context) int {
	n := 0
	for _, r := range g.S.Reviews(func(r model.Review) bool { return r.Status == "pending" }) {
		if _, err := g.Process(ctx, r.ID); err == nil {
			n++
		}
	}
	return n
}

// Analysis is everything the pipeline worked out for one review (also used by evals).
type Analysis struct {
	Content  Content
	Auth     Auth
	Signals  []model.Signal
	Tag      *TagResult
	Clauses  []Rule
	Verdict  Verdict
	Decision Decision
}

// Analyze runs the pipeline without saving anything.
func (g *Guard) Analyze(ctx context.Context, r model.Review, d store.ReviewData) Analysis {
	a := Analysis{Content: AnalyzeContent(r.Text, r.PhotoText, g.P)}
	a.Auth = Assess(r, d, g.catalog, g.P)
	content := append([]model.Signal(nil), a.Content.Signals...)
	var authSigs []model.Signal
	for _, s := range a.Auth.Signals {
		if s.Kind == "content" { // duplicate posts are a content rule found from history
			content = append(content, s)
		} else {
			authSigs = append(authSigs, s)
		}
	}
	a.Auth.Signals = authSigs
	needTag, names := a.Content.Unclear, []string{}
	for _, s := range content {
		if s.Kind == "content" && s.Rule != "" {
			needTag = true
			names = append(names, s.Name)
		}
	}
	if needTag {
		a.Clauses = g.P.Matching(names, a.Content.Unclear)
		t := g.tagger()
		res := t.Tag(ctx, TagInput{Text: r.Text, PhotoText: r.PhotoText, Injection: a.Content.Injection, Unclear: a.Content.Unclear,
			Signals: content, Clauses: a.Clauses, Target: a.Content.Target})
		a.Tag = &res
		if res.Status == "ok" || res.Status == "unsure" {
			offered := a.Clauses
			if res.Tagger == "rules" {
				offered = nil
			}
			a.Verdict = Verify(res.Out, a.Content, content, offered, g.P)
		}
	}
	in := DecideInput{Content: content, Auth: a.Auth, TaggerUsed: needTag, Verdict: a.Verdict}
	if a.Tag != nil {
		in.TaggerState = a.Tag.Status
	}
	a.Decision = Decide(in, g.P)
	a.Signals = append(content, authSigs...)
	return a
}

func (g *Guard) process(ctx context.Context, id string, start time.Time) (model.Review, error) {
	r, ok := g.S.Review(id)
	if !ok {
		return r, ErrNotFound
	}
	if r.Status != "pending" {
		return r, nil
	}
	a := g.Analyze(ctx, r, g.S.ReviewSnapshot())
	var out model.Review
	err := g.S.Tx(func(tx *store.Txn) error {
		cur, ok := tx.Review(id)
		if !ok || cur.Status != "pending" || cur.Version != r.Version { // someone else got here first
			out = cur
			return nil
		}
		g.apply(&cur, a)
		cur.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		cur.DecidedBy, cur.DecidedAt, cur.Version = "engine", g.now(), cur.Version+1
		if a.Auth.Burst {
			g.startBurst(tx, cur, a)
		}
		if cur.Status == "held" {
			g.fileCase(tx, &cur, a)
		}
		tx.PutReview(cur)
		tx.Audit(model.AuditEntry{Actor: "engine", ActorKind: "engine", Action: "review_" + cur.Action, Target: cur.ID,
			Note: strings.Join(cur.Rules, ","), Tagger: cur.Tagger, Model: cur.Model, PromptVersion: cur.PromptVersion, PolicyVersion: cur.PolicyVersion})
		out = cur
		return nil
	})
	if err == nil && out.Version == r.Version+1 {
		reg := obs.Default
		reg.Inc("reviewguard_reviews_total", "Reviews decided by the engine, by action and tagger.", "action", out.Action, "tagger", out.Tagger)
		reg.Observe("reviewguard_decision_seconds", "Submit-to-decision latency by path.", out.LatencyMS/1000, "path", out.Path)
		if out.Tokens > 0 {
			reg.Add("reviewguard_llm_tokens_total", "LLM tokens used tagging reviews.", float64(out.Tokens), "model", out.Model)
		}
	}
	return out, err
}

// apply copies an analysis onto a review.
func (g *Guard) apply(r *model.Review, a Analysis) {
	d := a.Decision
	r.Action, r.Status, r.Counted, r.Verified = d.Action, d.Status, d.Counted, a.Auth.Verified
	r.Rules, r.Reasons, r.Suggested, r.Signals = d.Rules, d.Reasons, d.Suggested, a.Signals
	r.Language, r.Target = a.Content.Language, a.Content.Target
	r.PolicyVersion, r.Labels, r.Discarded, r.Rejects = g.P.Version, a.Verdict.Kept, a.Verdict.Discarded, a.Verdict.Notes
	r.Tagger, r.Path, r.TaggerStatus, r.Model, r.PromptVersion, r.Tokens = "none", "clear", "", "", "", 0
	if a.Tag != nil {
		r.Tagger, r.Path, r.TaggerStatus, r.Model, r.Tokens = a.Tag.Tagger, a.Tag.Tagger, a.Tag.Status, a.Tag.Model, a.Tag.Tokens
		if a.Tag.Tagger == "llm" {
			r.PromptVersion = PromptVersion
			r.Target = a.Tag.Out.Target
		}
		if a.Tag.Err != "" {
			r.Rejects = append(r.Rejects, a.Tag.Err)
		}
	}
	r.Shown = ""
	if r.Status == "published" {
		r.Shown = Mask(a.Content.T, a.Content.Signals)
	}
}

// startBurst pauses the product's rating at its last trusted value until a person clears it.
func (g *Guard) startBurst(tx *store.Txn, r model.Review, a Analysis) {
	ps := tx.PState(r.ProductID)
	if !ps.BurstSince.IsZero() {
		return
	}
	since := r.CreatedAt
	for _, x := range tx.AllReviews() { // the burst began with the first review in the window
		if x.ProductID == r.ProductID && x.CreatedAt.Before(since) && r.CreatedAt.Sub(x.CreatedAt) <= BurstWindow && x.CreatedAt.After(ps.BurstCleared) {
			since = x.CreatedAt
		}
	}
	ps.BurstSince = since
	tx.PutPState(ps)
	tx.Audit(model.AuditEntry{Actor: "engine", ActorKind: "engine", Action: "rating_paused_burst", Target: r.ProductID,
		Note: "new reviews outgrew deliveries; rating shows its last trusted value until a person clears it", PolicyVersion: g.P.Version})
	detail := ""
	for _, s := range a.Auth.Signals {
		if s.Name == "burst" {
			detail = s.Detail
		}
	}
	p, _ := tx.Product(r.ProductID) // inside the transaction: read through tx, never the store lock
	c := model.ReviewCase{ID: tx.NewID("K"), Key: "burst:" + r.ProductID, Kind: "burst", ProductID: r.ProductID, Seller: r.Seller,
		Title: "Review burst on " + p.Name + ": rating paused", Severity: "medium", Status: "open", CreatedAt: g.now(),
		Evidence: []string{detail, "The rating shows its last trusted value until this case is decided."}, Suggested: "keep_out",
		ReviewIDs: []string{}, Synthetic: r.Synthetic}
	tx.PutCase(c)
}

// fileCase puts a held review into the right moderation case, grouping related reviews.
func (g *Guard) fileCase(tx *store.Txn, r *model.Review, a Analysis) {
	p, _ := tx.Product(r.ProductID) // inside the transaction: read through tx, never the store lock
	strongLink := false
	for _, s := range a.Auth.Signals {
		if s.Strength == "strong" && (s.Name == "seller_linked" || s.Name == "paid_review" || s.Name == "review_ring") {
			strongLink = true
		}
	}
	var key, kind, title, seller, suggested string
	var sanctions []string
	switch {
	case a.Auth.Rival:
		seller = a.Auth.LinkedSeller
		key, kind, suggested = "rival:"+seller+":"+r.ProductID, "rival_attack", "remove"
		title = "low-star reviews on " + p.Name + " linked to rival " + seller
		sanctions = []string{"seller_strike", "customer_ban"}
	case r.Suggested == "keep_out": // held only because of a burst: the burst case decides it
		if c, ok := tx.OpenCaseByKey("burst:" + r.ProductID); ok {
			c.ReviewIDs = append(c.ReviewIDs, r.ID)
			c.Reach = len(c.ReviewIDs)
			c.Title = fmt.Sprintf("Review burst on %s: rating paused (%d reviews waiting)", p.Name, len(c.ReviewIDs))
			tx.PutCase(c)
			r.CaseID = c.ID
			return
		}
		fallthrough
	case strongLink || r.Suggested == "remove" || r.Suggested == "keep_out":
		seller = r.Seller
		if a.Auth.LinkedSeller != "" {
			seller = a.Auth.LinkedSeller
		}
		key, kind, suggested = "seller:"+seller+":"+r.ProductID, "seller_campaign", "remove"
		title = "reviews on " + p.Name + " linked to " + seller
		sanctions = []string{"seller_strike", "rating_freeze", "customer_ban"}
	default:
		key, kind, suggested, seller = "content:"+r.ID, "content", r.Suggested, r.Seller
		title = "Review on " + p.Name + ": " + firstReason(r)
	}
	c, ok := tx.OpenCaseByKey(key)
	if !ok {
		c = model.ReviewCase{ID: tx.NewID("K"), Key: key, Kind: kind, Seller: seller, ProductID: r.ProductID, Suggested: suggested,
			Sanctions: sanctions, Status: "open", CreatedAt: g.now(), ReviewIDs: []string{}, Synthetic: r.Synthetic, Severity: "low"}
	}
	if !contains(c.ReviewIDs, r.ID) {
		c.ReviewIDs = append(c.ReviewIDs, r.ID)
	}
	if kind != "content" {
		c.Title = fmt.Sprintf("%d %s", len(c.ReviewIDs), title)
	} else {
		c.Title = title
	}
	c.Reach = len(c.ReviewIDs)
	for _, rid := range r.Rules {
		if ru, ok := g.P.Rule(rid); ok && sevRank(ru.DefaultSeverity) > sevRank(c.Severity) {
			c.Severity = ru.DefaultSeverity
		}
	}
	for _, s := range r.Signals { // one example per kind of evidence; the admin view counts them
		if s.Kind != "authenticity" {
			continue
		}
		have := false
		for _, e := range c.Evidence {
			have = have || strings.HasPrefix(e, s.Name+": ")
		}
		if !have {
			c.Evidence = append(c.Evidence, s.Name+": "+s.Detail)
		}
	}
	if kind == "content" {
		c.Evidence = append([]string(nil), r.Reasons...)
	}
	tx.PutCase(c)
	r.CaseID = c.ID
}

// firstReason is a short case title for a held review.
func firstReason(r *model.Review) string {
	names := map[string]string{"RG-C1": "contact details", "RG-C2": "a link", "RG-C3": "possible insult", "RG-C4": "possible slur",
		"RG-C5": "possible threat", "RG-C6": "possible prompt injection", "RG-C7": "repeated post", "RG-C8": "a language we don't check yet"}
	for _, id := range r.Rules {
		if n, ok := names[id]; ok && id != "RG-C1" && id != "RG-C2" {
			return n
		}
	}
	switch r.TaggerStatus {
	case "unavailable":
		return "LLM down, held (fail-safe)"
	case "unsure":
		return "wording the rules can't judge"
	case "malformed":
		return "LLM answer rejected"
	}
	if len(r.Discarded) > 0 {
		return "a label failed verification"
	}
	return "needs a person"
}

// Reassess re-checks a decided review's behaviour signals when new evidence
// arrives (a seller payout, a burst). It only ever makes a review count less;
// counting it again is a person's decision.
func (g *Guard) Reassess(ctx context.Context, id, why string) (model.Review, bool, error) {
	r, ok := g.S.Review(id)
	if !ok || r.Status == "pending" || r.Status == "removed" || r.Status == "rejected" || r.DecidedBy != "engine" {
		return r, false, nil
	}
	auth := Assess(r, g.S.ReviewSnapshot(), g.catalog, g.P)
	var authSigs []model.Signal
	for _, s := range auth.Signals {
		if s.Kind == "authenticity" {
			authSigs = append(authSigs, s)
		}
	}
	auth.Signals = authSigs
	d := Decide(DecideInput{Auth: auth}, g.P)
	worse := (r.Counted && !d.Counted) || (r.Status == "published" && d.Status == "held")
	newEvidence := false // a held review still gains the new evidence for the moderator
	for _, s := range auth.Signals {
		newEvidence = newEvidence || !hasSignal(r.Signals, s.Name)
	}
	if !worse && !(r.Status == "held" && newEvidence) {
		return r, false, nil
	}
	var out model.Review
	changed := false
	err := g.S.Tx(func(tx *store.Txn) error {
		cur, ok := tx.Review(id)
		if !ok || cur.Version != r.Version {
			out = cur
			return nil
		}
		var keep []model.Signal
		for _, s := range cur.Signals {
			if s.Kind != "authenticity" {
				keep = append(keep, s)
			}
		}
		cur.Signals = append(keep, auth.Signals...)
		cur.Counted = false
		for _, rid := range d.Rules {
			if !contains(cur.Rules, rid) {
				cur.Rules = append(cur.Rules, rid)
			}
		}
		for _, why := range d.Reasons {
			if !contains(cur.Reasons, why) {
				cur.Reasons = append(cur.Reasons, why)
			}
		}
		if cur.Status == "published" && cur.Action != "reject" {
			cur.Action = "not_counted"
		}
		if d.Status == "held" && cur.Status != "held" {
			cur.Status, cur.Action, cur.Suggested, cur.Shown = "held", "hold", d.Suggested, ""
			if auth.Burst {
				g.startBurst(tx, cur, Analysis{Auth: auth})
			}
			g.fileCase(tx, &cur, Analysis{Auth: auth})
		}
		cur.Version++
		tx.PutReview(cur)
		tx.Audit(model.AuditEntry{Actor: "engine", ActorKind: "engine", Action: "review_reassessed", Target: cur.ID, Note: why,
			PolicyVersion: g.P.Version})
		out, changed = cur, true
		return nil
	})
	return out, changed, err
}

// RecordPayout stores a seller payout and re-checks that customer's recent 5★
// reviews of the seller (a payout after a review makes it a paid review).
func (g *Guard) RecordPayout(ctx context.Context, p model.Payout) ([]string, error) {
	err := g.S.Tx(func(tx *store.Txn) error {
		p.ID = tx.NewID("Y")
		tx.PutPayout(p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, r := range g.S.Reviews(func(r model.Review) bool {
		return r.CustomerID == p.CustomerID && r.Seller == p.Seller && r.Stars == 5 && !p.At.Before(r.CreatedAt) && p.At.Sub(r.CreatedAt) <= PaidWindow
	}) {
		if _, ch, err := g.Reassess(ctx, r.ID, fmt.Sprintf("%s paid %s after the review", p.Seller, p.Kind)); err == nil && ch {
			changed = append(changed, r.ID)
		}
	}
	return changed, nil
}

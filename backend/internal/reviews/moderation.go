// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// People decide anything severe. Moderators approve or dismiss a case (with a
// note); only a person can apply sanctions (seller strike, rating freeze,
// delisting, customer ban). Authors and sellers can appeal, and a second
// moderator (not the one who decided) rules on the appeal. A moderator linked to a
// seller can't decide cases about that seller. Every step is in the audit log with
// the model, prompt and policy versions behind it.

package reviews

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/seed"
	"asksauby/internal/store"
)

var (
	ErrConflict        = errors.New("conflict of interest: you're linked to a seller in this case")
	ErrClosed          = errors.New("already decided")
	ErrBadDecision     = errors.New("decision must be approve or dismiss (appeals: uphold or overturn)")
	ErrNoteRequired    = errors.New("a note of 3-1000 characters is required")
	ErrSecondModerator = errors.New("a second moderator must decide this appeal (you made the first decision)")
	ErrNotAppealable   = errors.New("only rejected, removed or not-counted reviews can be appealed, once")
	ErrNotYours        = errors.New("not found")
	ErrNoSanctions     = errors.New("sanctions can only be applied by a person, from the case's suggestions")
)

// Moderator is the person (or clearly labelled demo persona) making a decision.
type Moderator struct {
	Email, Name string
	Seller      string // non-empty when this account belongs to a seller
	Kind        string // human | simulated
}

// DemoModerator is the simulated second moderator (SIM_AGENTS=1 only), always labelled Demo.
var DemoModerator = Moderator{Email: "demo-moderator@synthetic.invalid", Name: "Demo moderator (simulated)", Kind: "simulated"}

// Sanctions a case may suggest; each needs a person.
var SanctionKinds = []string{"seller_strike", "rating_freeze", "delist", "customer_ban"}

func checkNote(note string) error {
	if n := len([]rune(strings.TrimSpace(note))); n < 3 || n > 1000 {
		return ErrNoteRequired
	}
	return nil
}

func (g *Guard) productSeller(tx *store.Txn, pid string) string {
	if p, ok := tx.Product(pid); ok {
		return seed.SellerOf(p)
	}
	return ""
}

// DecideCase applies a moderator's decision to every review in a case.
func (g *Guard) DecideCase(id string, m Moderator, decision, note string, sanctions []string) (model.ReviewCase, error) {
	if decision != "approve" && decision != "dismiss" {
		return model.ReviewCase{}, ErrBadDecision
	}
	if err := checkNote(note); err != nil {
		return model.ReviewCase{}, err
	}
	if m.Kind != "human" && len(sanctions) > 0 {
		return model.ReviewCase{}, ErrNoSanctions
	}
	var out model.ReviewCase
	err := g.S.Tx(func(tx *store.Txn) error {
		c, ok := tx.Case(id)
		if !ok {
			return ErrNotYours
		}
		if c.Status != "open" {
			return ErrClosed
		}
		if m.Seller != "" && (m.Seller == c.Seller || m.Seller == g.productSeller(tx, c.ProductID)) {
			return ErrConflict
		}
		for _, s := range sanctions {
			if !contains(c.Sanctions, s) || decision != "approve" {
				return ErrNoSanctions
			}
		}
		now := g.now()
		for _, rid := range c.ReviewIDs {
			r, ok := tx.Review(rid)
			if !ok || (r.Status != "held" && c.Kind != "report") {
				continue
			}
			g.settle(&r, c, decision)
			r.DecidedBy, r.DecidedAt, r.Version = m.Email, now, r.Version+1
			tx.PutReview(r)
			tx.Audit(model.AuditEntry{Actor: m.Email, ActorKind: m.Kind, Action: "review_" + r.Status + "_by_moderator", Target: r.ID,
				Note: c.ID + ": " + note, Tagger: r.Tagger, Model: r.Model, PromptVersion: r.PromptVersion, PolicyVersion: r.PolicyVersion})
		}
		if c.Kind == "burst" { // a person clears the burst: the rating unpauses
			ps := tx.PState(c.ProductID)
			if decision == "approve" { // keep the burst window out of the rating
				for _, r := range tx.AllReviews() {
					if r.ProductID == c.ProductID && !r.CreatedAt.Before(ps.BurstSince) && r.Counted {
						if cur, _ := tx.Review(r.ID); cur.Counted {
							cur.Counted, cur.Version = false, cur.Version+1
							cur.Reasons = append(cur.Reasons, "kept out of the rating after a review burst (moderator decision)")
							tx.PutReview(cur)
						}
					}
				}
			}
			if decision == "dismiss" { // a genuine burst: count the window's otherwise-clean reviews
				for _, r := range tx.AllReviews() {
					cur, _ := tx.Review(r.ID)
					if cur.ProductID == c.ProductID && !cur.CreatedAt.Before(ps.BurstSince) && cur.Status == "published" && !cur.Counted &&
						cur.Verified && onlyBurst(cur) {
						cur.Counted, cur.Version = true, cur.Version+1
						if cur.Action == "not_counted" {
							cur.Action = "publish"
						}
						tx.PutReview(cur)
					}
				}
			}
			ps.BurstSince, ps.BurstCleared = time.Time{}, now
			tx.PutPState(ps)
		}
		for _, s := range sanctions {
			g.sanction(tx, c, s, m, note)
		}
		c.Status, c.Decision, c.Note, c.DecidedBy, c.DecidedAt, c.Applied = "decided", decision, strings.TrimSpace(note), m.Email, now, sanctions
		tx.PutCase(c)
		tx.Audit(model.AuditEntry{Actor: m.Email, ActorKind: m.Kind, Action: "case_" + decision, Target: c.ID, Note: note, PolicyVersion: g.P.Version})
		out = c
		return nil
	})
	return out, err
}

// settle sets a held review's outcome from a case decision.
func (g *Guard) settle(r *model.Review, c model.ReviewCase, decision string) {
	publish := func(counted bool) {
		r.Status, r.Counted = "published", counted
		r.Action = "publish"
		if !counted {
			r.Action = "not_counted"
		}
		r.Shown = g.mask(r.Text)
		if r.Shown != r.Text {
			r.Action = "mask_publish"
		}
	}
	if c.Kind == "report" { // approve keeps the review; dismiss means the report was right
		if decision == "dismiss" {
			r.Status, r.Counted, r.Action, r.Shown = "removed", false, "remove", ""
		}
		return
	}
	if decision == "dismiss" { // the evidence didn't hold up
		publish(r.Verified)
		return
	}
	switch c.Suggested {
	case "remove":
		r.Status, r.Counted, r.Action, r.Shown = "removed", false, "remove", ""
	case "reject":
		r.Status, r.Counted, r.Action, r.Shown = "rejected", false, "reject", ""
	case "keep_out":
		publish(false)
	default:
		publish(r.Verified && !untrusted(*r))
	}
}

func (g *Guard) mask(text string) string {
	c := AnalyzeContent(text, "", g.P)
	return Mask(c.T, c.Signals)
}

// onlyBurst: the burst is the only behaviour signal that kept the review from counting.
func onlyBurst(r model.Review) bool {
	for _, s := range r.Signals {
		if s.Kind == "authenticity" && s.Name != "burst" && s.Strength != "weak" {
			return false
		}
	}
	return true
}

// untrusted: any strong or medium behaviour signal on the review.
func untrusted(r model.Review) bool {
	for _, s := range r.Signals {
		if s.Kind == "authenticity" && (s.Strength == "strong" || s.Strength == "medium") {
			return true
		}
	}
	return false
}

func (g *Guard) sanction(tx *store.Txn, c model.ReviewCase, kind string, m Moderator, note string) {
	add := func(target string) {
		tx.PutSanction(model.Sanction{ID: tx.NewID("Z"), Kind: kind, Target: target, CaseID: c.ID, By: m.Email, Note: note, At: g.now()})
		tx.Audit(model.AuditEntry{Actor: m.Email, ActorKind: m.Kind, Action: "sanction_" + kind, Target: target, Note: c.ID, PolicyVersion: g.P.Version})
	}
	switch kind {
	case "seller_strike":
		add(c.Seller)
	case "delist":
		add(c.ProductID)
	case "rating_freeze":
		ps := tx.PState(c.ProductID)
		var rs []model.Review
		for _, r := range tx.AllReviews() {
			if r.ProductID == c.ProductID {
				cur, _ := tx.Review(r.ID)
				rs = append(rs, cur)
			}
		}
		rt := ComputeRating(c.ProductID, g.categoryMeanTx(tx, c.ProductID), rs, model.ProductReviewState{})
		ps.Frozen, ps.FrozenRating, ps.FrozenN = true, rt.Shown, rt.N
		tx.PutPState(ps)
		add(c.ProductID)
	case "customer_ban":
		seen := map[string]bool{}
		for _, rid := range c.ReviewIDs {
			if r, ok := tx.Review(rid); ok && untrusted(r) && !seen[r.CustomerID] {
				seen[r.CustomerID] = true
				add(r.CustomerID)
			}
		}
	}
}

func (g *Guard) categoryMeanTx(tx *store.Txn, pid string) float64 {
	p, ok := tx.Product(pid)
	if !ok {
		return 4
	}
	sum, n := 0.0, 0
	for _, id := range catalogIDs {
		if x, ok := tx.Product(id); ok && x.Category == p.Category {
			sum += x.Rating
			n++
		}
	}
	if n == 0 {
		return 4
	}
	return sum / float64(n)
}

// ---------------- appeals ----------------

// AuthorAppeal: the author asks a second person to look at their review.
func (g *Guard) AuthorAppeal(cid, rid, text string) (model.Appeal, error) {
	if err := checkNote(text); err != nil {
		return model.Appeal{}, err
	}
	var out model.Appeal
	err := g.S.Tx(func(tx *store.Txn) error {
		r, ok := tx.Review(rid)
		if !ok || r.CustomerID != cid {
			return ErrNotYours
		}
		if !(r.Status == "rejected" || r.Status == "removed" || (r.Status == "published" && !r.Counted)) {
			return ErrNotAppealable
		}
		for _, a := range tx.AllAppeals() {
			if contains(a.ReviewIDs, rid) && a.ByRole == "author" {
				return ErrNotAppealable
			}
		}
		out = model.Appeal{ID: tx.NewID("A"), By: cid, ByRole: "author", ReviewIDs: []string{rid}, ProductID: r.ProductID, Text: strings.TrimSpace(text),
			Status: "open", FirstDecider: r.DecidedBy, CreatedAt: g.now()}
		tx.PutAppeal(out)
		tx.Audit(model.AuditEntry{Actor: cid, ActorKind: "customer", Action: "appeal_opened", Target: rid, Note: out.ID})
		return nil
	})
	return out, err
}

// ReasonGroup describes, in general terms, why a review doesn't count (what sellers see).
func ReasonGroup(r model.Review) string {
	has := func(ids ...string) bool {
		for _, id := range ids {
			if contains(r.Rules, id) {
				return true
			}
		}
		return false
	}
	switch {
	case has("RG-A3"):
		return "paid or incentivised review"
	case has("RG-A2", "RG-A8", "RG-A9"):
		return "linked accounts (shared devices, payments, addresses or networks)"
	case has("RG-A7"):
		return "part of a review burst"
	case has("RG-A4", "RG-A6", "RG-A5"):
		return "unusual timing or copied wording"
	case has("RG-A1"):
		return "no delivered order"
	case r.Status == "rejected":
		return "content policy"
	}
	return "under review"
}

// SellerAppeal: a seller asks a second person to look at reviews of its product
// that stopped counting for one reason group. Sellers never see who wrote them.
func (g *Guard) SellerAppeal(seller, pid, reason, text string) (model.Appeal, error) {
	if err := checkNote(text); err != nil {
		return model.Appeal{}, err
	}
	var out model.Appeal
	err := g.S.Tx(func(tx *store.Txn) error {
		if g.productSeller(tx, pid) != seller {
			return ErrNotYours
		}
		var ids []string
		first := ""
		for _, r := range tx.AllReviews() {
			if r.ProductID == pid && !r.Counted && r.Status != "pending" && r.Status != "held" && ReasonGroup(r) == reason {
				ids = append(ids, r.ID)
				first = r.DecidedBy
			}
		}
		if len(ids) == 0 {
			return ErrNotAppealable
		}
		for _, a := range tx.AllAppeals() {
			if a.ByRole == "seller" && a.ProductID == pid && a.Reason == reason && a.Status == "open" {
				return ErrNotAppealable
			}
		}
		out = model.Appeal{ID: tx.NewID("A"), By: seller, ByRole: "seller", ReviewIDs: ids, ProductID: pid, Reason: reason, Text: strings.TrimSpace(text),
			Status: "open", FirstDecider: first, CreatedAt: g.now()}
		tx.PutAppeal(out)
		tx.Audit(model.AuditEntry{Actor: seller, ActorKind: "seller", Action: "appeal_opened", Target: pid, Note: out.ID + ": " + reason})
		return nil
	})
	return out, err
}

// DecideAppeal: a second moderator upholds or overturns.
func (g *Guard) DecideAppeal(id string, m Moderator, decision, note string) (model.Appeal, error) {
	if decision != "uphold" && decision != "overturn" {
		return model.Appeal{}, ErrBadDecision
	}
	if err := checkNote(note); err != nil {
		return model.Appeal{}, err
	}
	var out model.Appeal
	err := g.S.Tx(func(tx *store.Txn) error {
		a, ok := tx.Appeal(id)
		if !ok {
			return ErrNotYours
		}
		if a.Status != "open" {
			return ErrClosed
		}
		if strings.EqualFold(a.FirstDecider, m.Email) {
			return ErrSecondModerator
		}
		if m.Seller != "" && m.Seller == g.productSeller(tx, a.ProductID) {
			return ErrConflict
		}
		now := g.now()
		if decision == "overturn" {
			for _, rid := range a.ReviewIDs {
				r, ok := tx.Review(rid)
				if !ok {
					continue
				}
				r.Status, r.Counted = "published", r.Verified
				if a.ByRole == "author" {
					r.Counted = r.Verified && !untrusted(r)
				}
				r.Shown, r.Action = g.mask(r.Text), "publish"
				if r.Shown != r.Text {
					r.Action = "mask_publish"
				}
				r.DecidedBy, r.DecidedAt, r.Version = m.Email, now, r.Version+1
				tx.PutReview(r)
				tx.Audit(model.AuditEntry{Actor: m.Email, ActorKind: m.Kind, Action: "review_restored_on_appeal", Target: r.ID, Note: a.ID,
					Tagger: r.Tagger, Model: r.Model, PromptVersion: r.PromptVersion, PolicyVersion: r.PolicyVersion})
			}
			a.Status = "overturned"
		} else {
			a.Status = "upheld"
		}
		a.DecidedBy, a.DeciderKind, a.Note, a.DecidedAt = m.Email, m.Kind, strings.TrimSpace(note), now
		tx.PutAppeal(a)
		tx.Audit(model.AuditEntry{Actor: m.Email, ActorKind: m.Kind, Action: "appeal_" + a.Status, Target: a.ID, Note: note, PolicyVersion: g.P.Version})
		out = a
		return nil
	})
	return out, err
}

// DemoAppealDecision is what the simulated second moderator would do: overturn when
// the original decision rested only on the model's judgment or weak signals,
// uphold when a rule detector or a strong behaviour signal backs it.
func (g *Guard) DemoAppealDecision(id string) (string, string) {
	for _, a := range g.S.Appeals() {
		if a.ID != id {
			continue
		}
		for _, rid := range a.ReviewIDs {
			r, _ := g.S.Review(rid)
			for _, s := range r.Signals {
				if s.Strength == "strong" {
					return "uphold", "Demo: re-checked the evidence; a rule detector or strong behaviour signal (" + s.Name + ") backs the original decision."
				}
			}
		}
		return "overturn", "Demo: re-checked the evidence; nothing beyond weak signals or the model's own judgment backs it, so the review is restored."
	}
	return "uphold", "Demo: appeal not found."
}

// Report: anyone (including a seller) can report a published review. A report
// never hides or un-counts a review by itself: it only asks a person to look.
func (g *Guard) Report(by, byRole, reporterSeller, rid, reason string) (model.ReviewCase, error) {
	if err := checkNote(reason); err != nil {
		return model.ReviewCase{}, err
	}
	var out model.ReviewCase
	err := g.S.Tx(func(tx *store.Txn) error {
		r, ok := tx.Review(rid)
		if !ok || r.Status != "published" {
			return ErrNotYours
		}
		ev := fmt.Sprintf("reported by a %s: %s", byRole, trunc(strings.TrimSpace(reason), 200))
		if reporterSeller != "" && reporterSeller != r.Seller {
			ev += " (the reporter is a competing seller; a report never changes a review on its own)"
		}
		c, ok := tx.OpenCaseByKey("report:" + rid)
		if !ok {
			p, _ := tx.Product(r.ProductID)
			c = model.ReviewCase{ID: tx.NewID("K"), Key: "report:" + rid, Kind: "report", Title: "Reported review on " + p.Name, Seller: r.Seller,
				ProductID: r.ProductID, ReviewIDs: []string{rid}, Severity: "low", Suggested: "publish", Status: "open", CreatedAt: g.now(), Synthetic: r.Synthetic}
		}
		if len(c.Evidence) < 20 {
			c.Evidence = append(c.Evidence, ev)
		}
		c.Reach = len(c.Evidence)
		tx.PutCase(c)
		tx.Audit(model.AuditEntry{Actor: by, ActorKind: byRole, Action: "review_reported", Target: rid, Note: c.ID})
		out = c
		return nil
	})
	return out, err
}

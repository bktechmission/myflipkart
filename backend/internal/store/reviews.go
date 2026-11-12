// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// ReviewGuard records in the same write-ahead log as orders and refunds: reviews,
// identities, seller payouts, moderation cases, appeals, sanctions, per-product
// review state and the audit log. Every change goes through Store.Tx, so a review
// is durable (fsync'd) before anyone is told it was received, and a state change
// re-checked under the lock happens exactly once.

package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"asksauby/internal/model"
)

const maxAudit = 5000 // audit entries kept in memory and in the compacted snapshot

type reviewState struct {
	reviews    map[string]model.Review
	idem       map[string]string // cid|idempotency key -> review id
	identities map[string]model.Identity
	payouts    map[string]model.Payout
	cases      map[string]model.ReviewCase
	appeals    map[string]model.Appeal
	sanctions  map[string]model.Sanction
	pstate     map[string]model.ProductReviewState
	audit      []model.AuditEntry
	seq        int
}

func newReviewState() *reviewState {
	return &reviewState{reviews: map[string]model.Review{}, idem: map[string]string{}, identities: map[string]model.Identity{},
		payouts: map[string]model.Payout{}, cases: map[string]model.ReviewCase{}, appeals: map[string]model.Appeal{},
		sanctions: map[string]model.Sanction{}, pstate: map[string]model.ProductReviewState{}}
}

// bump keeps the id sequence above every id seen (ids are a letter prefix + digits).
func (r *reviewState) bump(id string) {
	if n, err := strconv.Atoi(strings.TrimLeft(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")); err == nil && n > r.seq {
		r.seq = n
	}
}

func (r *reviewState) apply(x rec) bool {
	switch x.Kind {
	case "review":
		v := *x.Review
		r.reviews[v.ID] = v
		if v.IdemKey != "" {
			r.idem[v.CustomerID+"|"+v.IdemKey] = v.ID
		}
		r.bump(v.ID)
	case "rv_case":
		r.cases[x.RCase.ID] = *x.RCase
		r.bump(x.RCase.ID)
	case "rv_appeal":
		r.appeals[x.Appeal.ID] = *x.Appeal
		r.bump(x.Appeal.ID)
	case "rv_sanction":
		r.sanctions[x.Sanction.ID] = *x.Sanction
		r.bump(x.Sanction.ID)
	case "identity":
		r.identities[x.Identity.Subject] = *x.Identity
	case "payout":
		r.payouts[x.Payout.ID] = *x.Payout
		r.bump(x.Payout.ID)
	case "rv_pstate":
		r.pstate[x.PState.ProductID] = *x.PState
	case "audit":
		r.audit = append(r.audit, *x.Audit)
		if len(r.audit) > maxAudit*2 {
			r.audit = append([]model.AuditEntry(nil), r.audit[len(r.audit)-maxAudit:]...)
		}
		r.bump(x.Audit.ID)
	case "rv_reset": // demo reset: review data goes, the audit log stays
		a := r.audit
		*r = *newReviewState()
		r.audit = a
	default:
		return false
	}
	return true
}

func (r *reviewState) snapshot() []rec {
	var out []rec
	for _, v := range r.identities {
		v := v
		out = append(out, rec{Kind: "identity", Identity: &v})
	}
	for _, v := range r.payouts {
		v := v
		out = append(out, rec{Kind: "payout", Payout: &v})
	}
	for _, v := range r.reviews {
		v := v
		out = append(out, rec{Kind: "review", Review: &v})
	}
	for _, v := range r.cases {
		v := v
		out = append(out, rec{Kind: "rv_case", RCase: &v})
	}
	for _, v := range r.appeals {
		v := v
		out = append(out, rec{Kind: "rv_appeal", Appeal: &v})
	}
	for _, v := range r.sanctions {
		v := v
		out = append(out, rec{Kind: "rv_sanction", Sanction: &v})
	}
	for _, v := range r.pstate {
		v := v
		out = append(out, rec{Kind: "rv_pstate", PState: &v})
	}
	a := r.audit
	if len(a) > maxAudit {
		a = a[len(a)-maxAudit:]
	}
	for _, v := range a {
		v := v
		out = append(out, rec{Kind: "audit", Audit: &v})
	}
	return out
}

// ---------------- inside a transaction ----------------

// NewID allocates the next ReviewGuard id with a one-letter prefix (R review, K case, A appeal...).
func (t *Txn) NewID(prefix string) string {
	t.s.rv.seq++
	return fmt.Sprintf("%s%06d", prefix, t.s.rv.seq)
}

func (t *Txn) Review(id string) (model.Review, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if v := t.recs[i].Review; v != nil && v.ID == id {
			return *v, true
		}
	}
	v, ok := t.s.rv.reviews[id]
	return v, ok
}

// ReviewByIdem finds a customer's review by its idempotency key (staged or committed).
func (t *Txn) ReviewByIdem(cid, key string) (model.Review, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if v := t.recs[i].Review; v != nil && v.CustomerID == cid && v.IdemKey == key {
			return *v, true
		}
	}
	if id, ok := t.s.rv.idem[cid+"|"+key]; ok {
		return t.Review(id)
	}
	return model.Review{}, false
}

// ReviewFor finds the customer's existing review of a product on one order.
func (t *Txn) ReviewFor(cid, orderID, productID string) (model.Review, bool) {
	for _, v := range t.s.rv.reviews {
		if v.CustomerID == cid && v.OrderID == orderID && v.ProductID == productID && v.Status != "removed" {
			return v, true
		}
	}
	return model.Review{}, false
}

func (t *Txn) Case(id string) (model.ReviewCase, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if v := t.recs[i].RCase; v != nil && v.ID == id {
			return *v, true
		}
	}
	v, ok := t.s.rv.cases[id]
	return v, ok
}

// OpenCaseByKey returns the open case for a grouping key, so related reviews join one case.
func (t *Txn) OpenCaseByKey(key string) (model.ReviewCase, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if v := t.recs[i].RCase; v != nil && v.Key == key && v.Status == "open" {
			return *v, true
		}
	}
	for _, v := range t.s.rv.cases {
		if v.Key == key && v.Status == "open" {
			if staged, ok := t.Case(v.ID); ok && staged.Status != "open" {
				continue
			}
			return v, true
		}
	}
	return model.ReviewCase{}, false
}

func (t *Txn) Appeal(id string) (model.Appeal, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if v := t.recs[i].Appeal; v != nil && v.ID == id {
			return *v, true
		}
	}
	v, ok := t.s.rv.appeals[id]
	return v, ok
}

func (t *Txn) PState(pid string) model.ProductReviewState {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if v := t.recs[i].PState; v != nil && v.ProductID == pid {
			return *v
		}
	}
	if v, ok := t.s.rv.pstate[pid]; ok {
		return v
	}
	return model.ProductReviewState{ProductID: pid}
}

func (t *Txn) PutReview(v model.Review)   { t.recs = append(t.recs, rec{Kind: "review", Review: &v}) }
func (t *Txn) PutCase(v model.ReviewCase) { t.recs = append(t.recs, rec{Kind: "rv_case", RCase: &v}) }
func (t *Txn) PutAppeal(v model.Appeal)   { t.recs = append(t.recs, rec{Kind: "rv_appeal", Appeal: &v}) }
func (t *Txn) PutSanction(v model.Sanction) {
	t.recs = append(t.recs, rec{Kind: "rv_sanction", Sanction: &v})
}
func (t *Txn) PutIdentity(v model.Identity) {
	t.recs = append(t.recs, rec{Kind: "identity", Identity: &v})
}
func (t *Txn) PutPayout(v model.Payout) { t.recs = append(t.recs, rec{Kind: "payout", Payout: &v}) }
func (t *Txn) PutPState(v model.ProductReviewState) {
	t.recs = append(t.recs, rec{Kind: "rv_pstate", PState: &v})
}
func (t *Txn) Audit(e model.AuditEntry) {
	e.ID, e.TS = t.NewID("L"), time.Now().UTC()
	t.recs = append(t.recs, rec{Kind: "audit", Audit: &e})
}
func (t *Txn) CustomerBanned(cid string) bool        { return t.s.bannedLocked(cid) }
func (t *Txn) Sanctions() map[string]model.Sanction  { return t.s.rv.sanctions }
func (t *Txn) AllCases() map[string]model.ReviewCase { return t.s.rv.cases }
func (t *Txn) AllReviews() map[string]model.Review   { return t.s.rv.reviews }
func (t *Txn) AllAppeals() map[string]model.Appeal   { return t.s.rv.appeals }
func (t *Txn) PutSyntheticOrder(o model.Order) {
	t.recs = append(t.recs, rec{Kind: "order", Order: &o})
}
func (t *Txn) Identity(sub string) (model.Identity, bool) {
	v, ok := t.s.rv.identities[sub]
	return v, ok
}

func (s *Store) bannedLocked(cid string) bool {
	for _, x := range s.rv.sanctions {
		if x.Kind == "customer_ban" && x.Target == cid && !x.Lifted {
			return true
		}
	}
	return false
}

// ---------------- reads ----------------

// ReviewData is a consistent read-only copy of everything ReviewGuard's detectors look at.
type ReviewData struct {
	Reviews    []model.Review
	Identities map[string]model.Identity
	Payouts    []model.Payout
	Orders     map[string]model.Order
	Users      map[string]model.User
	PState     map[string]model.ProductReviewState
}

// ReviewSnapshot copies review, identity, payout and order data under one read lock.
func (s *Store) ReviewSnapshot() ReviewData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := ReviewData{Identities: make(map[string]model.Identity, len(s.rv.identities)), Orders: make(map[string]model.Order, len(s.orders)),
		Users: make(map[string]model.User, len(s.users)), PState: make(map[string]model.ProductReviewState, len(s.rv.pstate))}
	for _, v := range s.rv.reviews {
		d.Reviews = append(d.Reviews, v)
	}
	sort.Slice(d.Reviews, func(i, j int) bool {
		if !d.Reviews[i].CreatedAt.Equal(d.Reviews[j].CreatedAt) {
			return d.Reviews[i].CreatedAt.Before(d.Reviews[j].CreatedAt)
		}
		return d.Reviews[i].ID < d.Reviews[j].ID
	})
	for k, v := range s.rv.identities {
		d.Identities[k] = v
	}
	for _, v := range s.rv.payouts {
		d.Payouts = append(d.Payouts, v)
	}
	sort.Slice(d.Payouts, func(i, j int) bool { return d.Payouts[i].ID < d.Payouts[j].ID })
	for k, v := range s.orders {
		d.Orders[k] = v
	}
	for k, v := range s.users {
		d.Users[k] = v
	}
	for k, v := range s.rv.pstate {
		d.PState[k] = v
	}
	return d
}

// Reviews returns reviews matching keep (nil = all), oldest first.
func (s *Store) Reviews(keep func(model.Review) bool) []model.Review {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Review
	for _, v := range s.rv.reviews {
		if keep == nil || keep(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) Review(id string) (model.Review, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.rv.reviews[id]
	return v, ok
}

func (s *Store) ReviewCases() []model.ReviewCase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.ReviewCase, 0, len(s.rv.cases))
	for _, v := range s.rv.cases {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) ReviewCase(id string) (model.ReviewCase, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.rv.cases[id]
	return v, ok
}

func (s *Store) Appeals() []model.Appeal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Appeal, 0, len(s.rv.appeals))
	for _, v := range s.rv.appeals {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Sanctions() []model.Sanction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Sanction, 0, len(s.rv.sanctions))
	for _, v := range s.rv.sanctions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) ProductReviewState(pid string) model.ProductReviewState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.rv.pstate[pid]; ok {
		return v
	}
	return model.ProductReviewState{ProductID: pid}
}

// Delisted reports whether a person delisted the product (hidden from search, no new orders).
func (s *Store) Delisted(pid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, x := range s.rv.sanctions {
		if x.Kind == "delist" && x.Target == pid && !x.Lifted {
			return true
		}
	}
	return false
}

// SellerStrikes counts active review-fraud strikes a person gave a seller.
func (s *Store) SellerStrikes(seller string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, x := range s.rv.sanctions {
		if x.Kind == "seller_strike" && x.Target == seller && !x.Lifted {
			n++
		}
	}
	return n
}

func (s *Store) CustomerBanned(cid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bannedLocked(cid)
}

// AuditLog returns the newest n audit entries, newest first.
func (s *Store) AuditLog(n int) []model.AuditEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.rv.audit
	out := []model.AuditEntry{}
	for i := len(a) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, a[i])
	}
	return out
}

// ResetReviews clears ReviewGuard demo data (reviews, cases, appeals, sanctions,
// identities, payouts and synthetic shoppers' orders). The audit log is kept and
// records the reset.
func (s *Store) ResetReviews(by string) error {
	return s.Tx(func(t *Txn) error {
		for id, o := range s.orders {
			if IsSyntheticCID(o.CustomerID) {
				_ = id
				o.Status = "deleted"
				t.recs = append(t.recs, rec{Kind: "order_delete", Order: &o})
			}
		}
		t.recs = append(t.recs, rec{Kind: "rv_reset"})
		t.Audit(model.AuditEntry{Actor: by, ActorKind: "human", Action: "reset_review_demo", Target: "all"})
		return nil
	})
}

// IsSyntheticCID reports whether a customer id belongs to a synthetic demo shopper.
func IsSyntheticCID(cid string) bool { return strings.HasPrefix(cid, "S") }

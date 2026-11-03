// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Customer and seller standing. Loyal customers get a smoother path; customers
// with a pattern of returns, rejected requests or missing-item claims go to a
// person every time. Sellers with poor ratings, many returns or repeated
// complaints are flagged, and suspended sellers can't sell through Sauby.

package agent

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/seed"
	"asksauby/internal/store"
)

// Standing summarizes a customer's history for guardrails and for the teammate.
type Standing struct {
	Tier       string   `json:"tier"` // loyal | standard | watch | blocked
	Orders     int      `json:"orders"`
	Returns    int      `json:"returns"`     // returns, refunds and replacements on record
	Returns30  int      `json:"returns_30d"` // returns started in the last 30 days
	ReturnRate float64  `json:"return_rate"`
	Rejected   int      `json:"rejected"` // requests a teammate declined (90 days)
	Claims     int      `json:"claims"`   // missing-item claims (90 days)
	PhotoOver  float64  `json:"photo_over"`
	Reasons    []string `json:"reasons"`
}

func isReturned(o model.Order) bool {
	switch o.Status {
	case "return_requested", "return_received", "return_pending_approval", "refund_pending_approval", "replacement_requested":
		return true
	case "refunded":
		return o.RMA != "" // refunded after a return (not a cancellation)
	}
	return false
}

func (t *Tools) standingTx(tx *store.Txn, cid string) Standing {
	st := Standing{}
	got := 0 // orders the customer actually received
	for _, o := range tx.OrdersFor(cid) {
		if o.Status == "cancelled" {
			continue
		}
		st.Orders++
		if o.DeliveredOn != "" || isReturned(o) {
			got++
		}
		if isReturned(o) {
			st.Returns++
		}
		if !o.ReturnStartedAt.IsZero() && time.Since(o.ReturnStartedAt) < 30*24*time.Hour {
			st.Returns30++
		}
	}
	for _, a := range tx.AllApprovals() {
		if a.CustomerID != cid || time.Since(a.CreatedAt) > 90*24*time.Hour {
			continue
		}
		if a.Status == "rejected" {
			st.Rejected++
		}
		if a.NoReturn {
			st.Claims++
		}
	}
	if got > 0 {
		st.ReturnRate = math.Round(float64(st.Returns)/float64(got)*100) / 100
	}
	strikes := st.Rejected + st.Claims
	switch {
	case strikes >= 3 || (st.Returns >= 4 && st.ReturnRate >= 0.6):
		st.Tier = "blocked"
	case strikes >= 2 || (st.Returns >= 3 && st.ReturnRate >= 0.4):
		st.Tier = "watch"
	case got >= 5 && st.ReturnRate < 0.25 && strikes == 0:
		st.Tier = "loyal"
	default:
		st.Tier = "standard"
	}
	if st.Rejected > 0 {
		st.Reasons = append(st.Reasons, fmt.Sprintf("%d declined request(s) in 90 days", st.Rejected))
	}
	if st.Claims > 0 {
		st.Reasons = append(st.Reasons, fmt.Sprintf("%d missing-item claim(s) in 90 days", st.Claims))
	}
	if st.Returns > 0 {
		st.Reasons = append(st.Reasons, fmt.Sprintf("returned %d of %d received orders (%.0f%%)", st.Returns, got, st.ReturnRate*100))
	}
	st.PhotoOver = map[string]float64{"loyal": 300, "standard": 150}[st.Tier] // 0: always ask for a photo
	return st
}

// CustomerStanding is the read-only view for the agent console.
func (t *Tools) CustomerStanding(cid string) Standing {
	var st Standing
	t.S.Tx(func(tx *store.Txn) error { st = t.standingTx(tx, cid); return nil })
	return st
}

// SellerStanding is a marketplace seller's scorecard.
type SellerStanding struct {
	Seller     string   `json:"seller"`
	Products   int      `json:"products"`
	AvgRating  float64  `json:"avg_rating"`
	Orders     int      `json:"orders"`
	Returns    int      `json:"returns"`
	ReturnRate float64  `json:"return_rate"`
	Complaints int      `json:"complaints"`
	Status     string   `json:"status"` // good | watch | suspended
	Reasons    []string `json:"reasons"`
}

// Sellers scores every seller from catalog ratings, returns on their items, and complaints.
func (t *Tools) Sellers() []SellerStanding {
	by := map[string]*SellerStanding{}
	sellerOf := map[string]string{}
	get := func(name string) *SellerStanding {
		if by[name] == nil {
			by[name] = &SellerStanding{Seller: name}
		}
		return by[name]
	}
	for _, p := range t.S.Products() {
		name := seed.SellerFor(p.Category)
		sellerOf[p.ID] = name
		s := get(name)
		s.Products++
		s.AvgRating += p.Rating
	}
	for _, o := range t.S.AllOrders() {
		if o.Status == "cancelled" || o.ReplacementFor != "" || len(o.Items) == 0 {
			continue
		}
		s := get(sellerOf[o.Items[0]])
		s.Orders++
		if isReturned(o) {
			s.Returns++
		}
	}
	for _, k := range t.S.Tickets() {
		if k.Topic != "seller_complaint" {
			continue
		}
		for name, s := range by {
			if strings.Contains(k.Summary, name) {
				s.Complaints++
			}
		}
	}
	out := make([]SellerStanding, 0, len(by))
	for _, s := range by {
		if s.Products > 0 {
			s.AvgRating = math.Round(s.AvgRating/float64(s.Products)*10) / 10
		}
		if s.Orders > 0 {
			s.ReturnRate = math.Round(float64(s.Returns)/float64(s.Orders)*100) / 100
		}
		s.Status = "good"
		if s.Complaints > 0 {
			s.Reasons = append(s.Reasons, fmt.Sprintf("%d complaint(s)", s.Complaints))
		}
		if s.Orders >= 3 && s.ReturnRate >= 0.3 {
			s.Reasons = append(s.Reasons, fmt.Sprintf("%.0f%% of orders returned", s.ReturnRate*100))
		}
		if s.AvgRating > 0 && s.AvgRating < 3.9 {
			s.Reasons = append(s.Reasons, fmt.Sprintf("low ratings (%.1f)", s.AvgRating))
		}
		switch {
		case s.Complaints >= 2 || (s.Orders >= 4 && s.ReturnRate >= 0.5) || (s.AvgRating > 0 && s.AvgRating < 3.5):
			s.Status = "suspended"
		case len(s.Reasons) > 0:
			s.Status = "watch"
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seller < out[j].Seller })
	return out
}

func (t *Tools) suspendedSellers() map[string]bool {
	m := map[string]bool{}
	for _, s := range t.Sellers() {
		if s.Status == "suspended" {
			m[s.Seller] = true
		}
	}
	return m
}

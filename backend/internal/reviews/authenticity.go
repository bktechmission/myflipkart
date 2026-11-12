// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Authenticity: is this a real customer's own opinion? Decided only from
// behaviour (orders, delivery times, accounts, devices, payments, timing),
// never from whether the review is positive or negative.

package reviews

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/store"
)

// Thresholds, in one place so tests and docs can name them.
const (
	PaidWindow       = 72 * time.Hour      // payout after a 5★ review
	TooFastWindow    = 6 * time.Hour       // "used it for months", written this soon after delivery
	NewAccountAge    = 14 * 24 * time.Hour // "new" account
	TemplatedJacc    = 0.5                 // 3-word shingle overlap with another account
	DuplicateJacc    = 0.8                 // same author, near-identical text
	BurstWindow      = 24 * time.Hour      // reviews counted in a burst
	BurstMinReviews  = 6                   // fewer than this is never a burst
	BurstRatio       = 1.5                 // reviews >= ratio x recent deliveries
	DeliveryWindow   = 14 * 24 * time.Hour // deliveries that could explain the reviews
	RingMinPeers     = 2                   // other accounts sharing >= 2 sellers with this one
	RingMaxAge       = 30 * 24 * time.Hour // ring members are young accounts
	TemplateLookback = 30 * 24 * time.Hour
)

// Auth is what the behaviour checks found.
type Auth struct {
	Verified     bool
	Signals      []model.Signal
	LinkedSeller string // seller the evidence points at (own seller or a rival)
	Rival        bool
	Burst        bool
}

// Catalog resolves a product and its seller.
type Catalog func(pid string) (model.Product, string, bool)

// Assess runs every behaviour check for review r against a consistent snapshot.
func Assess(r model.Review, d store.ReviewData, cat Catalog, p *Policy) Auth {
	a := Auth{}
	add := func(name, strength, detail string) {
		a.Signals = append(a.Signals, model.Signal{Name: name, Rule: p.ID(name), Kind: "authenticity", Strength: strength, Detail: detail, Start: -1, End: -1})
	}
	prod, seller, _ := cat(r.ProductID)
	at := r.CreatedAt

	// verified purchase: this customer's own delivered order containing this product
	o, ok := d.Orders[r.OrderID]
	if ok && o.CustomerID == r.CustomerID && hasItem(o, r.ProductID) && delivered(o) {
		a.Verified = true
	} else {
		why := "no delivered order of this product by this account"
		if ok && o.CustomerID == r.CustomerID && hasItem(o, r.ProductID) {
			why = "order is " + o.Status + ", not delivered"
		}
		add("unverified_purchase", "medium", why)
	}

	// shared device, card, address or network with seller staff
	me, hasID := d.Identities[r.CustomerID]
	if hasID {
		for _, st := range sortedStaff(d.Identities) {
			shared := sharedWith(me, st)
			if len(shared) == 0 {
				continue
			}
			strong := contains(shared, "device") || contains(shared, "payment card")
			strength := "medium"
			if strong {
				strength = "strong"
			}
			switch {
			case st.Seller == seller:
				if !hasSignal(a.Signals, "seller_linked") {
					add("seller_linked", strength, fmt.Sprintf("shares %s with %s staff", strings.Join(shared, " and "), seller))
					a.LinkedSeller = seller
				}
			case r.Stars <= 2 && sameCategory(cat, d, st.Seller, prod.Category):
				if !hasSignal(a.Signals, "rival_attack") {
					add("rival_attack", "strong", fmt.Sprintf("%d★ from an account that shares %s with rival seller %s", r.Stars, strings.Join(shared, " and "), st.Seller))
					a.LinkedSeller, a.Rival = st.Seller, true
				}
			}
		}
	}

	// paid review: the seller refunded or paid cashback within 3 days after a 5★ review
	if r.Stars == 5 {
		for _, py := range d.Payouts {
			if py.Seller == seller && py.CustomerID == r.CustomerID && !py.At.Before(at) && py.At.Sub(at) <= PaidWindow {
				add("paid_review", "strong", fmt.Sprintf("%s paid $%.0f %s %s after this 5★ review", seller, py.Amount, py.Kind, humanDur(py.At.Sub(at))))
				if a.LinkedSeller == "" {
					a.LinkedSeller = seller
				}
				break
			}
		}
	}

	// too fast: describes long use minutes or hours after delivery
	if a.Verified && ClaimsLongUse(r.Text) {
		if !o.DeliveredAt.IsZero() && at.Sub(o.DeliveredAt) < TooFastWindow && at.After(o.DeliveredAt.Add(-time.Minute)) {
			add("too_fast", "medium", fmt.Sprintf("describes weeks or months of use %s after delivery", humanDur(at.Sub(o.DeliveredAt))))
		} else if o.DeliveredAt.IsZero() && o.DeliveredOn == at.Format("2006-01-02") {
			add("too_fast", "medium", "describes weeks or months of use on the day it was delivered")
		}
	}

	// new account that only ever deals with one seller
	created := accountCreated(r.CustomerID, d, me, hasID)
	if !created.IsZero() && at.Sub(created) < NewAccountAge {
		sellers := map[string]bool{seller: true}
		for _, x := range d.Orders {
			if x.CustomerID == r.CustomerID {
				for _, it := range x.Items {
					if _, s, ok := cat(it); ok {
						sellers[s] = true
					}
				}
			}
		}
		for _, x := range d.Reviews {
			if x.CustomerID == r.CustomerID {
				sellers[x.Seller] = true
			}
		}
		if len(sellers) == 1 {
			add("new_single_seller", "weak", fmt.Sprintf("account is %s old and only deals with %s", humanDur(at.Sub(created)), seller))
		}
	}

	// templated wording across accounts, and the same author repeating themselves
	mine := Shingles(Normalize(r.Text))
	myNorm := normKey(r.Text)
	tmpl, dup := 0, 0
	for _, x := range d.Reviews {
		if x.ID == r.ID || x.Status == "removed" || at.Sub(x.CreatedAt) > TemplateLookback || x.CreatedAt.After(at) {
			continue
		}
		same := false
		j := Jaccard(mine, Shingles(Normalize(x.Text)))
		if len(strings.Fields(myNorm)) >= 4 && myNorm == normKey(x.Text) {
			same, j = true, 1
		}
		switch {
		case x.CustomerID == r.CustomerID && (j >= DuplicateJacc || same):
			dup++
		case x.CustomerID != r.CustomerID && x.Seller == seller && (j >= TemplatedJacc || same):
			tmpl++
		}
	}
	if tmpl > 0 {
		add("templated", "medium", fmt.Sprintf("wording nearly matches %d other account(s)' reviews of %s", tmpl, seller))
	}
	if dup > 0 {
		a.Signals = append(a.Signals, model.Signal{Name: "duplicate", Rule: p.ID("duplicate_spam"), Kind: "content", Strength: "medium",
			Detail: fmt.Sprintf("same author posted nearly the same text %d time(s) before", dup), Start: -1, End: -1})
	}

	// burst: reviews on this product outgrow its real deliveries
	ps := d.PState[r.ProductID]
	if !ps.BurstSince.IsZero() {
		a.Burst = true
		add("burst", "medium", "new reviews on this product are paused for a person to check a burst")
	} else {
		n := 1
		for _, x := range d.Reviews {
			if x.ProductID == r.ProductID && x.ID != r.ID && x.Status != "removed" && !x.CreatedAt.After(at) &&
				at.Sub(x.CreatedAt) <= BurstWindow && x.CreatedAt.After(ps.BurstCleared) {
				n++
			}
		}
		deliveries := 0
		for _, x := range d.Orders {
			if hasItem(x, r.ProductID) && delivered(x) {
				if t := deliveredTime(x); !t.After(at) && at.Sub(t) <= DeliveryWindow {
					deliveries++
				}
			}
		}
		if n >= BurstMinReviews && float64(n) >= BurstRatio*float64(max(deliveries, 1)) {
			a.Burst = true
			add("burst", "medium", fmt.Sprintf("%d reviews in 24 hours against %d deliveries in 14 days", n, deliveries))
		}
	}

	// review ring: young accounts that keep reviewing the same sellers together
	if ring := ringPeers(r, seller, d, created); ring >= RingMinPeers {
		add("review_ring", "strong", fmt.Sprintf("reviews the same sellers as %d other new accounts, always 1★ or 5★", ring))
	}
	return a
}

func ringPeers(r model.Review, seller string, d store.ReviewData, created time.Time) int {
	if created.IsZero() || r.CreatedAt.Sub(created) > RingMaxAge || (r.Stars != 1 && r.Stars != 5) {
		return 0
	}
	sellersOf := map[string]map[string]bool{}
	extremeOnly := map[string]bool{}
	for _, x := range d.Reviews {
		if x.Status == "removed" {
			continue
		}
		if sellersOf[x.CustomerID] == nil {
			sellersOf[x.CustomerID] = map[string]bool{}
			extremeOnly[x.CustomerID] = true
		}
		sellersOf[x.CustomerID][x.Seller] = true
		extremeOnly[x.CustomerID] = extremeOnly[x.CustomerID] && (x.Stars == 1 || x.Stars == 5)
	}
	mine := map[string]bool{seller: true}
	for s := range sellersOf[r.CustomerID] {
		mine[s] = true
	}
	if len(mine) < 2 {
		return 0
	}
	peers := 0
	for cid, ss := range sellersOf {
		if cid == r.CustomerID || !extremeOnly[cid] {
			continue
		}
		if c := accountCreated(cid, d, d.Identities[cid], true); c.IsZero() || r.CreatedAt.Sub(c) > RingMaxAge {
			continue
		}
		shared := 0
		for s := range ss {
			if mine[s] {
				shared++
			}
		}
		if shared >= 2 {
			peers++
		}
	}
	return peers
}

func sortedStaff(ids map[string]model.Identity) []model.Identity {
	var out []model.Identity
	for _, v := range ids {
		if v.Seller != "" {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

func sharedWith(a, b model.Identity) []string {
	var out []string
	if a.Device != "" && a.Device == b.Device {
		out = append(out, "device")
	}
	if a.Card != "" && a.Card == b.Card {
		out = append(out, "payment card")
	}
	if a.Address != "" && a.Address == b.Address {
		out = append(out, "address")
	}
	if a.Network != "" && a.Network == b.Network {
		out = append(out, "network")
	}
	return out
}

// sameCategory: does the other seller sell in this category (a rival)?
func sameCategory(cat Catalog, d store.ReviewData, other, category string) bool {
	for _, pid := range catalogIDs {
		if p, s, ok := cat(pid); ok && s == other && p.Category == category {
			return true
		}
	}
	return false
}

// catalogIDs lists the demo catalog (P1001-P1048) for rival lookups.
var catalogIDs = func() []string {
	var out []string
	for i := 1001; i <= 1048; i++ {
		out = append(out, fmt.Sprintf("P%d", i))
	}
	return out
}()

func accountCreated(cid string, d store.ReviewData, id model.Identity, hasID bool) time.Time {
	var c time.Time
	if hasID && !id.CreatedAt.IsZero() {
		c = id.CreatedAt
	} else if u, ok := d.Users[cid]; ok {
		c = u.CreatedAt
	}
	for _, o := range d.Orders { // an order placed earlier proves the account is older
		if o.CustomerID == cid {
			if t, err := time.Parse("2006-01-02", o.PlacedOn); err == nil && (c.IsZero() || t.Before(c)) {
				c = t
			}
		}
	}
	return c
}

func hasItem(o model.Order, pid string) bool {
	for _, it := range o.Items {
		if it == pid {
			return true
		}
	}
	return false
}

func delivered(o model.Order) bool {
	switch o.Status {
	case "delivered", "return_requested", "return_received", "refunded", "replacement_requested", "return_pending_approval", "refund_pending_approval":
		return o.DeliveredOn != "" || !o.DeliveredAt.IsZero()
	}
	return false
}

func deliveredTime(o model.Order) time.Time {
	if !o.DeliveredAt.IsZero() {
		return o.DeliveredAt
	}
	t, _ := time.Parse("2006-01-02", o.DeliveredOn)
	return t
}

func hasSignal(s []model.Signal, name string) bool {
	for _, x := range s {
		if x.Name == name {
			return true
		}
	}
	return false
}

func normKey(s string) string {
	var ws []string
	for _, w := range Normalize(s).Words {
		ws = append(ws, strings.ToLower(w.Text))
	}
	return strings.Join(ws, " ")
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "seconds"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

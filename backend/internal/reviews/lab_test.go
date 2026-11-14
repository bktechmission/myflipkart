// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// ReviewGuard Lab tests: every preset gets the action it promises, each author
// type produces its behaviour signal, and a lab run never writes to the store.
package reviews

import (
	"context"
	"testing"
)

func lab(t *testing.T, g *Guard, author string, stars int, text string) LabResult {
	t.Helper()
	res, err := g.Lab(context.Background(), LabInput{ProductID: FakeProduct, Stars: stars, Text: text, Author: author})
	if err != nil {
		t.Fatalf("lab %s: %v", author, err)
	}
	return res
}

func hasSig(res LabResult, name string) bool {
	for _, s := range res.Signals {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestLabPresetsGetTheirExpectedAction(t *testing.T) {
	g := New(mustStore(), nil, "rules")
	for _, p := range LabPresets {
		res := lab(t, g, p.Author, p.Stars, p.Text)
		if res.Action != p.Expect {
			t.Errorf("%s: action %s, want %s (rules %v, reasons %v)", p.Label, res.Action, p.Expect, res.Rules, res.Reasons)
		}
		if len(res.Stages) != 6 || res.Stages[5].Name != "Decision" {
			t.Errorf("%s: stages %+v", p.Label, res.Stages)
		}
	}
}

func TestLabAuthorsProduceTheirSignals(t *testing.T) {
	g := New(mustStore(), nil, "rules")
	plain := "Good sound for the price and the case feels solid."
	cases := []struct {
		author string
		stars  int
		text   string
		signal string
	}{
		{"unverified", 4, plain, "unverified_purchase"},
		{"new_account", 5, plain, "new_single_seller"},
		{"too_fast", 5, "Been using these for six months, still great.", "too_fast"},
		{"seller_staff", 5, plain, "seller_linked"},
		{"paid", 5, plain, "paid_review"},
		{"rival", 1, "Broke on day one.", "rival_attack"},
	}
	for _, c := range cases {
		if res := lab(t, g, c.author, c.stars, c.text); !hasSig(res, c.signal) {
			t.Errorf("%s: no %s signal in %+v", c.author, c.signal, res.Signals)
		}
	}
	// gated checks explain why they didn't fire
	if res := lab(t, g, "paid", 3, plain); hasSig(res, "paid_review") || res.AuthorNote == "" {
		t.Errorf("paid 3★ should not fire and should explain: %+v", res)
	}
	if res := lab(t, g, "verified", 5, plain); res.Action != "publish" || !res.Counted || !res.Verified {
		t.Errorf("clean verified review: %s counted=%v verified=%v", res.Action, res.Counted, res.Verified)
	}
}

func TestLabNeverWritesToTheStore(t *testing.T) {
	s := mustStore()
	g := New(s, nil, "rules")
	before := s.ReviewSnapshot()
	for _, p := range LabPresets {
		lab(t, g, p.Author, p.Stars, p.Text)
	}
	after := s.ReviewSnapshot()
	if len(after.Reviews) != len(before.Reviews) || len(after.Orders) != len(before.Orders) ||
		len(after.Identities) != len(before.Identities) || len(after.Payouts) != len(before.Payouts) {
		t.Fatalf("lab wrote to the store: reviews %d->%d orders %d->%d identities %d->%d payouts %d->%d",
			len(before.Reviews), len(after.Reviews), len(before.Orders), len(after.Orders),
			len(before.Identities), len(after.Identities), len(before.Payouts), len(after.Payouts))
	}
	if rt := g.Rating(FakeProduct); rt.N != 0 {
		t.Errorf("lab changed the rating: %+v", rt)
	}
}

func TestLabRejectsBadInput(t *testing.T) {
	g := New(mustStore(), nil, "rules")
	ctx := context.Background()
	if _, err := g.Lab(ctx, LabInput{ProductID: "P9999", Stars: 5, Text: "x", Author: "verified"}); err != ErrLabProduct {
		t.Errorf("unknown product: %v", err)
	}
	if _, err := g.Lab(ctx, LabInput{ProductID: FakeProduct, Stars: 5, Text: "x", Author: "bot"}); err != ErrLabAuthor {
		t.Errorf("unknown author: %v", err)
	}
	for _, in := range []LabInput{{Stars: 0, Text: "x"}, {Stars: 6, Text: "x"}, {Stars: 5, Text: "   "}, {Stars: 5, Text: string(make([]rune, 2001))}} {
		in.ProductID, in.Author = FakeProduct, "verified"
		if _, err := g.Lab(ctx, in); err != ErrLabInput {
			t.Errorf("bad input %d★ accepted: %v", in.Stars, err)
		}
	}
}

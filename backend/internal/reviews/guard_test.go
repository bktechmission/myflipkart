// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Pipeline tests: who may review what, idempotency, durability (WAL, restart,
// recovery of pending reviews), exactly-once under concurrency, re-checks after a
// seller payout, the trust-weighted rating and the demo scenarios.
package reviews

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/store"
)

func submit(g *Guard, cid, oid, pid string, stars int, text, key string) (model.Review, bool, error) {
	return g.Submit(context.Background(), SubmitInput{CID: cid, OrderID: oid, ProductID: pid, Stars: stars, Text: text, IdemKey: key})
}

func TestCustomersReviewOnlyTheirOwnDeliveredOrders(t *testing.T) {
	g := New(mustStore(), nil, "rules")
	// seed: O5101 = Meera (C1002), P1019, delivered; O5102 = Meera, shipped; O5001 = Aarav
	if _, _, err := submit(g, "C1002", "O5001", "P1001", 5, "nice", "key-00000001"); err != ErrNotFound {
		t.Errorf("someone else's order: %v", err)
	}
	if _, _, err := submit(g, "C1002", "O5102", "P1040", 5, "nice", "key-00000002"); err != ErrNotDelivered {
		t.Errorf("not delivered yet: %v", err)
	}
	if _, _, err := submit(g, "C1002", "O5101", "P1001", 5, "nice", "key-00000003"); err != ErrNotInOrder {
		t.Errorf("product not in the order: %v", err)
	}
	for _, bad := range []SubmitInput{{Stars: 0}, {Stars: 6}, {Stars: 5, Text: string(make([]rune, 2001))}} {
		bad.CID, bad.OrderID, bad.ProductID, bad.IdemKey = "C1002", "O5101", "P1019", "key-00000004"
		if _, _, err := g.Submit(context.Background(), bad); err != ErrBadInput {
			t.Errorf("bad input accepted: stars=%d", bad.Stars)
		}
	}
	if _, _, err := submit(g, "C1002", "O5101", "P1019", 5, "nice", "bad key!"); err == nil {
		t.Error("bad idempotency key accepted")
	}
	r, created, err := submit(g, "C1002", "O5101", "P1019", 2, "Strap broke in a week. Disappointed.", "key-00000005")
	if err != nil || !created || r.Status != "published" || !r.Counted || !r.Verified {
		t.Fatalf("own delivered order: %+v %v %v", r.Status, created, err)
	}
	again, created, err := submit(g, "C1002", "O5101", "P1019", 2, "Strap broke in a week. Disappointed.", "key-00000005")
	if err != nil || created || again.ID != r.ID {
		t.Fatalf("retry must return the same review: %v %v %v", again.ID, created, err)
	}
	if _, _, err := submit(g, "C1002", "O5101", "P1019", 5, "Changed my mind", "key-00000006"); err != ErrAlreadyReviewed {
		t.Errorf("second review of the same item: %v", err)
	}
	g.S.Tx(func(tx *store.Txn) error {
		tx.PutSanction(model.Sanction{ID: tx.NewID("Z"), Kind: "customer_ban", Target: "C1001", By: "mod@example.com", At: time.Now()})
		return nil
	})
	if _, _, err := submit(g, "C1001", "O5001", "P1001", 5, "great", "key-00000007"); err != ErrBanned {
		t.Errorf("banned customer: %v", err)
	}
}

func TestReviewsAreDurableAndRecoveredAfterACrash(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	g := New(s, nil, "rules")
	r, _, err := submit(g, "C1002", "O5101", "P1019", 4, "Good watch, GPS is quick.", "key-00000010")
	if err != nil {
		t.Fatal(err)
	}
	// a crash after the PENDING save but before the decision
	s.Tx(func(tx *store.Txn) error {
		tx.PutReview(model.Review{ID: "R900001", CustomerID: "C1001", OrderID: "O5001", ProductID: "P1001", Seller: "SoundCity Retail",
			Stars: 1, Text: "the seller is a zorkoid", CreatedAt: time.Now().UTC(), Status: "pending", Version: 1})
		return nil
	})
	s2, err := store.Open(dir, "") // restart: replay the WAL
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Review(r.ID)
	if !ok || got.Status != "published" || got.Text != r.Text {
		t.Fatalf("decided review lost on restart: %+v", got)
	}
	g2 := New(s2, nil, "rules")
	if n := g2.Recover(context.Background()); n != 1 {
		t.Fatalf("recovered %d pending reviews", n)
	}
	if x, _ := s2.Review("R900001"); x.Status != "rejected" || !contains(x.Rules, "RG-C4") {
		t.Fatalf("recovered review: %s %v", x.Status, x.Rules)
	}
	if n := g2.Recover(context.Background()); n != 0 {
		t.Fatal("recovery must not act twice")
	}
	if len(s2.AuditLog(100)) < 3 {
		t.Error("audit log not durable")
	}
}

func TestConcurrentSubmitsAndRetriesActExactlyOnce(t *testing.T) {
	if !exactlyOnce(context.Background()) {
		t.Fatal("a review was lost or decided twice under concurrent submits and retries")
	}
	// many workers racing to process the same pending review: one decision
	s := mustStore()
	g := New(s, nil, "rules")
	s.Tx(func(tx *store.Txn) error {
		tx.PutReview(model.Review{ID: "R800001", CustomerID: "C1002", OrderID: "O5101", ProductID: "P1019", Seller: "TimeTech Store",
			Stars: 5, Text: "Great", CreatedAt: time.Now().UTC(), Status: "pending", Version: 1})
		return nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); g.Process(context.Background(), "R800001") }()
	}
	wg.Wait()
	n := 0
	for _, a := range s.AuditLog(1000) {
		if a.Target == "R800001" && a.ActorKind == "engine" {
			n++
		}
	}
	if r, _ := s.Review("R800001"); n != 1 || r.Version != 2 {
		t.Fatalf("%d engine decisions, version %d", n, r.Version)
	}
}

func TestPayoutAfterA5StarMakesItAPaidReview(t *testing.T) {
	s := mustStore()
	g := New(s, nil, "rules")
	r, _, err := submit(g, "C1002", "O5106", "P1005", 5, "Love these, great noise cancelling.", "key-00000020")
	if err != nil || !r.Counted {
		t.Fatalf("genuine 5★: %+v %v", r.Counted, err)
	}
	ch, _ := g.RecordPayout(context.Background(), model.Payout{Seller: "SoundNest Store", CustomerID: "C1002", Kind: "cashback", Amount: 20, At: r.CreatedAt.Add(time.Hour)})
	got, _ := s.Review(r.ID)
	if len(ch) != 1 || got.Counted || got.Status != "held" || !contains(got.Rules, "RG-A3") || got.CaseID == "" {
		t.Fatalf("after cashback: changed=%v %s counted=%v rules=%v", ch, got.Status, got.Counted, got.Rules)
	}
	// a payout a week later is not linked
	r2, _, _ := submit(g, "C1001", "O5001", "P1001", 5, "Quiet flights at last.", "key-00000021")
	g.RecordPayout(context.Background(), model.Payout{Seller: "SoundCity Retail", CustomerID: "C1001", Kind: "refund", Amount: 5, At: r2.CreatedAt.Add(7 * 24 * time.Hour)})
	if x, _ := s.Review(r2.ID); !x.Counted {
		t.Error("an unrelated later refund must not un-count a review")
	}
}

func TestTrustWeightedRating(t *testing.T) {
	now := time.Now()
	rv := func(stars int, status string, counted bool, ago time.Duration) model.Review {
		return model.Review{Stars: stars, Status: status, Counted: counted, CreatedAt: now.Add(-ago)}
	}
	reviews := []model.Review{rv(5, "published", true, 48*time.Hour), rv(3, "published", true, 48*time.Hour),
		rv(5, "held", false, time.Hour), rv(5, "published", false, time.Hour), rv(1, "rejected", false, time.Hour)}
	r := ComputeRating("P1", 4.0, reviews, model.ProductReviewState{})
	if r.N != 2 || r.Shown != 4.0 || r.Naive != 3.8 || r.NaiveN != 5 { // (5*4 + 8) / 7 = 4.0
		t.Fatalf("rating %+v", r)
	}
	paused := ComputeRating("P1", 4.0, append(reviews, rv(5, "published", true, time.Minute)), model.ProductReviewState{BurstSince: now.Add(-2 * time.Hour)})
	if !paused.Paused || paused.N != 2 || paused.Shown != 4.0 {
		t.Fatalf("burst must keep the last trusted value: %+v", paused)
	}
	frozen := ComputeRating("P1", 4.0, reviews, model.ProductReviewState{Frozen: true, FrozenRating: 4.4, FrozenN: 9})
	if !frozen.Frozen || frozen.Shown != 4.4 || frozen.N != 9 {
		t.Fatalf("frozen %+v", frozen)
	}
}

func TestDemoScenarios(t *testing.T) {
	s := mustStore()
	g := New(s, nil, "rules")
	ctx := context.Background()
	before := g.Rating(FakeProduct)
	for _, sc := range []string{"fake5", "rival1", "viral", "abuse"} {
		res, err := g.Simulate(ctx, sc, "test")
		if err != nil || len(res.Reviews) == 0 {
			t.Fatalf("%s: %v", sc, err)
		}
		for _, r := range res.Reviews {
			if !r.Synthetic || !store.IsSyntheticCID(r.CustomerID) {
				t.Fatalf("%s: non-synthetic data %+v", sc, r.CustomerID)
			}
		}
	}
	var campaign, rival *model.ReviewCase
	for _, c := range s.ReviewCases() {
		c := c
		switch {
		case c.Kind == "seller_campaign" && c.ProductID == FakeProduct:
			campaign = &c
		case c.Kind == "rival_attack":
			rival = &c
		}
	}
	if campaign == nil || len(campaign.ReviewIDs) != 12 || campaign.Seller != FakeSeller || campaign.Title != "12 reviews on Quietude Zen 7 linked to SoundNest Store" {
		t.Fatalf("fake campaign case: %+v", campaign)
	}
	if rival == nil || len(rival.ReviewIDs) != 5 || rival.Seller != FakeSeller || rival.ProductID != RivalProduct {
		t.Fatalf("rival case: %+v", rival)
	}
	for _, r := range s.Reviews(nil) {
		if (r.Scenario == "fake5" || r.Scenario == "rival1") && r.Counted {
			t.Errorf("fake review counted: %s %s", r.ID, r.Text)
		}
		if r.Scenario == "viral" && (!r.Counted || r.Status != "published" || len(r.Rules) > 0) {
			t.Errorf("genuine viral review flagged: %s %v", r.Text, r.Rules)
		}
	}
	after := g.Rating(FakeProduct)
	if !after.Paused || after.Shown != before.Shown && after.N != 4 || after.Naive < 4.5 {
		t.Errorf("fake product rating: before %+v after %+v", before, after)
	}
	if rr := g.Rating(RivalProduct); rr.Naive > 3 || rr.Shown < 4 {
		t.Errorf("rival attack must not drag the shown rating: %+v", rr)
	}
	for i, r := range s.Reviews(func(r model.Review) bool { return r.Scenario == "abuse" }) {
		if r.Action != AbuseCases[i].Expect {
			t.Errorf("abuse %q: %s, want %s", r.Text, r.Action, AbuseCases[i].Expect)
		}
	}
	if _, err := g.Simulate(ctx, "reset", "test"); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Reviews(nil)) + len(s.ReviewCases()); n != 0 {
		t.Errorf("reset left %d records", n)
	}
	for _, o := range s.AllOrders() {
		if store.IsSyntheticCID(o.CustomerID) {
			t.Fatalf("reset left synthetic order %s", o.ID)
		}
	}
	if _, err := g.Simulate(ctx, "nope", "test"); err == nil {
		t.Error("unknown scenario accepted")
	}
}

func TestEvalGatesPass(t *testing.T) {
	r := RunEval(context.Background())
	for k, v := range r.Rates {
		if strings.HasPrefix(k, "heldout_") { // enforced by make gate; reported here, not hidden
			t.Logf("held-out %s = %.4f", k, v)
			continue
		}
		min := 1.0
		switch strings.TrimPrefix(k, "heldout_") {
		case "action_accuracy":
			min = 0.95
		case "adversarial_recall":
			min = 0.9
		case "auto_reject_precision":
			min = 0.99
		case "fake_held", "abuse_scenario":
			min = 0.9
		}
		if v < min {
			t.Errorf("%s = %.4f < %.2f", k, v, min)
		}
	}
	for _, f := range r.Failures {
		t.Logf("failure: %s %q expected %s got %s", f.ID, f.Text, f.Expect, f.Got)
	}
	for name, ok := range r.Guards {
		if !ok {
			t.Errorf("guard %s failed", name)
		}
	}
	if len(r.Rates) < 15 || r.N < 100 {
		t.Errorf("eval too small: %d rates, %d cases", len(r.Rates), r.N)
	}
	if r.Policy != DefaultPolicy().Version {
		t.Error(fmt.Sprint("eval ran on policy ", r.Policy))
	}
}

func TestBurstPausesTheRatingButNeverHidesAnHonestReview(t *testing.T) {
	s := mustStore()
	g := New(s, nil, "rules")
	ctx := context.Background()
	if _, err := g.Simulate(ctx, "fake5", "test"); err != nil {
		t.Fatal(err)
	}
	before := g.Rating(FakeProduct)
	r, _, err := submit(g, "C1002", "O5106", FakeProduct, 1, "Left cup creaks and the noise cancelling is weak. Disappointed.", "key-00000099")
	if err != nil || r.Status != "published" || r.Counted || r.Action != "not_counted" {
		t.Fatalf("honest review during a burst: %s counted=%v action=%s %v", r.Status, r.Counted, r.Action, err)
	}
	if after := g.Rating(FakeProduct); after.Shown != before.Shown || !after.Paused {
		t.Fatalf("rating moved during the pause: %+v -> %+v", before, after)
	}
	var burst model.ReviewCase
	for _, c := range s.ReviewCases() {
		if c.Kind == "burst" {
			burst = c
		}
	}
	mod := Moderator{Email: "mod@example.com", Kind: "human"}
	if _, err := g.DecideCase(burst.ID, DemoModerator, "dismiss", "demo tries", []string{"rating_freeze"}); err != ErrNoSanctions {
		t.Errorf("a simulated moderator applied a sanction: %v", err)
	}
	if _, err := g.DecideCase(burst.ID, mod, "dismiss", "the fakes are handled in their own case", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Review(r.ID)
	rt := g.Rating(FakeProduct)
	if !got.Counted || rt.Paused || rt.N != before.N+1 {
		t.Fatalf("after the person cleared the burst: counted=%v rating %+v", got.Counted, rt)
	}
}

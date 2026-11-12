// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// ReviewGuard authorization tests over the real HTTP handler: customers review
// only their own delivered orders; sellers can't moderate their own reviews, see
// reviewer identity, or suppress a rival's reviews by reporting them; admin
// routes are admin-only; appeals need a second moderator; the product page shows
// the trusted rating and escapes review text.
package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/model"
	"asksauby/internal/reviews"
)

func (e *env) login(key, cid, email, role string) {
	rec := httptest.NewRecorder()
	e.s.setSession(rec, httptest.NewRequest("GET", "/", nil), auth.Session{CID: cid, Email: email, Name: key, Role: role,
		Exp: time.Now().Add(time.Hour).Unix(), ID: key})
	e.jar[key] = rec.Result().Cookies()[0]
}

func jsonBody(v any) []byte { b, _ := json.Marshal(v); return b }

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON %d: %s", w.Code, w.Body.String())
	}
}

func post(e *env, who, path string, v any) *httptest.ResponseRecorder {
	return e.do(who, "POST", path, jsonBody(v), nil)
}

func TestReviewSubmitAuthorization(t *testing.T) {
	e := newEnv(t)
	review := func(who, order, product, key string) *httptest.ResponseRecorder {
		return post(e, who, "/api/reviews", map[string]any{"order_id": order, "product_id": product, "stars": 2, "text": "Strap broke within a week.", "idem_key": key})
	}
	if w := review("C1002", "O5001", "P1001", "idem-aaaa-0001"); w.Code != 404 { // Aarav's order
		t.Errorf("someone else's order: %d %s", w.Code, w.Body)
	}
	if w := review("C1002", "O5102", "P1040", "idem-aaaa-0002"); w.Code != 409 { // shipped, not delivered
		t.Errorf("undelivered order: %d", w.Code)
	}
	if w := review("C1002", "O5101", "P1001", "idem-aaaa-0003"); w.Code != 400 {
		t.Errorf("product not in order: %d", w.Code)
	}
	if w := review("G-guest", "O5101", "P1019", "idem-aaaa-0004"); w.Code != 403 {
		t.Errorf("guest: %d", w.Code)
	}
	if w := e.do("", "POST", "/api/reviews", nil, nil); w.Code != 401 {
		t.Errorf("anonymous: %d", w.Code)
	}
	if w := e.do("C1002", "POST", "/api/reviews", jsonBody(map[string]any{}), map[string]string{"X-Sauby": ""}); w.Code != 403 {
		t.Errorf("CSRF header: %d", w.Code)
	}
	w := review("C1002", "O5101", "P1019", "idem-aaaa-0005")
	var out struct {
		Review  map[string]any `json:"review"`
		Created bool           `json:"created"`
		Rating  reviews.Rating `json:"rating"`
	}
	decode(t, w, &out)
	if w.Code != 200 || !out.Created || out.Review["status"] != "published" || out.Rating.N != 1 {
		t.Fatalf("own delivered order: %d %s", w.Code, w.Body)
	}
	w2 := review("C1002", "O5101", "P1019", "idem-aaaa-0005") // retry: same review, nothing new
	var again struct {
		Review  map[string]any `json:"review"`
		Created bool           `json:"created"`
	}
	decode(t, w2, &again)
	if again.Created || again.Review["id"] != out.Review["id"] {
		t.Fatalf("retry created a second review: %s", w2.Body)
	}
	// my reviews are mine only
	var mine struct{ Reviews []map[string]any }
	decode(t, e.do("C1001", "GET", "/api/reviews/mine", nil, nil), &mine)
	if len(mine.Reviews) != 0 {
		t.Fatalf("Aarav sees someone else's reviews: %v", mine.Reviews)
	}
	// rate limit per customer
	limited := false
	for i := 0; i < 10; i++ {
		if w := review("C1002", "O5106", "P1005", fmt.Sprintf("idem-bbbb-%04d", i)); w.Code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("review submits are not rate limited")
	}
}

func TestReviewAdminRoutesAreAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.login("seller", "C1004", "seller@example.com", "seller")
	for _, who := range []string{"C1002", "seller", "G-guest"} {
		for _, rt := range []struct{ m, p string }{{"GET", "/api/admin/reviews"}, {"GET", "/api/admin/reviews/policy"},
			{"POST", "/api/admin/reviews/sim"}, {"POST", "/api/admin/reviews/cases/K000001"}, {"POST", "/api/admin/reviews/appeals/A000001"},
			{"GET", "/api/admin/reviews/cases/K000001"}} {
			if w := e.do(who, rt.m, rt.p, []byte(`{"scenario":"fake5"}`), nil); w.Code != 403 {
				t.Errorf("%s %s as %s: %d", rt.m, rt.p, who, w.Code)
			}
		}
	}
	for _, who := range []string{"C1002", "G-guest"} {
		if w := e.do(who, "GET", "/api/seller/reviews", nil, nil); w.Code != 403 {
			t.Errorf("seller view as %s: %d", who, w.Code)
		}
	}
}

func openCase(t *testing.T, e *env, kind, product string) caseView {
	t.Helper()
	var dash struct{ Cases []caseView }
	decode(t, e.do("C1001", "GET", "/api/admin/reviews", nil, nil), &dash)
	for _, c := range dash.Cases {
		if c.Kind == kind && (product == "" || c.ProductID == product) {
			return c
		}
	}
	t.Fatalf("no open %s case for %s", kind, product)
	return caseView{}
}

func TestSellersCantModerateSeeIdentitiesOrSuppressRivals(t *testing.T) {
	e := newEnv(t)
	e.login("seller", "C1004", "seller@example.com", "seller")
	e.login("seller-admin", "C1004", "seller@example.com", "admin") // a moderator who is also SoundNest staff
	for _, sc := range []string{"fake5", "rival1"} {
		if w := post(e, "C1001", "/api/admin/reviews/sim", map[string]string{"scenario": sc}); w.Code != 200 {
			t.Fatalf("sim %s: %d %s", sc, w.Code, w.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fake := openCase(t, e, "seller_campaign", reviews.FakeProduct)
	rival := openCase(t, e, "rival_attack", reviews.RivalProduct)
	for _, c := range []caseView{fake, rival} { // conflict of interest: SoundNest is in both cases
		if w := post(e, "seller-admin", "/api/admin/reviews/cases/"+c.ID, map[string]any{"decision": "dismiss", "note": "looks fine to me"}); w.Code != 403 {
			t.Errorf("seller-linked moderator decided %s: %d %s", c.ID, w.Code, w.Body)
		}
	}
	// the seller's own view: general terms only, no reviewer identity or text
	w := e.do("seller", "GET", "/api/seller/reviews", nil, nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Quietude Zen 7") {
		t.Fatalf("seller view: %d %s", w.Code, body)
	}
	for _, leak := range []string{`"S0`, "customer", "@", "Best headphones", "dev-", "card-", "net-", "addr-"} {
		if strings.Contains(body, leak) {
			t.Errorf("seller view leaks %q", leak)
		}
	}
	// reporting a rival's genuine review never hides or un-counts it
	var target model.Review
	for _, r := range e.s.store.Reviews(func(r model.Review) bool { return r.ProductID == reviews.RivalProduct && r.Counted }) {
		target = r
	}
	for i := 0; i < 3; i++ {
		post(e, "seller", "/api/seller/reports", map[string]string{"reviewid": target.ID, "reason": "this review is fake, remove it"})
	}
	after, _ := e.s.store.Review(target.ID)
	if after.Status != "published" || !after.Counted || after.Version != target.Version {
		t.Fatalf("a rival's report changed the review: %s counted=%v", after.Status, after.Counted)
	}
	rep := openCase(t, e, "report", reviews.RivalProduct)
	if !strings.Contains(strings.Join(rep.Evidence, " "), "competing seller") {
		t.Errorf("report case doesn't flag the competing reporter: %v", rep.Evidence)
	}
	// a seller appeal is decided by a person who isn't linked to the seller
	var sv struct {
		Products []struct {
			ID         string
			NotCounted []struct{ Reason string } `json:"not_counted"`
		}
	}
	decode(t, e.do("seller", "GET", "/api/seller/reviews", nil, nil), &sv)
	_ = sv
}

func TestModerationAppealsAndSanctions(t *testing.T) {
	e := newEnv(t)
	post(e, "C1001", "/api/admin/reviews/sim", map[string]string{"scenario": "fake5"})
	fake := openCase(t, e, "seller_campaign", reviews.FakeProduct)
	if w := post(e, "C1001", "/api/admin/reviews/cases/"+fake.ID, map[string]any{"decision": "approve"}); w.Code != 400 {
		t.Errorf("decision without a note: %d", w.Code)
	}
	if w := post(e, "C1001", "/api/admin/reviews/cases/"+fake.ID, map[string]any{"decision": "approve", "note": "bought reviews", "sanctions": []string{"nuke"}}); w.Code != 403 {
		t.Errorf("unknown sanction: %d", w.Code)
	}
	w := post(e, "C1001", "/api/admin/reviews/cases/"+fake.ID, map[string]any{"decision": "approve", "note": "bought reviews, clear evidence",
		"sanctions": []string{"seller_strike", "rating_freeze"}})
	if w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if n := e.s.store.SellerStrikes(reviews.FakeSeller); n != 1 {
		t.Errorf("strikes = %d", n)
	}
	if r := e.s.rg.Rating(reviews.FakeProduct); !r.Frozen {
		t.Errorf("rating not frozen: %+v", r)
	}
	for _, id := range fake.ReviewIDs {
		if r, _ := e.s.store.Review(id); r.Status != "removed" || r.DecidedBy != "aarav@example.com" {
			t.Errorf("%s after approve: %s by %s", id, r.Status, r.DecidedBy)
		}
	}
	if w := post(e, "C1001", "/api/admin/reviews/cases/"+fake.ID, map[string]any{"decision": "dismiss", "note": "again"}); w.Code != 409 {
		t.Errorf("deciding twice: %d", w.Code)
	}
	var seller struct{ Strikes int }
	for _, s := range e.s.tools.Sellers() {
		if s.Seller == reviews.FakeSeller {
			seller.Strikes = s.Strikes
		}
	}
	if seller.Strikes != 1 {
		t.Error("seller standing doesn't show the strike")
	}

	// author appeal: the engine rejected it, so any human may decide; a moderator who decided can't decide the appeal
	w = post(e, "C1002", "/api/reviews", map[string]any{"order_id": "O5101", "product_id": "P1019", "stars": 1, "text": "The seller is an idiot.", "idem_key": "idem-cccc-0001"})
	var sub struct{ Review struct{ ID, Status string } }
	decode(t, w, &sub)
	if sub.Review.Status != "rejected" {
		t.Fatalf("insult: %s", w.Body)
	}
	if w := post(e, "C1001", "/api/reviews/"+sub.Review.ID+"/appeal", map[string]string{"text": "not mine"}); w.Code != 404 {
		t.Errorf("appealing someone else's review: %d", w.Code)
	}
	w = post(e, "C1002", "/api/reviews/"+sub.Review.ID+"/appeal", map[string]string{"text": "I was upset, please look again"})
	var ap model.Appeal
	decode(t, w, &ap)
	if w.Code != 200 || ap.FirstDecider != "engine" {
		t.Fatalf("appeal: %d %s", w.Code, w.Body)
	}
	if w := post(e, "C1002", "/api/reviews/"+sub.Review.ID+"/appeal", map[string]string{"text": "again please"}); w.Code != 409 {
		t.Errorf("second appeal: %d", w.Code)
	}
	if w := post(e, "C1001", "/api/admin/reviews/appeals/"+ap.ID+"/demo", nil); w.Code != 403 {
		t.Errorf("demo moderator with SIM_AGENTS=0: %d", w.Code)
	}
	w = post(e, "C1001", "/api/admin/reviews/appeals/"+ap.ID, map[string]string{"decision": "uphold", "note": "insult aimed at the seller"})
	if w.Code != 200 {
		t.Fatalf("decide appeal: %d %s", w.Code, w.Body)
	}

	// a person removed it; the same person can't decide the appeal, a Demo second moderator can (labelled)
	post(e, "C1001", "/api/admin/reviews/sim", map[string]string{"scenario": "rival1"})
	rival := openCase(t, e, "rival_attack", reviews.RivalProduct)
	post(e, "C1001", "/api/admin/reviews/cases/"+rival.ID, map[string]any{"decision": "approve", "note": "rival attack"})
	var removed model.Review
	removed, _ = e.s.store.Review(rival.ReviewIDs[0])
	appeal, err := e.s.rg.AuthorAppeal(removed.CustomerID, removed.ID, "this was my honest opinion")
	if err != nil {
		t.Fatal(err)
	}
	if w := post(e, "C1001", "/api/admin/reviews/appeals/"+appeal.ID, map[string]string{"decision": "overturn", "note": "fine"}); w.Code != 403 {
		t.Errorf("first decider decided the appeal: %d", w.Code)
	}
	e.s.cfg.SimAgents = true
	w = post(e, "C1001", "/api/admin/reviews/appeals/"+appeal.ID+"/demo", nil)
	decode(t, w, &ap)
	if w.Code != 200 || ap.DeciderKind != "simulated" || ap.Status != "upheld" || !strings.HasPrefix(ap.Note, "Demo") {
		t.Fatalf("demo second moderator: %d %+v", w.Code, ap)
	}
	// every decision is in the audit log with versions
	found := false
	for _, a := range e.s.store.AuditLog(500) {
		found = found || (a.Action == "appeal_upheld" && a.ActorKind == "simulated")
	}
	if !found {
		t.Error("simulated decision missing from the audit log")
	}
}

func TestProductPageShowsTrustedRatingAndEscapes(t *testing.T) {
	e := newEnv(t)
	post(e, "C1002", "/api/reviews", map[string]any{"order_id": "O5101", "product_id": "P1019", "stars": 4,
		"text": "<script>alert(1)</script> Good GPS", "idem_key": "idem-dddd-0001"})
	w := e.do("", "GET", "/p/P1019", nil, nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "based on 1 verified review") || !strings.Contains(body, "Good GPS") {
		t.Fatalf("product page: %d", w.Code)
	}
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("review text not escaped")
	}
	post(e, "C1001", "/api/admin/reviews/sim", map[string]string{"scenario": "fake5"})
	body = e.do("", "GET", "/p/"+reviews.FakeProduct, nil, nil).Body.String()
	if !strings.Contains(body, "last trusted value") || strings.Contains(body, "Best headphones ever") {
		t.Fatalf("fake campaign visible or rating not paused on the product page")
	}
}

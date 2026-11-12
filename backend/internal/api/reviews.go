// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// ReviewGuard over HTTP.
//
//	shoppers  GET  /p/{id}                       product page: trusted rating + published reviews (server-rendered)
//	          GET  /api/reviews/mine?product=    my reviews of a product, and my delivered orders I can review
//	          POST /api/reviews                  submit (own delivered order only; idempotency key; rate limited)
//	          POST /api/reviews/{id}/appeal      ask a second person to look
//	sellers   GET  /api/seller/reviews           which reviews stopped counting, in general terms (no reviewer identity)
//	          POST /api/seller/appeals           appeal a reason group
//	          POST /api/seller/reports           report a review (never hides it by itself)
//	admins    GET  /api/admin/reviews            queue, appeals, ratings, metrics, audit
//	          GET  /api/admin/reviews/cases/{id} evidence, labels with quotes, tagger, suggested action
//	          POST /api/admin/reviews/cases/{id} approve | dismiss with a note (+ sanctions, people only)
//	          POST /api/admin/reviews/appeals/{id}        uphold | overturn (a second moderator)
//	          POST /api/admin/reviews/appeals/{id}/demo   Demo second moderator (SIM_AGENTS=1 only, labelled)
//	          POST /api/admin/reviews/sim                 demo scenarios on synthetic data
//	          GET  /api/admin/reviews/policy              the policy library

package api

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/evals"
	"asksauby/internal/model"
	"asksauby/internal/reviews"
	"asksauby/internal/seed"
)

func (s *Server) reviewRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/reviews/mine", s.withAuth(s.myReviews, false))
	m.HandleFunc("POST /api/reviews", s.withAuth(s.submitReview, false))
	m.HandleFunc("POST /api/reviews/{id}/appeal", s.withAuth(s.appealReview, false))
	m.HandleFunc("GET /api/seller/reviews", s.withAuth(s.sellerReviews, false))
	m.HandleFunc("POST /api/seller/appeals", s.withAuth(s.sellerAppeal, false))
	m.HandleFunc("POST /api/seller/reports", s.withAuth(s.sellerReport, false))
	m.HandleFunc("GET /seller", func(w http.ResponseWriter, r *http.Request) {
		if sess := s.session(r); sess == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		http.ServeFile(w, r, filepath.Join(s.cfg.WebDir, "seller.html"))
	})
	m.HandleFunc("GET /api/admin/reviews", s.withAuth(s.adminReviews, true))
	m.HandleFunc("GET /api/admin/reviews/policy", s.withAuth(s.reviewPolicy, true))
	m.HandleFunc("GET /api/admin/reviews/cases/{id}", s.withAuth(s.reviewCase, true))
	m.HandleFunc("POST /api/admin/reviews/cases/{id}", s.withAuth(s.decideReviewCase, true))
	m.HandleFunc("POST /api/admin/reviews/appeals/{id}", s.withAuth(s.decideReviewAppeal, true))
	m.HandleFunc("POST /api/admin/reviews/appeals/{id}/demo", s.withAuth(s.demoAppeal, true))
	m.HandleFunc("POST /api/admin/reviews/sim", s.withAuth(s.reviewSim, true))
}

func (s *Server) sellerOf(sess *auth.Session) string {
	return s.cfg.SellerAccounts[strings.ToLower(sess.Email)]
}

func (s *Server) moderator(sess *auth.Session) reviews.Moderator {
	return reviews.Moderator{Email: sess.Email, Name: sess.Name, Seller: s.sellerOf(sess), Kind: "human"}
}

func reviewErr(w http.ResponseWriter, err error) {
	code := 409
	switch {
	case errors.Is(err, reviews.ErrNotFound), errors.Is(err, reviews.ErrNotYours):
		code = 404
	case errors.Is(err, reviews.ErrBanned), errors.Is(err, reviews.ErrConflict), errors.Is(err, reviews.ErrSecondModerator), errors.Is(err, reviews.ErrNoSanctions):
		code = 403
	case errors.Is(err, reviews.ErrBadInput), errors.Is(err, reviews.ErrNotInOrder), errors.Is(err, reviews.ErrBadDecision), errors.Is(err, reviews.ErrNoteRequired):
		code = 400
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// ---------------- shoppers ----------------

// authorView is what an author sees about their own review: the outcome and the rule, never other people's data.
type authorView struct {
	ID        string        `json:"id"`
	ProductID string        `json:"product_id"`
	OrderID   string        `json:"order_id"`
	Stars     int           `json:"stars"`
	Text      string        `json:"text"`
	Shown     string        `json:"shown"`
	Status    string        `json:"status"`
	Action    string        `json:"action"`
	Counted   bool          `json:"counted"`
	Rules     []ruleView    `json:"rules"`
	Message   string        `json:"message"`
	Appeal    *model.Appeal `json:"appeal,omitempty"`
	Tagger    string        `json:"tagger"`
	CreatedAt time.Time     `json:"created_at"`
}

type ruleView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

func (s *Server) authorView(r model.Review) authorView {
	v := authorView{ID: r.ID, ProductID: r.ProductID, OrderID: r.OrderID, Stars: r.Stars, Text: r.Text, Shown: r.Shown, Status: r.Status,
		Action: r.Action, Counted: r.Counted, Tagger: r.Tagger, CreatedAt: r.CreatedAt, Rules: []ruleView{}}
	for _, id := range r.Rules {
		if ru, ok := s.rg.P.Rule(id); ok && (ru.Kind == "content" || r.Status != "held") {
			v.Rules = append(v.Rules, ruleView{ru.ID, ru.Name, ru.Definition})
		}
	}
	v.Message = map[string]string{
		"publish":      "Published and counted in the rating. Thank you!",
		"mask_publish": "Published. We removed contact details or links, which reviews can't include.",
		"not_counted":  "Published, but it doesn't count toward the rating (see the reason). You can appeal.",
		"hold":         "Thanks! A person on our review team is checking it before it appears.",
		"reject":       "Not published: it breaks the rule below. You can edit and post again, or appeal.",
		"remove":       "Removed by our review team. You can appeal.",
	}[r.Action]
	if r.Status == "pending" {
		v.Message = "Received. Checking it now."
	}
	for _, a := range s.store.Appeals() {
		if a.ByRole == "author" && len(a.ReviewIDs) == 1 && a.ReviewIDs[0] == r.ID {
			a := a
			v.Appeal = &a
		}
	}
	return v
}

func (s *Server) myReviews(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	pid := strings.ToUpper(r.URL.Query().Get("product"))
	mine := s.store.Reviews(func(x model.Review) bool { return x.CustomerID == sess.CID && (pid == "" || x.ProductID == pid) })
	out := []authorView{}
	reviewed := map[string]bool{}
	for _, x := range mine {
		out = append(out, s.authorView(x))
		reviewed[x.OrderID+"|"+x.ProductID] = true
	}
	type can struct {
		OrderID     string `json:"order_id"`
		ProductID   string `json:"product_id"`
		DeliveredOn string `json:"delivered_on"`
	}
	reviewable := []can{}
	for _, o := range s.store.OrdersFor(sess.CID) {
		if o.DeliveredOn == "" || o.Status == "cancelled" {
			continue
		}
		for _, it := range o.Items {
			if (pid == "" || it == pid) && !reviewed[o.ID+"|"+it] {
				reviewable = append(reviewable, can{o.ID, it, o.DeliveredOn})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"reviews": out, "reviewable": reviewable, "banned": s.store.CustomerBanned(sess.CID)})
}

func (s *Server) submitReview(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	if sess.Role == "guest" {
		writeJSON(w, 403, map[string]string{"error": "sign in to review a product you bought"})
		return
	}
	var in struct {
		OrderID   string `json:"order_id"`
		ProductID string `json:"product_id"`
		Stars     int    `json:"stars"`
		Text      string `json:"text"`
		IdemKey   string `json:"idem_key"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if ok, _ := s.rvLimit.Allow("rv:" + sess.CID); !ok {
		writeJSON(w, 429, map[string]string{"error": "You're posting reviews quickly. Try again in a minute."})
		return
	}
	rv, created, err := s.rg.Submit(r.Context(), reviews.SubmitInput{CID: sess.CID, OrderID: strings.ToUpper(in.OrderID),
		ProductID: strings.ToUpper(in.ProductID), Stars: in.Stars, Text: in.Text, IdemKey: in.IdemKey})
	if err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"review": s.authorView(rv), "created": created, "rating": s.rg.Rating(rv.ProductID)})
}

func (s *Server) appealReview(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct{ Text string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	a, err := s.rg.AuthorAppeal(sess.CID, r.PathValue("id"), in.Text)
	if err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, a)
}

// ---------------- sellers ----------------

func (s *Server) requireSeller(w http.ResponseWriter, sess *auth.Session) string {
	seller := s.sellerOf(sess)
	if seller == "" {
		writeJSON(w, 403, map[string]string{"error": "seller account required"})
	}
	return seller
}

// sellerReviews shows a seller its products' trusted ratings and the reviews that
// stopped counting, by reason group: stars, date and status only. Never who wrote
// them, their text (when not public) or any reviewer identifiers.
func (s *Server) sellerReviews(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	seller := s.requireSeller(w, sess)
	if seller == "" {
		return
	}
	type row struct {
		Date   string `json:"date"`
		Stars  int    `json:"stars"`
		Shown  bool   `json:"shown"`
		Reason string `json:"reason"`
	}
	type group struct {
		Reason  string `json:"reason"`
		Count   int    `json:"count"`
		Rows    []row  `json:"rows"`
		Appeal  string `json:"appeal,omitempty"` // open | upheld | overturned
		Appeals int    `json:"appeals"`
	}
	type prod struct {
		ID         string         `json:"id"`
		Name       string         `json:"name"`
		Rating     reviews.Rating `json:"rating"`
		Counted    int            `json:"counted"`
		NotCounted []*group       `json:"not_counted"`
		Waiting    int            `json:"waiting"` // with a person, outcome not known yet
	}
	var out []prod
	appeals := s.store.Appeals()
	for _, p := range s.store.Products() {
		if seed.SellerOf(p) != seller {
			continue
		}
		pr := prod{ID: p.ID, Name: p.Name, Rating: s.rg.Rating(p.ID), NotCounted: []*group{}}
		groups := map[string]*group{}
		for _, x := range s.store.Reviews(func(x model.Review) bool { return x.ProductID == p.ID && x.Status != "pending" }) {
			switch {
			case x.Counted:
				pr.Counted++
			case x.Status == "held":
				pr.Waiting++
			default:
				reason := reviews.ReasonGroup(x)
				g := groups[reason]
				if g == nil {
					g = &group{Reason: reason}
					groups[reason] = g
					pr.NotCounted = append(pr.NotCounted, g)
				}
				g.Count++
				g.Rows = append(g.Rows, row{x.CreatedAt.Format("2006-01-02"), x.Stars, x.Status == "published", reason})
			}
		}
		for _, g := range pr.NotCounted {
			for _, a := range appeals {
				if a.ByRole == "seller" && a.ProductID == p.ID && a.Reason == g.Reason {
					g.Appeal, g.Appeals = a.Status, g.Appeals+1
				}
			}
		}
		out = append(out, pr)
	}
	strikes := s.store.SellerStrikes(seller)
	writeJSON(w, 200, map[string]any{"seller": seller, "products": out, "strikes": strikes,
		"note": "Reviews are shown in general terms. MyFlipKart never shares who wrote a review."})
}

func (s *Server) sellerAppeal(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	seller := s.requireSeller(w, sess)
	if seller == "" {
		return
	}
	var in struct{ ProductID, Reason, Text string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if ok, _ := s.rvLimit.Allow("sa:" + seller); !ok {
		writeJSON(w, 429, map[string]string{"error": "slow down"})
		return
	}
	a, err := s.rg.SellerAppeal(seller, strings.ToUpper(in.ProductID), in.Reason, in.Text)
	if err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": a.ID, "status": a.Status, "reviews": len(a.ReviewIDs)})
}

func (s *Server) sellerReport(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	seller := s.requireSeller(w, sess)
	if seller == "" {
		return
	}
	var in struct{ ReviewID, Reason string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if ok, _ := s.rvLimit.Allow("sr:" + seller); !ok {
		writeJSON(w, 429, map[string]string{"error": "slow down"})
		return
	}
	if _, err := s.rg.Report(seller, "seller", seller, in.ReviewID, in.Reason); err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "Thanks. A person will look at it; the review stays up until they decide."})
}

// ---------------- admins ----------------

type caseView struct {
	model.ReviewCase
	ProductName string         `json:"product_name"`
	Rating      reviews.Rating `json:"rating"`
	Summary     []evidence     `json:"summary"`
	Order       float64        `json:"order"` // queue position score: severity, then reach
}

type evidence struct {
	Signal string `json:"signal"`
	Rule   string `json:"rule"`
	Count  int    `json:"count"`
	Detail string `json:"detail"`
}

func (s *Server) caseView(c model.ReviewCase) caseView {
	p, _ := s.store.Product(c.ProductID)
	v := caseView{ReviewCase: c, ProductName: p.Name, Rating: s.rg.Rating(c.ProductID), Summary: []evidence{}}
	byName := map[string]*evidence{}
	for _, rid := range c.ReviewIDs {
		r, _ := s.store.Review(rid)
		for _, sg := range r.Signals {
			if sg.Kind == "context" {
				continue
			}
			e := byName[sg.Name]
			if e == nil {
				e = &evidence{Signal: sg.Name, Rule: sg.Rule, Detail: sg.Detail}
				byName[sg.Name] = e
			}
			e.Count++
		}
	}
	for _, e := range byName {
		v.Summary = append(v.Summary, *e)
	}
	sort.Slice(v.Summary, func(i, j int) bool {
		return v.Summary[i].Count > v.Summary[j].Count || (v.Summary[i].Count == v.Summary[j].Count && v.Summary[i].Signal < v.Summary[j].Signal)
	})
	// severity first, then reach: reviews involved plus how far they would move the rating
	impact := v.Rating.Naive - v.Rating.Shown
	if impact < 0 {
		impact = -impact
	}
	v.Order = float64(map[string]int{"low": 1, "medium": 2, "high": 3}[c.Severity])*1000 + float64(c.Reach)*10 + impact*10
	return v
}

func (s *Server) adminReviews(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	open, decided := []caseView{}, []caseView{}
	for _, c := range s.store.ReviewCases() {
		if c.Status == "open" {
			open = append(open, s.caseView(c))
		} else {
			decided = append(decided, s.caseView(c))
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].Order > open[j].Order })
	sort.Slice(decided, func(i, j int) bool { return decided[i].DecidedAt.After(decided[j].DecidedAt) })
	if len(decided) > 30 {
		decided = decided[:30]
	}
	all := s.store.Reviews(nil)
	ratings := []reviews.Rating{}
	names := map[string]string{}
	seen := map[string]bool{}
	for _, x := range all {
		if !seen[x.ProductID] {
			seen[x.ProductID] = true
			ratings = append(ratings, s.rg.Rating(x.ProductID))
			p, _ := s.store.Product(x.ProductID)
			names[x.ProductID] = p.Name
		}
	}
	recent := []model.Review{}
	for i := len(all) - 1; i >= 0 && len(recent) < 40; i-- {
		recent = append(recent, all[i])
	}
	appeals := s.store.Appeals()
	sort.Slice(appeals, func(i, j int) bool { return appeals[i].CreatedAt.After(appeals[j].CreatedAt) })
	var latest any
	if e := evals.Latest(s.cfg.DataDir); e != nil && e.Reviews != nil {
		latest = e.Reviews // per-rule precision/recall, per language, gates (make eval / Evals tab)
	}
	writeJSON(w, 200, map[string]any{
		"tagger": s.rg.TaggerName(), "router_mode": s.cfg.Router, "policy_version": s.rg.P.Version, "prompt_version": reviews.PromptVersion,
		"sim": s.cfg.SimAgents, "cases": open, "decided": decided, "appeals": appeals, "ratings": ratings, "product_names": names,
		"recent": recent, "audit": s.store.AuditLog(60), "sanctions": s.store.Sanctions(), "metrics": s.rg.Metrics(),
		"eval": latest, "scenarios": reviews.Scenarios})
}

func (s *Server) reviewPolicy(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, 200, s.rg.P)
}

func (s *Server) reviewCase(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	c, ok := s.store.ReviewCase(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such case"})
		return
	}
	rs := []model.Review{}
	for _, id := range c.ReviewIDs {
		if x, ok := s.store.Review(id); ok {
			rs = append(rs, x)
		}
	}
	writeJSON(w, 200, map[string]any{"case": s.caseView(c), "reviews": rs, "sanction_kinds": reviews.SanctionKinds})
}

func (s *Server) decideReviewCase(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct {
		Decision  string   `json:"decision"`
		Note      string   `json:"note"`
		Sanctions []string `json:"sanctions"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	c, err := s.rg.DecideCase(r.PathValue("id"), s.moderator(sess), in.Decision, in.Note, in.Sanctions)
	if err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, s.caseView(c))
}

func (s *Server) decideReviewAppeal(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct{ Decision, Note string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	a, err := s.rg.DecideAppeal(r.PathValue("id"), s.moderator(sess), in.Decision, in.Note)
	if err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, a)
}

// demoAppeal lets a clearly labelled simulated second moderator decide (demo only).
func (s *Server) demoAppeal(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	if !s.cfg.SimAgents {
		writeJSON(w, 403, map[string]string{"error": "simulated moderators are off (SIM_AGENTS=0)"})
		return
	}
	decision, note := s.rg.DemoAppealDecision(r.PathValue("id"))
	a, err := s.rg.DecideAppeal(r.PathValue("id"), reviews.DemoModerator, decision, note)
	if err != nil {
		reviewErr(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) reviewSim(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct{ Scenario string }
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if ok, _ := s.rvLimit.Allow("sim:" + sess.ID); !ok && in.Scenario != "reset" {
		writeJSON(w, 429, map[string]string{"error": "one simulation at a time, please wait a few seconds"})
		return
	}
	res, err := s.rg.Simulate(r.Context(), in.Scenario, sess.Email)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	counts := map[string]int{}
	for _, x := range res.Reviews {
		cur, _ := s.store.Review(x.ID)
		counts[cur.Action]++
	}
	writeJSON(w, 200, map[string]any{"scenario": res.Scenario, "note": res.Note, "reviews": len(res.Reviews), "actions": counts,
		"rechecked": len(res.Changed), "synthetic": true})
}

// ---------------- product page ----------------

type pageReview struct {
	Verified  bool
	Counted   bool
	Stars     int
	StarsText string
	Text      string
	Date      string
	Synthetic bool
}

var productTmpl = template.Must(template.New("p").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.P.Name}} · MyFlipKart.com</title>
<link rel="stylesheet" href="/static/style.css"><script src="/dist/theme.js"></script></head><body>
<main class="pp"><a class="back" href="/#shop">&larr; Shop</a>
<div class="pp-grid"><img class="pp-img" src="/img/{{.P.ID}}.svg" alt="{{.P.Name}}">
<div><p class="muted">{{.P.Brand}} · {{.P.Category}} · sold by {{.Seller}}</p><h1>{{.P.Name}}</h1>
<div class="pp-price">${{printf "%.0f" .P.Price}}</div>
<p class="rating" id="rating" data-pid="{{.P.ID}}"><span class="stars" aria-hidden="true">{{.StarsText}}</span> <b>{{printf "%.1f" .R.Shown}}</b>
<span class="muted">{{if .R.N}}based on {{.R.N}} verified review{{if ne .R.N 1}}s{{end}}{{else}}no verified reviews yet (category average){{end}}</span></p>
{{if .R.Note}}<p class="note rv-note">{{.R.Note}}</p>{{end}}
<p class="muted">{{if .P.Stock}}In stock{{else}}Out of stock{{end}} · {{.P.ID}}</p>
<p>{{.P.Description}}</p><ul class="feat">{{range .P.Features}}<li>{{.}}</li>{{end}}</ul>
<a class="btn" href="/?ask={{.P.ID}}">Ask Sauby about this</a></div></div>
<section class="rv-section"><h2>Reviews</h2>
<p class="note">Only reviews from verified buyers count toward the rating. Every review is checked by ReviewGuard; honest negative reviews are always published.</p>
<div id="rv-mine"></div>
{{range .Reviews}}<article class="rv"><div class="rv-head"><span class="stars" aria-label="{{.Stars}} of 5 stars">{{.StarsText}}</span>
<span class="muted">{{if .Verified}}Verified purchase{{else}}Unverified{{end}}{{if not .Counted}} · not counted in the rating{{end}} · {{.Date}}{{if .Synthetic}} · <span class="pill">synthetic demo</span>{{end}}</span></div><p>{{.Text}}</p></article>
{{else}}<p class="muted">No published reviews yet.</p>{{end}}
</section>
<h3>Returns &amp; warranty</h3>{{range .Policies}}<details><summary>{{.Title}}</summary>{{range .Paras}}<p>{{.}}</p>{{end}}</details>{{end}}
</main><script type="module" src="/dist/reviews.js"></script></body></html>`))

func starsText(n float64) string {
	full := int(n + 0.5)
	return strings.Repeat("★", full) + strings.Repeat("☆", 5-full)
}

func (s *Server) productPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Product(strings.ToUpper(r.PathValue("id")))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var pols []model.PolicyDoc
	for _, d := range s.store.Policies() {
		if d.ID == "returns" || d.ID == "refunds" || d.ID == "warranty" {
			pols = append(pols, d)
		}
	}
	rt := s.rg.Rating(p.ID)
	var rs []pageReview
	list := s.store.Reviews(func(x model.Review) bool { return x.ProductID == p.ID && x.Status == "published" })
	for i := len(list) - 1; i >= 0 && len(rs) < 50; i-- { // newest first; published reviews only
		x := list[i]
		rs = append(rs, pageReview{x.Verified, x.Counted, x.Stars, starsText(float64(x.Stars)), x.Shown, x.CreatedAt.Format("Jan 2, 2006"), x.Synthetic})
	}
	productTmpl.Execute(w, map[string]any{"P": p, "Policies": pols, "R": rt, "StarsText": starsText(rt.Shown), "Reviews": rs, "Seller": seed.SellerOf(p)})
}

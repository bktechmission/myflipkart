// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package api wires HTTP: auth (OIDC + sessions), authZ, rate limits, chat (SSE), admin.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"asksauby/internal/agent"
	"asksauby/internal/auth"
	"asksauby/internal/config"
	"asksauby/internal/evals"
	"asksauby/internal/live"
	"asksauby/internal/llm"
	"asksauby/internal/mockid"
	"asksauby/internal/model"
	"asksauby/internal/obs"
	"asksauby/internal/search"
	"asksauby/internal/store"
)

const sessionCookie, stateCookie = "sauby_session", "sauby_oidc"

type Server struct {
	cfg       config.Config
	store     *store.Store
	retriever *search.Retriever
	gw        *llm.Gateway
	agent     *agent.Agent // control
	canary    *agent.Agent // optional candidate agent for a % of sessions
	tools     *agent.Tools
	Hub       *live.Hub // human handoff: queue, agent console, simulated teammates
	signer    auth.Signer
	providers map[string]*auth.Provider
	provMu    sync.Mutex
	mockByURL map[string]*auth.Provider // embedded MockID: one provider per public origin
	idp       *mockid.IdP
	chatLimit *TokenBucket
	authLimit *TokenBucket
	quota     *Quota

	evalMu    sync.Mutex
	evalState map[string]any
}

func New(cfg config.Config, st *store.Store, r *search.Retriever, gw, canaryGW *llm.Gateway) *Server {
	s := &Server{cfg: cfg, store: st, retriever: r, gw: gw, signer: auth.Signer{Key: []byte(cfg.SessionSecret)},
		chatLimit: NewTokenBucket(cfg.RatePerMin, cfg.RateBurst), authLimit: NewTokenBucket(20, 10),
		quota: NewQuota(cfg.DailyTokenQuota), providers: map[string]*auth.Provider{},
		evalState: map[string]any{"running": false}}
	tools := &agent.Tools{S: st, R: r, ReturnWindow: cfg.ReturnWindowDays,
		AutoRefundLimit: cfg.AutoRefundLimit, MaxAutoRefunds: cfg.MaxAutoRefunds}
	s.tools = tools
	s.agent = agent.New(tools, st, gw, cfg.Router, "chat")
	s.agent.Variant = "control"
	s.Hub = &live.Hub{S: st, Tools: tools, Sim: cfg.SimAgents, JoinAfter: time.Duration(cfg.SimJoinAfterSec * float64(time.Second)),
		Router: func() agent.Router { return s.agent.Router() }}
	if cfg.CanaryPercent > 0 && canaryGW != nil {
		s.canary = agent.New(tools, st, canaryGW, cfg.CanaryRouter, "chat")
		s.canary.Variant = "canary"
	}
	if cfg.GoogleClientID != "" {
		s.providers["google"] = &auth.Provider{Name: "google", Label: "Google", Issuer: "https://accounts.google.com",
			ExtraIssuers: []string{"accounts.google.com"}, ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret}
	}
	if cfg.MockClientSecret == "" {
		log.Println("MockID login disabled: MOCK_IDP_CLIENT_SECRET not set (run `make setup`)")
	}
	s.mockByURL = map[string]*auth.Provider{}
	if cfg.MockEmbedded { // public demo: the IdP lives inside this process at /mockid
		s.idp, _ = mockid.New(mockid.Config{Base: "/mockid", ClientID: cfg.MockClientID, ClientSecret: cfg.MockClientSecret,
			Password: os.Getenv("MOCK_IDP_PASSWORD"), AllowOther: !cfg.PublicDemo}, "")
	}
	s.providers["mock"] = &auth.Provider{Name: "mock", Label: "MockID (local dev)", Issuer: cfg.MockIssuer,
		ClientID: cfg.MockClientID, ClientSecret: cfg.MockClientSecret}
	return s
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	if s.idp != nil {
		m.Handle("/mockid/", s.idp.Handler())
	}
	web := s.cfg.WebDir
	page := func(name string, needLogin, needAdmin bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			sess := s.session(r)
			if needLogin && sess == nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			if needAdmin && sess.Role != "admin" {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<p style="font-family:system-ui;padding:40px">403 — your account isn't an Ask Sauby admin. <a href="/">Back to chat</a></p>`)
				return
			}
			http.ServeFile(w, r, filepath.Join(web, name))
		}
	}
	m.HandleFunc("GET /{$}", page("index.html", true, false))
	m.HandleFunc("GET /login", page("login.html", false, false))
	m.HandleFunc("GET /admin", page("admin.html", true, true))
	m.Handle("GET /dist/", http.FileServer(http.Dir(web)))
	m.Handle("GET /static/", http.FileServer(http.Dir(web)))
	m.HandleFunc("GET /p/{id}", s.productPage)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.health()) })

	m.HandleFunc("GET /auth/providers", s.authProviders)
	m.HandleFunc("GET /auth/login", s.authLogin)
	m.HandleFunc("GET /auth/callback", s.authCallback)
	m.HandleFunc("POST /auth/guest", s.authGuest)
	m.HandleFunc("POST /auth/logout", s.authLogout)

	m.HandleFunc("GET /api/me", s.withAuth(s.me, false))
	m.HandleFunc("GET /api/orders", s.withAuth(s.myOrders, false))
	m.HandleFunc("GET /api/orders/{id}", s.withAuth(s.myOrder, false))
	m.HandleFunc("GET /api/sessions", s.withAuth(s.sessions, false))
	m.HandleFunc("POST /api/sessions", s.withAuth(s.newSession, false))
	m.HandleFunc("GET /api/sessions/{sid}", s.withAuth(s.sessionMessages, false))
	m.HandleFunc("GET /api/sessions/{sid}/live", s.withAuth(s.liveState, false))
	m.HandleFunc("POST /api/sessions/{sid}/photo", s.withAuth(s.uploadPhoto, false))
	m.HandleFunc("GET /api/photos/{cid}/{id}", s.withAuth(s.photo, false))
	m.HandleFunc("GET /api/products", s.withAuth(s.products, false))
	m.HandleFunc("GET /img/{file}", s.productImage)
	m.HandleFunc("GET /metrics", obs.Default.Handler(s.cfg.MetricsToken))
	m.HandleFunc("POST /api/chat", s.withAuth(s.chat, false))
	m.HandleFunc("POST /api/feedback", s.withAuth(s.feedback, false))

	m.HandleFunc("GET /api/admin/metrics", s.withAuth(s.metrics, true))
	m.HandleFunc("GET /api/admin/evals/latest", s.withAuth(s.evalsLatest, true))
	m.HandleFunc("GET /api/admin/evals/history", s.withAuth(s.evalsHistory, true))
	m.HandleFunc("POST /api/admin/evals/run", s.withAuth(s.evalsRun, true))
	m.HandleFunc("POST /api/admin/reset-orders", s.withAuth(s.resetOrders, true))
	m.HandleFunc("GET /api/admin/approvals", s.withAuth(s.approvals, true))
	m.HandleFunc("POST /api/admin/approvals/{id}", s.withAuth(s.decideApproval, true))
	m.HandleFunc("POST /api/admin/orders/{id}/receive", s.withAuth(s.receiveReturn, true))
	m.HandleFunc("GET /api/admin/knowledge", s.withAuth(s.knowledge, true))
	m.HandleFunc("PUT /api/admin/policies/{id}", s.withAuth(s.savePolicy, true))
	m.HandleFunc("PUT /api/admin/products/{id}", s.withAuth(s.saveProduct, true))
	m.HandleFunc("GET /api/admin/insights", s.withAuth(s.insights, true))
	m.HandleFunc("GET /api/admin/transcripts/{cid}/{sid}", s.withAuth(s.transcript, true))
	m.HandleFunc("POST /api/admin/golden", s.withAuth(s.addGolden, true))
	m.HandleFunc("GET /api/admin/search", s.withAuth(s.searchProbe, true))
	m.HandleFunc("GET /api/admin/live", s.withAuth(s.liveQueue, true))
	m.HandleFunc("POST /api/admin/live/{id}/join", s.withAuth(s.liveJoin, true))
	m.HandleFunc("POST /api/admin/live/{id}/message", s.withAuth(s.liveSend, true))
	m.HandleFunc("POST /api/admin/live/{id}/close", s.withAuth(s.liveClose, true))
	return s.secure(m)
}

// ---------------- middleware ----------------

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic: %v", v)
				http.Error(w, "internal error", 500)
			}
		}()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https://*.googleusercontent.com; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		if s.isHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		limit := int64(64 << 10)
		if strings.HasSuffix(r.URL.Path, "/photo") {
			limit = maxPhotoBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		// CSRF: state-changing API calls must carry a custom header, which a
		// cross-site form or image tag cannot add (and SameSite=Lax cookies).
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-Sauby") != "1" {
			writeJSON(w, 403, map[string]string{"error": "missing X-Sauby header"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

func (s *Server) session(r *http.Request) *auth.Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var sess auth.Session
	if s.signer.Verify(c.Value, &sess) != nil || time.Now().Unix() > sess.Exp {
		return nil
	}
	return &sess
}

// withAuth = authentication (valid session) + coarse authorization (admin routes).
// Fine-grained authZ (which orders) lives in the tools, keyed by the session's cid.
func (s *Server) withAuth(h func(http.ResponseWriter, *http.Request, *auth.Session), admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil {
			writeJSON(w, 401, map[string]string{"error": "sign in required"})
			return
		}
		if admin && sess.Role != "admin" {
			writeJSON(w, 403, map[string]string{"error": "admin role required"})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)), sess)
	}
}

// base is the public origin the browser uses: PUBLIC_URL, or (PUBLIC_URL=auto) the
// request's own Host, so one image runs on any domain (workers.dev, custom domain).
func (s *Server) base(r *http.Request) string {
	if s.cfg.PublicURL == "auto" {
		return mockid.Origin(r)
	}
	return s.cfg.PublicURL
}

func (s *Server) isHTTPS(r *http.Request) bool { return strings.HasPrefix(s.base(r), "https://") }

// provider returns the sign-in provider; the embedded MockID gets one per origin.
func (s *Server) provider(name string, r *http.Request) *auth.Provider {
	if name != "mock" || s.idp == nil {
		return s.providers[name]
	}
	origin := s.base(r)
	s.provMu.Lock()
	defer s.provMu.Unlock()
	if p := s.mockByURL[origin]; p != nil {
		return p
	}
	pub, _ := url.Parse(origin)
	local := "127.0.0.1" + s.cfg.Addr
	if strings.HasPrefix(s.cfg.Addr, "127.") || strings.HasPrefix(s.cfg.Addr, "localhost") {
		local = s.cfg.Addr
	}
	p := &auth.Provider{Name: "mock", Label: "Demo accounts", Issuer: origin + "/mockid",
		ClientID: s.cfg.MockClientID, ClientSecret: s.cfg.MockClientSecret,
		Via: func(req *http.Request) { // call ourselves over loopback, keep the public identity
			req.URL.Scheme, req.URL.Host = "http", local
			req.Host = pub.Host
			req.Header.Set("X-Forwarded-Proto", pub.Scheme)
		}}
	s.mockByURL[origin] = p
	return p
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.signer.Sign(sess), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r),
		Expires: time.Unix(sess.Exp, 0)})
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// ---------------- auth handlers ----------------

func (s *Server) redirectURL(r *http.Request) string { return s.base(r) + "/auth/callback" }

func (s *Server) authProviders(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, name := range []string{"google", "mock"} {
		if p := s.provider(name, r); p != nil && (name != "mock" || s.cfg.MockClientSecret != "") {
			out = append(out, map[string]any{"name": name, "label": p.Label})
		}
	}
	writeJSON(w, 200, map[string]any{"providers": out, "google_configured": s.cfg.GoogleClientID != ""})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if ok, _ := s.authLimit.Allow("ip:" + clientIP(r)); !ok {
		http.Error(w, "too many login attempts", 429)
		return
	}
	p := s.provider(r.URL.Query().Get("provider"), r)
	ok := p != nil
	if !ok {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Unknown or unconfigured sign-in provider"), http.StatusFound)
		return
	}
	st := auth.LoginState{Provider: p.Name, State: auth.RandToken(16), Nonce: auth.RandToken(16),
		Verifier: auth.RandToken(32), Exp: time.Now().Add(10 * time.Minute).Unix()}
	u, err := p.AuthCodeURL(st, s.redirectURL(r))
	if err != nil {
		log.Printf("login %s: %v", p.Name, err)
		http.Redirect(w, r, "/login?error="+url.QueryEscape(p.Label+" is unreachable. Is it running?"), http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: s.signer.Sign(st), Path: "/auth", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r), MaxAge: 600})
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		log.Printf("auth callback: %s", msg)
		http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusFound)
	}
	c, err := r.Cookie(stateCookie)
	var st auth.LoginState
	if err != nil || s.signer.Verify(c.Value, &st) != nil || time.Now().Unix() > st.Exp {
		fail("Sign-in expired. Please try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth", MaxAge: -1})
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail("Sign-in was cancelled (" + e + ").")
		return
	}
	if q.Get("state") != st.State { // CSRF on the login flow
		fail("Sign-in state mismatch.")
		return
	}
	p := s.provider(st.Provider, r)
	if p == nil {
		fail("Unknown provider.")
		return
	}
	claims, err := p.Exchange(q.Get("code"), st.Verifier, s.redirectURL(r), st.Nonce)
	if err != nil {
		fail("Could not verify your identity: " + err.Error())
		return
	}
	u, err := s.store.LoginUser(p.Name, claims.Sub, claims.Email, claims.Name, claims.Picture)
	if err != nil {
		fail("Could not save your account.")
		return
	}
	role := "customer"
	if s.cfg.AdminEmails[u.Email] {
		role = "admin"
	}
	s.setSession(w, r, auth.Session{CID: u.CustomerID, Email: u.Email, Name: u.Name, Picture: u.Picture, Role: role,
		Provider: p.Name, Exp: time.Now().Add(12 * time.Hour).Unix(), ID: auth.RandToken(12)})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) authGuest(w http.ResponseWriter, r *http.Request) {
	if ok, _ := s.authLimit.Allow("ip:" + clientIP(r)); !ok {
		writeJSON(w, 429, map[string]string{"error": "slow down"})
		return
	}
	id := auth.RandToken(9)
	s.setSession(w, r, auth.Session{CID: "G-" + strings.NewReplacer("-", "x", "_", "y").Replace(id), Name: "Guest", Role: "guest",
		Provider: "guest", Exp: time.Now().Add(2 * time.Hour).Unix(), ID: id})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---------------- customer API ----------------

func (s *Server) me(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	writeJSON(w, 200, map[string]any{"cid": sess.CID, "email": sess.Email, "name": sess.Name, "picture": sess.Picture,
		"role": sess.Role, "provider": sess.Provider, "health": s.health()})
}

func (s *Server) myOrders(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	type item struct {
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		Image string  `json:"image"`
		Price float64 `json:"price"`
	}
	type view struct {
		model.Order
		Lines []item `json:"lines"`
	}
	out := []view{}
	orders := s.store.OrdersFor(sess.CID)   // only ever this customer's orders
	for i := len(orders) - 1; i >= 0; i-- { // newest first
		v := view{Order: orders[i]}
		for _, id := range orders[i].Items {
			p, _ := s.store.Product(id)
			v.Lines = append(v.Lines, item{p.ID, p.Name, "/img/" + p.ID + ".svg", p.Price})
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

func (s *Server) myOrder(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	o, ok := s.store.Order(strings.ToUpper(r.PathValue("id")))
	if !ok || o.CustomerID != sess.CID {
		writeJSON(w, 404, map[string]string{"error": "order not found"}) // 404, not 403: don't confirm it exists
		return
	}
	rf, has := s.store.RefundFor(o.ID)
	out := map[string]any{"order": o}
	if has {
		out["refund"] = rf
	}
	writeJSON(w, 200, out)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	out := s.store.Sessions(sess.CID)
	if out == nil {
		out = []store.SessionInfo{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	writeJSON(w, 200, map[string]string{"sid": newSID()})
}

func (s *Server) sessionMessages(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	msgs, err := s.store.Messages(sess.CID, r.PathValue("sid")) // scoped by cid: can't read others' chats
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "conversation not found"})
		return
	}
	writeJSON(w, 200, msgs)
}

func (s *Server) feedback(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct {
		SID     string `json:"sid"`
		Helpful bool   `json:"helpful"`
		Outcome string `json:"outcome"`
		Comment string `json:"comment"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || !store.ValidSID(in.SID) || len(in.Comment) > 1000 {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	s.store.AddFeedback(model.Feedback{TS: time.Now().UTC(), CustomerID: sess.CID, SessionID: in.SID, Helpful: in.Helpful,
		Outcome: in.Outcome, Comment: strings.TrimSpace(in.Comment), Variant: s.agentFor(in.SID).Variant})
	if in.Helpful || in.Comment != "" {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// "No, this didn't solve it": don't make the customer ask twice, bring in a person now.
	res := s.tools.Run("handoff", map[string]any{"summary": "Customer said the issue was not resolved (" + in.Outcome + ")"}, sess.CID, in.SID, false)
	if res.OK && res.Ticket != nil {
		s.store.AppendMessage(sess.CID, in.SID, model.Message{TS: time.Now().UTC(), Role: "system",
			Content: "Sorry this didn't solve it. Connecting you with a teammate now; they'll see this conversation."})
		s.store.LogEvent(model.Event{TS: time.Now().UTC(), CustomerID: sess.CID, SessionID: in.SID, Query: "feedback: not resolved",
			Tool: "handoff", Path: "handoff", OK: true, Status: 200, Source: "chat", Handoff: true, Variant: s.agentFor(in.SID).Variant,
			Flags: []string{"unresolved_feedback"}})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "live": res.OK && res.Ticket != nil, "message": res.Message})
}

// chat streams Server-Sent Events: "status" events while work happens (so the
// customer sees Sauby is busy during slow steps), then one "answer" event.
func (s *Server) chat(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct {
		SID     string `json:"sid"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !store.ValidSID(in.SID) {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" || len(msg) > 2000 {
		writeJSON(w, 400, map[string]string{"error": "message must be 1-2000 characters"})
		return
	}
	// per browser session, so many visitors sharing a demo account don't throttle each other
	if ok, wait := s.chatLimit.Allow("s:" + sess.ID); !ok {
		s.store.LogEvent(model.Event{TS: time.Now().UTC(), CustomerID: sess.CID, SessionID: in.SID, Query: msg, Tool: "rate_limited",
			Status: 429, Source: "chat", Flags: []string{"rate_limited"}})
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		writeJSON(w, 429, map[string]string{"error": fmt.Sprintf("You're sending messages quickly — try again in %ds.", int(wait.Seconds())+1)})
		return
	}
	if s.quota.Exceeded(sess.CID) {
		writeJSON(w, 429, map[string]string{"error": "Daily assistant limit reached. Please try again tomorrow."})
		return
	}
	// A person owns this conversation: Sauby stays silent and the message goes to them.
	if t, owned := s.Hub.HumanOwns(in.SID); owned && t.CustomerID == sess.CID {
		s.store.AppendMessage(sess.CID, in.SID, model.Message{TS: time.Now().UTC(), Role: "user", Content: msg})
		s.Hub.CustomerMessage(t, msg)
		obs.Default.Inc("sauby_turns_total", "Chat turns by tool, path and agent version.", "tool", "live", "path", "handoff", "variant", "human")
		writeJSON(w, 200, map[string]any{"live": s.Hub.StateFor(sess.CID, in.SID, 1<<30)})
		return
	}
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp := s.agentFor(in.SID).Handle(ctx, sess.CID, in.SID, msg, func(st string) {
		if st != "" {
			send("status", map[string]string{"text": st})
		}
	})
	s.quota.Add(sess.CID, resp.Trace.Tokens)
	send("answer", resp)
}

// agentFor pins a session to one agent version (sticky by session hash), so a
// conversation never switches agents mid-way during a canary rollout.
func (s *Server) agentFor(sid string) *agent.Agent {
	if s.canary == nil {
		return s.agent
	}
	h := 0
	for _, c := range sid {
		h = (h*31 + int(c)) % 1000003
	}
	if h%100 < s.cfg.CanaryPercent {
		return s.canary
	}
	return s.agent
}

func (s *Server) health() map[string]any {
	prov := map[string]any{}
	if s.gw.Internal != nil {
		prov["internal"] = map[string]any{"model": s.gw.Internal.Model, "up": s.gw.Internal.Up()}
	}
	if s.gw.External != nil {
		prov["external"] = map[string]any{"model": s.gw.External.Model, "up": s.gw.External.Up()}
	}
	h := map[string]any{"router": s.agent.RouterName(), "router_mode": s.cfg.Router, "llm": prov,
		"embeddings": s.retriever.Stats()["embeddings"], "retriever": s.cfg.Retriever,
		"google_login": s.cfg.GoogleClientID != "", "auto_refund_limit": s.cfg.AutoRefundLimit}
	if s.canary != nil {
		h["canary"] = map[string]any{"percent": s.cfg.CanaryPercent, "router": s.canary.RouterName(), "model": s.canary.GW.Internal.Model}
	}
	return h
}

// productTmpl is the server-rendered product page (good for SEO and link previews).
var productTmpl = template.Must(template.New("p").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.P.Name}} · MyFlipKart.com</title>
<link rel="stylesheet" href="/static/style.css"><script src="/dist/theme.js"></script></head><body>
<main class="pp"><a class="back" href="/#shop">&larr; Shop</a>
<div class="pp-grid"><img class="pp-img" src="/img/{{.P.ID}}.svg" alt="{{.P.Name}}">
<div><p class="muted">{{.P.Brand}} · {{.P.Category}}</p><h1>{{.P.Name}}</h1>
<div class="pp-price">${{printf "%.0f" .P.Price}}</div>
<p class="muted">★ {{.P.Rating}} · {{if .P.Stock}}In stock{{else}}Out of stock{{end}} · {{.P.ID}}</p>
<p>{{.P.Description}}</p><ul class="feat">{{range .P.Features}}<li>{{.}}</li>{{end}}</ul>
<a class="btn" href="/?ask={{.P.ID}}">Ask Sauby about this</a></div></div>
<h3>Returns &amp; warranty</h3>{{range .Policies}}<details><summary>{{.Title}}</summary>{{range .Paras}}<p>{{.}}</p>{{end}}</details>{{end}}
</main></body></html>`))

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
	productTmpl.Execute(w, map[string]any{"P": p, "Policies": pols})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func newSID() string { return strings.ToLower(fmt.Sprintf("%x", []byte(auth.RandToken(12)))[:24]) }

// ---------------- admin: evals ----------------

func (s *Server) evalsLatest(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	s.evalMu.Lock()
	st := s.evalState
	s.evalMu.Unlock()
	writeJSON(w, 200, map[string]any{"state": st, "result": evals.Latest(s.cfg.DataDir)})
}

func (s *Server) evalsHistory(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, 200, evals.History(s.cfg.DataDir))
}

func (s *Server) evalsRun(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct{ Suite, Router, Label string }
	json.NewDecoder(r.Body).Decode(&in)
	s.evalMu.Lock()
	if started, ok := s.evalState["started"].(time.Time); ok && s.cfg.PublicDemo && time.Since(started) < time.Minute {
		s.evalMu.Unlock()
		writeJSON(w, 429, map[string]string{"error": "public demo: one eval run per minute"})
		return
	}
	if s.evalState["running"] == true {
		s.evalMu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "an eval run is already in progress"})
		return
	}
	s.evalState = map[string]any{"running": true, "started": time.Now().UTC(), "by": sess.Email, "log": []string{}}
	s.evalMu.Unlock()
	suites := evals.AllSuites
	if in.Suite != "" && in.Suite != "all" {
		suites = []string{in.Suite}
	}
	label := in.Label
	if label == "" {
		label = "from admin"
	}
	go func() {
		res, err := evals.Run(evals.Options{Cfg: s.cfg, Suites: suites, Router: in.Router, Label: label,
			Retriever: s.retriever, GW: s.gw, Log: func(m string) {
				s.evalMu.Lock()
				s.evalState["log"] = append(s.evalState["log"].([]string), m)
				s.evalMu.Unlock()
			}})
		s.evalMu.Lock()
		defer s.evalMu.Unlock()
		s.evalState["running"] = false
		if err != nil {
			s.evalState["error"] = err.Error()
		} else {
			s.evalState["run_id"] = res.RunID
		}
	}()
	writeJSON(w, 200, map[string]any{"started": true, "suites": suites})
}

func (s *Server) resetOrders(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	if err := s.store.ResetOrders(s.cfg.DemoOrdersFor); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

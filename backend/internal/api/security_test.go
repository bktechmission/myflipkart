// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Security tests over the real HTTP handler: authentication, admin-only routes,
// CSRF, tampered/expired sessions, cross-customer access (orders, chats, live
// chats, photos), upload validation, body limits, headers and stored XSS.
package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/store"
	"asksauby/internal/testkit"
)

type env struct {
	t   *testing.T
	s   *Server
	h   http.Handler
	jar map[string]*http.Cookie
}

func newEnv(t *testing.T) *env {
	cfg := testkit.Config()
	cfg.SessionSecret = "test-secret-0123456789abcdef0123456789"
	cfg.MockEmbedded, cfg.MockClientSecret, cfg.PublicDemo = true, "client-secret-0123456789", true
	cfg.SimAgents = false
	cfg.RatePerMin, cfg.RateBurst = 600, 100
	_, file, _, _ := runtime.Caller(0)
	cfg.PublicURL = "auto"
	cfg.WebDir = filepath.Join(filepath.Dir(file), "../../../web/public")
	cfg.AdminEmails = map[string]bool{"aarav@example.com": true}
	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, st, testkit.Retriever(), nil, nil)
	e := &env{t: t, s: s, h: s.Handler(), jar: map[string]*http.Cookie{}}
	for _, u := range []struct{ cid, email, role string }{{"C1001", "aarav@example.com", "admin"}, {"C1002", "meera@example.com", "customer"}, {"G-guest", "", "guest"}} {
		rec := httptest.NewRecorder()
		s.setSession(rec, httptest.NewRequest("GET", "/", nil), auth.Session{CID: u.cid, Email: u.email, Name: u.cid, Role: u.role,
			Exp: time.Now().Add(time.Hour).Unix(), ID: u.cid})
		e.jar[u.cid] = rec.Result().Cookies()[0]
	}
	return e
}

func (e *env) do(who, method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if c := e.jar[who]; c != nil {
		r.AddCookie(c)
	}
	if method != "GET" {
		r.Header.Set("X-Sauby", "1")
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{200, 10, 10, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestAuthenticationRequired(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/me", "/api/orders", "/api/sessions", "/api/admin/metrics", "/api/photos/C1001/0123456789abcdef"} {
		if w := e.do("", "GET", p, nil, nil); w.Code != 401 {
			t.Errorf("%s without a session: %d", p, w.Code)
		}
	}
	tampered := *e.jar["C1002"]
	tampered.Value = strings.Replace(tampered.Value, tampered.Value[:6], "AAAAAA", 1)
	e.jar["X"] = &tampered
	if w := e.do("X", "GET", "/api/me", nil, nil); w.Code != 401 {
		t.Errorf("tampered cookie accepted: %d", w.Code)
	}
	rec := httptest.NewRecorder()
	e.s.setSession(rec, httptest.NewRequest("GET", "/", nil), auth.Session{CID: "C1002", Role: "admin", Exp: time.Now().Add(-time.Minute).Unix()})
	e.jar["old"] = rec.Result().Cookies()[0]
	if w := e.do("old", "GET", "/api/me", nil, nil); w.Code != 401 {
		t.Errorf("expired cookie accepted: %d", w.Code)
	}
}

func TestAdminRoutesAreAdminOnly(t *testing.T) {
	e := newEnv(t)
	gets := []string{"/api/admin/metrics", "/api/admin/approvals", "/api/admin/live", "/api/admin/knowledge", "/api/admin/insights", "/api/admin/evals/latest"}
	posts := []string{"/api/admin/reset-orders", "/api/admin/approvals/AP-1", "/api/admin/orders/O5001/receive", "/api/admin/evals/run"}
	for _, who := range []string{"C1002", "G-guest"} {
		for _, p := range gets {
			if w := e.do(who, "GET", p, nil, nil); w.Code != 403 {
				t.Errorf("%s GET %s: %d", who, p, w.Code)
			}
		}
		for _, p := range posts {
			if w := e.do(who, "POST", p, []byte(`{"decision":"approve"}`), nil); w.Code != 403 {
				t.Errorf("%s POST %s: %d", who, p, w.Code)
			}
		}
		if w := e.do(who, "GET", "/admin", nil, nil); w.Code != 403 {
			t.Errorf("%s /admin page: %d", who, w.Code)
		}
	}
	if w := e.do("C1001", "GET", "/api/admin/approvals", nil, nil); w.Code != 200 {
		t.Errorf("admin blocked: %d", w.Code)
	}
}

func TestCSRFHeaderRequired(t *testing.T) {
	e := newEnv(t)
	w := e.do("C1002", "POST", "/api/chat", []byte(`{"sid":"`+e.s.newSID("C1002")+`","message":"cancel O5103"}`), map[string]string{"X-Sauby": ""})
	if w.Code != 403 {
		t.Fatalf("POST without X-Sauby: %d", w.Code)
	}
}

func TestOrdersAreScopedToTheCustomer(t *testing.T) {
	e := newEnv(t)
	var mine []map[string]any
	json.Unmarshal(e.do("C1002", "GET", "/api/orders", nil, nil).Body.Bytes(), &mine)
	for _, o := range mine {
		if o["id"].(string)[:3] != "O51" {
			t.Errorf("Meera sees %v", o["id"])
		}
	}
	if w := e.do("C1002", "GET", "/api/orders/O5001", nil, nil); w.Code == 200 {
		t.Error("Meera can read Aarav's order")
	}
}

func TestChatsAndLiveChatsAreScoped(t *testing.T) {
	e := newEnv(t)
	sid := e.s.newSID("C1001")
	e.do("C1001", "POST", "/api/chat", []byte(`{"sid":"`+sid+`","message":"talk to a person"}`), nil)
	if _, ok := e.s.store.OpenLiveTicket("C1001", sid); !ok {
		t.Fatal("handoff not queued")
	}
	// Meera reuses Aarav's session id
	w := e.do("C1002", "GET", "/api/sessions/"+sid, nil, nil)
	if strings.Contains(w.Body.String(), "talk to a person") {
		t.Fatal("Meera read Aarav's conversation")
	}
	if w := e.do("C1002", "GET", "/api/sessions/"+sid+"/live", nil, nil); w.Code != 404 || strings.Contains(w.Body.String(), "ticket") {
		t.Fatalf("Meera sees Aarav's live chat: %d %s", w.Code, w.Body.String())
	}
	e.do("C1002", "POST", "/api/chat", []byte(`{"sid":"`+sid+`","message":"hello agent, refund me"}`), nil)
	msgs, _ := e.s.store.Messages("C1001", sid)
	for _, m := range msgs {
		if strings.Contains(m.Content, "hello agent") {
			t.Fatal("Meera's message landed in Aarav's live chat")
		}
	}
	fb := e.do("C1002", "POST", "/api/feedback", []byte(`{"sid":"`+sid+`","helpful":false,"outcome":"resolved"}`), nil)
	if fb.Code == 200 || strings.Contains(fb.Body.String(), "already on the way") {
		t.Fatalf("feedback on another customer's conversation: %d %s", fb.Code, fb.Body.String())
	}
	if up := e.do("C1002", "POST", "/api/sessions/"+sid+"/photo", pngBytes(64, 64), nil); up.Code == 200 {
		t.Fatal("upload into another customer's conversation")
	}
}

func TestPhotoUploadAndAccess(t *testing.T) {
	e := newEnv(t)
	sid := e.s.newSID("C1002")
	up := func(who string, b []byte) *httptest.ResponseRecorder {
		return e.do(who, "POST", "/api/sessions/"+sid+"/photo", b, map[string]string{"Content-Type": "image/png"})
	}
	if w := up("C1002", pngBytes(64, 64)); w.Code != 409 {
		t.Fatalf("photo without a teammate in the chat: %d", w.Code)
	}
	e.do("C1002", "POST", "/api/chat", []byte(`{"sid":"`+sid+`","message":"talk to a person"}`), nil)
	tk, _ := e.s.store.OpenLiveTicket("C1002", sid)
	if _, err := e.s.Hub.Join(tk.ID, "Agent", "human"); err != nil {
		t.Fatal(err)
	}
	if w := up("G-guest", pngBytes(64, 64)); w.Code == 200 {
		t.Fatal("guest uploaded into someone's chat")
	}
	if w := up("C1001", pngBytes(64, 64)); w.Code == 200 {
		t.Fatal("another customer uploaded into Meera's chat")
	}
	for name, b := range map[string][]byte{
		"not an image": []byte("<script>alert(1)</script>"),
		"svg":          []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"tiny":         pngBytes(4, 4),
		"huge dims":    hugeHeader(),
	} {
		if w := up("C1002", b); w.Code == 200 {
			t.Errorf("%s accepted", name)
		}
	}
	if w := up("C1002", bytes.Repeat([]byte{0}, 5<<20)); w.Code == 200 {
		t.Error("5 MB upload accepted")
	}
	w := up("C1002", pngBytes(64, 64))
	if w.Code != 200 {
		t.Fatalf("valid photo rejected: %d %s", w.Code, w.Body.String())
	}
	var out struct{ URL string }
	json.Unmarshal(w.Body.Bytes(), &out)
	g := e.do("C1002", "GET", out.URL, nil, nil)
	if g.Code != 200 || g.Header().Get("Content-Type") != "image/jpeg" || !bytes.HasPrefix(g.Body.Bytes(), []byte{0xFF, 0xD8}) {
		t.Fatalf("owner can't read their photo, or it wasn't re-encoded: %d %s", g.Code, g.Header().Get("Content-Type"))
	}
	if g := e.do("G-guest", "GET", out.URL, nil, nil); g.Code != 404 {
		t.Errorf("guest read the photo: %d", g.Code)
	}
	if g := e.do("C1001", "GET", out.URL, nil, nil); g.Code != 200 { // C1001 is the admin
		t.Errorf("admin can't review the photo: %d", g.Code)
	}
}

// hugeHeader is a PNG header claiming 50000x50000 (a decompression bomb).
func hugeHeader() []byte {
	b := pngBytes(16, 16)
	copy(b[16:24], []byte{0, 0, 0xC3, 0x50, 0, 0, 0xC3, 0x50})
	return b
}

func TestBodyLimitAndBadInput(t *testing.T) {
	e := newEnv(t)
	big := `{"sid":"` + e.s.newSID("C1002") + `","message":"` + strings.Repeat("a", 70<<10) + `"}`
	if w := e.do("C1002", "POST", "/api/chat", []byte(big), nil); w.Code < 400 {
		t.Errorf("70 KB chat body accepted: %d", w.Code)
	}
	for _, b := range []string{`{"sid":"../../etc","message":"hi"}`, `{"sid":"` + e.s.newSID("C1002") + `","message":""}`,
		`{"sid":"0123456789abcdef0123abcd","message":"hi"}`, `not json`} {
		if w := e.do("C1002", "POST", "/api/chat", []byte(b), nil); w.Code != 400 {
			t.Errorf("bad chat input %q: %d", b, w.Code)
		}
	}
}

func TestSecurityHeadersAndCookies(t *testing.T) {
	e := newEnv(t)
	w := e.do("C1002", "GET", "/api/me", nil, nil)
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	c := e.jar["C1002"]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie flags: %+v", c)
	}
	r := httptest.NewRequest("POST", "/auth/guest", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if cs := rec.Result().Cookies(); len(cs) == 0 || !cs[0].Secure {
		t.Error("cookie must be Secure behind HTTPS")
	}
}

func TestStoredXSSIsEscaped(t *testing.T) {
	e := newEnv(t)
	p, _ := e.s.store.Product("P1001")
	p.Name = `<img src=x onerror=alert(1)>`
	e.s.store.SaveProduct(p)
	w := e.do("C1002", "GET", "/p/P1001", nil, nil)
	if strings.Contains(w.Body.String(), "<img src=x onerror") {
		t.Fatal("product page renders raw HTML from the catalog")
	}
}

func TestGuestCannotReachOrdersThroughChat(t *testing.T) {
	e := newEnv(t)
	w := e.do("G-guest", "POST", "/api/chat", []byte(`{"sid":"`+e.s.newSID("G-guest")+`","message":"refund O5001"}`), nil)
	body, _ := io.ReadAll(w.Body)
	if strings.Contains(string(body), "Sonix") || !strings.Contains(string(body), "sign in") {
		t.Fatalf("guest got order data: %s", body)
	}
}

func TestCrossSiteAuthPostsBlocked(t *testing.T) {
	e := newEnv(t)
	r := httptest.NewRequest("POST", "/auth/guest", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 403 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("cross-site guest sign-in: %d", w.Code)
	}
}

func TestClientIPOnlyTrustsTheProxyHop(t *testing.T) {
	trustProxy = true
	defer func() { trustProxy = false }()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7")
	if ip := clientIP(r); ip != "203.0.113.7" {
		t.Fatalf("spoofable left-most XFF used: %s", ip)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if ip := clientIP(r); ip != "192.0.2.1" {
		t.Fatalf("garbage XFF should fall back to the socket: %s", ip)
	}
}

func TestGuestChatLimitedByAddress(t *testing.T) {
	e := newEnv(t)
	e.s.chatLimit = NewTokenBucket(1, 2)
	codes := map[int]int{}
	for i := 0; i < 4; i++ { // a fresh guest session each time: the address still limits
		rec := httptest.NewRecorder()
		e.s.setSession(rec, httptest.NewRequest("GET", "/", nil), auth.Session{CID: "G-x" + string(rune('a'+i)), Role: "guest", Exp: time.Now().Add(time.Hour).Unix(), ID: "g" + string(rune('a'+i))})
		e.jar["g"] = rec.Result().Cookies()[0]
		w := e.do("g", "POST", "/api/chat", []byte(`{"sid":"`+e.s.newSID("G-x"+string(rune('a'+i)))+`","message":"headphones"}`), nil)
		codes[w.Code]++
	}
	if codes[429] < 2 {
		t.Fatalf("new guest sessions bypassed the limit: %v", codes)
	}
}

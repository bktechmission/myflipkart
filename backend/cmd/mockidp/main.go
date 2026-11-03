// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Command mockidp is a local OpenID Connect identity provider for development.
// It speaks the same protocol as Google (discovery, authorization code + PKCE,
// RS256-signed ID tokens, JWKS), so the Ask Sauby server uses one code path for
// both. It is NOT Google and is clearly labeled as a local dev tool.
//
//	go run ./cmd/mockidp        # http://localhost:9000
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/seed"
)

type account struct{ Email, Name, Initial, Color string }

type grant struct {
	ClientID, Redirect, Nonce, Challenge, Email, Name string
	Exp                                               time.Time
}

var (
	issuer       = env("MOCK_IDP_ISSUER", "http://localhost:9000")
	clientID     = env("MOCK_IDP_CLIENT_ID", "asksauby-local")
	clientSecret = os.Getenv("MOCK_IDP_CLIENT_SECRET") // shared with the server via .env

	redirects = strings.Split(env("MOCK_IDP_REDIRECTS", "http://localhost:8080/auth/callback"), ",")
	password  = os.Getenv("MOCK_IDP_PASSWORD") // empty = passwordless account chooser (public demo)
	kid       = "mock-1"
	key       *rsa.PrivateKey
	mu        sync.Mutex
	codes     = map[string]grant{}
	colors    = []string{"#4f46e5", "#0d9488", "#d97706", "#db2777"}
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func accounts() []account {
	var out []account
	for i, u := range seed.DemoUsers {
		out = append(out, account{u.Email, u.Name, strings.ToUpper(u.Name[:1]), colors[i%len(colors)]})
	}
	return out
}

func main() {
	key = loadKey(filepath.Join(env("DATA_DIR", "data"), "mockidp-key.pem"))
	http.HandleFunc("/.well-known/openid-configuration", discovery)
	http.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keys": []auth.JWK{auth.JWKFromKey(kid, &key.PublicKey)}})
	})
	http.HandleFunc("/authorize", authorize)
	http.HandleFunc("/token", token)
	if len(clientSecret) < 16 {
		log.Fatal("MOCK_IDP_CLIENT_SECRET is not set. Run `make setup` to create .env with random secrets.")
	}
	addr := env("MOCK_IDP_ADDR", ":9000")
	log.Printf("MockID %s on %s (client_id=%s, password step: %v)", issuer, addr, clientID, password != "")
	log.Fatal(http.ListenAndServe(addr, nil))
}

func loadKey(path string) *rsa.PrivateKey {
	if b, err := os.ReadFile(path); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
				return k
			}
		}
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	return k
}

func discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
		"jwks_uri": issuer + "/jwks", "response_types_supported": []string{"code"},
		"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported": []string{"openid", "email", "profile"}, "code_challenge_methods_supported": []string{"S256"},
	})
}

func authorize(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q := r.Form
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != clientID || !allowed(redirect) {
		http.Error(w, "unknown client_id or redirect_uri not registered", 400) // never redirect to an unknown URI
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		redirectErr(w, r, redirect, q.Get("state"), "invalid_request")
		return
	}
	data := map[string]any{"Accounts": accounts(), "Q": q, "Client": "Ask Sauby", "Hidden": hidden(q)}
	if r.Method == http.MethodPost && q.Get("email") != "" {
		email := strings.ToLower(strings.TrimSpace(q.Get("email")))
		data["Email"], data["Name"] = email, nameFor(email)
		if password != "" && q.Get("password") == "" { // step 2: password (skipped in passwordless demo mode)
			page.ExecuteTemplate(w, "password", data)
			return
		}
		if password != "" && subtle.ConstantTimeCompare([]byte(q.Get("password")), []byte(password)) != 1 {
			data["Error"] = "Wrong password. Try again."
			page.ExecuteTemplate(w, "password", data)
			return
		}
		code := auth.RandToken(24)
		mu.Lock()
		codes[code] = grant{clientID, redirect, q.Get("nonce"), q.Get("code_challenge"), email, nameFor(email), time.Now().Add(time.Minute)}
		mu.Unlock()
		u, _ := url.Parse(redirect)
		v := u.Query()
		v.Set("code", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}
	page.ExecuteTemplate(w, "choose", data)
}

func token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	fail := func(code int, e string) { w.WriteHeader(code); writeJSON(w, map[string]string{"error": e}) }
	if r.Method != http.MethodPost || r.Form.Get("grant_type") != "authorization_code" {
		fail(400, "unsupported_grant_type")
		return
	}
	if r.Form.Get("client_id") != clientID || subtle.ConstantTimeCompare([]byte(r.Form.Get("client_secret")), []byte(clientSecret)) != 1 {
		fail(401, "invalid_client")
		return
	}
	mu.Lock()
	g, ok := codes[r.Form.Get("code")]
	delete(codes, r.Form.Get("code")) // single use
	mu.Unlock()
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	switch {
	case !ok || time.Now().After(g.Exp):
		fail(400, "invalid_grant")
		return
	case g.Redirect != r.Form.Get("redirect_uri"):
		fail(400, "invalid_grant")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.Challenge:
		fail(400, "invalid_grant") // PKCE check failed
		return
	}
	now := time.Now().Unix()
	idt, err := auth.SignRS256(key, kid, map[string]any{
		"iss": issuer, "aud": clientID, "sub": "mock|" + g.Email, "iat": now, "exp": now + 3600,
		"nonce": g.Nonce, "email": g.Email, "email_verified": true, "name": g.Name,
	})
	if err != nil {
		fail(500, "server_error")
		return
	}
	writeJSON(w, map[string]any{"access_token": auth.RandToken(24), "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
}

func allowed(u string) bool {
	for _, a := range redirects {
		if strings.TrimSpace(a) == u {
			return true
		}
	}
	return false
}

func nameFor(email string) string {
	for _, a := range accounts() {
		if a.Email == email {
			return a.Name
		}
	}
	return strings.Split(email, "@")[0]
}

func hidden(q url.Values) map[string]string {
	h := map[string]string{}
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "scope", "state", "nonce", "code_challenge", "code_challenge_method"} {
		h[k] = q.Get(k)
	}
	return h
}

func redirectErr(w http.ResponseWriter, r *http.Request, redirect, state, e string) {
	http.Redirect(w, r, redirect+"?error="+url.QueryEscape(e)+"&state="+url.QueryEscape(state), http.StatusFound)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

var page = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MockID · Sign in</title><style>
:root{--bg:#f3f4f6;--card:#fff;--ink:#1f2937;--mute:#6b7280;--line:#e5e7eb;--accent:#0f766e}
@media (prefers-color-scheme:dark){:root{--bg:#111315;--card:#1b1d20;--ink:#e5e7eb;--mute:#9ca3af;--line:#2c2f33;--accent:#2dd4bf}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--ink);font:15px/1.5 ui-sans-serif,system-ui,sans-serif;padding:16px}
.card{width:100%;max-width:420px;background:var(--card);border:1px solid var(--line);border-radius:14px;padding:32px 28px}
.brand{display:flex;align-items:center;gap:8px;font-weight:700;letter-spacing:.02em}.brand i{width:26px;height:26px;border-radius:7px;background:var(--accent);display:inline-block;position:relative}
.brand i:after{content:"";position:absolute;inset:7px;border:2px solid var(--card);border-radius:50%}
h1{font-size:22px;font-weight:500;margin:22px 0 4px}p.sub{color:var(--mute);margin:0 0 20px}
.acct{display:flex;align-items:center;gap:12px;width:100%;padding:12px 6px;background:none;border:0;border-top:1px solid var(--line);color:var(--ink);text-align:left;cursor:pointer;font:inherit}
.acct:hover{background:rgba(127,127,127,.08)}.av{width:34px;height:34px;border-radius:50%;color:#fff;display:grid;place-items:center;font-weight:600}
.acct small{display:block;color:var(--mute)}input{width:100%;padding:12px;border:1px solid var(--line);border-radius:8px;background:transparent;color:var(--ink);font:inherit;margin:6px 0 4px}
.btn{background:var(--accent);color:#fff;border:0;border-radius:8px;padding:10px 20px;font:inherit;font-weight:600;cursor:pointer;float:right;margin-top:14px}
.err{color:#dc2626;font-size:13px}.foot{clear:both;margin-top:28px;font-size:12px;color:var(--mute);border-top:1px dashed var(--line);padding-top:12px}
.chip{display:inline-flex;gap:8px;align-items:center;border:1px solid var(--line);border-radius:99px;padding:3px 12px 3px 4px;margin-bottom:8px}
</style></head><body><main class="card"><div class="brand"><i></i>MockID</div>{{end}}
{{define "foot"}}<div class="foot">Local OpenID Connect provider for development and demos. Not affiliated with Google. The password (if any) is in your <code>.env</code>.</div></main></body></html>{{end}}

{{define "choose"}}{{template "head"}}
<h1>Choose an account</h1><p class="sub">to continue to <b>{{.Client}}</b></p>
{{range .Accounts}}<form method="post" action="/authorize">{{range $k,$v := $.Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<input type="hidden" name="email" value="{{.Email}}"><button class="acct"><span class="av" style="background:{{.Color}}">{{.Initial}}</span>
<span>{{.Name}}<small>{{.Email}}</small></span></button></form>{{end}}
<form method="post" action="/authorize" style="border-top:1px solid var(--line);padding-top:12px">{{range $k,$v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label class="sub" style="font-size:13px;color:var(--mute)">Use another account</label><input name="email" type="email" placeholder="email@example.com" required>
<button class="btn">Next</button></form>
{{template "foot"}}{{end}}

{{define "password"}}{{template "head"}}
<h1>Welcome</h1><div class="chip"><span class="av" style="background:#4f46e5;width:24px;height:24px;font-size:12px">{{slice .Name 0 1}}</span>{{.Email}}</div>
<form method="post" action="/authorize">{{range $k,$v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<input type="hidden" name="email" value="{{.Email}}"><label for="pw" style="font-size:13px;color:var(--mute)">Enter your password</label>
<input id="pw" name="password" type="password" autofocus autocomplete="off">{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<button class="btn">Sign in</button></form>
{{template "foot"}}{{end}}
`))

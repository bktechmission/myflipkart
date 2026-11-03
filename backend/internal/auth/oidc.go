// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider is an OIDC identity provider (Google, or the local mock IdP).
type Provider struct {
	Name, Label  string
	Issuer       string   // e.g. https://accounts.google.com
	ExtraIssuers []string // Google also uses "accounts.google.com"
	ClientID     string
	ClientSecret string

	mu         sync.Mutex
	authURL    string
	tokenURL   string
	jwks       *JWKS
	issuerSeen string
}

func (p *Provider) discover() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.authURL != "" {
		return nil
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(p.Issuer + "/.well-known/openid-configuration")
	if err != nil { // not cached: the IdP may simply have started after us
		return fmt.Errorf("%s discovery: %w", p.Name, err)
	}
	defer resp.Body.Close()
	var d struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		JWKSURI               string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.AuthorizationEndpoint == "" {
		return fmt.Errorf("%s discovery: bad document", p.Name)
	}
	p.issuerSeen, p.authURL, p.tokenURL, p.jwks = d.Issuer, d.AuthorizationEndpoint, d.TokenEndpoint, &JWKS{URL: d.JWKSURI}
	return nil
}

// LoginState is kept in a short-lived signed cookie between redirect and callback.
type LoginState struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Exp      int64  `json:"e"`
}

func RandToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return b64.EncodeToString(b)
}

// AuthCodeURL builds the authorization request with PKCE (S256), state and nonce.
func (p *Provider) AuthCodeURL(st LoginState, redirect string) (string, error) {
	if err := p.discover(); err != nil {
		return "", err
	}
	ch := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {p.ClientID}, "redirect_uri": {redirect},
		"scope": {"openid email profile"}, "state": {st.State}, "nonce": {st.Nonce},
		"code_challenge": {b64.EncodeToString(ch[:])}, "code_challenge_method": {"S256"},
		"prompt": {"select_account"},
	}
	return p.authURL + "?" + q.Encode(), nil
}

// Exchange trades the code for tokens (server-to-server, with client secret + PKCE verifier) and verifies the ID token.
func (p *Provider) Exchange(code, verifier, redirect, nonce string) (*IDClaims, error) {
	if err := p.discover(); err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}, "code_verifier": {verifier}}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).PostForm(p.tokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	if resp.StatusCode != 200 || tok.IDToken == "" {
		return nil, fmt.Errorf("token exchange failed: %s %s", tok.Error, tok.Desc)
	}
	issuers := append([]string{p.Issuer, p.issuerSeen}, p.ExtraIssuers...)
	return VerifyIDToken(tok.IDToken, p.jwks, issuers, p.ClientID, nonce)
}

// ---------------- signed cookies ----------------

type Session struct {
	CID      string `json:"cid"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Picture  string `json:"pic,omitempty"`
	Role     string `json:"role"` // customer | admin | guest
	Provider string `json:"prov"`
	Exp      int64  `json:"exp"`
	ID       string `json:"jti"` // session id for rate limiting / revocation
}

type Signer struct{ Key []byte }

func (s Signer) Sign(v any) string {
	body, _ := json.Marshal(v)
	enc := b64.EncodeToString(body)
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(enc))
	return enc + "." + b64.EncodeToString(m.Sum(nil))
}

func (s Signer) Verify(token string, v any) error {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return errors.New("bad token")
	}
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(token[:i]))
	want := b64.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(token[i+1:])) {
		return errors.New("bad signature")
	}
	body, err := b64.DecodeString(token[:i])
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

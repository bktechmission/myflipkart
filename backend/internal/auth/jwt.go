// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package auth implements OpenID Connect login (authorization code + PKCE) with
// RS256 ID-token verification against the provider's JWKS, plus signed session
// cookies. Standard library only, so every step is readable.
package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k JWK) PublicKey() (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, errors.New("unsupported key type " + k.Kty)
	}
	n, err := b64.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	e, err := b64.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
}

func JWKFromKey(kid string, pub *rsa.PublicKey) JWK {
	return JWK{Kty: "RSA", Kid: kid, Alg: "RS256", Use: "sig", N: b64.EncodeToString(pub.N.Bytes()),
		E: b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}
}

// SignRS256 creates a compact JWT (used by the mock IdP).
func SignRS256(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	c, _ := json.Marshal(claims)
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64.EncodeToString(sig), nil
}

// JWKS caches a provider's signing keys and refreshes on unknown kid (key rotation).
type JWKS struct {
	URL     string
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func (j *JWKS) key(kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if k, ok := j.keys[kid]; ok && time.Since(j.fetched) < time.Hour {
		return k, nil
	}
	if time.Since(j.fetched) < 10*time.Second && j.keys != nil {
		if k, ok := j.keys[kid]; ok {
			return k, nil
		}
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(j.URL)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	var set struct{ Keys []JWK }
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	j.keys, j.fetched = map[string]*rsa.PublicKey{}, time.Now()
	for _, k := range set.Keys {
		if pk, err := k.PublicKey(); err == nil {
			j.keys[k.Kid] = pk
		}
	}
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

type IDClaims struct {
	Iss           string `json:"iss"`
	Sub           string `json:"sub"`
	Aud           any    `json:"aud"`
	Exp           int64  `json:"exp"`
	Iat           int64  `json:"iat"`
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"` // bool, or "true" for some providers
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// VerifyIDToken checks signature (RS256 only - never "none"), issuer, audience, expiry and nonce.
func VerifyIDToken(token string, jwks *JWKS, issuers []string, clientID, nonce string) (*IDClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	var hdr struct{ Alg, Kid string }
	hb, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil {
		return nil, errors.New("bad header")
	}
	if hdr.Alg != "RS256" {
		return nil, errors.New("unexpected alg " + hdr.Alg)
	}
	pub, err := jwks.key(hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, errors.New("signature invalid")
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("bad claims")
	}
	var c IDClaims
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, err
	}
	okIss := false
	for _, i := range issuers {
		okIss = okIss || c.Iss == i
	}
	if !okIss {
		return nil, fmt.Errorf("wrong issuer %q", c.Iss)
	}
	if !audHas(c.Aud, clientID) {
		return nil, errors.New("wrong audience")
	}
	now := time.Now().Unix()
	if c.Exp < now-60 {
		return nil, errors.New("token expired")
	}
	if c.Iat > now+300 {
		return nil, errors.New("token issued in the future")
	}
	if nonce != "" && c.Nonce != nonce {
		return nil, errors.New("nonce mismatch")
	}
	if c.Email == "" || !(c.EmailVerified == true || c.EmailVerified == "true") {
		return nil, errors.New("email missing or not verified")
	}
	c.Email = strings.ToLower(c.Email)
	return &c, nil
}

func audHas(aud any, id string) bool {
	switch v := aud.(type) {
	case string:
		return v == id
	case []any:
		for _, x := range v {
			if x == id {
				return true
			}
		}
	}
	return false
}

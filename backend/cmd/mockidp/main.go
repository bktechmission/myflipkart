// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Command mockidp runs the demo OpenID Connect provider on its own port for local
// development (see internal/mockid). For one-container deploys the server embeds it
// at /mockid instead (MOCK_EMBEDDED=1).
//
//	go run ./cmd/mockidp        # http://localhost:9000
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"asksauby/internal/mockid"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	secret := os.Getenv("MOCK_IDP_CLIENT_SECRET") // shared with the server via .env
	if len(secret) < 16 {
		log.Fatal("MOCK_IDP_CLIENT_SECRET is not set. Run `make setup` to create .env with random secrets.")
	}
	p, err := mockid.New(mockid.Config{
		Issuer:       env("MOCK_IDP_ISSUER", "http://localhost:9000"),
		ClientID:     env("MOCK_IDP_CLIENT_ID", "asksauby-local"),
		ClientSecret: secret,
		Redirects:    strings.Split(env("MOCK_IDP_REDIRECTS", "http://localhost:8080/auth/callback"), ","),
		Password:     os.Getenv("MOCK_IDP_PASSWORD"),
		AllowOther:   true,
	}, filepath.Join(env("DATA_DIR", "data"), "mockidp-key.pem"))
	if err != nil {
		log.Fatal(err)
	}
	addr := env("MOCK_IDP_ADDR", ":9000")
	log.Printf("MockID %s on %s (password step: %v)", p.Issuer, addr, p.Password != "")
	log.Fatal(http.ListenAndServe(addr, p.Handler()))
}

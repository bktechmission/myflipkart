// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Command server runs Ask Sauby (MyFlipKart.com's shopping assistant).
//
//	go run ./cmd/server      # http://localhost:8080
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"asksauby/internal/api"
	"asksauby/internal/config"
	"asksauby/internal/llm"
	"asksauby/internal/search"
	"asksauby/internal/store"
)

func main() {
	cfg := config.Load()
	if cfg.EphemeralSecret {
		log.Println("SESSION_SECRET not set: using a random one for this run (sign-ins reset on restart). Run `make setup`.")
	}
	st, err := store.Open(cfg.DataDir, cfg.DemoOrdersFor)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	gw := NewGateway(cfg)
	r, err := search.NewRetriever(cfg, st.Products(), st.Policies())
	if err != nil {
		log.Fatalf("retriever: %v", err)
	}
	var canaryGW *llm.Gateway
	if cfg.CanaryPercent > 0 { // candidate agent: same gateway shape, different internal model
		model := cfg.CanaryModel
		if model == "" {
			model = cfg.InternalLLMModel
		}
		canaryGW = &llm.Gateway{Internal: llm.NewProvider("internal-canary", cfg.InternalLLMURL, model, "", 60*time.Second), External: gw.External}
		canaryGW.Internal.Probe()
	}
	app := api.New(cfg, st, r, gw, canaryGW)
	srv := &http.Server{Addr: cfg.Addr, Handler: app.Handler(),
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 120 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go app.Hub.Run(ctx) // simulated teammates join waiting handoffs in demo mode
	go func() {         // keep provider health fresh for routing + the dashboard
		for {
			gw.Internal.Probe()
			if gw.External != nil {
				gw.External.Probe()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
	go func() {
		log.Printf("Ask Sauby on %s  embeddings=%s  router=%s  google_login=%v  mock_idp=%s",
			cfg.PublicURL, r.EmbedLabel, cfg.Router, cfg.GoogleClientID != "", cfg.MockIssuer)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	sh, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sh) // drain in-flight chats (rolling deploys without dropped turns)
}

func NewGateway(cfg config.Config) *llm.Gateway {
	gw := &llm.Gateway{Internal: llm.NewProvider("internal", cfg.InternalLLMURL, cfg.InternalLLMModel, "", 60*time.Second)}
	if cfg.ExternalLLMURL != "" && cfg.ExternalLLMModel != "" {
		gw.External = llm.NewProvider("external", cfg.ExternalLLMURL, cfg.ExternalLLMModel, cfg.ExternalLLMKey, 30*time.Second)
	}
	gw.Internal.Probe()
	if gw.External != nil {
		gw.External.Probe()
	}
	return gw
}

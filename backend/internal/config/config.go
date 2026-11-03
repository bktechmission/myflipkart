// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package config reads every knob from the environment so the eval runner can
// A/B settings without code changes.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr      string // server listen address
	PublicURL string // how the browser reaches the server (OIDC redirect base)
	DataDir   string
	WebDir    string

	SessionSecret string
	AdminEmails   map[string]bool

	// Google OIDC (real). Empty client ID disables the button.
	GoogleClientID     string
	GoogleClientSecret string

	// Mock OIDC identity provider (cmd/mockidp)
	MockIssuer       string
	MockClientID     string
	MockClientSecret string

	// LLM gateway: internal (Ollama, OpenAI-compatible) + optional external provider
	InternalLLMURL   string
	InternalLLMModel string
	ExternalLLMURL   string
	ExternalLLMModel string
	ExternalLLMKey   string
	EmbedURL         string
	EmbedModel       string
	EmbedBackend     string // auto | ollama | hash
	Router           string // auto | llm | rules

	// Retrieval
	Retriever      string // bm25 | vector | hybrid
	RRFK           float64
	BM25Weight     float64
	VectorWeight   float64
	TopK           int
	PolicyK        int
	CategoryFilter bool

	// Limits
	RatePerMin      float64 // chat requests per user per minute
	RateBurst       float64
	DailyTokenQuota int // estimated LLM tokens per user per day

	ReturnWindowDays int
	DemoOrdersFor    string // optional: give this (real Google) email a copy of the first demo customer's orders

	// Guardrails on the money path
	AutoRefundLimit float64 // refunds above this amount go to a human approver
	MaxAutoRefunds  int     // auto refunds per customer per 24h before human review

	// Canary rollout: a slice of sessions get the candidate agent
	CanaryPercent int    // 0-100
	CanaryModel   string // e.g. qwen2.5:7b
	CanaryRouter  string // auto | llm | rules
	EvalGates     string // path to gate thresholds JSON

	// Demo / hosting
	PublicDemo      bool    // hosted for anyone: passwordless MockID, eval cooldown
	MockEmbedded    bool    // serve MockID inside the app at /mockid (one container, one URL)
	InternalLLMKey  string  // API key for the internal provider (e.g. Workers AI token)
	SimAgents       bool    // simulated support teammates join if no human picks up
	SimJoinAfterSec float64 // seconds a handoff waits for a real human first
	Rerank          bool    // second-stage reranker after RRF fusion
	MetricsToken    string  // bearer token for /metrics (Prometheus scrape)
	EphemeralSecret bool    // SESSION_SECRET was missing and generated at start
}

func Load() Config {
	admins := map[string]bool{}
	for _, e := range strings.Split(env("ADMIN_EMAILS", "aarav@example.com"), ",") {
		if e = strings.TrimSpace(strings.ToLower(e)); e != "" {
			admins[e] = true
		}
	}
	c := Config{
		Addr:               env("ADDR", portAddr()),                                            // ADDR, else :$PORT (Render, Cloud Run), else :8080
		PublicURL:          strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"), // "auto" = from each request
		DataDir:            env("DATA_DIR", "data"),
		WebDir:             env("WEB_DIR", "../web/public"),
		SessionSecret:      os.Getenv("SESSION_SECRET"), // empty -> random per process (see Load)
		AdminEmails:        admins,
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		MockIssuer:         strings.TrimRight(env("MOCK_IDP_ISSUER", "http://localhost:9000"), "/"),
		MockClientID:       env("MOCK_IDP_CLIENT_ID", "asksauby-local"),
		MockClientSecret:   os.Getenv("MOCK_IDP_CLIENT_SECRET"), // from .env (make setup); never hard-coded
		InternalLLMURL:     strings.TrimRight(env("INTERNAL_LLM_URL", "http://localhost:11434/v1"), "/"),
		InternalLLMModel:   env("INTERNAL_LLM_MODEL", "qwen2.5:3b"),
		ExternalLLMURL:     strings.TrimRight(os.Getenv("EXTERNAL_LLM_URL"), "/"),
		ExternalLLMModel:   os.Getenv("EXTERNAL_LLM_MODEL"),
		ExternalLLMKey:     os.Getenv("EXTERNAL_LLM_API_KEY"),
		EmbedURL:           strings.TrimRight(env("EMBED_URL", "http://localhost:11434"), "/"),
		EmbedModel:         env("EMBED_MODEL", "nomic-embed-text"),
		EmbedBackend:       env("EMBED_BACKEND", "auto"),
		Router:             env("ROUTER", "auto"),
		Retriever:          env("RETRIEVER", "hybrid"),
		RRFK:               envF("RRF_K", 60),
		BM25Weight:         envF("BM25_WEIGHT", 1),
		VectorWeight:       envF("VECTOR_WEIGHT", 1),
		TopK:               int(envF("TOP_K", 5)),
		PolicyK:            int(envF("POLICY_K", 3)),
		CategoryFilter:     env("CATEGORY_FILTER", "1") == "1",
		RatePerMin:         envF("RATE_PER_MIN", 30),
		RateBurst:          envF("RATE_BURST", 10),
		DailyTokenQuota:    int(envF("DAILY_TOKEN_QUOTA", 200000)),
		ReturnWindowDays:   30,
		DemoOrdersFor:      strings.ToLower(os.Getenv("DEMO_ORDERS_FOR")),
		AutoRefundLimit:    envF("AUTO_REFUND_LIMIT", 29),
		MaxAutoRefunds:     int(envF("MAX_AUTO_REFUNDS_PER_DAY", 3)),
		CanaryPercent:      int(envF("CANARY_PERCENT", 0)),
		CanaryModel:        os.Getenv("CANARY_LLM_MODEL"),
		CanaryRouter:       env("CANARY_ROUTER", env("ROUTER", "auto")),
		EvalGates:          env("EVAL_GATES", "evals/gates.json"),
		PublicDemo:         os.Getenv("PUBLIC_DEMO") == "1",
		MockEmbedded:       os.Getenv("MOCK_EMBEDDED") == "1",
		InternalLLMKey:     os.Getenv("INTERNAL_LLM_API_KEY"),
		SimAgents:          env("SIM_AGENTS", "1") == "1",
		SimJoinAfterSec:    envF("SIM_JOIN_AFTER_SEC", 6),
		Rerank:             env("RERANK", "1") == "1",
		MetricsToken:       os.Getenv("METRICS_TOKEN"),
	}
	if c.MockEmbedded && len(c.MockClientSecret) < 16 {
		// Embedded IdP and app share one process: an in-memory random secret is enough.
		b := make([]byte, 24)
		rand.Read(b)
		c.MockClientSecret = hex.EncodeToString(b)
	}
	if len(c.SessionSecret) < 32 {
		// No secret configured: generate one for this process. Sessions reset on restart,
		// which is safe; a guessable default would let anyone forge sessions.
		b := make([]byte, 32)
		rand.Read(b)
		c.SessionSecret = hex.EncodeToString(b)
		c.EphemeralSecret = true
	}
	return c
}

// portAddr honours the PORT variable that hosting platforms (Render, Cloud Run) set.
func portAddr() string {
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envF(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

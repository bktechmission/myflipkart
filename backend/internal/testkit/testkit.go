// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package testkit builds a fresh in-memory store, retriever and tools for tests.
package testkit

import (
	"sync"
	"testing"

	"asksauby/internal/agent"
	"asksauby/internal/config"
	"asksauby/internal/search"
	"asksauby/internal/seed"
	"asksauby/internal/store"
)

var (
	once sync.Once
	ret  *search.Retriever
	cfg  config.Config
)

// Config is the default config with offline (hash) embeddings.
func Config() config.Config {
	once.Do(func() {
		cfg = config.Load()
		cfg.EmbedBackend = "hash"
		var err error
		ret, err = search.NewRetriever(cfg, seed.Products(), seed.Policies)
		if err != nil {
			panic(err)
		}
	})
	return cfg
}

// Tools returns production-like tools ($29 limit) over a fresh memory-only store.
func Tools(t testing.TB) (*agent.Tools, *store.Store) {
	t.Helper()
	c := Config()
	st, err := store.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	// the retriever is shared; give each test its own copy of the catalog stock view
	r, err := search.NewRetriever(c, st.Products(), st.Policies())
	if err != nil {
		t.Fatal(err)
	}
	return &agent.Tools{S: st, R: r, ReturnWindow: 30, AutoRefundLimit: 29, MaxAutoRefunds: 3}, st
}

// Retriever is the shared read-only retriever.
func Retriever() *search.Retriever { Config(); return ret }

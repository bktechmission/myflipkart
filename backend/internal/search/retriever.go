// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Retriever owns the product and policy indexes, applies metadata filters
// (category, price), runs the reranker, and re-indexes live when admins edit docs.

package search

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"asksauby/internal/config"
	"asksauby/internal/model"
	"asksauby/internal/seed"
)

type PolicyChunk struct {
	ID, Doc, Title, Text string
}

type Retriever struct {
	cfg        config.Config
	mu         sync.RWMutex // searches read-lock; live re-indexing swaps under the write lock
	Products   map[string]model.Product
	products   *Index
	policies   *Index
	Chunks     map[string]PolicyChunk
	EmbedLabel string
	Versions   map[string]int     // index name -> rebuild count
	BuildMS    map[string]float64 // index name -> last rebuild time
}

func NewRetriever(cfg config.Config, products []model.Product, policies []model.PolicyDoc) (*Retriever, error) {
	r := &Retriever{cfg: cfg, Versions: map[string]int{}, BuildMS: map[string]float64{}}
	if err := r.ReindexProducts(products); err != nil {
		return nil, err
	}
	if err := r.ReindexPolicies(policies); err != nil {
		return nil, err
	}
	return r, nil
}

// ReindexProducts rebuilds BM25 + vectors for the catalog and swaps them in atomically.
func (r *Retriever) ReindexProducts(products []model.Product) error {
	t0 := time.Now()
	pm := map[string]model.Product{}
	var docs []Doc
	for _, p := range products {
		pm[p.ID] = p
		docs = append(docs, Doc{p.ID, seed.PageText(p)})
	}
	e, err := NewEmbedder(r.cfg.EmbedBackend, r.cfg.EmbedURL, r.cfg.EmbedModel)
	if err != nil {
		return err
	}
	ix, err := NewIndex("products", docs, e)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.Products, r.products, r.EmbedLabel = pm, ix, e.Label()
	r.Versions["products"]++
	r.BuildMS["products"] = float64(time.Since(t0).Microseconds()) / 1000
	r.mu.Unlock()
	return nil
}

// ReindexPolicies chunks policy docs by paragraph ("returns#0") and rebuilds the policy index.
func (r *Retriever) ReindexPolicies(policies []model.PolicyDoc) error {
	t0 := time.Now()
	chunks := map[string]PolicyChunk{}
	var docs []Doc
	for _, pol := range policies {
		for i, para := range pol.Paras {
			if strings.TrimSpace(para) == "" {
				continue
			}
			c := PolicyChunk{fmt.Sprintf("%s#%d", pol.ID, i), pol.ID, pol.Title, pol.Title + ": " + para}
			chunks[c.ID] = c
			docs = append(docs, Doc{c.ID, c.Text})
		}
	}
	e, err := NewEmbedder(r.cfg.EmbedBackend, r.cfg.EmbedURL, r.cfg.EmbedModel)
	if err != nil {
		return err
	}
	ix, err := NewIndex("policies", docs, e)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.Chunks, r.policies = chunks, ix
	r.Versions["policies"]++
	r.BuildMS["policies"] = float64(time.Since(t0).Microseconds()) / 1000
	r.mu.Unlock()
	return nil
}

func (r *Retriever) Product(id string) (model.Product, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.Products[id]
	return p, ok
}

func (r *Retriever) Chunk(id string) PolicyChunk {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Chunks[id]
}

func (r *Retriever) Stats() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return map[string]any{"embeddings": r.EmbedLabel, "products_docs": len(r.Products), "policy_chunks": len(r.Chunks),
		"versions": r.Versions, "build_ms": r.BuildMS}
}

// DetectCategory is cheap metadata extraction: query words -> catalog category.
var synRe = func() map[string][]*regexp.Regexp {
	m := map[string][]*regexp.Regexp{}
	for cat, syns := range seed.CategorySynonyms {
		for _, s := range syns {
			m[cat] = append(m[cat], regexp.MustCompile(`\b`+regexp.QuoteMeta(s)+`\b`))
		}
	}
	return m
}()

func DetectCategory(q string) string {
	q = strings.ToLower(q)
	found := ""
	for cat, res := range synRe {
		for _, re := range res {
			if re.MatchString(q) {
				if found != "" && found != cat {
					return "" // ambiguous: don't filter
				}
				found = cat
				break
			}
		}
	}
	return found
}

type ProductQuery struct {
	Query          string
	K              int
	Mode           string
	Rerank         *bool // nil = config default
	MinPrice       *float64
	MaxPrice       *float64
	CategoryFilter *bool
}

type RetrievalInfo struct {
	Mode        string   `json:"mode"`
	Category    string   `json:"category,omitempty"`
	MinPrice    *float64 `json:"min_price,omitempty"`
	MaxPrice    *float64 `json:"max_price,omitempty"`
	Candidates  int      `json:"candidates"`
	Reranked    bool     `json:"reranked,omitempty"`
	RetrievalMS float64  `json:"retrieval_ms"`
}

func (r *Retriever) params(mode string) Params {
	if mode == "" {
		mode = r.cfg.Retriever
	}
	return Params{Mode: mode, RRFK: r.cfg.RRFK, BM25W: r.cfg.BM25Weight, VecW: r.cfg.VectorWeight}
}

func (r *Retriever) SearchProducts(q ProductQuery) ([]Hit, RetrievalInfo, error) {
	if q.K == 0 {
		q.K = r.cfg.TopK
	}
	useCat := r.cfg.CategoryFilter
	if q.CategoryFilter != nil {
		useCat = *q.CategoryFilter
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	cat := ""
	if useCat {
		cat = DetectCategory(q.Query)
	}
	ok := func(id string) bool {
		p := r.Products[id]
		return (cat == "" || p.Category == cat) &&
			(q.MinPrice == nil || p.Price >= *q.MinPrice) && (q.MaxPrice == nil || p.Price <= *q.MaxPrice)
	}
	n := 0
	for id := range r.Products {
		if ok(id) {
			n++
		}
	}
	var allowed func(string) bool
	if cat != "" || q.MinPrice != nil || q.MaxPrice != nil {
		allowed = ok
	}
	p := r.params(q.Mode)
	rr := r.cfg.Rerank
	if q.Rerank != nil {
		rr = *q.Rerank
	}
	fetch := q.K
	if rr && fetch < 20 {
		fetch = 20 // recall stage: over-fetch, then let the reranker pick the top k
	}
	hits, ms, err := r.products.Search(q.Query, fetch, p, allowed)
	if rr && err == nil {
		hits = Rerank(q.Query, hits, r.Products)
	}
	if len(hits) > q.K {
		hits = hits[:q.K]
	}
	return hits, RetrievalInfo{Mode: p.Mode, Category: cat, MinPrice: q.MinPrice, MaxPrice: q.MaxPrice,
		Candidates: n, Reranked: rr, RetrievalMS: ms}, err
}

func (r *Retriever) SearchPolicies(query string, k int, mode string) ([]Hit, RetrievalInfo, error) {
	if k == 0 {
		k = r.cfg.PolicyK
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p := r.params(mode)
	hits, ms, err := r.policies.Search(query, k, p, nil)
	return hits, RetrievalInfo{Mode: p.Mode, Candidates: len(r.Chunks), RetrievalMS: ms}, err
}

// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package search — second-stage reranker.
//
// Retrieval is two-stage, like most production RAG systems:
//
//  1. Recall stage: BM25 + vectors fused with RRF return a generous candidate set (top 20).
//  2. Precision stage: a reranker re-scores only those candidates with richer signals.
//
// Production systems use a cross-encoder (e.g. bge-reranker, Cohere Rerank) or an LLM
// here. This demo uses a transparent feature-based reranker so you can see exactly why
// an item moved: query-term coverage of the product name and page, plus small
// rating / availability priors. Swap Rerank() for a model call without touching callers.
package search

import (
	"math"
	"sort"
	"strings"

	"asksauby/internal/model"
	"asksauby/internal/seed"
)

// Reranker weights. Chosen with the eval suite (go run ./cmd/evals -suite retrieval):
// adding page-text coverage and rating/stock priors LOWERED recall@5 (0.987 vs 1.000),
// so they are off by default. Name coverage is kept: it lifts exact model-name matches.
var (
	WBase     = 0.9  // fused RRF score (normalized)
	WNameCov  = 0.15 // query terms found in product name/brand
	WPageCov  = 0.0  // query terms found anywhere on the product page
	WPriorMul = 0.0  // rating and in-stock priors
)

// Rerank re-orders RRF candidates and records each hit's RRF rank and rerank score.
func Rerank(query string, hits []Hit, products map[string]model.Product) []Hit {
	if len(hits) == 0 {
		return hits
	}
	q := uniq(Tokenize(query))
	maxS := hits[0].Score
	for i := range hits {
		hits[i].RRFRank = i + 1
		p := products[hits[i].ID]
		nameCov := coverage(q, Tokenize(p.Name+" "+p.Brand))
		pageCov := coverage(q, Tokenize(seed.PageText(p)))
		prior := 0.04*(p.Rating-4.2) + map[bool]float64{true: 0.02, false: -0.05}[p.Stock > 0]
		base := hits[i].Score / math.Max(maxS, 1e-9) // fused score, normalized 0..1
		hits[i].Rerank = math.Round((1.0*base+0.1*nameCov+0*pageCov+0*prior)*1e4) / 1e4
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Rerank > hits[b].Rerank })
	return hits
}

// coverage = share of query terms that appear in the text.
func coverage(q, text []string) float64 {
	if len(q) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, t := range text {
		set[t] = true
	}
	n := 0
	for _, t := range q {
		if set[t] {
			n++
		}
	}
	return float64(n) / float64(len(q))
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] && strings.TrimSpace(x) != "" {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

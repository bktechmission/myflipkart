// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package search implements hybrid retrieval from scratch:
//
//	BM25 (keyword)  +  flat cosine vector index (the same thing FAISS IndexFlatIP does)
//	fused with Reciprocal Rank Fusion:  rrf(d) = wb/(k+rank_bm25) + wv/(k+rank_vec)
package search

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var wordRe = regexp.MustCompile(`[a-z0-9]+`)
var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an the and or for to of in on at with my me i is it that this be do can you your what how when where which are was will from by as about please some any") {
		stop[w] = true
	}
}

func Tokenize(s string) []string {
	var out []string
	for _, t := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if !stop[t] {
			out = append(out, t)
		}
	}
	return out
}

type Doc struct {
	ID   string
	Text string
}

// ---------------- BM25 ----------------

type BM25 struct {
	k1, b  float64
	docs   [][]string
	tf     []map[string]int
	df     map[string]int
	avgLen float64
}

func NewBM25(docs []Doc) *BM25 {
	m := &BM25{k1: 1.5, b: 0.75, df: map[string]int{}}
	total := 0
	for _, d := range docs {
		toks := Tokenize(d.Text)
		m.docs = append(m.docs, toks)
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		for t := range tf {
			m.df[t]++
		}
		m.tf = append(m.tf, tf)
		total += len(toks)
	}
	m.avgLen = float64(total) / math.Max(1, float64(len(docs)))
	return m
}

func (m *BM25) Scores(query string) []float64 {
	n := float64(len(m.docs))
	scores := make([]float64, len(m.docs))
	for _, q := range Tokenize(query) {
		df := float64(m.df[q])
		if df == 0 {
			continue
		}
		idf := math.Log((n-df+0.5)/(df+0.5) + 1) // Lucene-style, always positive
		for i, tf := range m.tf {
			f := float64(tf[q])
			if f == 0 {
				continue
			}
			dl := float64(len(m.docs[i]))
			scores[i] += idf * f * (m.k1 + 1) / (f + m.k1*(1-m.b+m.b*dl/m.avgLen))
		}
	}
	return scores
}

// ---------------- vector index ----------------

// FlatIndex is exact inner-product search over L2-normalized vectors (cosine).
// At 48 docs brute force is microseconds; at millions swap in HNSW/IVF (FAISS, pgvector).
type FlatIndex struct{ vecs [][]float32 }

func (f *FlatIndex) Add(v []float32) { f.vecs = append(f.vecs, v) }
func (f *FlatIndex) Search(q []float32) []float64 {
	out := make([]float64, len(f.vecs))
	for i, v := range f.vecs {
		var s float32
		for j := range v {
			s += v[j] * q[j]
		}
		out[i] = float64(s)
	}
	return out
}

// ---------------- hybrid ----------------

type Params struct {
	Mode              string // bm25 | vector | hybrid
	RRFK, BM25W, VecW float64
}

type Hit struct {
	ID       string  `json:"id"`
	Score    float64 `json:"score"`
	BM25Rank int     `json:"bm25_rank,omitempty"`
	BM25     float64 `json:"bm25_score,omitempty"`
	VecRank  int     `json:"vec_rank,omitempty"`
	Cosine   float64 `json:"vec_score,omitempty"`
	RRFRank  int     `json:"rrf_rank,omitempty"`     // position after fusion, before rerank
	Rerank   float64 `json:"rerank_score,omitempty"` // second-stage score (0 = not reranked)
}

type Index struct {
	Name     string
	ids      []string
	bm25     *BM25
	vec      FlatIndex
	embedder Embedder
}

func NewIndex(name string, docs []Doc, e Embedder) (*Index, error) {
	ix := &Index{Name: name, bm25: NewBM25(docs), embedder: e}
	texts := make([]string, len(docs))
	for i, d := range docs {
		ix.ids = append(ix.ids, d.ID)
		texts[i] = d.Text
	}
	e.Fit(texts)
	vecs, err := e.Embed(texts, false)
	if err != nil {
		return nil, err
	}
	for _, v := range vecs {
		ix.vec.Add(v)
	}
	return ix, nil
}

type ranked struct {
	i int
	s float64
}

func rank(scores []float64, allowed func(string) bool, ids []string, positiveOnly bool) []ranked {
	var r []ranked
	for i, s := range scores {
		if (positiveOnly && s <= 0) || (allowed != nil && !allowed(ids[i])) {
			continue
		}
		r = append(r, ranked{i, s})
	}
	sort.SliceStable(r, func(a, b int) bool { return r[a].s > r[b].s })
	return r
}

// Search returns the top-k hits, with both component ranks for the trace UI.
func (ix *Index) Search(query string, k int, p Params, allowed func(string) bool) ([]Hit, float64, error) {
	t0 := time.Now()
	info := map[int]*Hit{}
	get := func(i int) *Hit {
		if info[i] == nil {
			info[i] = &Hit{ID: ix.ids[i]}
		}
		return info[i]
	}
	if p.Mode == "bm25" || p.Mode == "hybrid" {
		for r, x := range rank(ix.bm25.Scores(query), allowed, ix.ids, true) {
			h := get(x.i)
			h.BM25Rank, h.BM25 = r+1, round3(x.s)
		}
	}
	if p.Mode == "vector" || p.Mode == "hybrid" {
		q, err := ix.embedder.Embed([]string{query}, true)
		if err != nil {
			return nil, 0, err
		}
		for r, x := range rank(ix.vec.Search(q[0]), allowed, ix.ids, false) {
			h := get(x.i)
			h.VecRank, h.Cosine = r+1, round3(x.s)
		}
	}
	hits := make([]Hit, 0, len(info))
	for _, h := range info {
		switch p.Mode {
		case "bm25":
			h.Score = h.BM25
		case "vector":
			h.Score = h.Cosine
		default:
			if h.BM25Rank > 0 {
				h.Score += p.BM25W / (p.RRFK + float64(h.BM25Rank))
			}
			if h.VecRank > 0 {
				h.Score += p.VecW / (p.RRFK + float64(h.VecRank))
			}
			h.Score = math.Round(h.Score*1e5) / 1e5
		}
		hits = append(hits, *h)
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].ID < hits[b].ID
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, float64(time.Since(t0).Microseconds()) / 1000, nil
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

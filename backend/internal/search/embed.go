// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Embedding backends for the vector index: Ollama (e.g. nomic-embed-text) or an
// offline TF-IDF feature-hashing fallback. Vectors are L2-normalized for cosine.

package search

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strings"
	"time"
)

type Embedder interface {
	Fit(texts []string)
	Embed(texts []string, isQuery bool) ([][]float32, error)
	Label() string
}

// NewEmbedder returns a fresh embedder per index. backend: auto | ollama | hash.
func NewEmbedder(backend, url, model string) (Embedder, error) {
	if backend == "ollama" || backend == "auto" {
		o := &OllamaEmbedder{URL: url, Model: model, client: &http.Client{Timeout: 60 * time.Second}}
		if _, err := o.Embed([]string{"ping"}, true); err == nil {
			return o, nil
		} else if backend == "ollama" {
			return nil, err
		}
	}
	return &HashEmbedder{Dim: 1024}, nil
}

// OllamaEmbedder calls a local open-source embedding model (e.g. nomic-embed-text).
type OllamaEmbedder struct {
	URL, Model string
	client     *http.Client
}

func (o *OllamaEmbedder) Fit([]string)  {}
func (o *OllamaEmbedder) Label() string { return "ollama:" + o.Model }
func (o *OllamaEmbedder) Embed(texts []string, isQuery bool) ([][]float32, error) {
	prefix := "search_document: "
	if isQuery {
		prefix = "search_query: "
	}
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = prefix + t
	}
	body, _ := json.Marshal(map[string]any{"model": o.Model, "input": in})
	resp, err := o.client.Post(o.URL+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama embed: %s", resp.Status)
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	for _, v := range out.Embeddings {
		normalize(v)
	}
	return out.Embeddings, nil
}

// HashEmbedder is the offline fallback: TF-IDF weighted feature hashing of
// words, word bigrams and character trigrams. It catches morphology
// ("cancel"~"cancelled") and shared vocabulary, but has no real world knowledge
// ("airplane"~"flight") - that is what a neural embedding model adds.
type HashEmbedder struct {
	Dim int
	idf map[string]float64
}

func (h *HashEmbedder) Label() string { return "hash:tfidf-ngrams" }

func feats(text string) []string {
	toks := Tokenize(text)
	var f []string
	for i, t := range toks {
		f = append(f, "w:"+t)
		if i > 0 {
			f = append(f, "b:"+toks[i-1]+"_"+t)
		}
		p := "#" + t + "#"
		for j := 0; j+3 <= len(p); j++ {
			f = append(f, "c:"+p[j:j+3])
		}
	}
	return f
}

func (h *HashEmbedder) Fit(texts []string) {
	df := map[string]int{}
	for _, t := range texts {
		seen := map[string]bool{}
		for _, f := range feats(t) {
			if !seen[f] {
				seen[f] = true
				df[f]++
			}
		}
	}
	h.idf = map[string]float64{}
	n := float64(len(texts))
	for f, d := range df {
		h.idf[f] = math.Log((1+n)/(1+float64(d))) + 1
	}
}

func (h *HashEmbedder) Embed(texts []string, _ bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, h.Dim)
		tf := map[string]int{}
		for _, f := range feats(t) {
			tf[f]++
		}
		for f, c := range tf {
			idf, ok := h.idf[f]
			if !ok {
				continue // unseen in corpus: no signal
			}
			w := (1 + math.Log(float64(c))) * idf
			if strings.HasPrefix(f, "c:") {
				w *= 0.5
			}
			hs := fnv.New32a()
			hs.Write([]byte(f))
			x := hs.Sum32()
			sign := float32(1)
			if x&1 == 1 {
				sign = -1
			}
			v[int(x>>1)%h.Dim] += sign * float32(w)
		}
		normalize(v)
		out[i] = v
	}
	return out, nil
}

func normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return
	}
	n := float32(math.Sqrt(s))
	for i := range v {
		v[i] /= n
	}
}

// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package obs is observability: Prometheus metrics and structured JSON logs.
//
//   - Metrics are exposed at GET /metrics in the Prometheus text format, so a real
//     Prometheus (or Grafana Agent / Cloudflare-friendly scrapers) can scrape them.
//     Written by hand with the standard library so you can see the format.
//   - Logs are one JSON line per chat turn on stdout (log/slog). In production these
//     ship to Loki / Elasticsearch / S3, and the eval pipeline samples them to build
//     new golden cases (see README "Continuous improvement loop").
package obs

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

// Log is the structured logger (JSON lines on stdout).
var Log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Buckets for latency histograms, in seconds. Chosen around the SLOs:
// FAQ p95 < 1s / p99 < 5s, transactions p95 < 5s / p99 < 30s.
var Buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type series struct {
	help, kind string
	values     map[string]float64    // label string -> value (counters, gauges)
	hist       map[string]*histogram // label string -> histogram
}

type histogram struct {
	counts []uint64
	sum    float64
	n      uint64
}

// Registry holds every metric. The zero value is not usable; use Default.
type Registry struct {
	mu sync.Mutex
	m  map[string]*series
}

var Default = &Registry{m: map[string]*series{}}

func (r *Registry) get(name, help, kind string) *series {
	s := r.m[name]
	if s == nil {
		s = &series{help: help, kind: kind, values: map[string]float64{}, hist: map[string]*histogram{}}
		r.m[name] = s
	}
	return s
}

// labels renders {k="v",...} in a stable order. kv is k1, v1, k2, v2...
func labels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		parts = append(parts, fmt.Sprintf(`%s="%s"`, kv[i], v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Inc adds 1 to a counter.
func (r *Registry) Inc(name, help string, kv ...string) { r.Add(name, help, 1, kv...) }

// Add adds v to a counter.
func (r *Registry) Add(name, help string, v float64, kv ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(name, help, "counter").values[labels(kv...)] += v
}

// Set sets a gauge.
func (r *Registry) Set(name, help string, v float64, kv ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(name, help, "gauge").values[labels(kv...)] = v
}

// Observe records a value (seconds) in a histogram.
func (r *Registry) Observe(name, help string, v float64, kv ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(name, help, "histogram")
	l := labels(kv...)
	h := s.hist[l]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(Buckets))}
		s.hist[l] = h
	}
	for i, b := range Buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.n++
}

// Handler serves the Prometheus text exposition format. If token is set, the
// scraper must send "Authorization: Bearer <token>"; otherwise only loopback may read.
func (r *Registry) Handler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if token != "" {
			if req.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		} else if host := strings.Split(req.RemoteAddr, ":")[0]; host != "127.0.0.1" && !strings.HasPrefix(req.RemoteAddr, "[::1]") {
			http.Error(w, "set METRICS_TOKEN to scrape remotely", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.mu.Lock()
		defer r.mu.Unlock()
		names := make([]string, 0, len(r.m))
		for n := range r.m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := r.m[n]
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, s.help, n, s.kind)
			keys := sortedKeys(s.values)
			for _, l := range keys {
				fmt.Fprintf(w, "%s%s %s\n", n, l, num(s.values[l]))
			}
			hk := make([]string, 0, len(s.hist))
			for l := range s.hist {
				hk = append(hk, l)
			}
			sort.Strings(hk)
			for _, l := range hk {
				h := s.hist[l]
				inner := strings.TrimSuffix(strings.TrimPrefix(l, "{"), "}")
				sep := ""
				if inner != "" {
					sep = ","
				}
				for i, b := range Buckets {
					fmt.Fprintf(w, "%s_bucket{%s%sle=\"%s\"} %d\n", n, inner, sep, num(b), h.counts[i])
				}
				fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", n, inner, sep, h.n)
				fmt.Fprintf(w, "%s_sum%s %s\n%s_count%s %d\n", n, l, num(h.sum), n, l, h.n)
			}
		}
	}
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

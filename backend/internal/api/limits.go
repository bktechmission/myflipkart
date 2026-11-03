// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Token-bucket rate limiting per customer and a daily LLM-token quota.

package api

import (
	"math"
	"sync"
	"time"
)

// TokenBucket rate limiter keyed by user/session/IP:
// capacity = burst, refilled at rate tokens/second.
type TokenBucket struct {
	rate, burst float64
	mu          sync.Mutex
	b           map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewTokenBucket(perMin, burst float64) *TokenBucket {
	return &TokenBucket{rate: perMin / 60, burst: burst, b: map[string]*bucket{}}
}

// Allow takes one token; when empty it returns how long to wait.
func (t *TokenBucket) Allow(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if len(t.b) > 50000 { // drop idle buckets so a flood of keys can't grow memory forever
		for k, v := range t.b {
			if now.Sub(v.last) > 10*time.Minute {
				delete(t.b, k)
			}
		}
		if len(t.b) > 50000 {
			t.b = map[string]*bucket{}
		}
	}
	bk := t.b[key]
	if bk == nil {
		bk = &bucket{tokens: t.burst, last: now}
		t.b[key] = bk
	}
	bk.tokens = math.Min(t.burst, bk.tokens+now.Sub(bk.last).Seconds()*t.rate)
	bk.last = now
	if bk.tokens >= 1 {
		bk.tokens--
		return true, 0
	}
	return false, time.Duration((1 - bk.tokens) / t.rate * float64(time.Second))
}

// Quota is a daily LLM-token budget per customer (cost protection, abuse control).
type Quota struct {
	limit int
	mu    sync.Mutex
	day   string
	used  map[string]int
}

func NewQuota(limit int) *Quota { return &Quota{limit: limit, used: map[string]int{}} }

func (q *Quota) roll() {
	if d := time.Now().UTC().Format("2006-01-02"); d != q.day {
		q.day, q.used = d, map[string]int{}
	}
}

func (q *Quota) Exceeded(cid string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll()
	return q.used[cid] >= q.limit
}

func (q *Quota) Add(cid string, tokens int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll()
	if len(q.used) > 100000 {
		q.used = map[string]int{} // bounded; resets daily anyway
	}
	q.used[cid] += tokens
}

// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// FakeLLM is a scripted stand-in for the LLM gateway, used by tests and the
// offline evals. It can answer well (the same labels the rules found), or badly:
// hallucinated quotes, invented rule ids, malformed JSON, extra fields, timeouts,
// outages and answers that change between runs.

package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"asksauby/internal/llm"
	"asksauby/internal/model"
)

type FakeLLM struct {
	Script func(call int, review string) (string, error) // answer for the n-th call (1-based)
	Delay  time.Duration
	Down   bool

	mu    sync.Mutex
	calls int
}

func (f *FakeLLM) Available() bool { return !f.Down }

func (f *FakeLLM) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (f *FakeLLM) Chat(ctx context.Context, msgs []llm.Message, tools []map[string]any, _ bool) (llm.Result, error) {
	if len(tools) > 0 {
		return llm.Result{}, errors.New("fake: the tagger must never offer tools")
	}
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.Down {
		return llm.Result{}, errors.New("fake: connection refused")
	}
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return llm.Result{}, ctx.Err()
		}
	}
	review := ReviewFromPrompt(msgs)
	out, err := f.Script(n, review)
	if err != nil {
		return llm.Result{}, err
	}
	return llm.Result{Message: llm.Message{Role: "assistant", Content: out}, Provider: "fake", Model: "fake-llm", Tokens: 300}, nil
}

// ReviewFromPrompt extracts the review text the tagger was given.
func ReviewFromPrompt(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			s := m.Content
			a, b := strings.Index(s, "<<<REVIEW\n"), strings.Index(s, "\nREVIEW>>>")
			if a >= 0 && b > a {
				return s[a+len("<<<REVIEW\n") : b]
			}
		}
	}
	return ""
}

// HonestAnswer is what a well-behaved model would say: the rule tagger's labels.
func HonestAnswer(p *Policy) func(int, string) (string, error) {
	return func(_ int, review string) (string, error) {
		c := AnalyzeContent(review, "", p)
		res := RuleTagger{p}.Tag(context.Background(), TagInput{Text: review, Signals: c.Signals, Target: c.Target, Unclear: c.Unclear})
		b, _ := json.Marshal(res.Out)
		return string(b), nil
	}
}

// Answer marshals a fixed output.
func Answer(labels []model.Label, target string) string {
	if labels == nil {
		labels = []model.Label{}
	}
	b, _ := json.Marshal(TagOutput{Labels: labels, Target: target})
	return string(b)
}

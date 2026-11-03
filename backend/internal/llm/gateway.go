// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package llm is a small LLM gateway: every provider speaks the OpenAI-compatible
// /chat/completions API (Ollama serves it at /v1), so the internal open-source
// model and any external provider are interchangeable.
//
//	simple turns  -> internal model first, external as failover
//	complex turns -> external model first (if configured), internal as failover
//
// Each provider has a circuit breaker: 3 consecutive failures open it for 30s,
// so a dead provider costs one fast skip instead of a timeout per request.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // string (OpenAI) or object (some servers)
	} `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Args decodes a tool call's arguments whether they arrive as a JSON string or object.
func (t ToolCall) Args() map[string]any {
	out := map[string]any{}
	raw := t.Function.Arguments
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = []byte(s)
	}
	json.Unmarshal(raw, &out)
	return out
}

type Provider struct {
	Name, URL, Model, Key string
	client                *http.Client

	mu        sync.Mutex
	fails     int
	openUntil time.Time
	up        bool
}

func NewProvider(name, url, model, key string, timeout time.Duration) *Provider {
	return &Provider{Name: name, URL: url, Model: model, Key: key, client: &http.Client{Timeout: timeout}}
}

func (p *Provider) open() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.openUntil)
}

func (p *Provider) record(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.fails, p.up = 0, true
		return
	}
	p.fails++
	if p.fails >= 3 {
		p.openUntil, p.up = time.Now().Add(30*time.Second), false
	}
}

func (p *Provider) Up() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.up }

// Probe checks the provider and that the model is installed.
func (p *Provider) Probe() bool {
	req, _ := http.NewRequest("GET", p.URL+"/models", nil)
	if p.Key != "" {
		req.Header.Set("Authorization", "Bearer "+p.Key)
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Do(req)
	ok := false
	if err == nil && p.Key != "" && resp.StatusCode != 401 && resp.StatusCode != 403 && resp.StatusCode < 500 {
		// Hosted APIs (e.g. Workers AI) may not list models; a valid key is enough.
		resp.Body.Close()
		p.mu.Lock()
		p.up = true
		p.mu.Unlock()
		return true
	}
	if err == nil {
		defer resp.Body.Close()
		var out struct {
			Data []struct{ ID string } `json:"data"`
		}
		if resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&out) == nil {
			for _, m := range out.Data {
				if m.ID == p.Model || m.ID == p.Model+":latest" {
					ok = true
				}
			}
			if len(out.Data) == 0 && p.Key != "" { // some providers don't list models
				ok = true
			}
		}
	}
	p.mu.Lock()
	p.up = ok
	p.mu.Unlock()
	return ok
}

type Result struct {
	Message  Message
	Provider string
	Model    string
	Tokens   int
	Failover bool
}

func (p *Provider) chat(ctx context.Context, msgs []Message, tools []map[string]any) (Result, error) {
	body := map[string]any{"model": p.Model, "messages": msgs, "temperature": 0}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", p.URL+"/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if p.Key != "" {
		req.Header.Set("Authorization", "Bearer "+p.Key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return Result{}, fmt.Errorf("%s: %s %s", p.Name, resp.Status, b)
	}
	var out struct {
		Choices []struct{ Message Message } `json:"choices"`
		Usage   struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, err
	}
	if len(out.Choices) == 0 {
		return Result{}, errors.New("no choices")
	}
	return Result{Message: out.Choices[0].Message, Provider: p.Name, Model: p.Model, Tokens: out.Usage.TotalTokens}, nil
}

type Gateway struct {
	Internal *Provider
	External *Provider // optional
}

func (g *Gateway) Available() bool {
	return (g.Internal != nil && g.Internal.Up()) || (g.External != nil && g.External.Up())
}

func (g *Gateway) Chat(ctx context.Context, msgs []Message, tools []map[string]any, complex bool) (Result, error) {
	order := []*Provider{g.Internal, g.External}
	if complex {
		order = []*Provider{g.External, g.Internal}
	}
	var errs []error
	tried := 0
	for _, p := range order {
		if p == nil || p.open() {
			continue
		}
		res, err := p.chat(ctx, msgs, tools)
		p.record(err)
		if err == nil {
			res.Failover = tried > 0
			return res, nil
		}
		tried++
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return Result{}, errors.New("no LLM provider available (all circuits open)")
	}
	return Result{}, errors.Join(errs...)
}

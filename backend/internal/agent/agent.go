// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package agent runs one chat turn:
//
//	confirm-gate -> route (LLM or rules) -> tool (authZ + rules) -> answer -> persist + telemetry
//
// Money-moving tools (cancel, return, refund) are two-phase: a dry run checks
// eligibility and asks the customer to confirm; only "yes" commits.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"asksauby/internal/llm"
	"asksauby/internal/model"
	"asksauby/internal/obs"
	"asksauby/internal/store"
)

type Agent struct {
	Tools   *Tools
	Store   *store.Store
	GW      *llm.Gateway
	Mode    string // auto | llm | rules
	Source  string // chat | eval
	Variant string // agent version label for canary rollouts (e.g. "control", "canary")

	mu      sync.Mutex
	pending map[string]pendingAction // by session
	focus   map[string]string        // by session: the order the conversation is about ("this order")
	focusP  map[string]string        // by session: the product the conversation is about ("order this")
	shown   map[string][]string      // by session: products in the last result list
	history map[string][]llm.Message
}

type pendingAction struct {
	Tool    string
	Args    map[string]any
	Expires time.Time
}

type Trace struct {
	Router     string  `json:"router"`
	Provider   string  `json:"provider,omitempty"`
	Model      string  `json:"model,omitempty"`
	Failover   bool    `json:"failover,omitempty"`
	Complex    bool    `json:"complex"`
	AnswerMode string  `json:"answer_mode"`
	Path       string  `json:"path"`
	Variant    string  `json:"variant"`
	LatencyMS  float64 `json:"latency_ms"`
	TTFTMS     float64 `json:"ttft_ms"`
	RouteMS    float64 `json:"route_ms"`
	ToolMS     float64 `json:"tool_ms"`
	AnswerMS   float64 `json:"answer_ms"`
	Tokens     int     `json:"tokens"`
	RouterErr  string  `json:"router_error,omitempty"`
}

type Response struct {
	Answer       string         `json:"answer"`
	Tool         string         `json:"tool"`
	Args         map[string]any `json:"args"`
	Result       ToolResult     `json:"result"`
	OK           bool           `json:"ok"`
	NeedsConfirm bool           `json:"needs_confirm"`
	Escalated    bool           `json:"escalated"`
	AskFeedback  string         `json:"ask_feedback,omitempty"` // resolved | handoff | escalated: UI shows the feedback card
	Trace        Trace          `json:"trace"`
	CitedIDs     []string       `json:"cited_ids"`
}

var (
	yesRe = regexp.MustCompile(`(?i)^\s*(yes|yep|yeah|y|confirm|confirmed|go ahead|do it|sure|ok(ay)?|please do)\b`)
	noRe  = regexp.MustCompile(`(?i)^\s*(no|nope|n|cancel that|stop|don'?t|never ?mind)\b`)
	idRe  = regexp.MustCompile(`\b[PO]\d{4}\b`)
)

func New(t *Tools, s *store.Store, gw *llm.Gateway, mode, source string) *Agent {
	return &Agent{Tools: t, Store: s, GW: gw, Mode: mode, Source: source,
		pending: map[string]pendingAction{}, history: map[string][]llm.Message{}, focus: map[string]string{},
		focusP: map[string]string{}, shown: map[string][]string{}}
}

func (a *Agent) router() Router {
	if a.Mode == "rules" || a.GW == nil {
		return RuleRouter{}
	}
	if a.Mode == "llm" || a.GW.Available() {
		return LLMRouter{GW: a.GW}
	}
	return RuleRouter{}
}

func (a *Agent) RouterName() string { return a.router().Name() }

// Router exposes the current router (used by agent-assist for human teammates).
func (a *Agent) Router() Router { return a.router() }

func pathOf(tool string) string {
	switch tool {
	case "get_order", "list_orders", "cancel_order", "return_order", "request_refund", "place_order", "replace_order":
		return "transaction"
	case "chitchat":
		return "chitchat"
	case "handoff", "file_complaint":
		return "handoff"
	}
	return "faq"
}

var progress = map[string]string{
	"search_products": "Searching the catalog…", "price_search": "Filtering by price…",
	"get_product": "Pulling up that product…", "policy_search": "Checking our policies…",
	"get_order": "Looking up your order…", "list_orders": "Pulling up your orders…", "cancel_order": "Checking whether it can still be cancelled…",
	"return_order": "Checking the return window…", "request_refund": "Checking refund eligibility…",
	"handoff": "Connecting you with a teammate…", "place_order": "Checking stock and price…",
	"replace_order": "Checking the replacement window and stock…", "file_complaint": "Logging this for a teammate…",
}

// Handle runs one turn. emit receives progress messages for the "someone is
// here" feeling while slow steps run (sent over SSE by the API).
func (a *Agent) Handle(ctx context.Context, cid, sid, text string, emit func(string)) Response {
	t0 := time.Now()
	ttft := 0.0
	say := func(s string) {
		if ttft == 0 {
			ttft = ms(t0)
		}
		if emit != nil {
			emit(s)
		}
	}
	a.Store.AppendMessage(cid, sid, model.Message{TS: time.Now().UTC(), Role: "user", Content: text})
	say("Sauby is reading your message…")

	var resp Response
	var route Route
	a.mu.Lock()
	pend, hasPend := a.pending[sid]
	if hasPend && time.Now().After(pend.Expires) {
		hasPend = false
	}
	delete(a.pending, sid)
	a.mu.Unlock()

	switch {
	case hasPend && yesRe.MatchString(text):
		route = Route{Tool: pend.Tool, Args: pend.Args, Router: "confirm"}
		resp.Trace.RouteMS = ms(t0)
		say(map[string]string{"cancel_order": "Cancelling your order…", "return_order": "Creating your return…",
			"request_refund": "Processing your refund… this can take a few seconds.", "place_order": "Placing your order…",
			"replace_order": "Creating your replacement…"}[pend.Tool])
		t1 := time.Now()
		resp.Result = a.Tools.Run(route.Tool, route.Args, cid, sid, true)
		resp.Trace.ToolMS = ms(t1)
		resp.Answer, resp.Trace.AnswerMode = templateAnswer(route.Tool, route.Args, resp.Result, ""), "template"
	case hasPend && noRe.MatchString(text):
		route = Route{Tool: "chitchat", Args: map[string]any{}, Router: "confirm"}
		resp.Result = ToolResult{OK: true}
		resp.Answer, resp.Trace.AnswerMode = "No problem — I haven't changed anything. What else can I help with?", "template"
	default:
		hist := a.historyFor(cid, sid)
		route = a.router().Route(ctx, text, hist)
		resp.Trace.RouteMS = ms(t0)
		say(progress[route.Tool])
		args := clean(route.Args)
		if q, _ := args["query"].(string); q == "" && (route.Tool == "search_products" || route.Tool == "policy_search" || route.Tool == "price_search") {
			args["query"] = text
		}
		// "return this order": use the order the conversation is about
		if route.Tool == "cancel_order" || route.Tool == "return_order" || route.Tool == "request_refund" || route.Tool == "get_order" ||
			route.Tool == "replace_order" || route.Tool == "file_complaint" {
			if id, _ := args["order_id"].(string); !OrderRe.MatchString(id) && id != "LATEST" {
				a.mu.Lock()
				f := a.focus[sid]
				a.mu.Unlock()
				if f != "" {
					args["order_id"] = f
				}
			}
		}
		// "order this": use the product the conversation is about, or ask which one was meant
		var choose []string
		if route.Tool == "place_order" {
			if id, _ := args["product_id"].(string); !ProductRe.MatchString(id) {
				a.mu.Lock()
				f, shown := a.focusP[sid], a.shown[sid]
				a.mu.Unlock()
				if f != "" {
					args["product_id"] = f
				} else {
					choose = shown
				}
			}
		}
		route.Args = args
		t1 := time.Now()
		if len(choose) > 0 && !strings.HasPrefix(cid, "G-") {
			resp.Result = a.Tools.ChooseProduct(choose)
		} else {
			resp.Result = a.Tools.Run(route.Tool, args, cid, sid, false)
		}
		resp.Trace.ToolMS = ms(t1)
		t2 := time.Now()
		if resp.Result.NeedsConfirm {
			a.mu.Lock()
			a.pending[sid] = pendingAction{route.Tool, args, time.Now().Add(10 * time.Minute)}
			a.mu.Unlock()
			resp.NeedsConfirm = true
			resp.Answer, resp.Trace.AnswerMode = fmt.Sprintf("Just to confirm: I'll **%s**. Reply **yes** to go ahead or **no** to stop.", resp.Result.Message), "template"
		} else {
			resp.Answer, resp.Trace.AnswerMode, resp.Trace.Tokens = a.writeAnswer(ctx, text, route, resp.Result)
		}
		resp.Trace.AnswerMS = ms(t2)
	}

	resp.Tool, resp.Args = route.Tool, route.Args
	resp.OK = resp.Result.OK
	a.mu.Lock() // remember which order we're talking about
	if id := OrderRe.FindString(text); id != "" {
		a.focus[sid] = strings.ToUpper(id)
	}
	if resp.Result.Order != nil && resp.Result.OK {
		a.focus[sid] = resp.Result.Order.ID
	}
	if ps := resp.Result.Products; len(ps) > 0 && route.Tool != "place_order" { // what "this" refers to next
		a.shown[sid] = nil
		for _, p := range ps {
			a.shown[sid] = append(a.shown[sid], p.ID)
		}
		a.focusP[sid] = ""
		if len(ps) == 1 {
			a.focusP[sid] = ps[0].ID
		}
	}
	if id := ProductRe.FindString(text); id != "" {
		a.focusP[sid] = strings.ToUpper(id)
	}
	a.mu.Unlock()
	resp.Trace.Router, resp.Trace.Provider, resp.Trace.Model = route.Router, route.Provider, route.Model
	resp.Trace.Failover, resp.Trace.Complex, resp.Trace.RouterErr = route.Failover, route.Complex, route.Err
	resp.Trace.Tokens += route.Tokens
	resp.Trace.Path = pathOf(route.Tool)
	resp.Trace.Variant = a.Variant
	resp.Escalated = resp.Result.Escalated
	switch { // close the loop: ask for feedback when the issue is resolved or handed to a human
	case resp.Result.Committed:
		resp.AskFeedback = "resolved"
	case resp.Result.Ticket != nil && resp.Result.Ticket.Kind == "live":
		// a teammate is joining this chat: feedback is asked when they close it
	case resp.Result.Escalated && !resp.NeedsConfirm:
		resp.AskFeedback = "escalated"
		// handoff: feedback is asked when the human agent closes the chat (package live)
	}
	resp.Trace.LatencyMS, resp.Trace.TTFTMS = ms(t0), ttft
	seen := map[string]bool{}
	for _, id := range idRe.FindAllString(resp.Answer, -1) {
		if !seen[id] {
			seen[id] = true
			resp.CitedIDs = append(resp.CitedIDs, id)
		}
	}

	meta := map[string]any{"tool": resp.Tool, "ok": resp.OK, "needs_confirm": resp.NeedsConfirm, "trace": resp.Trace,
		"ask_feedback": resp.AskFeedback}
	if b, err := json.Marshal(resp.Result); err == nil {
		var m map[string]any
		json.Unmarshal(b, &m)
		meta["result"] = m
	}
	a.Store.AppendMessage(cid, sid, model.Message{TS: time.Now().UTC(), Role: "assistant", Content: resp.Answer, Tool: resp.Tool, Meta: meta})
	a.remember(sid, text, resp.Answer)

	n := len(resp.Result.Products) + len(resp.Result.Chunks)
	if resp.Result.Order != nil {
		n++
	}
	var flags []string
	if resp.Trace.Failover {
		flags = append(flags, "failover")
	}
	if route.Router == "rules-fallback" {
		flags = append(flags, "degraded")
	}
	if InjectionSuspect(text) {
		flags = append(flags, "injection_suspect")
	}
	if g := resp.Result.Guardrail; g != "" {
		flags = append(flags, "guardrail:"+g)
	}
	if a.Source == "chat" { // Prometheus metrics + one JSON log line per turn (evals read these)
		reg := obs.Default
		reg.Inc("sauby_turns_total", "Chat turns by tool, path and agent version.", "tool", resp.Tool, "path", resp.Trace.Path, "variant", a.Variant)
		reg.Observe("sauby_turn_latency_seconds", "End-to-end turn latency.", resp.Trace.LatencyMS/1000, "path", resp.Trace.Path)
		reg.Observe("sauby_first_status_seconds", "Time until the customer sees the first status message.", ttft/1000, "path", resp.Trace.Path)
		reg.Add("sauby_llm_tokens_total", "LLM tokens used.", float64(resp.Trace.Tokens), "model", route.Model)
		if !resp.OK {
			reg.Inc("sauby_tool_not_ok_total", "Turns where the tool returned not-ok (denied, not found, empty).", "tool", resp.Tool)
		}
		for _, f := range flags {
			reg.Inc("sauby_flags_total", "Guardrails, failovers, degraded mode and injection flags.", "flag", f)
		}
		if resp.Result.Committed {
			reg.Inc("sauby_actions_committed_total", "Order actions completed (resolutions).", "tool", resp.Tool)
		}
		obs.Log.Info("turn", "cid", cid, "sid", sid, "tool", resp.Tool, "path", resp.Trace.Path, "router", route.Router,
			"model", route.Model, "ok", resp.OK, "latency_ms", resp.Trace.LatencyMS, "ttft_ms", ttft, "tokens", resp.Trace.Tokens,
			"n_results", n, "committed", resp.Result.Committed, "escalated", resp.Escalated, "flags", flags, "variant", a.Variant)
	}
	a.Store.LogEvent(model.Event{TS: time.Now().UTC(), CustomerID: cid, SessionID: sid, Query: text, Tool: resp.Tool,
		Path: resp.Trace.Path, Router: route.Router, Model: route.Model, OK: resp.OK, Status: 200, Error: route.Err,
		NResults: n, LatencyMS: resp.Trace.LatencyMS, TTFTMS: ttft, Tokens: resp.Trace.Tokens, Source: a.Source,
		Resolved: resp.Result.Committed, Handoff: resp.OK && resp.Result.Ticket != nil && resp.Result.Ticket.Kind == "live",
		Escalated: resp.Result.Escalated && !resp.NeedsConfirm, Variant: a.Variant, Flags: flags})
	return resp
}

var injRe = regexp.MustCompile(`(?i)(ignore (all |the )?(previous|above) instructions|system prompt|you are now|developer mode|refund (all|every) order)`)

// InjectionSuspect is a cheap prompt-injection signal for the WAF/ops dashboard.
// Real protection comes from authZ living in the tools, not from the prompt.
func InjectionSuspect(text string) bool { return injRe.MatchString(text) }

func (a *Agent) historyFor(cid, sid string) []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if h, ok := a.history[sid]; ok {
		return h
	}
	msgs, _ := a.Store.Messages(cid, sid) // resume a session after restart
	var h []llm.Message
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "assistant" {
			h = append(h, llm.Message{Role: m.Role, Content: m.Content})
		}
	}
	if len(h) > 0 && h[len(h)-1].Role == "user" {
		h = h[:len(h)-1] // the message we're handling right now
	}
	a.history[sid] = tail(h, 8)
	return a.history[sid]
}

func (a *Agent) remember(sid, user, answer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history[sid] = tail(append(a.history[sid], llm.Message{Role: "user", Content: user},
		llm.Message{Role: "assistant", Content: answer}), 8)
}

func clean(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		if !strings.HasPrefix(k, "_") && v != nil {
			out[k] = v
		}
	}
	return out
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

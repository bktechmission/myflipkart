// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package live hands a conversation from Sauby (the AI) to a human support agent.
//
// How a real contact center does it, and how this package models it:
//
//  1. Sauby's handoff tool opens a "live" ticket: customer id (cid), session id (sid),
//     a one-line summary, and the customer's context (recent turns, orders, pending
//     approvals). The conversation switches to mode "waiting".
//  2. A human agent picks the ticket from the queue (Admin → Live chats), sees the
//     context, and joins THE SAME chat window: their messages are appended to the
//     same cid+sid conversation the customer is looking at.
//  3. While a human owns the conversation (mode "human"), Sauby stays silent.
//  4. The agent closes the ticket; Sauby takes over again and asks for feedback.
//
// Demo mode (SIM_AGENTS=1): if no real person joins within SIM_JOIN_AFTER_SEC, a
// simulated teammate joins so you can see the experience end to end. Simulated
// teammates are always labeled "Demo" in the customer's chat — presenting software
// as a real person to customers would be deceptive (and is regulated in some places).
package live

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"time"

	"asksauby/internal/agent"
	"asksauby/internal/model"
	"asksauby/internal/obs"
	"asksauby/internal/store"
)

// Personas used by simulated teammates in demo mode.
var Personas = []string{"Priya Sharma", "Daniel Brooks", "Mei Tan", "Carlos Rivera"}

// Hub tracks live tickets and runs simulated teammates.
type Hub struct {
	S         *store.Store
	Tools     *agent.Tools
	Router    func() agent.Router // agent-assist: suggests tools to the (simulated) human
	Sim       bool
	JoinAfter time.Duration

	mu     sync.Mutex
	typing map[string]string // sid -> name of agent currently typing
	joinMu sync.Mutex        // one agent per ticket, even if two click "Join" at once
}

// State is what the customer's chat window polls.
type State struct {
	Mode      string          `json:"mode"` // bot | waiting | human
	TicketID  string          `json:"ticket_id,omitempty"`
	AgentName string          `json:"agent_name,omitempty"`
	AgentKind string          `json:"agent_kind,omitempty"` // human | simulated
	Typing    string          `json:"typing,omitempty"`
	Messages  []model.Message `json:"messages"` // messages after the client's cursor
	Total     int             `json:"total"`    // cursor for the next poll
}

func (h *Hub) StateFor(cid, sid string, after int) State {
	st := State{Mode: "bot"}
	if t, ok := h.S.OpenLiveTicket(sid); ok && t.CustomerID == cid {
		st.TicketID, st.AgentName, st.AgentKind = t.ID, t.AgentName, t.AgentKind
		st.Mode = map[string]string{"waiting": "waiting", "active": "human"}[t.Status]
	}
	h.mu.Lock()
	st.Typing = h.typing[sid]
	h.mu.Unlock()
	msgs, _ := h.S.Messages(cid, sid)
	st.Total = len(msgs)
	if after < 0 {
		after = 0
	}
	if after < len(msgs) {
		st.Messages = msgs[after:]
	} else {
		st.Messages = []model.Message{}
	}
	return st
}

// HumanOwns reports whether a person (real or simulated) owns the conversation,
// in which case Sauby must not answer.
func (h *Hub) HumanOwns(sid string) (model.Ticket, bool) {
	t, ok := h.S.OpenLiveTicket(sid)
	return t, ok && (t.Status == "active" || t.Status == "waiting")
}

func (h *Hub) say(t model.Ticket, role, text string, meta map[string]any) {
	if meta == nil {
		meta = map[string]any{}
	}
	meta["agent_name"], meta["agent_kind"], meta["ticket_id"] = t.AgentName, t.AgentKind, t.ID
	h.S.AppendMessage(t.CustomerID, t.SessionID, model.Message{TS: time.Now().UTC(), Role: role, Content: text, Meta: meta})
}

// Join assigns an agent to a waiting ticket.
func (h *Hub) Join(ticketID, name, kind string) (model.Ticket, error) {
	h.joinMu.Lock()
	defer h.joinMu.Unlock()
	t, ok := h.S.Ticket(ticketID)
	if !ok || t.Kind != "live" {
		return t, fmt.Errorf("no live ticket %s", ticketID)
	}
	if t.Status != "waiting" {
		return t, fmt.Errorf("ticket %s is already %s (by %s)", ticketID, t.Status, t.AgentName)
	}
	t.Status, t.AgentName, t.AgentKind, t.JoinedAt = "active", name, kind, time.Now().UTC()
	if err := h.S.SaveTicket(t); err != nil {
		return t, err
	}
	h.say(t, "system", fmt.Sprintf("%s joined the chat", first(name)), nil)
	obs.Default.Observe("sauby_handoff_wait_seconds", "Time from handoff to an agent joining.",
		t.JoinedAt.Sub(t.CreatedAt).Seconds(), "agent_kind", kind)
	obs.Log.Info("agent_joined", "ticket", t.ID, "cid", t.CustomerID, "sid", t.SessionID, "agent_kind", kind)
	return t, nil
}

// Send posts an agent message into the customer's chat.
func (h *Hub) Send(ticketID, text string) error {
	t, ok := h.S.Ticket(ticketID)
	if !ok || t.Status != "active" {
		return fmt.Errorf("ticket %s is not active", ticketID)
	}
	h.say(t, "agent", strings.TrimSpace(text), nil)
	return nil
}

// Close ends the human part; Sauby takes over and asks for feedback.
func (h *Hub) Close(ticketID string) error {
	t, ok := h.S.Ticket(ticketID)
	if !ok || t.Status == "closed" {
		return fmt.Errorf("ticket %s is not open", ticketID)
	}
	t.Status, t.ClosedAt = "closed", time.Now().UTC()
	if err := h.S.SaveTicket(t); err != nil {
		return err
	}
	h.say(t, "system", fmt.Sprintf("%s left the chat. Sauby is back if you need anything else.", first(t.AgentName)),
		map[string]any{"ask_feedback": "handoff_closed"})
	obs.Default.Inc("sauby_handoffs_closed_total", "Live handoffs closed.", "agent_kind", t.AgentKind)
	return nil
}

// CustomerMessage is called when the customer writes while a human owns the chat.
// For a simulated teammate it schedules a reply.
func (h *Hub) CustomerMessage(t model.Ticket, text string) {
	if t.Status == "active" && t.AgentKind == "simulated" {
		go h.simReply(t, text)
	}
}

// Context is what the human agent sees when picking a ticket.
type Context struct {
	Ticket    model.Ticket     `json:"ticket"`
	Customer  model.User       `json:"customer"`
	Orders    []model.Order    `json:"orders"`
	Approvals []model.Approval `json:"approvals"`
	Messages  []model.Message  `json:"messages"`
}

func (h *Hub) ContextFor(t model.Ticket) Context {
	u, _ := h.S.User(t.CustomerID)
	msgs, _ := h.S.Messages(t.CustomerID, t.SessionID)
	var aps []model.Approval
	for _, a := range h.S.Approvals() {
		if a.CustomerID == t.CustomerID && a.Status == "pending" {
			aps = append(aps, a)
		}
	}
	orders := h.S.OrdersFor(t.CustomerID)
	if len(orders) > 5 {
		orders = orders[len(orders)-5:]
	}
	return Context{Ticket: t, Customer: u, Orders: orders, Approvals: aps, Messages: msgs}
}

// ---------------- simulated teammates (demo mode) ----------------

// Run joins waiting tickets with a simulated teammate after JoinAfter. Real agents
// always get the first chance.
func (h *Hub) Run(ctx context.Context) {
	if !h.Sim {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, t := range h.S.Tickets() {
			if t.Kind == "live" && t.Status == "waiting" && time.Since(t.CreatedAt) >= h.JoinAfter {
				h.simJoin(t) // synchronous: Join flips the status, so the next tick won't pick it again
			}
		}
	}
}

func persona(id string) string {
	f := fnv.New32a()
	f.Write([]byte(id))
	return Personas[int(f.Sum32())%len(Personas)]
}

func (h *Hub) typeFor(t model.Ticket, d time.Duration) {
	h.mu.Lock()
	if h.typing == nil {
		h.typing = map[string]string{}
	}
	h.typing[t.SessionID] = first(t.AgentName)
	h.mu.Unlock()
	time.Sleep(d)
	h.mu.Lock()
	delete(h.typing, t.SessionID)
	h.mu.Unlock()
}

func (h *Hub) simJoin(t model.Ticket) {
	t, err := h.Join(t.ID, persona(t.ID), "simulated")
	if err != nil {
		return // a real agent got there first
	}
	go h.simGreet(t)
}

func (h *Hub) simGreet(t model.Ticket) {
	c := h.ContextFor(t)
	h.typeFor(t, 1800*time.Millisecond)
	greet := fmt.Sprintf("Hi %s, I'm %s from MyFlipKart support. I've read your chat with Sauby.", first(c.Customer.Name), first(t.AgentName))
	if len(c.Approvals) > 0 { // a refund waiting for a human: the teammate reviews it
		a := c.Approvals[0]
		what := map[string]string{"order": "new order", "replacement": "replacement", "return": "return"}[a.Kind]
		if what == "" {
			what = "refund"
		}
		h.say(t, "agent", greet+fmt.Sprintf(" I can see your $%.2f %s for %s is waiting for approval. Let me review it.", a.Amount, what, a.OrderID), nil)
		h.typeFor(t, 2500*time.Millisecond)
		if d, err := h.Tools.DecideApproval(a.ID, true, t.AgentName+" (simulated)"); err == nil {
			msg := "Done — I've approved it. "
			switch {
			case d.Kind == "return":
				msg += fmt.Sprintf("Your prepaid return label is in your email, and the $%.2f refund is released as soon as the item reaches us.", d.Amount)
			case d.Kind == "order" || d.Kind == "replacement":
				msg += fmt.Sprintf("Order %s will ship soon.", d.OrderID)
			case d.Refunded:
				msg += fmt.Sprintf("$%.2f will be back on your original payment method in 5-7 business days.", d.Amount)
			default:
				msg += fmt.Sprintf("Your $%.2f refund is paid as soon as the returned item reaches us.", d.Amount)
			}
			h.say(t, "agent", msg+" Is there anything else I can help with?", nil)
		}
		return
	}
	h.say(t, "agent", greet+" How can I help?", nil)
}

var byeRe = regexp.MustCompile(`(?i)^\s*(thanks|thank you|thx|that'?s all|no(pe)?|bye|all good|perfect)\b`)

// simReply answers like a support teammate using agent-assist: the same tools Sauby
// uses, run read-only (commit=false), phrased in first person.
func (h *Hub) simReply(t model.Ticket, text string) {
	h.typeFor(t, 2000*time.Millisecond)
	if byeRe.MatchString(text) {
		h.say(t, "agent", "You're welcome! I'll hand you back to Sauby now. Have a great day.", nil)
		h.Close(t.ID)
		return
	}
	r := h.Router().Route(context.Background(), text, nil)
	if r.Tool == "handoff" || r.Tool == "chitchat" {
		h.say(t, "agent", "Sure — tell me a bit more, or share the order number and I'll look it up.", nil)
		return
	}
	res := h.Tools.Run(r.Tool, r.Args, t.CustomerID, t.SessionID, false)
	reply := res.Message
	switch {
	case res.NeedsConfirm:
		reply = "I can " + res.Message + ". Sauby can complete that for you right after we finish here — just ask it and reply yes."
	case len(res.Products) > 0:
		p := res.Products[0]
		reply = fmt.Sprintf("I'd suggest the %s (%s) at $%.0f — %s.", p.Name, p.ID, p.Price, strings.Join(p.Features[:min(2, len(p.Features))], ", "))
	case len(res.Chunks) > 0:
		var parts []string
		for i, c := range res.Chunks {
			if i == 2 {
				break
			}
			body := c.Text
			if j := strings.Index(body, ": "); j >= 0 {
				body = body[j+2:]
			}
			parts = append(parts, body)
		}
		reply = "Good question. " + strings.Join(parts, " ")
	case res.Order != nil && res.OK:
		reply = fmt.Sprintf("I've got order %s here: it's %s, total $%.2f.", res.Order.ID, strings.ReplaceAll(res.Order.Status, "_", " "), res.Order.Total)
	case reply == "":
		reply = "Let me check on that and get back to you in a moment."
	}
	h.say(t, "agent", reply, nil)
}

func first(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
}

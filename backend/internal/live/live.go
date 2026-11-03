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
	typing map[string]string         // sid -> name of agent currently typing
	inv    map[string]*investigation // ticket -> what the simulated teammate is checking
	joinMu sync.Mutex                // one agent per ticket, even if two click "Join" at once
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
	Standing  agent.Standing   `json:"standing"` // loyal / standard / watch / blocked, and why
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
	return Context{Ticket: t, Customer: u, Orders: orders, Approvals: aps, Messages: msgs, Standing: h.Tools.CustomerStanding(t.CustomerID)}
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

// investigation is how a teammate reviews a held return before approving it:
// ask about the item's condition, ask for a photo above the customer's threshold,
// then decide (approve, decline, or pass to a specialist).
type investigation struct {
	a     model.Approval
	step  string // condition | photo | done
	cond  string
	tries int
	note  []string
}

var (
	unusedRe  = regexp.MustCompile(`(?i)\b(unused|unopened|never (been )?(used|worn|opened|worn it)|not (been )?(used|worn|opened)|brand new|like new|sealed|still in (the |its )?box|original (packaging|box)|tags (are )?(still )?on|didn'?t (use|wear|open))\b`)
	defectRe  = regexp.MustCompile(`(?i)\b(defective|broken|faulty|damaged|cracked|doesn'?t (work|turn on|charge)|not working|stopped working|dead|wrong (item|size|color|colour|model)|arrived (broken|damaged))\b`)
	usedRe    = regexp.MustCompile(`(?i)\b(used|worn|wore|opened|tried (it|them) (on|out)|washed|scratched|for a (few|couple of) (days|weeks|months)|ran in)\b`)
	missingRe = regexp.MustCompile(`(?i)\b(missing|lost|threw away|without) (the )?(box|charger|cable|case|accessor\w*|parts?|packaging)\b`)
)

func (h *Hub) simGreet(t model.Ticket) {
	c := h.ContextFor(t)
	h.typeFor(t, 1800*time.Millisecond)
	greet := fmt.Sprintf("Hi %s, I'm %s from MyFlipKart support. I've read your chat with Sauby.", first(c.Customer.Name), first(t.AgentName))
	if c.Standing.Tier == "loyal" {
		greet += " Thanks for being a loyal customer."
	}
	if len(c.Approvals) == 0 {
		h.say(t, "agent", greet+" How can I help?", nil)
		return
	}
	a := c.Approvals[0]
	item := a.OrderID
	if o, ok := h.S.Order(a.OrderID); ok && len(o.Items) > 0 {
		if p, ok := h.S.Product(o.Items[0]); ok {
			item = fmt.Sprintf("the %s from order %s", p.Name, a.OrderID)
		}
	}
	switch {
	case a.NoReturn: // missing item: check the courier, decide on standing
		h.say(t, "agent", greet+fmt.Sprintf(" I'm sorry %s hasn't reached you. Let me check the tracking with the courier.", item), nil)
		h.typeFor(t, 3000*time.Millisecond)
		if c.Standing.Tier == "watch" || c.Standing.Tier == "blocked" {
			h.say(t, "agent", "The courier shows it as delivered, so I've opened a formal claim with them. Our claims team will email you within 24 hours with the outcome.", nil)
			return
		}
		d, err := h.Tools.DecideApproval(a.ID, agent.Decision{Approve: true, Condition: "not_received", Note: "courier could not confirm the drop-off"}, t.AgentName+" (simulated)")
		if err == nil {
			h.say(t, "agent", fmt.Sprintf("The courier couldn't confirm the drop-off, so I've approved a full refund of $%.2f. It reaches your original payment method in 5-7 business days. Anything else I can help with?", d.Amount), nil)
		}
		return
	case a.Kind == "order" || a.Kind == "replacement":
		h.say(t, "agent", greet+fmt.Sprintf(" I can see your %s %s is waiting for a review. Let me check it.", map[string]string{"order": "new order", "replacement": "replacement"}[a.Kind], a.OrderID), nil)
		h.typeFor(t, 2500*time.Millisecond)
		if _, err := h.Tools.DecideApproval(a.ID, agent.Decision{Approve: true, Note: "reviewed"}, t.AgentName+" (simulated)"); err == nil {
			h.say(t, "agent", fmt.Sprintf("All good: I've approved it, and order %s will ship soon. Is there anything else I can help with?", a.OrderID), nil)
		}
		return
	}
	h.mu.Lock()
	if h.inv == nil {
		h.inv = map[string]*investigation{}
	}
	h.inv[t.ID] = &investigation{a: a, step: "condition"}
	h.mu.Unlock()
	h.say(t, "agent", greet+fmt.Sprintf(" I'm reviewing your return for %s ($%.2f). Before I approve it, a couple of quick questions: "+
		"is the item unused and in its original packaging, or has it been used or worn? If something's wrong with it, tell me what. "+
		"And do you still have all the accessories?", item, a.Amount), nil)
}

// investigate continues a return review. It returns false when there's no review in progress.
func (h *Hub) investigate(t model.Ticket, text string, photo bool) bool {
	h.mu.Lock()
	iv := h.inv[t.ID]
	h.mu.Unlock()
	if iv == nil || iv.step == "done" {
		return false
	}
	st := h.Tools.CustomerStanding(t.CustomerID)
	decide := func(approve bool, note string) {
		iv.note = append(iv.note, note)
		d, err := h.Tools.DecideApproval(iv.a.ID, agent.Decision{Approve: approve, Condition: iv.cond, Note: strings.Join(iv.note, "; ")}, t.AgentName+" (simulated)")
		iv.step = "done"
		if err != nil {
			h.say(t, "agent", "I've passed this to our returns specialist, who'll email you within a day.", nil)
			return
		}
		if !approve {
			return
		}
		msg := "Thanks, that's everything I need. I've approved your return: the prepaid label is in your email, and "
		if d.Refunded {
			msg = fmt.Sprintf("Thanks, that's everything I need. I've approved it, and $%.2f goes back to your original payment method in 5-7 business days.", d.Amount)
		} else {
			msg += fmt.Sprintf("your $%.2f refund is released as soon as the item reaches our warehouse.", d.Amount)
		}
		h.say(t, "agent", msg+" Is there anything else I can help with?", nil)
	}
	switch iv.step {
	case "condition":
		if photo {
			iv.note = append(iv.note, "customer sent a photo early")
		}
		if missingRe.MatchString(text) {
			iv.note = append(iv.note, "customer reports missing accessories/packaging")
		}
		switch {
		case defectRe.MatchString(text):
			iv.cond = "defective"
			iv.note = append(iv.note, "customer reports: "+text)
		case unusedRe.MatchString(text):
			iv.cond = "unused"
		case usedRe.MatchString(text):
			iv.cond = "used"
			iv.step = "done"
			h.Tools.DecideApproval(iv.a.ID, agent.Decision{Approve: false, Condition: "used", Note: "item used and not faulty"}, t.AgentName+" (simulated)")
			h.say(t, "agent", "Thanks for being upfront. Because the item has been used and isn't faulty, it doesn't qualify for a return under our policy: "+
				"items need to be in their original condition. If it ever stops working, it's covered by the 1-year warranty and I'm happy to help with that. Is there anything else I can do for you?", nil)
			return true
		default:
			iv.tries++
			if iv.tries >= 3 {
				iv.step = "done"
				h.say(t, "agent", "No problem. I'll pass this to our returns specialist with our chat, and they'll email you within a day.", nil)
				return true
			}
			h.say(t, "agent", "Sorry, just so I get this right: is the item unused (still like new), or has it been used? And is anything wrong with it, like damage or a fault?", nil)
			return true
		}
		if missingRe.MatchString(text) && iv.cond == "unused" {
			h.say(t, "agent", "Thanks. Since some accessories or packaging are missing, I'll note that on the return; please send back everything you still have.", nil)
		}
		if iv.a.Amount > st.PhotoOver && !photo {
			iv.step = "photo"
			h.say(t, "agent", fmt.Sprintf("Got it, thank you. Because this item is over $%.0f, I need a quick look at it: could you upload a clear photo%s using the camera button next to the message box?",
				st.PhotoOver, map[bool]string{true: " showing the fault or damage", false: " of the item"}[iv.cond == "defective"]), nil)
			return true
		}
		decide(true, "condition: "+iv.cond)
	case "photo":
		if !photo {
			iv.tries++
			h.say(t, "agent", "Whenever you're ready, tap the camera button next to the message box to add the photo. I'll wait right here.", nil)
			return true
		}
		h.typeFor(t, 2000*time.Millisecond)
		h.say(t, "agent", "Thanks for the photo, I can see it clearly and it matches what you described.", nil)
		decide(true, "condition: "+iv.cond+"; photo reviewed")
	}
	return true
}

// CustomerPhoto is called after the customer uploads a photo into a live chat.
func (h *Hub) CustomerPhoto(t model.Ticket) {
	if t.Status == "active" && t.AgentKind == "simulated" {
		go func() {
			h.typeFor(t, 1200*time.Millisecond)
			if !h.investigate(t, "", true) {
				h.say(t, "agent", "Thanks for the photo, I've added it to your case.", nil)
			}
		}()
	}
}

var byeRe = regexp.MustCompile(`(?i)^\s*(thanks|thank you|thx|that'?s all|no(pe)?|bye|all good|perfect)\b`)

// simReply answers like a support teammate using agent-assist: the same tools Sauby
// uses, run read-only (commit=false), phrased in first person.
func (h *Hub) simReply(t model.Ticket, text string) {
	h.typeFor(t, 2000*time.Millisecond)
	if h.investigate(t, text, false) {
		return
	}
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

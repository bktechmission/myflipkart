// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Tools the orchestrator can call. Authorization (own orders only), two-phase
// confirm, refund guardrails ($30 limit, velocity) and idempotency all live here,
// inside one store transaction, so no prompt can talk its way past them.

package agent

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/search"
	"asksauby/internal/store"
)

// ToolSchemas describe the tools to the LLM (OpenAI function-calling format).
var ToolSchemas = []map[string]any{
	fn("search_products", "Search the MyFlipKart.com catalog by meaning or keywords (features, use cases, brand, model names).",
		props{"query": {"string", "What the customer is looking for"}}, "query"),
	fn("price_search", "Search products within a price range, e.g. 'headphones under $100' or 'laptops between 500 and 1000'.",
		props{"query": {"string", "Product type or need, without the price words"},
			"min_price": {"number", "Lower bound in USD; omit if none"}, "max_price": {"number", "Upper bound in USD; omit if none"}}, "query"),
	fn("get_product", "Get full details for one product by ID like P1003.", props{"product_id": {"string", ""}}, "product_id"),
	fn("get_order", "Look up status, items and dates of one of the customer's orders by ID like O5003. Use LATEST for their most recent order.", props{"order_id": {"string", "Order ID, or LATEST"}}, "order_id"),
	fn("list_orders", "List the customer's recent orders (order history).", props{}),
	fn("cancel_order", "Cancel one of the customer's orders before it ships (refund is automatic). Omit order_id if not given.", props{"order_id": {"string", "Order ID if the customer gave one"}}),
	fn("return_order", "Start a return (RMA + label) for a delivered order within the return window. Omit order_id if not given.",
		props{"order_id": {"string", "Order ID if the customer gave one"}, "reason": {"string", "Why the customer is returning it"}}),
	fn("request_refund", "Refund an order: a delivered order within the return window, or one whose return was started. Omit order_id if not given.",
		props{"order_id": {"string", "Order ID if the customer gave one"}, "reason": {"string", "Why the customer wants a refund"}}),
	fn("policy_search", "Answer general questions about MyFlipKart.com policies: returns, refunds, cancellation, shipping, warranty. Use when no order ID is given.",
		props{"query": {"string", ""}}, "query"),
	fn("handoff", "Transfer the customer to a human support agent when they ask for a person or are frustrated.",
		props{"summary": {"string", "One-line summary of the issue for the human agent"}}, "summary"),
}

var ToolNames = []string{"search_products", "price_search", "get_product", "list_orders", "get_order", "cancel_order",
	"return_order", "request_refund", "policy_search", "handoff", "chitchat"}

var ConfirmTools = map[string]bool{"cancel_order": true, "return_order": true, "request_refund": true}

type props map[string][2]string

func fn(name, desc string, p props, required ...string) map[string]any {
	properties := map[string]any{}
	for k, v := range p {
		properties[k] = map[string]any{"type": v[0], "description": v[1]}
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": required}}}
}

// ---------------- result types ----------------

type Card struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Category string      `json:"category"`
	Price    float64     `json:"price"`
	Rating   float64     `json:"rating"`
	Stock    int         `json:"stock"`
	Features []string    `json:"features"`
	Scores   *search.Hit `json:"scores,omitempty"`
}

// Choice is a suggested reply the customer can tap (e.g. "Return O5009").
type Choice struct {
	Label string `json:"label"`
	Say   string `json:"say"`
}

type OrderView struct {
	model.Order
	ItemCards []Card `json:"item_cards"`
}

type Chunk struct {
	search.Hit
	Doc   string `json:"doc"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type ToolResult struct {
	OK           bool                  `json:"ok"`
	Message      string                `json:"message,omitempty"`
	Products     []Card                `json:"products,omitempty"`
	Chunks       []Chunk               `json:"chunks,omitempty"`
	Order        *OrderView            `json:"order,omitempty"`
	Refund       *model.Refund         `json:"refund,omitempty"`
	Ticket       *model.Ticket         `json:"ticket,omitempty"`
	Approval     *model.Approval       `json:"approval,omitempty"`
	Orders       []*OrderView          `json:"orders,omitempty"`  // list_orders, or candidates when asking "which order?"
	Choices      []Choice              `json:"choices,omitempty"` // tappable follow-ups for the customer
	Retrieval    *search.RetrievalInfo `json:"retrieval,omitempty"`
	NeedsConfirm bool                  `json:"needs_confirm,omitempty"`
	Committed    bool                  `json:"committed,omitempty"` // the customer's action is done
	Escalated    bool                  `json:"escalated,omitempty"` // a guardrail handed it to a human
	Guardrail    string                `json:"guardrail,omitempty"` // which guardrail fired
}

type Tools struct {
	S               *store.Store
	R               *search.Retriever
	ReturnWindow    int
	AutoRefundLimit float64 // refunds above this go to a human (default $30)
	MaxAutoRefunds  int     // per customer per 24h before a human must look (velocity guardrail)
}

func (t *Tools) card(id string, h *search.Hit) Card {
	p, _ := t.R.Product(id)
	return Card{p.ID, p.Name, p.Category, p.Price, p.Rating, p.Stock, p.Features, h}
}

func (t *Tools) view(o model.Order) *OrderView {
	v := &OrderView{Order: o}
	for _, i := range o.Items {
		v.ItemCards = append(v.ItemCards, t.card(i, nil))
	}
	return v
}

// Run executes a tool for the authenticated customer cid. commit=false is a dry
// run for confirm-gated tools (eligibility check only, nothing written).
func (t *Tools) Run(name string, args map[string]any, cid, sid string, commit bool) ToolResult {
	str := func(k string) string { s, _ := args[k].(string); return strings.TrimSpace(s) }
	switch name {
	case "search_products":
		return t.search(str("query"), nil, nil)
	case "price_search":
		lo, hi := num(args["min_price"]), num(args["max_price"])
		q := str("query")
		if lo == nil && hi == nil { // model forgot bounds: parse them from text
			lo, hi = ParsePrice(q)
		}
		res := t.search(StripPrice(q), lo, hi)
		if !res.OK {
			res.Message = "No products in that price range."
		}
		return res
	case "get_product":
		id := strings.ToUpper(str("product_id"))
		if _, ok := t.R.Product(id); !ok {
			return ToolResult{Message: "No product with ID " + id + "."}
		}
		return ToolResult{OK: true, Products: []Card{t.card(id, nil)}}
	case "policy_search":
		hits, info, err := t.R.SearchPolicies(str("query"), 0, "")
		if err != nil {
			return ToolResult{Message: err.Error()}
		}
		res := ToolResult{OK: len(hits) > 0, Retrieval: &info}
		for _, h := range hits {
			c := t.R.Chunk(h.ID)
			res.Chunks = append(res.Chunks, Chunk{h, c.Doc, c.Title, c.Text})
		}
		return res
	case "get_order", "cancel_order", "return_order", "request_refund":
		if strings.HasPrefix(cid, "G-") { // guests can browse and ask, never touch orders
			return ToolResult{Message: "Please sign in with your MyFlipKart.com account to view or change orders.", Guardrail: "guest_blocked"}
		}
		oid := strings.ToUpper(str("order_id"))
		if !OrderRe.MatchString(oid) && oid != "LATEST" && name != "get_order" {
			return t.chooseOrder(name, cid) // "start a return" with no order in context: ask which one
		}
		if oid == "LATEST" || oid == "" { // "where is my latest order?"
			mine := t.S.OrdersFor(cid)
			if len(mine) == 0 {
				return ToolResult{Message: "I don't see any orders on your account yet."}
			}
			oid = mine[len(mine)-1].ID
		}
		return t.orderAction(name, oid, str("reason"), cid, sid, commit)
	case "list_orders":
		if strings.HasPrefix(cid, "G-") {
			return ToolResult{Message: "Please sign in with your MyFlipKart.com account to see your orders.", Guardrail: "guest_blocked"}
		}
		mine := t.S.OrdersFor(cid)
		res := ToolResult{OK: true}
		for i := len(mine) - 1; i >= 0 && len(res.Orders) < 6; i-- { // newest first
			res.Orders = append(res.Orders, t.view(mine[i]))
		}
		if len(res.Orders) == 0 {
			res.Message = "I don't see any orders on your account yet."
		}
		return res
	case "handoff":
		if strings.HasPrefix(cid, "G-") {
			return ToolResult{Message: "Please sign in so a teammate can see your account and orders.", Guardrail: "guest_blocked"}
		}
		if open, ok := t.S.OpenLiveTicket(sid); ok { // idempotent: one live ticket per conversation
			return ToolResult{OK: true, Ticket: &open, Message: "A teammate is already on the way (ticket " + open.ID + ")."}
		}
		tk := model.Ticket{ID: fmt.Sprintf("T%d", time.Now().UnixNano()%1e8), Kind: "live", CustomerID: cid, SessionID: sid,
			Summary: str("summary"), Status: "waiting", CreatedAt: time.Now().UTC()}
		if err := t.S.Tx(func(tx *store.Txn) error { tx.PutTicket(tk); return nil }); err != nil {
			return ToolResult{Message: err.Error()}
		}
		return ToolResult{OK: true, Ticket: &tk, Message: "I'm connecting you with a teammate now. They'll see our conversation, so you won't need to repeat yourself."}
	case "chitchat":
		return ToolResult{OK: true}
	}
	return ToolResult{Message: "Unknown tool " + name}
}

func (t *Tools) search(q string, lo, hi *float64) ToolResult {
	hits, info, err := t.R.SearchProducts(search.ProductQuery{Query: q, MinPrice: lo, MaxPrice: hi})
	if err != nil {
		return ToolResult{Message: err.Error()}
	}
	res := ToolResult{OK: len(hits) > 0, Retrieval: &info}
	for i := range hits {
		res.Products = append(res.Products, t.card(hits[i].ID, &hits[i]))
	}
	return res
}

// orderAction enforces authorization and business rules. Everything runs inside
// a store transaction, so eligibility is re-checked under the lock at commit time
// (no double refunds, no cancel after the warehouse marked it shipped).
func (t *Tools) orderAction(kind, oid, reason, cid, sid string, commit bool) ToolResult {
	var res ToolResult
	err := t.S.Tx(func(tx *store.Txn) error {
		o, ok := tx.Order(oid)
		if !ok || o.CustomerID != cid { // same answer either way: never reveal others' orders exist
			res = ToolResult{Message: fmt.Sprintf("I couldn't find order %s on your account.", oid)}
			return nil
		}
		res.Order = t.view(o)
		age := -1
		if o.DeliveredOn != "" {
			d, _ := time.Parse("2006-01-02", o.DeliveredOn)
			age = int(time.Since(d).Hours() / 24)
		}
		deny := func(msg string) { res.OK, res.Message = false, msg }
		switch kind {
		case "get_order":
			res.OK = true
			return nil
		case "cancel_order":
			if o.Status != "placed" {
				extra := ""
				if o.Status == "shipped" {
					extra = " It's on the way — once it's delivered I can start a return for you."
				}
				deny(fmt.Sprintf("Order %s is already '%s', so it can't be cancelled.%s", oid, o.Status, extra))
				return nil
			}
			res.OK = true
			if !commit {
				res.NeedsConfirm = true
				res.Message = fmt.Sprintf("cancel order %s and refund $%.2f to your original payment method", oid, o.Total)
				return nil
			}
			o.Status = "cancelled"
			rf := model.Refund{ID: "RF-" + oid[1:], OrderID: oid, CustomerID: cid, Amount: o.Total,
				Reason: "cancelled before shipping", CreatedAt: time.Now().UTC()}
			tx.PutOrder(o)
			tx.PutRefund(rf)
			res.Refund, res.Committed = &rf, true
			res.Message = fmt.Sprintf("Order %s is cancelled and $%.2f is being refunded (refund %s, 5-7 business days).", oid, o.Total, rf.ID)
		case "return_order":
			if o.Status != "delivered" {
				deny(fmt.Sprintf("Order %s is '%s', so a return can't be started.", oid, strings.ReplaceAll(o.Status, "_", " ")))
				return nil
			}
			if age > t.ReturnWindow {
				deny(fmt.Sprintf("Order %s was delivered %d days ago, outside the %d-day return window.", oid, age, t.ReturnWindow))
				return nil
			}
			res.OK = true
			if !commit {
				res.NeedsConfirm = true
				res.Message = fmt.Sprintf("start a return for order %s and email you a prepaid label", oid)
				return nil
			}
			o.Status, o.RMA = "return_requested", fmt.Sprintf("RMA-%s-%s", oid[1:], time.Now().Format("0102"))
			tx.PutOrder(o)
			res.Committed = true
			res.Message = fmt.Sprintf("Return started for %s. Your RMA is %s and a prepaid label is on its way.", oid, o.RMA)
		case "request_refund":
			if r, done := tx.Refund(oid); done { // idempotent: a second request returns the same refund
				res.OK, res.Refund = true, &r
				res.Message = fmt.Sprintf("Order %s was already refunded: $%.2f (refund %s).", oid, r.Amount, r.ID)
				return nil
			}
			if a, waiting := tx.PendingApprovalFor(oid); waiting {
				res.OK, res.Escalated, res.Approval = true, true, &a
				res.Message = fmt.Sprintf("Your refund for %s is already with a teammate for approval (ticket %s). You'll get an email once it's decided.", oid, a.TicketID)
				return nil
			}
			switch {
			case o.Status == "return_requested":
			case o.Status == "delivered" && age <= t.ReturnWindow:
			case o.Status == "delivered":
				deny(fmt.Sprintf("Order %s was delivered %d days ago, outside the %d-day refund window. Your item may still be covered by the 1-year warranty.", oid, age, t.ReturnWindow))
				return nil
			case o.Status == "placed":
				deny(fmt.Sprintf("Order %s hasn't shipped yet — cancelling it refunds you automatically. Want me to cancel it?", oid))
				return nil
			case o.Status == "shipped":
				deny(fmt.Sprintf("Order %s is still in transit. Once it's delivered you can request a refund.", oid))
				return nil
			default:
				deny(fmt.Sprintf("Order %s is '%s', so it can't be refunded.", oid, strings.ReplaceAll(o.Status, "_", " ")))
				return nil
			}
			// Guardrails: large refunds and unusual refund velocity need a human.
			why := ""
			if t.AutoRefundLimit > 0 && o.Total > t.AutoRefundLimit {
				why = fmt.Sprintf("refunds over $%.0f need a human approval", t.AutoRefundLimit)
				res.Guardrail = "amount_over_limit"
			} else if t.MaxAutoRefunds > 0 && tx.RefundsSince(cid, time.Now().Add(-24*time.Hour)) >= t.MaxAutoRefunds {
				why = fmt.Sprintf("more than %d refunds in 24 hours need a human review", t.MaxAutoRefunds)
				res.Guardrail = "velocity_limit"
			}
			res.OK = true
			if !commit {
				res.NeedsConfirm = true
				if why != "" {
					res.Message = fmt.Sprintf("send your $%.2f refund request for order %s to a teammate for approval (%s)", o.Total, oid, why)
				} else {
					res.Message = fmt.Sprintf("refund $%.2f for order %s to your original payment method", o.Total, oid)
				}
				return nil
			}
			if why != "" {
				now := time.Now().UTC()
				tk := model.Ticket{ID: fmt.Sprintf("T%s%d", oid[1:], now.Unix()%10000), Kind: "approval", Status: "waiting", CustomerID: cid, SessionID: sid,
					Summary: fmt.Sprintf("Approve refund $%.2f for %s (%s). Customer reason: %s", o.Total, oid, why, reason), CreatedAt: now}
				a := model.Approval{ID: "AP-" + oid[1:] + fmt.Sprint(now.Unix()%10000), OrderID: oid, CustomerID: cid, SessionID: sid, Amount: o.Total,
					Reason: reason, Why: why, PrevStatus: o.Status, Status: "pending", TicketID: tk.ID, CreatedAt: now}
				o.Status = "refund_pending_approval"
				tx.PutOrder(o)
				tx.PutTicket(tk)
				tx.PutApproval(a)
				res.Escalated, res.Approval = true, &a
				res.Message = fmt.Sprintf("I've sent your $%.2f refund for %s to a teammate for approval because %s (ticket %s). Most are decided within a few hours, and you'll get an email.", o.Total, oid, why, tk.ID)
				return nil
			}
			if o.RMA == "" {
				o.RMA = fmt.Sprintf("RMA-%s-%s", oid[1:], time.Now().Format("0102"))
			}
			o.Status = "refunded"
			rf := model.Refund{ID: "RF-" + oid[1:], OrderID: oid, CustomerID: cid, Amount: o.Total,
				Reason: reason, CreatedAt: time.Now().UTC()}
			tx.PutOrder(o)
			tx.PutRefund(rf)
			res.Refund, res.Committed = &rf, true
			res.Message = fmt.Sprintf("Done — $%.2f refunded for order %s (refund %s, 5-7 business days). Please ship the item back with RMA %s.", o.Total, oid, rf.ID, o.RMA)
		}
		if res.Committed || res.Escalated { // show the order as it is after this change
			o2, _ := tx.Order(oid)
			res.Order = t.view(o2)
		}
		return nil
	})
	if err != nil {
		return ToolResult{Message: "Sorry, I couldn't save that change. Nothing was charged or refunded. Please try again."}
	}
	return res
}

// chooseOrder lists the orders an action applies to so the customer can pick one.
func (t *Tools) chooseOrder(action, cid string) ToolResult {
	if strings.HasPrefix(cid, "G-") {
		return ToolResult{Message: "Please sign in with your MyFlipKart.com account to view or change orders.", Guardrail: "guest_blocked"}
	}
	verb := map[string]string{"cancel_order": "cancel", "return_order": "return", "request_refund": "refund"}[action]
	res := ToolResult{OK: true}
	mine := t.S.OrdersFor(cid)
	for i := len(mine) - 1; i >= 0; i-- {
		o := mine[i]
		age := 0
		if o.DeliveredOn != "" {
			d, _ := time.Parse("2006-01-02", o.DeliveredOn)
			age = int(time.Since(d).Hours() / 24)
		}
		ok := false
		switch action {
		case "cancel_order":
			ok = o.Status == "placed"
		case "return_order":
			ok = o.Status == "delivered" && age <= t.ReturnWindow
		case "request_refund":
			ok = (o.Status == "delivered" && age <= t.ReturnWindow) || o.Status == "return_requested"
		}
		if !ok {
			continue
		}
		v := t.view(o)
		res.Orders = append(res.Orders, v)
		name := o.ID
		if len(v.ItemCards) > 0 {
			name = v.ItemCards[0].Name
		}
		res.Choices = append(res.Choices, Choice{Label: fmt.Sprintf("%s · %s · $%.0f", o.ID, name, o.Total), Say: verb + " " + o.ID})
	}
	if len(res.Choices) == 0 {
		res.OK = false
		res.Message = fmt.Sprintf("I don't see any orders that can be %s right now. Say \"my orders\" to see them all.",
			map[string]string{"cancel": "cancelled (only orders that haven't shipped)", "return": "returned (delivered in the last 30 days)", "refund": "refunded"}[verb])
		return res
	}
	res.Message = fmt.Sprintf("Which order would you like to %s?", verb)
	return res
}

// DecideApproval is the human agent's action on a held refund (admin page).
func (t *Tools) DecideApproval(id string, approve bool, by string) (model.Approval, error) {
	var out model.Approval
	err := t.S.Tx(func(tx *store.Txn) error {
		a, ok := tx.Approval(id)
		if !ok {
			return fmt.Errorf("no approval %s", id)
		}
		if a.Status != "pending" {
			return fmt.Errorf("approval %s is already %s", id, a.Status)
		}
		o, _ := tx.Order(a.OrderID)
		a.DecidedBy, a.DecidedAt = by, time.Now().UTC()
		if approve {
			a.Status = "approved"
			if o.RMA == "" {
				o.RMA = fmt.Sprintf("RMA-%s-%s", o.ID[1:], time.Now().Format("0102"))
			}
			o.Status = "refunded"
			tx.PutRefund(model.Refund{ID: "RF-" + o.ID[1:], OrderID: o.ID, CustomerID: o.CustomerID, Amount: a.Amount,
				Reason: a.Reason + " (approved by " + by + ")", CreatedAt: time.Now().UTC()})
		} else {
			a.Status, o.Status = "rejected", a.PrevStatus
		}
		tx.PutOrder(o)
		tx.PutApproval(a)
		if tk, ok := t.S.TicketLocked(a.TicketID); ok { // close the back-office ticket
			tk.Status, tk.AgentName, tk.ClosedAt = "closed", by, time.Now().UTC()
			tx.PutTicket(tk)
		}
		out = a
		return nil
	})
	return out, err
}

// ---------------- price parsing ----------------

var (
	numRe     = `\$?\s*(\d+(?:\.\d+)?)`
	betweenRe = regexp.MustCompile(`between\s+` + numRe + `\s*(?:and|-|to)\s*` + numRe)
	rangeRe   = regexp.MustCompile(numRe + `\s*(?:-|to)\s*` + numRe)
	maxRe     = regexp.MustCompile(`(?:under|below|less than|cheaper than|up to|max(?:imum)?|<)\s*` + numRe)
	minRe     = regexp.MustCompile(`(?:over|above|more than|at least|min(?:imum)?|>)\s*` + numRe)
	stripRe   = regexp.MustCompile(`(?i)(between|under|below|less than|cheaper than|up to|over|above|more than|at least)\s*\$?\s*\d+(\.\d+)?(\s*(and|-|to)\s*\$?\s*\d+(\.\d+)?)?`)
	currRe    = regexp.MustCompile(`(?i)\b(dollars?|usd|bucks)\b`)
)

func ParsePrice(text string) (lo, hi *float64) {
	t := strings.ReplaceAll(strings.ToLower(text), ",", "")
	f := func(s string) *float64 { v, _ := strconv.ParseFloat(s, 64); return &v }
	if m := betweenRe.FindStringSubmatch(t); m != nil {
		a, b := f(m[1]), f(m[2])
		if *a > *b {
			a, b = b, a
		}
		return a, b
	}
	if m := rangeRe.FindStringSubmatch(t); m != nil {
		a, b := f(m[1]), f(m[2])
		if *a > *b {
			a, b = b, a
		}
		return a, b
	}
	if m := maxRe.FindStringSubmatch(t); m != nil {
		return nil, f(m[1])
	}
	if m := minRe.FindStringSubmatch(t); m != nil {
		return f(m[1]), nil
	}
	return nil, nil
}

func StripPrice(q string) string {
	s := strings.TrimSpace(currRe.ReplaceAllString(stripRe.ReplaceAllString(q, " "), " "))
	if s == "" {
		return q
	}
	return s
}

func num(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if x == 0 || math.IsNaN(x) {
			return nil
		}
		return &x
	case string:
		if f, err := strconv.ParseFloat(strings.TrimPrefix(x, "$"), 64); err == nil && f != 0 {
			return &f
		}
	}
	return nil
}

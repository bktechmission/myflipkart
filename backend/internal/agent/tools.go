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
	"asksauby/internal/seed"
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
	fn("list_orders", "List the customer's orders with a status summary. status: all (default), open (anything not finished), not_delivered (placed or shipped), delivered, returns.",
		props{"status": {"string", "all | open | not_delivered | delivered | returns"}}),
	fn("place_order", "Place a new order for a product (customer confirms first). Omit product_id if the customer said 'this' without an ID.",
		props{"product_id": {"string", "Product ID like P1020"}, "quantity": {"number", "1-3, default 1"}}),
	fn("replace_order", "Send a free replacement for a delivered order (within 30 days, item in stock) instead of a refund. Omit order_id if not given.",
		props{"order_id": {"string", "Order ID if the customer gave one"}, "reason": {"string", "What was wrong"}}),
	fn("file_complaint", "Log a complaint about a seller, a delivery (item never arrived) or a faulty product, and bring in a human teammate.",
		props{"order_id": {"string", "Order ID if known"}, "about": {"string", "seller | delivery | product | other"}, "summary": {"string", "The customer's complaint in one line"}}, "summary"),
	fn("cancel_order", "Cancel one of the customer's orders before it ships (refund is automatic). Omit order_id if not given.", props{"order_id": {"string", "Order ID if the customer gave one"}}),
	fn("return_order", "Start a return (RMA + label) for a delivered order within the return window. Omit order_id if not given.",
		props{"order_id": {"string", "Order ID if the customer gave one"}, "reason": {"string", "Why the customer is returning it"}}),
	fn("request_refund", "Refund an order. Money is released only after the returned item reaches us (or a teammate confirms it never arrived). Omit order_id if not given.",
		props{"order_id": {"string", "Order ID if the customer gave one"}, "reason": {"string", "Why the customer wants a refund"}}),
	fn("policy_search", "Answer general questions about MyFlipKart.com policies: returns, refunds, cancellation, shipping, warranty. Use when no order ID is given.",
		props{"query": {"string", ""}}, "query"),
	fn("handoff", "Transfer the customer to a human support agent when they ask for a person or are frustrated.",
		props{"summary": {"string", "One-line summary of the issue for the human agent"}}, "summary"),
}

var ToolNames = []string{"search_products", "price_search", "get_product", "list_orders", "get_order", "cancel_order",
	"return_order", "replace_order", "request_refund", "place_order", "file_complaint", "policy_search", "handoff", "chitchat"}

var ConfirmTools = map[string]bool{"cancel_order": true, "return_order": true, "request_refund": true, "place_order": true, "replace_order": true}

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
	Seller   string      `json:"seller"`
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
	Choices      []Choice              `json:"choices,omitempty"` // tappable answers to a question Sauby asked
	Suggest      []Choice              `json:"suggest,omitempty"` // "what next?" chips under an answer
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

	// Ordering guardrails (zero values get safe defaults, see limits()).
	MaxQty             int     // per item per order
	OrderReviewOver    float64 // orders above this are held for a teammate
	MaxOrdersPerDay    int     // more than this in 24h: held for a teammate
	ReplaceReviewOver  float64 // replacements above this value are held for a teammate
	MaxReturnsPerMonth int     // more returns than this in 30 days go to a teammate, whatever the amount
}

func (t *Tools) limits() (maxQty int, orderOver float64, perDay int, replaceOver float64) {
	maxQty, orderOver, perDay, replaceOver = t.MaxQty, t.OrderReviewOver, t.MaxOrdersPerDay, t.ReplaceReviewOver
	if maxQty <= 0 {
		maxQty = 3
	}
	if orderOver <= 0 {
		orderOver = 1000
	}
	if perDay <= 0 {
		perDay = 3
	}
	if replaceOver <= 0 {
		replaceOver = 500
	}
	return
}

func (t *Tools) maxReturns() int {
	if t.MaxReturnsPerMonth <= 0 {
		return 3
	}
	return t.MaxReturnsPerMonth
}

// returnGuard: a return worth more than the auto limit, the customer's 4th+ return
// in 30 days, or an account on watch always goes to a person before a label is issued.
// why is for the teammate; say is what the customer is told.
func (t *Tools) returnGuard(tx *store.Txn, o model.Order) (why, guardrail, say string) {
	st := t.standingTx(tx, o.CustomerID)
	if st.Tier == "watch" || st.Tier == "blocked" {
		return "account standing: " + st.Tier + " (" + strings.Join(st.Reasons, "; ") + ")", "account_review",
			"Returns on your account are reviewed by a teammate."
	}
	if t.AutoRefundLimit > 0 && o.Total > t.AutoRefundLimit {
		return fmt.Sprintf("returns over $%.0f are reviewed by a teammate", t.AutoRefundLimit), "return_over_limit",
			fmt.Sprintf("This item is $%.2f, which is above our $%.0f limit for automatic returns.", o.Total, t.AutoRefundLimit)
	}
	n := 0
	for _, x := range tx.OrdersFor(o.CustomerID) {
		if x.ID != o.ID && !x.ReturnStartedAt.IsZero() && time.Since(x.ReturnStartedAt) < 30*24*time.Hour {
			n++
		}
	}
	if n >= t.maxReturns() {
		return fmt.Sprintf("more than %d returns in 30 days (%d already)", t.maxReturns(), n), "return_velocity",
			fmt.Sprintf("You've already started %d returns in the last 30 days, which is the most we can process automatically in a month.", n)
	}
	return "", "", ""
}

// toHuman holds the request for a teammate AND brings one into this chat right away.
func (t *Tools) toHuman(tx *store.Txn, o model.Order, kind string, why, guardrail, say, reason, sid, prev string, noReturn bool, res *ToolResult) {
	t.hold(tx, o, kind, o.Total, why, reason, sid, prev, noReturn)
	lt, open := tx.OpenLiveTicket(sid)
	if !open {
		lt = model.Ticket{ID: fmt.Sprintf("T%d", time.Now().UnixNano()%1e8), Kind: "live", Topic: kind + "_review", CustomerID: o.CustomerID, SessionID: sid,
			Summary: fmt.Sprintf("Review %s for %s ($%.2f): %s. Customer said: %s", kind, o.ID, o.Total, why, reason), Status: "waiting", CreatedAt: time.Now().UTC()}
		tx.PutTicket(lt)
	}
	st := t.standingTx(tx, o.CustomerID)
	ask := "They'll ask a couple of quick questions about the item's condition"
	if noReturn {
		ask = "They'll check the delivery with the courier"
	} else if o.Total > st.PhotoOver {
		ask += " and ask for a photo of it"
	}
	res.OK, res.Escalated, res.Guardrail, res.Ticket = true, true, guardrail, &lt
	res.Message = fmt.Sprintf("%s I'm connecting you with a teammate right now, here in this chat. %s before approving.", say, ask)
}

// startReturn issues the RMA, or hands the return to a teammate when a guardrail fires.
// The refund always waits for the item (RefundOnReceipt).
func (t *Tools) startReturn(tx *store.Txn, o model.Order, reason, sid string, res *ToolResult) {
	o.RefundOnReceipt, o.ReturnStartedAt = true, time.Now().UTC()
	if why, g, say := t.returnGuard(tx, o); why != "" {
		o.Status = "return_pending_approval"
		tx.PutOrder(o)
		t.toHuman(tx, o, "return", why, g, say, reason, sid, "delivered", false, res)
		return
	}
	o.Status, o.RMA = "return_requested", rma(o.ID)
	tx.PutOrder(o)
	res.Committed = true
	res.Message = fmt.Sprintf("Return started for %s. Your RMA is %s and a prepaid label is on its way. Your $%.2f refund is released automatically once the item reaches our warehouse.", o.ID, o.RMA, o.Total)
}

// Phrases that mean "the item never reached me": no item to return, so a human checks with the courier.
var notReceivedRe = regexp.MustCompile(`(?i)\b(never (arrived|came|got here|received|got it|delivered)|not (been )?(received|arrived)|didn'?t (receive|arrive|get (it|my|the))|hasn'?t arrived|missing|stolen|empty box|lost (in|package|parcel)?)\b`)

func (t *Tools) card(id string, h *search.Hit) Card {
	p, _ := t.R.Product(id)
	return Card{p.ID, p.Name, p.Category, p.Price, p.Rating, p.Stock, seed.SellerFor(p.Category), p.Features, h}
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
	case "get_order", "cancel_order", "return_order", "request_refund", "replace_order":
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
		return t.listOrders(cid, str("status"))
	case "place_order":
		if strings.HasPrefix(cid, "G-") {
			return ToolResult{Message: "Please sign in with your MyFlipKart.com account to place an order.", Guardrail: "guest_blocked"}
		}
		q := 1
		if n := num(args["quantity"]); n != nil {
			q = int(*n)
		}
		return t.placeOrder(strings.ToUpper(str("product_id")), q, cid, sid, commit)
	case "file_complaint":
		if strings.HasPrefix(cid, "G-") {
			return ToolResult{Message: "Please sign in so a teammate can see your account and orders.", Guardrail: "guest_blocked"}
		}
		return t.complaint(strings.ToUpper(str("order_id")), str("about"), str("summary"), cid, sid)
	case "handoff":
		if strings.HasPrefix(cid, "G-") {
			return ToolResult{Message: "Please sign in so a teammate can see your account and orders.", Guardrail: "guest_blocked"}
		}
		if open, ok := t.S.OpenLiveTicket(sid); ok { // idempotent: one live ticket per conversation
			return ToolResult{OK: true, Ticket: &open, Message: "A teammate is already on the way (ticket " + open.ID + ")."}
		}
		tk, err := t.openLive(cid, sid, str("summary"), "")
		if err != nil {
			return ToolResult{Message: "Sorry, I couldn't reach the support queue just now. Please try again in a moment."}
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
	res := ToolResult{Retrieval: &info}
	bad := t.suspendedSellers() // keep suspended sellers out of results
	for i := range hits {
		if c := t.card(hits[i].ID, &hits[i]); !bad[c.Seller] {
			res.Products = append(res.Products, c)
		}
	}
	res.OK = len(res.Products) > 0
	return res
}

// orderAction enforces authorization and business rules. Everything runs inside
// a store transaction, so eligibility is re-checked under the lock at commit time
// (no double refunds, no cancel after the warehouse marked it shipped).
//
// Money rule: we never refund an item that wasn't returned. A refund is released
// when the returned item reaches the warehouse (return_received), or after a
// teammate confirms an item never arrived.
func (t *Tools) orderAction(kind, oid, reason, cid, sid string, commit bool) ToolResult {
	var res ToolResult
	var stock map[string]int // product stock to mirror into the search index after commit
	err := t.S.Tx(func(tx *store.Txn) error {
		o, ok := tx.Order(oid)
		if !ok || o.CustomerID != cid { // same answer either way: never reveal others' orders exist
			res = ToolResult{Message: fmt.Sprintf("I couldn't find order %s on your account.", oid)}
			return nil
		}
		res.Order = t.view(o)
		age := ageDays(o)
		status := strings.ReplaceAll(o.Status, "_", " ")
		deny := func(msg string) { res.OK, res.Message = false, msg }
		if o.Status == "on_hold" && kind != "get_order" && kind != "cancel_order" {
			deny(fmt.Sprintf("Order %s is waiting for a quick review by a teammate, so I can't change it yet. You can still cancel it.", oid))
			return nil
		}
		if a, waiting := tx.PendingApprovalFor(oid); waiting && (kind == "return_order" || kind == "replace_order") {
			res.OK, res.Escalated, res.Approval = true, true, &a
			res.Message = fmt.Sprintf("Order %s is already with a teammate (ticket %s), so I'll leave it with them. You'll get an email once it's decided.", oid, a.TicketID)
			return nil
		}
		switch kind {
		case "get_order":
			res.OK = true
			return nil
		case "cancel_order":
			if o.Status != "placed" && o.Status != "on_hold" {
				extra := ""
				switch o.Status {
				case "shipped":
					extra = " It's on the way — once it's delivered I can start a return or send a replacement."
				case "delivered":
					extra = " It's already delivered — I can start a return or send a replacement instead."
				}
				deny(fmt.Sprintf("Order %s is already %s, so it can't be cancelled.%s", oid, status, extra))
				if o.Status == "delivered" {
					res.Choices = []Choice{{"Return it", "return " + oid}, {"Replace it", "replace " + oid}}
				}
				return nil
			}
			res.OK = true
			paid := o.Total > 0 && o.Status == "placed" // held orders were never charged
			if !commit {
				res.NeedsConfirm = true
				switch {
				case o.ReplacementFor != "":
					res.Message = fmt.Sprintf("cancel replacement order %s (no charge; your original order %s stays as it is)", oid, o.ReplacementFor)
				case paid:
					res.Message = fmt.Sprintf("cancel order %s and refund $%.2f to your original payment method", oid, o.Total)
				default:
					res.Message = fmt.Sprintf("cancel order %s (you haven't been charged)", oid)
				}
				return nil
			}
			o.Status = "cancelled"
			stock = t.restock(tx, o)
			if o.ReplacementFor != "" { // the original goes back to delivered: the customer can return it instead
				if orig, ok := tx.Order(o.ReplacementFor); ok && orig.ReplacedBy == oid {
					orig.Status, orig.ReplacedBy, orig.RMA = "delivered", "", ""
					tx.PutOrder(orig)
				}
			}
			if a, waiting := tx.PendingApprovalFor(oid); waiting { // a held order: close its review
				a.Status, a.DecidedBy, a.DecidedAt = "withdrawn", "customer", time.Now().UTC()
				tx.PutApproval(a)
			}
			tx.PutOrder(o)
			res.Committed = true
			if paid {
				rf := model.Refund{ID: "RF-" + oid[1:], OrderID: oid, CustomerID: cid, Amount: o.Total,
					Reason: "cancelled before shipping", CreatedAt: time.Now().UTC()}
				tx.PutRefund(rf)
				res.Refund = &rf
				res.Message = fmt.Sprintf("Order %s is cancelled and $%.2f is being refunded (refund %s, 5-7 business days).", oid, o.Total, rf.ID)
			} else {
				res.Message = fmt.Sprintf("Order %s is cancelled. You weren't charged.", oid)
			}
		case "return_order":
			if msg := t.notReturnable(o, age, "return"); msg != "" {
				deny(msg)
				return nil
			}
			res.OK = true
			if why, _, _ := t.returnGuard(tx, o); why != "" { // nothing for the customer to confirm: a person takes it from here
				t.startReturn(tx, o, reason, sid, &res)
				break
			}
			if !commit {
				res.NeedsConfirm = true
				res.Message = fmt.Sprintf("start a return for order %s and email you a prepaid label. Your $%.2f refund is released as soon as the item reaches us", oid, o.Total)
				if t.inStock(tx, o) && o.ReplacementFor == "" {
					res.Choices = []Choice{{"Send a free replacement instead", "replace " + oid}}
				}
				return nil
			}
			t.startReturn(tx, o, reason, sid, &res)
		case "replace_order":
			if o.Status == "replacement_requested" && o.ReplacedBy != "" { // idempotent
				res.OK = true
				res.Message = fmt.Sprintf("A replacement for %s is already on its way (order %s).", oid, o.ReplacedBy)
				return nil
			}
			if o.ReplacementFor != "" { // a replacement of a replacement: a person should look
				deny(fmt.Sprintf("Order %s is already a replacement, so I can't send another one automatically. A teammate can sort this out for you.", oid))
				res.Choices, res.Guardrail = []Choice{{"Talk to a person", "talk to a person"}, {"Return it instead", "return " + oid}}, "second_replacement"
				return nil
			}
			if msg := t.notReturnable(o, age, "replace"); msg != "" {
				deny(msg)
				return nil
			}
			if st := t.standingTx(tx, cid); st.Tier == "watch" || st.Tier == "blocked" {
				deny("Replacements on your account are reviewed by a teammate. I can connect you with one now.")
				res.Choices, res.Guardrail = []Choice{{"Talk to a person", "talk to a person"}}, "account_review"
				return nil
			}
			if !t.inStock(tx, o) {
				deny(fmt.Sprintf("Sorry, the item in %s is out of stock, so I can't send a replacement right now. I can start a return for a refund instead.", oid))
				res.Choices = []Choice{{"Return for a refund", "return " + oid}}
				return nil
			}
			_, _, _, over := t.limits()
			value := 0.0
			for _, it := range o.Items {
				if p, ok := tx.Product(it); ok {
					value += p.Price
				}
			}
			review := value > over
			res.OK = true
			if !commit {
				res.NeedsConfirm = true
				res.Message = fmt.Sprintf("send a free replacement for order %s and email you a prepaid label to send the original back. No charge", oid)
				if review {
					res.Message += fmt.Sprintf(". Replacements over $%.0f get a quick teammate review before they ship", over)
				}
				return nil
			}
			nid, err := tx.NextOrderID()
			if err != nil {
				return err
			}
			n := model.Order{ID: nid, CustomerID: cid, Status: "placed", Items: o.Items, PlacedOn: today(),
				ReplacementFor: oid, Payment: "no charge (replacement)"}
			if review {
				n.Status = "on_hold"
			}
			stock = t.takeStock(tx, n.Items)
			o.Status, o.ReplacedBy, o.RMA = "replacement_requested", nid, rma(oid)
			tx.PutOrder(n)
			tx.PutOrder(o)
			if review {
				t.hold(tx, n, "replacement", value, fmt.Sprintf("replacements over $%.0f need a teammate review", over), reason, sid, "delivered", false)
				res.Escalated, res.Guardrail = true, "replacement_review"
				res.Message = fmt.Sprintf("Replacement order %s is created and waiting for a quick teammate review (replacements over $%.0f). Send the original back with RMA %s; the label is in your email.", nid, over, o.RMA)
			} else {
				res.Committed = true
				res.Message = fmt.Sprintf("A free replacement is on its way: order %s ships in 1-2 business days. Please send the original back with RMA %s (the prepaid label is in your email).", nid, o.RMA)
			}
			res.Order = t.view(n)
			return nil
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
			switch o.Status {
			case "return_received": // the item is back: money can move now
				why, g := t.refundGuard(tx, o)
				res.OK = true
				if why != "" { // a person reviews it with the customer, now
					o.Status = "refund_pending_approval"
					tx.PutOrder(o)
					say := fmt.Sprintf("We've received the item. This refund is $%.2f, which is above our $%.0f limit for automatic refunds.", o.Total, t.AutoRefundLimit)
					if g == "velocity_limit" {
						say = "We've received the item. You've had several refunds in the last day, so a teammate reviews this one."
					}
					t.toHuman(tx, o, "refund", why, g, say, reason, sid, "return_received", false, &res)
					break
				}
				if !commit {
					res.NeedsConfirm = true
					res.Message = fmt.Sprintf("refund $%.2f for order %s to your original payment method (we've received the item)", o.Total, oid)
					return nil
				}
				{
					rf := issueRefund(tx, o, reason)
					res.Refund, res.Committed = &rf, true
					res.Message = fmt.Sprintf("Done — $%.2f refunded for order %s (refund %s, 5-7 business days).", o.Total, oid, rf.ID)
				}
			case "return_requested":
				if o.RefundOnReceipt {
					res.OK = true
					res.Message = fmt.Sprintf("Your $%.2f refund for %s is set up: it's released as soon as the returned item reaches us (RMA %s). We haven't received it yet.", o.Total, oid, o.RMA)
					return nil
				}
				res.OK = true
				if !commit {
					res.NeedsConfirm = true
					res.Message = fmt.Sprintf("release your $%.2f refund for %s as soon as the returned item reaches us (RMA %s)", o.Total, oid, o.RMA)
					return nil
				}
				o.RefundOnReceipt = true
				tx.PutOrder(o)
				res.Committed = true
				res.Message = fmt.Sprintf("All set: your $%.2f refund for %s is released as soon as the returned item reaches us.", o.Total, oid)
			case "delivered":
				if notReceivedRe.MatchString(reason) { // nothing to return: a person checks with the courier
					t.toHuman(tx, o, "refund", "item reported not received: check with the courier", "not_received_claim",
						fmt.Sprintf("I'm sorry order %s hasn't reached you. I've opened a missing-item claim.", oid), reason, sid, "delivered", true, &res)
					break
				}
				if age > t.ReturnWindow {
					deny(fmt.Sprintf("Order %s was delivered %d days ago, outside the %d-day refund window. Your item may still be covered by the 1-year warranty.", oid, age, t.ReturnWindow))
					res.Choices = []Choice{{"Talk to a person", "talk to a person"}}
					return nil
				}
				// Delivered and in the window: the refund needs the item back first.
				res.OK = true
				if why, _, _ := t.returnGuard(tx, o); why != "" {
					t.startReturn(tx, o, reason, sid, &res)
					break
				}
				if !commit {
					res.NeedsConfirm = true
					res.Message = fmt.Sprintf("start a return for order %s (prepaid label by email). Refunds need the item back: your $%.2f is released once it reaches us", oid, o.Total)
					if t.inStock(tx, o) && o.ReplacementFor == "" {
						res.Choices = []Choice{{"Send a free replacement instead", "replace " + oid}}
					}
					return nil
				}
				t.startReturn(tx, o, reason, sid, &res)
			case "placed":
				deny(fmt.Sprintf("Order %s hasn't shipped yet — cancelling it refunds you automatically. Want me to cancel it?", oid))
				res.Choices = []Choice{{"Cancel it", "cancel " + oid}}
				return nil
			case "shipped":
				if notReceivedRe.MatchString(reason) {
					deny(fmt.Sprintf("Order %s is still in transit with the courier, so it hasn't been marked delivered yet. If it doesn't arrive in 3 business days, a teammate can open a claim.", oid))
					res.Choices = []Choice{{"Talk to a person", "talk to a person"}}
					return nil
				}
				deny(fmt.Sprintf("Order %s is still in transit. Once it's delivered you can return it for a refund.", oid))
				return nil
			case "replacement_requested":
				deny(fmt.Sprintf("You chose a free replacement for %s (order %s), so there's no refund on it. If the replacement doesn't work out, I can return that one for a refund.", oid, o.ReplacedBy))
				return nil
			case "cancelled":
				deny(fmt.Sprintf("Order %s was cancelled. Cancelled orders are refunded automatically when they were paid.", oid))
				return nil
			default:
				deny(fmt.Sprintf("Order %s is %s, so it can't be refunded.", oid, status))
				return nil
			}
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
	t.mirrorStock(stock)
	return res
}

// notReturnable explains why a delivered-item action (return / replace) can't happen, or "".
func (t *Tools) notReturnable(o model.Order, age int, verb string) string {
	switch {
	case o.Status == "delivered" && age <= t.ReturnWindow:
		return ""
	case o.Status == "delivered":
		return fmt.Sprintf("Order %s was delivered %d days ago, outside the %d-day window to %s it. It may still be covered by the 1-year warranty — a teammate can help.", o.ID, age, t.ReturnWindow, verb)
	case o.Status == "placed" || o.Status == "on_hold":
		return fmt.Sprintf("Order %s hasn't shipped yet, so there's nothing to %s. You can cancel it instead.", o.ID, verb)
	case o.Status == "shipped":
		return fmt.Sprintf("Order %s is still on its way. Once it's delivered, I can %s it if something's wrong.", o.ID, verb)
	case o.Status == "replacement_requested":
		return fmt.Sprintf("A replacement for %s is already on its way (order %s).", o.ID, o.ReplacedBy)
	case strings.HasPrefix(o.Status, "return") || strings.HasPrefix(o.Status, "refund"):
		return fmt.Sprintf("Order %s already has a return in progress (%s).", o.ID, strings.ReplaceAll(o.Status, "_", " "))
	}
	return fmt.Sprintf("Order %s is %s, so I can't %s it.", o.ID, strings.ReplaceAll(o.Status, "_", " "), verb)
}

// refundGuard: large refunds and unusual refund velocity need a human.
func (t *Tools) refundGuard(tx *store.Txn, o model.Order) (why, guardrail string) {
	if o.RefundApproved {
		return "", ""
	}
	if t.AutoRefundLimit > 0 && o.Total > t.AutoRefundLimit {
		return fmt.Sprintf("refunds over $%.0f need a human approval", t.AutoRefundLimit), "amount_over_limit"
	}
	if t.MaxAutoRefunds > 0 && tx.RefundsSince(o.CustomerID, time.Now().Add(-24*time.Hour)) >= t.MaxAutoRefunds {
		return fmt.Sprintf("more than %d refunds in 24 hours need a human review", t.MaxAutoRefunds), "velocity_limit"
	}
	return "", ""
}

// hold creates a back-office ticket + approval for a teammate and returns the ticket ID.
func (t *Tools) hold(tx *store.Txn, o model.Order, kind string, amount float64, why, reason, sid, prev string, noReturn bool) string {
	now := time.Now().UTC()
	what := map[string]string{"refund": "refund", "order": "new order", "replacement": "replacement"}[kind]
	tk := model.Ticket{ID: fmt.Sprintf("T%s%d", o.ID[1:], now.UnixNano()%10000), Kind: "approval", Status: "waiting", CustomerID: o.CustomerID, SessionID: sid,
		Summary: fmt.Sprintf("Approve %s $%.2f for %s (%s). Customer said: %s", what, amount, o.ID, why, reason), CreatedAt: now}
	a := model.Approval{ID: fmt.Sprintf("AP-%s%d", o.ID[1:], now.UnixNano()%10000), Kind: kind, NoReturn: noReturn, OrderID: o.ID, CustomerID: o.CustomerID,
		SessionID: sid, Amount: amount, Reason: reason, Why: why, PrevStatus: prev, Status: "pending", TicketID: tk.ID, CreatedAt: now}
	tx.PutTicket(tk)
	tx.PutApproval(a)
	return tk.ID
}

func issueRefund(tx *store.Txn, o model.Order, reason string) model.Refund {
	o.Status = "refunded"
	rf := model.Refund{ID: "RF-" + o.ID[1:], OrderID: o.ID, CustomerID: o.CustomerID, Amount: o.Total, Reason: reason, CreatedAt: time.Now().UTC()}
	tx.PutOrder(o)
	tx.PutRefund(rf)
	return rf
}

func (t *Tools) inStock(tx *store.Txn, o model.Order) bool {
	need := map[string]int{}
	for _, it := range o.Items {
		need[it]++
	}
	for id, n := range need {
		if p, ok := tx.Product(id); !ok || p.Stock < n {
			return false
		}
	}
	return true
}

func (t *Tools) takeStock(tx *store.Txn, items []string) map[string]int {
	return t.moveStock(tx, items, -1)
}
func (t *Tools) restock(tx *store.Txn, o model.Order) map[string]int {
	if o.ReplacementFor == "" && o.Payment == "" { // seed orders predate stock tracking
		return nil
	}
	return t.moveStock(tx, o.Items, +1)
}
func (t *Tools) moveStock(tx *store.Txn, items []string, sign int) map[string]int {
	out := map[string]int{}
	for _, it := range items {
		if p, ok := tx.Product(it); ok {
			p.Stock += sign
			if p.Stock < 0 {
				p.Stock = 0
			}
			tx.PutProduct(p)
			out[it] = p.Stock
		}
	}
	return out
}
func (t *Tools) mirrorStock(m map[string]int) {
	for id, n := range m {
		t.R.SetStock(id, n)
	}
}

func ageDays(o model.Order) int {
	if o.DeliveredOn == "" {
		return -1
	}
	d, _ := time.Parse("2006-01-02", o.DeliveredOn)
	return int(time.Since(d).Hours() / 24)
}
func rma(oid string) string { return fmt.Sprintf("RMA-%s-%s", oid[1:], time.Now().Format("0102")) }
func today() string         { return time.Now().Format("2006-01-02") }

// placeOrder: confirm item, quantity, total and payment; check stock under the lock;
// hold big or unusually frequent orders for a teammate (fraud guardrail).
func (t *Tools) placeOrder(pid string, qty int, cid, sid string, commit bool) ToolResult {
	if !ProductRe.MatchString(pid) {
		return ToolResult{Message: "Which product would you like to order? Tell me its ID (like P1020) or search for it first."}
	}
	maxQty, over, perDay, _ := t.limits()
	if qty < 1 || qty > maxQty {
		return ToolResult{Message: fmt.Sprintf("You can order 1 to %d of an item at a time.", maxQty), Guardrail: "quantity_limit"}
	}
	var res ToolResult
	var stock map[string]int
	var cat string
	bad := t.suspendedSellers()
	err := t.S.Tx(func(tx *store.Txn) error {
		p, ok := tx.Product(pid)
		if !ok {
			res = ToolResult{Message: "I couldn't find product " + pid + "."}
			return nil
		}
		cat = p.Category
		if seller := seed.SellerFor(p.Category); bad[seller] {
			res = ToolResult{Message: fmt.Sprintf("Sorry, we've paused sales from %s while we review quality complaints, so I can't order %s right now.", seller, p.Name),
				Guardrail: "seller_suspended"}
			return nil
		}
		c := t.card(pid, nil)
		c.Stock = p.Stock
		res.Products = []Card{c}
		if p.Stock < qty {
			if p.Stock == 0 {
				res.Message = fmt.Sprintf("Sorry, %s (%s) is out of stock right now.", p.Name, pid)
			} else {
				res.Message = fmt.Sprintf("Sorry, only %d of %s (%s) left in stock.", p.Stock, p.Name, pid)
			}
			res.Guardrail = "out_of_stock"
			return nil
		}
		sub := p.Price * float64(qty)
		ship := 0.0
		if sub < 50 {
			ship = 5.99
		}
		total := math.Round((sub+ship)*100) / 100
		why, g := "", ""
		recent, dup := 0, ""
		for _, o := range tx.OrdersFor(cid) {
			if o.Payment != "" && o.ReplacementFor == "" && o.PlacedOn == today() && o.Status != "cancelled" {
				recent++
				if len(o.Items) > 0 && o.Items[0] == pid {
					dup = o.ID
				}
			}
		}
		switch {
		case t.standingTx(tx, cid).Tier == "blocked":
			why, g = "new orders on this account get a quick teammate review", "account_review"
		case total > over:
			why, g = fmt.Sprintf("orders over $%.0f get a quick teammate review", over), "order_review_amount"
		case recent >= perDay:
			why, g = fmt.Sprintf("more than %d orders in a day get a quick teammate review", perDay), "order_velocity"
		}
		shipTxt := "free shipping"
		if ship > 0 {
			shipTxt = fmt.Sprintf("$%.2f shipping", ship)
		}
		res.OK = true
		if !commit {
			res.NeedsConfirm = true
			res.Message = fmt.Sprintf("order %d × %s (%s) from %s: $%.2f + %s = $%.2f, charged to your card on file ending 4242 (demo), arriving in 3-5 business days",
				qty, p.Name, pid, seed.SellerFor(p.Category), sub, shipTxt, total)
			if why != "" {
				res.Message += ". Because " + why + ", it ships after that review and you're only charged once it's approved"
			}
			if dup != "" {
				res.Message += fmt.Sprintf(". Heads-up: you already ordered this today (%s)", dup)
				if d, ok := tx.Order(dup); ok {
					res.Orders = []*OrderView{t.view(d)}
				}
			}
			return nil
		}
		id, err := tx.NextOrderID()
		if err != nil {
			return err
		}
		items := make([]string, qty)
		for i := range items {
			items[i] = pid
		}
		o := model.Order{ID: id, CustomerID: cid, Status: "placed", PlacedOn: today(), Items: items, Total: total,
			Shipping: ship, Payment: "card ending 4242 (demo)"}
		if why != "" {
			o.Status = "on_hold"
		}
		stock = t.takeStock(tx, items)
		tx.PutOrder(o)
		res.Order = t.view(o)
		res.Products = nil
		if why != "" {
			tk := t.hold(tx, o, "order", total, why, "new order", sid, "placed", false)
			res.Escalated, res.Guardrail = true, g
			res.Message = fmt.Sprintf("Order %s is created and waiting for a quick teammate review because %s (ticket %s). You'll only be charged $%.2f once it's approved.", id, why, tk, total)
		} else {
			res.Committed = true
			res.Message = fmt.Sprintf("Order %s is placed: %d × %s, $%.2f total. It ships from %s and arrives in 3-5 business days. You can cancel it any time before it ships.",
				id, qty, p.Name, total, seed.SellerFor(p.Category))
		}
		return nil
	})
	if err != nil {
		return ToolResult{Message: "Sorry, I couldn't place that order. You weren't charged. Please try again."}
	}
	t.mirrorStock(stock)
	if res.Guardrail == "out_of_stock" || res.Guardrail == "seller_suspended" { // suggest in-stock alternatives
		hits, _, _ := t.R.SearchProducts(search.ProductQuery{Query: cat})
		for _, h := range hits {
			if c := t.card(h.ID, nil); c.ID != pid && c.Stock > 0 && c.Category == cat && !bad[c.Seller] && len(res.Choices) < 3 {
				res.Products = append(res.Products, c)
				res.Choices = append(res.Choices, Choice{"Order " + c.Name, "order " + c.ID})
			}
		}
	}
	return res
}

// ChooseProduct asks which of the products just shown the customer means by "order this".
func (t *Tools) ChooseProduct(ids []string) ToolResult {
	res := ToolResult{OK: true, Message: "Which one would you like to order?"}
	for _, id := range ids {
		if len(res.Choices) == 4 {
			break
		}
		c := t.card(id, nil)
		res.Products = append(res.Products, c)
		res.Choices = append(res.Choices, Choice{fmt.Sprintf("%s · $%.0f", c.Name, c.Price), "order " + c.ID})
	}
	return res
}

// complaint logs a complaint and loops in a human. Product faults on a recent
// delivery get self-service options first (replacement is usually the happiest fix).
func (t *Tools) complaint(oid, about, summary, cid, sid string) ToolResult {
	var o model.Order
	var v *OrderView
	if OrderRe.MatchString(oid) {
		got, ok := t.S.Order(oid)
		if !ok || got.CustomerID != cid {
			return ToolResult{Message: fmt.Sprintf("I couldn't find order %s on your account.", oid)}
		}
		o, v = got, t.view(got)
	}
	about = strings.ToLower(about)
	if about == "" {
		about = "other"
	}
	if about == "product" && v != nil && o.Status == "delivered" && ageDays(o) <= t.ReturnWindow {
		return ToolResult{OK: true, Order: v, Message: fmt.Sprintf("I'm sorry %s isn't right. I can send a free replacement, start a return for a refund, or bring in a teammate.", o.ID),
			Choices: []Choice{{"Send a replacement", "replace " + o.ID}, {"Return for a refund", "return " + o.ID}, {"Talk to a person", "talk to a person"}}}
	}
	seller := ""
	if v != nil && len(v.ItemCards) > 0 {
		seller = v.ItemCards[0].Seller
	}
	head := map[string]string{"seller": "Seller complaint", "delivery": "Delivery issue", "product": "Product issue"}[about]
	if head == "" {
		head = "Complaint"
	}
	line := head
	if o.ID != "" {
		line += " · " + o.ID
	}
	if seller != "" {
		line += " · " + seller
	}
	tk, err := t.openLive(cid, sid, line+": "+summary, strings.ToLower(strings.ReplaceAll(head, " ", "_")))
	if err != nil {
		return ToolResult{Message: "Sorry, I couldn't reach the support queue just now. Please try again in a moment."}
	}
	about2 := "this"
	if seller != "" && about == "seller" {
		about2 = seller
	}
	msg := fmt.Sprintf("I'm sorry about that. I've logged it (ticket %s) and I'm connecting you with a teammate now; they'll see our conversation and follow up on %s.", tk.ID, about2)
	if about == "seller" {
		msg += " Sellers with repeated complaints are reviewed by our marketplace team."
	}
	return ToolResult{OK: true, Ticket: &tk, Order: v, Message: msg, Guardrail: "complaint_to_human"}
}

// openLive queues the conversation for a human (idempotent per conversation).
func (t *Tools) openLive(cid, sid, summary, topic string) (model.Ticket, error) {
	if open, ok := t.S.OpenLiveTicket(sid); ok {
		return open, nil
	}
	tk := model.Ticket{ID: fmt.Sprintf("T%d", time.Now().UnixNano()%1e8), Kind: "live", Topic: topic, CustomerID: cid, SessionID: sid,
		Summary: summary, Status: "waiting", CreatedAt: time.Now().UTC()}
	return tk, t.S.Tx(func(tx *store.Txn) error { tx.PutTicket(tk); return nil })
}

// listOrders answers "what did I order / what hasn't arrived / what's pending".
func (t *Tools) listOrders(cid, filter string) ToolResult {
	mine := t.S.OrdersFor(cid)
	if len(mine) == 0 {
		return ToolResult{OK: true, Message: "I don't see any orders on your account yet."}
	}
	group := func(s string) string {
		switch s {
		case "placed", "on_hold":
			return "not shipped yet"
		case "shipped":
			return "on the way"
		case "delivered":
			return "delivered"
		case "return_requested", "return_received", "refund_pending_approval", "replacement_requested", "return_pending_approval":
			return "return or replacement in progress"
		}
		return strings.ReplaceAll(s, "_", " ")
	}
	match := func(s string) bool {
		g := group(s)
		switch filter {
		case "open":
			return g == "not shipped yet" || g == "on the way" || g == "return or replacement in progress"
		case "not_delivered":
			return g == "not shipped yet" || g == "on the way"
		case "delivered":
			return g == "delivered"
		case "returns":
			return g == "return or replacement in progress" || s == "refunded"
		}
		return true
	}
	counts, order := map[string]int{}, []string{}
	res := ToolResult{OK: true}
	for i := len(mine) - 1; i >= 0; i-- {
		g := group(mine[i].Status)
		if counts[g] == 0 {
			order = append(order, g)
		}
		counts[g]++
		if match(mine[i].Status) && len(res.Orders) < 8 {
			res.Orders = append(res.Orders, t.view(mine[i]))
		}
	}
	var parts []string
	for _, g := range []string{"not shipped yet", "on the way", "delivered", "return or replacement in progress", "refunded", "cancelled"} {
		if counts[g] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[g], g))
		}
	}
	for _, g := range order { // any other status
		if !contains([]string{"not shipped yet", "on the way", "delivered", "return or replacement in progress", "refunded", "cancelled"}, g) {
			parts = append(parts, fmt.Sprintf("%d %s", counts[g], g))
		}
	}
	res.Message = fmt.Sprintf("You have %d orders: %s.", len(mine), strings.Join(parts, ", "))
	if len(res.Orders) == 0 {
		res.Message += map[string]string{"open": " Nothing is in progress right now.", "not_delivered": " Everything you ordered has been delivered.",
			"delivered": " None are delivered yet.", "returns": " No returns or refunds in progress."}[filter]
	}
	return res
}

// chooseOrder lists the orders an action applies to so the customer can pick one.
func (t *Tools) chooseOrder(action, cid string) ToolResult {
	if strings.HasPrefix(cid, "G-") {
		return ToolResult{Message: "Please sign in with your MyFlipKart.com account to view or change orders.", Guardrail: "guest_blocked"}
	}
	verb := map[string]string{"cancel_order": "cancel", "return_order": "return", "request_refund": "refund", "replace_order": "replace"}[action]
	res := ToolResult{OK: true}
	mine := t.S.OrdersFor(cid)
	for i := len(mine) - 1; i >= 0; i-- {
		o := mine[i]
		age := ageDays(o)
		ok := false
		switch action {
		case "cancel_order":
			ok = o.Status == "placed" || o.Status == "on_hold"
		case "return_order":
			ok = o.Status == "delivered" && age <= t.ReturnWindow
		case "replace_order":
			ok = o.Status == "delivered" && age <= t.ReturnWindow && o.ReplacementFor == ""
		case "request_refund":
			ok = (o.Status == "delivered" && age <= t.ReturnWindow) || o.Status == "return_received" ||
				(o.Status == "return_requested" && !o.RefundOnReceipt)
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
			map[string]string{"cancel": "cancelled (only orders that haven't shipped)", "return": "returned (delivered in the last 30 days)",
				"refund": "refunded (items have to be returned first)", "replace": "replaced (delivered in the last 30 days)"}[verb])
		return res
	}
	res.Message = fmt.Sprintf("Which order would you like to %s?", verb)
	return res
}

// ReceiveReturn is the warehouse scan of a returned item (admin console / courier
// webhook). If the customer asked for a refund, it is released here, under the
// same guardrails. Returns a customer-facing update.
func (t *Tools) ReceiveReturn(oid, by string) (string, error) {
	msg := ""
	err := t.S.Tx(func(tx *store.Txn) error {
		o, ok := tx.Order(oid)
		if !ok {
			return fmt.Errorf("no order %s", oid)
		}
		if o.Status != "return_requested" && o.Status != "replacement_requested" {
			return fmt.Errorf("order %s is %s, not awaiting a returned item", oid, o.Status)
		}
		if o.Status == "replacement_requested" { // original came back; nothing to pay out
			msg = fmt.Sprintf("We received the original item for %s. Thanks for sending it back!", oid)
			return nil
		}
		o.Status = "return_received"
		tx.PutOrder(o)
		msg = fmt.Sprintf("We received your returned item for %s.", oid)
		if !o.RefundOnReceipt {
			return nil
		}
		if _, waiting := tx.PendingApprovalFor(oid); waiting {
			msg += " Your refund is with a teammate for approval."
			return nil
		}
		if why, _ := t.refundGuard(tx, o); why != "" {
			o.Status = "refund_pending_approval"
			tx.PutOrder(o)
			t.hold(tx, o, "refund", o.Total, why, "return received", "", "return_received", false)
			msg += " Your refund is with a teammate for approval because " + why + "."
			return nil
		}
		rf := issueRefund(tx, o, "return received ("+by+")")
		msg += fmt.Sprintf(" $%.2f is refunded (refund %s, 5-7 business days).", rf.Amount, rf.ID)
		return nil
	})
	return msg, err
}

// DecideApproval is the human agent's action on held work (admin page / teammate).
// Decision is what the teammate records: what they found, and why.
type Decision struct {
	Approve   bool
	Condition string // unused | defective | used | not_received (required to approve a return or refund)
	Note      string
}

// DecideApproval applies a teammate's decision. Returns and refunds can't be approved
// without recording the item's condition, and above the customer's photo threshold
// not without a photo of the item (a person has to actually look at it).
func (t *Tools) DecideApproval(id string, d Decision, by string) (model.Approval, error) {
	approve := d.Approve
	var out model.Approval
	var stock map[string]int
	err := t.S.Tx(func(tx *store.Txn) error {
		a, ok := tx.Approval(id)
		if !ok {
			return fmt.Errorf("no approval %s", id)
		}
		if a.Status != "pending" {
			return fmt.Errorf("approval %s is already %s", id, a.Status)
		}
		o, _ := tx.Order(a.OrderID)
		if approve && (a.Kind == "" || a.Kind == "refund" || a.Kind == "return") {
			cond := strings.ToLower(strings.TrimSpace(d.Condition))
			if a.NoReturn && cond == "" {
				cond = "not_received"
			}
			switch cond {
			case "unused", "defective", "not_received":
			case "used":
				if strings.TrimSpace(d.Note) == "" {
					return fmt.Errorf("the item was used: add a note explaining why the return is approved anyway")
				}
			case "":
				return fmt.Errorf("record the item's condition (unused, defective or used) before approving")
			default:
				return fmt.Errorf("unknown condition %q", d.Condition)
			}
			if st := t.standingTx(tx, a.CustomerID); !a.NoReturn && a.Amount > st.PhotoOver && len(a.Evidence) == 0 {
				return fmt.Errorf("ask the customer for a photo of the item first ($%.0f is above the $%.0f photo threshold for this customer)", a.Amount, st.PhotoOver)
			}
			a.Condition = cond
		}
		a.Note = strings.TrimSpace(d.Note)
		a.DecidedBy, a.DecidedAt = by, time.Now().UTC()
		switch a.Kind {
		case "order", "replacement": // a held order: ship it, or cancel it and put the stock back
			if approve {
				a.Status, o.Status = "approved", "placed"
			} else {
				a.Status, o.Status = "rejected", "cancelled"
				stock = t.restock(tx, o)
				if orig, ok := tx.Order(o.ReplacementFor); ok && a.Kind == "replacement" {
					orig.Status, orig.ReplacedBy, orig.RMA = "delivered", "", ""
					tx.PutOrder(orig)
				}
			}
			tx.PutOrder(o)
		case "return": // approve: issue the RMA; the refund is pre-approved and released on receipt
			if approve {
				a.Status = "approved"
				o.Status, o.RMA, o.RefundApproved = "return_requested", rma(o.ID), true
			} else {
				a.Status = "rejected"
				o.Status, o.RefundOnReceipt, o.ReturnStartedAt = a.PrevStatus, false, time.Time{}
			}
			tx.PutOrder(o)
		default: // refund
			if approve {
				a.Status = "approved"
				if a.NoReturn || o.Status == "return_received" || o.Status == "refund_pending_approval" {
					issueRefund(tx, o, a.Reason+" (approved by "+by+")")
					a.Refunded = true
				} else { // item still on its way back: refund releases on receipt
					o.RefundApproved = true
					tx.PutOrder(o)
				}
			} else {
				a.Status = "rejected"
				if o.Status == "refund_pending_approval" {
					o.Status = a.PrevStatus
				}
				o.RefundOnReceipt = false
				tx.PutOrder(o)
			}
		}
		tx.PutApproval(a)
		if tk, ok := t.S.TicketLocked(a.TicketID); ok { // close the back-office ticket
			tk.Status, tk.AgentName, tk.ClosedAt = "closed", by, time.Now().UTC()
			tx.PutTicket(tk)
		}
		out = a
		return nil
	})
	t.mirrorStock(stock)
	return out, err
}

// ApprovalUpdate is the message the customer sees when a teammate decides.
func ApprovalUpdate(a model.Approval, by string) string {
	switch {
	case a.Status == "rejected":
		return fmt.Sprintf("Update from %s: we couldn't approve the request for %s. A teammate will email you the details.", by, a.OrderID)
	case a.Kind == "return":
		return fmt.Sprintf("Update from %s: your return for %s was approved. A prepaid label is in your email, and your $%.2f refund is released as soon as the item reaches us.", by, a.OrderID, a.Amount)
	case a.Kind == "order":
		return fmt.Sprintf("Update from %s: your order %s was approved and will ship soon. $%.2f is charged to your card on file.", by, a.OrderID, a.Amount)
	case a.Kind == "replacement":
		return fmt.Sprintf("Update from %s: your replacement order %s was approved and ships in 1-2 business days.", by, a.OrderID)
	case a.Refunded:
		return fmt.Sprintf("Update from %s: your $%.2f refund for %s was approved. It will reach your original payment method in 5-7 business days.", by, a.Amount, a.OrderID)
	}
	return fmt.Sprintf("Update from %s: your $%.2f refund for %s was approved. It's paid as soon as the returned item reaches us.", by, a.Amount, a.OrderID)
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

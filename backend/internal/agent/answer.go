// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Answer writing: the LLM phrases FAQ answers from retrieved passages; order and
// money outcomes always use deterministic templates so they never depend on generation.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"asksauby/internal/llm"
)

const answerPrompt = `You are Sauby from MyFlipKart.com. Answer the customer using ONLY the tool result.
Be warm and concise (2-5 sentences or a short list). Mention product IDs, order IDs and prices exactly as given.
Never invent products, orders, prices, refunds or policies. If the result says something failed, explain why
and what the customer can do next.`

// writeAnswer uses the LLM for FAQ-style answers; order/refund outcomes use
// deterministic templates so the money path never depends on generation.
func (a *Agent) writeAnswer(ctx context.Context, text string, r Route, res ToolResult) (string, string, int) {
	if r.Tool == "chitchat" || pathOf(r.Tool) == "transaction" || r.Tool == "handoff" || r.Tool == "file_complaint" || len(res.Choices) > 0 ||
		r.Router != "llm" || a.GW == nil {
		return templateAnswer(r.Tool, r.Args, res, r.Reply), "template", 0
	}
	payload, _ := json.Marshal(compact(res))
	out, err := a.GW.Chat(ctx, []llm.Message{{Role: "system", Content: answerPrompt},
		{Role: "user", Content: fmt.Sprintf("Customer: %s\n\nTool `%s` result:\n%s", text, r.Tool, payload)}}, nil, false)
	if err != nil || strings.TrimSpace(out.Message.Content) == "" {
		return templateAnswer(r.Tool, r.Args, res, ""), "template-fallback", 0
	}
	return strings.TrimSpace(out.Message.Content), "llm:" + out.Model, out.Tokens
}

func compact(res ToolResult) map[string]any {
	m := map[string]any{"ok": res.OK}
	if res.Message != "" {
		m["message"] = res.Message
	}
	var ps []map[string]any
	for _, p := range res.Products {
		ps = append(ps, map[string]any{"id": p.ID, "name": p.Name, "price": p.Price, "rating": p.Rating,
			"in_stock": p.Stock > 0, "features": p.Features})
	}
	if ps != nil {
		m["products"] = ps
	}
	var cs []map[string]string
	for _, c := range res.Chunks {
		cs = append(cs, map[string]string{"source": c.Title, "text": c.Text})
	}
	if cs != nil {
		m["policy_passages"] = cs
	}
	return m
}

func templateAnswer(tool string, args map[string]any, res ToolResult, reply string) string {
	switch {
	case tool == "chitchat":
		if reply != "" {
			return reply
		}
		return "Hi, I'm Sauby! I can find products, search by price, track, cancel or return your orders, process refunds, and answer policy questions."
	case len(res.Choices) > 0 || tool == "place_order":
		return res.Message
	case len(res.Products) > 0:
		head := "Here's what I found:"
		if tool == "get_product" {
			head = "Product details:"
		}
		var b strings.Builder
		b.WriteString(head)
		for _, p := range res.Products {
			feats := p.Features
			if len(feats) > 2 {
				feats = feats[:2]
			}
			fmt.Fprintf(&b, "\n- **%s** (%s) - $%.0f, %.1f stars: %s", p.Name, p.ID, p.Price, p.Rating, strings.Join(feats, ", "))
		}
		return b.String()
	case tool == "list_orders" && len(res.Orders) > 0:
		var b strings.Builder
		b.WriteString(res.Message)
		for _, o := range res.Orders {
			name := ""
			if len(o.ItemCards) > 0 {
				name = o.ItemCards[0].Name
				if len(o.ItemCards) > 1 {
					name += fmt.Sprintf(" +%d more", len(o.ItemCards)-1)
				}
			}
			fmt.Fprintf(&b, "\n- **%s** · %s · %s · $%.2f", o.ID, name, strings.ReplaceAll(o.Status, "_", " "), o.Total)
		}
		return b.String()
	case tool == "policy_search" && len(res.Chunks) > 0:
		// Without an LLM to synthesize, quote the top two passages so a near-miss at rank 1
		// still gets the customer the right answer (and the sources show where it came from).
		var parts, srcs []string
		for i, c := range res.Chunks {
			if i == 2 {
				break
			}
			body := c.Text
			if j := strings.Index(body, ": "); j >= 0 {
				body = body[j+2:]
			}
			parts = append(parts, body)
			srcs = append(srcs, c.Title)
		}
		return fmt.Sprintf("%s (sources: %s)", strings.Join(parts, " "), strings.Join(dedupe(srcs), ", "))
	case tool == "get_order" && res.OK && res.Order != nil:
		return orderStory(res.Order)
	}
	if res.Message != "" {
		return res.Message
	}
	return "Sorry, I couldn't find anything for that. Could you rephrase, or give me an order or product ID?"
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// orderStory describes an order the way a support person would.
func orderStory(o *OrderView) string {
	item := o.ID
	if len(o.ItemCards) > 0 {
		item = "your " + o.ItemCards[0].Name
		if len(o.ItemCards) > 1 {
			item += fmt.Sprintf(" (+%d more)", len(o.ItemCards)-1)
		}
	}
	when := func(d string) string {
		if t, err := time.Parse("2006-01-02", d); err == nil {
			return t.Format("Mon, Jan 2")
		}
		return d
	}
	ref := fmt.Sprintf("order %s, $%.2f", o.ID, o.Total)
	switch o.Status {
	case "delivered":
		return fmt.Sprintf("Good news: %s was delivered on %s (%s). How's it working out? I can help if anything's wrong with it.", item, when(o.DeliveredOn), ref)
	case "shipped":
		return fmt.Sprintf("%s is on its way 🚚 It shipped after you ordered on %s and should arrive in the next couple of days (%s).", upper(item), when(o.PlacedOn), ref)
	case "placed":
		return fmt.Sprintf("%s is confirmed and getting ready to ship (%s, placed %s). You can still cancel it until it ships.", upper(item), ref, when(o.PlacedOn))
	case "on_hold":
		return fmt.Sprintf("%s is waiting for a quick review by a teammate (%s). You won't be charged until it's approved.", upper(item), ref)
	case "cancelled":
		return fmt.Sprintf("That order was cancelled (%s). If it was paid, the refund went back to your original payment method.", ref)
	case "return_pending_approval":
		return fmt.Sprintf("Your return for %s is with a teammate for review (%s). They'll check the item's condition with you.", strings.TrimPrefix(item, "your "), ref)
	case "return_requested":
		msg := fmt.Sprintf("Your return for %s is in progress (RMA %s). Drop it off with the prepaid label from your email.", strings.TrimPrefix(item, "your "), o.RMA)
		if o.RefundOnReceipt {
			msg += fmt.Sprintf(" Your $%.2f refund is released as soon as it reaches our warehouse.", o.Total)
		}
		return msg
	case "return_received":
		return fmt.Sprintf("We've received your returned %s (%s). Ask me for the refund and I'll get it moving.", strings.TrimPrefix(item, "your "), ref)
	case "refund_pending_approval":
		return fmt.Sprintf("We've received %s back and the $%.2f refund is with a teammate for approval.", strings.TrimPrefix(item, "your "), o.Total)
	case "refunded":
		return fmt.Sprintf("Order %s is refunded: $%.2f went back to your original payment method (5-7 business days).", o.ID, o.Total)
	case "replacement_requested":
		return fmt.Sprintf("A free replacement for %s is on its way (order %s). Please send the original back with RMA %s.", strings.TrimPrefix(item, "your "), o.ReplacedBy, o.RMA)
	}
	return fmt.Sprintf("Order %s is %s (%s).", o.ID, strings.ReplaceAll(o.Status, "_", " "), ref)
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// FollowUps are the "what next?" chips under an answer: the actions a support
// person would offer next, based on what just happened.
func FollowUps(tool string, res ToolResult, guest bool) []Choice {
	if guest {
		return []Choice{{"Headphones under $100", "headphones under $100"}, {"How do returns work?", "how do returns work?"}}
	}
	if len(res.Choices) > 0 || res.NeedsConfirm || (res.Ticket != nil && res.Ticket.Kind == "live") {
		return nil // Sauby asked a question, or a person is joining: don't crowd it
	}
	if o := res.Order; o != nil && (tool == "get_order" || res.Committed) {
		id := o.ID
		switch o.Status {
		case "delivered":
			return []Choice{{"Something's wrong with it", "my " + id + " arrived broken"}, {"Return it", "return " + id},
				{"Get a replacement", "replace " + id}, {"It never arrived", "my order " + id + " never arrived"}}
		case "shipped":
			return []Choice{{"It's late", "my order " + id + " hasn't arrived yet"}, {"My other orders", "get my orders"}, {"Talk to a person", "talk to a person"}}
		case "placed", "on_hold":
			return []Choice{{"Cancel it", "cancel " + id}, {"My other orders", "get my orders"}}
		case "return_requested", "return_pending_approval":
			return []Choice{{"When is my refund?", "refund " + id}, {"My returns", "my returns"}}
		case "return_received":
			return []Choice{{"Refund it now", "refund " + id}}
		}
		return []Choice{{"My orders", "get my orders"}, {"Talk to a person", "talk to a person"}}
	}
	switch {
	case len(res.Products) == 1:
		p := res.Products[0]
		out := []Choice{{"More like this", p.Category}}
		if p.Stock > 0 {
			out = append([]Choice{{fmt.Sprintf("Order it · $%.0f", p.Price), "order " + p.ID}}, out...)
		}
		return append(out, Choice{fmt.Sprintf("%s under $%.0f", upper(p.Category), p.Price), fmt.Sprintf("%s under $%.0f", p.Category, p.Price)})
	case len(res.Products) > 1:
		top := res.Products[0]
		return []Choice{{"Tell me about the " + top.Name, "tell me about " + top.ID}, {"Order the " + top.Name, "order " + top.ID},
			{fmt.Sprintf("Cheaper than $%.0f", top.Price), fmt.Sprintf("%s under $%.0f", top.Category, top.Price)}}
	case tool == "list_orders" && len(res.Orders) > 0:
		return []Choice{{"What hasn't arrived?", "what hasn't been delivered yet"}, {"Start a return", "start a return"}, {"My returns", "my returns"}}
	case tool == "policy_search":
		return []Choice{{"Start a return", "start a return"}, {"See my orders", "get my orders"}, {"Talk to a person", "talk to a person"}}
	case tool == "chitchat":
		return []Choice{{"Where's my latest order?", "where is my latest order?"}, {"Show my orders", "get my orders"}, {"Find headphones under $100", "headphones under $100"}}
	}
	return nil
}

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

	"asksauby/internal/llm"
)

const answerPrompt = `You are Sauby from MyFlipKart.com. Answer the customer using ONLY the tool result.
Be warm and concise (2-5 sentences or a short list). Mention product IDs, order IDs and prices exactly as given.
Never invent products, orders, prices, refunds or policies. If the result says something failed, explain why
and what the customer can do next.`

// writeAnswer uses the LLM for FAQ-style answers; order/refund outcomes use
// deterministic templates so the money path never depends on generation.
func (a *Agent) writeAnswer(ctx context.Context, text string, r Route, res ToolResult) (string, string, int) {
	if r.Tool == "chitchat" || pathOf(r.Tool) == "transaction" || r.Tool == "handoff" ||
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
	case len(res.Choices) > 0:
		return res.Message
	case tool == "list_orders" && len(res.Orders) > 0:
		var b strings.Builder
		b.WriteString("Here are your recent orders:")
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
		o := res.Order
		var items []string
		for _, c := range o.ItemCards {
			items = append(items, fmt.Sprintf("%s (%s)", c.Name, c.ID))
		}
		when := ""
		if o.DeliveredOn != "" {
			when = ", delivered " + o.DeliveredOn
		}
		return fmt.Sprintf("Order %s is **%s** (placed %s%s). Items: %s. Total $%.2f.", o.ID,
			strings.ReplaceAll(o.Status, "_", " "), o.PlacedOn, when, strings.Join(items, ", "), o.Total)
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

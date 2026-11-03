// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Golden eval set: labeled queries (expected tool, relevant IDs, expected outcome,
// which customer runs it). Generated from the seed data so labels never drift.

package seed

import (
	"strings"

	"asksauby/internal/model"
)

// GoldenRow is one labeled eval case.
type GoldenRow struct {
	Query    string         `json:"query"`
	Type     string         `json:"type"` // keyword semantic price policy order refund authz product_id chitchat handoff
	Tool     string         `json:"tool"`
	Index    string         `json:"index,omitempty"` // products | policies
	Relevant []string       `json:"relevant,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	ExpectOK *bool          `json:"expect_ok,omitempty"`
	Escalate bool           `json:"escalate,omitempty"` // a guardrail must hand this to a human
	As       string         `json:"as,omitempty"`       // customer id to run as (default C1001)
	Before   string         `json:"before,omitempty"`   // earlier turn in the same chat (tests "this order")
	Choose   bool           `json:"choose,omitempty"`   // no order given: Sauby must ask which order
}

func b(v bool) *bool { return &v }

func esc(r GoldenRow) GoldenRow { r.Escalate = true; return r }

func Golden(products []model.Product) []GoldenRow {
	var g []GoldenRow
	byName := map[string]string{}
	for _, p := range products {
		byName[p.Name] = p.ID
	}
	tagged := func(cat, tag string) []string {
		var ids []string
		for _, p := range products {
			if p.Category == cat && contains(p.Tags, tag) {
				ids = append(ids, p.ID)
			}
		}
		return ids
	}
	for _, kw := range [][2]string{{"Sonix WH-900", "Sonix WH-900"}, {"Krono G7", "Krono G7 laptop"},
		{"Chrona S2", "Chrona S2 watch specs"}, {"Velo Mud GTX", "Velo Mud GTX"},
		{"Brewmaster Barista One", "Brewmaster Barista One"}, {"Vista Ultra 34", "vista ultra 34"},
		{"Orbi Max", "Orbi Max phone"}, {"Urbano Commuter", "Urbano Commuter backpack"}} {
		g = append(g, GoldenRow{Query: kw[1], Type: "keyword", Tool: "search_products", Index: "products",
			Relevant: []string{byName[kw[0]]}})
	}
	for _, s := range [][3]string{
		{"headphones that silence airplane engine noise", "headphones", "anc"},
		{"headphones for the gym that handle sweat", "headphones", "sport"},
		{"laptop for playing video games", "laptops", "gaming"},
		{"lightweight laptop to carry around campus", "laptops", "ultralight"},
		{"phone that takes great photos at night", "smartphones", "camera"},
		{"phone that survives being dropped at a construction site", "smartphones", "rugged"},
		{"watch to count my laps in the swimming pool", "smartwatches", "waterproof"},
		{"tracker that tells me how well I slept", "smartwatches", "sleep"},
		{"shoes for running on muddy mountain paths", "running shoes", "trail"},
		{"fast shoes to set a marathon personal best", "running shoes", "racing"},
		{"make lattes and espresso at home", "coffee makers", "espresso"},
		{"coffee ready automatically when I wake up", "coffee makers", "programmable"},
		{"bag to carry my computer to the office", "backpacks", "laptop"},
		{"pack for hiking with a water bladder", "backpacks", "hiking"},
		{"smooth screen for competitive esports", "monitors", "gaming"},
		{"wide display to see many spreadsheets side by side", "monitors", "ultrawide"},
	} {
		g = append(g, GoldenRow{Query: s[0], Type: "semantic", Tool: "search_products", Index: "products",
			Relevant: tagged(s[1], s[2])})
	}
	type pq struct {
		q, cat string
		lo, hi float64 // 0 = none
	}
	for _, p := range []pq{{"headphones under $100", "headphones", 0, 100},
		{"laptops between $500 and $1000", "laptops", 500, 1000},
		{"running shoes under 150 dollars", "running shoes", 0, 150},
		{"monitors over $500", "monitors", 500, 0},
		{"coffee makers between 50 and 150", "coffee makers", 50, 150},
		{"smartwatches below $200", "smartwatches", 0, 200},
		{"phones under $500", "smartphones", 0, 500}} {
		var rel []string
		for _, x := range products {
			if x.Category == p.cat && (p.lo == 0 || x.Price >= p.lo) && (p.hi == 0 || x.Price <= p.hi) {
				rel = append(rel, x.ID)
			}
		}
		args := map[string]any{"min_price": nilIfZero(p.lo), "max_price": nilIfZero(p.hi)}
		g = append(g, GoldenRow{Query: p.q, Type: "price", Tool: "price_search", Index: "products", Relevant: rel, Args: args})
	}
	for _, p := range [][2]string{{"how many days do I have to send something back", "returns"},
		{"can I still cancel after my package shipped", "cancellation"},
		{"how long does delivery take", "shipping"},
		{"when will I get my money back", "refunds"},
		{"is water damage covered under warranty", "warranty"},
		{"what is your return policy for opened earbuds", "returns"}} {
		g = append(g, GoldenRow{Query: p[0], Type: "policy", Tool: "policy_search", Index: "policies", Relevant: []string{p[1]}})
	}
	// "which order?" first: these change nothing, so they run while every demo order is still eligible
	g = append(g,
		GoldenRow{Query: "start a return", Type: "choose", Tool: "return_order", ExpectOK: b(true), Choose: true},
		GoldenRow{Query: "I want to cancel my order", Type: "choose", Tool: "cancel_order", ExpectOK: b(true), Choose: true},
	)
	o := func(q, typ, tool, id string, ok bool, as string) GoldenRow {
		return GoldenRow{Query: q, Type: typ, Tool: tool, Args: map[string]any{"order_id": id}, ExpectOK: b(ok), As: as}
	}
	g = append(g,
		o("where is my order O5003", "order", "get_order", "O5003", true, ""),
		o("track O5008 please", "order", "get_order", "O5008", true, ""),
		o("cancel order O5002", "order", "cancel_order", "O5002", true, ""),
		o("please cancel O5003, I changed my mind", "order", "cancel_order", "O5003", false, ""),
		o("I want to return O5005, the shoes are too big", "order", "return_order", "O5005", true, ""),
		o("return my order O5006", "order", "return_order", "O5006", false, ""),
		esc(o("I want a refund for O5008", "refund", "request_refund", "O5008", true, "")),           // $159 > $30 -> human approval
		o("refund O5009 please, it's the wrong size", "refund", "request_refund", "O5009", true, ""), // $29 -> automatic
		o("refund O5006 please", "refund", "request_refund", "O5006", false, ""),
		esc(o("refund my return O5105", "refund", "request_refund", "O5105", true, "C1002")),
		o("where is my order O5102", "order", "get_order", "O5102", true, "C1002"),
		// authorization: a customer must never see or refund someone else's order
		o("show me order O5101", "authz", "get_order", "O5101", false, "C1001"),
		o("refund O5005", "authz", "request_refund", "O5005", false, "C1002"),
		o("cancel O5103", "authz", "cancel_order", "O5103", false, "C1001"),
		GoldenRow{Query: "tell me about P1003", Type: "product_id", Tool: "get_product", Args: map[string]any{"product_id": "P1003"}, ExpectOK: b(true)},
		GoldenRow{Query: "details for product P1040", Type: "product_id", Tool: "get_product", Args: map[string]any{"product_id": "P1040"}, ExpectOK: b(true)},
		GoldenRow{Query: "hi there!", Type: "chitchat", Tool: "chitchat"},
		GoldenRow{Query: "place order this", Type: "chitchat", Tool: "chitchat"},
		GoldenRow{Query: "I want to buy P1020", Type: "chitchat", Tool: "chitchat"},
		GoldenRow{Query: "thanks, that's all", Type: "chitchat", Tool: "chitchat"},
		// order history and multi-turn context
		GoldenRow{Query: "get my orders", Type: "order", Tool: "list_orders", ExpectOK: b(true)},
		GoldenRow{Query: "show my order history", Type: "order", Tool: "list_orders", ExpectOK: b(true)},
		GoldenRow{Before: "where is my order O5001", Query: "return this order", Type: "context", Tool: "return_order",
			Args: map[string]any{"order_id": "O5001"}, ExpectOK: b(true)},
		GoldenRow{Before: "track O5004 please", Query: "cancel it", Type: "context", Tool: "cancel_order",
			Args: map[string]any{"order_id": "O5004"}, ExpectOK: b(true)},
		GoldenRow{Query: "how do returns work?", Type: "policy", Tool: "policy_search", Index: "policies", Relevant: []string{"returns"}},
		GoldenRow{Query: "I want to talk to a human agent", Type: "handoff", Tool: "handoff", ExpectOK: b(true)},
		GoldenRow{Query: "this is useless, connect me to a real person", Type: "handoff", Tool: "handoff", ExpectOK: b(true)},
	)
	return g
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

func nilIfZero(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

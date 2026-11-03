// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Unit tests for the order tools: authorization, the money rules and every guardrail.
package agent_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"asksauby/internal/agent"
	"asksauby/internal/store"
	"asksauby/internal/testkit"
)

const sid = "0123456789abcdef"

func run(t *agent.Tools, tool string, args map[string]any, cid string, commit bool) agent.ToolResult {
	return t.Run(tool, args, cid, sid, commit)
}

func pendingFor(st *store.Store, oid string) (string, bool) {
	for _, a := range st.Approvals() {
		if a.OrderID == oid && a.Status == "pending" {
			return a.ID, true
		}
	}
	return "", false
}

func TestCrossCustomerAccessIsBlocked(t *testing.T) {
	tl, st := testkit.Tools(t)
	for _, tool := range []string{"get_order", "cancel_order", "return_order", "request_refund", "replace_order"} {
		r := run(tl, tool, map[string]any{"order_id": "O5101"}, "C1001", true) // Meera's order, Aarav asking
		if r.OK || r.Committed || r.Escalated || r.Order != nil {
			t.Errorf("%s on another customer's order: %+v", tool, r)
		}
		if !strings.Contains(r.Message, "couldn't find order O5101") {
			t.Errorf("%s should look exactly like a missing order, got %q", tool, r.Message)
		}
	}
	if o, _ := st.Order("O5101"); o.Status != "delivered" {
		t.Fatalf("order changed: %s", o.Status)
	}
	r := run(tl, "file_complaint", map[string]any{"order_id": "O5101", "about": "seller", "summary": "x"}, "C1001", false)
	if r.OK {
		t.Error("complaint on another customer's order must fail")
	}
}

func TestGuestsCannotTouchOrders(t *testing.T) {
	tl, _ := testkit.Tools(t)
	for _, tool := range []string{"get_order", "list_orders", "cancel_order", "return_order", "request_refund", "place_order", "handoff", "file_complaint"} {
		r := run(tl, tool, map[string]any{"order_id": "O5001", "product_id": "P1001", "summary": "x"}, "G-abc", true)
		if r.OK || r.Committed {
			t.Errorf("guest %s succeeded: %+v", tool, r)
		}
	}
}

func TestNoRefundWithoutTheItem(t *testing.T) {
	tl, st := testkit.Tools(t)
	r := run(tl, "request_refund", map[string]any{"order_id": "O5009"}, "C1001", false)
	if !r.NeedsConfirm {
		t.Fatalf("expected a confirm for a $29 refund: %+v", r)
	}
	r = run(tl, "request_refund", map[string]any{"order_id": "O5009"}, "C1001", true)
	if !r.Committed {
		t.Fatalf("return should start: %+v", r)
	}
	if _, paid := st.RefundFor("O5009"); paid {
		t.Fatal("refund paid before the item came back")
	}
	if o, _ := st.Order("O5009"); o.Status != "return_requested" || !o.RefundOnReceipt || o.RMA == "" {
		t.Fatalf("bad order state %+v", o)
	}
	msg, err := tl.ReceiveReturn("O5009", "warehouse")
	if err != nil || !strings.Contains(msg, "refunded") {
		t.Fatalf("receive: %v %q", err, msg)
	}
	if _, err := tl.ReceiveReturn("O5009", "warehouse"); err == nil {
		t.Fatal("a second scan must not refund again")
	}
	if n := len(st.RefundsList()); n != 1 {
		t.Fatalf("want exactly one refund, got %d", n)
	}
}

func TestOverLimitReturnGoesToAPersonImmediately(t *testing.T) {
	tl, st := testkit.Tools(t)
	r := run(tl, "return_order", map[string]any{"order_id": "O5008"}, "C1001", false) // $159, dry run
	if r.NeedsConfirm || !r.Escalated || r.Ticket == nil || r.Ticket.Kind != "live" {
		t.Fatalf("expected an immediate live transfer: %+v", r)
	}
	if !strings.Contains(r.Message, "$29 limit") {
		t.Errorf("customer should hear the threshold: %q", r.Message)
	}
	if o, _ := st.Order("O5008"); o.Status != "return_pending_approval" || o.RMA != "" {
		t.Fatalf("no label before approval: %+v", o)
	}
	again := run(tl, "return_order", map[string]any{"order_id": "O5008"}, "C1001", false)
	if !again.Escalated || len(st.Approvals()) != 1 {
		t.Fatalf("asking twice must not open a second review: %+v (%d approvals)", again, len(st.Approvals()))
	}
}

func TestFourthReturnInAMonthGoesToAPerson(t *testing.T) {
	tl, st := testkit.Tools(t)
	st.Tx(func(tx *store.Txn) error {
		for _, id := range []string{"O5001", "O5005", "O5008"} {
			o, _ := tx.Order(id)
			o.ReturnStartedAt = time.Now().Add(-72 * time.Hour)
			tx.PutOrder(o)
		}
		return nil
	})
	r := run(tl, "return_order", map[string]any{"order_id": "O5009"}, "C1001", false) // $29: under the limit
	if !r.Escalated || r.Guardrail != "return_velocity" || !strings.Contains(r.Message, "3 returns in the last 30 days") {
		t.Fatalf("4th return must go to a person with the reason: %+v", r)
	}
}

func TestApprovalNeedsConditionAndPhoto(t *testing.T) {
	tl, st := testkit.Tools(t)
	run(tl, "return_order", map[string]any{"order_id": "O5104"}, "C1002", false) // 45 days: outside the window
	if _, ok := pendingFor(st, "O5104"); ok {
		t.Fatal("out-of-window return must be refused, not queued")
	}
	run(tl, "return_order", map[string]any{"order_id": "O5101"}, "C1002", false) // $249, Meera: standard, photo over $150
	id, ok := pendingFor(st, "O5101")
	if !ok {
		t.Fatal("expected a pending review")
	}
	if _, err := tl.DecideApproval(id, agent.Decision{Approve: true}, "agent"); err == nil {
		t.Fatal("approved without a condition")
	}
	if _, err := tl.DecideApproval(id, agent.Decision{Approve: true, Condition: "unused"}, "agent"); err == nil || !strings.Contains(err.Error(), "photo") {
		t.Fatalf("approved without a photo: %v", err)
	}
	if _, err := tl.DecideApproval(id, agent.Decision{Approve: true, Condition: "used"}, "agent"); err == nil {
		t.Fatal("used item approved without a note")
	}
	if _, err := tl.DecideApproval(id, agent.Decision{Approve: true, Condition: "bogus"}, "agent"); err == nil {
		t.Fatal("unknown condition accepted")
	}
	st.AttachEvidence("C1002", "00000000000000ab")
	a, err := tl.DecideApproval(id, agent.Decision{Approve: true, Condition: "unused"}, "agent")
	if err != nil || a.Condition != "unused" {
		t.Fatalf("approve: %v", err)
	}
	if _, err := tl.DecideApproval(id, agent.Decision{Approve: true, Condition: "unused"}, "agent"); err == nil {
		t.Fatal("approved twice")
	}
	if o, _ := st.Order("O5101"); o.Status != "return_requested" || o.RMA == "" || !o.RefundApproved {
		t.Fatalf("approved return state: %+v", o)
	}
	if _, paid := st.RefundFor("O5101"); paid {
		t.Fatal("approval must not pay before the item arrives")
	}
	if msg, _ := tl.ReceiveReturn("O5101", "wh"); !strings.Contains(msg, "refunded") {
		t.Fatalf("pre-approved refund should release on receipt: %q", msg)
	}
}

func TestRejectedReturnRestoresOrder(t *testing.T) {
	tl, st := testkit.Tools(t)
	run(tl, "return_order", map[string]any{"order_id": "O5008"}, "C1001", false)
	id, _ := pendingFor(st, "O5008")
	if _, err := tl.DecideApproval(id, agent.Decision{Approve: false, Note: "used"}, "agent"); err != nil {
		t.Fatal(err)
	}
	if o, _ := st.Order("O5008"); o.Status != "delivered" || o.RefundOnReceipt || !o.ReturnStartedAt.IsZero() {
		t.Fatalf("rejected return must restore the order: %+v", o)
	}
}

func TestConcurrentRefundsPayOnce(t *testing.T) {
	tl, st := testkit.Tools(t)
	tl.AutoRefundLimit = 0
	st.Tx(func(tx *store.Txn) error {
		o, _ := tx.Order("O5005")
		o.Status, o.RMA = "return_received", "RMA-X"
		tx.PutOrder(o)
		return nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); run(tl, "request_refund", map[string]any{"order_id": "O5005"}, "C1001", true) }()
	}
	wg.Wait()
	if n := len(st.RefundsList()); n != 1 {
		t.Fatalf("want 1 refund, got %d", n)
	}
}

func TestPlaceOrderRules(t *testing.T) {
	tl, st := testkit.Tools(t)
	for _, q := range []float64{0, -1, 4, 1e9} {
		if r := run(tl, "place_order", map[string]any{"product_id": "P1020", "quantity": q}, "C1001", true); r.Committed && q != 0 {
			t.Errorf("quantity %v accepted", q)
		}
	}
	if r := run(tl, "place_order", map[string]any{"product_id": "P1005"}, "C1001", true); r.Committed || r.Guardrail != "out_of_stock" || len(r.Choices) == 0 {
		t.Errorf("out of stock should offer alternatives: %+v", r)
	}
	if r := run(tl, "place_order", map[string]any{"product_id": "P9999"}, "C1001", true); r.OK {
		t.Error("unknown product accepted")
	}
	if r := run(tl, "place_order", map[string]any{"product_id": "P1011", "quantity": 1.0}, "C1001", true); !r.Escalated || r.Order.Status != "on_hold" {
		t.Errorf("$1299 order must be held: %+v", r)
	}
	p0, _ := st.Product("P1020")
	r := run(tl, "place_order", map[string]any{"product_id": "P1020", "quantity": 2.0}, "C1001", true)
	if !r.Committed || r.Order == nil || r.Order.Total != 158 {
		t.Fatalf("order: %+v", r)
	}
	if p1, _ := st.Product("P1020"); p1.Stock != p0.Stock-2 {
		t.Fatalf("stock %d -> %d", p0.Stock, p1.Stock)
	}
	c := run(tl, "cancel_order", map[string]any{"order_id": r.Order.ID}, "C1001", true)
	if !c.Committed || c.Refund == nil || c.Refund.Amount != 158 {
		t.Fatalf("cancel should refund the charge: %+v", c)
	}
	if p2, _ := st.Product("P1020"); p2.Stock != p0.Stock {
		t.Fatal("cancel must restock")
	}
	held := run(tl, "place_order", map[string]any{"product_id": "P1011"}, "C1002", true)
	hc := run(tl, "cancel_order", map[string]any{"order_id": held.Order.ID}, "C1002", true)
	if !hc.Committed || hc.Refund != nil {
		t.Fatalf("cancelling a held (uncharged) order must not refund: %+v", hc)
	}
}

func TestNoOversell(t *testing.T) {
	tl, st := testkit.Tools(t)
	st.Tx(func(tx *store.Txn) error { p, _ := tx.Product("P1033"); p.Stock = 3; tx.PutProduct(p); return nil })
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if r := run(tl, "place_order", map[string]any{"product_id": "P1033"}, []string{"C1001", "C1002", "C1003"}[i%3], true); r.Committed || r.Escalated {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if p, _ := st.Product("P1033"); p.Stock != 0 || ok != 3 {
		t.Fatalf("sold %d, stock left %d", ok, p.Stock)
	}
}

func TestReplacementRules(t *testing.T) {
	tl, st := testkit.Tools(t)
	if r := run(tl, "replace_order", map[string]any{"order_id": "O5106"}, "C1002", false); r.OK || len(r.Choices) == 0 {
		t.Errorf("out-of-stock replacement should offer a return: %+v", r)
	}
	r := run(tl, "replace_order", map[string]any{"order_id": "O5009"}, "C1001", true)
	if !r.Committed || r.Order == nil || r.Order.Total != 0 || r.Order.ReplacementFor != "O5009" {
		t.Fatalf("replacement: %+v", r)
	}
	again := run(tl, "replace_order", map[string]any{"order_id": "O5009"}, "C1001", true)
	if again.Committed || !strings.Contains(again.Message, "already on its way") {
		t.Fatalf("second replacement: %+v", again)
	}
	// deliver the replacement, then try to replace the replacement
	st.Tx(func(tx *store.Txn) error {
		o, _ := tx.Order(r.Order.ID)
		o.Status, o.DeliveredOn = "delivered", time.Now().Format("2006-01-02")
		tx.PutOrder(o)
		return nil
	})
	rr := run(tl, "replace_order", map[string]any{"order_id": r.Order.ID}, "C1001", true)
	if rr.Committed || rr.Guardrail != "second_replacement" {
		t.Fatalf("replacing a replacement must go to a person: %+v", rr)
	}
	if rf := run(tl, "request_refund", map[string]any{"order_id": "O5009"}, "C1001", true); rf.OK {
		t.Fatal("no refund on an order that was replaced")
	}
}

func TestStandingTiers(t *testing.T) {
	tl, st := testkit.Tools(t)
	if got := tl.CustomerStanding("C1001").Tier; got != "loyal" {
		t.Fatalf("Aarav starts loyal, got %s", got)
	}
	for _, id := range []string{"O5008", "O5005"} {
		run(tl, "return_order", map[string]any{"order_id": id}, "C1001", false)
	}
	for _, a := range st.Approvals() {
		tl.DecideApproval(a.ID, agent.Decision{Approve: false, Note: "used"}, "agent")
	}
	sd := tl.CustomerStanding("C1001")
	if sd.Tier != "watch" || sd.PhotoOver != 0 {
		t.Fatalf("two declined returns -> watch, got %+v", sd)
	}
	if r := run(tl, "replace_order", map[string]any{"order_id": "O5009"}, "C1001", true); r.Committed {
		t.Fatal("watched accounts can't self-serve replacements")
	}
	// withdrawn requests don't count against a customer
	_, st2 := testkit.Tools(t)
	tl2 := &agent.Tools{S: st2, R: tl.R, ReturnWindow: 30, AutoRefundLimit: 29}
	run(tl2, "return_order", map[string]any{"order_id": "O5008"}, "C1001", false)
	id, _ := pendingFor(st2, "O5008")
	tl2.WithdrawApproval(id, "agent")
	if tier := tl2.CustomerStanding("C1001").Tier; tier == "watch" || tier == "blocked" {
		t.Fatalf("withdrawn request penalized: %s", tier)
	}
}

func TestSuspendedSellerIsHidden(t *testing.T) {
	tl, _ := testkit.Tools(t)
	for i, s := range []string{"00000000000000c1", "00000000000000c2"} {
		tl.Run("file_complaint", map[string]any{"order_id": []string{"O5001", "O5007"}[i], "about": "seller", "summary": "counterfeit"}, "C1001", s, false)
	}
	r := run(tl, "search_products", map[string]any{"query": "noise cancelling headphones for flights"}, "C1003", false)
	for _, p := range r.Products {
		if p.Seller == "SoundCity Retail" {
			t.Fatalf("suspended seller in results: %s", p.Name)
		}
	}
	if o := run(tl, "place_order", map[string]any{"product_id": "P1001"}, "C1003", true); o.Committed || o.Guardrail != "seller_suspended" {
		t.Fatalf("order from suspended seller: %+v", o)
	}
}

func TestMissingItemClaim(t *testing.T) {
	tl, st := testkit.Tools(t)
	r := run(tl, "request_refund", map[string]any{"order_id": "O5101", "reason": "it never arrived"}, "C1002", false)
	if !r.Escalated || r.Ticket == nil || r.Guardrail != "not_received_claim" {
		t.Fatalf("missing item goes to a person: %+v", r)
	}
	id, _ := pendingFor(st, "O5101")
	a, err := tl.DecideApproval(id, agent.Decision{Approve: true}, "agent") // condition defaults to not_received; no photo needed
	if err != nil || !a.Refunded {
		t.Fatalf("claim approval should refund without a return: %v %+v", err, a)
	}
}

func TestCustomerTextCannotSkipTheReturnLabel(t *testing.T) {
	tl, st := testkit.Tools(t)
	r := run(tl, "replace_order", map[string]any{"order_id": "O5009", "reason": "not received yet, can you replace it?"}, "C1001", true)
	if !r.Committed {
		t.Fatalf("replacement: %+v", r)
	}
	if o, _ := st.Order("O5009"); o.RMA == "" {
		t.Fatal("customer-written reason dropped the return label: free item + keep the original")
	}
}

func TestLateMissingItemClaimIsRefused(t *testing.T) {
	tl, st := testkit.Tools(t)
	r := run(tl, "request_refund", map[string]any{"order_id": "O5006", "reason": "it never arrived"}, "C1001", false) // delivered 60 days ago
	if r.Escalated || r.OK {
		t.Fatalf("60-day-old missing-item claim auto-queued: %+v", r)
	}
	if len(st.Approvals()) != 0 {
		t.Fatal("approval created")
	}
}

// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Agent tests for simulated teammates: every concern ends in a resolution (fixed
// now, or a tracked case with an email), and returns are investigated before approval.
package live

import (
	"strings"
	"testing"
	"time"

	"asksauby/internal/agent"
	"asksauby/internal/model"
	"asksauby/internal/testkit"
)

type harness struct {
	t   *testing.T
	h   *Hub
	cid string
	sid string
	tk  model.Ticket
}

func newHarness(t *testing.T, cid, sid string, opener func(*agent.Tools) agent.ToolResult) *harness {
	tl, st := testkit.Tools(t)
	h := &Hub{S: st, Tools: tl, Sim: true, Pace: 0.001, Router: func() agent.Router { return agent.RuleRouter{} }}
	r := opener(tl)
	if r.Ticket == nil || r.Ticket.Kind != "live" {
		t.Fatalf("no live ticket: %+v", r)
	}
	tk, err := h.Join(r.Ticket.ID, "Mei Tan", "simulated")
	if err != nil {
		t.Fatal(err)
	}
	h.simGreet(tk)
	return &harness{t, h, cid, sid, tk}
}

func (x *harness) say(text string) string {
	x.h.S.AppendMessage(x.cid, x.sid, model.Message{TS: time.Now(), Role: "user", Content: text})
	x.h.simReply(x.tk, text)
	return x.last()
}

func (x *harness) last() string {
	msgs, _ := x.h.S.Messages(x.cid, x.sid)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "agent" {
			return msgs[i].Content
		}
	}
	return ""
}

func (x *harness) all() string {
	msgs, _ := x.h.S.Messages(x.cid, x.sid)
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}
	return b.String()
}

func handoff(cid, sid string) func(*agent.Tools) agent.ToolResult {
	return func(tl *agent.Tools) agent.ToolResult {
		return tl.Run("handoff", map[string]any{"summary": "asked for a person"}, cid, sid, false)
	}
}

func TestTeammateResolvesMissingItemWithoutLooping(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e1", handoff("C1001", "00000000000000e1"))
	if r := x.say("item did not arrive"); !strings.Contains(r, "Which order") {
		t.Fatalf("should ask which order: %q", r)
	}
	if r := x.say("O5009"); !strings.Contains(r, "replacement") {
		t.Fatalf("loyal customer should be offered a fix: %q\n%s", r, x.all())
	}
	r := x.say("yes")
	if !strings.Contains(r, "free replacement is on its way") {
		t.Fatalf("yes should send the replacement: %q", r)
	}
	if o, _ := x.h.S.Order("O5009"); o.Status != "replacement_requested" || o.RMA != "" {
		t.Fatalf("missing-item replacement needs nothing back: %+v", o)
	}
	if strings.Count(x.all(), "I've opened case") != 1 {
		t.Fatalf("case opened more than once:\n%s", x.all())
	}
	if r := x.say("no thanks"); !strings.Contains(r, "Have a great day") {
		t.Fatalf("should close: %q", r)
	}
}

func TestTeammateRefundsMissingItemOnNo(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e2", handoff("C1001", "00000000000000e2"))
	x.say("my order O5009 says delivered but it never arrived")
	r := x.say("no, refund please")
	if !strings.Contains(r, "approved a full refund") {
		t.Fatalf("refund expected: %q\n%s", r, x.all())
	}
	if _, paid := x.h.S.RefundFor("O5009"); !paid {
		t.Fatal("refund not recorded")
	}
}

func TestStandardCustomerGetsCaseAndEmail(t *testing.T) {
	x := newHarness(t, "C1002", "00000000000000e3", handoff("C1002", "00000000000000e3"))
	r := x.say("my O5101 never arrived")
	if !strings.Contains(r, "email") || !strings.Contains(x.all(), "CASE-") {
		t.Fatalf("should open a case and promise an email: %q", r)
	}
	if r := x.say("it never arrived!!"); !strings.Contains(r, "I'm on it") {
		t.Fatalf("repeating the concern should give status, not a new case: %q", r)
	}
}

func TestVagueRepliesEndInACase(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e4", handoff("C1001", "00000000000000e4"))
	for _, m := range []string{"sure", "yes", "hmm"} {
		r := x.say(m)
		if strings.Contains(r, "I'd suggest") || strings.Contains(r, "connecting you with a teammate") {
			t.Fatalf("%q got an off-topic reply: %q", m, r)
		}
	}
	if !strings.Contains(x.last(), "opened case") {
		t.Fatalf("third vague reply should open a specialist case:\n%s", x.all())
	}
}

func TestInvestigationBeforeApproval(t *testing.T) {
	x := newHarness(t, "C1002", "00000000000000e5", func(tl *agent.Tools) agent.ToolResult {
		return tl.Run("return_order", map[string]any{"order_id": "O5101"}, "C1002", "00000000000000e5", false)
	})
	if !strings.Contains(x.last(), "Before I approve it") {
		t.Fatalf("teammate must ask about condition first:\n%s", x.all())
	}
	if r := x.say("it's unused, still sealed in the box"); !strings.Contains(r, "photo") {
		t.Fatalf("$249 > $150: photo needed: %q", r)
	}
	if r := x.say("ok"); !strings.Contains(r, "camera button") {
		t.Fatalf("should keep waiting for the photo: %q", r)
	}
	x.h.S.AttachEvidence("C1002", "00000000000000ff")
	x.h.investigate(x.tk, "", true)
	if !strings.Contains(x.last(), "approved your return") {
		t.Fatalf("photo then approve:\n%s", x.all())
	}
}

func TestUsedItemIsDeclined(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e6", func(tl *agent.Tools) agent.ToolResult {
		return tl.Run("return_order", map[string]any{"order_id": "O5005"}, "C1001", "00000000000000e6", false)
	})
	r := x.say("I wore them for a couple of weeks, just don't like them")
	if !strings.Contains(r, "doesn't qualify") {
		t.Fatalf("used item should be declined: %q", r)
	}
	if o, _ := x.h.S.Order("O5005"); o.Status != "delivered" {
		t.Fatalf("declined return must restore the order: %s", o.Status)
	}
}

func TestFaultyItemReplacement(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e7", handoff("C1001", "00000000000000e7"))
	if r := x.say("my O5009 bag is broken"); !strings.Contains(r, "replacement or start a return") {
		t.Fatalf("should offer replacement or refund: %q", r)
	}
	if r := x.say("replacement please"); !strings.Contains(r, "replacement is on its way") {
		t.Fatalf("replacement: %q", r)
	}
}

func TestTeammateDoesNotApproveHeldOrders(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e8", func(tl *agent.Tools) agent.ToolResult {
		r := tl.Run("place_order", map[string]any{"product_id": "P1011"}, "C1001", "00000000000000e8", true) // $1299: held
		if !r.Escalated {
			t.Fatalf("expected a held order: %+v", r)
		}
		return tl.Run("handoff", map[string]any{"summary": "approve my order"}, "C1001", "00000000000000e8", false)
	})
	for _, a := range x.h.S.Approvals() {
		if a.Status != "pending" {
			t.Fatalf("simulated teammate decided a held order: %+v", a)
		}
	}
	if !strings.Contains(x.last(), "orders team") {
		t.Fatalf("customer should be told who decides: %q", x.last())
	}
}

func TestTeammateDoesNotSettleLargeClaims(t *testing.T) {
	x := newHarness(t, "C1001", "00000000000000e9", handoff("C1001", "00000000000000e9"))
	x.say("my O5005 never arrived") // $568
	if strings.Contains(x.all(), "approved a full refund") || strings.Contains(x.last(), "Would you like a replacement") {
		t.Fatalf("$568 claim settled without the claims team:\n%s", x.all())
	}
	if !strings.Contains(x.last(), "email") {
		t.Fatalf("customer must be told what happens next: %q", x.last())
	}
	if _, paid := x.h.S.RefundFor("O5005"); paid {
		t.Fatal("paid")
	}
}

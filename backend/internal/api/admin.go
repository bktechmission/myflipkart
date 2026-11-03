// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Admin endpoints: approvals queue, live chats (human agent console), knowledge
// base editing with live re-index, insights for the feedback loop, golden-set growth.

package api

import (
	"asksauby/internal/agent"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/img"
	"asksauby/internal/live"
	"asksauby/internal/model"
	"asksauby/internal/search"
	"asksauby/internal/store"
)

// ---------------- human-agent approval queue ----------------

func (s *Server) approvals(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	out := s.store.Approvals()
	if out == nil {
		out = []model.Approval{}
	}
	ret := s.store.AwaitingReturn()
	if ret == nil {
		ret = []model.Order{}
	}
	writeJSON(w, 200, map[string]any{"approvals": out, "tickets": s.store.Tickets(), "limit": s.cfg.AutoRefundLimit, "returns": ret,
		"sellers": s.tools.Sellers()})
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct{ Decision, Condition, Note string }
	if json.NewDecoder(r.Body).Decode(&in) != nil || (in.Decision != "approve" && in.Decision != "reject") {
		writeJSON(w, 400, map[string]string{"error": "decision must be approve or reject"})
		return
	}
	a, err := s.tools.DecideApproval(r.PathValue("id"), agent.Decision{Approve: in.Decision == "approve", Condition: in.Condition, Note: in.Note}, sess.Email)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if a.SessionID != "" { // tell the customer in their chat
		s.store.AppendMessage(a.CustomerID, a.SessionID, model.Message{TS: time.Now().UTC(), Role: "agent", Content: agent.ApprovalUpdate(a, sess.Name),
			Meta: map[string]any{"agent_name": sess.Name, "agent_kind": "human", "ask_feedback": "escalated_decided"}})
	}
	s.store.LogEvent(model.Event{TS: time.Now().UTC(), CustomerID: a.CustomerID, Query: "approval " + a.ID + " " + a.Status + " by " + sess.Email,
		Tool: "human_approval", Path: "transaction", OK: true, Status: 200, Source: "admin", Resolved: a.Status == "approved"})
	writeJSON(w, 200, a)
}

// receiveReturn is the warehouse scan of a returned item. Refunds waiting on the
// item are released here (under the same guardrails), never before.
func (s *Server) receiveReturn(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	oid := r.PathValue("id")
	msg, err := s.tools.ReceiveReturn(oid, sess.Email)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if o, ok := s.store.Order(oid); ok { // tell the customer in their most recent conversation
		if ss := s.store.Sessions(o.CustomerID); len(ss) > 0 {
			s.store.AppendMessage(o.CustomerID, ss[0].SID, model.Message{TS: time.Now().UTC(), Role: "system", Content: msg})
		}
	}
	s.store.LogEvent(model.Event{TS: time.Now().UTC(), Query: "return received " + oid + " by " + sess.Email,
		Tool: "warehouse_receive", Path: "transaction", OK: true, Status: 200, Source: "admin"})
	writeJSON(w, 200, map[string]string{"message": msg})
}

// ---------------- knowledge base: edit -> re-index live ----------------

func (s *Server) knowledge(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, 200, map[string]any{"policies": s.store.Policies(), "products": s.store.Products(), "index": s.retriever.Stats()})
}

func (s *Server) savePolicy(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in model.PolicyDoc
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Title) == "" {
		writeJSON(w, 400, map[string]string{"error": "title and paragraphs required"})
		return
	}
	in.ID = r.PathValue("id")
	var paras []string
	for _, p := range in.Paras {
		if p = strings.TrimSpace(p); p != "" {
			paras = append(paras, p)
		}
	}
	if len(paras) == 0 || len(paras) > 40 {
		writeJSON(w, 400, map[string]string{"error": "1-40 non-empty paragraphs required"})
		return
	}
	in.Paras = paras
	doc, err := s.store.SavePolicy(in, sess.Email)
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error(), "current": doc})
		return
	}
	t0 := time.Now()
	if err := s.retriever.ReindexPolicies(s.store.Policies()); err != nil { // BM25 + vectors rebuilt, swapped atomically
		writeJSON(w, 500, map[string]string{"error": "saved, but re-index failed: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"policy": doc, "reindex_ms": time.Since(t0).Milliseconds(), "index": s.retriever.Stats()})
}

func (s *Server) saveProduct(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	var in struct {
		Description string   `json:"description"`
		Features    []string `json:"features"`
		Price       float64  `json:"price"`
	}
	p, ok := s.store.Product(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such product"})
		return
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Description) == "" || in.Price <= 0 {
		writeJSON(w, 400, map[string]string{"error": "description and a positive price required"})
		return
	}
	p.Description, p.Price = strings.TrimSpace(in.Description), in.Price
	if len(in.Features) > 0 {
		p.Features = nil
		for _, f := range in.Features {
			if f = strings.TrimSpace(f); f != "" {
				p.Features = append(p.Features, f)
			}
		}
	}
	if err := s.store.SaveProduct(p); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	t0 := time.Now()
	if err := s.retriever.ReindexProducts(s.store.Products()); err != nil {
		writeJSON(w, 500, map[string]string{"error": "saved, but re-index failed: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"product": p, "reindex_ms": time.Since(t0).Milliseconds(), "index": s.retriever.Stats()})
}

// ---------------- insights: where the agent fails, from real traffic ----------------

type countRow struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
	Extra string `json:"extra,omitempty"`
}

func top(m map[string]*countRow, n int) []countRow {
	out := []countRow{}
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (s *Server) insights(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	since := time.Now().Add(-7 * 24 * time.Hour)
	zero, handoffs, failedTx, degraded, guards := map[string]*countRow{}, map[string]*countRow{}, map[string]*countRow{}, map[string]*countRow{}, map[string]int{}
	firstQ := map[string]model.Event{}
	bump := func(m map[string]*countRow, k, extra string) {
		k = strings.ToLower(strings.TrimSpace(k))
		if m[k] == nil {
			m[k] = &countRow{Text: k, Extra: extra}
		}
		m[k].Count++
	}
	for _, e := range s.store.Events(since) {
		if e.Source != "chat" {
			continue
		}
		if _, ok := firstQ[e.SessionID]; !ok {
			firstQ[e.SessionID] = e
		}
		switch {
		case (e.Tool == "search_products" || e.Tool == "price_search" || e.Tool == "policy_search") && e.NResults == 0:
			bump(zero, e.Query, e.Tool)
		case e.Handoff:
			bump(handoffs, e.Query, "")
		case e.Path == "transaction" && !e.OK:
			bump(failedTx, e.Query, e.Tool)
		}
		for _, f := range e.Flags {
			if f == "degraded" || f == "failover" {
				bump(degraded, e.Query, f)
			}
			if strings.HasPrefix(f, "guardrail:") {
				guards[strings.TrimPrefix(f, "guardrail:")]++
			}
		}
	}
	type fbRow struct {
		model.Feedback
		FirstQuery string `json:"first_query"`
		Tool       string `json:"tool"`
	}
	var down []fbRow
	for _, f := range s.store.Feedback(since) {
		if !f.Helpful {
			down = append(down, fbRow{f, firstQ[f.SessionID].Query, firstQ[f.SessionID].Tool})
		}
	}
	sort.Slice(down, func(i, j int) bool { return down[i].TS.After(down[j].TS) })
	if len(down) > 20 {
		down = down[:20]
	}
	var actions []string // plain-language next steps derived from the data
	if n := len(zero); n > 0 {
		actions = append(actions, fmt.Sprintf("%d distinct searches returned nothing: add synonyms/products or a policy paragraph, then label them into the golden set.", n))
	}
	if n := len(handoffs); n > 0 {
		actions = append(actions, fmt.Sprintf("%d distinct questions ended in a human handoff: read the transcripts, fix the knowledge gap, add each as a golden case.", n))
	}
	if n := len(down); n > 0 {
		actions = append(actions, fmt.Sprintf("%d thumbs-down sessions: open the transcript, label the right tool/answer, and add to the golden set before the next rollout.", n))
	}
	if g := guards["amount_over_limit"]; g > 0 {
		actions = append(actions, fmt.Sprintf("%d refunds were held for approval: if approvers approve ~all of them, consider raising AUTO_REFUND_LIMIT.", g))
	}
	if len(degraded) > 0 {
		actions = append(actions, "Some turns ran degraded (rules fallback) or failed over: check LLM provider health and circuit breaker settings.")
	}
	writeJSON(w, 200, map[string]any{"zero_results": top(zero, 15), "handoffs": top(handoffs, 15), "failed_transactions": top(failedTx, 15),
		"degraded": top(degraded, 15), "guardrails": guards, "thumbs_down": down, "actions": actions})
}

func (s *Server) transcript(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	msgs, err := s.store.Messages(r.PathValue("cid"), r.PathValue("sid"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, 200, msgs)
}

// addGolden: an admin labels a real query (expected tool, optional relevant IDs) and it
// joins the eval set, so every future candidate agent is tested on it.
func (s *Server) addGolden(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct {
		Query    string   `json:"query"`
		Tool     string   `json:"tool"`
		Relevant []string `json:"relevant"`
		Index    string   `json:"index"`
		Note     string   `json:"note"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Query) == "" || in.Tool == "" {
		writeJSON(w, 400, map[string]string{"error": "query and tool required"})
		return
	}
	row := map[string]any{"query": strings.TrimSpace(in.Query), "type": "mined", "tool": in.Tool,
		"added_by": sess.Email, "added_at": time.Now().UTC(), "note": in.Note}
	if len(in.Relevant) > 0 && (in.Index == "products" || in.Index == "policies") {
		row["relevant"], row["index"] = in.Relevant, in.Index
	}
	if err := s.store.AppendGolden(row); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// searchProbe lets an admin confirm an edit is live in BM25 + vectors right away.
func (s *Server) searchProbe(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	q := r.URL.Query().Get("q")
	if r.URL.Query().Get("index") == "products" {
		hits, info, err := s.retriever.SearchProducts(search.ProductQuery{Query: q})
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"hits": hits, "info": info})
		return
	}
	hits, info, err := s.retriever.SearchPolicies(q, 5, "")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := []map[string]any{}
	for _, h := range hits {
		c := s.retriever.Chunk(h.ID)
		out = append(out, map[string]any{"hit": h, "text": c.Text})
	}
	writeJSON(w, 200, map[string]any{"hits": out, "info": info})
}

// ---------------- live chats (human agent console) ----------------

func (s *Server) liveQueue(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	var out []live.Context
	for _, t := range s.store.Tickets() {
		if t.Kind != "live" || (t.Status == "closed" && time.Since(t.ClosedAt) > 2*time.Hour) {
			continue
		}
		out = append(out, s.Hub.ContextFor(t))
	}
	if out == nil {
		out = []live.Context{}
	}
	writeJSON(w, 200, map[string]any{"tickets": out, "sim_agents": s.cfg.SimAgents, "sim_join_after_sec": s.cfg.SimJoinAfterSec})
}

func (s *Server) liveJoin(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	t, err := s.Hub.Join(r.PathValue("id"), sess.Name, "human")
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) liveSend(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var in struct{ Text string }
	if json.NewDecoder(r.Body).Decode(&in) != nil || strings.TrimSpace(in.Text) == "" || len(in.Text) > 2000 {
		writeJSON(w, 400, map[string]string{"error": "text required (max 2000 chars)"})
		return
	}
	if t, ok := s.store.Ticket(r.PathValue("id")); !ok || t.AgentName != sess.Name || t.AgentKind != "human" {
		writeJSON(w, 403, map[string]string{"error": "join the ticket first"})
		return
	}
	if err := s.Hub.Send(r.PathValue("id"), in.Text); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) liveClose(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	if err := s.Hub.Close(r.PathValue("id")); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------------- customer-facing catalog, images, live state ----------------

// liveState is polled by the customer's chat while a human (or simulated teammate) owns it.
func (s *Server) liveState(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if !store.ValidSID(r.PathValue("sid")) {
		writeJSON(w, 400, map[string]string{"error": "bad session"})
		return
	}
	writeJSON(w, 200, s.Hub.StateFor(sess.CID, r.PathValue("sid"), after))
}

func (s *Server) products(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	type card struct {
		model.Product
		Image string `json:"image"`
	}
	out := []card{}
	cat := r.URL.Query().Get("category")
	for _, p := range s.store.Products() {
		if cat == "" || p.Category == cat {
			out = append(out, card{p, "/img/" + p.ID + ".svg"})
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) productImage(w http.ResponseWriter, r *http.Request) {
	id := strings.ToUpper(strings.TrimSuffix(r.PathValue("file"), ".svg"))
	p, ok := s.store.Product(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	fmt.Fprint(w, img.SVG(p.Category, p.Name))
}

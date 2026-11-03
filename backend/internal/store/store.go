// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package store is a small durable store built on the standard library.
//
//   - Orders, refunds, approvals, users, tickets, policy docs and product edits:
//     in-memory maps behind one mutex, with a write-ahead log (wal.jsonl,
//     fsync'd) replayed on start. A Tx writes the WAL record BEFORE applying the
//     change, so an acknowledged refund is never lost and two concurrent refunds
//     on one order serialize (strong consistency on the money path).
//   - Conversations: data/conversations/<cid>/<sid>.jsonl, fsync'd per message.
//   - Telemetry + feedback: events.jsonl, appended without fsync (eventual, cheap).
//
// In production: Postgres for orders/refunds/approvals (row locks or SERIALIZABLE),
// DynamoDB/Cassandra keyed by (cid, sid) for conversations, a log pipeline for events.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"asksauby/internal/model"
	"asksauby/internal/seed"
)

type Store struct {
	dir string // "" = memory only (used by evals)

	mu        sync.RWMutex
	products  map[string]model.Product
	order     []string
	policies  map[string]model.PolicyDoc
	polOrder  []string
	users     map[string]model.User
	orders    map[string]model.Order
	refunds   map[string]model.Refund // by order id: one refund per order (idempotency)
	approvals map[string]model.Approval
	tickets   map[string]model.Ticket // by id (updated in place as status changes)
	wal       *os.File

	evMu     sync.Mutex
	events   []model.Event
	feedback []model.Feedback
	evFile   *os.File
	convMu   sync.Mutex
	mem      map[string][]byte          // photos for memory-only stores (evals)
	convs    map[string][]model.Message // conversations for memory-only stores
}

type rec struct {
	Kind     string           `json:"kind"`
	User     *model.User      `json:"user,omitempty"`
	Order    *model.Order     `json:"order,omitempty"`
	Refund   *model.Refund    `json:"refund,omitempty"`
	Approval *model.Approval  `json:"approval,omitempty"`
	Ticket   *model.Ticket    `json:"ticket,omitempty"`
	Policy   *model.PolicyDoc `json:"policy,omitempty"`
	Product  *model.Product   `json:"product,omitempty"`
}

var sidRe = regexp.MustCompile(`^[a-f0-9]{8,32}$`)
var cidRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// Open loads (or seeds) a store under dir. dir == "" gives a fresh in-memory store.
func Open(dir, demoOrdersFor string) (*Store, error) {
	s := &Store{dir: dir, products: map[string]model.Product{}, users: map[string]model.User{},
		orders: map[string]model.Order{}, refunds: map[string]model.Refund{}, approvals: map[string]model.Approval{},
		tickets:  map[string]model.Ticket{},
		policies: map[string]model.PolicyDoc{}}
	for _, p := range seed.Products() {
		s.products[p.ID] = p
		s.order = append(s.order, p.ID)
	}
	for _, p := range seed.Policies {
		p.UpdatedAt = time.Now().UTC()
		s.policies[p.ID] = p
		s.polOrder = append(s.polOrder, p.ID)
	}
	if dir == "" {
		s.seedRecords(demoOrdersFor)
		return s, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0o700); err != nil {
		return nil, err
	}
	walPath := filepath.Join(dir, "wal.jsonl")
	if _, err := os.Stat(walPath); errors.Is(err, os.ErrNotExist) {
		s.seedRecords(demoOrdersFor)
		if err := s.writeSnapshot(walPath); err != nil {
			return nil, err
		}
	} else if err := s.replay(walPath); err != nil {
		return nil, err
	}
	var err error
	if s.wal, err = os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
		return nil, err
	}
	s.loadEvents()
	if s.evFile, err = os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) seedRecords(demoOrdersFor string) {
	now := time.Now().UTC()
	for _, u := range seed.DemoUsers {
		s.users[u.CustomerID] = model.User{CustomerID: u.CustomerID, Email: u.Email, Name: u.Name, CreatedAt: now}
	}
	extra := ""
	if demoOrdersFor != "" && s.userByEmailLocked(demoOrdersFor) == nil {
		extra = "C2001"
		s.users[extra] = model.User{CustomerID: extra, Email: demoOrdersFor, Name: demoOrdersFor, CreatedAt: now}
	}
	for _, o := range seed.Orders(seed.Products(), time.Now(), extra) {
		s.orders[o.ID] = o
	}
}

// writeSnapshot writes the full state as WAL records to a temp file and renames it (atomic compaction).
func (s *Store) writeSnapshot(path string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, u := range s.users {
		u := u
		enc.Encode(rec{Kind: "user", User: &u})
	}
	for _, o := range s.orders {
		o := o
		enc.Encode(rec{Kind: "order", Order: &o})
	}
	for _, r := range s.refunds {
		r := r
		enc.Encode(rec{Kind: "refund", Refund: &r})
	}
	for _, a := range s.approvals {
		a := a
		enc.Encode(rec{Kind: "approval", Approval: &a})
	}
	for _, t := range s.tickets {
		t := t
		enc.Encode(rec{Kind: "ticket", Ticket: &t})
	}
	for _, p := range s.policies {
		if p.Version > 1 {
			p := p
			enc.Encode(rec{Kind: "policy", Policy: &p})
		}
	}
	for _, p := range s.products { // persist edited products only
		if orig, _ := seedProduct(p.ID); orig.Description != p.Description || len(orig.Features) != len(p.Features) || orig.Price != p.Price {
			p := p
			enc.Encode(rec{Kind: "product", Product: &p})
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

func seedProduct(id string) (model.Product, bool) {
	for _, p := range seed.Products() {
		if p.ID == id {
			return p, true
		}
	}
	return model.Product{}, false
}

func (s *Store) replay(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) == nil { // a torn last line is skipped
			s.apply(r)
		}
	}
	f.Close()
	return s.writeSnapshot(path) // compact on start
}

func (s *Store) apply(r rec) {
	switch r.Kind {
	case "user":
		s.users[r.User.CustomerID] = *r.User
	case "order":
		s.orders[r.Order.ID] = *r.Order
	case "refund":
		s.refunds[r.Refund.OrderID] = *r.Refund
	case "approval":
		s.approvals[r.Approval.ID] = *r.Approval
	case "ticket":
		s.tickets[r.Ticket.ID] = *r.Ticket
	case "policy":
		if _, ok := s.policies[r.Policy.ID]; !ok {
			s.polOrder = append(s.polOrder, r.Policy.ID)
		}
		s.policies[r.Policy.ID] = *r.Policy
	case "product":
		s.products[r.Product.ID] = *r.Product
	}
}

// ---------------- transactions ----------------

// Txn stages changes; nothing is visible until the WAL write succeeds.
type Txn struct {
	s    *Store
	recs []rec
}

func (t *Txn) Order(id string) (model.Order, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if t.recs[i].Order != nil && t.recs[i].Order.ID == id {
			return *t.recs[i].Order, true
		}
	}
	o, ok := t.s.orders[id]
	return o, ok
}
func (t *Txn) Refund(orderID string) (model.Refund, bool) {
	r, ok := t.s.refunds[orderID]
	return r, ok
}
func (t *Txn) Approval(id string) (model.Approval, bool) { a, ok := t.s.approvals[id]; return a, ok }

// PendingApprovalFor returns an open approval on the order, if any.
func (t *Txn) PendingApprovalFor(orderID string) (model.Approval, bool) {
	for _, a := range t.s.approvals {
		if a.OrderID == orderID && a.Status == "pending" {
			return a, true
		}
	}
	return model.Approval{}, false
}

// RefundsSince counts a customer's refunds since t (velocity guardrail).
func (t *Txn) RefundsSince(cid string, since time.Time) int {
	n := 0
	for _, r := range t.s.refunds {
		if r.CustomerID == cid && r.CreatedAt.After(since) {
			n++
		}
	}
	return n
}

// OpenLiveTicket finds the conversation's open live ticket inside the transaction.
func (t *Txn) OpenLiveTicket(cid, sid string) (model.Ticket, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if k := t.recs[i].Ticket; k != nil && k.CustomerID == cid && k.SessionID == sid && k.Kind == "live" && k.Status != "closed" {
			return *k, true
		}
	}
	for _, k := range t.s.tickets {
		if k.CustomerID == cid && k.SessionID == sid && k.Kind == "live" && k.Status != "closed" {
			return k, true
		}
	}
	return model.Ticket{}, false
}

// Approvals and tickets as the transaction sees them (for customer/seller standing).
func (t *Txn) AllApprovals() []model.Approval {
	out := make([]model.Approval, 0, len(t.s.approvals))
	for _, a := range t.s.approvals {
		out = append(out, a)
	}
	return out
}

// Product reads a product inside the transaction (stock is checked and decremented under the lock).
func (t *Txn) Product(id string) (model.Product, bool) {
	for i := len(t.recs) - 1; i >= 0; i-- {
		if t.recs[i].Product != nil && t.recs[i].Product.ID == id {
			return *t.recs[i].Product, true
		}
	}
	p, ok := t.s.products[id]
	return p, ok
}
func (t *Txn) PutProduct(p model.Product) { t.recs = append(t.recs, rec{Kind: "product", Product: &p}) }

// OrdersFor lists a customer's orders as this transaction sees them.
func (t *Txn) OrdersFor(cid string) []model.Order {
	seen := map[string]bool{}
	var out []model.Order
	for i := len(t.recs) - 1; i >= 0; i-- {
		if o := t.recs[i].Order; o != nil && o.CustomerID == cid && !seen[o.ID] {
			seen[o.ID] = true
			out = append(out, *o)
		}
	}
	for id, o := range t.s.orders {
		if o.CustomerID == cid && !seen[id] {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NextOrderID allocates the next order number (O + 4 digits) under the write lock.
func (t *Txn) NextOrderID() (string, error) {
	max := 5000
	bump := func(id string) {
		if n, err := strconv.Atoi(strings.TrimPrefix(id, "O")); err == nil && n > max {
			max = n
		}
	}
	for id := range t.s.orders {
		bump(id)
	}
	for _, r := range t.recs {
		if r.Order != nil {
			bump(r.Order.ID)
		}
	}
	if max >= 9999 {
		return "", fmt.Errorf("order numbers exhausted")
	}
	return fmt.Sprintf("O%d", max+1), nil
}

func (t *Txn) PutOrder(o model.Order) {
	o.UpdatedAt = time.Now().UTC()
	t.recs = append(t.recs, rec{Kind: "order", Order: &o})
}
func (t *Txn) PutRefund(r model.Refund) { t.recs = append(t.recs, rec{Kind: "refund", Refund: &r}) }
func (t *Txn) PutApproval(a model.Approval) {
	t.recs = append(t.recs, rec{Kind: "approval", Approval: &a})
}
func (t *Txn) PutTicket(k model.Ticket) { t.recs = append(t.recs, rec{Kind: "ticket", Ticket: &k}) }
func (t *Txn) PutUser(u model.User)     { t.recs = append(t.recs, rec{Kind: "user", User: &u}) }

// Tx runs fn under the write lock, then durably logs and applies its changes.
func (s *Store) Tx(fn func(*Txn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Txn{s: s}
	if err := fn(t); err != nil {
		return err
	}
	return s.commit(t.recs)
}

func (s *Store) commit(recs []rec) error {
	if s.wal != nil && len(recs) > 0 {
		buf := []byte{}
		for _, r := range recs {
			line, _ := json.Marshal(r)
			buf = append(append(buf, line...), '\n')
		}
		if _, err := s.wal.Write(buf); err != nil {
			return fmt.Errorf("wal write: %w", err)
		}
		if err := s.wal.Sync(); err != nil {
			return fmt.Errorf("wal sync: %w", err)
		}
	}
	for _, r := range recs {
		s.apply(r)
	}
	return nil
}

// ResetOrders restores demo orders and clears refunds/approvals/tickets (admin action).
func (s *Store) ResetOrders(demoOrdersFor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders, s.refunds, s.approvals, s.tickets = map[string]model.Order{}, map[string]model.Refund{}, map[string]model.Approval{}, map[string]model.Ticket{}
	extra := ""
	if demoOrdersFor != "" {
		if u := s.userByEmailLocked(demoOrdersFor); u != nil {
			extra = u.CustomerID
		}
	}
	for _, o := range seed.Orders(seed.Products(), time.Now(), extra) {
		s.orders[o.ID] = o
	}
	if s.dir == "" {
		return nil
	}
	path := filepath.Join(s.dir, "wal.jsonl")
	s.wal.Close()
	if err := s.writeSnapshot(path); err != nil {
		return err
	}
	var err error
	s.wal, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	return err
}

// ---------------- knowledge base (admin-editable) ----------------

func (s *Store) Products() []model.Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Product, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.products[id])
	}
	return out
}

func (s *Store) Product(id string) (model.Product, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.products[id]
	return p, ok
}

func (s *Store) Policies() []model.PolicyDoc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.PolicyDoc, 0, len(s.polOrder))
	for _, id := range s.polOrder {
		out = append(out, s.policies[id])
	}
	return out
}

// SavePolicy stores a new version of a policy doc (optimistic concurrency on Version).
func (s *Store) SavePolicy(p model.PolicyDoc, by string) (model.PolicyDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.policies[p.ID]
	if exists && p.Version != cur.Version {
		return cur, fmt.Errorf("policy %s was changed by %s (version %d); reload and retry", p.ID, cur.UpdatedBy, cur.Version)
	}
	p.Version = cur.Version + 1
	p.UpdatedBy, p.UpdatedAt = by, time.Now().UTC()
	return p, s.commit([]rec{{Kind: "policy", Policy: &p}})
}

func (s *Store) SaveProduct(p model.Product) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.products[p.ID]; !ok {
		return fmt.Errorf("no product %s", p.ID)
	}
	return s.commit([]rec{{Kind: "product", Product: &p}})
}

// ---------------- reads ----------------

func (s *Store) Order(id string) (model.Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	return o, ok
}

func (s *Store) RefundFor(orderID string) (model.Refund, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.refunds[orderID]
	return r, ok
}

func (s *Store) OrdersFor(cid string) []model.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Order
	for _, o := range s.orders {
		if o.CustomerID == cid {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AllOrders returns every order (seller scorecards).
func (s *Store) AllOrders() []model.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out
}

// AwaitingReturn lists orders whose item is on its way back to the warehouse.
func (s *Store) AwaitingReturn() []model.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Order
	for _, o := range s.orders {
		if o.Status == "return_requested" || (o.Status == "replacement_requested" && o.RMA != "") {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Approvals() []model.Approval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Approval
	for _, a := range s.approvals {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) User(cid string) (model.User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[cid]
	return u, ok
}

func (s *Store) userByEmailLocked(email string) *model.User {
	for _, u := range s.users {
		if u.Email == email {
			u := u
			return &u
		}
	}
	return nil
}

// LoginUser finds the customer for a verified IdP identity, creating one on first login.
func (s *Store) LoginUser(provider, sub, email, name, picture string) (model.User, error) {
	var out model.User
	err := s.Tx(func(t *Txn) error {
		u := s.userByEmailLocked(email)
		if u == nil {
			u = &model.User{CustomerID: fmt.Sprintf("C%d", 3000+len(s.users)), Email: email, CreatedAt: time.Now().UTC()}
		}
		u.Provider, u.Subject = provider, sub
		if name != "" {
			u.Name = name
		}
		if picture != "" {
			u.Picture = picture
		}
		t.PutUser(*u)
		out = *u
		return nil
	})
	return out, err
}

// Tickets returns all tickets, newest first.
func (s *Store) Tickets() []model.Ticket {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Ticket, 0, len(s.tickets))
	for _, t := range s.tickets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// TicketLocked reads a ticket while the caller already holds the store lock (inside Tx).
func (s *Store) TicketLocked(id string) (model.Ticket, bool) { t, ok := s.tickets[id]; return t, ok }

func (s *Store) Ticket(id string) (model.Ticket, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tickets[id]
	return t, ok
}

// OpenLiveTicket returns the waiting/active live ticket for a session, if any.
func (s *Store) OpenLiveTicket(cid, sid string) (model.Ticket, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.tickets {
		if t.CustomerID == cid && t.SessionID == sid && t.Kind == "live" && t.Status != "closed" {
			return t, true
		}
	}
	return model.Ticket{}, false
}

// AttachEvidence adds an uploaded photo to the customer's pending return/refund approvals.
func (s *Store) AttachEvidence(cid, photoID string) []string {
	var ids []string
	s.Tx(func(tx *Txn) error {
		for _, a := range tx.AllApprovals() {
			if a.CustomerID == cid && a.Status == "pending" && (a.Kind == "return" || a.Kind == "refund" || a.Kind == "") {
				a.Evidence = append(a.Evidence, photoID)
				tx.PutApproval(a)
				ids = append(ids, a.ID)
			}
		}
		return nil
	})
	return ids
}

// Photos: customer uploads (re-encoded JPEG, metadata stripped) under photos/<cid>/<id>.jpg.
var photoIDRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

func (s *Store) SavePhoto(cid, id string, jpeg []byte) error {
	if !cidRe.MatchString(cid) || !photoIDRe.MatchString(id) {
		return errors.New("bad photo key")
	}
	if s.dir == "" {
		s.convMu.Lock()
		if s.mem == nil {
			s.mem = map[string][]byte{}
		}
		s.mem[cid+"/"+id] = jpeg
		s.convMu.Unlock()
		return nil
	}
	dir := filepath.Join(s.dir, "photos", cid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".jpg"), jpeg, 0o600)
}

func (s *Store) Photo(cid, id string) ([]byte, error) {
	if !cidRe.MatchString(cid) || !photoIDRe.MatchString(id) {
		return nil, errors.New("bad photo key")
	}
	if s.dir == "" {
		s.convMu.Lock()
		defer s.convMu.Unlock()
		if b, ok := s.mem[cid+"/"+id]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(s.dir, "photos", cid, id+".jpg"))
}

// SaveTicket durably writes a ticket's new state.
func (s *Store) SaveTicket(t model.Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commit([]rec{{Kind: "ticket", Ticket: &t}})
}

func (s *Store) RefundsList() []model.Refund {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Refund
	for _, r := range s.refunds {
		out = append(out, r)
	}
	return out
}

// ---------------- conversations (cid + sid) ----------------

func ValidSID(sid string) bool { return sidRe.MatchString(sid) }

func (s *Store) convPath(cid, sid string) (string, error) {
	if !cidRe.MatchString(cid) || !ValidSID(sid) {
		return "", errors.New("bad conversation key")
	}
	return filepath.Join(s.dir, "conversations", cid, sid+".jsonl"), nil
}

// AppendMessage durably stores one message before the caller acknowledges it.
func (s *Store) AppendMessage(cid, sid string, m model.Message) error {
	p, err := s.convPath(cid, sid)
	if err != nil {
		return err
	}
	if s.dir == "" { // memory-only store (tests, evals)
		s.convMu.Lock()
		defer s.convMu.Unlock()
		if s.convs == nil {
			s.convs = map[string][]model.Message{}
		}
		s.convs[p] = append(s.convs[p], m)
		return nil
	}
	s.convMu.Lock()
	defer s.convMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(m)
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) Messages(cid, sid string) ([]model.Message, error) {
	p, err := s.convPath(cid, sid)
	if err != nil {
		return nil, err
	}
	if s.dir == "" {
		s.convMu.Lock()
		defer s.convMu.Unlock()
		return append([]model.Message(nil), s.convs[p]...), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []model.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var m model.Message
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

type SessionInfo struct {
	SID     string    `json:"sid"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
}

func (s *Store) Sessions(cid string) []SessionInfo {
	if s.dir == "" || !cidRe.MatchString(cid) {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(s.dir, "conversations", cid, "*.jsonl"))
	var out []SessionInfo
	for _, f := range files {
		sid := filepath.Base(f[:len(f)-len(".jsonl")])
		msgs, _ := s.Messages(cid, sid)
		info := SessionInfo{SID: sid}
		for _, m := range msgs {
			if m.Role == "user" && info.Title == "" {
				info.Title = m.Content
			}
			info.Updated = m.TS
		}
		if len(info.Title) > 48 {
			info.Title = info.Title[:48] + "…"
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// ---------------- telemetry + feedback ----------------

func (s *Store) LogEvent(e model.Event) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	s.events = append(s.events, e)
	if len(s.events) > 200000 { // keep memory bounded; the file keeps everything
		s.events = append([]model.Event(nil), s.events[len(s.events)-100000:]...)
	}
	if s.evFile != nil {
		line, _ := json.Marshal(e)
		s.evFile.Write(append(line, '\n'))
	}
}

func (s *Store) Events(since time.Time) []model.Event {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	var out []model.Event
	for _, e := range s.events {
		if !e.TS.Before(since) {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) AddFeedback(f model.Feedback) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	s.feedback = append(s.feedback, f)
	if len(s.feedback) > 100000 {
		s.feedback = append([]model.Feedback(nil), s.feedback[len(s.feedback)-50000:]...)
	}
	if s.evFile != nil {
		line, _ := json.Marshal(map[string]any{"feedback": f})
		s.evFile.Write(append(line, '\n'))
	}
}

func (s *Store) Feedback(since time.Time) []model.Feedback {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	var out []model.Feedback
	for _, f := range s.feedback {
		if !f.TS.Before(since) {
			out = append(out, f)
		}
	}
	return out
}

func (s *Store) loadEvents() {
	f, err := os.Open(filepath.Join(s.dir, "events.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for sc.Scan() {
		var fb struct {
			Feedback *model.Feedback `json:"feedback"`
		}
		if json.Unmarshal(sc.Bytes(), &fb) == nil && fb.Feedback != nil {
			if fb.Feedback.TS.After(cutoff) {
				s.feedback = append(s.feedback, *fb.Feedback)
			}
			continue
		}
		var e model.Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.TS.After(cutoff) {
			s.events = append(s.events, e)
		}
	}
}

// ---------------- golden-set growth (feedback loop) ----------------

// AppendGolden adds an admin-labeled case mined from live traffic to data/golden_extra.jsonl.
func (s *Store) AppendGolden(row any) error {
	if s.dir == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "golden_extra.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(row)
	_, err = f.Write(append(line, '\n'))
	return err
}

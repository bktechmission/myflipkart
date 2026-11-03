// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package model holds the shared domain types.
package model

import "time"

type Product struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Brand       string   `json:"brand"`
	Category    string   `json:"category"`
	Price       float64  `json:"price"`
	Rating      float64  `json:"rating"`
	Stock       int      `json:"stock"`
	Tags        []string `json:"tags"`
	Features    []string `json:"features"`
	Description string   `json:"description"`
}

type User struct {
	CustomerID string    `json:"customer_id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Picture    string    `json:"picture,omitempty"`
	Provider   string    `json:"provider,omitempty"` // google | mock | guest
	Subject    string    `json:"subject,omitempty"`  // IdP "sub"
	CreatedAt  time.Time `json:"created_at"`
}

type Order struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	// Lifecycle: placed -> shipped -> delivered -> (return_requested -> return_received -> refunded)
	// or (replacement_requested, with a new $0 replacement order). on_hold = waiting for a teammate.
	Status      string   `json:"status"`
	PlacedOn    string   `json:"placed_on"`
	DeliveredOn string   `json:"delivered_on,omitempty"`
	Items       []string `json:"items"`
	Total       float64  `json:"total"`
	RMA         string   `json:"rma,omitempty"`
	// RefundOnReceipt: the customer asked for a refund; it is released only when the
	// returned item reaches the warehouse (we never refund an item that wasn't returned).
	RefundOnReceipt bool      `json:"refund_on_receipt,omitempty"`
	RefundApproved  bool      `json:"refund_approved,omitempty"`   // a teammate pre-approved the refund (over the limit)
	ReturnStartedAt time.Time `json:"return_started_at,omitempty"` // returns-per-month guardrail
	ReplacementFor  string    `json:"replacement_for,omitempty"`   // this order replaces that one
	ReplacedBy      string    `json:"replaced_by,omitempty"`       // the replacement order sent for this one
	Shipping        float64   `json:"shipping,omitempty"`
	Payment         string    `json:"payment,omitempty"` // e.g. "card ending 4242" (demo)
	UpdatedAt       time.Time `json:"updated_at"`
}

type Refund struct {
	ID         string    `json:"id"`
	OrderID    string    `json:"order_id"`
	CustomerID string    `json:"customer_id"`
	Amount     float64   `json:"amount"`
	Reason     string    `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
}

// Approval is a refund held for a human agent (amount over the auto-refund limit,
// or velocity limit hit). Nothing moves until an admin approves it.
type Approval struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind,omitempty"`      // "" or refund | return | order (large/fast orders) | replacement
	NoReturn   bool      `json:"no_return,omitempty"` // item reported not received: refund on approval
	Refunded   bool      `json:"refunded,omitempty"`  // money actually moved when decided
	Evidence   []string  `json:"evidence,omitempty"`  // photo IDs the customer uploaded for this request
	Condition  string    `json:"condition,omitempty"` // what the teammate found: unused | defective | used | not_received
	Note       string    `json:"note,omitempty"`      // the teammate's decision note
	OrderID    string    `json:"order_id"`
	CustomerID string    `json:"customer_id"`
	SessionID  string    `json:"session_id,omitempty"` // conversation to notify when decided
	Amount     float64   `json:"amount"`
	Reason     string    `json:"reason"`
	Why        string    `json:"why"` // why it needs a human
	PrevStatus string    `json:"prev_status"`
	Status     string    `json:"status"` // pending | approved | rejected
	TicketID   string    `json:"ticket_id"`
	CreatedAt  time.Time `json:"created_at"`
	DecidedBy  string    `json:"decided_by,omitempty"`
	DecidedAt  time.Time `json:"decided_at,omitempty"`
}

// PolicyDoc is an editable knowledge-base document (indexed for RAG).
type PolicyDoc struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Paras     []string  `json:"paras"`
	Version   int       `json:"version"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Ticket is a request for a human. Kind "live" moves the customer's chat to a
// human agent (the same chat window, keyed by cid + sid); kind "approval" is a
// back-office task (e.g. approve a refund over the limit).
type Ticket struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`            // live | approval
	Topic      string    `json:"topic,omitempty"` // e.g. seller_complaint, delivery_issue
	CustomerID string    `json:"customer_id"`
	SessionID  string    `json:"session_id"`
	Summary    string    `json:"summary"`
	Status     string    `json:"status"` // waiting | active | closed
	AgentName  string    `json:"agent_name,omitempty"`
	AgentKind  string    `json:"agent_kind,omitempty"` // human | simulated
	CreatedAt  time.Time `json:"created_at"`
	JoinedAt   time.Time `json:"joined_at,omitempty"`
	ClosedAt   time.Time `json:"closed_at,omitempty"`
}

// Message is one durable conversation entry, stored per customer (cid) per session (sid).
type Message struct {
	TS      time.Time      `json:"ts"`
	Role    string         `json:"role"` // user | assistant | status
	Content string         `json:"content"`
	Tool    string         `json:"tool,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// Event is one telemetry row per chat turn.
type Event struct {
	TS         time.Time `json:"ts"`
	CustomerID string    `json:"customer_id"`
	SessionID  string    `json:"session_id"`
	Query      string    `json:"query"`
	Tool       string    `json:"tool"`
	Path       string    `json:"path"` // faq | transaction | chitchat | handoff
	Router     string    `json:"router"`
	Model      string    `json:"model"`
	OK         bool      `json:"ok"`
	Status     int       `json:"status"` // HTTP-ish status of the turn (200, 429, 500)
	Error      string    `json:"error,omitempty"`
	NResults   int       `json:"n_results"`
	LatencyMS  float64   `json:"latency_ms"`
	TTFTMS     float64   `json:"ttft_ms"` // time to first byte the customer sees (status message)
	Tokens     int       `json:"tokens"`
	Source     string    `json:"source"`    // chat | eval
	Resolved   bool      `json:"resolved"`  // the customer's task got done (order action committed)
	Handoff    bool      `json:"handoff"`   // customer asked for a human
	Escalated  bool      `json:"escalated"` // guardrail sent it to a human (e.g. refund > $30)
	Variant    string    `json:"variant"`   // agent version (control / canary) for A/B rollout
	Flags      []string  `json:"flags,omitempty"`
}

type Feedback struct {
	TS         time.Time `json:"ts"`
	CustomerID string    `json:"customer_id"`
	SessionID  string    `json:"session_id"`
	Helpful    bool      `json:"helpful"`
	Outcome    string    `json:"outcome,omitempty"` // resolved | handoff | escalated | answer
	Comment    string    `json:"comment,omitempty"`
	Variant    string    `json:"variant,omitempty"`
}

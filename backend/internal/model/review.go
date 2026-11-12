// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// ReviewGuard domain types: product reviews, the signals and labels that explain
// each decision, moderation cases, appeals, sanctions and the audit log. All of
// them are durable WAL records (see package store).

package model

import "time"

// Review is one shopper review. Text is the original and is never rewritten;
// Shown is what shoppers see (Text, or Text with contact info masked by code).
type Review struct {
	ID         string    `json:"id"`
	ProductID  string    `json:"product_id"`
	OrderID    string    `json:"order_id"`
	CustomerID string    `json:"customer_id"`
	Seller     string    `json:"seller"`
	Stars      int       `json:"stars"`
	Text       string    `json:"text"`
	PhotoText  string    `json:"photo_text,omitempty"` // text found on an attached photo (context only, never quoted)
	Shown      string    `json:"shown,omitempty"`
	IdemKey    string    `json:"idem_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`

	// Pipeline outcome. Status: pending | published | held | rejected | removed.
	Status    string   `json:"status"`
	Action    string   `json:"action,omitempty"` // publish | mask_publish | hold | reject | not_counted
	Counted   bool     `json:"counted"`          // counts toward the product's shown rating
	Verified  bool     `json:"verified"`         // a delivered order of this product by this customer
	Language  string   `json:"language,omitempty"`
	Rules     []string `json:"rules,omitempty"`   // policy rule ids cited for the action
	Reasons   []string `json:"reasons,omitempty"` // plain-language reasons
	Signals   []Signal `json:"signals,omitempty"`
	Labels    []Label  `json:"labels,omitempty"`    // tagger labels that passed the verifier
	Discarded []Label  `json:"discarded,omitempty"` // tagger labels the verifier threw out
	Rejects   []string `json:"verifier_notes,omitempty"`
	Target    string   `json:"target,omitempty"` // self | other | product (from the tagger)
	Suggested string   `json:"suggested,omitempty"`

	// Which tagger ran, and the versions every decision is audited with.
	Tagger        string  `json:"tagger,omitempty"` // none | rules | llm
	TaggerStatus  string  `json:"tagger_status,omitempty"`
	Model         string  `json:"model,omitempty"`
	PolicyVersion string  `json:"policy_version,omitempty"`
	PromptVersion string  `json:"prompt_version,omitempty"`
	Tokens        int     `json:"tokens,omitempty"`
	LatencyMS     float64 `json:"latency_ms,omitempty"`
	Path          string  `json:"path,omitempty"` // clear | rules | llm (latency SLO buckets)

	CaseID    string    `json:"case_id,omitempty"`
	DecidedBy string    `json:"decided_by,omitempty"` // "engine" or a moderator
	DecidedAt time.Time `json:"decided_at,omitempty"`
	Version   int       `json:"version"` // bumped on every state change (compare-and-set)

	Synthetic bool   `json:"synthetic,omitempty"` // demo data, never a real shopper
	Scenario  string `json:"scenario,omitempty"`
}

// Signal is one deterministic finding. Start/End are rune offsets into the
// original text (-1 when the signal is about behaviour, not wording).
type Signal struct {
	Name     string `json:"name"`
	Rule     string `json:"rule,omitempty"`
	Kind     string `json:"kind"`     // content | authenticity | context
	Strength string `json:"strength"` // strong | medium | weak | info
	Detail   string `json:"detail"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

// Label is one tagger finding, in the exact JSON shape the LLM must produce.
type Label struct {
	RuleID   string  `json:"rule_id"`
	Severity string  `json:"severity"`
	Quotes   []Quote `json:"quotes"`
}

// Quote is an exact substring of the review: runes [Start, End) of Text.
type Quote struct {
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Identity is what the platform knows about an account or a seller's staff
// member: hashed device, payment card, address and network. Seller != "" marks
// seller staff. Demo identities are synthetic.
type Identity struct {
	Subject   string    `json:"subject"` // customer id, or "staff:<seller>:<n>"
	Seller    string    `json:"seller,omitempty"`
	Device    string    `json:"device,omitempty"`
	Card      string    `json:"card,omitempty"`
	Address   string    `json:"address,omitempty"`
	Network   string    `json:"network,omitempty"`
	CreatedAt time.Time `json:"created_at"` // account created
	Synthetic bool      `json:"synthetic,omitempty"`
}

// Payout is money a seller sent to a customer outside a normal return
// (a refund or cashback). A 5★ review followed by one is a paid review.
type Payout struct {
	ID         string    `json:"id"`
	Seller     string    `json:"seller"`
	CustomerID string    `json:"customer_id"`
	OrderID    string    `json:"order_id,omitempty"`
	Kind       string    `json:"kind"` // refund | cashback
	Amount     float64   `json:"amount"`
	At         time.Time `json:"at"`
	Synthetic  bool      `json:"synthetic,omitempty"`
}

// ReviewCase groups related held reviews for one moderator decision.
type ReviewCase struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`  // grouping key, e.g. "seller:SoundNest Store:P1005"
	Kind      string    `json:"kind"` // seller_campaign | rival_attack | burst | ring | content | report | appeal
	Title     string    `json:"title"`
	Seller    string    `json:"seller,omitempty"` // the seller the evidence links to
	ProductID string    `json:"product_id,omitempty"`
	ReviewIDs []string  `json:"review_ids"`
	Severity  string    `json:"severity"` // low | medium | high
	Reach     int       `json:"reach"`
	Evidence  []string  `json:"evidence"`
	Suggested string    `json:"suggested"`           // reject | remove | publish | keep_out
	Sanctions []string  `json:"sanctions,omitempty"` // suggested, applied only by a person
	Status    string    `json:"status"`              // open | decided
	Decision  string    `json:"decision,omitempty"`  // approve | dismiss
	Applied   []string  `json:"applied,omitempty"`   // sanctions the moderator applied
	Note      string    `json:"note,omitempty"`
	DecidedBy string    `json:"decided_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	DecidedAt time.Time `json:"decided_at,omitempty"`
	Synthetic bool      `json:"synthetic,omitempty"`
}

// Appeal asks a second person to look again. Authors appeal their own review;
// sellers appeal reviews that stopped counting (they never see who wrote them).
type Appeal struct {
	ID           string    `json:"id"`
	By           string    `json:"by"`      // customer id or seller name
	ByRole       string    `json:"by_role"` // author | seller
	ReviewIDs    []string  `json:"review_ids"`
	ProductID    string    `json:"product_id,omitempty"`
	Reason       string    `json:"reason,omitempty"` // reason group the seller appeals
	Text         string    `json:"text"`
	Status       string    `json:"status"` // open | upheld | overturned
	FirstDecider string    `json:"first_decider"`
	DecidedBy    string    `json:"decided_by,omitempty"`
	DeciderKind  string    `json:"decider_kind,omitempty"` // human | simulated
	Note         string    `json:"note,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	DecidedAt    time.Time `json:"decided_at,omitempty"`
}

// Sanction is a penalty a person applied: seller_strike | rating_freeze | delist | customer_ban.
type Sanction struct {
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	Target string    `json:"target"` // seller name, product id or customer id
	CaseID string    `json:"case_id"`
	By     string    `json:"by"`
	Note   string    `json:"note,omitempty"`
	At     time.Time `json:"at"`
	Lifted bool      `json:"lifted,omitempty"`
}

// ProductReviewState is per-product review state: an automatic burst pause
// (new reviews wait for a person) and a rating freeze a person applied.
type ProductReviewState struct {
	ProductID    string    `json:"product_id"`
	BurstSince   time.Time `json:"burst_since,omitempty"` // non-zero: rating paused at its last trusted value
	BurstCleared time.Time `json:"burst_cleared,omitempty"`
	Frozen       bool      `json:"frozen,omitempty"`
	FrozenRating float64   `json:"frozen_rating,omitempty"`
	FrozenN      int       `json:"frozen_n,omitempty"`
}

// AuditEntry is one append-only record of who did what, with the versions used.
type AuditEntry struct {
	ID            string    `json:"id"`
	TS            time.Time `json:"ts"`
	Actor         string    `json:"actor"`      // "engine", a moderator email, or a demo persona
	ActorKind     string    `json:"actor_kind"` // engine | human | simulated | customer | seller
	Action        string    `json:"action"`
	Target        string    `json:"target"`
	Note          string    `json:"note,omitempty"`
	Tagger        string    `json:"tagger,omitempty"`
	Model         string    `json:"model,omitempty"`
	PromptVersion string    `json:"prompt_version,omitempty"`
	PolicyVersion string    `json:"policy_version,omitempty"`
}

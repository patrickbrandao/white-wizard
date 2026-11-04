// Package limits enforces rate limits, quotas, blocks and bans for the four
// entity levels (ip → key → user → account). Store is the interface; memory
// and Valkey implement it.
package limits

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// EntityKind is one of the four levels.
type EntityKind string

const (
	KindIP      EntityKind = "ip"
	KindKey     EntityKind = "key"
	KindUser    EntityKind = "user"
	KindAccount EntityKind = "account"
	KindProject EntityKind = "project"
	KindModel   EntityKind = "model" // per-model concurrency/queue
	KindGlobal  EntityKind = "global"
	// KindFreeModels is the unit of the requests to free models: OpenRouter's
	// ":free" variants (20 per minute and 50 or 1000 per day per account;
	// ADR 0060) and OpenCode's free models (a daily count; ADR 0072). Its id
	// is the account's, or "ip:<address>" in OpenCode's public mode (no
	// key). It is not an account, so promotion and tiers never see it.
	KindFreeModels EntityKind = "free_models"
	// KindModelQuota is the dollar quota of one model (OpenCode Go's
	// quota_usd.models; ADR 0072), id "<owner>/<model id>", the owner being
	// the account or, in Go Teams, the member. An ordinary entity: its
	// limits are quota_usd (and the windows q5h, qwk, qmo).
	KindModelQuota EntityKind = "model_quota"
	// KindModelSpend is the monthly spend of one model at one level (the
	// budgets of coverage "models", spend_monthly_models_usd; ADR 0072), id
	// "<kind>:<id>/<model id>", e.g. "account:acme/glm-5.3". An ordinary
	// entity: its limit is spend_monthly_usd.
	KindModelSpend EntityKind = "model_spend"
	// KindDeployment is a deployment of a model in an account (Azure
	// OpenAI, spec 006 16.6): the unit the provider limits, with TPM and
	// the RPM derived from it. Its id is "<account>/<model id>".
	KindDeployment EntityKind = "deployment"
)

// Entity identifies something that can be limited or blocked.
type Entity struct {
	Kind EntityKind `json:"kind"`
	ID   string     `json:"id"`
}

// Status of an entity.
type Status string

const (
	StatusActive    Status = "active"
	StatusLimited   Status = "limited"
	StatusSuspended Status = "suspended"
	StatusBanned    Status = "banned"
)

// EntityState is what the panel shows.
type EntityState struct {
	Entity   Entity    `json:"entity"`
	Status   Status    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	Until    time.Time `json:"until,omitzero"`
	Since    time.Time `json:"since,omitzero"`
	Strikes  int       `json:"strikes"`
	By       string    `json:"by,omitempty"`
	Tier     string    `json:"tier,omitempty"`
	Requests int64     `json:"requests"`
	SpendUSD float64   `json:"spend_usd"`
	Credits  *float64  `json:"credits_usd,omitempty"`
	// PurchasedUSD is the accumulated credit purchases (AddCredits, add_usd),
	// the base of promote_when.purchased_usd (ADR 0051); 0 without purchases.
	PurchasedUSD float64 `json:"purchased_usd,omitempty"`
	// SpentTotalUSD is the accumulated spend, with no window (field spent of
	// ent), the base of promote_when.spend_usd and of the usage of the
	// OpenRouter key routes. Zeroed by the counter resets. Not part of the
	// admin contract.
	SpentTotalUSD float64 `json:"-"`
	// SpendDailyUSD and SpendWeeklyUSD are the spend of the current UTC day
	// and of the current UTC week (from Monday 00:00), as the OpenRouter
	// key routes report it (usage_daily, usage_weekly; task 258). Not part
	// of the admin contract.
	SpendDailyUSD  float64 `json:"-"`
	SpendWeeklyUSD float64 `json:"-"`
	// FreeRequestsToday counts the requests of the current UTC day to a
	// model whose id ends in ":free" (OpenRouter's
	// free_model_daily_requests.used; task 258). Not part of the admin
	// contract.
	FreeRequestsToday int64 `json:"-"`
	// Quota5hUSD is the quota_usd spend of the last 5 h (the window of
	// quota_5h) and Quota5hSince the time of its oldest spend in that
	// window (zero without spend), for OpenCode's usage route
	// (rolling.percent and rolling.resetsAt; task 296). The Valkey store
	// keeps minute buckets, so its Quota5hSince is the start of the minute.
	// Not part of the admin contract.
	Quota5hUSD   float64   `json:"-"`
	Quota5hSince time.Time `json:"-"`
	// QuotaWeeklyUSD and QuotaMonthlyUSD are the quota_usd debit of the
	// current UTC week (from Monday 00:00, the window of quota_weekly) and
	// of the current UTC month (the window of quota_monthly), read from the
	// windows the check denies by (qwk and win qmo in Valkey). They follow
	// the quota debit, not the cost, so a base that debits only the quota
	// (OpenCode Go, ADR 0072) reports them with no spend, for OpenCode's
	// usage route (task 432). Not part of the admin contract.
	QuotaWeeklyUSD  float64 `json:"-"`
	QuotaMonthlyUSD float64 `json:"-"`
}

// dayStart is 00:00 UTC of the day of now.
func dayStart(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// weekStart is Monday 00:00 UTC of the week of now (OpenRouter's
// usage_weekly window).
func weekStart(now time.Time) time.Time {
	d := dayStart(now)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// FreeModel reports whether model is a free variant (OpenRouter's ":free"
// suffix), counted per UTC day by Finish (EntityState.FreeRequestsToday).
func FreeModel(model string) bool { return strings.HasSuffix(model, ":free") }

// Escalation is the long-lived abuse record of an entity (Escalator).
type Escalation struct {
	// Level is how many escalations the entity had while the record lived:
	// 1 at the first, so the rung is ladder[Level-1] (the last repeats).
	Level int
	// Strikes is how many strikes the record counted, the base of
	// abuse.ban_after_strikes.
	Strikes int
}

// Escalator is implemented by stores that keep the escalation record of
// the abuse ladder apart from the strike window (spec 012, task 258): the
// strikes of strike_429.window decide when to escalate, the record decides
// the rung, so a block longer than the window does not restart the ladder.
// Escalate counts one strike and, when escalated, one escalation, renews
// the record's TTL to ttl (abuse.strike_ttl, 30 days by default) and
// returns the totals. ResetEntity with strikes drops the record; the
// counter resets keep it.
type Escalator interface {
	Escalate(ctx context.Context, e Entity, escalated bool, ttl time.Duration) (Escalation, error)
}

// Usage is the promotion history of the entity at now (ADR 0051): purchases,
// total spend, age since it was first seen and request count.
func (s EntityState) Usage(now time.Time) config.PromotionUsage {
	u := config.PromotionUsage{PurchasedUSD: s.PurchasedUSD, SpentUSD: s.SpentTotalUSD, Requests: s.Requests}
	if !s.Since.IsZero() && now.After(s.Since) {
		u.Age = now.Sub(s.Since)
	}
	return u
}

// Subject is one entity plus the limits that apply to it in this request.
type Subject struct {
	Entity Entity
	Limits config.Limits
	// Pricing is used to convert tokens into spend.
	Pricing config.Pricing
	// Window, when set, is the short interval the requests per minute are
	// evaluated over (Azure OpenAI: 1 s or 10 s; spec 006 16.6): no more
	// than RPM × Window / 1 min requests in it (at least one), refilled at
	// RPM / 60 per second. It takes the place of the rps unit (Limits.RPS
	// and Burst are ignored) and reports RPM as the requests limit.
	Window time.Duration
}

// requestRate is the rps unit of s: refill per second, bucket capacity and
// the limit reported in the headers. ok is false when s has no such unit.
func (s Subject) requestRate() (rate, burst float64, limit int, ok bool) {
	l := s.Limits
	if s.Window > 0 && l.RPM > 0 {
		rate = float64(l.RPM) / 60
		return rate, math.Max(1, rate*s.Window.Seconds()), l.RPM, true
	}
	if l.RPS <= 0 {
		return 0, 0, 0, false
	}
	burst = float64(l.Burst)
	if burst <= 0 {
		burst = float64(l.RPS)
	}
	return float64(l.RPS), burst, l.RPS, true
}

// requestRateMessage is the 429 text of the rps unit.
func (s Subject) requestRateMessage() string {
	if s.Window > 0 && s.Limits.RPM > 0 {
		return fmt.Sprintf("rate limit of %d requests per minute exceeded (evaluated every %s)", s.Limits.RPM, s.Window)
	}
	return fmt.Sprintf("rate limit of %d requests per second exceeded", s.Limits.RPS)
}

// CheckRequest is evaluated atomically on entry.
type CheckRequest struct {
	Subjects        []Subject
	EstPromptTokens int
	EstOutputTokens int // reserved (max_tokens) when TPMReserve == max_tokens
	CachedTokens    int
	Now             time.Time
	TestID          string
	RequestID       string
}

// Decision is the result of Check.
type Decision struct {
	Allowed    bool
	Kind       core.ErrorKind // why not allowed
	LimitName  string         // rps, rpm, tpm, itpm, otpm, rpd, tpd, concurrency, spend, credits, quota_5h, ...
	Entity     Entity
	RetryAfter time.Duration
	Message    string
	State      core.RateLimitState
	// Queue, when true, asks the engine to wait up to RetryAfter then retry.
	Queue bool
}

// FinishRequest adjusts counters after the response is done.
type FinishRequest struct {
	Subjects        []Subject
	RequestID       string
	Now             time.Time
	EstOutputTokens int // what was reserved
	ActualOutput    int
	ActualPrompt    int
	CachedTokens    int
	ReasoningTokens int
	CostMicroUSD    int64
	// QuotaMicroUSD, when QuotaSet, is what the quota windows (quota_usd:
	// 5 h, week, month) are debited by, independent of CostMicroUSD, which
	// still debits spend, credits and spent (ADR 0072: OpenCode Go reports
	// cost 0 and counts the allowance in the quota). Without QuotaSet the
	// quota is debited by the cost, as before. Negative counts as 0.
	QuotaMicroUSD int64
	QuotaSet      bool
	// BillingSource is the billing origin the driver reports (opencode:
	// free, balance, go, go-plus; ADR 0072). Non-empty, it goes to
	// fields.billing_source of a served request (asyncapi 0.5.0).
	BillingSource string
	// Key is the key of the event when no subject is a key: OpenCode's
	// public credential, which has no key entity (asyncapi: key "public",
	// no account; ADR 0072).
	Key        string
	Status     int
	ErrorKind  core.ErrorKind
	DurationMS int64
	Model      string
	Driver     string
	// Endpoint is the logical endpoint label of the metrics (chat, messages,
	// responses, generate_content, ...); it goes to the event (ADR 0063).
	Endpoint string
	TestID   string // value of the chaos.scope_header header, if any
	// Account is the raw id of the key's account (keys[].account), if any.
	// Empty falls back to the account subject.
	Account string
	// ToolCalls has the name of each tool call emitted, repeated per call.
	ToolCalls []string
	// InputBreakdown is the input composition sent in x-wwz-input-breakdown
	// (I16); nil when the response did not carry the header. It goes to
	// fields.input_breakdown of a served request (ADR 0032).
	InputBreakdown *InputBreakdown
	// Denial, when set, marks a request that Check refused (429/403: rate
	// limit, quota, block, ban). Finish then only publishes the error event
	// to ev:stream and touches no counter: nothing was reserved.
	Denial *Denial
	// Reason, when set, goes to fields.reason of the error event: the code
	// of a forged_request refusal (spec 020, asyncapi). Such a request has
	// no subject, so Finish publishes the event and touches no counter.
	Reason string
	// OnUsage, when set, receives the promotion history (purchased, spent,
	// age, requests) of every account subject after its counters were
	// adjusted, read in the same script (ADR 0051, README 3.5), so the
	// caller can evaluate promote_when. It is called synchronously, before
	// Finish returns, and not for a denial, for a subject whose finish was
	// skipped (store.local_first) or while the circuit of store: valkey is
	// open.
	OnUsage func(Entity, config.PromotionUsage)
}

// InputBreakdown is the input token composition of a request, with the keys
// and values of the x-wwz-input-breakdown header (spec 013, I16, ADR 0032).
// The field order is the header's.
type InputBreakdown struct {
	System      int `json:"system"`
	ToolSchemas int `json:"tool_schemas"`
	User        int `json:"user"`
	Assistant   int `json:"assistant"`
	ToolCalls   int `json:"tool_calls"`
	ToolResults int `json:"tool_results"`
	Images      int `json:"images"`
	Cached      int `json:"cached"`
	Total       int `json:"total"`
}

// Denial describes why Check refused a request (FinishRequest.Denial).
type Denial struct {
	Entity Entity // entity whose limit or status denied the request
	Limit  string // Decision.LimitName (rpm, tpm, ban, block, ...)
}

// event builds the request/error event of a FinishRequest. entity is the most
// specific subject (the key when there is one, else the IP, else the first
// subject) and key/ip carry the raw ids of both, so the panel can attribute
// the request to a key and still see the client IP (task 066). For a denial
// entity is the one that denied, key/ip are kept and the limit goes to
// fields.limit. account is the raw account id (task 075); a served request
// also carries reasoning_tokens and cached_tokens (zero included, so the panel
// can compute the cache hit rate), cost_usd when positive and tool_calls when
// any was emitted and, when the driver reports it, billing_source and
// allowance_usd (eventbus/valkey/asyncapi.yaml 0.5.0, ADR 0072).
func (req *FinishRequest) event(now time.Time) Event {
	ev := Event{Time: now, Type: "request", RequestID: req.RequestID, TestID: req.TestID, Driver: req.Driver, Endpoint: req.Endpoint, Model: req.Model, Account: req.Account, Status: req.Status, Fields: map[string]any{"duration_ms": req.DurationMS, "prompt_tokens": req.ActualPrompt, "output_tokens": req.ActualOutput}}
	if req.ErrorKind != "" {
		ev.Type = "error"
		ev.Error = string(req.ErrorKind)
	}
	var ip *Entity
	for i := range req.Subjects {
		e := &req.Subjects[i].Entity
		switch e.Kind {
		case KindKey:
			if ev.Key == "" {
				ev.Key = e.ID
				ev.Entity = &Entity{Kind: e.Kind, ID: e.ID}
			}
		case KindIP:
			if ev.IP == "" {
				ev.IP = e.ID
				ip = e
			}
		case KindAccount:
			if ev.Account == "" {
				ev.Account = e.ID
			}
		}
	}
	if ev.Key == "" {
		ev.Key = req.Key
	}
	if ev.Entity == nil && ip != nil {
		ev.Entity = &Entity{Kind: ip.Kind, ID: ip.ID}
	}
	if ev.Entity == nil && len(req.Subjects) > 0 {
		e := req.Subjects[0].Entity
		ev.Entity = &e
	}
	if d := req.Denial; d != nil {
		ev.Type = "error"
		if d.Entity.Kind != "" {
			e := d.Entity
			ev.Entity = &e
		}
		ev.Fields["denied"] = true
		if d.Limit != "" {
			ev.Fields["limit"] = d.Limit
		}
		return ev
	}
	if req.Reason != "" {
		ev.Fields["reason"] = req.Reason
	}
	ev.Fields["reasoning_tokens"] = req.ReasoningTokens
	ev.Fields["cached_tokens"] = req.CachedTokens
	ev.Fields["cost_usd"] = float64(max(req.CostMicroUSD, 0)) / 1e6
	if len(req.ToolCalls) > 0 {
		ev.Fields["tool_calls"] = append([]string(nil), req.ToolCalls...)
	}
	if b := req.InputBreakdown; b != nil {
		ev.Fields["input_breakdown"] = *b
	}
	if src := req.BillingSource; src != "" {
		ev.Fields["billing_source"] = src
		if q := req.quotaDebit(); q > 0 && (src == "go" || src == "go-plus") && req.hasQuota() {
			ev.Fields["allowance_usd"] = float64(q) / 1e6
		}
	}
	return ev
}

// quotaDebit is what the quota windows are debited by, in microdollars:
// QuotaMicroUSD with QuotaSet, else the cost (README 3.5, ARGV 13).
func (req *FinishRequest) quotaDebit() int64 {
	if req.QuotaSet {
		return max(req.QuotaMicroUSD, 0)
	}
	return max(req.CostMicroUSD, 0)
}

// quotaArg is ARGV 13 of finish: the quota debit with QuotaSet, -1 (the
// cost) without it.
func (req *FinishRequest) quotaArg() int64 {
	if req.QuotaSet {
		return max(req.QuotaMicroUSD, 0)
	}
	return -1
}

// hasQuota reports whether some subject has quota_usd, that is, whether a
// quota debit actually lands somewhere.
func (req *FinishRequest) hasQuota() bool {
	for i := range req.Subjects {
		if req.Subjects[i].Limits.QuotaUSD != nil {
			return true
		}
	}
	return false
}

// Event is one entry of the event stream consumed by the panel. Its JSON is
// the payload of eventbus/valkey/asyncapi.yaml (ADR 0019).
type Event struct {
	Time      time.Time      `json:"t"`
	Type      string         `json:"type"` // request | error | block | unblock | ban | unban | strike
	Instance  string         `json:"instance"`
	RequestID string         `json:"request_id,omitempty"`
	TestID    string         `json:"test_id,omitempty"`
	Driver    string         `json:"driver,omitempty"`
	Endpoint  string         `json:"endpoint,omitempty"` // logical endpoint, same label as the metrics (ADR 0063)
	Model     string         `json:"model,omitempty"`
	Entity    *Entity        `json:"entity,omitempty"`
	Key       string         `json:"key,omitempty"`     // raw key id of the request (request and error)
	Account   string         `json:"account,omitempty"` // raw account id of the key (request and error)
	IP        string         `json:"ip,omitempty"`      // client IP of the request (request and error)
	Status    int            `json:"status,omitempty"`
	Error     string         `json:"error,omitempty"`
	Sample    float64        `json:"sample,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// Store is the control-state backend.
type Store interface {
	Check(ctx context.Context, req CheckRequest) (Decision, error)
	Finish(ctx context.Context, req FinishRequest) error
	// Block applies a temporary block; zero duration means until Unblock.
	Block(ctx context.Context, e Entity, reason string, d time.Duration, by string) error
	Unblock(ctx context.Context, e Entity) error
	Ban(ctx context.Context, e Entity, reason string, by string) error
	Unban(ctx context.Context, e Entity) error
	Strike(ctx context.Context, e Entity, reason string, ttl time.Duration) (int, error)
	Get(ctx context.Context, e Entity) (EntityState, error)
	List(ctx context.Context, status Status, limit int) ([]EntityState, error)
	// Attempts increments and returns the attempt counter of a scenario step
	// for a conversation (fail_first support).
	Attempts(ctx context.Context, conversation, step string, ttl time.Duration) (int, error)
	// SetCredits sets a credit balance in USD.
	SetCredits(ctx context.Context, e Entity, usd float64) error
	// Reset clears counters (admin/reset).
	Reset(ctx context.Context) error
	// Events returns recent events, newest first.
	Events(ctx context.Context, limit int) ([]Event, error)
	Kind() string
	Close() error
}

// CreditAdder is implemented by stores that can simulate a credit purchase
// (POST /admin/entities/{tipo}/{id}/credits with add_usd, ADR 0051): usd
// (> 0) is added to the balance (created at 0 when absent) and to the
// accumulated purchases. It returns the resulting state; its Usage feeds the
// promote_when evaluation.
type CreditAdder interface {
	AddCredits(ctx context.Context, e Entity, usd float64) (EntityState, error)
}

// TierSwapper is implemented by stores that can assign a tier with
// compare-and-swap (automatic promotion, ADR 0051): the ent:<tipo>:<id>
// field of the cluster policies becomes tier only if it still holds expected
// ("" = absent), and then everything SetTier does follows (display field,
// version + 1, cfg:changed, local watchers before it returns). swapped is
// false when another writer got there first; nothing is written then.
type TierSwapper interface {
	SwapTier(ctx context.Context, e Entity, expected, tier string) (swapped bool, err error)
}

// checkPurchase validates the amount of AddCredits.
func checkPurchase(usd float64) error {
	if !(usd > 0) || math.IsInf(usd, 1) {
		return fmt.Errorf("limits: credits to add must be a positive amount, got %v", usd)
	}
	return nil
}

// CounterResetter is implemented by stores that can zero rate-limit counters
// while keeping blocks, bans, strikes and credits (POST /admin/reset with
// scope "limits"). Stores without it fall back to Reset.
type CounterResetter interface {
	ResetCounters(ctx context.Context) error
}

// ConversationResetter is implemented by stores that keep scenario attempt
// counters (fail_first) per conversation. An empty conversation resets all of
// them. It returns how many conversations were reset (may be an estimate).
type ConversationResetter interface {
	ResetConversations(ctx context.Context, conversation string) (int, error)
}

// ResetCounters zeroes rate-limit windows, spend and request counters of
// every entity, keeping status (block/ban), strikes, credits and in-flight
// concurrency so running requests still release their slot.
func (m *Memory) ResetCounters(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for e, r := range m.ents {
		m.ents[e] = &entityRec{entity: r.entity, since: r.since, block: r.block, ban: r.ban, conc: r.conc, credits: r.credits, purchased: r.purchased, strikes: r.strikes, esc: r.esc, tier: r.tier}
	}
	return nil
}

// ResetConversations drops the attempt counters of one conversation (or of
// all of them when conversation is empty).
func (m *Memory) ResetConversations(_ context.Context, conversation string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if conversation == "" {
		convs := map[string]struct{}{}
		for k := range m.attempts {
			c, _, _ := strings.Cut(k, "\x00")
			convs[c] = struct{}{}
		}
		m.attempts = map[string]attempt{}
		return len(convs), nil
	}
	n := 0
	prefix := conversation + "\x00"
	for k := range m.attempts {
		if strings.HasPrefix(k, prefix) {
			delete(m.attempts, k)
			n = 1
		}
	}
	return n, nil
}

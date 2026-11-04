package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// maxAdminBody bounds what the panel reads from one admin response.
const maxAdminBody = 32 << 20

// Problem is the error envelope of the wwz control plane.
type Problem struct {
	Error   string   `json:"error"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

// AdminError is a failed call to the wwz admin API.
type AdminError struct {
	Method, Path string
	// Status is the HTTP status of wwz; 0 when the request did not get an
	// answer (connection refused, timeout, bad body).
	Status  int
	Problem Problem
	Err     error
}

func (e *AdminError) Error() string {
	switch {
	case e.Status == 0:
		return fmt.Sprintf("source: admin %s %s: %v", e.Method, e.Path, e.Err)
	case e.Problem.Message != "":
		return fmt.Sprintf("source: admin %s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Problem.Error, e.Problem.Message)
	default:
		return fmt.Sprintf("source: admin %s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
}

func (e *AdminError) Unwrap() error { return e.Err }

// HTTPStatus is what the panel API answers for this error (spec 016,
// "Ações"): 400 and 404 of wwz are passed through, anything else is 502.
func (e *AdminError) HTTPStatus() int {
	if e.Status == http.StatusBadRequest || e.Status == http.StatusNotFound {
		return e.Status
	}
	return http.StatusBadGateway
}

// HTTPStatus maps any source error to the panel API status: AdminError per
// its rule, ErrInvalidPolicy 400, ErrPolicyConflict and ErrValkeyOff 409,
// anything else 502.
func HTTPStatus(err error) int {
	var ae *AdminError
	switch {
	case errors.As(err, &ae):
		return ae.HTTPStatus()
	case errors.Is(err, ErrInvalidPolicy):
		return http.StatusBadRequest
	case errors.Is(err, ErrPolicyConflict), errors.Is(err, ErrValkeyOff):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

// Admin is the client of the wwz control plane (cmd/wwz/openapi.yaml),
// authenticated with WWZ_ADMIN_TOKEN.
type Admin struct {
	base  string
	token string
	hc    *http.Client
}

// NewAdmin builds the client for baseURL (WWZ_PANEL_WWZ_ADMIN_URL). hc nil =
// a client with a 10 s timeout.
func NewAdmin(baseURL, token string, hc *http.Client) *Admin {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Admin{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

// BaseURL is the admin URL in use.
func (a *Admin) BaseURL() string { return a.base }

// raw sends the request and returns the body of a 2xx answer.
func (a *Admin) raw(ctx context.Context, method, path, ctype string, body []byte) ([]byte, error) {
	fail := func(status int, err error) *AdminError {
		return &AdminError{Method: method, Path: path, Status: status, Err: err}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return nil, fail(0, err)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fail(0, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminBody))
	if err != nil {
		return nil, fail(0, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := fail(resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode))
		_ = json.Unmarshal(data, &e.Problem)
		return nil, e
	}
	return data, nil
}

// call sends in (JSON, nil = no body) and decodes the answer into out (nil
// = ignore).
func (a *Admin) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	ctype := ""
	if in != nil {
		body, _ = json.Marshal(in)
		ctype = "application/json"
	}
	data, err := a.raw(ctx, method, path, ctype, body)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &AdminError{Method: method, Path: path, Err: fmt.Errorf("resposta inválida: %w", err)}
	}
	return nil
}

// Ping is GET /health (state of /health/ready).
func (a *Admin) Ping(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, "/health", nil, nil)
}

// ---- stats ----

// Stats is GET /admin/stats.
type Stats struct {
	Started time.Time    `json:"started"`
	UptimeS float64      `json:"uptime_s"`
	ResetAt time.Time    `json:"reset_at,omitzero"`
	Pod     string       `json:"pod,omitempty"`
	Store   string       `json:"store"`
	Totals  StatsTotals  `json:"totals"`
	ByModel []ModelStats `json:"by_model"`
	ByKey   []KeyStats   `json:"by_key,omitempty"`
}

// StatsTotals are the cluster-instance totals of Stats.
type StatsTotals struct {
	Requests           int64            `json:"requests"`
	InFlight           int64            `json:"in_flight"`
	StreamsOpen        int64            `json:"streams_open"`
	ErrorsInjected     int64            `json:"errors_injected"`
	RateLimited        int64            `json:"rate_limited"`
	Blocked            int64            `json:"blocked"`
	ByStatus           map[string]int64 `json:"by_status,omitempty"`
	ErrorsByKind       map[string]int64 `json:"errors_by_kind,omitempty"`
	ErrorsByPhase      map[string]int64 `json:"errors_by_phase,omitempty"`
	RateLimitedByLimit map[string]int64 `json:"rate_limited_by_limit,omitempty"`
}

// ModelStats is one driver/model row of Stats.
type ModelStats struct {
	Driver    string           `json:"driver"`
	Model     string           `json:"model"`
	Requests  int64            `json:"requests"`
	Errors    int64            `json:"errors,omitempty"`
	TTFTP50MS *float64         `json:"ttft_p50_ms,omitempty"`
	TTFTP99MS *float64         `json:"ttft_p99_ms,omitempty"`
	ITLP50MS  *float64         `json:"itl_p50_ms,omitempty"`
	ITLP99MS  *float64         `json:"itl_p99_ms,omitempty"`
	E2EP50MS  *float64         `json:"e2e_p50_ms,omitempty"`
	E2EP99MS  *float64         `json:"e2e_p99_ms,omitempty"`
	Tokens    map[string]int64 `json:"tokens,omitempty"`
}

// KeyStats is one key row of Stats.
type KeyStats struct {
	Key      string        `json:"key"`
	Status   limits.Status `json:"status,omitempty"`
	Requests int64         `json:"requests"`
	SpendUSD float64       `json:"spend_usd,omitempty"`
}

// Stats is GET /admin/stats.
func (a *Admin) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := a.call(ctx, http.MethodGet, "/admin/stats", nil, &s)
	return s, err
}

// ---- entities ----

func entityPath(e limits.Entity, action string) string {
	p := "/admin/entities/" + url.PathEscape(string(e.Kind)) + "/" + url.PathEscape(e.ID)
	if action != "" {
		p += "/" + action
	}
	return p
}

// Entities is GET /admin/entities?status=&limit= (status "" = all, limit
// <= 0 = default of wwz).
func (a *Admin) Entities(ctx context.Context, status limits.Status, limit int) ([]limits.EntityState, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", string(status))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/admin/entities"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Entities []limits.EntityState `json:"entities"`
	}
	if err := a.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Entities == nil {
		out.Entities = []limits.EntityState{}
	}
	return out.Entities, nil
}

// Entity is GET /admin/entities/{tipo}/{id}.
func (a *Admin) Entity(ctx context.Context, e limits.Entity) (limits.EntityState, error) {
	var st limits.EntityState
	err := a.call(ctx, http.MethodGet, entityPath(e, ""), nil, &st)
	return st, err
}

func (a *Admin) entityAction(ctx context.Context, e limits.Entity, action string, in any) (limits.EntityState, error) {
	var st limits.EntityState
	err := a.call(ctx, http.MethodPost, entityPath(e, action), in, &st)
	return st, err
}

// BlockRequest is the body of POST .../block.
type BlockRequest struct {
	Reason string
	// Duration 0 blocks until unblock.
	Duration time.Duration
	By       string
}

// Block is POST /admin/entities/{tipo}/{id}/block.
func (a *Admin) Block(ctx context.Context, e limits.Entity, r BlockRequest) (limits.EntityState, error) {
	body := map[string]string{}
	if r.Reason != "" {
		body["reason"] = r.Reason
	}
	if r.Duration > 0 {
		body["duration"] = r.Duration.String()
	}
	if r.By != "" {
		body["by"] = r.By
	}
	return a.entityAction(ctx, e, "block", body)
}

// Unblock is POST /admin/entities/{tipo}/{id}/unblock.
func (a *Admin) Unblock(ctx context.Context, e limits.Entity) (limits.EntityState, error) {
	return a.entityAction(ctx, e, "unblock", nil)
}

// Ban is POST /admin/entities/{tipo}/{id}/ban.
func (a *Admin) Ban(ctx context.Context, e limits.Entity, reason, by string) (limits.EntityState, error) {
	body := map[string]string{}
	if reason != "" {
		body["reason"] = reason
	}
	if by != "" {
		body["by"] = by
	}
	return a.entityAction(ctx, e, "ban", body)
}

// Unban is POST /admin/entities/{tipo}/{id}/unban.
func (a *Admin) Unban(ctx context.Context, e limits.Entity) (limits.EntityState, error) {
	return a.entityAction(ctx, e, "unban", nil)
}

// SetCredits is POST /admin/entities/{tipo}/{id}/credits (replaces the
// balance).
func (a *Admin) SetCredits(ctx context.Context, e limits.Entity, usd float64) (limits.EntityState, error) {
	return a.entityAction(ctx, e, "credits", map[string]float64{"usd": usd})
}

// AddCredits is POST /admin/entities/{tipo}/{id}/credits with add_usd: it
// simulates a purchase (adds to the balance and to purchased_usd, the base
// of promote_when; ADR 0051).
func (a *Admin) AddCredits(ctx context.Context, e limits.Entity, usd float64) (limits.EntityState, error) {
	return a.entityAction(ctx, e, "credits", map[string]float64{"add_usd": usd})
}

// SetTier is POST /admin/entities/{tipo}/{id}/tier; "" removes the
// override (ADR 0025).
func (a *Admin) SetTier(ctx context.Context, e limits.Entity, tier string) (limits.EntityState, error) {
	return a.entityAction(ctx, e, "tier", map[string]string{"tier": tier})
}

// Reset scopes of POST .../reset.
const (
	ResetCounters = "counters"
	ResetStrikes  = "strikes"
)

// Reset is POST /admin/entities/{tipo}/{id}/reset; no scope = the default of
// wwz (counters). Forgiving strikes is Reset(ctx, e, ResetStrikes).
func (a *Admin) Reset(ctx context.Context, e limits.Entity, scope ...string) (limits.EntityState, error) {
	var body any
	if len(scope) > 0 {
		body = map[string][]string{"scope": scope}
	}
	return a.entityAction(ctx, e, "reset", body)
}

// LeakRequest is the body of POST /admin/leak (Label or Key required).
type LeakRequest struct {
	Label  string `json:"label,omitempty"`
	Key    string `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// LeakResult is the answer of POST /admin/leak.
type LeakResult struct {
	Key     limits.EntityState  `json:"key"`
	Account *limits.EntityState `json:"account,omitempty"`
}

// Leak is POST /admin/leak.
func (a *Admin) Leak(ctx context.Context, r LeakRequest) (LeakResult, error) {
	var out LeakResult
	err := a.call(ctx, http.MethodPost, "/admin/leak", r, &out)
	return out, err
}

// ---- chaos, scenarios, config ----

// StartChaos is POST /admin/chaos with a ChaosRequest of the openapi (map or
// struct); returns the ChaosEvent as JSON.
func (a *Admin) StartChaos(ctx context.Context, req any) (json.RawMessage, error) {
	var out json.RawMessage
	err := a.call(ctx, http.MethodPost, "/admin/chaos", req, &out)
	return out, err
}

// ListChaos is GET /admin/chaos (ChaosList as JSON).
func (a *Admin) ListChaos(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := a.call(ctx, http.MethodGet, "/admin/chaos", nil, &out)
	return out, err
}

// StopChaos is DELETE /admin/chaos (id and testID "" = all); returns how
// many events stopped.
func (a *Admin) StopChaos(ctx context.Context, id, testID string) (int, error) {
	q := url.Values{}
	if id != "" {
		q.Set("id", id)
	}
	if testID != "" {
		q.Set("test_id", testID)
	}
	path := "/admin/chaos"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Stopped int `json:"stopped"`
	}
	err := a.call(ctx, http.MethodDelete, path, nil, &out)
	return out.Stopped, err
}

// ScenarioSummary is one item of GET /admin/scenarios.
type ScenarioSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       int    `json:"steps"`
	OnNoMatch   string `json:"on_no_match,omitempty"`
	OnExhausted string `json:"on_exhausted,omitempty"`
}

// ScenarioLoaded is the answer of POST /admin/scenarios.
type ScenarioLoaded struct {
	Loaded   []string `json:"loaded"`
	Replaced []string `json:"replaced,omitempty"`
}

// Scenarios is GET /admin/scenarios.
func (a *Admin) Scenarios(ctx context.Context) ([]ScenarioSummary, error) {
	var out struct {
		Scenarios []ScenarioSummary `json:"scenarios"`
	}
	if err := a.call(ctx, http.MethodGet, "/admin/scenarios", nil, &out); err != nil {
		return nil, err
	}
	if out.Scenarios == nil {
		out.Scenarios = []ScenarioSummary{}
	}
	return out.Scenarios, nil
}

// LoadScenarios is POST /admin/scenarios with a YAML body (one or more
// documents); validation errors come back as AdminError with status 400 and
// Problem.Details.
func (a *Admin) LoadScenarios(ctx context.Context, yaml []byte) (ScenarioLoaded, error) {
	var out ScenarioLoaded
	const path = "/admin/scenarios"
	data, err := a.raw(ctx, http.MethodPost, path, "application/yaml", yaml)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, &AdminError{Method: http.MethodPost, Path: path, Err: fmt.Errorf("resposta inválida: %w", err)}
	}
	return out, nil
}

// DeleteScenario is DELETE /admin/scenarios?name=.
func (a *Admin) DeleteScenario(ctx context.Context, name string) error {
	return a.call(ctx, http.MethodDelete, "/admin/scenarios?"+url.Values{"name": {name}}.Encode(), nil, nil)
}

// Config is GET /admin/config (effective document, secrets masked).
func (a *Admin) Config(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := a.call(ctx, http.MethodGet, "/admin/config", nil, &out)
	return out, err
}

// PatchConfig is POST /admin/config with a merge patch (RFC 7396); returns
// the resulting effective document.
func (a *Admin) PatchConfig(ctx context.Context, patch any) (json.RawMessage, error) {
	var out struct {
		Config json.RawMessage `json:"config"`
	}
	err := a.call(ctx, http.MethodPost, "/admin/config", patch, &out)
	return out.Config, err
}

// ---- requests ----

// RequestRecord is one item of GET /admin/requests (engine.Record).
type RequestRecord struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Driver           string    `json:"driver"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"`
	Status           int       `json:"status"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMS           int64     `json:"ttft_ms,omitempty"`
	ITLMS            float64   `json:"itl_ms,omitempty"`
	IP               string    `json:"ip,omitempty"`
	Key              string    `json:"key,omitempty"`
	Stream           bool      `json:"stream"`
	TestID           string    `json:"test_id,omitempty"`
	Scenario         string    `json:"scenario,omitempty"`
	Step             string    `json:"step,omitempty"`
	Attempt          int       `json:"attempt,omitempty"`
	Error            string    `json:"error,omitempty"`
	MidStream        string    `json:"mid_stream,omitempty"`
	ToolBreak        string    `json:"tool_break,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	ReasoningTokens  int64     `json:"reasoning_tokens,omitempty"`
	ToolCalls        []string  `json:"tool_calls,omitempty"`
	FinishReason     string    `json:"finish_reason,omitempty"`
	Seed             int64     `json:"seed,omitempty"`
}

// RequestList is GET /admin/requests.
type RequestList struct {
	// Enabled is admin.debug of wwz; false = empty list (the screen warns).
	Enabled  bool            `json:"enabled"`
	Requests []RequestRecord `json:"requests"`
}

// Requests is GET /admin/requests?last= (last <= 0 = default of wwz).
func (a *Admin) Requests(ctx context.Context, last int) (RequestList, error) {
	path := "/admin/requests"
	if last > 0 {
		path += "?last=" + strconv.Itoa(last)
	}
	var out RequestList
	err := a.call(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

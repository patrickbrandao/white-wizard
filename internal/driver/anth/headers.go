package anth

import (
	"encoding/binary"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/driver/ids"
)

// The limits of Anthropic's default tier, observed on 2026-10-31 (ADR
// 0049): the anthropic-ratelimit-* values when no limit is configured.
const (
	DefaultRequestsLimit     = 10_000
	DefaultInputTokensLimit  = 10_000_000
	DefaultOutputTokensLimit = 2_000_000
	DefaultTokensLimit       = 12_000_000
	rateWindow               = time.Minute
)

// AnthropicRateLimitHeaders renders anthropic-ratelimit-* with RFC 3339
// resets for the families rl reports.
func AnthropicRateLimitHeaders(rl *core.RateLimitState) map[string]string {
	return rateLimitHeaders(rl, time.Now())
}

func rateLimitHeaders(rl *core.RateLimitState, now time.Time) map[string]string {
	m := map[string]string{}
	now = now.UTC()
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	if rl.RequestsLimit > 0 {
		m["anthropic-ratelimit-requests-limit"] = strconv.Itoa(rl.RequestsLimit)
		m["anthropic-ratelimit-requests-remaining"] = strconv.Itoa(max(0, rl.RequestsRemaining))
		m["anthropic-ratelimit-requests-reset"] = rfc(rl.RequestsReset)
	}
	if rl.TokensLimit > 0 {
		m["anthropic-ratelimit-tokens-limit"] = strconv.Itoa(rl.TokensLimit)
		m["anthropic-ratelimit-tokens-remaining"] = strconv.Itoa(roundThousand(rl.TokensRemaining))
		m["anthropic-ratelimit-tokens-reset"] = rfc(rl.TokensReset)
	}
	if rl.InputTokensLimit > 0 {
		m["anthropic-ratelimit-input-tokens-limit"] = strconv.Itoa(rl.InputTokensLimit)
		m["anthropic-ratelimit-input-tokens-remaining"] = strconv.Itoa(roundThousand(rl.InputTokensRemain))
		m["anthropic-ratelimit-input-tokens-reset"] = rfc(rl.TokensReset)
	}
	if rl.OutputTokensLimit > 0 {
		m["anthropic-ratelimit-output-tokens-limit"] = strconv.Itoa(rl.OutputTokensLimit)
		m["anthropic-ratelimit-output-tokens-remaining"] = strconv.Itoa(roundThousand(rl.OutputTokensRem))
		m["anthropic-ratelimit-output-tokens-reset"] = rfc(rl.TokensReset)
	}
	return m
}

func roundThousand(n int) int {
	if n < 0 {
		return 0
	}
	return (n / 1000) * 1000
}

// DefaultRateLimit fills the families rl does not report with the default
// tier (ADR 0049). This request already counts against requests (remaining
// = limit - 1, reset = the refill of one request); the token buckets show
// the whole limit, as observed (13 input tokens left the 10 000 000
// remaining untouched after the rounding to the thousand; the exact rule
// is not conferred). The values are shown, not enforced.
func DefaultRateLimit(rl *core.RateLimitState) core.RateLimitState {
	var out core.RateLimitState
	if rl != nil {
		out = *rl
	}
	if out.RequestsLimit <= 0 {
		out.RequestsLimit = DefaultRequestsLimit
		out.RequestsRemaining = DefaultRequestsLimit - 1
		out.RequestsReset = rateWindow / DefaultRequestsLimit
	}
	if out.TokensLimit <= 0 {
		out.TokensLimit, out.TokensRemaining = DefaultTokensLimit, DefaultTokensLimit
	}
	if out.InputTokensLimit <= 0 {
		out.InputTokensLimit, out.InputTokensRemain = DefaultInputTokensLimit, DefaultInputTokensLimit
	}
	if out.OutputTokensLimit <= 0 {
		out.OutputTokensLimit, out.OutputTokensRem = DefaultOutputTokensLimit, DefaultOutputTokensLimit
	}
	out.Present = true
	return out
}

// AnthropicSuccessHeaders are the headers of every Messages 200, body and
// stream (spec 006 16.7): the account (anthropic-organization-id,
// anthropic-workspace-id) and the 12 anthropic-ratelimit-*, always. Resets
// are RFC 3339 truncated to the second.
func AnthropicSuccessHeaders(req *core.Request, _ *core.Response, rl *core.RateLimitState) map[string]string {
	st := DefaultRateLimit(rl)
	m := rateLimitHeaders(&st, time.Now())
	for k, v := range AnthropicAccountHeaders(req) {
		m[k] = v
	}
	return m
}

// AnthropicAccountHeaders are anthropic-organization-id (a UUID) and
// anthropic-workspace-id ("wrkspc_01" + 22 base58), stable per account
// (ADR 0049): derived from keys[].account and keys[].project when the key
// has an account, else from the key as presented.
func AnthropicAccountHeaders(req *core.Request) map[string]string {
	return map[string]string{
		"anthropic-organization-id": OrganizationID(req),
		"anthropic-workspace-id":    WorkspaceID(req),
	}
}

// OrganizationID is the anthropic-organization-id of req's account, also
// quoted in the 429 text.
func OrganizationID(req *core.Request) string {
	a := driver.AccountOf(req)
	seed := a.Account
	if seed == "" {
		seed = "key\x00" + presentedKey(req, a)
	}
	return uuid(derive(seed, "organization", 16))
}

// WorkspaceID is the anthropic-workspace-id of req's account and project.
func WorkspaceID(req *core.Request) string {
	a := driver.AccountOf(req)
	seed := a.Account + "\x00" + a.Project
	if a.Account == "" {
		seed = "key\x00" + presentedKey(req, a)
	}
	b := derive(seed, "workspace", 22)
	out := make([]byte, 0, 31)
	out = append(out, "wrkspc_01"...)
	for _, c := range b {
		out = append(out, base58[int(c)%len(base58)])
	}
	return string(out)
}

const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// presentedKey is the key the engine recorded, else the one in the
// request's x-api-key or Authorization header.
func presentedKey(req *core.Request, a driver.Account) string {
	if a.Key != "" || req == nil {
		return a.Key
	}
	if k := req.Header("x-api-key"); k != "" {
		return k
	}
	auth := req.Header("authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// derive draws n bytes from a hash of salt and seed.
func derive(seed, salt string, n int) []byte {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(seed))
	r := rand.New(rand.NewPCG(h.Sum64(), 0x9e3779b97f4a7c15))
	b := make([]byte, 0, n+8)
	for len(b) < n {
		b = binary.LittleEndian.AppendUint64(b, r.Uint64())
	}
	return b[:n]
}

const hexDigits = "0123456789abcdef"

// uuid formats 16 bytes as a version 4 UUID.
func uuid(u []byte) string {
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	b := make([]byte, 0, 36)
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b = append(b, '-')
		}
		b = append(b, hexDigits[c>>4], hexDigits[c&0xF])
	}
	return string(b)
}

// reqID is the request-id value: the driver-minted "req_011C…" as is,
// "req_" + the engine's opaque id for drivers that do not mint ids, or a
// fresh Anthropic id when the request has none (parse errors, /models).
func reqID(req *core.Request) string {
	if req != nil && req.RequestID != "" {
		if strings.HasPrefix(req.RequestID, "req_") {
			return req.RequestID
		}
		return "req_" + req.RequestID
	}
	return NewRequestID()
}

// NewRequestID is a fresh request-id, "req_011C" + 20 base62 like the
// driver's minted ones.
func NewRequestID() string {
	return ids.AnthropicRequest(ids.FromToken(strconv.FormatUint(rand.Uint64(), 36)))
}

// requestIDHeader is the header that carries the request id for the
// driver named h.Name: request-id on Anthropic, none on the gateways
// (driver.Conventions, spec 006 16.7).
func (h *Hooks) requestIDHeader() string {
	return driver.DefaultConventions(h.Name).RequestIDHeader
}

// shouldRetry is x-should-retry for a status: false on the client errors
// the SDKs do not retry (observed on 400, 401 and 404), true on 408, 409,
// 429 and 5xx (not conferred; the SDKs retry those).
func shouldRetry(status int) string {
	if status == 408 || status == 409 || status == 429 || status >= 500 {
		return "true"
	}
	return "false"
}

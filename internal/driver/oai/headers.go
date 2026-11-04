package oai

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

// OpenAIVersion is the openai-version header of every success.
const OpenAIVersion = "2020-10-01"

// The limits of OpenAI's default tier, observed on 2026-10-31 (ADR 0049):
// the x-ratelimit-* values when no limit is configured.
const (
	DefaultRequestsLimit = 5000
	DefaultTokensLimit   = 2_000_000
	rateWindow           = time.Minute
)

// OpenAIRateLimitHeaders renders x-ratelimit-* with Go-style durations.
func OpenAIRateLimitHeaders(rl *core.RateLimitState) map[string]string {
	// Sized for the nine x-ratelimit-* plus OpenAISuccessHeaders' four, so
	// the map never grows (task 270).
	m := make(map[string]string, 13)
	if rl.RequestsLimit > 0 {
		m["x-ratelimit-limit-requests"] = strconv.Itoa(rl.RequestsLimit)
		m["x-ratelimit-remaining-requests"] = strconv.Itoa(max(0, rl.RequestsRemaining))
		m["x-ratelimit-reset-requests"] = goDuration(rl.RequestsReset)
	}
	if rl.TokensLimit > 0 {
		m["x-ratelimit-limit-tokens"] = strconv.Itoa(rl.TokensLimit)
		m["x-ratelimit-remaining-tokens"] = strconv.Itoa(max(0, rl.TokensRemaining))
		m["x-ratelimit-reset-tokens"] = goDuration(rl.TokensReset)
	}
	// With a project tpm (projects[].limits.tpm) the real adds the
	// project-scoped tokens unit ("Project-token headers may be present
	// when a project-scoped token limit applies"; rate-limits guide, not
	// conferred on the real; spec 010, task 234).
	if rl.ProjectTokensLimit > 0 {
		m["x-ratelimit-limit-project-tokens"] = strconv.Itoa(rl.ProjectTokensLimit)
		m["x-ratelimit-remaining-project-tokens"] = strconv.Itoa(max(0, rl.ProjectTokensRemain))
		m["x-ratelimit-reset-project-tokens"] = goDuration(rl.ProjectTokensReset)
	}
	return m
}

// DefaultRateLimit fills the families rl does not report with the default
// tier (ADR 0049): this request already counts (remaining = limit - 1 and
// limit - the token estimate) and each family refills at limit per minute,
// so reset = window * consumed / limit (observed 12ms and 0s). The values
// are shown, not enforced.
func DefaultRateLimit(req *core.Request, rl *core.RateLimitState) core.RateLimitState {
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
		est := EstimateTokens(req)
		out.TokensLimit = DefaultTokensLimit
		out.TokensRemaining = DefaultTokensLimit - est
		out.TokensReset = rateWindow * time.Duration(est) / DefaultTokensLimit
	}
	out.Present = true
	return out
}

// EstimateTokens is the rate limiter's guess of the prompt: a quarter of
// the characters of system and messages, rounded up, plus 2. The real
// rule is not conferred; this one reproduces the one sample (a 15
// character message counted 6, ADR 0049).
func EstimateTokens(req *core.Request) int {
	if req == nil {
		return 0
	}
	n := utf8.RuneCountInString(req.System)
	for _, m := range req.Messages {
		n += utf8.RuneCountInString(m.Content)
	}
	return (n+3)/4 + 2
}

// OpenAISuccessHeaders are the headers of every OpenAI 200, body and stream
// (spec 006 16.7): openai-organization and openai-project of the account,
// openai-processing-ms, openai-version and the six x-ratelimit-*, always.
func OpenAISuccessHeaders(req *core.Request, _ *core.Response, rl *core.RateLimitState) map[string]string {
	st := DefaultRateLimit(req, rl)
	m := OpenAIRateLimitHeaders(&st)
	org, project := AccountHeaders(req)
	m["openai-organization"] = org
	m["openai-project"] = project
	m["openai-processing-ms"] = ProcessingMS(req)
	m["openai-version"] = OpenAIVersion
	return m
}

// ProcessingMS is openai-processing-ms: whole milliseconds since the
// request arrived.
func ProcessingMS(req *core.Request) string {
	if req == nil || req.Received.IsZero() {
		return "0"
	}
	return strconv.FormatInt(max(0, time.Since(req.Received).Milliseconds()), 10)
}

const (
	lowerAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	base62     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// AccountHeaders are openai-organization and openai-project (ADR 0049).
// With an account (driver.AccountOf: keys[].account and keys[].project) the
// organization is the lower-case slug of the account and the project
// "proj_" + 24 alphanumerics stable per account and project; without one
// (auth: none, a key outside keys[]) the organization is "user-" + 24
// characters and the project "proj_" + 24, stable per presented key. An
// openai-project the client sent wins, as on the real API.
func AccountHeaders(req *core.Request) (org, project string) {
	a := driver.AccountOf(req)
	key := ""
	if req != nil {
		key = presentedKey(req)
		project = req.Header("openai-project")
	}
	if org = Slug(a.Account); org != "" {
		if project == "" {
			project = deriveID("proj_", a.Account+"\x00"+a.Project, "project", base62, 24)
		}
		return org, project
	}
	org = deriveID("user-", key, "organization", lowerAlnum, 24)
	if project == "" {
		project = deriveID("proj_", key, "project", base62, 24)
	}
	return org, project
}

// OrganizationID is the organization the 429 text names ("in organization
// org-…"): on the real API the organization id, "org-" + 24 alphanumerics.
// It is stable per account (driver.AccountOf); a key outside keys[] belongs
// to no account and reads org-wwz.
func OrganizationID(req *core.Request) string {
	a := driver.AccountOf(req)
	if Slug(a.Account) == "" {
		return "org-wwz"
	}
	return deriveID("org-", a.Account, "organization-id", base62, 24)
}

// Slug lower-cases s and turns every run of characters other than ASCII
// letters and digits into one "-", trimmed at both ends.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	return b.String()
}

// derive draws n characters of alphabet from a hash of salt and seed.
func derive(seed, salt, alphabet string, n int) string { return deriveID("", seed, salt, alphabet, n) }

// deriveID is prefix + derive(seed, salt, alphabet, n), built on the stack:
// the PCG (behind rand.Source) and the string are its only allocations
// (task 270).
func deriveID(prefix, seed, salt, alphabet string, n int) string {
	h := fnv64a(fnv64a(fnvOffset64, salt)*fnvPrime64, seed) // salt, NUL, seed
	var pcg rand.PCG
	pcg.Seed(h, 0x9e3779b97f4a7c15)
	r := rand.New(&pcg)
	var buf [64]byte
	b := append(buf[:0], prefix...)
	for range n {
		b = append(b, alphabet[r.IntN(len(alphabet))])
	}
	return string(b)
}

// FNV-64a without hash.Hash64, which escapes to the heap.
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnv64a continues the FNV-64a hash h over s.
func fnv64a(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// NewRequestID is a fresh x-request-id: "req_" + 32 hex.
func NewRequestID() string {
	b := make([]byte, 4, 36)
	copy(b, "req_")
	for range 4 {
		v := rand.Uint64()
		for range 8 {
			b = append(b, hexDigits[v&0xF])
			v >>= 4
		}
	}
	return string(b)
}

// NewUUID is a random UUID (version 4), the x-request-id of /v1/models.
func NewUUID() string {
	var u [16]byte
	binary.LittleEndian.PutUint64(u[:8], rand.Uint64())
	binary.LittleEndian.PutUint64(u[8:], rand.Uint64())
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

// obfuscator draws the obfuscation strings of one stream: base62 of a
// varying length (observed 5 to 11), reproducible from the response id.
// The source lives in the struct: one allocation per stream (task 270).
type obfuscator struct {
	r   rand.Rand
	pcg rand.PCG
}

func newObfuscator(id string) *obfuscator {
	o := &obfuscator{}
	o.pcg.Seed(fnv64a(fnvOffset64, id), 0x5851f42d4c957f2d)
	o.r = *rand.New(&o.pcg)
	return o
}

func (o *obfuscator) next() string {
	var b [16]byte
	return string(o.appendNext(b[:0]))
}

// appendNext appends the next string to dst, drawing exactly as next does.
func (o *obfuscator) appendNext(dst []byte) []byte {
	n := 4 + o.r.IntN(8)
	for range n {
		dst = append(dst, base62[o.r.IntN(len(base62))])
	}
	return dst
}

// includeObfuscation reads stream_options.include_obfuscation (default
// true) from the raw body.
func includeObfuscation(req *core.Request) bool {
	if req == nil || !bytes.Contains(req.Raw, []byte("include_obfuscation")) {
		return true
	}
	var b struct {
		StreamOptions struct {
			IncludeObfuscation *bool `json:"include_obfuscation"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(req.Raw, &b) != nil || b.StreamOptions.IncludeObfuscation == nil {
		return true
	}
	return *b.StreamOptions.IncludeObfuscation
}

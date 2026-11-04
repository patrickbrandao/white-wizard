// Package chaos decides which failures to inject into a request: errors
// before the first byte, faults in the middle of the stream, broken tool
// calls and content moderation. Everything is drawn from the per-request RNG
// so a seeded run reproduces the same chaos.
package chaos

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/generate"
	"github.com/patrickbrandao/white-wizard/internal/toolargs"
)

// PreTypes are the error kinds that can be injected before the first byte.
var PreTypes = []core.ErrorKind{
	core.ErrInvalidRequest, core.ErrContextLength, core.ErrInvalidAPIKey, core.ErrPermission,
	core.ErrModelNotFound, core.ErrRequestTooLarge, core.ErrRateLimit, core.ErrQuota,
	core.ErrServerError, core.ErrBadGateway, core.ErrOverloaded, core.ErrGatewayTimeout,
	core.ErrContentFilter, core.ErrSlowDown,
}

// TransportTypes are non-HTTP faults selectable by name in errors.types.
var TransportTypes = map[string]core.TransportFault{
	"connection_reset":       core.TransportReset,
	"slow_headers":           core.TransportSlowHeaders,
	"empty_response":         core.TransportEmpty,
	"random_data_then_close": core.TransportRandomData,
	"corrupt_body":           core.TransportCorruptBody,
	"malformed_chunk":        core.TransportMalformedSSE,
	"hang":                   core.TransportHang,
}

// StreamOnlyTypes are names accepted in errors.types that act on the frames
// of a stream rather than before the first byte (spec 017 I19).
var StreamOnlyTypes = map[string]bool{
	string(core.StreamDupFrames): true,
}

// StreamFinishLength ends the stream early with a valid length finish
// (finish_reason length, stop_reason max_tokens), without an error
// (spec 009, mid-stream table).
const StreamFinishLength core.MidStreamKind = "finish_length"

// MidTypes are the mid-stream fault kinds.
var MidTypes = []core.MidStreamKind{
	core.StreamErrorEvent, core.StreamTruncated, core.StreamHang, core.StreamMalformed,
	core.StreamReset, core.StreamSlowTail, core.StreamDupFrames, core.StreamUsageMiss,
	StreamFinishLength,
}

// Decision is what the injector decided for one request.
type Decision struct {
	Pre        *core.Error
	Mid        *core.MidStreamFault
	ToolBreak  string // toolargs.Mis* or "unknown_tool" | "duplicate_id" | "length_mid_tool" | "empty_response" | "reasoning_only"
	Moderation *config.Moderation
	Source     string // profile | header | step | admin
	// Outage is true when Pre comes from an errors.outages window (Source
	// is then "profile": headers never change a window).
	Outage bool
}

// Query is what DecideQuery evaluates for one request.
type Query struct {
	// Profile is the resolved error profile (key > model > global, with
	// x-wwz-chaos-* headers applied).
	Profile *config.ErrorProfile
	Model   string // model id: counters, chaos event matching
	Driver  string // driver name: errors.outages[].drivers
	TestID  string
	// Outages are the errors.outages lists that apply to the request
	// (root, model, key, header profile), evaluated in order; the union of
	// their windows applies, and a list repeated by two levels (the same
	// profile) is evaluated once.
	Outages [][]config.Outage
}

func init() {
	config.RegisterErrorTypes(KnownType)
	config.RegisterProfileTypes(
		ProfileType,
		func(t string) bool { return midKinds[t] },
	)
}

// Injector evaluates error profiles with per-model counters for after_n and
// burst windows.
type Injector struct {
	counters sync.Map // key -> *int64
	started  time.Time
	// oneShot holds admin-scheduled next-request errors keyed by test id.
	oneShot sync.Map // testID -> *pending
	// scoped holds admin chaos overrides keyed by test id.
	scoped sync.Map // testID -> *config.ErrorProfile
	// events are the timed chaos events of POST /admin/chaos; writers hold
	// evMu, Decide reads the pointer without locking.
	events atomic.Pointer[eventSet]
	evMu   sync.Mutex
	seq    atomic.Uint64
}

// New creates an injector.
func New() *Injector { return &Injector{started: time.Now()} }

// SetNextError schedules a one-shot error for the next request carrying testID.
func (in *Injector) SetNextError(testID string, e *core.Error) {
	in.oneShot.Store(testID, &pending{next: Next{TestID: testID, Type: string(e.Kind), Status: e.Status, Message: e.Message, Code: e.Code, RetryAfter: e.RetryAfter}, err: e})
}

// SetScoped sets a chaos profile that applies to requests carrying testID.
func (in *Injector) SetScoped(testID string, p *config.ErrorProfile) {
	if p == nil {
		in.scoped.Delete(testID)
		return
	}
	in.scoped.Store(testID, p)
}

// Scoped returns the chaos profile for a test id, if any.
func (in *Injector) Scoped(testID string) *config.ErrorProfile {
	if v, ok := in.scoped.Load(testID); ok {
		return v.(*config.ErrorProfile)
	}
	return nil
}

func (in *Injector) count(key string) int64 {
	// Load first: the counter exists after the first request of a key, and
	// LoadOrStore would allocate a fresh one every time (task 270).
	v, ok := in.counters.Load(key)
	if !ok {
		v, _ = in.counters.LoadOrStore(key, new(int64))
	}
	return atomic.AddInt64(v.(*int64), 1)
}

// Decide evaluates the profile (plus admin overrides) for one request
// without outages; see DecideQuery.
func (in *Injector) Decide(p *config.ErrorProfile, modelKey, testID string, r *generate.RNG, now time.Time) Decision {
	return in.DecideQuery(Query{Profile: p, Model: modelKey, TestID: testID}, r, now)
}

// DecideQuery evaluates the profile (plus admin overrides and outage
// windows) for one request. Precedence: one-shot for the test id, then a
// test-scoped chaos event, then a scoped profile (SetScoped), then a global
// chaos event for the model, then an active errors.outages window, then the
// request's own profile.
func (in *Injector) DecideQuery(q Query, r *generate.RNG, now time.Time) Decision {
	p, modelKey, testID := q.Profile, q.Model, q.TestID
	var d Decision
	if testID != "" {
		if v, ok := in.oneShot.LoadAndDelete(testID); ok {
			pe := v.(*pending).err
			cp := *pe
			if pe.Extra != nil {
				cp.Extra = make(map[string]any, len(pe.Extra))
				for k, v := range pe.Extra {
					cp.Extra[k] = v
				}
			}
			d.Pre = &cp
			d.Source = "admin"
			return d
		}
	}
	ep, scopedEvent := in.eventProfile(modelKey, testID, now)
	var sp *config.ErrorProfile
	if testID != "" && !scopedEvent {
		sp = in.Scoped(testID)
	}
	switch {
	case scopedEvent:
		p = ep
	case sp != nil:
		p = sp
	case ep != nil:
		p = ep
	}
	if scopedEvent || sp != nil || ep != nil {
		d.Source = "admin"
	}
	if p == nil {
		return d
	}
	n := in.count(modelKey)
	if d.Source == "" {
		if e := outageError(q.Outages, q.Driver, r, now); e != nil {
			d.Pre, d.Source, d.Outage = e, "profile", true
			return d
		}
	}
	rate := p.Rate
	if p.AfterN > 0 && n <= p.AfterN {
		rate = 0
	}
	if b := p.Burst; b != nil && b.Every > 0 && b.Duration > 0 {
		phase := now.Sub(in.started) % b.Every
		if phase < b.Duration {
			rate = b.Rate
		}
	}
	if r.Chance(rate) {
		name := pickPreName(p, r)
		if StreamOnlyTypes[name] {
			// I19: a frame-level fault named in errors.types becomes a
			// mid-stream fault (a non-streaming reply is left intact).
			d.Mid = midFault(p, name, r)
		} else {
			d.Pre = BuildError(name, p)
		}
		if d.Source == "" {
			d.Source = "profile"
		}
		return d
	}
	if p.MidStreamRate > 0 && r.Chance(p.MidStreamRate) {
		d.Mid = in.pickMid(p, r)
	}
	if tm := p.ToolMisbehavior; tm != nil && tm.Rate > 0 && r.Chance(tm.Rate) {
		kinds := tm.Kinds
		if len(kinds) == 0 {
			kinds = append(append([]string{}, toolargs.AllMisbehaviours...), "unknown_tool", "duplicate_id", "length_mid_tool")
		}
		d.ToolBreak = kinds[r.IntN(len(kinds))]
	}
	if m := p.Moderation; m != nil && m.Probability > 0 && r.Chance(m.Probability) {
		d.Moderation = m
	}
	if d.Source == "" && (d.Mid != nil || d.ToolBreak != "" || d.Moderation != nil) {
		d.Source = "profile"
	}
	return d
}

// outageError returns the error of the first active outage window whose
// rate draw hits, with retry-after up to the end of that window.
func outageError(levels [][]config.Outage, driver string, r *generate.RNG, now time.Time) *core.Error {
	for i, list := range levels {
		if len(list) == 0 || repeated(levels[:i], list) {
			continue
		}
		for j := range list {
			o := &list[j]
			if !o.AppliesTo(driver) {
				continue
			}
			end, ok := o.Window(now)
			if !ok || !r.Chance(o.EffectiveRate()) {
				continue
			}
			e := BuildError(o.Kind(), nil)
			e.RetryAfter = end.Sub(now)
			return e
		}
	}
	return nil
}

// repeated reports whether list shares its backing array with one of prev
// (the same profile reached through two levels).
func repeated(prev [][]config.Outage, list []config.Outage) bool {
	for _, p := range prev {
		if len(p) > 0 && &p[0] == &list[0] {
			return true
		}
	}
	return false
}

func pickPreName(p *config.ErrorProfile, r *generate.RNG) string {
	types := p.Types
	if len(types) == 0 {
		types = config.DefaultErrorTypes
	}
	return pickWeighted(types, p.Weights, r)
}

// DefaultSlowHeadersDelay is the slow_headers wait when the profile sets none.
const DefaultSlowHeadersDelay = 15 * time.Second

// BuildError creates the Error for a named type (chaos type or transport fault).
func BuildError(name string, p *config.ErrorProfile) *core.Error {
	if tf, ok := TransportTypes[name]; ok {
		e := &core.Error{Kind: core.ErrServerError, Transport: tf, Message: "transport fault: " + name}
		if tf == core.TransportSlowHeaders {
			e.TransportDelay = DefaultSlowHeadersDelay
			if p != nil && p.SlowHeadersDelay > 0 {
				e.TransportDelay = p.SlowHeadersDelay
			}
		}
		return e
	}
	e := &core.Error{Kind: core.ErrorKind(name)}
	ra := time.Duration(0)
	if p != nil {
		ra = p.RetryAfter
	}
	switch e.Kind {
	case core.ErrRateLimit:
		e.Message = "Rate limit reached. Please try again later."
		if ra == 0 {
			ra = 20 * time.Second
		}
		e.RetryAfter = ra
	case core.ErrQuota:
		e.Message = "You exceeded your current quota, please check your plan and billing details."
	case core.ErrSpendLimit:
		e.Message = "You have reached your specified API usage limits."
	case core.ErrServerError:
		e.Message = "The server had an error while processing your request. Sorry about that!"
	case core.ErrBadGateway:
		e.Message = "Bad gateway"
	case core.ErrOverloaded:
		e.Message = "Overloaded"
		if ra == 0 {
			ra = 5 * time.Second
		}
		e.RetryAfter = ra
	case core.ErrGatewayTimeout:
		e.Message = "Gateway timeout"
	case core.ErrInvalidRequest:
		e.Message = "Invalid value: unexpected parameter."
		e.Param = "messages"
	case core.ErrContextLength:
		e.Message = "This model's maximum context length is 128000 tokens. However, your messages resulted in 131072 tokens. Please reduce the length of the messages."
		e.Param = "messages"
	case core.ErrInvalidAPIKey:
		e.Message = "Incorrect API key provided."
	case core.ErrPermission:
		e.Message = "You do not have permission to access this resource."
	case core.ErrModelNotFound:
		e.Message = "The model does not exist or you do not have access to it."
	case core.ErrRequestTooLarge:
		e.Message = "Request entity too large."
	case core.ErrContentFilter:
		e.Message = "The response was filtered due to the prompt triggering content management policy."
	case core.ErrSlowDown:
		e.Message = "Traffic increased too quickly; reduce your request rate and ramp up gradually."
		if ra == 0 {
			ra = 60 * time.Second
		}
		e.RetryAfter = ra
	case core.ErrBudget:
		e.Message = "A budget policy for this model has been reached."
	case core.ErrGeoBlocked:
		e.Message = "Country, region, or territory not supported."
	case core.ErrAccountSuspended:
		e.Message = "This account has been temporarily suspended."
	case core.ErrAccountBanned:
		e.Message = "This account has been deactivated."
	default:
		e.Kind = core.ErrServerError
		e.Message = "injected error: " + name
	}
	return e
}

func (in *Injector) pickMid(p *config.ErrorProfile, r *generate.RNG) *core.MidStreamFault {
	types := p.MidStreamTypes
	if len(types) == 0 {
		types = []string{string(core.StreamErrorEvent), string(core.StreamTruncated), string(core.StreamHang)}
	}
	return midFault(p, pickWeighted(types, p.Weights, r), r)
}

// midFault builds the mid-stream fault of a named kind.
func midFault(p *config.ErrorProfile, name string, r *generate.RNG) *core.MidStreamFault {
	f := &core.MidStreamFault{Kind: core.MidStreamKind(name), AfterChunks: p.AfterChunks, AfterRatio: p.AfterRatio, HangFor: p.HangDuration}
	if f.AfterChunks == 0 && f.AfterRatio == 0 {
		f.AfterRatio = 0.3 + r.Float64()*0.4
	}
	if f.HangFor == 0 {
		f.HangFor = 10 * time.Second
	}
	if f.Kind == core.StreamErrorEvent {
		f.Error = &core.Error{Kind: core.ErrOverloaded, Message: "Overloaded"}
	}
	return f
}

func pickWeighted(names []string, weights map[string]float64, r *generate.RNG) string {
	if len(names) == 1 || len(weights) == 0 {
		return names[r.IntN(len(names))]
	}
	total := 0.0
	ws := make([]float64, len(names))
	for i, n := range names {
		w, ok := weights[n]
		if !ok || w <= 0 {
			w = 1
		}
		ws[i] = w
		total += w
	}
	x := r.Float64() * total
	last := len(names) - 1
	for i, n := range names[:last] {
		x -= ws[i]
		if x <= 0 {
			return n
		}
	}
	return names[last] // also absorbs floating-point rounding
}

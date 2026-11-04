package chaos

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
)

// ExtraRawBody is the core.Error.Extra key carrying a raw response body that
// replaces the driver's error envelope (POST /admin/chaos/next "body").
// Drivers that honour it write the value verbatim with the error status.
const ExtraRawBody = "wwz:raw_body"

// knownTypes are the error type names BuildError understands besides the
// transport faults.
var knownTypes = map[core.ErrorKind]bool{
	core.ErrInvalidRequest: true, core.ErrContextLength: true, core.ErrInvalidAPIKey: true,
	core.ErrPermission: true, core.ErrModelNotFound: true, core.ErrRequestTooLarge: true,
	core.ErrRateLimit: true, core.ErrQuota: true, core.ErrSpendLimit: true, core.ErrServerError: true,
	core.ErrBadGateway: true, core.ErrOverloaded: true, core.ErrGatewayTimeout: true,
	core.ErrContentFilter: true, core.ErrSlowDown: true, core.ErrBudget: true, core.ErrGeoBlocked: true,
	core.ErrAccountSuspended: true, core.ErrAccountBanned: true,
}

var midKinds = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range MidTypes {
		m[string(k)] = true
	}
	return m
}()

// KnownType reports whether name is an injectable error type (core.ErrorKind
// or transport fault).
func KnownType(name string) bool {
	if _, ok := TransportTypes[name]; ok {
		return true
	}
	return knownTypes[core.ErrorKind(name)]
}

// ProfileType reports whether name is valid in errors.types: an injectable
// error type or a stream-only fault such as duplicate_frames.
func ProfileType(name string) bool { return KnownType(name) || StreamOnlyTypes[name] }

// ValidateProfile checks an error profile received at runtime.
func ValidateProfile(p *config.ErrorProfile) error {
	if p == nil {
		return errors.New("profile is required")
	}
	for name, v := range map[string]float64{"rate": p.Rate, "mid_stream_rate": p.MidStreamRate} {
		if v < 0 || v > 100 {
			return fmt.Errorf("%s must be between 0 and 100", name)
		}
	}
	for _, t := range p.Types {
		if !ProfileType(t) {
			return fmt.Errorf("unknown error type %q", t)
		}
	}
	for _, t := range p.MidStreamTypes {
		if !midKinds[t] {
			return fmt.Errorf("unknown mid_stream type %q", t)
		}
	}
	if p.AfterRatio < 0 || p.AfterRatio > 1 {
		return errors.New("after_ratio must be between 0 and 1")
	}
	if p.AfterChunks < 0 || p.AfterN < 0 {
		return errors.New("after_chunks and after_n must be >= 0")
	}
	return nil
}

// EventSpec is the input of Start (POST /admin/chaos).
type EventSpec struct {
	// Type is a single error type; exactly one of Type and Profile is set.
	Type    string
	Profile *config.ErrorProfile
	// Duration the event stays active (> 0).
	Duration time.Duration
	// Models affected; empty means all ("*"). Entries are path.Match globs.
	Models []string
	// Rate is the per-request probability (0..100) when Type is used; nil = 100.
	Rate       *float64
	RetryAfter time.Duration
	// TestID restricts the event to requests carrying this X-Test-Id.
	TestID string
}

// Event is an active chaos event.
type Event struct {
	ID         string
	Type       string
	Profile    *config.ErrorProfile // as given (nil when Type is used)
	Models     []string
	Rate       float64
	RetryAfter time.Duration
	TestID     string
	Started    time.Time
	Until      time.Time
	effective  *config.ErrorProfile
}

func (ev *Event) matches(model, testID string) bool {
	if ev.TestID != "" && ev.TestID != testID {
		return false
	}
	for _, m := range ev.Models {
		if m == "*" || m == model {
			return true
		}
		if ok, _ := path.Match(m, model); ok {
			return true
		}
	}
	return false
}

// eventSet is the immutable list of events swapped atomically, so Decide
// reads it without locks.
type eventSet struct{ list []*Event }

// Next is a one-shot error for the next request of a test (POST /admin/chaos/next).
type Next struct {
	TestID     string
	Type       string
	Status     int
	Message    string
	Code       string
	RetryAfter time.Duration
	Body       string
}

type pending struct {
	next Next
	err  *core.Error
}

// kindForStatus infers an error kind from an HTTP status.
func kindForStatus(status int) core.ErrorKind {
	switch status {
	case 400:
		return core.ErrInvalidRequest
	case 401:
		return core.ErrInvalidAPIKey
	case 403:
		return core.ErrPermission
	case 404:
		return core.ErrModelNotFound
	case 413:
		return core.ErrRequestTooLarge
	case 429:
		return core.ErrRateLimit
	case 502:
		return core.ErrBadGateway
	case 503, 529:
		return core.ErrOverloaded
	case 504:
		return core.ErrGatewayTimeout
	}
	if status >= 400 && status < 500 {
		return core.ErrInvalidRequest
	}
	return core.ErrServerError
}

// NextError validates a one-shot request and builds the error it injects.
func NextError(n Next) (*core.Error, error) {
	if n.TestID == "" {
		return nil, errors.New("test_id is required")
	}
	if n.Type == "" && n.Status == 0 {
		return nil, errors.New("type or status is required")
	}
	if n.Status != 0 && (n.Status < 100 || n.Status > 599) {
		return nil, fmt.Errorf("status must be between 100 and 599, got %d", n.Status)
	}
	if n.RetryAfter < 0 {
		return nil, errors.New("retry_after must be >= 0")
	}
	name := n.Type
	if name == "" {
		name = string(kindForStatus(n.Status))
	} else if !KnownType(name) {
		return nil, fmt.Errorf("unknown error type %q", name)
	}
	e := BuildError(name, &config.ErrorProfile{RetryAfter: n.RetryAfter})
	if n.Status != 0 {
		e.Status = n.Status
	}
	if n.Message != "" {
		e.Message = n.Message
	}
	if n.Code != "" {
		e.Code = n.Code
	}
	if n.RetryAfter > 0 {
		e.RetryAfter = n.RetryAfter
	}
	if n.Body != "" {
		e.Extra = map[string]any{ExtraRawBody: n.Body}
	}
	return e, nil
}

// ScheduleNext validates n and schedules it for the next request with
// n.TestID, replacing any pending one.
func (in *Injector) ScheduleNext(n Next) error {
	e, err := NextError(n)
	if err != nil {
		return err
	}
	in.oneShot.Store(n.TestID, &pending{next: n, err: e})
	return nil
}

// PendingNext lists one-shot errors not consumed yet, sorted by test id.
func (in *Injector) PendingNext() []Next {
	var out []Next
	in.oneShot.Range(func(_, v any) bool {
		out = append(out, v.(*pending).next)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].TestID < out[j].TestID })
	return out
}

// Start activates a chaos event.
func (in *Injector) Start(spec EventSpec, now time.Time) (Event, error) {
	if (spec.Type == "") == (spec.Profile == nil) {
		return Event{}, errors.New("exactly one of type and profile is required")
	}
	if spec.Duration <= 0 {
		return Event{}, errors.New("duration must be > 0")
	}
	if spec.RetryAfter < 0 {
		return Event{}, errors.New("retry_after must be >= 0")
	}
	models := spec.Models
	if len(models) == 0 {
		models = []string{"*"}
	}
	for _, m := range models {
		if m == "" {
			return Event{}, errors.New("models entries must not be empty")
		}
		if _, err := path.Match(m, ""); err != nil {
			return Event{}, fmt.Errorf("invalid model pattern %q", m)
		}
	}
	ev := &Event{
		ID:   "chaos_" + strconv.FormatInt(now.UnixNano(), 36) + strconv.FormatUint(in.seq.Add(1), 36),
		Type: spec.Type, Models: append([]string(nil), models...), RetryAfter: spec.RetryAfter,
		TestID: spec.TestID, Started: now, Until: now.Add(spec.Duration),
	}
	if spec.Type != "" {
		if !KnownType(spec.Type) {
			return Event{}, fmt.Errorf("unknown error type %q", spec.Type)
		}
		ev.Rate = 100
		if spec.Rate != nil {
			ev.Rate = *spec.Rate
		}
		if ev.Rate < 0 || ev.Rate > 100 {
			return Event{}, errors.New("rate must be between 0 and 100")
		}
		ev.effective = &config.ErrorProfile{Rate: ev.Rate, Types: []string{spec.Type}, RetryAfter: spec.RetryAfter}
	} else {
		if err := ValidateProfile(spec.Profile); err != nil {
			return Event{}, err
		}
		p := *spec.Profile
		if spec.RetryAfter > 0 {
			p.RetryAfter = spec.RetryAfter
		}
		ev.Profile = spec.Profile
		ev.Rate = p.Rate
		ev.effective = &p
	}
	in.evMu.Lock()
	defer in.evMu.Unlock()
	cur := in.active(now)
	next := make([]*Event, 0, len(cur)+1)
	next = append(next, cur...)
	next = append(next, ev)
	in.events.Store(&eventSet{list: next})
	return *ev, nil
}

// active returns the non-expired events; callers hold evMu or accept a
// stale view.
func (in *Injector) active(now time.Time) []*Event {
	set := in.events.Load()
	if set == nil {
		return nil
	}
	out := make([]*Event, 0, len(set.list))
	for _, ev := range set.list {
		if now.Before(ev.Until) {
			out = append(out, ev)
		}
	}
	return out
}

// Events lists the active events, oldest first.
func (in *Injector) Events(now time.Time) []Event {
	in.evMu.Lock()
	defer in.evMu.Unlock()
	cur := in.active(now)
	in.events.Store(&eventSet{list: cur})
	out := make([]Event, 0, len(cur))
	for _, ev := range cur {
		out = append(out, *ev)
	}
	return out
}

// Stop ends events before their time. With id it stops that event; with
// testID it stops the events and the pending one-shot of that test; with
// both empty it stops every event, one-shot and scoped profile. It returns
// how many items were stopped.
func (in *Injector) Stop(id, testID string) int {
	in.evMu.Lock()
	defer in.evMu.Unlock()
	cur := in.active(time.Now())
	kept := make([]*Event, 0, len(cur))
	n := 0
	for _, ev := range cur {
		hit := (id == "" && testID == "") || (id != "" && ev.ID == id) || (id == "" && testID != "" && ev.TestID == testID)
		if hit {
			n++
		} else {
			kept = append(kept, ev)
		}
	}
	in.events.Store(&eventSet{list: kept})
	switch {
	case id == "" && testID == "":
		in.oneShot.Range(func(k, _ any) bool {
			in.oneShot.Delete(k)
			n++
			return true
		})
		in.scoped.Range(func(k, _ any) bool {
			in.scoped.Delete(k)
			n++
			return true
		})
	case id == "":
		if _, ok := in.oneShot.LoadAndDelete(testID); ok {
			n++
		}
		if _, ok := in.scoped.LoadAndDelete(testID); ok {
			n++
		}
	}
	return n
}

// Reset drops every event, one-shot, scoped profile and after_n counter.
func (in *Injector) Reset() {
	in.Stop("", "")
	in.counters.Range(func(k, _ any) bool {
		in.counters.Delete(k)
		return true
	})
}

// eventProfile returns the profile of the newest active event matching the
// request and whether it is test-scoped; test-scoped events win over global
// ones.
func (in *Injector) eventProfile(model, testID string, now time.Time) (*config.ErrorProfile, bool) {
	set := in.events.Load()
	if set == nil || len(set.list) == 0 {
		return nil, false
	}
	var global *config.ErrorProfile
	for i := len(set.list) - 1; i >= 0; i-- {
		ev := set.list[i]
		if !now.Before(ev.Until) || !ev.matches(model, testID) {
			continue
		}
		if ev.TestID != "" {
			return ev.effective, true
		}
		if global == nil {
			global = ev.effective
		}
	}
	return global, false
}

package admin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/patrickbrandao/white-wizard/internal/chaos"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
)

// intParam parses an optional integer query parameter within [lo, hi].
func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, lo, hi)
	}
	return n, nil
}

// Duration is a Go duration string in JSON ("30s").
type Duration time.Duration

// UnmarshalJSON accepts a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string like \"30s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON renders the Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// viaYAML converts between JSON documents and the yaml-tagged config types,
// so JSON bodies use the same snake_case keys and duration strings as YAML.
func fromJSONViaYAML(raw json.RawMessage, out any) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("must be an object")
	}
	return yaml.UnmarshalWithOptions(mustMarshalYAML(normalize(v)), out, yaml.Strict())
}

func toJSONViaYAML(v any) any {
	var out any
	mustUnmarshalYAML(mustMarshalYAML(v), &out)
	return pruneZero(out)
}

// pruneZero drops null, zero and empty values so profiles render compactly.
func pruneZero(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			val = pruneZero(val)
			if isZero(val) {
				delete(x, k)
				continue
			}
			x[k] = val
		}
		return x
	case []any:
		for i := range x {
			x[i] = pruneZero(x[i])
		}
	}
	return v
}

func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == "" || x == "0s"
	case bool:
		return !x
	case uint64:
		return x == 0
	case int64:
		return x == 0
	case int:
		return x == 0
	case float64:
		return x == 0
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// --- /admin/requests ---

// RequestList is the body of GET /admin/requests.
type RequestList struct {
	Enabled  bool            `json:"enabled"`
	Requests []engine.Record `json:"requests"`
}

func (s *Server) getRequests(w http.ResponseWriter, r *http.Request) {
	last, err := intParam(r, "last", 100, 1, 10000)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	q := r.URL.Query()
	testID, key, scn := q.Get("test_id"), q.Get("key"), q.Get("scenario")
	if q.Has("test_id") && testID == "" {
		invalid(w, "test_id must not be empty")
		return
	}
	cfg := s.eng.Config()
	out := RequestList{Requests: []engine.Record{}}
	if cfg != nil && cfg.Admin.Debug && s.eng.Recorder != nil {
		out.Enabled = true
		out.Requests = s.eng.Recorder.Last(last, func(rec *engine.Record) bool {
			return (testID == "" || rec.TestID == testID) && (key == "" || rec.Key == key) && (scn == "" || rec.Scenario == scn)
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- /admin/scenarios ---

// ScenarioSummary is one row of GET /admin/scenarios.
type ScenarioSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       int    `json:"steps"`
	OnNoMatch   string `json:"on_no_match,omitempty"`
	OnExhausted string `json:"on_exhausted,omitempty"`
}

func (s *Server) getScenarios(w http.ResponseWriter, _ *http.Request) {
	out := []ScenarioSummary{}
	if reg := s.eng.Scenarios; reg != nil {
		for _, n := range reg.Names() {
			// A scenario removed between Names and Get is skipped.
			if sc := reg.Get(n); sc != nil {
				out = append(out, ScenarioSummary{Name: sc.Name, Description: sc.Description, Steps: len(sc.Steps), OnNoMatch: sc.OnNoMatch, OnExhausted: sc.OnExhausted})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenarios": out})
}

// splitYAMLDocs splits a multi-document YAML stream on "---" lines.
func splitYAMLDocs(b []byte) [][]byte {
	var docs [][]byte
	var cur bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), maxBody+1)
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			docs = append(docs, append([]byte(nil), cur.Bytes()...))
		}
		cur.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		if t := strings.TrimRight(line, " \t\r"); t == "---" || strings.HasPrefix(t, "--- ") || t == "..." {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return docs
}

func (s *Server) postScenarios(w http.ResponseWriter, r *http.Request) {
	switch mediaType(r) {
	case "application/yaml", "application/x-yaml", "text/yaml", "text/x-yaml":
	default:
		problem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "use application/yaml")
		return
	}
	body, err := readBody(r)
	if err != nil || len(body) > maxBody {
		invalid(w, "cannot read body")
		return
	}
	if s.eng.Scenarios == nil {
		problem(w, http.StatusInternalServerError, "internal", "scenario registry not configured")
		return
	}
	docs := splitYAMLDocs(body)
	if len(docs) == 0 {
		invalid(w, "no scenario documents in body")
		return
	}
	var parsed []*scenario.Scenario
	var details []string
	names := map[string]bool{}
	for i, d := range docs {
		sc, err := scenario.ParseYAML(d)
		if err != nil {
			details = append(details, fmt.Sprintf("document %d: %v", i+1, err))
			continue
		}
		if names[sc.Name] {
			details = append(details, fmt.Sprintf("document %d: duplicate scenario name %q", i+1, sc.Name))
			continue
		}
		names[sc.Name] = true
		parsed = append(parsed, sc)
	}
	if len(details) > 0 {
		invalid(w, "no scenario loaded; the previous ones are still in effect", details...)
		return
	}
	loaded, replaced := []string{}, []string{}
	for _, sc := range parsed {
		if s.eng.Scenarios.Get(sc.Name) != nil {
			replaced = append(replaced, sc.Name)
		}
		s.eng.Scenarios.Add(sc)
		loaded = append(loaded, sc.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"loaded": loaded, "replaced": replaced})
}

func (s *Server) deleteScenario(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		invalid(w, "name is required")
		return
	}
	if s.eng.Scenarios == nil || s.eng.Scenarios.Get(name) == nil {
		problem(w, http.StatusNotFound, "not_found", "scenario "+strconv.Quote(name)+" not found")
		return
	}
	s.eng.Scenarios.Remove(name)
	w.WriteHeader(http.StatusNoContent)
}

// --- /admin/chaos ---

// ChaosRequest is the body of POST /admin/chaos.
type ChaosRequest struct {
	Type       string          `json:"type"`
	Profile    json.RawMessage `json:"profile"`
	Duration   *Duration       `json:"duration"`
	Models     []string        `json:"models"`
	Rate       *float64        `json:"rate"`
	RetryAfter *Duration       `json:"retry_after"`
	TestID     *string         `json:"test_id"`
}

// ChaosEvent is an active event in responses.
type ChaosEvent struct {
	ID      string    `json:"id"`
	Type    string    `json:"type,omitempty"`
	Profile any       `json:"profile,omitempty"`
	Models  []string  `json:"models"`
	Rate    float64   `json:"rate"`
	TestID  string    `json:"test_id,omitempty"`
	Started time.Time `json:"started"`
	Until   time.Time `json:"until"`
}

// ChaosNextRequest is the body (and response) of POST /admin/chaos/next.
type ChaosNextRequest struct {
	TestID     string    `json:"test_id"`
	Type       string    `json:"type,omitempty"`
	Status     int       `json:"status,omitempty"`
	Message    string    `json:"message,omitempty"`
	Code       string    `json:"code,omitempty"`
	RetryAfter *Duration `json:"retry_after,omitempty"`
	Body       string    `json:"body,omitempty"`
}

func chaosEvent(ev chaos.Event) ChaosEvent {
	out := ChaosEvent{ID: ev.ID, Type: ev.Type, Models: ev.Models, Rate: ev.Rate, TestID: ev.TestID, Started: ev.Started, Until: ev.Until}
	if ev.Profile != nil {
		out.Profile = toJSONViaYAML(ev.Profile)
	}
	return out
}

func nextJSON(n chaos.Next) ChaosNextRequest {
	out := ChaosNextRequest{TestID: n.TestID, Type: n.Type, Status: n.Status, Message: n.Message, Code: n.Code, Body: n.Body}
	if n.RetryAfter > 0 {
		d := Duration(n.RetryAfter)
		out.RetryAfter = &d
	}
	return out
}

func (s *Server) postChaos(w http.ResponseWriter, r *http.Request) {
	var req ChaosRequest
	ok, err := decodeJSON(r, &req)
	if err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	if !ok {
		invalid(w, "body is required")
		return
	}
	if req.Duration == nil {
		invalid(w, "duration is required")
		return
	}
	spec := chaos.EventSpec{Type: req.Type, Duration: time.Duration(*req.Duration), Models: req.Models, Rate: req.Rate}
	if req.RetryAfter != nil {
		spec.RetryAfter = time.Duration(*req.RetryAfter)
	}
	if req.TestID != nil {
		if *req.TestID == "" {
			invalid(w, "test_id must not be empty")
			return
		}
		spec.TestID = *req.TestID
	}
	if len(req.Profile) > 0 && string(req.Profile) != "null" {
		var p config.ErrorProfile
		if err := fromJSONViaYAML(req.Profile, &p); err != nil {
			invalid(w, "invalid profile", err.Error())
			return
		}
		spec.Profile = &p
	}
	ev, err := s.eng.Injector.Start(spec, s.now())
	if err != nil {
		invalid(w, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, chaosEvent(ev))
}

func (s *Server) getChaos(w http.ResponseWriter, _ *http.Request) {
	evs := s.eng.Injector.Events(s.now())
	out := struct {
		Events      []ChaosEvent       `json:"events"`
		PendingNext []ChaosNextRequest `json:"pending_next"`
	}{Events: []ChaosEvent{}, PendingNext: []ChaosNextRequest{}}
	for _, ev := range evs {
		out.Events = append(out.Events, chaosEvent(ev))
	}
	for _, n := range s.eng.Injector.PendingNext() {
		out.PendingNext = append(out.PendingNext, nextJSON(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteChaos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Has("test_id") && q.Get("test_id") == "" {
		invalid(w, "test_id must not be empty")
		return
	}
	n := s.eng.Injector.Stop(q.Get("id"), q.Get("test_id"))
	writeJSON(w, http.StatusOK, map[string]int{"stopped": n})
}

func (s *Server) postChaosNext(w http.ResponseWriter, r *http.Request) {
	var req ChaosNextRequest
	ok, err := decodeJSON(r, &req)
	if err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	if !ok {
		invalid(w, "body is required")
		return
	}
	n := chaos.Next{TestID: req.TestID, Type: req.Type, Status: req.Status, Message: req.Message, Code: req.Code, Body: req.Body}
	if req.RetryAfter != nil {
		n.RetryAfter = time.Duration(*req.RetryAfter)
	}
	if err := s.eng.Injector.ScheduleNext(n); err != nil {
		invalid(w, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, nextJSON(n))
}

// --- /admin/entities ---

var entityKinds = map[string]limits.EntityKind{
	"ip": limits.KindIP, "key": limits.KindKey, "user": limits.KindUser,
	"account": limits.KindAccount, "project": limits.KindProject,
	"deployment": limits.KindDeployment,
}

var entityStatuses = map[string]limits.Status{
	"active": limits.StatusActive, "limited": limits.StatusLimited,
	"suspended": limits.StatusSuspended, "banned": limits.StatusBanned,
}

func entityFrom(w http.ResponseWriter, r *http.Request) (limits.Entity, bool) {
	k, ok := entityKinds[r.PathValue("tipo")]
	id := r.PathValue("id")
	if !ok {
		invalid(w, "tipo must be one of ip, key, user, account, project, deployment")
		return limits.Entity{}, false
	}
	if id == "" {
		invalid(w, "id is required")
		return limits.Entity{}, false
	}
	return limits.Entity{Kind: k, ID: id}, true
}

func (s *Server) listEntities(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 100, 1, 10000)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	var status limits.Status
	if v := r.URL.Query().Get("status"); v != "" {
		st, ok := entityStatuses[v]
		if !ok {
			invalid(w, "status must be one of active, limited, suspended, banned")
			return
		}
		status = st
	}
	ctx, cancel := storeCtx(r)
	defer cancel()
	ents, err := s.eng.Store.List(ctx, status, limit)
	if err != nil {
		storeProblem(w, "", err)
		return
	}
	if ents == nil {
		ents = []limits.EntityState{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": ents})
}

// storeProblem answers a store error: 503 store_unavailable (with
// Retry-After: 1) while Valkey is unreachable (limits.ErrStoreDown, see
// cache/valkey/README.md §6), 500 internal otherwise.
func storeProblem(w http.ResponseWriter, prefix string, err error) {
	msg := prefix + err.Error()
	if errors.Is(err, limits.ErrStoreDown) {
		w.Header().Set("Retry-After", "1")
		problem(w, http.StatusServiceUnavailable, "store_unavailable", msg)
		return
	}
	problem(w, http.StatusInternalServerError, "internal", msg)
}

// entityOp runs op on the entity from the path and answers with its state.
func (s *Server) entityOp(w http.ResponseWriter, r *http.Request, op func(*http.Request, limits.Entity) error) {
	e, ok := entityFrom(w, r)
	if !ok {
		return
	}
	s.entityDo(w, r, e, op)
}

// entityDo runs op on e and answers with the state of e.
func (s *Server) entityDo(w http.ResponseWriter, r *http.Request, e limits.Entity, op func(*http.Request, limits.Entity) error) {
	ctx, cancel := storeCtx(r)
	defer cancel()
	if op != nil {
		if err := op(r, e); err != nil {
			var bad *badRequest
			if errors.As(err, &bad) {
				invalid(w, bad.msg, bad.details...)
				return
			}
			storeProblem(w, "", err)
			return
		}
	}
	st, err := s.eng.Store.Get(ctx, e)
	if err != nil {
		storeProblem(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type badRequest struct {
	msg     string
	details []string
}

func (b *badRequest) Error() string { return b.msg }

func badBody(err error) error {
	return &badRequest{msg: "invalid body", details: []string{err.Error()}}
}

func (s *Server) getEntity(w http.ResponseWriter, r *http.Request) { s.entityOp(w, r, nil) }

func (s *Server) blockEntity(w http.ResponseWriter, r *http.Request) {
	s.entityOp(w, r, func(r *http.Request, e limits.Entity) error {
		var req struct {
			Reason   string    `json:"reason"`
			Duration *Duration `json:"duration"`
			By       string    `json:"by"`
		}
		if _, err := decodeJSON(r, &req); err != nil {
			return badBody(err)
		}
		d := time.Duration(0)
		if req.Duration != nil {
			d = time.Duration(*req.Duration)
		}
		if d < 0 {
			return &badRequest{msg: "duration must be >= 0"}
		}
		if req.By == "" {
			req.By = "admin"
		}
		ctx, cancel := storeCtx(r)
		defer cancel()
		return s.eng.Store.Block(ctx, e, req.Reason, d, req.By)
	})
}

func (s *Server) unblockEntity(w http.ResponseWriter, r *http.Request) {
	s.entityOp(w, r, func(r *http.Request, e limits.Entity) error {
		ctx, cancel := storeCtx(r)
		defer cancel()
		return s.eng.Store.Unblock(ctx, e)
	})
}

func (s *Server) banEntity(w http.ResponseWriter, r *http.Request) {
	s.entityOp(w, r, func(r *http.Request, e limits.Entity) error {
		var req struct {
			Reason string `json:"reason"`
			By     string `json:"by"`
		}
		if _, err := decodeJSON(r, &req); err != nil {
			return badBody(err)
		}
		if req.By == "" {
			req.By = "admin"
		}
		ctx, cancel := storeCtx(r)
		defer cancel()
		return s.eng.Store.Ban(ctx, e, req.Reason, req.By)
	})
}

func (s *Server) unbanEntity(w http.ResponseWriter, r *http.Request) {
	s.entityOp(w, r, func(r *http.Request, e limits.Entity) error {
		ctx, cancel := storeCtx(r)
		defer cancel()
		return s.eng.Store.Unban(ctx, e)
	})
}

// setCredits is POST /admin/entities/{tipo}/{id}/credits: {"usd": n} sets
// the balance; {"add_usd": n} (n > 0) simulates a purchase (balance and
// purchased_usd) and then evaluates promote_when of an account (ADR 0051).
func (s *Server) setCredits(w http.ResponseWriter, r *http.Request) {
	e0, ok := entityFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		USD    *float64 `json:"usd"`
		AddUSD *float64 `json:"add_usd"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	if (req.USD == nil) == (req.AddUSD == nil) {
		invalid(w, "exactly one of usd and add_usd is required")
		return
	}
	if req.USD != nil {
		s.entityOp(w, r, func(r *http.Request, e limits.Entity) error {
			ctx, cancel := storeCtx(r)
			defer cancel()
			return s.eng.Store.SetCredits(ctx, e, *req.USD)
		})
		return
	}
	if !(*req.AddUSD > 0) {
		invalid(w, "add_usd must be greater than 0")
		return
	}
	ca, ok := s.eng.Store.(limits.CreditAdder)
	if !ok {
		unsupported(w, "credit purchase")
		return
	}
	target := e0
	if e0.Kind == limits.KindKey {
		// A purchase belongs to the account (ADR 0058).
		k := keyByLabel(s.eng.Config(), e0.ID)
		if k == nil {
			invalid(w, "unknown key "+strconv.Quote(e0.ID))
			return
		}
		if k.Account == "" {
			invalid(w, "key "+strconv.Quote(e0.ID)+" has no account")
			return
		}
		target = limits.Entity{Kind: limits.KindAccount, ID: k.Account}
	}
	s.entityDo(w, r, target, func(r *http.Request, e limits.Entity) error {
		ctx, cancel := storeCtx(r)
		defer cancel()
		st, err := ca.AddCredits(ctx, e, *req.AddUSD)
		if err != nil {
			return err
		}
		if e.Kind == limits.KindAccount {
			// A failed promotion is logged by the promoter and retried on
			// the next evaluation; the purchase itself succeeded.
			_, _ = s.eng.Promoter().Evaluate(ctx, e.ID, st.Usage(s.now()))
		}
		return nil
	})
}

// unsupported answers 503 when the store lacks an optional operation.
func unsupported(w http.ResponseWriter, op string) {
	problem(w, http.StatusServiceUnavailable, "store_unavailable", "the "+op+" operation is not supported by this store")
}

// setTier is POST /admin/entities/{tipo}/{id}/tier (ADR 0051 §5): the tier
// of an account; tipo key is resolved to the account of the key in the
// effective configuration. The tier must exist among the effective tiers
// ("" or null removes). The answer is the state of the account.
func (s *Server) setTier(w http.ResponseWriter, r *http.Request) {
	e, ok := entityFrom(w, r)
	if !ok {
		return
	}
	cfg := s.eng.Config()
	switch e.Kind {
	case limits.KindAccount:
	case limits.KindKey:
		k := keyByLabel(cfg, e.ID)
		if k == nil {
			invalid(w, "unknown key "+strconv.Quote(e.ID))
			return
		}
		if k.Account == "" {
			invalid(w, "key "+strconv.Quote(e.ID)+" has no account")
			return
		}
		e = limits.Entity{Kind: limits.KindAccount, ID: k.Account}
	default:
		invalid(w, "tier can only be assigned to tipo account (or key, resolved to its account)")
		return
	}
	var req struct {
		Tier json.RawMessage `json:"tier"`
	}
	present, err := decodeJSON(r, &req)
	if err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	if !present || len(req.Tier) == 0 {
		invalid(w, "tier is required (use \"\" or null to remove it)")
		return
	}
	var tier *string
	if err := json.Unmarshal(req.Tier, &tier); err != nil {
		invalid(w, "tier must be a string or null")
		return
	}
	name := ""
	if tier != nil {
		name = *tier
	}
	if name != "" && (cfg == nil || !hasTier(cfg, name)) {
		invalid(w, "unknown tier "+strconv.Quote(name))
		return
	}
	ts, ok := s.eng.Store.(limits.TierSetter)
	if !ok {
		unsupported(w, "tier")
		return
	}
	ctx, cancel := storeCtx(r)
	defer cancel()
	if err := ts.SetTier(ctx, e, name); err != nil {
		storeProblem(w, "", err)
		return
	}
	st, err := s.eng.Store.Get(ctx, e)
	if err != nil {
		storeProblem(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// keyByLabel is the keys[] entry with that label (the id of tipo key), or nil.
func keyByLabel(cfg *config.Config, label string) *config.KeyConfig {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Keys {
		if cfg.Keys[i].Label == label {
			return &cfg.Keys[i]
		}
	}
	return nil
}

func hasTier(cfg *config.Config, name string) bool {
	_, ok := cfg.Tiers[name]
	return ok
}

// resetEntity is POST /admin/entities/{tipo}/{id}/reset (ADR 0025).
func (s *Server) resetEntity(w http.ResponseWriter, r *http.Request) {
	if _, ok := entityFrom(w, r); !ok {
		return
	}
	var req struct {
		Scope *[]string `json:"scope"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	counters, strikes := true, false
	if req.Scope != nil {
		if len(*req.Scope) == 0 {
			invalid(w, "scope must not be empty")
			return
		}
		counters = false
		seen := map[string]bool{}
		for _, sc := range *req.Scope {
			if seen[sc] {
				invalid(w, "scope has duplicate item "+strconv.Quote(sc))
				return
			}
			seen[sc] = true
			switch sc {
			case "counters":
				counters = true
			case "strikes":
				strikes = true
			default:
				invalid(w, "scope items must be counters or strikes")
				return
			}
		}
	}
	er, ok := s.eng.Store.(limits.EntityResetter)
	if !ok {
		unsupported(w, "reset")
		return
	}
	s.entityOp(w, r, func(r *http.Request, e limits.Entity) error {
		ctx, cancel := storeCtx(r)
		defer cancel()
		return er.ResetEntity(ctx, e, counters, strikes)
	})
}

// --- /admin/leak ---

// LeakResult is the response of POST /admin/leak.
type LeakResult struct {
	Key     limits.EntityState  `json:"key"`
	Account *limits.EntityState `json:"account,omitempty"`
}

func (s *Server) postLeak(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label  string `json:"label"`
		Key    string `json:"key"`
		Reason string `json:"reason"`
	}
	ok, err := decodeJSON(r, &req)
	if err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	if !ok || (req.Label == "" && req.Key == "") {
		invalid(w, "label or key is required")
		return
	}
	if req.Reason == "" {
		req.Reason = "leaked"
	}
	cfg := s.eng.Config()
	var kc *config.KeyConfig
	for i := range cfg.Keys {
		k := &cfg.Keys[i]
		if (req.Label != "" && k.Label == req.Label) || (req.Label == "" && k.Key == req.Key) {
			kc = k
			break
		}
	}
	if kc == nil {
		problem(w, http.StatusNotFound, "not_found", "key not found in the configuration")
		return
	}
	ctx, cancel := storeCtx(r)
	defer cancel()
	ke := limits.Entity{Kind: limits.KindKey, ID: kc.Label}
	if err := s.eng.Store.Ban(ctx, ke, req.Reason, "admin"); err != nil {
		storeProblem(w, "", err)
		return
	}
	var out LeakResult
	if out.Key, err = s.eng.Store.Get(ctx, ke); err != nil {
		storeProblem(w, "", err)
		return
	}
	if kc.Account != "" {
		ae := limits.Entity{Kind: limits.KindAccount, ID: kc.Account}
		if err := s.eng.Store.Block(ctx, ae, req.Reason, 0, "admin"); err != nil {
			storeProblem(w, "", err)
			return
		}
		st, err := s.eng.Store.Get(ctx, ae)
		if err != nil {
			storeProblem(w, "", err)
			return
		}
		out.Account = &st
	}
	writeJSON(w, http.StatusOK, out)
}

// --- /admin/resilience and /admin/conversations/reset ---

func (s *Server) getResilience(w http.ResponseWriter, r *http.Request) {
	cfg := s.eng.Config()
	maxReports := 100
	tracking := true
	if cfg != nil {
		tracking = cfg.Resilience.Tracking()
		if cfg.Resilience.MaxReports > 0 {
			maxReports = cfg.Resilience.MaxReports
		}
	}
	limit, err := intParam(r, "limit", maxReports, 1, 10000)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	q := r.URL.Query()
	if q.Has("test_id") {
		id := q.Get("test_id")
		if id == "" {
			invalid(w, "test_id must not be empty")
			return
		}
		var rep *engine.TestReport
		if tracking && s.eng.Resilience != nil {
			rep = s.eng.Resilience.Report(id)
		}
		if rep == nil {
			msg := "no resilience report for test_id " + strconv.Quote(id)
			if !tracking {
				msg = "resilience.track is off"
			}
			problem(w, http.StatusNotFound, "not_found", msg)
			return
		}
		writeJSON(w, http.StatusOK, rep)
		return
	}
	tests := []*engine.TestReport{}
	if tracking && s.eng.Resilience != nil {
		tests = append(tests, s.eng.Resilience.List(limit)...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tests": tests})
}

func (s *Server) resetConversations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConversationID *string `json:"conversation_id"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		invalid(w, "invalid body", err.Error())
		return
	}
	id := ""
	if req.ConversationID != nil {
		if *req.ConversationID == "" {
			invalid(w, "conversation_id must not be empty")
			return
		}
		id = *req.ConversationID
	}
	n := 0
	if cr, ok := s.eng.Store.(limits.ConversationResetter); ok {
		ctx, cancel := storeCtx(r)
		defer cancel()
		var err error
		if n, err = cr.ResetConversations(ctx, id); err != nil {
			storeProblem(w, "", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"reset": n})
}

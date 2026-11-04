package drift

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrUsage marks invalid options (exit code 2).
var ErrUsage = errors.New("uso inválido")

// DefaultTimeout is the default limit of one probe.
const DefaultTimeout = 60 * time.Second

// maxBody caps how much of one answer is read (model lists are large).
const maxBody = 64 << 20

// Options configure a check. Zero values take the defaults of spec 017.
type Options struct {
	// Providers to probe (default DefaultProviders); the others are
	// reported skipped/not_selected.
	Providers []string
	// Probes to run (default ProbeNames).
	Probes []string
	// Keys maps provider to its real key (see LoadKeys); a provider
	// without key is skipped/no_key.
	Keys map[string]string
	// WWZURL is the data plane base to compare; empty starts the embedded
	// wwz (StartWWZ).
	WWZURL string
	// WWZVersion is reported for an external wwz; the embedded one reports
	// ToolVersion.
	WWZVersion string
	// WWZKey is sent to wwz (default DefaultWWZKey).
	WWZKey string
	// Models overrides the real probe model per provider.
	Models map[string]string
	// BaseURLs overrides the real base per provider (tests use httptest).
	BaseURLs map[string]string
	// Real confirms probing the real base of a provider that has a key but
	// no BaseURLs entry; without it that provider is skipped/real_not_confirmed
	// (a test key in the environment must not reach the real API by accident).
	Real bool
	// Ignores are added to DefaultIgnores.
	Ignores []Ignore
	// BudgetFile (default DefaultBudgetFile) and BudgetUSD (default
	// DefaultBudgetUSD) cap the real spend per provider.
	BudgetFile string
	BudgetUSD  float64
	// Timeout limits each probe (default DefaultTimeout).
	Timeout time.Duration
	// SaveRaw, when set, receives the raw answers (no auth headers,
	// secrets masked); it must be under a tmp/ directory.
	SaveRaw string
	// ToolVersion is reported as tool_version and sent in the User-Agent.
	ToolVersion string
	// Client sends the requests (default a new http.Client).
	Client *http.Client
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Session is the OpenCode session id (default ses_<64 random hex>).
	Session string
	// NewSession makes the next OpenCode session when the upstream form
	// must change (default NewSessionID).
	NewSession func() string
}

func (o *Options) defaults() {
	if len(o.Providers) == 0 {
		o.Providers = DefaultProviders
	}
	if len(o.Probes) == 0 {
		o.Probes = ProbeNames
	}
	if o.WWZKey == "" {
		o.WWZKey = DefaultWWZKey
	}
	if o.BudgetFile == "" {
		o.BudgetFile = DefaultBudgetFile
	}
	if o.BudgetUSD == 0 {
		o.BudgetUSD = DefaultBudgetUSD
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	if o.ToolVersion == "" {
		o.ToolVersion = "dev"
	}
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Session == "" {
		o.Session = NewSessionID()
	}
}

func (r *runner) newSession() string {
	if r.o.NewSession != nil {
		return r.o.NewSession()
	}
	return NewSessionID()
}

// NewSessionID returns an OpenCode session id: ses_ and 64 random hex.
func NewSessionID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return "ses_" + hex.EncodeToString(b)
}

func usage(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUsage}, args...)...)
}

func checkNames(what string, names, known []string) error {
	for _, n := range names {
		if !slices.Contains(known, n) {
			return usage("%s %q desconhecido (use %s)", what, n, strings.Join(known, ","))
		}
	}
	return nil
}

func checkURL(what, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return usage("%s %q não é uma URL http(s)", what, raw)
	}
	return nil
}

// UnderTmp reports whether dir is (or is under) a directory named tmp.
func UnderTmp(dir string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(filepath.Clean(dir)), "/"), "tmp")
}

// Validate checks the options after defaults; errors wrap ErrUsage.
func (o *Options) Validate() error {
	if err := checkNames("provedor", o.Providers, ProviderNames); err != nil {
		return err
	}
	if err := checkNames("sonda", o.Probes, ProbeNames); err != nil {
		return err
	}
	for _, m := range []map[string]string{o.Models, o.BaseURLs, o.Keys} {
		for p := range m {
			if err := checkNames("provedor", []string{p}, ProviderNames); err != nil {
				return err
			}
		}
	}
	for p, u := range o.BaseURLs {
		if err := checkURL("--base-url "+p, u); err != nil {
			return err
		}
	}
	if o.WWZURL != "" {
		if err := checkURL("--wwz", o.WWZURL); err != nil {
			return err
		}
	}
	if o.BudgetUSD < 0 {
		return usage("--budget-usd negativo")
	}
	if o.Timeout < 0 {
		return usage("--timeout negativo")
	}
	if o.SaveRaw != "" && !UnderTmp(o.SaveRaw) {
		return usage("--save-raw %q precisa estar sob tmp/", o.SaveRaw)
	}
	for i, ig := range o.Ignores {
		if err := ig.validate(); err != nil {
			return usage("ignore[%d]: %v", i, err)
		}
	}
	return nil
}

// runner holds one check.
type runner struct {
	o       Options
	red     *Redactor
	ignores []Ignore
	wwz     string
	// session is the OpenCode session of this run (Options.Session, replaced
	// when the upstream form must change, ADR 0059).
	session string
	// realID is the "id" of the last real chat answer probed.
	realID string
}

// Run executes the check. On invalid options it returns (nil, err) with
// err wrapping ErrUsage; on an execution failure (wwz unreachable, budget
// file unreadable or not writable) it returns the report so far and the
// error. Use ExitCode to map the result.
func Run(ctx context.Context, opts Options) (*Report, error) {
	opts.defaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	secrets := make([]string, 0, len(opts.Keys))
	for _, k := range opts.Keys {
		secrets = append(secrets, k)
	}
	r := &runner{o: opts, red: NewRedactor(secrets...), ignores: append(DefaultIgnores(), opts.Ignores...), session: opts.Session}
	rep := &Report{
		Version: ReportVersion, Kind: ReportKind, ToolVersion: opts.ToolVersion,
		Started: opts.Now().UTC().Truncate(time.Second), red: r.red,
		Providers: []ProviderReport{},
	}
	err := r.run(ctx, rep)
	rep.Finished = opts.Now().UTC().Truncate(time.Second)
	rep.summarize()
	return rep, err
}

func (r *runner) run(ctx context.Context, rep *Report) error {
	if r.o.WWZURL == "" {
		w, err := startWWZ(r.o.Models)
		if err != nil {
			return fmt.Errorf("drift: wwz embutido: %w", err)
		}
		defer func() { _ = w.Close() }()
		r.wwz = w.URL
		rep.WWZ = WWZInfo{URL: InProcessURL, Version: r.o.ToolVersion}
	} else {
		r.wwz = strings.TrimRight(r.o.WWZURL, "/")
		rep.WWZ = WWZInfo{URL: r.red.String(r.wwz), Version: r.o.WWZVersion}
		if err := r.reachable(ctx); err != nil {
			return err
		}
	}
	for _, name := range ProviderNames {
		pr, err := r.provider(ctx, name)
		rep.Providers = append(rep.Providers, pr)
		if err != nil {
			return err
		}
	}
	return nil
}

// reachable checks that the external wwz answers HTTP at all.
func (r *runner) reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.wwz+"/", nil) // URL validated
	resp, err := r.o.Client.Do(req)
	if err != nil {
		return fmt.Errorf("drift: wwz inalcançável: %s", r.red.String(err.Error()))
	}
	_ = resp.Body.Close()
	return nil
}

func skipped(name, reason string) ProviderReport {
	return ProviderReport{Provider: name, Status: StatusSkipped, Reason: reason, Probes: []ProbeReport{}}
}

// target resolves the real base and key of a provider. gate is non-nil
// when the provider must not be probed: skipped (not selected, no key, real
// base not confirmed, budget exhausted) or in error when the budget file
// cannot be read (err set). Shared by check and record.
func (r *runner) target(name string) (p Provider, key string, gate *ProviderReport, err error) {
	p = providers[name]
	if u := r.o.BaseURLs[name]; u != "" {
		p.BaseURL = strings.TrimRight(u, "/")
	}
	skip := func(reason string) *ProviderReport {
		sk := skipped(name, reason)
		sk.Base = p.BaseURL
		return &sk
	}
	if !slices.Contains(r.o.Providers, name) {
		sk := skipped(name, ReasonNotSelected)
		return p, "", &sk, nil
	}
	key = r.o.Keys[name]
	if key == "" || p.BaseURL == "" { // azure: no real base without --base-url
		return p, "", skip(ReasonNoKey), nil
	}
	if r.o.BaseURLs[name] == "" && !r.o.Real {
		return p, "", skip(ReasonRealNotConfirmed), nil
	}
	budget, err := ReadBudget(r.o.BudgetFile)
	if err != nil {
		return p, "", &ProviderReport{Provider: name, Status: StatusError, Reason: err.Error(), Probes: []ProbeReport{}}, err
	}
	if budget[name].USD >= r.o.BudgetUSD {
		return p, "", skip(ReasonBudgetExhausted), nil
	}
	return p, key, nil, nil
}

// provider runs every selected probe of one provider.
func (r *runner) provider(ctx context.Context, name string) (ProviderReport, error) {
	p, key, gate, err := r.target(name)
	if gate != nil {
		return *gate, err
	}
	pr := ProviderReport{Provider: name, Base: p.BaseURL, Model: probeModel(r.o.Models, name), Probes: []ProbeReport{}}
	if p.Session {
		pr.Session = r.session
	}
	for _, probe := range ProbeNames {
		if !slices.Contains(r.o.Probes, probe) {
			pr.Probes = append(pr.Probes, ProbeReport{Probe: probe, Status: StatusSkipped, Reason: ReasonNotSelected, Divergences: []Divergence{}})
			continue
		}
		if responsesProbe(probe) && p.ResponsesPath == "" {
			pr.Probes = append(pr.Probes, ProbeReport{Probe: probe, Status: StatusSkipped, Reason: ReasonNotSupported, Divergences: []Divergence{}})
			continue
		}
		res, cost := r.probe(ctx, p, probe, key, pr.Model)
		unreproduced := false
		if p.Session && probe == ProbeChat && res.Status != StatusError && r.realID != "" && upstreamForm(r.realID) != formGW {
			var extra float64
			res, extra, unreproduced = r.changeSession(ctx, p, key, pr.Model, res)
			cost += extra
			pr.Session = r.session
		}
		pr.Probes = append(pr.Probes, res)
		if cost > 0 {
			pr.SpendUSD += cost
			if _, err := AddSpend(r.o.BudgetFile, name, cost, r.o.Now()); err != nil {
				pr.Status = rollup(pr.Probes)
				return pr, err
			}
		}
		if unreproduced {
			pr.Status, pr.Reason, pr.Probes = StatusSkipped, ReasonUpstreamFormUnreproduced, []ProbeReport{}
			return pr, nil
		}
	}
	pr.Status = rollup(pr.Probes)
	return pr, nil
}

// probeModel is the model of provider: the --model override or the fixed
// default, sent to both sides.
func probeModel(overrides map[string]string, provider string) string {
	if m := overrides[provider]; m != "" {
		return m
	}
	return providers[provider].DefaultModel
}

// answer is one HTTP response.
type answer struct {
	status int
	header http.Header
	body   []byte
}

func (r *runner) do(ctx context.Context, base string, p Provider, rq request, key string) (answer, error) {
	var body io.Reader
	if rq.body != nil {
		body = bytes.NewReader(rq.body)
	}
	req, _ := http.NewRequestWithContext(ctx, rq.method, base+rq.path, body) // method and URL validated
	req.Header = p.headers(key, r.session, "wwz-drift/"+r.o.ToolVersion)
	if rq.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.o.Client.Do(req)
	if err != nil {
		return answer{}, errors.New(r.red.String(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return answer{}, errors.New(r.red.String(err.Error()))
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// probe runs one probe on both sides; failures are reported in res. cost
// is the real spend to record.
func (r *runner) probe(ctx context.Context, p Provider, probe, key, model string) (res ProbeReport, cost float64) {
	res = ProbeReport{Probe: probe, Divergences: []Divergence{}}
	r.realID = ""
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	realKey, wwzKey := key, r.o.WWZKey
	if probe == ProbeErrorAuth {
		realKey, wwzKey = InvalidKey, InvalidKey
	}
	fail := func(side string, e error) ProbeReport {
		res.Status, res.Reason = StatusError, side+": "+r.red.String(e.Error())
		return res
	}
	ra, err := r.do(ctx, p.BaseURL, p, p.probeRequest(probe, model), realKey)
	if err != nil {
		return fail("real", err), 0
	}
	res.RealStatus = ra.status
	if probe == ProbeChat && ra.status/100 == 2 {
		r.realID = bodyID(ra.body)
	}
	if paid(probe) && ra.status/100 == 2 {
		in, out, ok := usageOf(ra.body, probe == ProbeChatStream)
		cost = CostUSD(model, in, out, ok)
	}
	wa, err := r.do(ctx, r.wwz+p.WWZPrefix, p, p.probeRequest(probe, model), wwzKey)
	if err != nil {
		return fail("wwz", err), cost
	}
	res.WWZStatus = wa.status
	if err := r.saveRaw(p.Name, probe, ra, wa); err != nil {
		return fail("save-raw", err), cost
	}
	sign := Sign
	if probe == ProbeModels {
		sign = func(status int, h http.Header, body []byte) (Signature, error) {
			return SignModels(status, h, body, model)
		}
	}
	rs, err := sign(ra.status, ra.header, ra.body)
	if err != nil {
		return fail("real", err), cost
	}
	ws, err := sign(wa.status, wa.header, wa.body)
	if err != nil {
		return fail("wwz", err), cost
	}
	res.Divergences = Compare(rs, ws)
	if res.Divergences == nil {
		res.Divergences = []Divergence{}
	}
	res.Status = StatusOK
	if markIgnored(r.ignores, p.Name, probe, res.Divergences) {
		res.Status = StatusDrift
	}
	return res, cost
}

// saveRaw writes both answers under SaveRaw, without sensitive headers and
// with secrets masked.
func (r *runner) saveRaw(provider, probe string, ra, wa answer) error {
	if r.o.SaveRaw == "" {
		return nil
	}
	if err := os.MkdirAll(r.o.SaveRaw, 0o750); err != nil {
		return err
	}
	for side, a := range map[string]answer{"real": ra, "wwz": wa} {
		var b bytes.Buffer
		fmt.Fprintf(&b, "HTTP %d\n", a.status)
		names := make([]string, 0, len(a.header))
		for k := range a.header {
			if !isSensitive(k) {
				names = append(names, k)
			}
		}
		slices.Sort(names)
		for _, k := range names {
			fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(a.header[k], ", "))
		}
		b.WriteByte('\n')
		b.Write(a.body)
		path := filepath.Join(r.o.SaveRaw, provider+"-"+probe+"-"+side+".txt")
		if err := os.WriteFile(path, r.red.Bytes(b.Bytes()), 0o600); err != nil {
			return err
		}
	}
	return nil
}

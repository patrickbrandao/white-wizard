package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/claudecode/client"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// ccName prefixes the messages of wwz-load claude-code.
const ccName = "wwz-load claude-code"

// ccDial replaces the dialer of the client (tests count connections).
var ccDial client.Dialer

// ccNow is the clock of the token expiry check.
var ccNow = time.Now

// ccOptions are the flags of wwz-load claude-code (spec 015, "Contrato do
// wwz-load claude-code").
type ccOptions struct {
	action                         string
	args                           []string
	targetS, version, entrypoint   string
	auth, token, key, statePath    string
	model, systemPrompt, arch, osS string
	stream                         bool
	retries                        int
	timeout                        time.Duration
	manual, setupToken             bool          // login
	resume                         bool          // ask
	dump                           string        // ask
	send                           bool          // replay
	turns                          int           // run
	prompt, output, out            string        // run
	rps                            float64       // run
	concurrency                    int           // run
	duration                       time.Duration // run
	requests                       int64         // run

	target *client.Target
	set    map[string]bool
	// scopes and plan of the state's token, set by credential: they tell a
	// subscriber from enterprise in the retries (spec 015, "Transporte").
	scopes []string
	plan   string
}

func ccUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: wwz-load claude-code <action> [flags]

A test client that behaves like Claude Code 2.1.308 against the wwz (ADR 0074).
It refuses the real Anthropic hosts (anthropic.com, claude.ai, claude.com).

Actions:
  login [--manual] [--setup-token]  OAuth login against --target; writes --state
  refresh                           renew the access token of --state
  ask <text> [--resume] [--dump F]  one turn; prints the answer
  replay <capture.raw> [--send]     rebuild a capture and compare byte for byte
  run [--turns N] [--rps R | --concurrency C] [--duration D] [--requests N]
      [--output text|json|csv] [--out F] [--prompt P]
                                    load: one session per unit, run report

Common flags: --target URL --version 2.1.308 --entrypoint sdk-cli|cli
  --auth oauth|api-key --token T --key K --state FILE --model M
  --system-prompt S --arch arm64|x64 --os MacOS|Linux|Windows
  --stream=true --retries 2 --timeout 600s

Run "wwz-load claude-code <action> -h" for the flags of an action.
`)
}

// runClaudeCode is `wwz-load claude-code <action> [flags]`. Exit codes: 0
// ok (replay: identical), 1 refusal of the server or failure (replay:
// different), 2 usage, including a target on a real Anthropic host.
func runClaudeCode(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		ccUsage(stderr)
		return exitUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		ccUsage(stdout)
		return exitOK
	}
	o, err := parseClaudeCode(args[0], args[1:], stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return ccFail(stderr, err)
	}
	actions := map[string]func(context.Context, *ccOptions, io.Writer, io.Writer) int{
		"login": ccLogin, "refresh": ccRefresh, "ask": ccAsk, "replay": ccReplay, "run": ccRun,
	}
	return actions[o.action](ctx, o, stdout, stderr)
}

// ccFail prints err and returns its exit code: 2 for usage (and a real
// host), 1 otherwise.
func ccFail(stderr io.Writer, err error) int {
	var real *client.RealHostError
	usage := errors.Is(err, errUsage) || errors.As(err, &real) || errors.Is(err, client.ErrNoTurns) ||
		errors.Is(err, loadgen.ErrBadRunConfig)
	_, _ = fmt.Fprintf(stderr, "%s: %s\n", ccName, strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
	if usage {
		return exitUsage
	}
	return exitError
}

func parseClaudeCode(action string, args []string, stderr io.Writer) (*ccOptions, error) {
	o := &ccOptions{action: action, set: map[string]bool{}}
	fs := flag.NewFlagSet(ccName+" "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.targetS, "target", "", "base URL of the wwz (ANTHROPIC_BASE_URL), with the mount prefix")
	fs.StringVar(&o.version, "version", client.DefaultVersion, "Claude Code profile version")
	fs.StringVar(&o.entrypoint, "entrypoint", string(claudecode.EntrypointSDKCLI), "sdk-cli (claude -p) or cli (interactive)")
	fs.StringVar(&o.auth, "auth", "oauth", "oauth (token of --state or --token) or api-key (--key)")
	fs.StringVar(&o.token, "token", "", "explicit OAuth access token (not from the state)")
	fs.StringVar(&o.key, "key", "", "API key, with --auth api-key")
	fs.StringVar(&o.statePath, "state", "", "state file (default $XDG_STATE_HOME/wwz-load/claude-code.json)")
	fs.StringVar(&o.model, "model", "", "model (default "+client.DefaultModelOAuth+" with oauth, "+client.DefaultModelAPIKey+" with api-key)")
	fs.StringVar(&o.systemPrompt, "system-prompt", client.DefaultSystemPrompt, "the --system-prompt of Claude Code")
	fs.StringVar(&o.arch, "arch", "arm64", "X-Stainless-Arch")
	fs.StringVar(&o.osS, "os", "MacOS", "X-Stainless-OS")
	fs.BoolVar(&o.stream, "stream", true, "false uses the non-streaming fallback")
	fs.IntVar(&o.retries, "retries", client.DefaultRetries, "retries after the first attempt")
	fs.DurationVar(&o.timeout, "timeout", client.DefaultTimeout, "ceiling of each request")
	nargs := 0
	switch action {
	case "login":
		fs.BoolVar(&o.manual, "manual", false, "manual redirect: read code#state from the authorize page")
		fs.BoolVar(&o.setupToken, "setup-token", false, "only user:inference and a one-year token, like claude setup-token")
	case "refresh":
	case "ask":
		nargs = 1
		fs.BoolVar(&o.resume, "resume", false, "continue the conversation of the state")
		fs.StringVar(&o.dump, "dump", "", "write the bytes sent to this file")
	case "replay":
		nargs = 1
		fs.BoolVar(&o.send, "send", false, "also send the rebuilt request to --target")
	case "run":
		fs.IntVar(&o.turns, "turns", 1, "turns per session")
		fs.StringVar(&o.prompt, "prompt", client.DefaultPrompt, "user message of every turn")
		fs.Float64Var(&o.rps, "rps", 0, "fixed rate of sessions (open loop)")
		fs.IntVar(&o.concurrency, "concurrency", 1, "concurrent sessions (closed loop, the default mode)")
		fs.DurationVar(&o.duration, "duration", 30*time.Second, "measurement duration (0 = until --requests)")
		fs.Int64Var(&o.requests, "requests", 0, "stop after starting N sessions (0 = no limit)")
		fs.StringVar(&o.output, "output", loadgen.FormatText, "report format: text, json or csv")
		fs.StringVar(&o.out, "out", "", "write the report to this file (default stdout)")
	default:
		return nil, usageErr("unknown action %q (want login, refresh, ask, replay or run)", action)
	}
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %w", errUsage, err)
		}
		if fs.NArg() == 0 {
			break
		}
		o.args = append(o.args, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	if len(o.args) != nargs {
		return nil, usageErr("%s takes %d argument(s), got %d", action, nargs, len(o.args))
	}
	return o, o.validate()
}

func (o *ccOptions) validate() error {
	if o.targetS != "" {
		t, err := client.ParseTarget(o.targetS)
		var real *client.RealHostError
		if errors.As(err, &real) {
			return err
		}
		if err != nil {
			return usageErr("--target: %v", err)
		}
		o.target = t
	}
	switch {
	case o.target == nil && (o.action != "replay" || o.send):
		return usageErr("--target is required")
	case o.auth != "oauth" && o.auth != "api-key":
		return usageErr("--auth %q: want oauth or api-key", o.auth)
	case o.auth == "oauth" && o.key != "":
		return usageErr("--key needs --auth api-key")
	case o.auth == "api-key" && o.token != "":
		return usageErr("--token needs --auth oauth")
	case o.auth == "api-key" && o.key == "" && (o.action == "ask" || o.action == "run"):
		return usageErr("--auth api-key needs --key")
	case !slices.Contains([]string{loadgen.FormatText, loadgen.FormatJSON, loadgen.FormatCSV}, o.output) && o.action == "run":
		return usageErr("--output %q: want text, json or csv", o.output)
	case o.set["rps"] && o.set["concurrency"]:
		return usageErr("--rps and --concurrency are exclusive")
	}
	if o.statePath == "" {
		p, err := client.DefaultStatePath()
		if err != nil {
			return usageErr("%v", err)
		}
		o.statePath = p
	}
	return nil
}

// newClient builds the client of the flags with cred.
func (o *ccOptions) newClient(cred claudecode.Credential, account, device string) (*client.Client, error) {
	c, err := client.New(client.Options{
		Target: o.target, Version: o.version, Entrypoint: claudecode.Entrypoint(o.entrypoint),
		Credential: cred, AccountUUID: account, DeviceID: device, Model: o.model, SystemPrompt: o.systemPrompt,
		Arch: o.arch, OS: o.osS, NonStream: !o.stream, Retries: o.retries, Timeout: o.timeout, Dial: ccDial,
		Scopes: o.scopes, Plan: o.plan,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	return c, nil
}

// credential returns the credential of ask and run: the API key, the
// explicit token or the token of the state (renewed first when it expired),
// with the account of the login when the state has the profile scope.
func (o *ccOptions) credential(ctx context.Context, st *client.State) (claudecode.Credential, string, error) {
	switch {
	case o.auth == "api-key":
		return claudecode.Credential{Kind: claudecode.CredentialAPIKey, Secret: o.key}, "", nil
	case o.token != "":
		return claudecode.Credential{Kind: claudecode.CredentialOAuth, Secret: o.token}, "", nil
	case st.AccessToken == "":
		return claudecode.Credential{}, "", usageErr("no access token in %s: run login or pass --token", o.statePath)
	}
	if st.ExpiresAt != 0 && !ccNow().Before(time.Unix(st.ExpiresAt, 0)) && st.RefreshToken != "" {
		c, err := o.newClient(claudecode.Credential{}, "", st.DeviceID)
		if err != nil {
			return claudecode.Credential{}, "", err
		}
		t, err := c.Refresh(ctx, st.RefreshToken, st.Scopes)
		if err != nil {
			return claudecode.Credential{}, "", fmt.Errorf("refresh of the expired token: %w", err)
		}
		st.SetTokens(t)
		if err := st.Save(o.statePath); err != nil {
			return claudecode.Credential{}, "", err
		}
	}
	account := ""
	if st.Account != nil && slices.Contains(st.Scopes, client.ScopeProfile) {
		account = st.Account.UUID
	}
	o.scopes, o.plan = st.Scopes, st.Plan
	return claudecode.Credential{Kind: claudecode.CredentialOAuth, Secret: st.AccessToken}, account, nil
}

func ccLogin(ctx context.Context, o *ccOptions, stdout, stderr io.Writer) int {
	c, err := o.newClient(claudecode.Credential{}, "", "")
	if err != nil {
		return ccFail(stderr, err)
	}
	st, err := client.LoadState(o.statePath)
	if err != nil {
		return ccFail(stderr, err)
	}
	res, err := c.Login(ctx, client.LoginOptions{Manual: o.manual, SetupToken: o.setupToken})
	if err != nil {
		return ccFail(stderr, fmt.Errorf("login: %w", err))
	}
	st.SetLogin(o.target.String(), res)
	if err := st.Save(o.statePath); err != nil {
		return ccFail(stderr, err)
	}
	t := res.Tokens
	_, _ = fmt.Fprintf(stdout, "account       %s (%s)\norganization  %s (%s)\n", t.Account.Email, t.Account.UUID,
		t.Organization.Name, t.Organization.UUID)
	switch p := res.Profile; {
	case p == nil:
		_, _ = fmt.Fprintf(stdout, "plan          - (scopes %s: no profile)\n", strings.Join(t.Scopes, " "))
	case !p.Verified:
		_, _ = fmt.Fprintf(stdout, "plan          %s (%s; unconfirmed: the profile has no request-id)\n", p.Plan, p.Organization.RateLimitTier)
	default:
		_, _ = fmt.Fprintf(stdout, "plan          %s (%s)\n", p.Plan, p.Organization.RateLimitTier)
	}
	_, _ = fmt.Fprintf(stdout, "expires       %s\nstate         %s\n", t.ExpiresAt.UTC().Format(time.RFC3339), o.statePath)
	return exitOK
}

func ccRefresh(ctx context.Context, o *ccOptions, stdout, stderr io.Writer) int {
	c, err := o.newClient(claudecode.Credential{}, "", "")
	if err != nil {
		return ccFail(stderr, err)
	}
	st, err := client.LoadState(o.statePath)
	if err != nil {
		return ccFail(stderr, err)
	}
	if st.RefreshToken == "" {
		return ccFail(stderr, fmt.Errorf("no refresh token in %s: run login", o.statePath))
	}
	t, err := c.Refresh(ctx, st.RefreshToken, st.Scopes)
	if err != nil {
		return ccFail(stderr, fmt.Errorf("refresh: %w", err))
	}
	st.SetTokens(t)
	if err := st.Save(o.statePath); err != nil {
		return ccFail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "expires       %s\nstate         %s\n", t.ExpiresAt.UTC().Format(time.RFC3339), o.statePath)
	return exitOK
}

// exchangeErr describes a failed exchange (nil when it succeeded).
func exchangeErr(x *client.Exchange, err error) error {
	switch {
	case err != nil:
		return err
	case x.OK():
		return nil
	case x.Status >= 400:
		return fmt.Errorf("HTTP %d (%s)\n%s", x.Status, x.Err(), x.ErrBody)
	}
	last := x.Results[len(x.Results)-1]
	return fmt.Errorf("%s: %v", last.Err, last.Cause)
}

func ccAsk(ctx context.Context, o *ccOptions, stdout, stderr io.Writer) int {
	st, err := client.LoadState(o.statePath)
	if err != nil {
		return ccFail(stderr, err)
	}
	conv := st.Conversation
	if o.resume {
		switch {
		case conv == nil:
			return ccFail(stderr, usageErr("no conversation to resume in %s", o.statePath))
		case conv.Entrypoint != o.entrypoint || conv.Auth != o.auth:
			return ccFail(stderr, usageErr("the conversation of the state is %s with %s, not %s with %s",
				conv.Entrypoint, conv.Auth, o.entrypoint, o.auth))
		}
		if !o.set["model"] {
			o.model = conv.Model
		}
	}
	cred, account, err := o.credential(ctx, st)
	if err != nil {
		return ccFail(stderr, err)
	}
	c, err := o.newClient(cred, account, st.DeviceID)
	if err != nil {
		return ccFail(stderr, err)
	}
	s := c.NewSession()
	if o.resume {
		s = c.Resume(conv.SessionID, conv.Turns)
	}
	defer s.Close()
	x, err := s.Ask(ctx, o.args[0])
	if o.dump != "" {
		if werr := os.WriteFile(o.dump, x.Wire, 0o600); werr != nil { //nolint:gosec // G703: operator-supplied file
			return ccFail(stderr, werr)
		}
	}
	if err := exchangeErr(x, err); err != nil {
		return ccFail(stderr, err)
	}
	st.Conversation = &client.Conversation{
		SessionID: s.ID, Entrypoint: o.entrypoint, Auth: o.auth, Model: c.Options().Model, Turns: s.Turns,
	}
	if err := st.Save(o.statePath); err != nil {
		return ccFail(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, x.Text)
	return exitOK
}

func ccReplay(ctx context.Context, o *ccOptions, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(o.args[0]) //nolint:gosec // G304/G703: operator-supplied file
	if err != nil {
		return ccFail(stderr, usageErr("%v", err))
	}
	rp, err := client.ReplayCapture(raw)
	if err != nil {
		return ccFail(stderr, usageErr("replay %s: %v", o.args[0], err))
	}
	code := exitOK
	if rp.Diff == nil {
		_, _ = fmt.Fprintf(stdout, "identical sha256=%s\n", rp.SHA256())
	} else {
		sum := sha256.Sum256(rp.Got)
		_, _ = fmt.Fprintf(stdout, "%s\nsha256 want=%s got=%s\n", rp.Diff, rp.SHA256(), hex.EncodeToString(sum[:]))
		code = exitError
	}
	if !o.send {
		return code
	}
	r := rp.Request
	switch {
	case o.token != "":
		r.Credential = claudecode.Credential{Kind: claudecode.CredentialOAuth, Secret: o.token}
	case o.key != "":
		r.Credential = claudecode.Credential{Kind: claudecode.CredentialAPIKey, Secret: o.key}
	}
	o.version, o.entrypoint = r.Version, string(r.Entrypoint)
	c, err := o.newClient(r.Credential, r.AccountUUID, r.DeviceID)
	if err != nil {
		return ccFail(stderr, err)
	}
	x, err := c.Send(ctx, r)
	if err := exchangeErr(x, err); err != nil && x.Status == 0 {
		return ccFail(stderr, usageErr("send: %v", err))
	}
	_, _ = fmt.Fprintf(stdout, "sent: HTTP %d\n", x.Status)
	if x.Status >= 400 {
		_, _ = fmt.Fprintf(stderr, "%s\n", x.ErrBody)
	}
	return code
}

func ccRun(ctx context.Context, o *ccOptions, stdout, stderr io.Writer) int {
	st, err := client.LoadState(o.statePath)
	if err != nil {
		return ccFail(stderr, err)
	}
	cred, account, err := o.credential(ctx, st)
	if err != nil {
		return ccFail(stderr, err)
	}
	c, err := o.newClient(cred, account, st.DeviceID)
	if err != nil {
		return ccFail(stderr, err)
	}
	cfg := loadgen.RunConfig{Mode: loadgen.ModeConcurrency, Concurrency: o.concurrency, Duration: o.duration, Requests: o.requests}
	if o.set["rps"] {
		cfg = loadgen.RunConfig{Mode: loadgen.ModeRPS, RPS: o.rps, Duration: o.duration, Requests: o.requests}
	}
	res, err := c.Run(ctx, cfg, o.turns, o.prompt)
	if err != nil {
		return ccFail(stderr, err)
	}
	out := stdout
	if o.out != "" {
		f, err := os.Create(o.out) //nolint:gosec // G703: operator-supplied file
		if err != nil {
			return ccFail(stderr, err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	if err := loadgen.NewReport(c.ReportConfig(cfg), res, versionTag()).Write(out, o.output); err != nil {
		return ccFail(stderr, err)
	}
	return exitOK
}

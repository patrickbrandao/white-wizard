// Package client is a test client that behaves like Claude Code 2.1.308
// against the wwz (spec 015, "Contrato do wwz-load claude-code"; ADR 0074):
// inference requests leave byte for byte as Claude Code sends them (the
// profile of internal/claudecode, spec 020), over HTTP/1.1 written by hand,
// and the OAuth login follows the flow of Claude Code against the routes of
// the anthropic driver (spec 006, 16.2.1).
//
// The client only talks to the simulator: [ParseTarget] refuses the real
// Anthropic hosts before any connection, no redirect is followed to another
// host and nothing is read from an installed Claude Code (~/.claude, the
// keychain, CLAUDE_CODE_OAUTH_TOKEN).
package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// Defaults of spec 015 ("Flags comuns").
const (
	DefaultVersion      = "2.1.308"
	DefaultSystemPrompt = "You are a terse test assistant."
	DefaultModelOAuth   = "claude-sonnet-5-5"
	DefaultModelAPIKey  = "claude-opus-5-5" //nolint:gosec // G101: a model id, not a credential
	DefaultRetries      = 2
	DefaultTimeout      = 600 * time.Second
)

// Options configure a [Client].
type Options struct {
	Target     *Target
	Version    string                // profile version; "" is DefaultVersion
	Entrypoint claudecode.Entrypoint // "" is sdk-cli
	Credential claudecode.Credential // needed for inference, not for login
	// AccountUUID goes in metadata.user_id: the account of a login, "" with
	// an explicit token or an API key.
	AccountUUID string
	// Scopes and Plan of an OAuth credential (State.Scopes and State.Plan)
	// decide whether it is a subscriber, whose 429 with the subscription
	// headers is not retried (spec 015, "Transporte"). nil Scopes (an
	// explicit token) count as user:inference, and "" Plan as a subscriber.
	Scopes       []string
	Plan         string
	DeviceID     string // 64 lowercase hex, from the state
	Model        string // "" is the default of the credential
	SystemPrompt string // "" is DefaultSystemPrompt
	Arch, OS     string // X-Stainless-Arch and -OS; "" is arm64 and MacOS
	NonStream    bool   // the non-streaming fallback of a turn
	Retries      int    // retries after the first attempt
	Timeout      time.Duration
	Dial         Dialer      // nil dials TCP
	TLSConfig    *tls.Config // for https targets
}

// Client sends Claude Code requests to one target. It is safe for
// concurrent use; each [Session] has its own connection.
type Client struct {
	o     Options
	p     *claudecode.Profile
	sub   bool // a subscriber credential (see Options.Scopes)
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	newID func() string
	rand  func() float64
}

// New validates o and fills its defaults.
func New(o Options) (*Client, error) {
	if o.Target == nil {
		return nil, errors.New("no target")
	}
	if o.Version == "" {
		o.Version = DefaultVersion
	}
	p, ok := claudecode.Lookup(o.Version)
	if !ok {
		return nil, fmt.Errorf("%w %q (have %s)", claudecode.ErrUnknownVersion, o.Version,
			strings.Join(claudecode.Versions(), ", "))
	}
	if o.Entrypoint == "" {
		o.Entrypoint = claudecode.EntrypointSDKCLI
	}
	if o.Entrypoint != claudecode.EntrypointSDKCLI && o.Entrypoint != claudecode.EntrypointCLI {
		return nil, fmt.Errorf("entrypoint %q: want sdk-cli or cli", o.Entrypoint)
	}
	o.Arch = cmpOr(o.Arch, p.Archs[0])
	if !slices.Contains(p.Archs, o.Arch) {
		return nil, fmt.Errorf("arch %q: want %s", o.Arch, strings.Join(p.Archs, " or "))
	}
	o.OS = cmpOr(o.OS, p.OSes[0])
	if !slices.Contains(p.OSes, o.OS) {
		return nil, fmt.Errorf("os %q: want %s", o.OS, strings.Join(p.OSes, ", "))
	}
	if o.Model == "" {
		o.Model = DefaultModelOAuth
		if o.Credential.Kind == claudecode.CredentialAPIKey {
			o.Model = DefaultModelAPIKey
		}
	}
	o.SystemPrompt = cmpOr(o.SystemPrompt, DefaultSystemPrompt)
	if o.Retries < 0 {
		return nil, fmt.Errorf("retries %d: must be ≥ 0", o.Retries)
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	return &Client{
		o: o, p: p, sub: subscriber(o), now: time.Now, sleep: sleepCtx, newID: NewSessionID, rand: mrand.Float64,
	}, nil
}

// subscriber reports whether o authenticates as a subscriber of Claude
// Code: an OAuth credential with the user:inference scope whose plan is not
// enterprise.
func subscriber(o Options) bool {
	return o.Credential.Kind == claudecode.CredentialOAuth &&
		(o.Scopes == nil || slices.Contains(o.Scopes, ScopeInference)) && o.Plan != "enterprise"
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Options returns the options with their defaults.
func (c *Client) Options() Options { return c.o }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewSessionID returns a random lowercase UUID v4, like crypto.randomUUID.
func NewSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := fmt.Sprintf("%x", b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Turn is one finished turn of a conversation.
type Turn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

// Session is one Claude Code session: its id goes in
// X-Claude-Code-Session-Id and metadata.user_id, and its requests share one
// keep-alive connection.
type Session struct {
	c     *Client
	ID    string
	Turns []Turn
	conn  *conn
}

// NewSession starts a session with a new id.
func (c *Client) NewSession() *Session { return c.Resume(c.newID(), nil) }

// Resume continues the session id after turns (claude --resume).
func (c *Client) Resume(id string, turns []Turn) *Session {
	return &Session{c: c, ID: id, Turns: slices.Clone(turns)}
}

// Close closes the connection of the session.
func (s *Session) Close() { s.drop() }

func (s *Session) drop() {
	if s.conn != nil {
		s.conn.close()
		s.conn = nil
	}
}

// Exchange is what one ask (or one replay send) did on the wire.
type Exchange struct {
	// Text is the assistant text of the turn.
	Text string
	// Results has one measure per attempt of every request, in order.
	Results []loadgen.Result
	// Wire has every byte written, in order (--dump).
	Wire []byte
	// Status and ErrBody are those of the last response (ErrBody only
	// with status ≥ 400).
	Status  int
	ErrBody []byte
}

// OK reports whether the last request succeeded.
func (x *Exchange) OK() bool { return len(x.Results) > 0 && x.Results[len(x.Results)-1].OK() }

// Err returns the error class of the last request ("" on success).
func (x *Exchange) Err() string {
	if len(x.Results) == 0 {
		return ""
	}
	return x.Results[len(x.Results)-1].Err
}

// Ask sends one turn with text as the user message. In the interactive
// entrypoint (cli) the first turn of a session is preceded by the quota
// probe and turns carry safeguards, as Claude Code does. On success the turn
// is appended to s.Turns. The error is only for a request that cannot be
// built; HTTP and transport failures are in the Exchange.
func (s *Session) Ask(ctx context.Context, text string) (*Exchange, error) {
	x := &Exchange{}
	cli := s.c.o.Entrypoint == claudecode.EntrypointCLI
	if cli && len(s.Turns) == 0 {
		if _, err := s.send(ctx, s.c.request(claudecode.TypeQuota, s.ID, nil), x); err != nil || !x.OK() {
			return x, err
		}
	}
	typ := claudecode.TypeTurn
	if cli {
		typ = claudecode.TypeTurnSafeguards
	}
	r := s.c.request(typ, s.ID, s.c.messages(s.ID, s.Turns, text))
	answer, err := s.send(ctx, r, x)
	if err != nil {
		return x, err
	}
	if x.OK() {
		x.Text = answer
		s.Turns = append(s.Turns, Turn{User: text, Assistant: answer})
	}
	return x, nil
}

// Send sends r once (with the retries) to the target of the client, with
// the Host and the prefix of the target (replay --send).
func (c *Client) Send(ctx context.Context, r claudecode.Request) (*Exchange, error) {
	s := c.Resume(r.SessionID, nil)
	defer s.Close()
	r.Host, r.Prefix = c.o.Target.host, c.o.Target.prefix
	x := &Exchange{}
	text, err := s.send(ctx, r, x)
	x.Text = text
	return x, err
}

// request returns the inputs of a request of type t in session id.
func (c *Client) request(t claudecode.Type, id string, msgs []claudecode.Value) claudecode.Request {
	r := claudecode.Request{
		Version: c.p.Version, Entrypoint: c.o.Entrypoint, Type: t, Credential: c.o.Credential,
		Prefix: c.o.Target.prefix, Host: c.o.Target.host,
		SessionID: id, DeviceID: c.o.DeviceID, AccountUUID: c.o.AccountUUID,
		Arch: c.o.Arch, OS: c.o.OS, Model: c.o.Model, Messages: msgs,
		SystemPrompt: c.o.SystemPrompt, NonStream: c.o.NonStream,
	}
	if t == claudecode.TypeTurnSafeguards {
		r.Safeguards = safeguards
	}
	return r
}

// send builds and sends r, retrying as the SDK of Claude Code does
// (X-Stainless-Retry-Count incremented, same body). Every attempt goes in
// x. It returns the assistant text of the last attempt.
func (s *Session) send(ctx context.Context, r claudecode.Request, x *Exchange) (string, error) {
	for attempt := 0; ; attempt++ {
		r.RetryCount = attempt
		head, body, err := claudecode.Build(r)
		if err != nil {
			return "", err
		}
		raw := append(head, body...)
		x.Wire = append(x.Wire, raw...)
		res, resp, text, errBody := s.do(ctx, raw, r.Type == claudecode.TypeHello)
		x.Results = append(x.Results, res)
		x.Status, x.ErrBody = res.Status, errBody
		if attempt >= s.c.o.Retries || !shouldRetry(&res, resp, s.c.sub) {
			return text, nil
		}
		if s.c.sleep(ctx, s.c.backoff(attempt, resp)) != nil {
			return text, nil
		}
	}
}

// do sends raw on the connection of the session (dialing it if needed) and
// measures the response as the run does (spec 015, "Medidas no cliente").
// resp is nil when no response head arrived.
func (s *Session) do(ctx context.Context, raw []byte, head bool) (res loadgen.Result, resp *Response, text string, errBody []byte) {
	ctx, cancel := context.WithTimeout(ctx, s.c.o.Timeout)
	defer cancel()
	res.Start = time.Now()
	defer func() { res.E2E = time.Since(res.Start) }()
	if s.conn == nil {
		cn, err := dial(ctx, s.c.o.Dial, s.c.o.Target, s.c.o.TLSConfig)
		if err != nil {
			res.Err, res.Cause = dialClass(ctx, err), err
			return res, nil, "", nil
		}
		s.conn = cn
	}
	cn := s.conn
	defer cn.arm(ctx)()
	resp, err := cn.exchange(raw, head)
	if err != nil {
		s.drop()
		res.Err, res.Cause = transportClass(ctx, err), err
		return res, nil, "", nil
	}
	res.Status = resp.Status
	text, errBody, err = s.body(ctx, resp, &res)
	if err != nil || !resp.drain() {
		s.drop()
	}
	return res, resp, text, errBody
}

// body reads the body of resp into res.
func (s *Session) body(ctx context.Context, resp *Response, res *loadgen.Result) (text string, errBody []byte, err error) {
	d := decoder{res: res}
	ct, _ := resp.Get("Content-Type")
	switch {
	case resp.Status < 200 || resp.Status > 299:
		errBody, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		res.Err = loadgen.HTTPError(resp.Status, errBody)
		res.RetryAfterMS, _ = resp.Get("retry-after-ms")
		res.RetryAfter, _ = resp.Get("retry-after")
		return "", errBody, err
	case strings.HasPrefix(ct, "text/event-stream"):
		var last time.Time
		err = readSSE(resp.Body, func(name string, data []byte) error {
			content, err := d.event(name, data)
			if content {
				now := time.Now()
				res.ContentEvents++
				if res.ContentEvents == 1 {
					res.TTFT = now.Sub(res.Start)
				} else {
					res.ITL = append(res.ITL, now.Sub(last))
				}
				last = now
			}
			return err
		})
		if errors.Is(err, io.EOF) {
			err = nil
		}
	default:
		var body []byte
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err == nil && len(body) > 0 {
			err = d.whole(body)
		}
		if err == nil && len(body) == 0 {
			d.done = true // HEAD /api/hello, or an empty 2xx
		}
	}
	var se *streamError
	switch {
	case errors.As(err, &se):
		res.Err, res.Cause = se.Error(), err
	case errors.Is(err, errParse):
		res.Err, res.Cause = loadgen.ErrParse, err
	case err != nil && !d.done:
		res.Err, res.Cause = transportClass(ctx, err), err
	case !d.done:
		res.Err = loadgen.ErrStreamTruncated
	default:
		if res.ContentEvents == 0 || !strings.HasPrefix(ct, "text/event-stream") {
			res.TTFT = time.Since(res.Start)
		}
		d.finish()
		return d.text.String(), nil, err
	}
	return "", nil, err
}

// dialClass classifies a failed connection: timeout, canceled or connect.
func dialClass(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return transportClass(ctx, err)
	}
	return loadgen.ErrConnect
}

// transportClass classifies a failure after the connection: canceled,
// timeout, parse or reset.
func transportClass(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return loadgen.ErrCanceled
	case ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded):
		return loadgen.ErrTimeout
	case errors.Is(err, errParse):
		return loadgen.ErrParse
	}
	return loadgen.ErrReset
}

// Subscription headers: a 429 of a subscriber that carries one of them is
// the limit of the subscription, which Claude Code does not retry.
const (
	hdrClaim          = "anthropic-ratelimit-unified-representative-claim"
	hdrOverageStatus  = "anthropic-ratelimit-unified-overage-status"
	hdrOverageDisable = "anthropic-ratelimit-unified-overage-disabled-reason"
)

var subscriptionHeaders = []string{hdrClaim, hdrOverageStatus, hdrOverageDisable}

// shouldRetry is the retry rule of spec 015 ("Transporte"), the shouldRetry
// of Claude Code 2.1.308; the first rule that matches wins: (1) connect and
// reset retry; (2) a 429 whose overage was disabled for the organization
// never retries; (3) a 429 of a subscriber with a subscription header never
// retries; (4) x-should-retry: true retries, except for a subscriber, where
// it forces nothing; (5) x-should-retry: false never retries; (6) 408, 409,
// 429 and 5xx retry.
func shouldRetry(res *loadgen.Result, resp *Response, subscriber bool) bool {
	if resp == nil {
		return res.Err == loadgen.ErrConnect || res.Err == loadgen.ErrReset
	}
	has := func(name string) bool { v, _ := resp.Get(name); return v != "" }
	if res.Status == 429 {
		v, _ := resp.Get(hdrOverageDisable)
		switch v {
		case "org_spend_cap_reached", "org_level_disabled_until":
			return false
		case "out_of_credits", "org_level_disabled", "org_service_level_disabled":
			if !has(hdrClaim) && !has(hdrOverageStatus) {
				return false
			}
		}
		if subscriber && slices.ContainsFunc(subscriptionHeaders, has) {
			return false
		}
	}
	switch v, _ := resp.Get("x-should-retry"); {
	case v == "true" && !subscriber:
		return true
	case v == "false":
		return false
	}
	return res.Status == 408 || res.Status == 409 || res.Status == 429 || res.Status >= 500
}

// backoff is the wait before retry attempt+1, as the Stainless SDK: the
// retry-after-ms or retry-after of the response when in (0, 60s), else
// 0.5 s × 2^attempt up to 8 s, minus up to 25 % of jitter.
func (c *Client) backoff(attempt int, resp *Response) time.Duration {
	if resp != nil {
		for _, h := range []struct {
			name string
			unit time.Duration
		}{{"retry-after-ms", time.Millisecond}, {"retry-after", time.Second}} {
			v, _ := resp.Get(h.name)
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && time.Duration(f*float64(h.unit)) < time.Minute {
				return time.Duration(f * float64(h.unit))
			}
		}
	}
	d := math.Min(0.5*math.Pow(2, float64(attempt)), 8)
	return time.Duration(d * (1 - c.rand()*0.25) * float64(time.Second))
}

// Environment of the simulated machine. The fixtures normalize the working
// directory to /work; the client uses it as is.
const (
	workDir     = "/work"
	totalTokens = "<total_tokens>15000000 tokens left</total_tokens>"
)

// platformOf returns the platform, shell and OS version the environment
// block shows for X-Stainless-OS (only MacOS was captured; the others are
// inferred).
func platformOf(os string) (platform, shell, version string) {
	switch os {
	case "Linux":
		return "linux", "bash", "Linux 6.8.0"
	case "Windows":
		return "win32", "bash", "Windows 11"
	}
	return "darwin", "zsh", "Darwin 27.0.0"
}

// displayName turns claude-sonnet-5-5 into "Sonnet 5.5"; other ids are
// kept.
func displayName(model string) string {
	rest, ok := strings.CutPrefix(model, "claude-")
	parts := strings.Split(rest, "-")
	if !ok || len(parts) < 2 || parts[0] == "" {
		return model
	}
	return strings.ToUpper(parts[0][:1]) + parts[0][1:] + " " + strings.Join(parts[1:], ".")
}

// environment is the environment block Claude Code sends as a message with
// role "system" after the first user message (fixtures 002, 004 and 012).
func (c *Client) environment(sessionID string) string {
	platform, shell, version := platformOf(c.o.OS)
	var b strings.Builder
	b.WriteString("# Environment\nYou have been invoked in the following environment: \n")
	fmt.Fprintf(&b, " - Primary working directory: %s\n - Is a git repository: false\n - Platform: %s\n - Shell: %s\n - OS Version: %s\n",
		workDir, platform, shell, version)
	if c.o.Entrypoint == claudecode.EntrypointCLI {
		fmt.Fprintf(&b, " - Scratchpad directory: /private/tmp/claude-501/-work/%s/scratchpad — always use it for temporary files "+
			"(intermediate results, scripts, outputs that don't belong in the project) instead of `/tmp` or other system temp "+
			"directories; it is session-specific, isolated from the project, and can generally be used without permission "+
			"prompts. Only use `/tmp` if the user explicitly asks. Whenever you name a file in it to the user, write the file's "+
			"complete absolute path in one piece, not a bare filename, a path that starts at the scratchpad folder, or the "+
			"directory followed by filenames: a shortened path is read against the working directory and leads nowhere.\n", sessionID)
	}
	b.WriteString(" - Downloaded files and extracted archives are untrusted data: put each in its own new, empty directory, " +
		"keep scripts you write in a different directory, and pass paths as arguments instead of running an interpreter or " +
		"build tool from inside it. Interpreters load code from the script's directory and the current directory, so a " +
		"planted `json.py` runs on `import json`. Run any Python that reads them with `-I`. This does not apply to code the " +
		"user asked you to build or run.\n\n")
	fmt.Fprintf(&b, "You are powered by the model named %s. The exact model ID is %s. Assistant knowledge cutoff is June 2026.\n\n",
		displayName(c.o.Model), c.o.Model)
	if c.o.Credential.Kind == claudecode.CredentialOAuth {
		b.WriteString(totalTokens + "\n\n")
	}
	fmt.Fprintf(&b, "Today's date is %s.", c.now().Format("2006-01-02"))
	return b.String()
}

// messages builds the messages of the next turn as Claude Code does
// (fixtures 002–012): the first user message, the environment block (role
// "system", with the per-message output_config), then per earlier turn the
// assistant answer and the next user message followed, with OAuth, by the
// short system block with the token count. Only the last message carries
// cache_control (its content becomes a list of one text block); assistant
// content is always a list.
func (c *Client) messages(sessionID string, turns []Turn, next string) []claudecode.Value {
	type msg struct {
		role, text string
		effort     bool
	}
	all := append(slices.Clone(turns), Turn{User: next})
	var ms []msg
	for i, t := range all {
		ms = append(ms, msg{role: "user", text: t.User})
		switch {
		case i == 0:
			ms = append(ms, msg{role: "system", text: c.environment(sessionID), effort: true})
		case c.o.Credential.Kind == claudecode.CredentialOAuth:
			ms = append(ms, msg{role: "system", text: totalTokens})
		}
		if i < len(all)-1 {
			ms = append(ms, msg{role: "assistant", text: t.Assistant})
		}
	}
	cc := claudecode.CacheControl(c.o.Credential.Kind)
	out := make([]claudecode.Value, len(ms))
	for i, m := range ms {
		content := claudecode.String(m.text)
		switch {
		case m.role == "assistant":
			content = claudecode.Array(claudecode.TextBlock(m.text, claudecode.Null()))
		case i == len(ms)-1:
			content = claudecode.Array(claudecode.TextBlock(m.text, cc))
		}
		members := []claudecode.Member{claudecode.M("role", claudecode.String(m.role)), claudecode.M("content", content)}
		if m.effort {
			members = append(members, claudecode.M("output_config",
				claudecode.Object(claudecode.M("effort", claudecode.String(c.p.DefaultEffort)))))
		}
		out[i] = claudecode.Object(members...)
	}
	return out
}

// safeguards is the safeguards value of the interactive turn (fixture
// 012): the classifier context of a session in /work with every tool
// narrowed away.
var safeguards, _ = claudecode.Parse([]byte(safeguardsJSON))

var safeguardsJSON = `[{"type":"dangerous_tool_use","classifier_context":{"v":1,"permission_mode":"auto","platform":"macos",` +
	`"live_cwd":"/work","home_dir":"/home/user","rule_roots":{"userSettings":"/cfg","projectSettings":"/work",` +
	`"localSettings":"/work","flagSettings":"/work","policySettings":"/work","cliArg":"/work","command":"/work",` +
	`"session":"/work","toolsNarrowing":"/work","mcpServerPolicy":"/work","hostCredential":"/work"},` +
	`"trusted_directories":{"primary":{"path":"/work","resolved":["/work"]},"additional":[],"network":[],` +
	`"block_reads_outside_working_directories":false},"rules":{"allow":[],"deny":[` + denyRules + `],"ask":[]},` +
	`"auto_mode":{"allow":[],"soft_deny":[],"hard_deny":[],"environment":[]},"artifact_consent_holdback":false,` +
	`"case_insensitive_paths":false,"restricted":false,"is_remote_mode":false,"classify_all_shell":false,` +
	`"user_identity":"user","git_state":{"cwd":"/work","root":null,"branch":null,"default_branch":null,"status":null,` +
	`"visibility":null,"error":"not_a_repo"}}}]`

// denyRules lists the tools narrowed away by --tools "" in fixture 012.
var denyRules = func() string {
	tools := []string{
		"Agent", "Bash", "ExitPlanMode", "Read", "Edit", "Write", "NotebookEdit", "memory_list", "memory_read",
		"memory_write", "WebFetch", "TodoWrite", "ReportFindings", "WebSearch", "TaskStop", "AskUserQuestion",
		"SendFeedback", "Skill", "EnterPlanMode", "DesignSync", "ClaudeDesign", "Projects", "TaskCreate", "TaskGet",
		"TaskUpdate", "TaskList", "Artifact", "ArtifactComments", "ArtifactData", "ArtifactCheck", "AppifactRepl",
		"LSP", "EnterWorktree", "ExitWorktree", "SendMessage", "SendFile", "ListAgents", "ShareOnboardingGuide",
		"Poll", "Workflow", "CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "RemoteTrigger",
		"SearchMcpRegistry", "SuggestConnectors", "ShowOnboardingRolePicker", "OfferChromeSetup", "ReadNotifications",
		"enable__mcp__claude-in-chrome", "enable__mcp__remote-devices__Claude_Browser",
		"enable__mcp__remote-devices__computer", "request_computer", "ListConnectors", "SearchPlugins", "SearchSkills",
		"SuggestPluginInstall", "SuggestSkills", "ListPlugins", "ListSkills", "Monitor", "SendUserMessage",
		"SendUserFile", "propose_skills", "ProposeGoal", "PublishPlugin", "PushNotification", "FetchInboxMessage",
		"EndConversation", "ListMcpResourcesTool", "ReadMcpResourceTool", "ReadMcpResourceDirTool", "ToolSearch",
		"WaitForMcpServers", "GetTask", "Glob", "Grep",
	}
	parts := make([]string, len(tools))
	for i, t := range tools {
		parts[i] = `{"rule":"` + t + `","source":"toolsNarrowing"}`
	}
	return strings.Join(parts, ",")
}()

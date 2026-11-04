package drift

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var idRe = regexp.MustCompile(`"id": ?"[^"]*"`)

func TestUpstreamForm(t *testing.T) {
	cases := map[string]string{
		"gw-0123456789ab-cdef01":            formGW,
		"0123456789abcdef0123456789abcdef":  formHex32,
		"chatcmpl-0123456789abcdef":         formChatcmpl,
		"chatcmpl-":                         formChatcmpl,
		"":                                  formUnknown,
		"gw-xyz":                            formUnknown,
		"0123456789abcdef0123456789abcde":   formUnknown,
		idLong:                              formUnknown, // id longo (53 caracteres, ADR 0060)
		"gw-0123456789ab-cdef01-0123456789": formUnknown,
	}
	for id, want := range cases {
		if got := upstreamForm(id); got != want {
			t.Errorf("upstreamForm(%q) = %s, want %s", id, got, want)
		}
	}
	if bodyID([]byte(`{"id":"x"}`)) != "x" || bodyID([]byte(`{`)) != "" || bodyID([]byte(`{"id":1}`)) != "" {
		t.Error("bodyID")
	}
}

const (
	idChatcmpl = "chatcmpl-0123456789abcdef"
	idGW       = "gw-0123456789ab-cdef01"
	// idLong has the 53 characters of the long ids seen on other upstreams.
	idLong = "msg_0123456789abcdef0123456789abcdef0123456789abcdef0"
)

// TestOpenCodeLongIDUnreproduced: three sessions that answer with long ids
// leave the provider skipped with upstream_form_unreproduced, exit 0.
func TestOpenCodeLongIDUnreproduced(t *testing.T) {
	if len(idLong) != 53 {
		t.Fatalf("idLong has %d characters", len(idLong))
	}
	rep, sessions := opencodeRun(t, func(int32, *int) string { return idLong }, nil)
	p := providerReport(t, rep, ProviderOpenCode)
	if p.Status != StatusSkipped || p.Reason != ReasonUpstreamFormUnreproduced || len(p.Probes) != 0 || p.SpendUSD <= 0 {
		t.Fatalf("%+v", p)
	}
	if len(sessions) != 2+MaxNewSessions || p.Session != "ses_new3" || ExitCode(rep, nil) != ExitOK {
		t.Errorf("sessions %v, report %s, exit %d", sessions, p.Session, ExitCode(rep, nil))
	}
}

// opencodeRun runs the opencode models and chat probes against a fake real
// provider whose chat id (and status) is chosen by the chat call count.
func opencodeRun(t *testing.T, idOf func(n int32, status *int) string, client *http.Client) (*Report, []string) {
	t.Helper()
	upstream := startEmbedded(t)
	var chats atomic.Int32
	urls, fs := fakes(t, upstream, map[string]mutation{
		ProviderOpenCode: func(probe string, status *int, _ http.Header, body []byte) []byte {
			if probe != ProbeChat {
				return body
			}
			return idRe.ReplaceAll(body, []byte(`"id":"`+idOf(chats.Add(1), status)+`"`))
		},
	})
	var n atomic.Int32
	rep, err := Run(context.Background(), Options{
		Providers: []string{ProviderOpenCode}, Probes: []string{ProbeModels, ProbeChat},
		Keys: allKeys(sentinel), BaseURLs: urls, BudgetFile: filepath.Join(t.TempDir(), "spend.json"),
		Timeout: 10 * time.Second, Session: "ses_first", Client: client,
		NewSession: func() string { return "ses_new" + string(rune('0'+n.Add(1))) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, h := range fs[ProviderOpenCode].seen {
		sessions = append(sessions, h.Get("x-opencode-session"))
	}
	return rep, sessions
}

func TestOpenCodeGWFormKeepsTheSession(t *testing.T) {
	rep, sessions := opencodeRun(t, func(int32, *int) string { return idGW }, nil)
	p := providerReport(t, rep, ProviderOpenCode)
	if p.Status != StatusOK || p.Session != "ses_first" || ExitCode(rep, nil) != ExitOK {
		t.Errorf("%+v", p)
	}
	for _, s := range sessions {
		if s != "ses_first" {
			t.Errorf("session %s, want one session per run", s)
		}
	}
}

func TestOpenCodeChangesSessionOnChatcmpl(t *testing.T) {
	// Any upstream other than gw- (chatcmpl-, 32 hex, a long id) is retried.
	rep, sessions := opencodeRun(t, func(n int32, _ *int) string {
		switch n {
		case 1:
			return idChatcmpl
		case 2:
			return "0123456789abcdef0123456789abcdef0"
		}
		return idGW
	}, nil)
	p := providerReport(t, rep, ProviderOpenCode)
	if p.Status != StatusOK || p.Session != "ses_new2" || len(p.Probes[1].Divergences) != 0 {
		t.Fatalf("%+v", p)
	}
	// models, chat, two new sessions, the chat probe again.
	if strings.Join(sessions, ",") != "ses_first,ses_first,ses_new1,ses_new2,ses_new2" {
		t.Errorf("sessions %v", sessions)
	}
	if p.SpendUSD <= 0 {
		t.Errorf("spend %v", p.SpendUSD)
	}
}

func TestOpenCodeUpstreamFormUnreproduced(t *testing.T) {
	rep, sessions := opencodeRun(t, func(int32, *int) string { return idChatcmpl }, nil)
	p := providerReport(t, rep, ProviderOpenCode)
	if p.Status != StatusSkipped || p.Reason != ReasonUpstreamFormUnreproduced || len(p.Probes) != 0 || p.SpendUSD <= 0 {
		t.Fatalf("%+v", p)
	}
	if len(sessions) != 2+MaxNewSessions || p.Session != "ses_new3" {
		t.Errorf("sessions %v, report %s", sessions, p.Session)
	}
	if ExitCode(rep, nil) != ExitOK || rep.Summary.Skipped != 7 { // 5 provedores sem chave (inclui gemini e opencode-google), azure fora do padrão + opencode
		t.Errorf("exit %d summary %+v", ExitCode(rep, nil), rep.Summary)
	}
	_, txt := render(t, rep)
	if !strings.Contains(txt, "upstream_form_unreproduced") || !strings.Contains(txt, "sessão=ses_new3") {
		t.Errorf("text:\n%s", txt)
	}
}

func TestOpenCodeDefaultNewSession(t *testing.T) {
	r := &runner{}
	if s := r.newSession(); !strings.HasPrefix(s, "ses_") || len(s) != 68 {
		t.Errorf("default = %q", s)
	}
}

// failNthPost fails the nth POST sent through it.
type failNthPost struct {
	n    int32
	seen atomic.Int32
}

func (f *failNthPost) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && f.seen.Add(1) == f.n {
		return nil, errors.New("rede caiu " + sentinel)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestOpenCodeChangeSessionFailures(t *testing.T) {
	// POSTs through the client: real chat (1), wwz chat (2), retry (3).
	rep, _ := opencodeRun(t, func(int32, *int) string { return idChatcmpl },
		&http.Client{Transport: &failNthPost{n: 3}})
	p := providerReport(t, rep, ProviderOpenCode)
	if p.Status != StatusError || !strings.Contains(p.Probes[1].Reason, "rede caiu ***") {
		t.Fatalf("%+v", p)
	}
	// Non-2xx answers to the retries cost nothing and are not the gw- form:
	// the sessions run out.
	rep, _ = opencodeRun(t, func(n int32, status *int) string {
		if n == 1 {
			return idChatcmpl
		}
		*status = http.StatusBadGateway
		return "x"
	}, nil)
	if p := providerReport(t, rep, ProviderOpenCode); p.Reason != ReasonUpstreamFormUnreproduced {
		t.Errorf("%+v", p)
	}
}

func TestOpenCodeRealErrorIsNotAnUpstreamForm(t *testing.T) {
	// A non-2xx real chat has no id to classify: plain drift, no retries.
	rep, sessions := opencodeRun(t, func(_ int32, status *int) string {
		*status = http.StatusBadGateway
		return "x"
	}, nil)
	p := providerReport(t, rep, ProviderOpenCode)
	if p.Status != StatusDrift || p.Session != "ses_first" || len(sessions) != 2 {
		t.Errorf("%+v %v", p, sessions)
	}
}

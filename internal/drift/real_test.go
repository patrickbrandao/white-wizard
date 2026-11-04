package drift

// Regression tests with the real answers of the first drift (task 167,
// tmp/drift/2026-10-31-raw/), copied to testdata/real/<provider>/ with the
// account ids redacted. The "-wwz" files are the answers of the wwz of that
// day (before tasks 170–175; opencode-google: of task 297), so they carry
// driver divergences; the tests check what the comparison reports, not
// that wwz matches.

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// rawAnswer reads testdata/real/<provider>/<probe>-<side>.txt, written by
// --save-raw: "HTTP <status>", "Name: value" lines, a blank line and the
// body.
func rawAnswer(t *testing.T, provider, probe, side string) answer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "real", provider, probe+"-"+side+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := bytes.Cut(data, []byte("\n\n"))
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Scan()
	status, err := strconv.Atoi(strings.TrimPrefix(sc.Text(), "HTTP "))
	if err != nil {
		t.Fatalf("%s/%s-%s: status line %q", provider, probe, side, sc.Text())
	}
	h := http.Header{}
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ": ")
		h.Add(k, v)
	}
	return answer{status: status, header: h, body: body}
}

func rawSignature(t *testing.T, provider, probe, side string) Signature {
	t.Helper()
	a := rawAnswer(t, provider, probe, side)
	var s Signature
	var err error
	if probe == ProbeModels {
		s, err = SignModels(a.status, a.header, a.body, providers[provider].DefaultModel)
	} else {
		s, err = Sign(a.status, a.header, a.body)
	}
	if err != nil {
		t.Fatalf("%s/%s-%s: %v", provider, probe, side, err)
	}
	return s
}

// rawDivergences compares the real answer with the wwz one of that day.
func rawDivergences(t *testing.T, provider, probe string) []Divergence {
	t.Helper()
	ds := Compare(rawSignature(t, provider, probe, "real"), rawSignature(t, provider, probe, "wwz"))
	markIgnored(DefaultIgnores(), provider, probe, ds)
	return ds
}

func findDivergence(ds []Divergence, kind, path string) bool {
	for _, d := range ds {
		if d.Kind == kind && d.Path == path && !d.Ignored {
			return true
		}
	}
	return false
}

// recorded are the providers with real answers under testdata/real/: all
// but azure, which has no real key (ADR 0043).
func recorded() []string {
	return slices.DeleteFunc(slices.Clone(ProviderNames), func(p string) bool { return p == ProviderAzure })
}

// listing are the recorded providers whose models probe is a real list:
// the Gemini route of OpenCode Go has none (404 without body).
func listing() []string {
	return slices.DeleteFunc(recorded(), func(p string) bool { return p == ProviderOpenCodeGoogle })
}

// TestRealOpenCodeGoogle: the first real drift of the Gemini route of
// OpenCode Go (task 156) against the wwz of task 297 (its "-wwz" files):
// the 404 without body (and with x-opencode-log-id) to GET
// .../google/v1beta/models and the translated reasoning in
// thoughtsTokenCount, so no probe diverges.
func TestRealOpenCodeGoogle(t *testing.T) {
	p := ProviderOpenCodeGoogle
	for _, side := range []string{"real", "wwz"} {
		a := rawAnswer(t, p, ProbeModels, side)
		if a.status != http.StatusNotFound || len(bytes.TrimSpace(a.body)) != 0 || a.header.Get("X-Opencode-Log-Id") == "" {
			t.Errorf("models %s: %d %q %v", side, a.status, a.body, a.header)
		}
	}
	if a := rawAnswer(t, p, ProbeChat, "wwz"); !bytes.Contains(a.body, []byte(`"thoughtsTokenCount":`)) {
		t.Errorf("chat wwz: %s", a.body)
	}
	for _, probe := range []string{ProbeModels, ProbeChat, ProbeChatStream, ProbeErrorAuth, ProbeErrorModel} {
		if ds := rawDivergences(t, p, probe); len(ds) != 0 {
			t.Errorf("%s: %s", probe, kinds(ds))
		}
	}
}

// TestRealSelf: every real answer compared with itself has no divergence,
// so parsing (indented JSON, keep-alive spaces, padded SSE data) is stable.
func TestRealSelf(t *testing.T) {
	for _, p := range recorded() {
		for _, probe := range ProbeNames {
			if responsesProbe(probe) && providers[p].ResponsesPath == "" {
				continue
			}
			s := rawSignature(t, p, probe, "real")
			if ds := Compare(s, s); len(ds) != 0 {
				t.Errorf("%s/%s: %s", p, probe, kinds(ds))
			}
		}
	}
}

// TestRealModelsProbeItem: /models keeps the top level and only the item
// of the probe model, which every real list has; the fused list would
// report fields of other models.
func TestRealModelsProbeItem(t *testing.T) {
	for _, p := range listing() {
		a := rawAnswer(t, p, ProbeModels, "real")
		model := providers[p].DefaultModel
		real := rawSignature(t, p, ProbeModels, "real")
		if real.Item != model || !real.ItemFound {
			t.Errorf("%s: probe model %s not in the real list", p, model)
			continue
		}
		fused, err := Sign(a.status, a.header, a.body)
		if err != nil {
			t.Fatal(err)
		}
		// OpenRouter and OpenCode list heterogeneous models: the fused
		// shape has fields the probe item lacks.
		hetero := p == ProviderOpenRouter || p == ProviderOpenCode
		if len(fused.Body) < len(real.Body) || hetero != (len(fused.Body) > len(real.Body)) {
			t.Errorf("%s: fused list has %d fields, the item %d", p, len(fused.Body), len(real.Body))
		}
		// A wwz listing only the probe item, with the real shape, is not
		// a divergence.
		only, found, ok := focusModel(a.body, model)
		if !found || !ok {
			t.Fatalf("%s: focusModel", p)
		}
		sim, err := SignModels(a.status, a.header, only, model)
		if err != nil {
			t.Fatal(err)
		}
		if ds := Compare(real, sim); len(ds) != 0 {
			t.Errorf("%s: real list × its probe item: %s", p, kinds(ds))
		}
	}
}

// TestRealModelsNoFusion: against the wwz of that day, the item fields
// reported are fields of the probe item, never of other models; a wwz
// without the probe model reports the item itself, not its fields.
func TestRealModelsNoFusion(t *testing.T) {
	for _, p := range listing() {
		model := providers[p].DefaultModel
		a := rawAnswer(t, p, ProbeModels, "real")
		item, _, _ := focusModel(a.body, model)
		own, err := JSONShape(item)
		if err != nil {
			t.Fatal(err)
		}
		itemPaths := index(own)
		ds := rawDivergences(t, p, ProbeModels)
		wwz := rawSignature(t, p, ProbeModels, "wwz")
		if !wwz.ItemFound {
			// The wwz of that day served another id (gpt-4.1-nano,
			// claude-haiku-4-5, glm-4.6).
			if !findDivergence(ds, KindMissingField, "data[id="+model+"]") {
				t.Errorf("%s: missing item not reported: %s", p, kinds(ds))
			}
		}
		for _, d := range ds {
			if !strings.HasPrefix(d.Path, "data[]") {
				continue
			}
			if !wwz.ItemFound {
				t.Errorf("%s: item field reported without the item: %+v", p, d)
			}
			if d.Kind != KindExtraField {
				if _, ok := itemPaths[d.Path]; !ok {
					t.Errorf("%s: %s %s is not a field of %s", p, d.Kind, d.Path, model)
				}
			}
		}
	}
}

// TestRealStreamAllDifferences: one pass lists every stream difference,
// e.g. Anthropic's missing ping and the different message_delta.
func TestRealStreamAllDifferences(t *testing.T) {
	ds := rawDivergences(t, ProviderAnthropic, ProbeChatStream)
	var ping, delta, start bool
	for _, d := range ds {
		switch {
		case d.Kind == KindEventSequence && d.Path == "events[2]" && strings.HasPrefix(d.Real, "event=ping "):
			ping = true
		case strings.HasPrefix(d.Path, "events[5:message_delta]."):
			delta = true
		case strings.HasPrefix(d.Path, "events[0:message_start]"):
			start = true
		}
	}
	if !ping || !delta || !start {
		t.Errorf("ping=%v message_delta=%v message_start=%v\n%s", ping, delta, start, kinds(ds))
	}
	// OpenAI: the wwz stream of that day lacked chunk fields; every chunk
	// position is compared, not only the first.
	ds = rawDivergences(t, ProviderOpenAI, ProbeChatStream)
	positions := map[string]bool{}
	for _, d := range ds {
		if i := strings.Index(d.Path, "]"); strings.HasPrefix(d.Path, "events[") && i > 0 {
			positions[d.Path[:i+1]] = true
		}
	}
	if len(positions) < 2 {
		t.Errorf("openai stream: only %v\n%s", positions, kinds(ds))
	}
}

// TestRealSSELeadingComment: the ": OPENROUTER PROCESSING" comment that
// the wwz of that day sent before the first event is not a divergence.
func TestRealSSELeadingComment(t *testing.T) {
	a := rawAnswer(t, ProviderOpenRouter, ProbeChatStream, "wwz")
	if !bytes.HasPrefix(a.body, []byte(": OPENROUTER PROCESSING")) {
		t.Fatal("fixture lost its leading comment")
	}
	for _, d := range rawDivergences(t, ProviderOpenRouter, ProbeChatStream) {
		if strings.Contains(d.Real+d.WWZ, EventComment) {
			t.Errorf("comment reported: %+v", d)
		}
	}
}

// TestRealHeaders: provider headers a router may read are compared,
// account headers included; proxy headers are not.
func TestRealHeaders(t *testing.T) {
	for _, c := range []struct {
		provider, probe string
		missing         []string
	}{
		{ProviderOpenAI, ProbeErrorAuth, []string{"x-openai-authorization-error", "x-openai-ide-error-code", "x-error-json"}},
		{ProviderOpenAI, ProbeChat, []string{"openai-organization", "openai-project"}},
		{ProviderAnthropic, ProbeErrorAuth, []string{"x-should-retry"}},
		{ProviderAnthropic, ProbeChat, []string{"anthropic-organization-id", "anthropic-workspace-id"}},
		{ProviderAnthropic, ProbeModels, []string{"anthropic-workspace-id", "request-id"}},
		{ProviderOpenRouter, ProbeErrorModel, nil}, // status differs: headers not compared
		{ProviderOpenRouter, ProbeChat, []string{"x-generation-id"}},
		{ProviderOpenCode, ProbeChat, []string{"x-opencode-log-id", "x-zen-model"}},
	} {
		ds := rawDivergences(t, c.provider, c.probe)
		for _, h := range c.missing {
			if !findDivergence(ds, KindMissingHeader, h) {
				t.Errorf("%s/%s: missing_header %s not reported\n%s", c.provider, c.probe, h, kinds(ds))
			}
		}
		for _, d := range ds {
			if strings.Contains(d.Path, "proxy-wasm") || strings.HasPrefix(d.Path, "cf-") || d.Path == "server" {
				t.Errorf("%s/%s: infrastructure header %s", c.provider, c.probe, d.Path)
			}
		}
	}
	h := rawAnswer(t, ProviderOpenRouter, ProbeErrorModel, "real").header
	if fmt.Sprint(HeaderNames(h)) != "[x-generation-id]" {
		t.Errorf("openrouter error_model headers: %v", HeaderNames(h))
	}
}

// TestRealResponses: the first real drift of the responses probes (task
// 239, 2026-10-31), real answers against the wwz of that day: no
// divergence on OpenAI and on the two gateways (OpenRouter without
// "event:" lines and with [DONE], OpenCode with them).
func TestRealResponses(t *testing.T) {
	for _, p := range []string{ProviderOpenAI, ProviderOpenRouter, ProviderOpenCode} {
		for _, probe := range []string{ProbeResponses, ProbeResponsesStream} {
			if ds := rawDivergences(t, p, probe); len(ds) != 0 {
				t.Errorf("%s/%s: %s", p, probe, kinds(ds))
			}
		}
		real := rawSignature(t, p, ProbeResponsesStream, "real")
		if !real.Stream || len(real.Events) == 0 || real.Events[0].Key() != "response.created" {
			t.Errorf("%s: responses_stream real events %v", p, real.Events)
		}
		if a := rawAnswer(t, p, ProbeResponses, "real"); !bytes.Contains(a.body, []byte(`"object"`)) {
			t.Errorf("%s: responses real body %s", p, a.body)
		}
		// The usage of the real answers feeds the budget.
		if in, out, ok := usageOf(rawAnswer(t, p, ProbeResponsesStream, "real").body, true); !ok || in == 0 || out == 0 {
			t.Errorf("%s: responses_stream usage %d/%d/%v", p, in, out, ok)
		}
	}
}

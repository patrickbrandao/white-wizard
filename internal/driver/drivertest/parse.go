package drivertest

import (
	"slices"
	"sort"
	"testing"

	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
)

func (s *suite) testParse(t *testing.T) {
	for _, k := range s.surfaces() {
		s.run(t, string(k), func(t *testing.T) {
			switch k {
			case driver.KindChat:
				s.parseChat(t)
			case driver.KindCompletions:
				s.parseCompletions(t)
			case driver.KindMessages:
				s.parseMessages(t)
			}
			s.parseInvalid(t, k)
		})
	}
	if s.hasKind(driver.KindCountTokens) {
		s.run(t, "count_tokens", s.parseCountTokens)
	}
}

func (s *suite) parse(t *testing.T, k driver.Kind, body []byte) *core.Request {
	t.Helper()
	req, e := s.d.ParseRequest(k, body, driver.MapHeaders(s.p.Credential))
	if e != nil {
		t.Fatalf("ParseRequest(%s): %v\n%s", k, e, body)
	}
	if req == nil {
		t.Fatalf("ParseRequest(%s): nil request and nil error", k)
	}
	if req.Driver != s.d.Name() {
		t.Errorf("Driver = %q, want %q", req.Driver, s.d.Name())
	}
	if req.Endpoint != endpointOf(k) {
		t.Errorf("Endpoint = %q, want %q", req.Endpoint, endpointOf(k))
	}
	if string(req.Raw) != string(body) && k != driver.KindCountTokens {
		t.Errorf("Raw does not keep the original body")
	}
	return req
}

func roles(ms []core.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}

func toolNames(ts []core.ToolDef) []string {
	out := make([]string, len(ts))
	for i, td := range ts {
		out[i] = td.Name
	}
	sort.Strings(out)
	return out
}

func eqStrings(a, b []string) bool { return slices.Equal(a, b) }

func (s *suite) parseChat(t *testing.T) {
	s.run(t, "Full", func(t *testing.T) {
		req := s.parse(t, driver.KindChat, readTestdata(t, "chat_full.json"))
		if req.Model != "gpt-test" {
			t.Errorf("Model = %q", req.Model)
		}
		if req.SystemPrompt() != "You are terse." {
			t.Errorf("SystemPrompt = %q", req.SystemPrompt())
		}
		if got, want := roles(req.Messages), []string{"system", "user", "assistant", "tool", "user"}; !eqStrings(got, want) {
			t.Fatalf("roles %v, want %v", got, want)
		}
		if m := req.Messages[1]; m.Content != "What is the weather?" || m.ImageParts != 1 {
			t.Errorf("multimodal user turn = %+v", m)
		}
		if tc := req.Messages[2].ToolCalls; len(tc) != 1 || tc[0].ID != "call_prev" || tc[0].Name != "get_weather" || !jsonEq(tc[0].Arguments, `{"city":"Paris"}`) {
			t.Errorf("assistant tool_calls = %+v", tc)
		}
		if req.Messages[3].ToolCallID != "call_prev" {
			t.Errorf("tool turn ToolCallID = %q", req.Messages[3].ToolCallID)
		}
		if req.LastUserMessage() != "And tomorrow?" {
			t.Errorf("LastUserMessage = %q", req.LastUserMessage())
		}
		if got := toolNames(req.Tools); !eqStrings(got, []string{"get_time", "get_weather"}) {
			t.Errorf("Tools = %v", got)
		}
		for _, td := range req.Tools {
			if td.Name == "get_weather" && (td.Description != "Weather for a city" || !jsonEq(string(td.Parameters), `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)) {
				t.Errorf("tool get_weather = %+v", td)
			}
		}
		if req.ToolChoice != (core.ToolChoice{Mode: core.ToolChoiceNamed, Name: "get_weather"}) {
			t.Errorf("ToolChoice = %+v", req.ToolChoice)
		}
		if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
			t.Errorf("ParallelToolCalls = %v, want false", req.ParallelToolCalls)
		}
		if req.MaxTokens != 512 {
			t.Errorf("MaxTokens = %d (max_completion_tokens)", req.MaxTokens)
		}
		if !req.Stream || !req.IncludeUsage {
			t.Errorf("Stream = %v, IncludeUsage = %v", req.Stream, req.IncludeUsage)
		}
		if !req.Reasoning.Requested || req.Reasoning.Effort != "high" {
			t.Errorf("Reasoning = %+v, want requested high", req.Reasoning)
		}
	})
	s.run(t, "Minimal", func(t *testing.T) {
		req := s.parse(t, driver.KindChat, variant(t, "chat_variants.json", "minimal"))
		if req.Stream || req.IncludeUsage || req.MaxTokens != 0 {
			t.Errorf("Stream %v IncludeUsage %v MaxTokens %d, want defaults", req.Stream, req.IncludeUsage, req.MaxTokens)
		}
		// Task 010 / spec 007 R7: reasoning only when asked, except where
		// the upstream reasons by default.
		if req.Reasoning.Requested != s.surface(driver.KindChat).ReasoningByDefault {
			t.Errorf("Reasoning.Requested = %v without reasoning_effort", req.Reasoning.Requested)
		}
		if req.ToolChoice.Mode != core.ToolChoiceAuto {
			t.Errorf("ToolChoice = %+v, want auto", req.ToolChoice)
		}
		if req.ParallelToolCalls != nil {
			t.Errorf("ParallelToolCalls = %v, want unset", *req.ParallelToolCalls)
		}
	})
	s.run(t, "ToolChoice", func(t *testing.T) {
		req := s.parse(t, driver.KindChat, variant(t, "chat_variants.json", "tool_choice_required"))
		if req.ToolChoice.Mode != core.ToolChoiceRequired || req.MaxTokens != 64 {
			t.Errorf("required: ToolChoice %+v MaxTokens %d", req.ToolChoice, req.MaxTokens)
		}
		req = s.parse(t, driver.KindChat, variant(t, "chat_variants.json", "tool_choice_none"))
		if req.ToolChoice.Mode != core.ToolChoiceNone {
			t.Errorf("none: ToolChoice %+v", req.ToolChoice)
		}
	})
	if s.surface(driver.KindChat).ReasoningObject {
		s.run(t, "ReasoningObject", func(t *testing.T) {
			req := s.parse(t, driver.KindChat, variant(t, "chat_variants.json", "reasoning_object"))
			if !req.Reasoning.Requested || req.Reasoning.Effort != "low" || req.Reasoning.BudgetTokens != 900 {
				t.Errorf("reasoning {effort low, max_tokens 900}: %+v", req.Reasoning)
			}
			req = s.parse(t, driver.KindChat, variant(t, "chat_variants.json", "reasoning_exclude"))
			// ADR 0022: exclude still reasons (and bills); only the text is hidden.
			if !req.Reasoning.Requested || !req.Reasoning.Exclude || req.Reasoning.Effort != "high" {
				t.Errorf("reasoning {effort high, exclude: true}: %+v, want Requested and Exclude", req.Reasoning)
			}
		})
	}
}

func (s *suite) parseCompletions(t *testing.T) {
	req := s.parse(t, driver.KindCompletions, variant(t, "chat_variants.json", "completions"))
	if req.Model != "gpt-test-instruct" || req.MaxTokens != 16 || !req.Stream || !req.IncludeUsage {
		t.Errorf("completions: %+v", req)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "Say hi" {
		t.Errorf("prompt as one user message: %+v", req.Messages)
	}
	if req.ToolChoice.Mode != core.ToolChoiceNone {
		t.Errorf("completions take no tools: ToolChoice %+v", req.ToolChoice)
	}
}

func (s *suite) parseMessages(t *testing.T) {
	s.run(t, "Full", func(t *testing.T) {
		req := s.parse(t, driver.KindMessages, readTestdata(t, "messages_full.json"))
		if req.Model != "claude-test" || req.MaxTokens != 4096 {
			t.Errorf("Model %q MaxTokens %d", req.Model, req.MaxTokens)
		}
		if req.System != "Be brief." {
			t.Errorf("System = %q (system blocks)", req.System)
		}
		if got, want := roles(req.Messages), []string{"user", "assistant", "user"}; !eqStrings(got, want) {
			t.Fatalf("roles %v, want %v", got, want)
		}
		if m := req.Messages[0]; m.Content != "Weather in Paris?" || m.ImageParts != 1 {
			t.Errorf("multimodal user turn = %+v", m)
		}
		if tc := req.Messages[1].ToolCalls; len(tc) != 1 || tc[0].ID != "toolu_prev" || tc[0].Name != "get_weather" || !jsonEq(tc[0].Arguments, `{"city":"Paris"}`) {
			t.Errorf("assistant tool_use = %+v", tc)
		}
		if tr := req.Messages[2].ToolResults; len(tr) != 1 || tr[0].ToolCallID != "toolu_prev" || tr[0].Content != "21C" {
			t.Errorf("tool_result = %+v", tr)
		}
		if got := toolNames(req.Tools); !eqStrings(got, []string{"get_time", "get_weather"}) {
			t.Errorf("Tools = %v", got)
		}
		if req.ToolChoice != (core.ToolChoice{Mode: core.ToolChoiceNamed, Name: "get_weather"}) {
			t.Errorf("ToolChoice = %+v", req.ToolChoice)
		}
		if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
			t.Errorf("disable_parallel_tool_use: ParallelToolCalls = %v", req.ParallelToolCalls)
		}
		if !req.Reasoning.Requested || req.Reasoning.BudgetTokens != 2048 {
			t.Errorf("thinking enabled 2048: Reasoning = %+v", req.Reasoning)
		}
		if !req.Stream {
			t.Error("Stream = false")
		}
		if req.Metadata["user_id"] != "user-1" {
			t.Errorf("metadata.user_id = %q", req.Metadata["user_id"])
		}
	})
	s.run(t, "Minimal", func(t *testing.T) {
		req := s.parse(t, driver.KindMessages, variant(t, "messages_variants.json", "minimal"))
		if req.Stream || req.MaxTokens != 64 {
			t.Errorf("Stream %v MaxTokens %d", req.Stream, req.MaxTokens)
		}
		if req.Reasoning.Requested != s.surface(driver.KindMessages).ReasoningByDefault {
			t.Errorf("Reasoning.Requested = %v without thinking", req.Reasoning.Requested)
		}
		if req.ToolChoice.Mode != core.ToolChoiceAuto {
			t.Errorf("ToolChoice = %+v, want auto", req.ToolChoice)
		}
	})
	s.run(t, "RoleOrder", func(t *testing.T) {
		// The real API accepts a conversation that starts with assistant
		// and two turns of the same role in a row (conferred 2026-10-31).
		for _, name := range []string{"assistant_first", "same_role_twice"} {
			req := s.parse(t, driver.KindMessages, variant(t, "messages_variants.json", name))
			if len(req.Messages) != 2 {
				t.Errorf("%s: %d messages, want 2", name, len(req.Messages))
			}
		}
	})
	s.run(t, "ToolChoice", func(t *testing.T) {
		req := s.parse(t, driver.KindMessages, variant(t, "messages_variants.json", "tool_choice_any"))
		if req.ToolChoice.Mode != core.ToolChoiceRequired {
			t.Errorf("any: ToolChoice %+v, want required", req.ToolChoice)
		}
		req = s.parse(t, driver.KindMessages, variant(t, "messages_variants.json", "tool_choice_none"))
		if req.ToolChoice.Mode != core.ToolChoiceNone {
			t.Errorf("none: ToolChoice %+v", req.ToolChoice)
		}
	})
}

func (s *suite) parseCountTokens(t *testing.T) {
	req := s.parse(t, driver.KindCountTokens, variant(t, "messages_variants.json", "count_tokens"))
	if req.LastUserMessage() != "count me" {
		t.Errorf("messages: %+v", req.Messages)
	}
	w := core.NewMemWriter()
	if err := s.d.WriteResponse(w, req, &core.Response{Usage: core.Usage{PromptTokens: 42}}, nil); err != nil {
		t.Fatal(err)
	}
	body := expectJSONResponse(t, w, 200)
	expectPath(t, body, "input_tokens", 42)
	if len(body) != 1 {
		t.Errorf("count_tokens body = %v, want only input_tokens", body)
	}
}

// parseInvalid: bad bodies are core.ErrInvalidRequest and render as 400 in
// the dialect's envelope.
func (s *suite) parseInvalid(t *testing.T, k driver.Kind) {
	s.run(t, "Invalid", func(t *testing.T) {
		for name, body := range invalidBodies(t, string(k)) {
			s.run(t, name, func(t *testing.T) {
				req, e := s.d.ParseRequest(k, []byte(body), driver.MapHeaders(s.p.Credential))
				if where, ok := s.surface(k).Accepted[name]; ok {
					// The real provider accepts this body (conferred: where).
					if e != nil {
						t.Errorf("refused %s (%v); the real accepts it (%s)", body, e, where)
					}
					return
				}
				if e == nil {
					t.Fatalf("accepted %s: %+v", body, req)
				}
				if e.Kind != core.ErrInvalidRequest {
					t.Errorf("Kind = %s, want invalid_request", e.Kind)
				}
				if e.Message == "" {
					t.Error("empty error message")
				}
				w := core.NewMemWriter()
				s.d.WriteError(w, &core.Request{Endpoint: endpointOf(k), Driver: s.d.Name()}, e, nil)
				if w.StatusCode() != 400 {
					t.Errorf("rendered status %d, want 400", w.StatusCode())
				}
				s.checkEnvelope(t, k, w, &core.Request{Endpoint: endpointOf(k)}, e)
			})
		}
	})
}

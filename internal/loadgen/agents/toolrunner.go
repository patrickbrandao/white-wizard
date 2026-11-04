package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// ToolRequest is one tool call of a session.
type ToolRequest struct {
	// Session is the tools session (<run>-a<agent>-s<session>); unknown
	// sessions are created with profile all.
	Session string
	// Seed of the session when it is created (0 = the executor's).
	Seed int64
	Call loadgen.ToolCall
}

// ToolResult is the outcome of a call, as it goes back to the model.
type ToolResult struct {
	Content string
	IsError bool
	Outcome tools.Outcome
}

// ToolRunner executes tool calls. An error means the call could not be run
// at all (the session fails with ReasonTools); tool failures are results.
type ToolRunner interface {
	Call(ctx context.Context, req ToolRequest) (ToolResult, error)
}

// arguments sends the arguments as a JSON string holding them, as OpenAI
// does in function.arguments: both runners get the same bytes, and text
// that is not a JSON object becomes a natural_error of the tool.
func arguments(s string) json.RawMessage {
	b, _ := json.Marshal(s) // a string always encodes
	return b
}

// InProcess runs the calls on a tools.Executor in this process.
type InProcess struct{ Ex *tools.Executor }

// Call implements ToolRunner. A tool outside the catalogue is the
// natural_error the wwz-tools server gives (404 with the same message).
func (r InProcess) Call(ctx context.Context, req ToolRequest) (ToolResult, error) {
	res, err := r.Ex.Call(ctx, tools.CallRequest{
		Session: req.Session, Name: req.Call.Name, Arguments: arguments(req.Call.Arguments),
		ToolCallID: req.Call.ID, Seed: req.Seed,
	})
	switch {
	case errors.Is(err, tools.ErrUnknownTool):
		return ToolResult{Content: err.Error(), IsError: true, Outcome: tools.OutcomeNatural}, nil
	case err != nil:
		return ToolResult{}, err
	}
	return ToolResult{Content: res.Content, IsError: res.IsError, Outcome: res.Outcome}, nil
}

// maxToolBody caps a wwz-tools response (its request limit is 1 MiB; a
// result is of the same order).
const maxToolBody = 8 << 20

// Remote runs the calls on a `wwz-tools serve` (POST /v1/call,
// cmd/wwz-tools/openapi.yaml).
type Remote struct {
	// URL is the base URL of the server.
	URL string
	// Token is the bearer ($WWZ_TOOLS_TOKEN); "" sends none.
	Token  string
	Client *http.Client
}

type callBody struct {
	Session    string          `json:"session"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type callResult struct {
	Content string        `json:"content"`
	IsError bool          `json:"is_error"`
	Outcome tools.Outcome `json:"outcome"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Call implements ToolRunner: 200 is the result, 504 the timeout outcome,
// 404 (unknown tool) a natural_error; anything else is an error.
func (r Remote) Call(ctx context.Context, req ToolRequest) (ToolResult, error) {
	body, _ := json.Marshal(callBody{
		Session: req.Session, Name: req.Call.Name, Arguments: arguments(req.Call.Arguments), ToolCallID: req.Call.ID,
	})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.URL, "/")+"/v1/call", bytes.NewReader(body))
	if err != nil {
		return ToolResult{}, fmt.Errorf("tools: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	if r.Token != "" {
		hr.Header.Set("Authorization", "Bearer "+r.Token)
	}
	if req.Seed != 0 {
		hr.Header.Set("x-wwz-seed", strconv.FormatInt(req.Seed, 10))
	}
	resp, err := r.Client.Do(hr)
	if err != nil {
		return ToolResult{}, fmt.Errorf("tools: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxToolBody))
	if err != nil {
		return ToolResult{}, fmt.Errorf("tools: %w", err)
	}
	var out callResult
	if json.Unmarshal(raw, &out) != nil {
		return ToolResult{}, fmt.Errorf("tools: HTTP %d: invalid JSON body", resp.StatusCode)
	}
	msg := ""
	if out.Error != nil {
		msg = out.Error.Message
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return ToolResult{Content: out.Content, IsError: out.IsError, Outcome: out.Outcome}, nil
	case http.StatusGatewayTimeout:
		return ToolResult{Content: msg, IsError: true, Outcome: tools.OutcomeTimeout}, nil
	case http.StatusNotFound:
		return ToolResult{Content: msg, IsError: true, Outcome: tools.OutcomeNatural}, nil
	}
	return ToolResult{}, fmt.Errorf("tools: HTTP %d: %s", resp.StatusCode, msg)
}

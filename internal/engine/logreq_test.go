package engine_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestRequestLogLevels: a success logs at debug with the usage and the
// scenario step, an error at info with its kind; with the level off nothing
// is built or written (task 268).
func TestRequestLogLevels(t *testing.T) {
	e := newEngine(t, fastCfg)
	var buf bytes.Buffer
	e.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	body := `{"model":"wwz-fast","messages":[{"role":"user","content":"hello"}]}`
	tools := `"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}}}]`
	scnBody := `{"model":"wwz-fast","messages":[{"role":"user","content":"do the file task"}],` + tools + `}`
	call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01", "x-wwz-scenario": "file-agent-flow", "x-wwz-conversation-id": "c1"}, scnBody)
	out := buf.String()
	for _, want := range []string{"level=DEBUG", "key=", "scenario=file-agent-flow", "step=", "prompt_tokens="} {
		if !strings.Contains(out, want) {
			t.Fatalf("success log missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer nope"}, body)
	if out := buf.String(); !strings.Contains(out, "level=INFO") || !strings.Contains(out, "error=") {
		t.Fatalf("error log: %s", out)
	}

	buf.Reset()
	e.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	call(t, e, "openai", "/v1/chat/completions", map[string]string{"Authorization": "Bearer wwz-key-01"}, body)
	if buf.Len() != 0 {
		t.Fatalf("success logged at info level: %s", buf.String())
	}
}

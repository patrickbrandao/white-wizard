package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/admin"
	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/driver"
	"github.com/patrickbrandao/white-wizard/internal/engine"
	"github.com/patrickbrandao/white-wizard/internal/limits"
	"github.com/patrickbrandao/white-wizard/internal/scenario"
	"github.com/patrickbrandao/white-wizard/internal/server"
	"github.com/patrickbrandao/white-wizard/scenarios"
)

// These tests cover the defensive paths that the real dependencies never
// take after Validate: each one swaps a seam (serve.go, simulate.go).

func badBuiltin(t *testing.T) {
	t.Helper()
	orig := builtinFiles
	t.Cleanup(func() { builtinFiles = orig })
	builtinFiles = func() []scenarios.File {
		return []scenarios.File{{Name: "broken.yaml", Data: []byte("name: broken\nsteps: []\n")}}
	}
}

func TestBuiltinScenariosParseError(t *testing.T) {
	cleanEnv(t)
	badBuiltin(t)
	if _, err := builtinScenarios(); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("builtinScenarios: %v", err)
	}
	var logs syncBuffer
	reg := scenario.NewRegistry()
	loadScenarios(reg, &config.Config{}, newLogger(&config.Config{}, &logs))
	if !strings.Contains(logs.String(), "built-in scenarios") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	if err := checkScenarios(&config.Config{}); err == nil || !strings.Contains(err.Error(), "(built-in) broken.yaml") {
		t.Fatalf("checkScenarios: %v", err)
	}
	var out, errb bytes.Buffer
	if code := runSimulate(context.Background(), []string{"-scenario", "no-such-scenario"}, &out, &errb); code != exitError {
		t.Fatalf("simulate: code %d, stderr %s", code, errb.String())
	}
}

type noRoutes struct{ driver.Driver }

func (noRoutes) Routes() []driver.Route { return nil }

func TestSimulateDriverWithoutRoute(t *testing.T) {
	cleanEnv(t)
	orig := newDriver
	t.Cleanup(func() { newDriver = orig })
	newDriver = func(name string, opts map[string]string) (driver.Driver, error) {
		d, err := orig(name, opts)
		return noRoutes{d}, err
	}
	var out, errb bytes.Buffer
	code := runSimulate(context.Background(), []string{"-scenario", "file-agent-flow"}, &out, &errb)
	if code != exitError || !strings.Contains(errb.String(), "no chat route") {
		t.Fatalf("code %d, stderr %s", code, errb.String())
	}
}

func TestSimulateMarshalAndParseFailures(t *testing.T) {
	cleanEnv(t)
	t.Run("request body", func(t *testing.T) {
		orig := jsonMarshal
		t.Cleanup(func() { jsonMarshal = orig })
		jsonMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal boom") }
		var out, errb bytes.Buffer
		code := runSimulate(context.Background(), []string{"-scenario", "file-agent-flow"}, &out, &errb)
		if code != exitError || !strings.Contains(errb.String(), "marshal boom") {
			t.Fatalf("code %d, stderr %s", code, errb.String())
		}
	})
	t.Run("json turn", func(t *testing.T) {
		orig := jsonMarshal
		t.Cleanup(func() { jsonMarshal = orig })
		jsonMarshal = func(v any) ([]byte, error) {
			if _, ok := v.(simTurn); ok {
				return nil, errors.New("turn boom")
			}
			return orig(v)
		}
		var out, errb bytes.Buffer
		code := runSimulate(context.Background(), []string{"-scenario", "file-agent-flow", "-json"}, &out, &errb)
		if code != exitError || !strings.Contains(errb.String(), "turn boom") {
			t.Fatalf("code %d, stderr %s", code, errb.String())
		}
	})
	t.Run("unreadable 200", func(t *testing.T) {
		orig := engineHandle
		t.Cleanup(func() { engineHandle = orig })
		engineHandle = func(_ *engine.Engine, _ context.Context, _ engine.Input, w core.ResponseWriter) {
			w.Status(200)
			_, _ = w.Write([]byte("not json"))
		}
		var out, errb bytes.Buffer
		code := runSimulate(context.Background(), []string{"-scenario", "file-agent-flow"}, &out, &errb)
		if code != exitError || !strings.Contains(errb.String(), "turn 1") {
			t.Fatalf("code %d, stderr %s", code, errb.String())
		}
	})
}

// faultyStore is a Memory store whose Close fails and whose policy read
// returns a version that fails validation.
type faultyStore struct{ *limits.Memory }

func (faultyStore) Close() error { return errors.New("close boom") }

func (faultyStore) Policies(context.Context) (limits.Policies, error) {
	return limits.Policies{Version: 2}, errors.New("policies boom")
}

func TestServeStoreFaultsWarningsAndPodName(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POD_NAME", "pod-test")
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func(context.Context, *config.Config, string, *slog.Logger, func(bool)) (limits.Store, error) {
		return faultyStore{limits.NewMemory()}, nil
	}
	cfg := filepath.Join(t.TempDir(), "w.yaml")
	writeFile(t, cfg, "accounts: [{id: orphan}]\n")
	in := startServe(t, "-config", cfg)
	if code := in.stop(); code != exitOK {
		t.Fatalf("code %d", code)
	}
	logs := in.logs.String()
	for _, want := range []string{"not referenced by any key", "store close", "close boom", "policies boom"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q in logs:\n%s", want, logs)
		}
	}
}

func TestServeConstructorFailures(t *testing.T) {
	cleanEnv(t)
	args := []string{"-port", "0", "-admin-port", "0"}
	run := func(t *testing.T) (int, string) {
		t.Helper()
		var errb syncBuffer
		code := runServe(context.Background(), args, io.Discard, &errb, func(addrs) { t.Error("must not start") })
		return code, errb.String()
	}
	t.Run("store", func(t *testing.T) {
		orig := openStore
		t.Cleanup(func() { openStore = orig })
		openStore = func(context.Context, *config.Config, string, *slog.Logger, func(bool)) (limits.Store, error) {
			return nil, errors.New("store boom")
		}
		if code, logs := run(t); code != exitError || !strings.Contains(logs, "store boom") {
			t.Fatalf("code %d:\n%s", code, logs)
		}
	})
	t.Run("admin", func(t *testing.T) {
		orig := newAdmin
		t.Cleanup(func() { newAdmin = orig })
		newAdmin = func(admin.Options) (*admin.Server, error) { return nil, errors.New("admin boom") }
		if code, logs := run(t); code != exitError || !strings.Contains(logs, "admin boom") {
			t.Fatalf("code %d:\n%s", code, logs)
		}
	})
	t.Run("server", func(t *testing.T) {
		orig := newServer
		t.Cleanup(func() { newServer = orig })
		newServer = func(*engine.Engine, server.Options) (*server.Server, error) { return nil, errors.New("server boom") }
		if code, logs := run(t); code != exitError || !strings.Contains(logs, "server boom") {
			t.Fatalf("code %d:\n%s", code, logs)
		}
	})
}

// brokenListener fails on the first Accept, as a listener that died.
type brokenListener struct{ net.Listener }

func (brokenListener) Accept() (net.Conn, error) { return nil, errors.New("accept boom") }

func TestServeListenerFailure(t *testing.T) {
	cleanEnv(t)
	orig := listenAdmin
	t.Cleanup(func() { listenAdmin = orig })
	listenAdmin = func(network, addr string) (net.Listener, error) {
		ln, err := orig(network, addr)
		return brokenListener{ln}, err
	}
	var errb syncBuffer
	code := runServe(context.Background(), []string{"-port", "0", "-admin-port", "0"}, io.Discard, &errb, nil)
	if code != exitError || !strings.Contains(errb.String(), "listener failed") {
		t.Fatalf("code %d:\n%s", code, errb.String())
	}
}

func TestServeShutdownTimeoutWarnings(t *testing.T) {
	cleanEnv(t)
	in := startServe(t, "-shutdown-timeout", "50ms")
	// Half-sent requests keep one connection open on each plane.
	for _, addr := range []string{in.a.Data, in.a.Admin} {
		_, port, _ := net.SplitHostPort(addr)
		c, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	in.stop()
	logs := in.logs.String()
	for _, want := range []string{"data plane shutdown", "control plane shutdown"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q in logs:\n%s", want, logs)
		}
	}
}

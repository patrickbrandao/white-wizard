package client

import (
	"context"
	"errors"

	"github.com/patrickbrandao/white-wizard/internal/claudecode"
	"github.com/patrickbrandao/white-wizard/internal/loadgen"
)

// Protocol is config.protocol of the report of wwz-load claude-code.
const Protocol = "claude-code"

// DefaultPrompt is the user message of every turn of run.
const DefaultPrompt = "Hello there, reply with ok"

// Unit returns the load unit of run: one session with a new id and
// connection doing turns turns (each an [Session.Ask] with prompt), every
// request recorded. A failed request ends the session.
func (c *Client) Unit(turns int, prompt string) loadgen.Unit {
	return func(ctx context.Context, _ int64, rec *loadgen.Recorder) {
		s := c.NewSession()
		defer s.Close()
		for t := 0; t < turns; t++ {
			if t > 0 {
				select {
				case <-rec.Done():
					return
				default:
				}
			}
			x, err := s.Ask(ctx, prompt)
			for i := range x.Results {
				rec.Record(&x.Results[i])
			}
			if err != nil || !x.OK() {
				return
			}
		}
	}
}

// ErrNoTurns is returned by [Client.Run] with turns < 1.
var ErrNoTurns = errors.New("turns must be ≥ 1")

// Run runs the load plan cfg with sessions of turns turns.
func (c *Client) Run(ctx context.Context, cfg loadgen.RunConfig, turns int, prompt string) (*loadgen.RunResult, error) {
	if turns < 1 {
		return nil, ErrNoTurns
	}
	if prompt == "" {
		prompt = DefaultPrompt
	}
	return loadgen.Run(ctx, cfg, c.Unit(turns, prompt))
}

// ReportConfig returns the config of the run report: the schema of the
// run (spec 015, "Saída") with protocol "claude-code".
func (c *Client) ReportConfig(cfg loadgen.RunConfig) loadgen.ReportConfig {
	maxTokens := c.p.TurnMaxTokens
	if c.o.NonStream {
		maxTokens = c.p.FallbackMaxTokens
	}
	rc := loadgen.ReportConfig{
		Target: c.o.Target.raw, Protocol: Protocol, Path: c.o.Target.prefix + claudecode.PathMessages,
		Model: c.o.Model, Stream: !c.o.NonStream, MaxTokens: int(maxTokens),
		Mode: cfg.Mode, RPS: cfg.RPS, Concurrency: cfg.Concurrency,
		DurationMs: float64(cfg.Duration.Milliseconds()), Requests: cfg.Requests,
		WarmupMs: float64(cfg.Warmup.Milliseconds()), TimeoutMs: float64(c.o.Timeout.Milliseconds()),
		Instances: 1,
	}
	return rc
}

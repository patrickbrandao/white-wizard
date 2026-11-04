package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestWaitPromotions covers task 244: shutdown waits for promotions in
// flight, but never past the shutdown deadline.
func TestWaitPromotions(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	finished := false
	waitPromotions(context.Background(), func() { time.Sleep(20 * time.Millisecond); finished = true }, log)
	if !finished || buf.Len() != 0 {
		t.Fatalf("did not wait: finished=%v log=%q", finished, buf.String())
	}

	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	waitPromotions(ctx, func() { <-release }, log)
	if time.Since(start) > 2*time.Second || !strings.Contains(buf.String(), "still running") {
		t.Fatalf("deadline not honored: %v %q", time.Since(start), buf.String())
	}
}

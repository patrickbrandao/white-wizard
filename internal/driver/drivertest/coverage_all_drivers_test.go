package drivertest_test

import (
	"testing"

	_ "github.com/patrickbrandao/white-wizard/internal/driver/anthropic"
	"github.com/patrickbrandao/white-wizard/internal/driver/drivertest"
	_ "github.com/patrickbrandao/white-wizard/internal/driver/opencode"
)

// TestAnthropic and TestOpenCode run the suite on the two drivers the
// other tests here leave out: the Messages checks, and the OpenCode
// gateway with its Chat, Messages, Responses and Gemini (Vertex) dialects.
func TestAnthropic(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "anthropic")) }

func TestOpenCode(t *testing.T) { drivertest.Run(t, drivertest.MustNew(t, "opencode")) }

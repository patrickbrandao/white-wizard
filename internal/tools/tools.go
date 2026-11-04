// Package tools is the core of wwz-tools (spec 017, "Contrato do wwz-tools";
// ADR 0034): a fixed catalogue of fake tools, a virtual in-memory sandbox per
// session and an Executor that answers tool calls with simulated latency and
// injected outcomes. Nothing touches the host: files live in memory,
// run_command never executes anything and web_search/fetch_url never reach
// the network. The package is used in process by `wwz-load agents` and served
// over HTTP by cmd/wwz-tools.
//
// Determinism: with the same seed, the same session and the same sequence of
// calls, content, latency and injected outcome are identical.
package tools

import (
	"errors"
	"fmt"
)

// Profile selects a subset of the catalogue.
type Profile string

// Profiles of the catalogue.
const (
	ProfileCoder    Profile = "coder"
	ProfilePesquisa Profile = "pesquisa"
	ProfileChat     Profile = "chat"
	ProfileAll      Profile = "all"
)

// Format is the shape of the catalogue output.
type Format string

// Catalogue formats.
const (
	FormatOpenAI    Format = "openai"
	FormatAnthropic Format = "anthropic"
	FormatRaw       Format = "raw"
)

// Outcome is the result class of a call.
type Outcome string

// Outcomes. OutcomeNatural is never injected nor forced: it is the error the
// real tool would give (bad arguments, missing file, path outside sandbox).
const (
	OutcomeSuccess        Outcome = "success"
	OutcomeNatural        Outcome = "natural_error"
	OutcomeError          Outcome = "error"
	OutcomeTimeout        Outcome = "timeout"
	OutcomeInvalidPayload Outcome = "invalid_payload"
)

// Errors returned by the package (wrapped with detail; test with errors.Is).
var (
	ErrUnknownTool     = errors.New("unknown tool")
	ErrInvalidProfile  = errors.New("invalid profile")
	ErrInvalidFormat   = errors.New("invalid format")
	ErrInvalidOutcome  = errors.New("invalid outcome")
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionExists   = errors.New("session already exists")
)

// ParseProfile validates a profile name; "" means ProfileAll.
func ParseProfile(s string) (Profile, error) {
	switch p := Profile(s); p {
	case "":
		return ProfileAll, nil
	case ProfileCoder, ProfilePesquisa, ProfileChat, ProfileAll:
		return p, nil
	}
	return "", fmt.Errorf("%w %q: want coder, pesquisa, chat or all", ErrInvalidProfile, s)
}

// ParseFormat validates a catalogue format; "" means FormatRaw.
func ParseFormat(s string) (Format, error) {
	switch f := Format(s); f {
	case "":
		return FormatRaw, nil
	case FormatOpenAI, FormatAnthropic, FormatRaw:
		return f, nil
	}
	return "", fmt.Errorf("%w %q: want openai, anthropic or raw", ErrInvalidFormat, s)
}

// ParseOutcome validates a forced outcome (x-wwz-tools-outcome); "" means
// none. natural_error cannot be forced.
func ParseOutcome(s string) (Outcome, error) {
	switch o := Outcome(s); o {
	case "", OutcomeSuccess, OutcomeError, OutcomeTimeout, OutcomeInvalidPayload:
		return o, nil
	}
	return "", fmt.Errorf("%w %q: want success, error, timeout or invalid_payload", ErrInvalidOutcome, s)
}

package drift

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Status values of providers and probes.
const (
	StatusOK      = "ok"
	StatusDrift   = "drift"
	StatusSkipped = "skipped"
	StatusError   = "error"
)

// Skip reasons.
const (
	ReasonNoKey            = "no_key"
	ReasonRealNotConfirmed = "real_not_confirmed"
	ReasonBudgetExhausted  = "budget_exhausted"
	ReasonNotSelected      = "not_selected"
	// ReasonNotSupported: the provider has no route for the probe (the
	// responses probes on anthropic and the Gemini dialects).
	ReasonNotSupported = "not_supported"
	// ReasonUpstreamFormUnreproduced: the OpenCode gateway only answered
	// with the chatcmpl- form, which wwz does not reproduce (ADR 0059).
	ReasonUpstreamFormUnreproduced = "upstream_form_unreproduced"
)

// Report versions.
const (
	ReportVersion = 1
	ReportKind    = "drift"
	// RecordKind is the Kind of the result of Record.
	RecordKind = "record"
)

// Exit codes of wwz-drift (ADR 0035).
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
	ExitDrift = 3
)

// Report is the result of a check (spec 017, JSON version 1 kind drift).
type Report struct {
	Version     int              `json:"version"`
	Kind        string           `json:"kind"`
	ToolVersion string           `json:"tool_version"`
	Started     time.Time        `json:"started"`
	Finished    time.Time        `json:"finished"`
	WWZ         WWZInfo          `json:"wwz"`
	Providers   []ProviderReport `json:"providers"`
	Summary     Summary          `json:"summary"`
	// Dir is where Record wrote the fixtures (empty in a check).
	Dir string `json:"dir,omitempty"`

	red *Redactor
}

// WWZInfo identifies the wwz compared.
type WWZInfo struct {
	URL     string `json:"url"`
	Version string `json:"version"`
}

// ProviderReport is the result of one provider.
type ProviderReport struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Base     string `json:"base"`
	Model    string `json:"model"`
	// Session is the OpenCode session used by every probe of the run.
	Session  string        `json:"session,omitempty"`
	SpendUSD float64       `json:"spend_usd"`
	Probes   []ProbeReport `json:"probes"`
}

// ProbeReport is the result of one probe.
type ProbeReport struct {
	Probe       string       `json:"probe"`
	Status      string       `json:"status"`
	Reason      string       `json:"reason,omitempty"`
	RealStatus  int          `json:"real_status"`
	WWZStatus   int          `json:"wwz_status"`
	Divergences []Divergence `json:"divergences"`
	// File is the fixture written by Record (empty in a check).
	File string `json:"file,omitempty"`
}

// Summary counts providers by status.
type Summary struct {
	OK      int `json:"ok"`
	Drift   int `json:"drift"`
	Skipped int `json:"skipped"`
	Error   int `json:"error"`
}

// summarize fills Summary from the providers.
func (r *Report) summarize() {
	r.Summary = Summary{}
	for _, p := range r.Providers {
		switch p.Status {
		case StatusOK:
			r.Summary.OK++
		case StatusDrift:
			r.Summary.Drift++
		case StatusSkipped:
			r.Summary.Skipped++
		default:
			r.Summary.Error++
		}
	}
}

// rollup derives a provider status from its probes: drift wins over
// error, error over ok; a provider whose probes were all skipped is ok.
func rollup(probes []ProbeReport) string {
	st := StatusOK
	for _, p := range probes {
		switch p.Status {
		case StatusDrift:
			return StatusDrift
		case StatusError:
			st = StatusError
		}
	}
	return st
}

// redactor returns the report redactor (none for a zero Report).
func (r *Report) redactor() *Redactor {
	if r.red == nil {
		return NewRedactor()
	}
	return r.red
}

// WriteJSON writes the report as indented JSON, secrets masked.
func (r *Report) WriteJSON(w io.Writer) error {
	data, _ := json.MarshalIndent(r, "", "  ") // plain structs: cannot fail
	_, err := w.Write(append(r.redactor().Bytes(data), '\n'))
	return err
}

// WriteText writes a human summary (not a contract), secrets masked.
func (r *Report) WriteText(w io.Writer) error {
	var b bytes.Buffer
	if r.Kind == RecordKind {
		fmt.Fprintf(&b, "wwz-drift %s: record em %s", r.ToolVersion, r.Dir)
	} else {
		fmt.Fprintf(&b, "wwz-drift %s: wwz %s", r.ToolVersion, r.WWZ.URL)
	}
	if r.WWZ.Version != "" {
		fmt.Fprintf(&b, " (%s)", r.WWZ.Version)
	}
	b.WriteByte('\n')
	for _, p := range r.Providers {
		fmt.Fprintf(&b, "\n%s: %s", p.Provider, p.Status)
		if p.Reason != "" {
			fmt.Fprintf(&b, " (%s)", p.Reason)
		}
		if p.Base != "" {
			fmt.Fprintf(&b, " base=%s", p.Base)
		}
		if p.Session != "" {
			fmt.Fprintf(&b, " sessão=%s", p.Session)
		}
		if p.Model != "" {
			fmt.Fprintf(&b, " modelo=%s gasto=US$ %.4f", p.Model, p.SpendUSD)
		}
		b.WriteByte('\n')
		for _, pr := range p.Probes {
			fmt.Fprintf(&b, "  %s: %s", pr.Probe, pr.Status)
			if pr.Reason != "" {
				fmt.Fprintf(&b, " (%s)", pr.Reason)
			}
			switch {
			case pr.File != "":
				fmt.Fprintf(&b, " real=%d → %s", pr.RealStatus, pr.File)
			case pr.RealStatus != 0 || pr.WWZStatus != 0:
				fmt.Fprintf(&b, " real=%d wwz=%d", pr.RealStatus, pr.WWZStatus)
			}
			b.WriteByte('\n')
			for _, d := range pr.Divergences {
				mark := ""
				if d.Ignored {
					mark = " [ignorada]"
				}
				fmt.Fprintf(&b, "    %s %s real=%q wwz=%q%s\n", d.Kind, d.Path, d.Real, d.WWZ, mark)
			}
		}
	}
	fmt.Fprintf(&b, "\nresumo: ok=%d drift=%d skipped=%d error=%d\n", r.Summary.OK, r.Summary.Drift, r.Summary.Skipped, r.Summary.Error)
	_, err := w.Write(r.redactor().Bytes(b.Bytes()))
	return err
}

// ExitCode maps the result of Run to the exit code of wwz-drift check: 3
// on any counted divergence (wins over 1), 2 on invalid usage, 1 on a
// failure or a probe in error, else 0.
func ExitCode(r *Report, err error) int {
	switch {
	case r != nil && r.Summary.Drift > 0:
		return ExitDrift
	case errors.Is(err, ErrUsage):
		return ExitUsage
	case err != nil, r != nil && r.Summary.Error > 0:
		return ExitError
	}
	return ExitOK
}

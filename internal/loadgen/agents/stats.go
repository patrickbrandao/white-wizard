package agents

import (
	"time"

	"github.com/patrickbrandao/white-wizard/internal/loadgen"
	"github.com/patrickbrandao/white-wizard/internal/loadgen/hist"
	"github.com/patrickbrandao/white-wizard/internal/tools"
)

// Stats are the measures of one agent (or, merged, of the run). Each agent
// owns its Stats, so no lock is needed while it runs.
type Stats struct {
	Agent   int
	Profile Profile
	// Requests holds the final attempts (latency, tokens, errors by type);
	// Requests.Requests also counts the retried attempts.
	Requests *loadgen.Stats

	SessionsStarted, SessionsCompleted, SessionsFailed, SessionsInterrupted int64
	FailedByReason                                                          map[string]int64
	// Turns that reached a final answer.
	Turns         int64
	Retries       int64
	RetriesByType map[string]int64
	RetryWait     *hist.Hist
	TurnE2E       *hist.Hist
	// SessionE2E of the completed sessions.
	SessionE2E  *hist.Hist
	ToolCalls   int64
	ToolErrors  int64
	CallsByName map[string]int64
	Outcomes    map[tools.Outcome]int64
}

// NewStats returns empty Stats of agent a.
func NewStats(a int, p Profile) *Stats {
	return &Stats{
		Agent: a, Profile: p, Requests: loadgen.NewStats(), FailedByReason: map[string]int64{},
		RetriesByType: map[string]int64{}, RetryWait: hist.New(), TurnE2E: hist.New(), SessionE2E: hist.New(),
		CallsByName: map[string]int64{}, Outcomes: map[tools.Outcome]int64{},
	}
}

// Merge adds o into s.
func (s *Stats) Merge(o *Stats) {
	s.Requests.Merge(o.Requests)
	s.SessionsStarted += o.SessionsStarted
	s.SessionsCompleted += o.SessionsCompleted
	s.SessionsFailed += o.SessionsFailed
	s.SessionsInterrupted += o.SessionsInterrupted
	addAll(s.FailedByReason, o.FailedByReason)
	s.Turns += o.Turns
	s.Retries += o.Retries
	addAll(s.RetriesByType, o.RetriesByType)
	s.RetryWait.Merge(o.RetryWait)
	s.TurnE2E.Merge(o.TurnE2E)
	s.SessionE2E.Merge(o.SessionE2E)
	s.ToolCalls += o.ToolCalls
	s.ToolErrors += o.ToolErrors
	addAll(s.CallsByName, o.CallsByName)
	addAll(s.Outcomes, o.Outcomes)
}

func addAll[K comparable](dst, src map[K]int64) {
	for k, v := range src {
		dst[k] += v
	}
}

// retry counts an attempt that will be repeated after wait.
func (s *Stats) retry(class string, wait time.Duration) {
	s.Requests.Requests++
	s.Retries++
	s.RetriesByType[class]++
	s.RetryWait.Record(wait)
}

// tool counts one executed call.
func (s *Stats) tool(name string, r ToolResult) {
	s.ToolCalls++
	s.CallsByName[name]++
	s.Outcomes[r.Outcome]++
	if r.IsError {
		s.ToolErrors++
	}
}

// session statuses.
const (
	statusCompleted   = "completed"
	statusFailed      = "failed"
	statusInterrupted = "interrupted"
)

// end counts a session that ended with status (and reason when failed).
func (s *Stats) end(status, reason string, e2e time.Duration) {
	switch status {
	case statusCompleted:
		s.SessionsCompleted++
		s.SessionE2E.Record(e2e)
	case statusFailed:
		s.SessionsFailed++
		s.FailedByReason[reason]++
	default:
		s.SessionsInterrupted++
	}
}

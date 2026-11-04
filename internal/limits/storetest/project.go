package storetest

import (
	"context"
	"time"

	"github.com/patrickbrandao/white-wizard/internal/config"
	"github.com/patrickbrandao/white-wizard/internal/core"
	"github.com/patrickbrandao/white-wizard/internal/limits"
)

// projectEnt is the OpenAI project (Anthropic workspace) level.
var projectEnt = limits.Entity{Kind: limits.KindProject, ID: "proj-1"}

// projectCases cover the project tokens unit of the state (task 234, spec
// 010): with projects[].limits.tpm the openai driver adds
// x-ratelimit-*-project-tokens next to the most restrictive tokens unit.
func projectCases() []testCase {
	return []testCase{
		{"ProjectTPMReported", testProjectTPMReported},
		{"ProjectWithoutTPMNotReported", testProjectWithoutTPM},
		{"ProjectTPMDenial", testProjectTPMDenial},
		{"ProjectRPMDenialPeeksTokens", testProjectRPMDenialPeeks},
		{"ProjectNotReportedOnOtherDenials", testProjectOtherDenials},
	}
}

// keyProjAcct is the request of a key of a project of an account.
func keyProjAcct(k, p, a config.Limits) []limits.Subject {
	return subs(sub(keyEnt, k), sub(projectEnt, p), sub(accountEnt, a))
}

func wantProject(t T, st core.RateLimitState, limit, remaining int, what string) {
	t.Helper()
	if st.ProjectTokensLimit != limit || st.ProjectTokensRemain != remaining {
		t.Fatalf("%s: project tokens = %d/%d, want %d/%d (%+v)", what, st.ProjectTokensLimit, st.ProjectTokensRemain, limit, remaining, st)
	}
	if limit > 0 && (!st.Present || st.ProjectTokensReset < 0 || st.ProjectTokensReset > time.Minute) {
		t.Fatalf("%s: project reset/present: %+v", what, st)
	}
}

func testProjectTPMReported(t T, s limits.Store) {
	now := baseTime()
	ss := keyProjAcct(config.Limits{}, config.Limits{TPM: 1000}, config.Limits{TPM: 500})
	d := check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now})
	mustAllow(t, d, "project with tpm")
	// The tokens unit is the most restrictive (the account); the project
	// unit is reported on its own.
	if d.State.TokensLimit != 500 || d.State.TokensRemaining != 490 {
		t.Fatalf("tokens = %d/%d, want the account's 500/490", d.State.TokensLimit, d.State.TokensRemaining)
	}
	wantProject(t, d.State, 1000, 990, "first request")
	if d.State.ProjectTokensReset <= 0 {
		t.Fatalf("project reset = %v, want > 0 after consuming", d.State.ProjectTokensReset)
	}
	d = check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now})
	mustAllow(t, d, "second request")
	wantProject(t, d.State, 1000, 980, "second request")

	// A key with tpm is no project: nothing in the project unit.
	d = check(t, s, limits.CheckRequest{Subjects: subs(sub(key2Ent, config.Limits{TPM: 100})), EstPromptTokens: 10, Now: now})
	mustAllow(t, d, "key tpm")
	wantProject(t, d.State, 0, 0, "key tpm")
}

func testProjectWithoutTPM(t T, s limits.Store) {
	now := baseTime()
	d := check(t, s, limits.CheckRequest{Subjects: keyProjAcct(config.Limits{}, config.Limits{RPM: 10, ITPM: 1000}, config.Limits{TPM: 500}), EstPromptTokens: 10, Now: now})
	mustAllow(t, d, "project without tpm")
	wantProject(t, d.State, 0, 0, "project rpm/itpm only")
	if d.State.RequestsLimit != 10 {
		t.Fatalf("requests = %+v, want the project's rpm", d.State)
	}
}

func testProjectTPMDenial(t T, s limits.Store) {
	now := baseTime()
	ss := keyProjAcct(config.Limits{}, config.Limits{TPM: 100}, config.Limits{TPM: 10000})
	d := check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 60, Now: now})
	mustAllow(t, d, "within the project tpm")
	wantProject(t, d.State, 100, 40, "first")
	d = check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 60, Now: now})
	mustDeny(t, d, projectEnt, "tpm", "over the project tpm")
	if d.State.ProjectTokensLimit != 100 || d.State.ProjectTokensRemain > 40 {
		t.Fatalf("denial project tokens: %+v", d.State)
	}
}

func testProjectRPMDenialPeeks(t T, s limits.Store) {
	now := baseTime()
	ss := keyProjAcct(config.Limits{}, config.Limits{RPM: 1, TPM: 1000}, config.Limits{})
	mustAllow(t, check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now}), "first")
	d := check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now})
	mustDeny(t, d, projectEnt, "rpm", "second over the project rpm")
	wantProject(t, d.State, 1000, 990, "rpm denial")
}

func testProjectOtherDenials(t T, s limits.Store) {
	now := baseTime()
	// rpd on the account: no state at all.
	ss := keyProjAcct(config.Limits{}, config.Limits{TPM: 1000}, config.Limits{RPD: 1})
	mustAllow(t, check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now}), "first")
	d := check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now})
	mustDeny(t, d, accountEnt, "rpd", "rpd")
	wantProject(t, d.State, 0, 0, "rpd denial")

	// rps after the project: only the rps unit.
	acct2 := limits.Entity{Kind: limits.KindAccount, ID: "acct-2"}
	ss = subs(sub(projectEnt, config.Limits{TPM: 1000}), sub(acct2, config.Limits{RPS: 1}))
	mustAllow(t, check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now}), "rps first")
	d = check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now})
	mustDeny(t, d, acct2, "rps", "rps")
	wantProject(t, d.State, 0, 0, "rps denial")

	// A banned project reports nothing either.
	must(t, s.Ban(context.Background(), projectEnt, "abuse", "test"))
	d = check(t, s, limits.CheckRequest{Subjects: ss, EstPromptTokens: 10, Now: now})
	mustDeny(t, d, projectEnt, "ban", "banned project")
	wantProject(t, d.State, 0, 0, "ban")
}

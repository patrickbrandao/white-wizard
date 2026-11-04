// Package claudecode describes, version by version, the exact shape of the
// requests the Claude Code CLI sends to an ANTHROPIC_BASE_URL of its own
// (spec 020, ADR 0074).
//
// The package has four parts:
//
//   - the profile ([Profile], [Lookup]): request line, order and spelling of
//     the headers, fixed values, known betas in canonical order, the beta
//     list of each request type and credential, identities and the order of
//     the top-level body keys;
//   - the pieces Claude Code computes per request: [Fingerprint],
//     [BillingHeader] and [UserID];
//   - a JSON codec compatible with ECMAScript JSON.stringify over ordered
//     values ([Value], [Parse], [Marshal]): Go's encoding/json escapes <, >,
//     & and U+2028/U+2029 and sorts maps, so it cannot reproduce the bytes;
//   - [Build], which turns the inputs of a request ([Request]) into the
//     same bytes Claude Code sends, [Extract], its inverse over a captured
//     request, [ParseHead] for raw request heads, and [Verify], the strict
//     check behind drivers[].options.client_verification: claude-code.
//
// The package imports nothing from internal/driver or internal/server: the
// anthropic driver (strict mode) and the wwz-load claude-code client use it.
package claudecode

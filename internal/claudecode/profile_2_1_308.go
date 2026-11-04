package claudecode

// Betas of Claude Code 2.1.308.
const (
	betaClaudeCode       = "claude-code-20250219"
	betaOAuth            = "oauth-2025-04-20"
	betaContext1M        = "context-1m-2025-08-07"
	betaInterleaved      = "interleaved-thinking-2025-05-14"
	betaRedactThinking   = "redact-thinking-2026-02-12"
	betaThinkingTokens   = "thinking-token-count-2026-05-13"
	betaContextMgmt      = "context-management-2025-06-27"
	betaPerTurnControl   = "per-turn-control-2026-07-01"
	betaMidConvTools     = "mid-conversation-tool-changes-2026-07-01"
	betaInlineTools      = "inline-tools-2026-09-15"
	betaAdvisorTool      = "advisor-tool-2026-03-01"
	betaAdvancedToolUse  = "advanced-tool-use-2025-11-20"
	betaEffort           = "effort-2025-11-24"
	betaStructuredOutput = "structured-outputs-2025-12-15"
	betaDangerousTool    = "dangerous-tool-use-2026-09-03"
	betaThinkingDisplay  = "thinking-display-updates-2026-08-18"
	betaAFKMode          = "afk-mode-2026-01-31"
	betaExtendedCacheTTL = "extended-cache-ttl-2025-04-11"
	betaTokenCounting    = "token-counting-2024-11-01"
)

// The profile of Claude Code 2.1.308 (spec 020; captures of 2026-11-03,
// fixtures in testdata/real/claude-code/2.1.308/).
//
// The anthropic-beta list is built by insertion, not sorted: model betas
// (claude-code, oauth, interleaved-thinking, redact-thinking,
// thinking-token-count, context-management), then the per-turn ones
// (per-turn-control, mid-conversation-tool-changes, inline-tools), the
// advisor, the tool search, effort and structured outputs, and last the
// per-request ones (dangerous-tool-use, thinking-display-updates, afk-mode,
// extended-cache-ttl); the SDK appends token-counting to count_tokens.
// Betas lists that order. Redact-thinking is dropped when the request sets
// thinking.display, which is why it only shows in quota and title.
//
// Flag betas (task 457, the real Claude Code in normal use):
//   - inline-tools and advisor-tool follow feature flags Claude Code
//     fetches (inline-tools: tengu_brisk_meadow, or CLAUDE_CODE_INLINE_TOOLS;
//     advisor-tool: tengu_sage_compass2, or
//     CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL), with either credential:
//     with CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 (no flags fetched)
//     `claude -p` sends neither, the two variables add each one, also with
//     OAuth; capture 004 has both. Turn types and title.
//   - extended-cache-ttl goes with the 1-hour cache TTL, which a
//     subscriber gets only on the main thread, the SDK and the auto-mode
//     classifier (allowlist repl_main_thread*, sdk, auto_mode,
//     memdir_relevance) and not while using overage: a subagent sends it
//     without the beta and with cache_control {"type":"ephemeral"}. OAuth
//     only; turn types and classifier. [Verify] ties the identity's
//     cache_control to the beta.
//
// The auto-mode classifier (TypeClassifier, interactive with OAuth) is a
// non-streaming side query with X-Stainless-Timeout 60, the model betas
// (context-1m right after oauth) and system[1] = its own prompt.
//
// Lists marked "inferred" have no capture: they apply to the captured lists
// the differences seen between OAuth and API key in `claude -p` (without
// oauth and extended-cache-ttl, with inline-tools and advisor-tool) and
// between sdk-cli and cli (thinking-display-updates with display "updates").
func init() {
	turnOAuth := []string{
		betaClaudeCode, betaOAuth, betaInterleaved, betaThinkingTokens, betaContextMgmt,
		betaPerTurnControl, betaMidConvTools, betaEffort, betaExtendedCacheTTL,
	}
	turnAPIKey := []string{
		betaClaudeCode, betaInterleaved, betaThinkingTokens, betaContextMgmt,
		betaPerTurnControl, betaMidConvTools, betaInlineTools, betaAdvisorTool, betaEffort,
	}
	countOAuth := []string{betaClaudeCode, betaOAuth, betaInterleaved, betaContextMgmt, betaTokenCounting}
	countAPIKey := []string{betaClaudeCode, betaInterleaved, betaContextMgmt, betaTokenCounting}

	register(&Profile{
		Version:          "2.1.308",
		BunVersion:       "1.4.4",
		PackageVersion:   "0.128.0",
		Lang:             "js",
		Runtime:          "node",
		RuntimeVersion:   "v26.3.0",
		Timeout:          "600",
		FallbackTimeout:  "300",
		AnthropicVersion: "2023-06-01",
		Accept:           "application/json",
		ContentType:      "application/json",
		DirectBrowser:    "true",
		App:              "cli",
		Connection:       "keep-alive",
		AcceptEncoding:   "gzip, deflate, br, zstd",
		HelloAccept:      "*/*",
		Archs:            []string{"arm64", "x64"},
		OSes:             []string{"MacOS", "Linux", "Windows"},
		Headers: []HeaderSpec{
			{Name: HeaderAccept},
			{Name: HeaderAuthorization, Only: CredentialOAuth},
			{Name: HeaderContentType},
			{Name: HeaderUserAgent},
			{Name: HeaderSessionID},
			{Name: HeaderStainlessArch},
			{Name: HeaderStainlessLang},
			{Name: HeaderStainlessOS},
			{Name: HeaderStainlessPackageVersion},
			{Name: HeaderStainlessRetryCount},
			{Name: HeaderStainlessRuntime},
			{Name: HeaderStainlessRuntimeVersion},
			{Name: HeaderStainlessTimeout, Skip: []Type{TypeCountTokens}},
			{Name: HeaderAnthropicBeta},
			{Name: HeaderDangerousDirectBrowserAccess},
			{Name: HeaderAnthropicVersion},
			{Name: HeaderAPIKey, Only: CredentialAPIKey},
			{Name: HeaderApp},
			{Name: HeaderAgentID, Optional: true},
			{Name: HeaderParentAgentID, Optional: true},
			{Name: HeaderConnection},
			{Name: HeaderHost},
			{Name: HeaderAcceptEncoding},
			{Name: HeaderContentLength},
		},
		HelloHeaders: []string{HeaderConnection, HeaderUserAgent, HeaderAccept, HeaderHost, HeaderAcceptEncoding},
		Betas: []string{
			betaClaudeCode, betaOAuth, betaContext1M, betaInterleaved, betaRedactThinking, betaThinkingTokens,
			betaContextMgmt, betaPerTurnControl, betaMidConvTools, betaInlineTools, betaAdvisorTool,
			betaAdvancedToolUse, betaEffort, betaStructuredOutput, betaDangerousTool,
			betaThinkingDisplay, betaAFKMode, betaExtendedCacheTTL, betaTokenCounting,
		},
		BetaLists: map[BetaKey][]string{
			// Captured (fixtures 002, 004, 006, 008, 010, 012, 013, 014; title from capture 011).
			{TypeTurn, CredentialOAuth, EntrypointSDKCLI}:  turnOAuth,
			{TypeTurn, CredentialAPIKey, EntrypointSDKCLI}: turnAPIKey,
			{TypeQuota, CredentialOAuth, EntrypointCLI}: {
				betaClaudeCode, betaOAuth, betaInterleaved, betaRedactThinking, betaThinkingTokens, betaContextMgmt,
			},
			{TypeTitle, CredentialOAuth, EntrypointCLI}: {
				betaClaudeCode, betaOAuth, betaInterleaved, betaRedactThinking, betaThinkingTokens, betaContextMgmt,
				betaPerTurnControl, betaMidConvTools, betaEffort, betaStructuredOutput,
			},
			{TypeTurnSafeguards, CredentialOAuth, EntrypointCLI}: {
				betaClaudeCode, betaOAuth, betaInterleaved, betaThinkingTokens, betaContextMgmt,
				betaPerTurnControl, betaMidConvTools, betaEffort, betaDangerousTool, betaThinkingDisplay,
				betaAFKMode, betaExtendedCacheTTL,
			},
			{TypeClassifier, CredentialOAuth, EntrypointCLI}: {
				betaClaudeCode, betaOAuth, betaContext1M, betaInterleaved, betaRedactThinking, betaContextMgmt,
				betaExtendedCacheTTL,
			},
			{TypeCountTokens, CredentialOAuth, EntrypointSDKCLI}:  countOAuth,
			{TypeCountTokens, CredentialAPIKey, EntrypointSDKCLI}: countAPIKey,
			// Inferred.
			{TypeCountTokens, CredentialOAuth, EntrypointCLI}:  countOAuth,
			{TypeCountTokens, CredentialAPIKey, EntrypointCLI}: countAPIKey,
			{TypeTurn, CredentialOAuth, EntrypointCLI}: {
				betaClaudeCode, betaOAuth, betaInterleaved, betaThinkingTokens, betaContextMgmt,
				betaPerTurnControl, betaMidConvTools, betaEffort, betaThinkingDisplay, betaExtendedCacheTTL,
			},
			{TypeTurn, CredentialAPIKey, EntrypointCLI}: {
				betaClaudeCode, betaInterleaved, betaThinkingTokens, betaContextMgmt,
				betaPerTurnControl, betaMidConvTools, betaInlineTools, betaAdvisorTool, betaEffort, betaThinkingDisplay,
			},
			{TypeTurnSafeguards, CredentialAPIKey, EntrypointCLI}: {
				betaClaudeCode, betaInterleaved, betaThinkingTokens, betaContextMgmt,
				betaPerTurnControl, betaMidConvTools, betaInlineTools, betaAdvisorTool, betaEffort,
				betaDangerousTool, betaThinkingDisplay, betaAFKMode,
			},
			{TypeClassifier, CredentialAPIKey, EntrypointCLI}: {
				betaClaudeCode, betaContext1M, betaInterleaved, betaRedactThinking, betaContextMgmt,
			},
			{TypeQuota, CredentialAPIKey, EntrypointCLI}: {
				betaClaudeCode, betaInterleaved, betaRedactThinking, betaThinkingTokens, betaContextMgmt,
			},
			{TypeTitle, CredentialAPIKey, EntrypointCLI}: {
				betaClaudeCode, betaInterleaved, betaRedactThinking, betaThinkingTokens, betaContextMgmt,
				betaPerTurnControl, betaMidConvTools, betaInlineTools, betaAdvisorTool, betaEffort, betaStructuredOutput,
			},
		},
		ToolSearchBeta: betaAdvancedToolUse,
		FlagBetas: []FlagBeta{
			{Beta: betaInlineTools, Types: []Type{TypeTurn, TypeTurnSafeguards, TypeTitle}},
			{Beta: betaAdvisorTool, Types: []Type{TypeTurn, TypeTurnSafeguards, TypeTitle}},
			{Beta: betaExtendedCacheTTL, Types: []Type{TypeTurn, TypeTurnSafeguards, TypeClassifier}, Only: CredentialOAuth},
		},
		KeyOrders: map[Type][][]string{
			TypeTurn: {{
				"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking",
				"context_management", "output_config", "stream",
			}},
			TypeTurnSafeguards: {{
				"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking",
				"context_management", "safeguards", "output_config", "stream",
			}},
			TypeQuota: {{"model", "max_tokens", "messages", "metadata"}},
			TypeClassifier: {
				{"model", "max_tokens", "system", "messages", "stop_sequences", "thinking", "metadata"},
				{"model", "max_tokens", "system", "messages", "thinking", "metadata"},
			},
			TypeTitle: {{
				"model", "messages", "system", "tools", "metadata", "max_tokens", "output_config", "stream",
			}},
			TypeCountTokens: {
				{"model", "messages", "tools"},
				{"model", "messages", "tools", "thinking"},
			},
		},
		Identities: map[Entrypoint]string{
			EntrypointSDKCLI: "You are a Claude agent, built on Anthropic's Claude Agent SDK.",
			EntrypointCLI:    "You are Claude Code, Anthropic's official CLI for Claude.",
		},
		ClassifierIdentity: "You are a security monitor for autonomous AI coding agents.",
		ClassifierTimeout:  "60",
		ThinkingDisplay: map[Entrypoint]string{
			EntrypointSDKCLI: "omitted",
			EntrypointCLI:    "updates",
		},
		TurnMaxTokens:     128000,
		FallbackMaxTokens: 64000,
		QuotaMaxTokens:    1,
		QuotaContent:      "quota",
		DefaultEffort:     "medium",
	})
}

#!/usr/bin/env bash
# The real Claude Code 2.1.308 in normal use (default system prompt and
# tools) against a local wwz whose anthropic mount has
# client_verification: claude-code (spec 020, task 457). Fails on any 403
# forged_request_error or any "API error" in Claude Code's debug file.
#
# Out of the verify (it needs the claude binary): skips with exit 0 when
# claude is not in PATH or is not 2.1.308. Never a real credential nor the
# keychain: env -i, an empty CLAUDE_CONFIG_DIR per mode, a setup token
# issued by the wwz itself (wwz-load claude-code login --setup-token) or a
# key of the local keys[]. Claude Code still calls api.anthropic.com on the
# account routes with the wwz token (accepted in ADR 0074).
#
# Modes (all of them by default): -p with OAuth and API key; -p --resume (2nd
# turn); -p with the Read tool (a large file: count_tokens too); -p with the
# Agent tool (a subagent); -p in a directory with CLAUDE.md; interactive
# (driven through a pty, killed after the turn) with OAuth and API key, with
# the Read tool, with a Write outside the directory (the auto-mode
# classifier), with /context and with a !command before the prompt. The
# wwz never calls a tool on its own (tool_calls probability 0): scenarios
# matched by a word of the prompt (WWZREAD, WWZAGENT, WWZWRITE) ask for one
# tool call; the Write goes to $WORK/outside and the classifier, answered
# with lorem, refuses it.
#
# Usage: tests/claudecode/default-mode.sh [mode ...]
#   CLAUDE_BIN=path   Claude Code binary (default: claude in PATH)
#   WWZ_CC_PORT=N     data plane port (default 18457; admin on N+1)
#   KEEP=1            keep the work directory (debug files, wwz log)
set -euo pipefail

CLAUDE=${CLAUDE_BIN:-$(command -v claude || true)}
if [ -z "$CLAUDE" ]; then
	echo "skip: claude not in PATH"
	exit 0
fi
ver=$("$CLAUDE" --version 2>/dev/null | awk '{print $1}')
if [ "$ver" != 2.1.308 ]; then
	echo "skip: Claude Code $ver has no profile (only 2.1.308)"
	exit 0
fi

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/wwz-cc-default.XXXXXX")
WORK=$(cd "$WORK" && pwd -P)
PORT=${WWZ_CC_PORT:-18457}
ADMIN=$((PORT + 1))
BASE=http://127.0.0.1:$PORT
KEY=sk-ant-api03-WWZDEFAULTMODE0000000000
WWZ_PID=

cleanup() {
	[ -n "$WWZ_PID" ] && kill "$WWZ_PID" 2>/dev/null || true
	if [ "${KEEP:-}" = 1 ]; then
		echo "work directory: $WORK"
	else
		rm -rf "$WORK"
	fi
}
trap cleanup EXIT

(cd "$ROOT" && go build -o "$WORK/bin/" ./cmd/wwz ./cmd/wwz-load)

# The file the Read tool reads, in the working directory of the tool modes
# (outside it Read asks for permission and -p denies it): ~40 KB, so Claude
# Code may count its tokens (POST /v1/messages/count_tokens) before
# attaching the result.
TOOLDIR=$WORK/tooldir
mkdir -p "$TOOLDIR"
python3 -c 'import sys; open(sys.argv[1], "w").write(("lorem ipsum dolor sit amet " * 20 + "\n") * 75)' "$TOOLDIR/notes.txt"

mkdir -p "$WORK/scenarios" "$WORK/outside"
# scenario <name> <tool> <word> <args>: one call of <tool> when the request
# offers it and the last user message has <word>; text after the result.
scenario() {
	cat >"$WORK/scenarios/$1.yaml" <<EOF
name: $1
match:
  tools_include: [$2]
  user_message_matches: "$3"
on_no_match: fallback
steps:
  - id: call
    expect: { tool_results: 0 }
    tool_calls:
      - { name: $2, args: $4 }
  - id: done
    expect: { tool_results_gte: 1 }
    content: "done"
    finish_reason: stop
EOF
}
scenario cc-read Read WWZREAD "{ file_path: \"$TOOLDIR/notes.txt\" }"
scenario cc-agent Agent WWZAGENT '{ description: "probe", prompt: "Subagent probe, reply ok", subagent_type: "general-purpose" }'
scenario cc-write Write WWZWRITE "{ file_path: \"$WORK/outside/probe.txt\", content: \"x\" }"

cat >"$WORK/wwz.yaml" <<EOF
server: {auth: keys, seed: 7}
store: {kind: memory}
scenarios: {builtin: false, dir: $WORK/scenarios}
drivers:
  - {name: anthropic, options: {oauth_key: assinante, client_verification: claude-code}}
keys:
  - {key: wwz-oauth-secret-default-mode, label: assinante}
  - {key: $KEY, label: cc}
profiles:
  latency: {zero: {}}
  generation:
    # probability 0 alone falls back to the default 35 (config/load.go):
    # another field set keeps it at 0.
    text: {mode: lorem, output_tokens: {dist: fixed, value: 8}, tool_calls: {probability: 0, extra_call_probability: 1}}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: text, scenarios: [cc-read, cc-agent, cc-write]}
  - {id: claude-opus-5-5, latency: zero, generation: text, scenarios: [cc-read, cc-agent, cc-write]}
  - {id: claude-haiku-4-5, latency: zero, generation: text, scenarios: [cc-read, cc-agent, cc-write]}
  - {id: claude-sonnet-5, latency: zero, generation: text, scenarios: [cc-read, cc-agent, cc-write]}
EOF

"$WORK/bin/wwz" serve -config "$WORK/wwz.yaml" -listen "127.0.0.1:$PORT" -admin-listen "127.0.0.1:$ADMIN" >"$WORK/wwz.log" 2>&1 &
WWZ_PID=$!
for _ in $(seq 50); do
	curl -sf -o /dev/null -I "$BASE/api/hello" && break
	sleep 0.1
done
"$WORK/bin/wwz-load" claude-code login --setup-token --target "$BASE" --state "$WORK/cc-state.json" >/dev/null
TOKEN=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["access_token"])' "$WORK/cc-state.json")

# cc <name> <oauth|api-key> <dir> <claude args…>: one run of Claude Code
# with an empty configuration directory of its own (kept across the runs of
# a name, for --resume), the environment cleared and the debug file
# $WORK/<name>.debug.
cc() {
	local name=$1 auth=$2 dir=$3
	shift 3
	local cfg="$WORK/cfg-$name" cred
	mkdir -p "$cfg" "$dir"
	if [ "$auth" = oauth ]; then
		cred=CLAUDE_CODE_OAUTH_TOKEN=$TOKEN
	else
		cred=ANTHROPIC_API_KEY=$KEY
	fi
	(cd "$dir" && env -i HOME="$HOME" PATH="$(dirname "$CLAUDE"):/usr/bin:/bin" TERM=xterm-256color \
		USER="$USER" SHELL=/bin/bash LANG=en_US.UTF-8 CLAUDE_CONFIG_DIR="$cfg" \
		CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 ANTHROPIC_BASE_URL="$BASE" "$cred" \
		${CC_WRAP:-} "$CLAUDE" "$@" --debug-file "$WORK/$name.debug" </dev/null)
}

# interactive <name> <oauth|api-key> <dir> <input…>: the TUI in a pty, with
# onboarding done, the directory trusted and the API key approved; each
# input is typed and sent with Enter once the previous one settled, and the
# session is killed a few seconds after the last turn ends.
interactive() {
	local name=$1 auth=$2 dir=$3
	shift 3
	local cfg="$WORK/cfg-$name"
	mkdir -p "$cfg" "$dir"
	python3 - "$cfg/.claude.json" "$dir" "$KEY" <<'PY'
import json, sys
path, d, key = sys.argv[1:]
json.dump({"hasCompletedOnboarding": True, "theme": "dark",
           "projects": {d: {"hasTrustDialogAccepted": True, "hasCompletedProjectOnboarding": True}},
           "customApiKeyResponses": {"approved": [key[-20:]], "rejected": []}}, open(path, "w"))
PY
	printf '%s\n' "$@" >"$WORK/$name.inputs"
	CC_WRAP="python3 -I $WORK/ccpty.py $WORK/$name.debug $WORK/$name.inputs --" cc "$name" "$auth" "$dir" </dev/null >"$WORK/$name.tty" 2>&1
	grep -q 'turn 1 end' "$WORK/$name.debug" || { echo "$name: the turn did not end" >&2; return 1; }
}

# ccpty.py <debug file> <inputs file> -- <command…>: runs the command in a
# pty; types each line of the inputs file (then Enter) once the screen has
# been quiet for 4 s; after a prompt (not a /command) waits up to 60 s for
# one more "turn N end" in the debug file; then 3 s more, and kills it.
cat >"$WORK/ccpty.py" <<'PY'
import os, pty, re, select, signal, sys, time
dbg, inputs_file = sys.argv[1], sys.argv[2]
cmd = sys.argv[sys.argv.index("--") + 1:]
inputs = open(inputs_file).read().splitlines()
pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
def pump(t, quiet=4):
    end, last = time.time() + t, time.time()
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.1)
        if r:
            try:
                d = os.read(fd, 65536)
            except OSError:
                return
            sys.stdout.buffer.write(d)
            last = time.time()
        elif time.time() - last > quiet:
            return
def ends():
    try:
        return len(re.findall(r"turn \d+ end", open(dbg, errors="replace").read()))
    except OSError:
        return 0
pump(15)
for text in inputs:
    before = ends()
    for ch in text:
        os.write(fd, ch.encode())
        time.sleep(0.02)
    time.sleep(0.3)
    os.write(fd, b"\r")
    deadline = time.time() + 60
    while time.time() < deadline and ends() == before and not text.startswith(("/", "!")):
        pump(0.5, quiet=1)
    pump(4)
pump(3, quiet=3)
os.kill(pid, signal.SIGTERM)
time.sleep(0.5)
try:
    os.kill(pid, signal.SIGKILL)
except ProcessLookupError:
    pass
PY

READ="WWZREAD: read notes.txt and tell me what it says"

run_mode() {
	case $1 in
	p-oauth) cc "$1" oauth "$WORK/dir-$1" -p "say ok" ;;
	p-apikey) cc "$1" api-key "$WORK/dir-$1" -p "say ok" ;;
	resume-oauth | resume-apikey)
		local auth=oauth sid
		[ "$1" = resume-apikey ] && auth=api-key
		sid=$(cc "$1" "$auth" "$WORK/dir-$1" -p "Second conversation, first turn" --output-format json |
			python3 -c 'import json, sys; print(json.load(sys.stdin)["session_id"])')
		cc "$1" "$auth" "$WORK/dir-$1" -p "and the second turn" --resume "$sid"
		;;
	tool-oauth) cc "$1" oauth "$TOOLDIR" -p "$READ" ;;
	tool-apikey) cc "$1" api-key "$TOOLDIR" -p "$READ" ;;
	agent-oauth) cc "$1" oauth "$WORK/dir-$1" -p "WWZAGENT: ask a subagent to say ok" ;;
	agent-apikey) cc "$1" api-key "$WORK/dir-$1" -p "WWZAGENT: ask a subagent to say ok" ;;
	claudemd-oauth)
		mkdir -p "$WORK/dir-$1"
		printf '# Project\n\nAlways answer in one short sentence.\n' >"$WORK/dir-$1/CLAUDE.md"
		cc "$1" oauth "$WORK/dir-$1" -p "say ok"
		;;
	interactive-oauth) interactive "$1" oauth "$WORK/dir-$1" "say ok" ;;
	interactive-apikey) interactive "$1" api-key "$WORK/dir-$1" "say ok" ;;
	interactive-tool-oauth) interactive "$1" oauth "$TOOLDIR" "$READ" ;;
	interactive-classifier-oauth) interactive "$1" oauth "$WORK/dir-$1" "WWZWRITE: write the probe file" ;;
	interactive-context-oauth) interactive "$1" oauth "$WORK/dir-$1" "/context" "say ok" ;;
	interactive-bang-oauth) interactive "$1" oauth "$WORK/dir-$1" "!echo hi" "say ok" ;;
	*)
		echo "unknown mode $1" >&2
		return 2
		;;
	esac
}

MODES=("$@")
[ ${#MODES[@]} -gt 0 ] || MODES=(p-oauth p-apikey resume-oauth resume-apikey tool-oauth tool-apikey agent-oauth agent-apikey
	claudemd-oauth interactive-oauth interactive-apikey interactive-tool-oauth interactive-classifier-oauth
	interactive-context-oauth interactive-bang-oauth)

fail=0
for m in "${MODES[@]}"; do
	if ! run_mode "$m" >"$WORK/$m.out" 2>&1; then
		echo "FAIL $m: claude failed: $(tail -c 400 "$WORK/$m.out")"
		fail=1
		continue
	fi
	reqs=$(grep -o '\[API REQUEST\] [^ ]* source=[a-z_]*' "$WORK/$m.debug" | sed 's/\[API REQUEST\] //' | sort | uniq -c | awk '{printf "%s %s×%s; ", $2, $3, $1}')
	if grep -q 'API error\|forged_request_error' "$WORK/$m.debug"; then
		echo "FAIL $m: $(grep -m1 -o 'API error.*' "$WORK/$m.debug" | cut -c1-400)"
		fail=1
	else
		echo "ok   $m: ${reqs:-no request}"
	fi
done

forged=$(curl -s "http://127.0.0.1:$ADMIN/metrics" | grep '^wwz_client_verification_failures_total' || true)
if [ -n "$forged" ]; then
	echo "FAIL refusals of the strict verification:"
	echo "$forged"
	grep client_verification_reason "$WORK/wwz.log" | cut -c1-600 || true
	fail=1
fi
tools=$(curl -s "http://127.0.0.1:$ADMIN/metrics" | grep '^wwz_tool_calls_total' | grep -v 'tool="\(Read\|Agent\|Write\)"' || true)
if [ -n "$tools" ]; then
	echo "FAIL the wwz called tools other than the scenarios':"
	echo "$tools"
	fail=1
fi
curl -s "http://127.0.0.1:$ADMIN/metrics" | grep '^wwz_requests_total\|^wwz_tool_calls_total' || true
exit $fail

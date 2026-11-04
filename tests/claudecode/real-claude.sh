#!/usr/bin/env bash
# Roteiro do Claude Code real contra o wwz (task 447, ADR 0074, spec 020).
# Fora do verify: roda à mão, uma vez por versão do Claude Code. Ver README.md.
#
# Sobe um wwz local com o driver anthropic em dois mounts, /strict
# (client_verification: claude-code) e /off (sem a opção), ambos com a
# assinatura OAuth da chave "assinante"; emite um access token com
# `wwz-load claude-code login --setup-token` (token do wwz, não é credencial
# real) e roda o `claude` instalado com `env -i`, CLAUDE_CONFIG_DIR vazio e
# CLAUDE_CODE_OAUTH_TOKEN=<token do wwz>, em dois modos:
#   minimo  claude -p --system-prompt "You are a terse test assistant." --tools "" "say ok"
#   padrao  claude -p "say ok" (prompt e tools padrão do Claude Code)
#   projeto o mínimo num cwd com CLAUDE.md (o Claude Code o manda como
#           <system-reminder> antes de "say ok", na mesma mensagem)
# Cada caso passa com saída 0 e "ok" no stdout. Sai 0 só se todos passarem;
# sem `claude` no PATH, sai 0 dizendo "pulado".
#
# Variáveis: CLAUDE_BIN (padrão: `command -v claude`), WWZ_REAL_CLAUDE_MODES
# (padrão "minimo padrao projeto"), WWZ_REAL_CLAUDE_MOUNTS (padrão "strict off"),
# WWZ_REAL_CLAUDE_KEEP=1 mantém o diretório temporário (log do wwz).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLAUDE_BIN="${CLAUDE_BIN:-$(command -v claude || true)}"
if [ -z "$CLAUDE_BIN" ]; then
	echo "real-claude: pulado (claude fora do PATH)"
	exit 0
fi
CLAUDE_VERSION="$("$CLAUDE_BIN" --version 2>/dev/null | awk '{print $1}')"
MODES="${WWZ_REAL_CLAUDE_MODES:-minimo padrao projeto}"
# timeout(1) do coreutils, se houver (no macOS, do Homebrew).
TIMEOUT=()
if t="$(command -v timeout || command -v gtimeout)"; then
	TIMEOUT=("$t" 120)
fi
MOUNTS="${WWZ_REAL_CLAUDE_MOUNTS:-strict off}"

# Fora do repositório: o Claude Code lê o CLAUDE.md/AGENTS.md dos diretórios
# acima do cwd e os manda como <system-reminder> na primeira mensagem, o que
# muda a forma do turno (ver README.md).
WORK="$(mktemp -d "${TMPDIR:-/tmp}/wwz-real-claude.XXXXXX")"
WWZ_PID=""
cleanup() {
	if [ -n "$WWZ_PID" ]; then
		kill "$WWZ_PID" 2>/dev/null || true
		wait "$WWZ_PID" 2>/dev/null || true
	fi
	if [ "${WWZ_REAL_CLAUDE_KEEP:-0}" = 1 ]; then
		echo "real-claude: diretório mantido em $WORK"
	else
		rm -rf "$WORK"
	fi
}
trap cleanup EXIT

free_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'
}

echo "real-claude: Claude Code $CLAUDE_VERSION ($CLAUDE_BIN)"
(cd "$ROOT" && go build -o "$WORK/bin/wwz" ./cmd/wwz && go build -o "$WORK/bin/wwz-load" ./cmd/wwz-load)

PORT="$(free_port)"
APORT="$(free_port)"
# Segredo dos tokens OAuth (oauth_key) gerado por execução: só este wwz o
# conhece. Os modelos respondem "ok", sem tool call (o modo padrão manda as
# tools do Claude Code).
SECRET="wwz-real-claude-$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
cat >"$WORK/wwz.yaml" <<EOF
server: {auth: keys, seed: 7}
admin: {listen: "127.0.0.1:$APORT"}
store: {kind: memory}
logging: {level: info, format: text}
drivers:
  - {name: anthropic, prefix: /strict, options: {oauth_key: assinante, client_verification: claude-code}}
  - {name: anthropic, prefix: /off, options: {oauth_key: assinante}}
keys:
  - {key: $SECRET, label: assinante}
profiles:
  latency: {zero: {}}
  generation:
    ok:
      mode: template
      template: "ok"
      output_tokens: {dist: fixed, value: 1}
      reasoning_tokens: {min: 0, max: 0, probability: 0}
      tool_calls: {probability: 0}
models:
  - {id: claude-sonnet-5-5, latency: zero, generation: ok}
  - {id: claude-opus-5-5, latency: zero, generation: ok}
  - {id: claude-haiku-5-5, latency: zero, generation: ok}
EOF

"$WORK/bin/wwz" serve --config "$WORK/wwz.yaml" --listen "127.0.0.1:$PORT" >"$WORK/wwz.log" 2>&1 &
WWZ_PID=$!
BASE="http://127.0.0.1:$PORT"
for _ in $(seq 1 100); do
	curl -fsS -o /dev/null -I "$BASE/strict/api/hello" 2>/dev/null && break
	sleep 0.1
done
curl -fsS -o /dev/null -I "$BASE/strict/api/hello"

# Access token do wwz (como o `claude setup-token`: só user:inference, 1 ano).
"$WORK/bin/wwz-load" claude-code login --setup-token --target "$BASE/strict" --state "$WORK/state.json" >/dev/null
TOKEN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["access_token"])' "$WORK/state.json")"
case "$TOKEN" in sk-ant-oat01-*) ;; *)
	echo "real-claude: token inesperado do wwz" >&2
	exit 1
	;;
esac

fails=0
printf '\n%-8s %-8s %-6s %-5s %s\n' modo mount saida ok detalhe
for mode in $MODES; do
	for mount in $MOUNTS; do
		home="$WORK/home-$mode-$mount"
		cfg="$WORK/cfg-$mode-$mount"
		cwd="$WORK/cwd-$mode-$mount"
		mkdir -p "$home" "$cfg" "$cwd"
		if [ "$mode" = projeto ]; then
			printf '# Projeto de teste\n\nResponda curto.\n' >"$cwd/CLAUDE.md"
		fi
		if [ "$mode" != padrao ]; then
			# O prompt vem logo depois de -p: --tools é variádico e engoliria
			# um argumento posicional depois dele.
			args=(-p "say ok" --system-prompt "You are a terse test assistant." --tools "")
		else
			args=(-p "say ok")
		fi
		before="$(grep -c 'error=forged_request' "$WORK/wwz.log" || true)"
		set +e
		out="$(cd "$cwd" && env -i HOME="$home" PATH="$(dirname "$CLAUDE_BIN"):/usr/bin:/bin" \
			ANTHROPIC_BASE_URL="$BASE/$mount" CLAUDE_CODE_OAUTH_TOKEN="$TOKEN" \
			CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 CLAUDE_CONFIG_DIR="$cfg" \
			${TIMEOUT[@]+"${TIMEOUT[@]}"} "$CLAUDE_BIN" "${args[@]}" </dev/null 2>"$WORK/stderr-$mode-$mount")"
		code=$?
		set -e
		detail=""
		forged="$(grep 'error=forged_request' "$WORK/wwz.log" | tail -n +"$((before + 1))" | head -1 || true)"
		if [ -n "$forged" ]; then
			detail="403 $(printf '%s' "$forged" | grep -o 'client_verification_reason=[a-z_]*' | head -1 | cut -d= -f2): $(printf '%s' "$forged" | sed -n 's/.*client_verification_detail="\([^"]*\)".*/\1/p')"
		fi
		okmark=nao
		if [ "$code" = 0 ] && printf '%s' "$out" | grep -qx 'ok'; then
			okmark=sim
		else
			fails=$((fails + 1))
			[ -n "$detail" ] || detail="stdout=$(printf '%s' "$out" | head -c 160 | tr '\n' ' ') stderr=$(head -c 160 "$WORK/stderr-$mode-$mount" | tr '\n' ' ')"
		fi
		printf '%-8s %-8s %-6s %-5s %s\n' "$mode" "/$mount" "$code" "$okmark" "$detail"
	done
done

echo
echo "real-claude: requisições ao wwz (endpoint, status):"
grep 'msg=request ' "$WORK/wwz.log" | sed -n 's/.*endpoint=\([^ ]*\) status=\([0-9]*\).*/\1 \2/p' | sort | uniq -c | sed 's/^/  /' || true
if [ "$fails" -gt 0 ]; then
	echo "real-claude: $fails caso(s) falharam" >&2
	exit 1
fi
echo "real-claude: todos os casos passaram"

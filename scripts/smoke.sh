#!/usr/bin/env bash
# Smoke do ambiente docker compose (spec 018): sobe wwz + Valkey + Prometheus +
# wwz-panel + wwz-tools, faz uma chamada por driver (stream e não-stream), confere o control
# plane, o alvo e as regras de alerta do Prometheus, o painel (saúde, login do
# operador inicial, uso, bloqueio/desbloqueio de key/test-01 conferido no
# /admin/entities do wwz), roda o wwz-load (perfil load) por 5 s exigindo
# TTFT/ITL/E2E no JSON e confere os eventos no painel: uso por chave,
# wwz_panel_events_ingested_total > 0 e, com valkey, o evento block.
# Derruba tudo (down -v) no fim. Sai ≠0 em qualquer falha.
#
#   make smoke
#   WWZ_STORE=memory make smoke              # sem store Valkey
#   SMOKE_PROJECT=wwz-x WWZ_PORT=28000 WWZ_ADMIN_PORT=28001 WWZ_PANEL_PORT=28002 \
#     VALKEY_PORT=26379 PROMETHEUS_PORT=29090 make smoke
#
# Variáveis: SMOKE_PROJECT (nome do projeto compose, padrão wwz-smoke),
# SMOKE_KEEP=1 (não derruba no fim), SMOKE_TIMEOUT (segundos, padrão 90),
# SMOKE_KEY (chave de API, padrão wwz-key-01), WWZ_TOOLS_PORT (padrão 18003), SMOKE_ENTITY (id da chave
# bloqueada pelo painel, padrão test-01). WWZ_ADMIN_TOKEN e
# WWZ_PANEL_ADMIN_PASSWORD são gerados aleatoriamente se não definidos.
# Requer curl e jq.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Lê os padrões do .env sem sobrescrever o que veio do ambiente.
if [[ -f .env ]]; then
	while IFS='=' read -r k v; do
		[[ "$k" =~ ^[A-Z_][A-Z0-9_]*$ ]] || continue
		[[ -n "${!k+x}" ]] || export "$k=$v"
	done < <(grep -E '^[A-Z_][A-Z0-9_]*=' .env)
fi

PROJECT="${SMOKE_PROJECT:-wwz-smoke}"
TIMEOUT="${SMOKE_TIMEOUT:-90}"
KEY="${SMOKE_KEY:-wwz-key-01}"
export WWZ_ADMIN_TOKEN="${WWZ_ADMIN_TOKEN:-smoke-$RANDOM$RANDOM}"
# WWZ_VALKEY_ADDR força store valkey no wwz; com memory, fica vazio.
if [[ "${WWZ_STORE:-valkey}" == "memory" ]]; then
	export WWZ_VALKEY_ADDR=""
fi

command -v jq >/dev/null || {
	echo "smoke: jq é obrigatório" >&2
	exit 2
}

# Painel: operador inicial com senha aleatória local (volume novo a cada smoke).
PANEL_USER="${WWZ_PANEL_ADMIN_USER:-admin}"
export WWZ_PANEL_ADMIN_PASSWORD="${WWZ_PANEL_ADMIN_PASSWORD:-smoke-$RANDOM$RANDOM$RANDOM}"
ENTITY="${SMOKE_ENTITY:-test-01}"

DATA="http://127.0.0.1:${WWZ_PORT:-18000}"
PANEL="http://127.0.0.1:${WWZ_PANEL_PORT:-18002}"
ADMIN="http://127.0.0.1:${WWZ_ADMIN_PORT:-18001}"
PROM="http://127.0.0.1:${PROMETHEUS_PORT:-19090}"

compose() { docker compose -p "$PROJECT" "$@"; }

FAILS=0
pass() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*" >&2
	FAILS=$((FAILS + 1))
}

cleanup() {
	local code=$?
	[[ -n "${JAR:-}" ]] && rm -f "$JAR"
	if ((code != 0 || FAILS != 0)); then
		echo "--- logs (wwz, wwz-panel) ---" >&2
		compose logs --no-color --tail 50 wwz wwz-panel >&2 || true
	fi
	if [[ "${SMOKE_KEEP:-0}" != "1" ]]; then
		compose down -v --remove-orphans >/dev/null 2>&1 || true
	fi
	exit "$code"
}
trap cleanup EXIT

wait_for() { # descrição, comando...
	local what=$1
	shift
	local deadline=$((SECONDS + TIMEOUT))
	until "$@" >/dev/null 2>&1; do
		if ((SECONDS >= deadline)); then
			fail "$what (timeout ${TIMEOUT}s)"
			return 1
		fi
		sleep 1
	done
	pass "$what"
}

# status METHOD URL [curl args...] -> imprime o código HTTP
status() {
	local method=$1 url=$2
	shift 2
	curl -sS -o /dev/null -w '%{http_code}' -X "$method" --max-time 30 "$@" "$url" || true
}

# check NOME URL HEADER BODY PADRAO: POST que deve dar 200 e conter PADRAO.
check() {
	local name=$1 url=$2 hdr=$3 body=$4 want=$5 out code
	out=$(curl -sS -N --max-time 30 -w '\n%{http_code}' -H 'Content-Type: application/json' \
		-H "$hdr" -H 'anthropic-version: 2023-06-01' -H 'x-opencode-session: ses-smoke' -d "$body" "$url") || {
		fail "$name: curl"
		return 0
	}
	code=${out##*$'\n'}
	if [[ "$code" != "200" ]]; then
		fail "$name: HTTP $code: $(head -c 300 <<<"${out%$'\n'*}")"
	elif ! grep -q -- "$want" <<<"$out"; then
		fail "$name: resposta sem '$want': $(head -c 300 <<<"$out")"
	else
		pass "$name"
	fi
}

echo "== smoke: projeto $PROJECT, store ${WWZ_STORE:-valkey}"
# Projeto dedicado ao smoke: começa limpo (sobra de SMOKE_KEEP=1 deixaria chave
# bloqueada e o operador do painel com outra senha).
compose --profile load down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d --build --wait --wait-timeout "$TIMEOUT"

wait_for "wwz /health/ready" curl -fsS --max-time 2 "$ADMIN/health/ready"

BEARER="Authorization: Bearer $KEY"
XKEY="x-api-key: $KEY"
OAI='{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":16}'
OAI_S='{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":16,"stream":true}'
ANT='{"model":"wwz-fast","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}'
ANT_S='{"model":"wwz-fast","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"oi"}]}'

check "openai     /v1/chat/completions" "$DATA/v1/chat/completions" "$BEARER" "$OAI" '"chat.completion"'
check "openai     /v1/chat/completions stream" "$DATA/v1/chat/completions" "$BEARER" "$OAI_S" 'data: \[DONE\]'
check "anthropic  /v1/messages" "$DATA/v1/messages" "$XKEY" "$ANT" '"type":"message"'
check "anthropic  /v1/messages stream" "$DATA/v1/messages" "$XKEY" "$ANT_S" 'message_stop'
check "openrouter /api/v1/chat/completions" "$DATA/api/v1/chat/completions" "$BEARER" "$OAI" '"chat.completion"'
check "openrouter /api/v1/chat/completions stream" "$DATA/api/v1/chat/completions" "$BEARER" "$OAI_S" 'data: \[DONE\]'
check "opencode   /zen/go/v1/chat/completions" "$DATA/zen/go/v1/chat/completions" "$BEARER" "$OAI" '"chat.completion"'
check "opencode   /zen/go/v1/chat/completions stream" "$DATA/zen/go/v1/chat/completions" "$BEARER" "$OAI_S" 'data: \[DONE\]'

# Cenários embutidos na imagem (spec 011): os seis nomes têm de estar carregados.
BUILTIN_SCENARIOS="file-agent-flow long-reasoning multi-tool-parallel refusal retry-on-error search-then-answer"
scenario_names=$(curl -sS --max-time 10 -H "Authorization: Bearer $WWZ_ADMIN_TOKEN" "$ADMIN/admin/scenarios" |
	jq -r '[.scenarios[].name] | join(" ")' 2>/dev/null || true)
missing_scn=""
for n in $BUILTIN_SCENARIOS; do
	[[ " $scenario_names " == *" $n "* ]] || missing_scn="$missing_scn $n"
done
[[ -z "$missing_scn" ]] && pass "/admin/scenarios lista os 6 cenários embutidos" ||
	fail "/admin/scenarios sem:$missing_scn (obtido: ${scenario_names:-nada})"

code=$(status GET "$ADMIN/admin/config")
[[ "$code" == "401" ]] && pass "/admin/config sem token -> 401" || fail "/admin/config sem token -> $code (esperado 401)"
code=$(status GET "$ADMIN/admin/config" -H "Authorization: Bearer $WWZ_ADMIN_TOKEN")
[[ "$code" == "200" ]] && pass "/admin/config com token -> 200" || fail "/admin/config com token -> $code (esperado 200)"

prom_up() {
	curl -fsS --max-time 2 -G "$PROM/api/v1/query" --data-urlencode 'query=up{job="wwz"}' |
		grep -Eq '"value":\[[0-9.]+,"1"\]'
}
wait_for "prometheus alvo wwz up" prom_up || true

# wwz-tools (spec 017): serviço do compose, /healthz direto e alvo up no Prometheus.
TOOLS="http://127.0.0.1:${WWZ_TOOLS_PORT:-18003}"
wait_for "wwz-tools /healthz" curl -fsS --max-time 2 "$TOOLS/healthz" || true
prom_tools_up() {
	curl -fsS --max-time 2 -G "$PROM/api/v1/query" --data-urlencode 'query=up{job="wwz-tools"}' |
		grep -Eq '"value":\[[0-9.]+,"1"\]'
}
wait_for "prometheus alvo wwz-tools up" prom_tools_up || true

prom_rules() {
	curl -fsS --max-time 2 "$PROM/api/v1/rules?type=alert" | jq -e \
		'[.data.groups[].rules[].name] | contains(["WWZInstanceDown","WWZStoreCircuitOpen","WWZHigh429Rate","WWZSimulatorOverhead","WWZPanelStreamLag"])'
}
wait_for "prometheus regras de alerta da spec 016 carregadas" prom_rules || true

# --- Painel (spec 016): saúde, login do operador inicial, uso, bloqueio/desbloqueio.
wait_for "wwz-panel /health/ready" curl -fsS --max-time 2 "$PANEL/health/ready"

JAR=$(mktemp "${TMPDIR:-/tmp}/wwz-smoke-jar.XXXXXX")
LOGIN=$(jq -nc --arg u "$PANEL_USER" --arg p "$WWZ_PANEL_ADMIN_PASSWORD" '{username:$u,password:$p}')
code=$(status POST "$PANEL/api/v1/login" -c "$JAR" -H 'Content-Type: application/json' -d "$LOGIN")
[[ "$code" == "200" ]] && pass "painel login do operador inicial -> 200" || fail "painel login -> $code (esperado 200)"
CSRF=$(curl -sS --max-time 10 -b "$JAR" "$PANEL/api/v1/me" | jq -r '.csrf // empty' || true)
[[ -n "$CSRF" ]] && pass "painel /api/v1/me devolve csrf" || fail "painel /api/v1/me sem csrf"

code=$(status GET "$PANEL/api/v1/usage?group=driver" -b "$JAR")
[[ "$code" == "200" ]] && pass "painel /api/v1/usage -> 200" || fail "painel /api/v1/usage -> $code (esperado 200)"

# panel_action ACAO ESTADO: ação no painel e conferência no /admin/entities do wwz.
panel_action() {
	local action=$1 want=$2 got body=()
	# Só o block aceita corpo (BlockRequest); unblock vai sem corpo.
	[[ "$action" == "block" ]] && body=(-H 'Content-Type: application/json' -d '{"reason":"smoke"}')
	code=$(status POST "$PANEL/api/v1/entities/key/$ENTITY/$action" -b "$JAR" \
		-H "X-CSRF-Token: $CSRF" ${body[@]+"${body[@]}"})
	if [[ "$code" != "200" ]]; then
		fail "painel $action key/$ENTITY -> $code (esperado 200)"
		return 0
	fi
	got=$(curl -sS --max-time 10 -H "Authorization: Bearer $WWZ_ADMIN_TOKEN" \
		"$ADMIN/admin/entities/key/$ENTITY" | jq -r '.status // empty' || true)
	[[ "$got" == "$want" ]] && pass "painel $action key/$ENTITY -> wwz status $got" ||
		fail "painel $action key/$ENTITY -> wwz status '$got' (esperado $want)"
}
panel_action block suspended
panel_action unblock active

# --- Imagem load (spec 018): wwz-tools e wwz-drift respondem `version` com o commit.
LOAD_IMAGE="${WWZ_LOAD_IMAGE:-white-wizard-load:dev}"
compose --profile load build wwz-load >&2
for tool in wwz-tools wwz-drift; do
	got=$(docker run --rm "$LOAD_IMAGE" "$tool" version 2>&1 || true)
	if [[ "$got" == "$tool "*"(commit "* ]]; then
		pass "imagem load: $got"
	else
		fail "imagem load: $tool version sem '(commit ': ${got:0:200}"
	fi
done

# --- wwz-load (spec 015): 5 s contra o wwz pelo perfil load; exige TTFT/ITL/E2E.
# Build separado: a saída do build não pode se misturar ao JSON do stdout.
compose --profile load build wwz-load >&2
LOAD_OUT=$(compose run --rm -T wwz-load --rps 20 --duration 5s --progress 0 --output json) ||
	fail "wwz-load: saiu com erro"
if jq -e '.total.requests > 0 and .total.ttft_ms.count > 0 and .total.itl_ms.count > 0 and .total.e2e_ms.count > 0' \
	<<<"$LOAD_OUT" >/dev/null 2>&1; then
	pass "wwz-load 5s: $(jq -r '.total | "\(.requests) req, \(.errors) erros, TTFT p50 \(.ttft_ms.p50) ms, ITL p50 \(.itl_ms.p50) ms, E2E p50 \(.e2e_ms.p50) ms"' <<<"$LOAD_OUT")"
else
	fail "wwz-load: JSON sem TTFT/ITL/E2E: $(head -c 300 <<<"$LOAD_OUT")"
fi

# --- Eventos no painel (deploy/wwz/wwz.yaml): com valkey vêm do ev:stream
# (store.event_stream); com memory, do /admin/requests (admin.debug). O tráfego
# acima tem de aparecer no uso por chave e no contador de ingestão.
panel_usage_key() {
	curl -fsS --max-time 5 -b "$JAR" "$PANEL/api/v1/usage?group=key" |
		jq -e '[.rows[] | select(.value != "" and .requests > 0)] | length > 0'
}
wait_for "painel /api/v1/usage?group=key com uso do tráfego" panel_usage_key || true
panel_ingested() {
	curl -fsS --max-time 5 "$PANEL/metrics" |
		awk '$1 == "wwz_panel_events_ingested_total" { ok = ($2 + 0 > 0) } END { exit !ok }'
}
wait_for "painel wwz_panel_events_ingested_total > 0" panel_ingested || true
# Bloqueio vira evento só no ev:stream; sem Valkey a auditoria já foi conferida.
if [[ "${WWZ_STORE:-valkey}" != "memory" ]]; then
	panel_block_event() {
		curl -fsS --max-time 5 -b "$JAR" "$PANEL/api/v1/events?type=block&entity_kind=key" |
			jq -e '.events | length > 0'
	}
	wait_for "painel /api/v1/events?type=block com o bloqueio de key/$ENTITY" panel_block_event || true
fi
rm -f "$JAR"

if ((FAILS != 0)); then
	echo "== smoke: $FAILS falha(s)" >&2
	exit 1
fi
echo "== smoke: tudo verde"

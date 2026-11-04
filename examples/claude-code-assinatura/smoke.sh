#!/usr/bin/env bash
# Smoke do exemplo claude-code-assinatura (ADR 0016, ADR 0074 item 17): login OAuth
# pelo wwz-load claude-code, um turno (ask) e o replay de uma fixture do Claude
# Code 2.1.308 no mount estrito (200); um curl comum e um SDK-like com o token no
# mesmo mount (403 forged_request_error); os headers anthropic-ratelimit-unified-*
# no mount sem verificação; a cota de 5 h esgotada (429 status rejected) e o ask
# que não retenta esse 429, como o Claude Code assinante.
#
# O cliente roda no host: WWZ_LOAD_BIN aponta um wwz-load pronto; sem ele, o smoke
# compila ./cmd/wwz-load num diretório temporário (exige Go, como o passo examples).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init claude-code-assinatura 17

ROOT="$(cd "$EX_DIR/../.." && pwd)"
FIXTURE="$ROOT/testdata/real/claude-code/2.1.308/002-sdk-cli-oauth-turn.raw"
[ -f "$FIXTURE" ] || ex_fail "fixture ausente: $FIXTURE"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/wwz-ex-cc.XXXXXX")"
trap 'rc=$?; set +e; rm -rf "$WORK"; (exit $rc); ex_down' EXIT

LOAD="${WWZ_LOAD_BIN:-}"
if [ -z "$LOAD" ]; then
	command -v go >/dev/null 2>&1 || ex_fail "go não encontrado (ou defina WWZ_LOAD_BIN)"
	(cd "$ROOT" && go build -trimpath -o "$WORK/wwz-load" ./cmd/wwz-load) || ex_fail "go build ./cmd/wwz-load"
	LOAD="$WORK/wwz-load"
fi
STATE="$WORK/claude-code.json"

ex_up

BASE="http://127.0.0.1:$EX_PORT"
STRICT="$BASE/claude-code" # client_verification: claude-code
OFF="$BASE/anthropic"      # a API real (sem verificação)
HDR="$WORK/headers"

# cc <ação> [args...]: wwz-load claude-code contra o mount estrito, com o estado do smoke.
cc() {
	local action="$1"
	shift
	"$LOAD" claude-code "$action" --target "$STRICT" --state "$STATE" "$@"
}

# hdr <nome>: valor do header da última chamada (sem caixa).
hdr() { grep -i "^$1:" "$HDR" | head -1 | cut -d: -f2- | sed 's/^ *//' | tr -d '\r'; }

# post <url> <corpo> <curl args...>: POST com headers em $HDR; preenche
# EX_HTTP_CODE e EX_HTTP_BODY.
post() {
	local url="$1" body="$2" out
	shift 2
	out="$(curl -sS --max-time 30 -D "$HDR" -w $'\n%{http_code}' -H 'anthropic-version: 2023-06-01' \
		-H 'content-type: application/json' "$@" -d "$body" "$url" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
}

# 429s no log do wwz (o ask do assinante manda uma requisição só).
count429() { ex_compose logs --no-color wwz 2>/dev/null | grep -c '"status":429' || true; }

BODY='{"model":"claude-sonnet-5-5","max_tokens":1024,"messages":[{"role":"user","content":"oi"}]}'

# --- 0. O cliente recusa hosts reais da Anthropic, sem abrir conexão (código 2).
set +e
"$LOAD" claude-code ask oi --target https://api.anthropic.com --state "$WORK/nao-usado.json" >/dev/null 2>"$WORK/err"
rc=$?
set -e
[ "$rc" = 2 ] && grep -q 'refusing real Anthropic host' "$WORK/err" ||
	ex_fail "--target real: esperado código 2 e 'refusing real Anthropic host', veio $rc: $(cat "$WORK/err")"
echo "ok  --target https://api.anthropic.com recusado (código 2)"

# --- 1. HEAD /api/hello: a sonda do Claude Code, fora da verificação.
code="$(curl -sS -I -o /dev/null -w '%{http_code}' --max-time 10 "$STRICT/api/hello")"
[ "$code" = 200 ] || ex_fail "HEAD /claude-code/api/hello: HTTP $code"
echo "ok  HEAD /api/hello 200 no mount estrito"

# --- 2. Login OAuth (PKCE, callback local, token, perfil, papéis).
out="$(cc login)" || ex_fail "login falhou: $out"
grep -q '^plan  *max (default_claude_max_20x)$' <<<"$out" || ex_fail "login sem o plano max_20x: $out"
TOKEN="$(jq -r .access_token "$STATE")"
case "$TOKEN" in sk-ant-oat01-*) ;; *) ex_fail "access token fora da forma sk-ant-oat01-…: ${TOKEN:0:20}" ;; esac
echo "ok  login: token sk-ant-oat01-…, plano max (default_claude_max_20x)"

# --- 3. ask: um turno byte a byte do Claude Code, aceito pelo modo estrito.
out="$(cc ask "say ok" 2>&1)" || ex_fail "ask no mount estrito falhou: $out"
[ -n "$out" ] || ex_fail "ask sem texto"
echo "ok  ask no mount estrito: 200"

# --- 4. replay --send de uma fixture real (002, claude -p com OAuth), com o token do wwz.
out="$("$LOAD" claude-code replay "$FIXTURE" --send --target "$STRICT" --token "$TOKEN" 2>&1)" ||
	ex_fail "replay --send falhou: $out"
grep -q '^identical sha256=5644b70ff35f1ac004b1a44597fef60c5a4fae949c14026b92ab3b749a42014d$' <<<"$out" ||
	ex_fail "replay não reconstruiu a fixture byte a byte: $out"
grep -q '^sent: HTTP 200$' <<<"$out" || ex_fail "replay --send sem 200: $out"
echo "ok  replay da fixture 002: identical e HTTP 200"

# --- 5. Cliente comum no mount estrito: 403 forged_request_error, sem débito de cota.
post "$STRICT/v1/messages" "$BODY" -H 'x-api-key: wwz-ex-claude-code-assinante'
[ "$EX_HTTP_CODE" = 403 ] || ex_fail "curl comum no mount estrito: HTTP $EX_HTTP_CODE (esperado 403): $EX_HTTP_BODY"
jq -e '.type == "error" and .error.type == "forged_request_error"
	and (.error.message | startswith("Client verification failed: this request does not match Claude Code 2.1.308"))
	and (.request_id | startswith("req_"))' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "403 fora da forma forged_request_error: $EX_HTTP_BODY"
[ "$(hdr x-wwz-client-verification)" = "failed; reason=query" ] || ex_fail "x-wwz-client-verification: $(hdr x-wwz-client-verification)"
[ "$(hdr x-should-retry)" = false ] || ex_fail "x-should-retry do 403: $(hdr x-should-retry)"
echo "ok  curl comum (sem ?beta=true): 403 forged_request_error, reason=query"

# O token do wwz e ?beta=true não bastam: faltam os headers do Claude Code.
post "$STRICT/v1/messages?beta=true" "$BODY" -H "Authorization: Bearer $TOKEN" -H 'anthropic-beta: oauth-2025-04-20'
[ "$EX_HTTP_CODE" = 403 ] && [ "$(hdr x-wwz-client-verification)" = "failed; reason=header_missing" ] ||
	ex_fail "curl com token e ?beta=true: HTTP $EX_HTTP_CODE, $(hdr x-wwz-client-verification) (esperado 403 header_missing)"
echo "ok  curl com o token e ?beta=true: 403 reason=header_missing"

# --- 6. Headers de assinatura no mount sem verificação (mesma conta, mesma cota).
# Antes desta chamada, ask e replay gastaram ~US$ 3 de US$ 12 (5 h) e de US$ 30 (semana).
post "$OFF/v1/messages" "$BODY" -H "Authorization: Bearer $TOKEN" -H 'anthropic-beta: oauth-2025-04-20'
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "token no mount /anthropic: HTTP $EX_HTTP_CODE: $EX_HTTP_BODY"
[ "$(hdr anthropic-ratelimit-unified-status)" = allowed ] ||
	ex_fail "anthropic-ratelimit-unified-status: '$(hdr anthropic-ratelimit-unified-status)' (esperado allowed)"
[ "$(hdr anthropic-ratelimit-unified-5h-utilization)" = 0.25 ] ||
	ex_fail "5h-utilization: '$(hdr anthropic-ratelimit-unified-5h-utilization)' (esperado 0.25)"
[ "$(hdr anthropic-ratelimit-unified-7d-utilization)" = 0.1 ] ||
	ex_fail "7d-utilization: '$(hdr anthropic-ratelimit-unified-7d-utilization)' (esperado 0.1)"
[ "$(hdr anthropic-ratelimit-unified-representative-claim)" = five_hour ] ||
	ex_fail "representative-claim: '$(hdr anthropic-ratelimit-unified-representative-claim)'"
[ -z "$(hdr anthropic-ratelimit-requests-limit)" ] || ex_fail "token OAuth não leva anthropic-ratelimit-requests-*"
echo "ok  token no /anthropic: 200, unified-status allowed, 5h 0.25, 7d 0.1, claim five_hour"

# --- 7. Esgota a janela de 5 h (US$ 12, ~US$ 1,50 por chamada): 429 status rejected.
ok200=0
for _ in $(seq 1 12); do
	post "$OFF/v1/messages" "$BODY" -H "Authorization: Bearer $TOKEN" -H 'anthropic-beta: oauth-2025-04-20'
	case "$EX_HTTP_CODE" in
	200) ok200=$((ok200 + 1)) ;;
	429) break ;;
	*) ex_fail "HTTP $EX_HTTP_CODE inesperado ao esgotar a cota: $EX_HTTP_BODY" ;;
	esac
done
[ "$EX_HTTP_CODE" = 429 ] || ex_fail "nenhum 429 em 12 chamadas"
ex_assert_between "$ok200" 4 6 "respostas 200 até esgotar a janela de 5 h"
jq -e '.type == "error" and .error.type == "rate_limit_error"' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "429 fora da forma rate_limit_error: $EX_HTTP_BODY"
[ "$(hdr anthropic-ratelimit-unified-status)" = rejected ] ||
	ex_fail "unified-status do 429: '$(hdr anthropic-ratelimit-unified-status)' (esperado rejected)"
[ "$(hdr anthropic-ratelimit-unified-representative-claim)" = five_hour ] ||
	ex_fail "representative-claim do 429: '$(hdr anthropic-ratelimit-unified-representative-claim)'"
ex_assert_between "$(hdr retry-after)" 1 18010 "retry-after (s) do 429 de cota"
echo "ok  cota de 5 h esgotada: 429 rate_limit_error, unified-status rejected, claim five_hour"

# --- 8. O ask do assinante não retenta o 429 de assinatura (x-should-retry: true).
[ "$(hdr x-should-retry)" = true ] || ex_fail "x-should-retry do 429: $(hdr x-should-retry)"
before="$(count429)"
set +e
cc ask "say ok" >/dev/null 2>"$WORK/err"
rc=$?
set -e
[ "$rc" = 1 ] && grep -q 'HTTP 429' "$WORK/err" || ex_fail "ask com a cota esgotada: código $rc (esperado 1 e HTTP 429): $(cat "$WORK/err")"
after="$(count429)"
[ "$((after - before))" = 1 ] || ex_fail "ask mandou $((after - before)) requisições com o 429 de assinatura (esperado 1, sem retentativa)"
echo "ok  ask com a cota esgotada: 429, uma requisição só (sem retentativa)"

echo "OK claude-code-assinatura"

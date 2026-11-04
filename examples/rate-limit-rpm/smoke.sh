#!/usr/bin/env bash
# Smoke do exemplo rate-limit-rpm (ADR 0016): rajada acima do rpm da conta acme
# (30) devolve 429 com retry-after e x-ratelimit-*; o remaining decresce; a
# segunda chave da mesma conta divide o limite; a conta globex não é afetada.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init rate-limit-rpm 5
ex_up

URL="http://127.0.0.1:$EX_PORT/v1/chat/completions"
BODY='{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}'
HDR="$(mktemp)"
trap 'rc=$?; set +e; rm -f "$HDR" "$HDR".*; (exit $rc); ex_down' EXIT

# hdr <nome>: valor do header (sem diferenciar maiúsculas) da última chamada.
hdr() { grep -i "^$1:" "$HDR" | head -1 | cut -d: -f2- | tr -d ' \r'; }

# call <chave>: preenche EX_HTTP_CODE, EX_HTTP_BODY e o arquivo de headers.
call() {
	EX_HTTP_CODE="$(curl -sS --max-time 30 -D "$HDR" -o /tmp/wwz-ex-rpm-body.$$ -w '%{http_code}' \
		-H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d "$BODY" "$URL" 2>/dev/null)" || EX_HTTP_CODE=000
	EX_HTTP_BODY="$(cat /tmp/wwz-ex-rpm-body.$$ 2>/dev/null || true)"
	rm -f /tmp/wwz-ex-rpm-body.$$
}

# --- 1. Primeira chamada: headers de limite da conta acme (rpm 30).
call wwz-key-acme-a
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "primeira chamada: HTTP $EX_HTTP_CODE"
ex_assert_between "$(hdr x-ratelimit-limit-requests)" 30 30 "x-ratelimit-limit-requests"
first_remaining="$(hdr x-ratelimit-remaining-requests)"
ex_assert_between "$first_remaining" 28 29 "x-ratelimit-remaining-requests na 1a chamada"

# --- 2. Rajada de 44 chamadas alternando as duas chaves da conta acme (45 no total).
# O token bucket tem 30 fichas e repõe 0,5/s: uma rajada de ~1 s admite 30 a 32.
last=$first_remaining
decreasing=1
ok200=1
n429=0
for i in $(seq 1 44); do
	if [ $((i % 2)) -eq 0 ]; then key=wwz-key-acme-a; else key=wwz-key-acme-b; fi
	call "$key"
	case "$EX_HTTP_CODE" in
	200)
		ok200=$((ok200 + 1))
		rem="$(hdr x-ratelimit-remaining-requests)"
		[ "$rem" -le "$last" ] || decreasing=0
		last="$rem"
		;;
	429)
		n429=$((n429 + 1))
		# guarda o último 429 de cada chave (headers e corpo) para conferir abaixo
		cp "$HDR" "$HDR.$key"
		printf '%s' "$EX_HTTP_BODY" >"$HDR.$key.body"
		;;
	*) ex_fail "rajada: HTTP $EX_HTTP_CODE inesperado" ;;
	esac
done
echo "rajada: 200=$ok200 429=$n429 (remaining final nos 200: $last)"
ex_assert_between "$ok200" 30 33 "respostas 200 da conta acme (rpm 30, 2 chaves)"
ex_assert_between "$n429" 12 15 "respostas 429 da conta acme"
ex_assert_between "$decreasing" 1 1 "x-ratelimit-remaining-requests nunca sobe nos 200"
ex_assert_between "$last" 0 1 "remaining ao fim dos 200"

# --- 3. O 429 de cada chave (guardado na rajada, sem esperar mais tempo, porque o
# token bucket repõe uma ficha a cada 2 s): retry-after, x-ratelimit-* e envelope.
for key in wwz-key-acme-a wwz-key-acme-b; do
	[ -s "$HDR.$key" ] || ex_fail "$key não recebeu nenhum 429 na rajada"
	cp "$HDR.$key" "$HDR"
	body="$(cat "$HDR.$key.body")"
	ex_assert_between "$(hdr retry-after)" 1 60 "retry-after (s) de $key"
	ex_assert_between "$(hdr x-ratelimit-limit-requests)" 30 30 "limit-requests no 429 de $key"
	ex_assert_between "$(hdr x-ratelimit-remaining-requests)" 0 0 "remaining-requests no 429 de $key"
	jq -e '.error.code == "rate_limit_exceeded" and .error.type == "requests" and (.error.message | test("Limit 30"))' \
		>/dev/null <<<"$body" || ex_fail "envelope do 429 fora do formato OpenAI: $body"
done
echo "ok  429 rate_limit_exceeded (type requests) com retry-after e x-ratelimit-*"

# --- 4. Outra conta (globex, rpm 100) não é afetada.
call wwz-key-globex
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "globex: HTTP $EX_HTTP_CODE com a conta acme esgotada"
ex_assert_between "$(hdr x-ratelimit-limit-requests)" 100 100 "limit-requests da conta globex"
ex_assert_between "$(hdr x-ratelimit-remaining-requests)" 98 99 "remaining-requests da conta globex"

echo "OK rate-limit-rpm"

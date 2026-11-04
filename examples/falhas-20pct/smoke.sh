#!/usr/bin/env bash
# Smoke do exemplo falhas-20pct (ADR 0016): 500 chamadas ao driver OpenAI e 200
# ao Anthropic; confere a taxa de erro (20 %, margem de 4 sigma), a variedade de
# tipos e o envelope de erro de cada dialeto.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init falhas-20pct 2
ex_up

KEY=wwz-key-demo
URL="http://127.0.0.1:$EX_PORT"

# Com `seed` fixo o sorteio depende da conversa (hash do primeiro prompt do
# usuário, spec 003 e 009): cada chamada leva um prompt diferente, senão todas
# teriam o mesmo destino.
k=0
bad_envelope=0

# call_openai: uma chamada ao dialeto OpenAI. Erros JSON precisam de
# error.message e error.type; o 502 (bad_gateway) é HTML, como o de um proxy.
call_openai() {
	k=$((k + 1))
	ex_post_chat "$EX_PORT" "$KEY" "$(printf '{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"teste %d"}]}' "$k")" || return 0
	case "$EX_HTTP_CODE" in
	200) jq -e '.choices[0].message' >/dev/null <<<"$EX_HTTP_BODY" || bad_envelope=$((bad_envelope + 1)) ;;
	502) case "$EX_HTTP_BODY" in *"<html"* | *"<HTML"*) ;; *) bad_envelope=$((bad_envelope + 1)) ;; esac ;;
	*) jq -e '.error.message and .error.type' >/dev/null <<<"$EX_HTTP_BODY" || bad_envelope=$((bad_envelope + 1)) ;;
	esac
}

# call_anthropic: idem no dialeto Anthropic ({"type":"error","error":{...}}).
call_anthropic() {
	k=$((k + 1))
	local body out
	body="$(printf '{"model":"claude-sonnet-4","max_tokens":16,"messages":[{"role":"user","content":"teste %d"}]}' "$k")"
	out="$(curl -sS --max-time 30 -w $'\n%{http_code}' -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' \
		-H 'Content-Type: application/json' -d "$body" "$URL/v1/messages" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
	case "$EX_HTTP_CODE" in
	200) jq -e '.type == "message"' >/dev/null <<<"$EX_HTTP_BODY" || bad_envelope=$((bad_envelope + 1)) ;;
	502) ;; # corpo HTML
	*) jq -e '.type == "error" and .error.type and .error.message' >/dev/null <<<"$EX_HTTP_BODY" || bad_envelope=$((bad_envelope + 1)) ;;
	esac
}

# --- OpenAI: 500 chamadas. 20 % de 500 = 100, sigma = 8,94, 4 sigma = 36.
N=500
echo "== OpenAI: $N chamadas"
ex_count_codes "$N" call_openai
ok200="$(ex_hist 200)"
errors=$((N - ok200))
ex_assert_between "$errors" 64 136 "erros em $N chamadas (20 % +- 4 sigma)"

kinds=0
for c in 429 500 502 503; do
	[ "$(ex_hist "$c")" -gt 0 ] && kinds=$((kinds + 1))
done
ex_assert_between "$kinds" 3 4 "tipos distintos de erro (429/500/502/503)"
other=$((N - ok200 - $(ex_hist 429) - $(ex_hist 500) - $(ex_hist 502) - $(ex_hist 503)))
ex_assert_between "$other" 0 0 "respostas fora de 200/429/500/502/503"
ex_assert_between "$bad_envelope" 0 0 "envelopes OpenAI fora do formato"

# --- Anthropic: 200 chamadas. 20 % de 200 = 40, sigma = 5,66, 4 sigma = 23.
bad_envelope=0
N=200
echo "== Anthropic: $N chamadas"
ex_count_codes "$N" call_anthropic
errors=$((N - $(ex_hist 200)))
ex_assert_between "$errors" 17 63 "erros em $N chamadas (20 % +- 4 sigma)"
ex_assert_between "$(ex_hist 529)" 1 63 "respostas 529 overloaded_error (Anthropic)"
ex_assert_between "$bad_envelope" 0 0 "envelopes Anthropic fora do formato"

echo "OK falhas-20pct"

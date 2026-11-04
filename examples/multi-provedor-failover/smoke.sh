#!/usr/bin/env bash
# Smoke do exemplo multi-provedor-failover (ADR 0016): chama cada prefixo pelo
# dialeto dele (/openai, /anthropic, /openrouter, /opencode, /gemini com
# generateContent e x-goog-api-key, /azure por deployment com api-key). O
# OpenRouter é o instável (30 % de erro, 4 sigma de margem); os outros cinco
# não falham. Por fim simula um cliente com failover (OpenRouter -> OpenAI):
# nenhuma requisição se perde e a fração que cai para o OpenAI é a taxa de erro
# do instável.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init multi-provedor-failover 13
ex_up

KEY=wwz-key-demo
BASE="http://127.0.0.1:$EX_PORT"
k=0

# post <caminho> <corpo> [headers...]: preenche EX_HTTP_CODE e EX_HTTP_BODY.
post() {
	local path="$1" body="$2" out
	shift 2
	out="$(curl -sS --max-time 30 -w $'\n%{http_code}' -H 'Content-Type: application/json' "$@" -d "$body" "$BASE$path" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
	[ "$EX_HTTP_CODE" != 000 ]
}

# Com `seed` fixo o sorteio depende do primeiro prompt: cada chamada leva um
# prompt diferente ($k).
call_openai() {
	k=$((k + 1))
	post /openai/v1/chat/completions "$(printf '{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"openai %d"}]}' "$k")" -H "Authorization: Bearer $KEY" || true
}
call_anthropic() {
	k=$((k + 1))
	post /anthropic/v1/messages "$(printf '{"model":"claude-sonnet-4","max_tokens":16,"messages":[{"role":"user","content":"anthropic %d"}]}' "$k")" -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' || true
}
call_openrouter() {
	k=$((k + 1))
	post /openrouter/api/v1/chat/completions "$(printf '{"model":"openai/gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"openrouter %d"}]}' "$k")" -H "Authorization: Bearer $KEY" || true
}
call_opencode() {
	k=$((k + 1))
	post /opencode/zen/go/v1/chat/completions "$(printf '{"model":"kimi-k2","max_tokens":16,"messages":[{"role":"user","content":"opencode %d"}]}' "$k")" -H "Authorization: Bearer $KEY" -H "x-opencode-session: ses-exemplo" || true
}
call_gemini() {
	k=$((k + 1))
	post /gemini/v1beta/models/gemini-2.5-flash:generateContent "$(printf '{"contents":[{"role":"user","parts":[{"text":"gemini %d"}]}],"generationConfig":{"maxOutputTokens":16}}' "$k")" -H "x-goog-api-key: $KEY" || true
}
call_azure() {
	k=$((k + 1))
	post "/azure/openai/deployments/gpt-4o-mini/chat/completions?api-version=2024-10-21" "$(printf '{"max_tokens":16,"messages":[{"role":"user","content":"azure %d"}]}' "$k")" -H "api-key: $KEY" || true
}

# --- Cada dialeto, uma chamada, formato do provedor.
call_openai
[ "$EX_HTTP_CODE" = 200 ] && jq -e '.object == "chat.completion" and (.id | startswith("chatcmpl-"))' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "/openai: HTTP $EX_HTTP_CODE $EX_HTTP_BODY"
call_anthropic
[ "$EX_HTTP_CODE" = 200 ] && jq -e '.type == "message" and (.id | startswith("msg_"))' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "/anthropic: HTTP $EX_HTTP_CODE $EX_HTTP_BODY"
call_opencode
[ "$EX_HTTP_CODE" = 200 ] && jq -e '.choices[0].message' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "/opencode: HTTP $EX_HTTP_CODE $EX_HTTP_BODY"
call_gemini
[ "$EX_HTTP_CODE" = 200 ] && jq -e '.candidates[0].content.parts and .usageMetadata.totalTokenCount and .modelVersion' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "/gemini: HTTP $EX_HTTP_CODE $EX_HTTP_BODY"
call_azure
[ "$EX_HTTP_CODE" = 200 ] && jq -e '.object == "chat.completion" and .prompt_filter_results and .choices[0].content_filter_results' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "/azure: HTTP $EX_HTTP_CODE $EX_HTTP_BODY"
# O OpenRouter pode falhar nesta chamada (30 %): tenta até 20 prompts.
or_ok=0
for _ in $(seq 1 20); do
	call_openrouter
	if [ "$EX_HTTP_CODE" = 200 ]; then
		or_ok=1
		break
	fi
done
[ "$or_ok" = 1 ] && jq -e '.choices[0].message and .provider' >/dev/null <<<"$EX_HTTP_BODY" ||
	ex_fail "/openrouter sem 200 com .provider em 20 tentativas: HTTP $EX_HTTP_CODE $EX_HTTP_BODY"
echo "ok  os 6 prefixos respondem no dialeto do provedor"

# --- Falhas por provedor. Instável: 30 % de 300 = 90, sigma = 7,9, 4 sigma = 32.
N=300
echo "== /openrouter (instável): $N chamadas"
ex_count_codes "$N" call_openrouter
errors=$((N - $(ex_hist 200)))
ex_assert_between "$errors" 58 122 "erros do OpenRouter em $N (30 % +- 4 sigma)"
ex_assert_between "$(($(ex_hist 200) + $(ex_hist 429) + $(ex_hist 500) + $(ex_hist 503) + $(ex_hist 529)))" "$N" "$N" "respostas em 200/429/500/503/529"

for drv in openai anthropic opencode gemini azure; do
	echo "== /$drv (estável): 100 chamadas"
	ex_count_codes 100 "call_$drv"
	ex_assert_between "$(ex_hist 200)" 100 100 "respostas 200 de /$drv"
done

# --- Failover: OpenRouter primeiro, OpenAI quando falha.
echo "== failover OpenRouter -> OpenAI: $N requisições"
lost=0 fallbacks=0
for _ in $(seq 1 "$N"); do
	call_openrouter
	if [ "$EX_HTTP_CODE" != 200 ]; then
		fallbacks=$((fallbacks + 1))
		call_openai
		[ "$EX_HTTP_CODE" = 200 ] || lost=$((lost + 1))
	fi
done
echo "fallbacks=$fallbacks perdidas=$lost"
ex_assert_between "$lost" 0 0 "requisições perdidas com failover"
ex_assert_between "$fallbacks" 58 122 "requisições que caíram para o OpenAI em $N (30 % +- 4 sigma)"

echo "OK multi-provedor-failover"

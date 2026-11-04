#!/usr/bin/env bash
# Smoke do exemplo tool-calls-quebradas (ADR 0016): 300 chamadas com `tools` ao
# dialeto OpenAI e 200 ao Anthropic; ~40 % devolvem `arguments` que não parseiam
# como JSON (margem de 4 sigma), as demais parseiam, e uma chamada sem `tools`
# nunca é quebrada.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init tool-calls-quebradas 10
ex_up

KEY=wwz-key-demo
URL="http://127.0.0.1:$EX_PORT"
k=0
bad=0 good=0 other=0

# Com `seed` fixo o sorteio depende do primeiro prompt: cada chamada leva um
# prompt diferente.
call_openai() {
	k=$((k + 1))
	ex_post_chat "$EX_PORT" "$KEY" "$(printf '{"model":"gpt-4o-mini","max_tokens":64,"messages":[{"role":"user","content":"leia o arquivo %d"}],"tool_choice":"required","tools":[{"type":"function","function":{"name":"read_file","description":"le um arquivo","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]}' "$k")" || { other=$((other + 1)); return 0; }
	[ "$EX_HTTP_CODE" = 200 ] || { other=$((other + 1)); return 0; }
	if jq -e '.choices[0].message.tool_calls[0].function.arguments | fromjson | type == "object"' >/dev/null 2>&1 <<<"$EX_HTTP_BODY"; then
		good=$((good + 1))
	elif jq -e '.choices[0].message.tool_calls[0].function.arguments | type == "string"' >/dev/null 2>&1 <<<"$EX_HTTP_BODY"; then
		bad=$((bad + 1))
	else
		other=$((other + 1))
	fi
}

# Anthropic: no real o `input` do não-stream é sempre objeto; o JSON quebrado só
# existe no stream, nos `partial_json` do primeiro bloco tool_use, que o cliente
# concatena e parseia no fim.
call_anthropic() {
	k=$((k + 1))
	local out args
	out="$(curl -sS --max-time 30 -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' \
		-H 'Content-Type: application/json' \
		-d "$(printf '{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"leia o arquivo %d"}],"tool_choice":{"type":"any"},"tools":[{"name":"read_file","description":"le um arquivo","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}' "$k")" \
		"$URL/v1/messages" 2>/dev/null)" || { other=$((other + 1)); return 0; }
	args="$(jq -R -r 'select(startswith("data: ")) | .[6:] | fromjson? | select(.type == "content_block_delta" and .index == 0) | .delta.partial_json' <<<"$out" | tr -d '\n')"
	if [ -z "$args" ]; then
		other=$((other + 1))
	elif jq -e 'type == "object"' >/dev/null 2>&1 <<<"$args"; then
		good=$((good + 1))
	else
		bad=$((bad + 1))
	fi
}

# --- OpenAI: 300 chamadas. 40 % de 300 = 120, sigma = 8,5, 4 sigma = 34.
N=300
echo "== OpenAI: $N chamadas com tools"
for _ in $(seq 1 "$N"); do call_openai; done
echo "arguments inválidos=$bad válidos=$good outros=$other"
ex_assert_between "$other" 0 0 "respostas sem tool call ou com erro"
ex_assert_between "$bad" 86 154 "arguments que não parseiam em $N (40 % +- 4 sigma)"
ex_assert_between "$good" $((N - 154)) $((N - 86)) "arguments que parseiam em $N"

# --- Anthropic: 200 chamadas. 40 % de 200 = 80, sigma = 6,9, 4 sigma = 28.
bad=0 good=0 other=0
N=200
echo "== Anthropic (stream): $N chamadas com tools"
for _ in $(seq 1 "$N"); do call_anthropic; done
echo "input inválido=$bad válido=$good outros=$other"
ex_assert_between "$other" 0 0 "streams sem argumentos de tool_use"
ex_assert_between "$bad" 52 108 "tool_use com JSON quebrado em $N (40 % +- 4 sigma)"

# --- Sem `tools` no pedido não há tool call para quebrar.
for i in 1 2 3 4 5; do
	ex_post_chat "$EX_PORT" "$KEY" "$(printf '{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"sem tools %d"}]}' "$i")"
	[ "$EX_HTTP_CODE" = 200 ] || ex_fail "sem tools: HTTP $EX_HTTP_CODE"
	jq -e '.choices[0].message.content | type == "string"' >/dev/null <<<"$EX_HTTP_BODY" || ex_fail "sem tools: sem content"
done
echo "ok  pedidos sem tools seguem normais"

echo "OK tool-calls-quebradas"

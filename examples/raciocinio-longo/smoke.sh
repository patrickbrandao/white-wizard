#!/usr/bin/env bash
# Smoke de raciocinio-longo (spec 019, ADR 0016, spec 007): o pedido com
# raciocínio traz tokens de raciocínio acima de um piso e demora mais para o
# primeiro token da resposta que o pedido sem raciocínio.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init raciocinio-longo 12
ex_up

BASE="http://127.0.0.1:$EX_PORT"
TMP="$(mktemp -d)"
trap 'rc=$?; rm -rf "$TMP"; (exit $rc); ex_down' EXIT
N=6                # pares (com e sem raciocínio) por dialeto
FLOOR=250          # piso de tokens de raciocínio: o YAML sorteia 300 a 400 (effort medium)
MIN_DELTA_MS=1500  # o raciocínio custa 300 x 8 ms = 2,4 s no mínimo; o resto é ruído de ~100 ms

# req <arquivo-base> <dialeto> <raciocínio:0|1> <stream:true|false>
# Grava "<ttfb_s> <total_s>" em <base>.res e o corpo em <base>.body.
req() {
	local base="$1" dialect="$2" think="$3" stream="$4" body path
	if [ "$dialect" = openai ]; then
		path=/v1/chat/completions
		body='{"model":"wwz-reasoner","stream":'$stream
		[ "$stream" = true ] && body="$body"',"stream_options":{"include_usage":true}'
		[ "$think" = 1 ] && body="$body"',"reasoning_effort":"medium"'
		body="$body"',"messages":[{"role":"user","content":"Resolva com cuidado: quanto é 17 vezes 23?"}]}'
		curl -sS --max-time 60 -o "$base.body" -w '%{time_starttransfer} %{time_total}\n' \
			-H 'Authorization: Bearer wwz-key-01' -H 'Content-Type: application/json' \
			-d "$body" "$BASE$path" >"$base.res" 2>/dev/null || echo "0 0" >"$base.res"
	else
		path=/v1/messages
		body='{"model":"wwz-reasoner","max_tokens":4000,"stream":'$stream
		[ "$think" = 1 ] && body="$body"',"thinking":{"type":"enabled","budget_tokens":2000}'
		body="$body"',"messages":[{"role":"user","content":"Resolva com cuidado: quanto é 17 vezes 23?"}]}'
		curl -sS --max-time 60 -o "$base.body" -w '%{time_starttransfer} %{time_total}\n' \
			-H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01' \
			-H 'Content-Type: application/json' \
			-d "$body" "$BASE$path" >"$base.res" 2>/dev/null || echo "0 0" >"$base.res"
	fi
}

# Todas as chamadas em paralelo (cada uma leva de 0,5 a 4 s).
for ((i = 1; i <= N; i++)); do
	req "$TMP/oa-plain-$i" openai 0 true &
	req "$TMP/oa-think-$i" openai 1 true &
	req "$TMP/an-plain-$i" anthropic 0 true &
	req "$TMP/an-think-$i" anthropic 1 true &
	wait
done
# Corpos sem stream, para os contadores de tokens.
req "$TMP/oa-json-plain" openai 0 false &
req "$TMP/oa-json-think" openai 1 false &
req "$TMP/an-json-plain" anthropic 0 false &
req "$TMP/an-json-think" anthropic 1 false &
wait

# mean_ms <prefixo> <coluna 1=ttfb|2=total>: média em ms dos $N arquivos.
mean_ms() {
	local sum=0 i v
	for ((i = 1; i <= N; i++)); do
		v="$(awk -v c="$2" '{ printf "%d", $c * 1000 }' "$TMP/$1-$i.res")"
		sum=$((sum + v))
	done
	echo $((sum / N))
}

# OpenAI (reasoning_field: hidden): o raciocínio não é serializado, só contado e
# vira atraso até o primeiro byte do stream.
oa_plain="$(mean_ms oa-plain 1)"
oa_think="$(mean_ms oa-think 1)"
oa_tokens="$(jq -r '.usage.completion_tokens_details.reasoning_tokens' "$TMP/oa-json-think.body")"
oa_tokens0="$(jq -r '.usage.completion_tokens_details.reasoning_tokens' "$TMP/oa-json-plain.body")"
oa_visible="$(jq -r '.choices[0].message | has("reasoning_content") or has("reasoning")' "$TMP/oa-json-think.body")"
echo "openai: TTFT sem raciocínio ${oa_plain} ms, com raciocínio ${oa_think} ms; reasoning_tokens $oa_tokens (sem: $oa_tokens0)"
ex_assert_between "$oa_tokens" "$FLOOR" 1000 "openai reasoning_tokens com effort medium"
ex_assert_between "$oa_tokens0" 0 0 "openai reasoning_tokens sem effort"
ex_assert_between "$([ "$oa_visible" = false ] && echo 1 || echo 0)" 1 1 "openai não serializa o texto do raciocínio (hidden)"
ex_assert_between "$((oa_think - oa_plain))" "$MIN_DELTA_MS" 20000 "openai: atraso do raciocínio no TTFT (ms)"

# Anthropic (formato fixo): bloco thinking antes do texto; no stream, o primeiro
# byte (message_start) sai logo, então o atraso até o texto aparece no tempo total.
an_plain="$(mean_ms an-plain 2)"
an_think="$(mean_ms an-think 2)"
an_tokens="$(jq -r '.usage.output_tokens_details.thinking_tokens' "$TMP/an-json-think.body")"
an_blocks="$(jq -r '[.content[].type] | join(",")' "$TMP/an-json-think.body")"
an_plain_blocks="$(jq -r '[.content[].type] | join(",")' "$TMP/an-json-plain.body")"
an_deltas="$(grep -c '"type":"thinking_delta"' "$TMP/an-think-1.body" || true)"
echo "anthropic: tempo total sem raciocínio ${an_plain} ms, com ${an_think} ms; thinking_tokens $an_tokens; blocos '$an_blocks' (sem: '$an_plain_blocks'); thinking_delta no stream $an_deltas"
[ "$an_blocks" = "thinking,text" ] || ex_fail "esperava blocos thinking,text; veio '$an_blocks'"
[ "$an_plain_blocks" = "text" ] || ex_fail "sem thinking esperava só text; veio '$an_plain_blocks'"
ex_assert_between "$an_tokens" "$FLOOR" 1000 "anthropic thinking_tokens"
ex_assert_between "$an_deltas" "$FLOOR" 1000 "anthropic thinking_delta no stream"
ex_assert_between "$((an_think - an_plain))" "$MIN_DELTA_MS" 20000 "anthropic: atraso do raciocínio até o texto (ms)"
echo "OK raciocinio-longo"

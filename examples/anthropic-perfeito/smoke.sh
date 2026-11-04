#!/usr/bin/env bash
# Smoke de anthropic-perfeito (spec 019, ADR 0016): 50 chamadas a /v1/messages,
# metade com stream, todas 200, com TTFT medido e a Messages API no corpo.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init anthropic-perfeito 1
ex_up

URL="http://127.0.0.1:$EX_PORT/v1/messages"
TMP="$(mktemp -d)"
trap 'rc=$?; rm -rf "$TMP"; (exit $rc); ex_down' EXIT
N_EACH=25 # 25 sem stream + 25 com stream = 50

# one <modo> <i>: grava "<código> <ttft_s>" em $TMP/<modo>-<i>.res e o corpo em .body.
one() {
	local mode="$1" i="$2" stream=false body
	[ "$mode" = stream ] && stream=true
	body="{\"model\":\"claude-sonnet-4-5\",\"max_tokens\":64,\"stream\":$stream,\"messages\":[{\"role\":\"user\",\"content\":\"Diga oi ($i)\"}]}"
	curl -sS --max-time 60 -o "$TMP/$mode-$i.body" \
		-w '%{http_code} %{time_starttransfer}\n' \
		-H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01' \
		-H 'Content-Type: application/json' \
		-d "$body" "$URL" >"$TMP/$mode-$i.res" 2>/dev/null || echo "000 0" >"$TMP/$mode-$i.res"
}

# Em lotes de 10 conexões paralelas (cada chamada leva ~1 s).
batch=0
for mode in json stream; do
	for ((i = 1; i <= N_EACH; i++)); do
		one "$mode" "$i" &
		batch=$((batch + 1))
		if [ $((batch % 10)) -eq 0 ]; then wait; fi
	done
done
wait

ok200=0 bad_shape=0 ttft_sum=0 ttft_zero=0
for mode in json stream; do
	for ((i = 1; i <= N_EACH; i++)); do
		read -r code ttft <"$TMP/$mode-$i.res"
		[ "$code" = 200 ] && ok200=$((ok200 + 1))
		ms="$(awk -v t="$ttft" 'BEGIN { printf "%d", t * 1000 }')"
		# sem stream o primeiro byte é a resposta inteira; o TTFT de verdade é o do stream
		[ "$mode" = stream ] && ttft_sum=$((ttft_sum + ms))
		[ "$ms" -gt 0 ] || ttft_zero=$((ttft_zero + 1))
		f="$TMP/$mode-$i.body"
		if [ "$mode" = json ]; then
			# corpo: id msg_*, type message, bloco de texto, usage coerente
			jq -e '(.id | test("^msg_[A-Za-z0-9]+$")) and .type == "message" and .role == "assistant"
				and (.content[0].type == "text") and (.content[0].text | length > 0)
				and .stop_reason == "end_turn"
				and (.usage.input_tokens > 0) and (.usage.output_tokens > 0)' \
				"$f" >/dev/null 2>&1 || bad_shape=$((bad_shape + 1))
		else
			# SSE: message_start, content_block_start, ping/delta..., content_block_stop,
			# message_delta, message_stop, nesta ordem e sem evento de erro.
			seq="$(grep '^event: ' "$f" | sed 's/^event: //' | grep -v -e '^ping$' -e '^content_block_delta$' | tr '\n' ' ')"
			[ "$seq" = "message_start content_block_start content_block_stop message_delta message_stop " ] &&
				grep -q '^event: content_block_delta' "$f" &&
				grep '^data: ' "$f" | sed 's/^data: //' | jq -e -s \
					'(.[0].message.id | test("^msg_")) and (.[-1].type == "message_stop")
					and any(.[]; .type == "message_delta" and .usage.output_tokens > 0)' >/dev/null 2>&1 ||
				bad_shape=$((bad_shape + 1))
		fi
	done
done

total=$((N_EACH * 2))
mean_ttft=$((ttft_sum / N_EACH))
echo "respostas 200: $ok200/$total, TTFT médio (stream): ${mean_ttft} ms, formato inválido: $bad_shape"

# errors.rate: 0 => nenhuma exceção possível, nem pela cauda do sorteio.
ex_assert_between "$ok200" "$total" "$total" "respostas 200"
ex_assert_between "$bad_shape" 0 0 "respostas com formato fora do padrão Anthropic"
ex_assert_between "$ttft_zero" 0 0 "chamadas com TTFT 0"
# TTFT ~ 400 ms (normal, desvio 90 ms) + fila lognormal (mediana 20 ms, p99 200 ms):
# a média de 25 amostras tem desvio ~22 ms, então [200, 800] fica a mais de 4 sigma.
ex_assert_between "$mean_ttft" 200 800 "TTFT médio do stream (ms)"
echo "OK anthropic-perfeito"

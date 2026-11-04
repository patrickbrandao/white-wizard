#!/usr/bin/env bash
# Smoke de openai-perfeito (spec 019, ADR 0016): 50 chamadas de chat, metade com
# stream, todas 200, com TTFT medido e o formato da OpenAI no corpo.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init openai-perfeito 0
ex_up

URL="http://127.0.0.1:$EX_PORT/v1/chat/completions"
TMP="$(mktemp -d)"
trap 'rc=$?; rm -rf "$TMP"; (exit $rc); ex_down' EXIT
N_EACH=25 # 25 sem stream + 25 com stream = 50

# one <modo> <i>: grava "<código> <ttft_ms>" em $TMP/<modo>-<i>.res e o corpo em .body.
one() {
	local mode="$1" i="$2" extra='"stream":false,' body
	# stream_options só é aceito com stream: true (como na OpenAI real).
	[ "$mode" = stream ] && extra='"stream":true,"stream_options":{"include_usage":true},'
	body="{\"model\":\"gpt-4o-mini\",$extra\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"Diga oi ($i)\"}]}"
	curl -sS --max-time 60 -o "$TMP/$mode-$i.body" \
		-w '%{http_code} %{time_starttransfer}\n' \
		-H 'Authorization: Bearer wwz-key-01' -H 'Content-Type: application/json' \
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
		if [ "$mode" = json ]; then
			# corpo: id chatcmpl-*, object, usage coerente
			jq -e '(.id | test("^chatcmpl-[A-Za-z0-9]+$")) and .object == "chat.completion"
				and (.choices[0].message.content | length > 0)
				and (.usage.prompt_tokens > 0) and (.usage.completion_tokens > 0)
				and (.usage.total_tokens == .usage.prompt_tokens + .usage.completion_tokens)' \
				"$TMP/$mode-$i.body" >/dev/null 2>&1 || bad_shape=$((bad_shape + 1))
		else
			# SSE: chunks com o mesmo id chatcmpl-*, usage no último chunk e [DONE]
			f="$TMP/$mode-$i.body"
			if ! tail -n 2 "$f" | grep -q '^data: \[DONE\]' ||
				! grep '^data: {' "$f" | sed 's/^data: //' | jq -e -s \
					'length > 1 and all(.[]; .id | test("^chatcmpl-")) and (map(.id) | unique | length == 1)
					and all(.[]; .object == "chat.completion.chunk")
					and (.[-1].usage.total_tokens > 0)' >/dev/null 2>&1; then
				bad_shape=$((bad_shape + 1))
			fi
		fi
	done
done

total=$((N_EACH * 2))
mean_ttft=$((ttft_sum / N_EACH))
echo "respostas 200: $ok200/$total, TTFT médio (stream): ${mean_ttft} ms, formato inválido: $bad_shape"

# errors.rate: 0 => nenhuma exceção possível, nem pela cauda do sorteio.
ex_assert_between "$ok200" "$total" "$total" "respostas 200"
ex_assert_between "$bad_shape" 0 0 "respostas com formato fora do padrão OpenAI"
ex_assert_between "$ttft_zero" 0 0 "chamadas com TTFT 0"
# TTFT ~ 350 ms (normal, desvio 80 ms) + fila lognormal (mediana 20 ms, p99 200 ms):
# a média de 25 amostras tem desvio ~20 ms, então [200, 700] fica a mais de 4 sigma.
ex_assert_between "$mean_ttft" 200 700 "TTFT médio do stream (ms)"
echo "OK openai-perfeito"

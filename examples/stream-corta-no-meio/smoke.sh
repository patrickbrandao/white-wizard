#!/usr/bin/env bash
# Smoke do exemplo stream-corta-no-meio (ADR 0016): 200 streams OpenAI e 100
# Anthropic; confere a fração dos que terminam sem o evento final (`[DONE]` /
# `message_stop`) e que os cortados trazem bytes parciais válidos.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init stream-corta-no-meio 9
ex_up

KEY=wwz-key-demo
URL="http://127.0.0.1:$EX_PORT"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/wwz-ex-stream.XXXXXX")"
trap '_rc=$?; rm -rf "$TMP"; (exit $_rc); ex_down' EXIT

# Com `seed` fixo o sorteio depende da conversa (hash do primeiro prompt do
# usuário): cada stream leva um prompt diferente.
k=0
complete=0
cut=0
curl_fail=0    # curl terminou com erro (RST ou fim de chunked ausente)
bad_frames=0   # frames `data:` completos de streams cortados que não são JSON
empty_cut=0    # streams cortados sem nenhum frame útil

# stream_one <openai|anthropic>: abre um stream e classifica.
stream_one() {
	local kind="$1" body out="$TMP/s.txt" rc=0 final content frames
	k=$((k + 1))
	if [ "$kind" = openai ]; then
		body="$(printf '{"model":"gpt-4o-mini","stream":true,"max_tokens":40,"messages":[{"role":"user","content":"teste %d"}]}' "$k")"
		curl -sS -N --max-time 20 -o "$out" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
			-d "$body" "$URL/v1/chat/completions" 2>/dev/null || rc=$?
		grep -q '^data: \[DONE\]' "$out" && final=1 || final=0
	else
		body="$(printf '{"model":"claude-sonnet-4","stream":true,"max_tokens":40,"messages":[{"role":"user","content":"teste %d"}]}' "$k")"
		curl -sS -N --max-time 20 -o "$out" -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' \
			-H 'Content-Type: application/json' -d "$body" "$URL/v1/messages" 2>/dev/null || rc=$?
		grep -q '^event: message_stop' "$out" && final=1 || final=0
	fi
	if [ "$final" -eq 1 ]; then
		complete=$((complete + 1))
		return 0
	fi
	cut=$((cut + 1))
	[ "$rc" -ne 0 ] && curl_fail=$((curl_fail + 1))
	# Bytes parciais válidos: descarta a última linha se o corte caiu no meio
	# dela e exige que todo frame `data:` completo seja JSON.
	content="$(cat "$out"; printf x)"
	content="${content%x}"
	case "$content" in *$'\n') ;; *) content="${content%$'\n'*}" ;; esac
	frames="$(printf '%s\n' "$content" | grep '^data: ' | sed 's/^data: //' || true)"
	if [ -z "$frames" ]; then
		empty_cut=$((empty_cut + 1))
		return 0
	fi
	printf '%s\n' "$frames" | jq -e . >/dev/null 2>&1 || bad_frames=$((bad_frames + 1))
}

reset_counts() { complete=0 cut=0 curl_fail=0 bad_frames=0 empty_cut=0; }

# --- OpenAI: 200 streams, 30 % cortados: 60 +- 4 sigma (6,48 -> 26) = 34..86.
echo "== OpenAI: 200 streams"
reset_counts
for ((i = 0; i < 200; i++)); do stream_one openai; done
echo "completos=$complete cortados=$cut (curl com erro: $curl_fail)"
ex_assert_between "$cut" 34 86 "streams OpenAI sem [DONE] (30 % +- 4 sigma)"
ex_assert_between "$bad_frames" 0 0 "streams cortados com frame data: inválido"
ex_assert_between "$empty_cut" 0 0 "streams cortados sem nenhum frame (sem bytes parciais)"
# stream_truncated fecha limpo; stream_reset termina com erro no curl: os dois ocorrem.
ex_assert_between "$curl_fail" 1 "$cut" "cortes que o curl viu como erro (stream_reset)"
ex_assert_between "$((cut - curl_fail))" 1 "$cut" "cortes limpos (stream_truncated)"

# --- Anthropic: 100 streams, 30 % cortados: 30 +- 4 sigma (4,58 -> 18) = 12..48.
echo "== Anthropic: 100 streams"
reset_counts
for ((i = 0; i < 100; i++)); do stream_one anthropic; done
echo "completos=$complete cortados=$cut (curl com erro: $curl_fail)"
ex_assert_between "$cut" 12 48 "streams Anthropic sem message_stop (30 % +- 4 sigma)"
ex_assert_between "$bad_frames" 0 0 "streams cortados com frame data: inválido"
ex_assert_between "$empty_cut" 0 0 "streams cortados sem nenhum frame (sem bytes parciais)"

echo "OK stream-corta-no-meio"

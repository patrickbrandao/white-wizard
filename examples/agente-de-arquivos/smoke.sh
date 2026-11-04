#!/usr/bin/env bash
# Smoke do exemplo agente-de-arquivos (ADR 0016): percorre o cenário embutido
# `file-agent-flow` como um agente de verdade, nos dialetos OpenAI e Anthropic:
# a cada turno executa a tool pedida (resultado "ok"), devolve o resultado e
# confere a ordem read_file -> write_file -> edit_file -> delete_file -> resposta
# final (finish_reason stop), os passos em x-wwz-step e os ids de tool call.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init agente-de-arquivos 11
ex_up

KEY=wwz-key-demo
URL="http://127.0.0.1:$EX_PORT"
HDR="$(mktemp)"
trap 'rc=$?; set +e; rm -f "$HDR"; (exit $rc); ex_down' EXIT

WANT_TOOLS="read_file write_file edit_file delete_file"
WANT_STEPS="read write edit delete done"

# Tools oferecidas ao modelo (o cenário só casa se as quatro estão no pedido).
OPENAI_TOOLS='[
 {"type":"function","function":{"name":"read_file","description":"le","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},
 {"type":"function","function":{"name":"write_file","description":"grava","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}}},
 {"type":"function","function":{"name":"edit_file","description":"edita","parameters":{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}}},
 {"type":"function","function":{"name":"delete_file","description":"apaga","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]'
ANTHROPIC_TOOLS='[
 {"name":"read_file","description":"le","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
 {"name":"write_file","description":"grava","input_schema":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}},
 {"name":"edit_file","description":"edita","input_schema":{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}},
 {"name":"delete_file","description":"apaga","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]'

step_header() { grep -i '^x-wwz-step:' "$HDR" | head -1 | cut -d: -f2- | tr -d ' \r'; }

# --- OpenAI: histórico cresce a cada turno (assistant com tool_calls + tool).
echo "== OpenAI: agente de arquivos"
msgs='[{"role":"user","content":"Crie, edite e apague o arquivo de notas."}]'
tools_seen="" steps_seen="" finish=""
for turn in $(seq 1 8); do
	body="$(jq -nc --argjson m "$msgs" --argjson t "$OPENAI_TOOLS" '{model:"gpt-4o-mini",max_tokens:512,messages:$m,tools:$t}')"
	resp="$(curl -sS --max-time 30 -D "$HDR" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "$body" "$URL/v1/chat/completions")" ||
		ex_fail "turno $turn: curl falhou"
	steps_seen="$steps_seen $(step_header)"
	finish="$(jq -r '.choices[0].finish_reason' <<<"$resp")"
	[ "$finish" != null ] || ex_fail "turno $turn: sem finish_reason: $resp"
	if [ "$finish" = stop ]; then
		jq -e '.choices[0].message.content | type == "string" and length > 0' >/dev/null <<<"$resp" ||
			ex_fail "resposta final sem texto: $resp"
		break
	fi
	[ "$finish" = tool_calls ] || ex_fail "turno $turn: finish_reason inesperado $finish"
	n="$(jq '.choices[0].message.tool_calls | length' <<<"$resp")"
	[ "$n" = 1 ] || ex_fail "turno $turn: esperava 1 tool call, vieram $n"
	name="$(jq -r '.choices[0].message.tool_calls[0].function.name' <<<"$resp")"
	jq -e '.choices[0].message.tool_calls[0].function.arguments | fromjson | type == "object"' >/dev/null <<<"$resp" ||
		ex_fail "turno $turn: arguments não parseiam: $resp"
	tools_seen="$tools_seen $name"
	id="$(jq -r '.choices[0].message.tool_calls[0].id' <<<"$resp")"
	msgs="$(jq -c --argjson a "$(jq -c '.choices[0].message | {role, content, tool_calls}' <<<"$resp")" --arg id "$id" \
		'. + [$a, {role:"tool", tool_call_id:$id, content:"ok"}]' <<<"$msgs")"
done
[ "$finish" = stop ] || ex_fail "o agente não terminou em 8 turnos"
tools_seen="${tools_seen# }"
steps_seen="${steps_seen# }"
[ "$tools_seen" = "$WANT_TOOLS" ] || ex_fail "tools na ordem errada: '$tools_seen' (esperava '$WANT_TOOLS')"
[ "$steps_seen" = "$WANT_STEPS" ] || ex_fail "x-wwz-step: '$steps_seen' (esperava '$WANT_STEPS')"
echo "ok  OpenAI: tools [$tools_seen], passos [$steps_seen], terminou em stop"

# --- Anthropic: mesmo fluxo com tool_use / tool_result.
echo "== Anthropic: agente de arquivos"
msgs='[{"role":"user","content":"Crie, edite e apague o arquivo de notas."}]'
tools_seen="" steps_seen="" finish=""
for turn in $(seq 1 8); do
	body="$(jq -nc --argjson m "$msgs" --argjson t "$ANTHROPIC_TOOLS" '{model:"claude-sonnet-4",max_tokens:512,messages:$m,tools:$t}')"
	resp="$(curl -sS --max-time 30 -D "$HDR" -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' \
		-H 'Content-Type: application/json' -d "$body" "$URL/v1/messages")" || ex_fail "turno $turn: curl falhou"
	steps_seen="$steps_seen $(step_header)"
	finish="$(jq -r '.stop_reason' <<<"$resp")"
	if [ "$finish" = end_turn ]; then
		jq -e '[.content[] | select(.type == "text")] | length > 0' >/dev/null <<<"$resp" || ex_fail "resposta final sem texto: $resp"
		break
	fi
	[ "$finish" = tool_use ] || ex_fail "turno $turn: stop_reason inesperado $finish: $resp"
	n="$(jq '[.content[] | select(.type == "tool_use")] | length' <<<"$resp")"
	[ "$n" = 1 ] || ex_fail "turno $turn: esperava 1 tool_use, vieram $n"
	name="$(jq -r '[.content[] | select(.type == "tool_use")][0].name' <<<"$resp")"
	tools_seen="$tools_seen $name"
	id="$(jq -r '[.content[] | select(.type == "tool_use")][0].id' <<<"$resp")"
	msgs="$(jq -c --argjson a "$(jq -c '{role:"assistant", content}' <<<"$resp")" --arg id "$id" \
		'. + [$a, {role:"user", content:[{type:"tool_result", tool_use_id:$id, content:"ok"}]}]' <<<"$msgs")"
done
[ "$finish" = end_turn ] || ex_fail "o agente não terminou em 8 turnos"
tools_seen="${tools_seen# }"
steps_seen="${steps_seen# }"
[ "$tools_seen" = "$WANT_TOOLS" ] || ex_fail "tools na ordem errada: '$tools_seen' (esperava '$WANT_TOOLS')"
[ "$steps_seen" = "$WANT_STEPS" ] || ex_fail "x-wwz-step: '$steps_seen' (esperava '$WANT_STEPS')"
echo "ok  Anthropic: tools [$tools_seen], passos [$steps_seen], terminou em end_turn"

echo "OK agente-de-arquivos"

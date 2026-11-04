#!/usr/bin/env bash
# Smoke do exemplo creditos-openrouter-esgotando (ADR 0016): o saldo da conta
# "pobre" (US$ 3, chamadas de ~US$ 0,50) decresce monotonicamente a cada chamada
# (lido na API admin) e a chamada seguinte ao zero devolve 402 no envelope do
# OpenRouter; a segunda chave da conta divide o saldo; a conta "rica" não é
# afetada; recarregar o saldo pela admin devolve o 200.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init creditos-openrouter-esgotando 7
export EX_ADMIN_PORT="$((EX_PORT + 1000))"
ex_up

URL="http://127.0.0.1:$EX_PORT/api/v1/chat/completions"
ADMIN="http://127.0.0.1:$EX_ADMIN_PORT"
TOKEN=wwz-ex-admin-token
BODY='{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}'

# call <chave>: preenche EX_HTTP_CODE e EX_HTTP_BODY.
call() { ex_post_chat "$EX_PORT" "$1" "$BODY" /api/v1/chat/completions || true; }

# credits <conta>: saldo atual em US$ (campo credits_usd da entidade, API admin).
credits() {
	curl -fsS -H "Authorization: Bearer $TOKEN" "$ADMIN/admin/entities/account/$1" | jq -r '.credits_usd'
}

# lt <a> <b>: a < b (decimais).
lt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a < b) }'; }

# --- 1. O saldo decresce monotonicamente até zerar.
echo "== conta pobre: saldo inicial US\$ 3,00"
prev=3
n200=0
for i in $(seq 1 12); do
	if [ $((i % 2)) -eq 0 ]; then key=wwz-key-or-a; else key=wwz-key-or-b; fi # as duas chaves, mesmo saldo
	call "$key"
	if [ "$EX_HTTP_CODE" = 402 ]; then break; fi
	[ "$EX_HTTP_CODE" = 200 ] || ex_fail "chamada $i: HTTP $EX_HTTP_CODE inesperado: $EX_HTTP_BODY"
	n200=$((n200 + 1))
	cur="$(credits pobre)"
	lt "$cur" "$prev" || ex_fail "saldo não decresceu na chamada $i: $prev -> $cur"
	printf 'chamada %2d: 200, saldo US$ %.4f\n' "$i" "$cur"
	prev="$cur"
done
ex_assert_between "$n200" 5 7 "respostas 200 até o saldo zerar (US\$ 3 / ~US\$ 0,50)"
lt "$prev" 0.001 || ex_fail "o saldo deveria estar em ~0 ao fim dos 200 (veio $prev)"
echo "ok  saldo decresceu a cada chamada até ~0"

# --- 2. A chamada seguinte ao zero: 402 no envelope do OpenRouter, nas duas chaves.
for key in wwz-key-or-a wwz-key-or-b; do
	call "$key"
	[ "$EX_HTTP_CODE" = 402 ] || ex_fail "$key com saldo zerado: esperado 402, veio $EX_HTTP_CODE"
	jq -e '.error.code == 402 and .error.metadata.limit_source == "openrouter_credits" and .error.metadata.reason == "insufficient_credits" and (.error.message | test("Insufficient credits"))' \
		>/dev/null <<<"$EX_HTTP_BODY" || ex_fail "402 fora do formato OpenRouter: $EX_HTTP_BODY"
done
echo "ok  402 payment required (openrouter_credits / insufficient_credits) nas duas chaves da conta"

# --- 3. A conta rica não é afetada.
call wwz-key-or-rica
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "conta rica: HTTP $EX_HTTP_CODE com a conta pobre zerada"
rica="$(credits rica)"
lt "$rica" 1000 || ex_fail "saldo da conta rica deveria ter caído abaixo de 1000 (veio $rica)"
echo "ok  conta rica segue em 200 (saldo US\$ $rica)"

# --- 4. Recarga pela admin devolve o acesso.
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
	-d '{"usd":2}' "$ADMIN/admin/entities/account/pobre/credits" >/dev/null || ex_fail "recarga pela admin falhou"
call wwz-key-or-a
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "depois da recarga de US\$ 2: HTTP $EX_HTTP_CODE"
echo "ok  recarga de US\$ 2 pela admin devolve o 200"

echo "OK creditos-openrouter-esgotando"

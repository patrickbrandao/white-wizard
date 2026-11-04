#!/usr/bin/env bash
# Smoke do exemplo ban-por-abuso (ADR 0016): a chave "abusiva" (rpm 6) insiste
# depois do 429; no quinto 429 em 1 minuto a política de abuso a bloqueia e ela
# passa a receber 403 account_suspended (sem retry-after); GET /admin/entities
# mostra o bloqueio; a chave bem-comportada não é afetada; o desbloqueio pela
# admin (mais o perdão dos strikes) restaura o 200.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init ban-por-abuso 8
export EX_ADMIN_PORT="$((EX_PORT + 1000))"
ex_up

ADMIN="http://127.0.0.1:$EX_ADMIN_PORT"
TOKEN=wwz-ex-admin-token
BODY='{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}'
HDR="$(mktemp)"
trap 'rc=$?; set +e; rm -f "$HDR"; (exit $rc); ex_down' EXIT

# call <chave>: preenche EX_HTTP_CODE, EX_HTTP_BODY e o arquivo de headers.
call() {
	local out
	out="$(curl -sS --max-time 30 -D "$HDR" -w $'\n%{http_code}' \
		-H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d "$BODY" \
		"http://127.0.0.1:$EX_PORT/v1/chat/completions" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
}

# admin <método> <caminho> [corpo]: chama a API admin e imprime o corpo.
admin() {
	curl -fsS -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
		${3:+-d "$3"} "$ADMIN$2"
}

# --- 1. A chave abusiva insiste: 200 (rajada de 6), 429 (5 strikes), então 403.
echo "== chave abusiva (rpm 6): insiste depois do 429"
n200=0 n429=0 n403=0
for i in $(seq 1 25); do
	call wwz-key-abusiva
	case "$EX_HTTP_CODE" in
	200) n200=$((n200 + 1)) ;;
	429) n429=$((n429 + 1)) ;;
	403)
		n403=$((n403 + 1))
		if [ "$n403" -eq 1 ]; then
			jq -e '.error.code == "account_suspended"' >/dev/null <<<"$EX_HTTP_BODY" ||
				ex_fail "403 fora do formato OpenAI (account_suspended): $EX_HTTP_BODY"
			if grep -qi '^retry-after:' "$HDR"; then ex_fail "403 de bloqueio não deve trazer retry-after"; fi
		fi
		[ "$n403" -ge 5 ] && break
		;;
	*) ex_fail "HTTP $EX_HTTP_CODE inesperado: $EX_HTTP_BODY" ;;
	esac
done
echo "abusiva: 200=$n200 429=$n429 403=$n403"
ex_assert_between "$n200" 6 7 "respostas 200 antes do primeiro 429 (rpm 6)"
ex_assert_between "$n429" 5 6 "respostas 429 até o bloqueio (strike_429.count = 5)"
ex_assert_between "$n403" 5 5 "respostas 403 account_suspended depois do bloqueio"
echo "ok  403 account_suspended sem retry-after"

# --- 2. A admin mostra o bloqueio.
state="$(admin GET /admin/entities/key/abusiva)"
jq -e '.status == "suspended" and (.until | type == "string") and .by == "abuse-ladder" and .strikes >= 5' \
	>/dev/null <<<"$state" || ex_fail "GET /admin/entities/key/abusiva sem o bloqueio da escada de abuso: $state"
listed="$(admin GET '/admin/entities?status=suspended' | jq -r '[.entities[] | select(.status == "suspended") | .entity.id] | join(",")')"
[ "$listed" = abusiva ] || ex_fail "GET /admin/entities?status=suspended deveria listar só a abusiva, veio: $listed"
echo "ok  /admin/entities: abusiva suspended (by abuse-ladder, com until); lista suspended = [$listed]"

# --- 3. A chave bem-comportada não é afetada.
for i in 1 2 3; do
	call wwz-key-ok
	[ "$EX_HTTP_CODE" = 200 ] || ex_fail "chave bem-comportada: HTTP $EX_HTTP_CODE"
done
echo "ok  bem-comportada segue em 200"

# --- 4. Desbloqueio pela admin: acesso volta. O perdão dos strikes e o reset dos
# contadores evitam que o próximo 429 reabra o bloqueio.
admin POST /admin/entities/key/abusiva/unblock >/dev/null || ex_fail "unblock falhou"
admin POST /admin/entities/key/abusiva/reset '{"scope":["counters","strikes"]}' >/dev/null || ex_fail "reset falhou"
call wwz-key-abusiva
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "abusiva depois do desbloqueio: HTTP $EX_HTTP_CODE"
state="$(admin GET /admin/entities/key/abusiva)"
jq -e '.status == "active" and .strikes == 0' >/dev/null <<<"$state" || ex_fail "estado depois do desbloqueio: $state"
echo "ok  unblock + reset restauram o 200 (status active, strikes 0)"

echo "OK ban-por-abuso"

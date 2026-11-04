#!/usr/bin/env bash
# Smoke do exemplo cota-5h-semana (ADR 0016): esgota a janela de 5 h de uma conta
# (429 GoUsageLimitError "5 hour" com retry-after de ~5 h) e a janela semanal de
# outra (limitName "weekly", retry-after até a segunda 00:00 UTC); GET /v1/usage
# mostra as três janelas (rolling com resetsAt = primeiro gasto + 5 h); a conta sem cota continua em
# 200; o reset de contadores pela API admin devolve o acesso. Nada espera por
# janela real: o preço inflado do modelo faz poucas chamadas gastarem a cota.
# Na base Go a cota cai pelo opencode.allowance (allowance_factor 1 = preço de
# lista) e o saldo não é debitado (cost 0, spend_usd 0; ADR 0072).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init cota-5h-semana 6
export EX_ADMIN_PORT="$((EX_PORT + 1000))"
ex_up

URL="http://127.0.0.1:$EX_PORT/zen/go/v1/chat/completions"
ADMIN="http://127.0.0.1:$EX_ADMIN_PORT"
TOKEN=wwz-ex-admin-token
BODY='{"model":"glm-5.2","messages":[{"role":"user","content":"oi"}]}'
HDR="$(mktemp)"
trap 'rc=$?; set +e; rm -f "$HDR"; (exit $rc); ex_down' EXIT

# hdr <nome>: valor do header da última chamada.
hdr() { grep -i "^$1:" "$HDR" | head -1 | cut -d: -f2- | tr -d ' \r'; }

# call <chave>: preenche EX_HTTP_CODE, EX_HTTP_BODY e o arquivo de headers.
call() {
	local out
	out="$(curl -sS --max-time 30 -D "$HDR" -w $'\n%{http_code}' \
		-H "Authorization: Bearer $1" -H "x-opencode-session: ses-exemplo" -H 'Content-Type: application/json' -d "$BODY" "$URL" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
}

# drain <chave> <máx>: chama até o primeiro 429 (ou <máx> chamadas); deixa o
# número de 200 em OK200, o último 200 em LAST_OK_BODY e a soma de
# usage.opencode.allowance (o débito da cota, em US$) em QUOTA.
OK200=0
LAST_OK_BODY=""
QUOTA=0
drain() {
	local i
	OK200=0
	QUOTA=0
	for ((i = 0; i < $2; i++)); do
		call "$1"
		case "$EX_HTTP_CODE" in
		200)
			OK200=$((OK200 + 1))
			LAST_OK_BODY="$EX_HTTP_BODY"
			QUOTA="$(jq -r --argjson q "$QUOTA" '$q + .usage.opencode.allowance' <<<"$EX_HTTP_BODY")"
			;;
		429) return 0 ;;
		*) ex_fail "$1: HTTP $EX_HTTP_CODE inesperado: $EX_HTTP_BODY" ;;
		esac
	done
	ex_fail "$1: nenhum 429 em $2 chamadas"
}

# spend <conta>: gasto acumulado em US$ pela API admin.
spend() {
	curl -fsS -H "Authorization: Bearer $TOKEN" "$ADMIN/admin/entities/account/$1" | jq -r '.spend_usd // 0'
}

# --- 1. Janela de 5 h (20 % de US$ 60 = US$ 12): ~US$ 1,50 por chamada, 8 chamadas.
echo "== ws-5h: cota de 5 h (US$ 12)"
drain wwz-key-go-5h 20
ex_assert_between "$OK200" 7 9 "respostas 200 até esgotar a janela de 5 h"
jq -e '.usage.cost == 0 and .usage.opencode.billing_source == "go" and (.usage.opencode.list_cost > 1.0 and .usage.opencode.list_cost < 2.0) and .usage.opencode.allowance == .usage.opencode.list_cost' \
	>/dev/null <<<"$LAST_OK_BODY" || ex_fail "uso por chamada incoerente (esperado cost 0 e ~US\$ 1,50 em opencode.allowance): $LAST_OK_BODY"
echo "ok  cada chamada debita ~US\$ 1,50 da cota (usage.opencode.allowance) e cost 0"
jq -e '.type == "error" and .error.type == "GoUsageLimitError" and .metadata.limitName == "5 hour"' \
	>/dev/null <<<"$EX_HTTP_BODY" || ex_fail "429 fora do formato GoUsageLimitError/5 hour: $EX_HTTP_BODY"
ex_assert_between "$(hdr retry-after)" 14400 18010 "retry-after (s) até o reset da janela de 5 h"
ex_assert_between "${QUOTA%%.*}" 12 12 "cota debitada da conta ws-5h em US\$ (inteiro)"
gasto="$(spend ws-5h)"
[ "$gasto" = 0 ] || ex_fail "a base Go não debita o saldo: spend_usd de ws-5h = $gasto"
echo "ok  base Go: spend_usd 0 (só a cota cai)"

# usage <chave>: GET /zen/go/v1/usage (percentuais da cota da conta).
usage() {
	curl -fsS --max-time 30 -H "Authorization: Bearer $1" "http://127.0.0.1:$EX_PORT/zen/go/v1/usage"
}

# US$ 12 de cota: 5 h = 12/12 = 100 % (rate-limited: a janela nega), semana =
# 12/30 = 40 %, mês = 12/60 = 20 %;
# a janela de 5 h libera 5 h depois do primeiro gasto, feito há segundos.
u="$(usage wwz-key-go-5h)" || ex_fail "GET /zen/go/v1/usage falhou"
jq -e '.usage.rolling.percent == 100 and .usage.weekly.percent == 40 and .usage.monthly.percent == 20 and .usage.rolling.status == "rate-limited" and .usage.weekly.status == "ok" and .usage.monthly.status == "ok"' \
	>/dev/null <<<"$u" || ex_fail "uso da cota de ws-5h incoerente (esperado 5 h 100 %, semana 40 %, mês 20 %): $u"
falta="$(jq '(.usage.rolling.resetsAt | sub("\\.[0-9]+Z$"; "Z") | fromdate) - (now | floor)' <<<"$u")"
ex_assert_between "$falta" 17000 18000 "rolling.resetsAt (s à frente) = primeiro gasto + 5 h"
echo "ok  GET /zen/go/v1/usage: 5 h 100 % (rate-limited), semana 40 %, mês 20 %"


# --- 2. Janela semanal (25 % de US$ 60 = US$ 15): 10 chamadas; a de 5 h (100 %) não barra.
echo "== ws-semana: cota semanal (US\$ 15)"
drain wwz-key-go-semana 25
ex_assert_between "$OK200" 9 11 "respostas 200 até esgotar a janela semanal"
jq -e '.type == "error" and .error.type == "GoUsageLimitError" and .metadata.limitName == "weekly"' \
	>/dev/null <<<"$EX_HTTP_BODY" || ex_fail "429 fora do formato GoUsageLimitError/weekly: $EX_HTTP_BODY"
# A semana vira na segunda 00:00 UTC (como no real): retry-after = segundos até lá.
segunda="$(jq -n 'now | floor | . - (. - 345600) % 604800 + 604800 - .')"
ex_assert_between "$(hdr retry-after)" "$((segunda - 60))" "$((segunda + 1))" "retry-after (s) até a segunda 00:00 UTC"
u="$(usage wwz-key-go-semana)" || ex_fail "GET /zen/go/v1/usage falhou"
# O percentual segue o débito da cota: 5 h = cota/60 (rolling_5h_pct 100 %),
# semana = cota/15 (limitado a 100 %), mês = cota/60.
gasto="$QUOTA"
jq -e --argjson s "$gasto" '.usage.rolling.percent == ($s / 60 * 100 | round) and .usage.weekly.percent == ([($s / 15 * 100 | round), 100] | min) and .usage.monthly.percent == ($s / 60 * 100 | round)' \
	>/dev/null <<<"$u" || ex_fail "uso da cota de ws-semana incoerente com o débito de US\$ $gasto: $u"
echo "ok  GET /zen/go/v1/usage: percentuais de ws-semana batem com a cota debitada (US\$ $gasto)"

# Sem chave: /v1/models responde (público no real) e /v1/usage é 401 AuthError.
code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 "http://127.0.0.1:$EX_PORT/zen/go/v1/models")"
[ "$code" = 200 ] || ex_fail "GET /zen/go/v1/models sem chave: HTTP $code"
u="$(curl -sS --max-time 30 "http://127.0.0.1:$EX_PORT/zen/go/v1/usage")"
jq -e '.type == "error" and .error.type == "AuthError" and .error.message == "Unauthorized"' \
	>/dev/null <<<"$u" || ex_fail "GET /zen/go/v1/usage sem chave fora do formato real: $u"
echo "ok  sem chave: /v1/models 200 e /v1/usage 401 AuthError"

# --- 3. A conta sem cota não é afetada.
call wwz-key-go-livre
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "ws-livre: HTTP $EX_HTTP_CODE com as outras contas esgotadas"
echo "ok  ws-livre (sem cota) segue em 200"

# --- 4. Reset de contadores pela API admin (stand-in de esperar 5 h): acesso volta.
call wwz-key-go-5h
[ "$EX_HTTP_CODE" = 429 ] || ex_fail "ws-5h deveria seguir em 429 antes do reset, veio $EX_HTTP_CODE"
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
	-d '{"scope":["counters"]}' "$ADMIN/admin/entities/account/ws-5h/reset" >/dev/null || ex_fail "reset da conta ws-5h falhou"
call wwz-key-go-5h
[ "$EX_HTTP_CODE" = 200 ] || ex_fail "ws-5h depois do reset: HTTP $EX_HTTP_CODE"
echo "ok  reset de contadores devolve o acesso (200)"

echo "OK cota-5h-semana"

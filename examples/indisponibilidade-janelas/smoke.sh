#!/usr/bin/env bash
# Smoke do exemplo indisponibilidade-janelas (ADR 0016, spec 009).
#  1. Janela de segundos (`*/20 * * * * *`, 5 s): sonda 1/s por 45 s; dentro da
#     janela 100 % de 503 com retry-after até o fim dela, fora dela 200.
#  2. Janelas de minutos e de horas: um teste de relógio. O relógio vem do
#     cabeçalho `Date` do próprio wwz; o resultado esperado é calculado a partir
#     dele (minuto múltiplo de 10 -> 500; hora múltipla de 6 -> 502; senão 200).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init indisponibilidade-janelas 4
ex_up

KEY=wwz-key-demo
URL="http://127.0.0.1:$EX_PORT"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/wwz-ex-janelas.XXXXXX")"
trap '_rc=$?; rm -rf "$TMP"; (exit $_rc); ex_down' EXIT

# probe <modelo>: uma chamada; preenche P_CODE, P_RA (retry-after, vazio se
# ausente), P_BODY e P_H/P_M/P_S (hora, minuto, segundo do cabeçalho Date, UTC).
probe() {
	local model="$1" date_line
	P_CODE="$(curl -sS --max-time 10 -D "$TMP/h.txt" -o "$TMP/b.txt" -w '%{http_code}' \
		-H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$model\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"oi\"}]}" \
		"$URL/v1/chat/completions" 2>/dev/null)" || P_CODE=000
	P_BODY="$(cat "$TMP/b.txt" 2>/dev/null || true)"
	P_RA="$(tr -d '\r' <"$TMP/h.txt" | awk -F': ' 'tolower($1)=="retry-after"{print $2}')"
	date_line="$(tr -d '\r' <"$TMP/h.txt" | awk -F': ' 'tolower($1)=="date"{print $2}')"
	# "Sat, 31 Oct 2026 13:46:55 GMT" -> 13 46 55 (sem zeros à esquerda, base 10)
	set -- $(printf '%s' "$date_line" | sed -E 's/.* ([0-9]{2}):([0-9]{2}):([0-9]{2}) GMT.*/\1 \2 \3/')
	P_H=$((10#${1:-99})) P_M=$((10#${2:-99})) P_S=$((10#${3:-99}))
}

# --- 1. Janela de segundos: 45 s de sonda a 1/s. A janela cobre os segundos
# :00-:04, :20-:24 e :40-:44; avaliam-se só os segundos longe das bordas (o
# Date tem resolução de 1 s): dentro = 1..3 de cada 20, fora = 6..18.
echo "== janela de segundos: sonda 1/s por 45 s"
in_n=0 out_n=0 in_bad=0 out_bad=0 ra_bad=0
end=$((SECONDS + 45))
while [ "$SECONDS" -lt "$end" ]; do
	probe janela-segundos
	s20=$((P_S % 20))
	if [ "$s20" -ge 1 ] && [ "$s20" -le 3 ]; then
		in_n=$((in_n + 1))
		[ "$P_CODE" = 503 ] || in_bad=$((in_bad + 1))
		jq -e '.error.message and .error.type' >/dev/null 2>&1 <<<"$P_BODY" || in_bad=$((in_bad + 1))
		# retry-after = segundos até o fim da janela: no máximo 5 - s20 (+1 de arredondamento).
		case "$P_RA" in "" | *[!0-9]*) ra_bad=$((ra_bad + 1)) ;; *)
			{ [ "$P_RA" -ge 1 ] && [ "$P_RA" -le $((6 - s20)) ]; } || ra_bad=$((ra_bad + 1))
			;;
		esac
	elif [ "$s20" -ge 6 ] && [ "$s20" -le 18 ]; then
		out_n=$((out_n + 1))
		[ "$P_CODE" = 200 ] || out_bad=$((out_bad + 1))
	fi
	sleep 1
done
echo "amostras dentro da janela: $in_n; fora: $out_n"
[ "$in_n" -ge 4 ] || ex_fail "poucas amostras dentro da janela ($in_n < 4)"
[ "$out_n" -ge 15 ] || ex_fail "poucas amostras fora da janela ($out_n < 15)"
ex_assert_between "$in_bad" 0 0 "amostras dentro da janela que não deram 503 com envelope"
ex_assert_between "$ra_bad" 0 0 "503 sem retry-after válido (<= fim da janela)"
ex_assert_between "$out_bad" 0 0 "amostras fora da janela que não deram 200"

# --- 2. Minutos e horas pelo relógio do servidor. Longe da virada de minuto
# (segundos 5..54) o estado das janelas é inequívoco.
echo "== janelas de minutos e de horas: teste de relógio"
probe janela-minutos
tries=0
while [ "$P_S" -lt 5 ] || [ "$P_S" -gt 54 ]; do
	tries=$((tries + 1))
	[ "$tries" -le 70 ] || ex_fail "cabeçalho Date ausente ou ilegível nas respostas"
	sleep 1
	probe janela-minutos
done
if [ $((P_M % 10)) -eq 0 ]; then
	want_min=500
	echo "agora ${P_H}:${P_M} UTC: DENTRO da janela de minutos (hh:m0, 1 min)"
else
	want_min=200
	echo "agora ${P_H}:${P_M} UTC: fora da janela de minutos"
fi
[ "$P_CODE" = "$want_min" ] || ex_fail "janela-minutos às ${P_H}:${P_M}:${P_S} respondeu $P_CODE (esperado $want_min)"
# 500 e 502 não levam retry-after (como o real, ADR 0042); se vier, não passa do fim da janela.
if [ "$want_min" = 500 ] && [ -n "$P_RA" ]; then
	ex_assert_between "$P_RA" 1 60 "retry-after da janela de minutos (<= 1 min)"
fi
echo "ok  janela-minutos = $P_CODE"

probe janela-horas
if [ $((P_H % 6)) -eq 0 ]; then
	want_h=502
	echo "agora ${P_H}h UTC: DENTRO da janela de horas (00/06/12/18, 1 h)"
else
	want_h=200
	echo "agora ${P_H}h UTC: fora da janela de horas"
fi
[ "$P_CODE" = "$want_h" ] || ex_fail "janela-horas às ${P_H}:${P_M}:${P_S} respondeu $P_CODE (esperado $want_h)"
if [ "$want_h" = 502 ] && [ -n "$P_RA" ]; then
	ex_assert_between "$P_RA" 1 3600 "retry-after da janela de horas (<= 1 h)"
fi
echo "ok  janela-horas = $P_CODE"

echo "OK indisponibilidade-janelas"

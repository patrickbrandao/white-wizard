#!/usr/bin/env bash
# Smoke do exemplo latencia-intermitente (ADR 0016): 300 chamadas a ~10/s
# medem o tempo total de cada uma; confere que existem janelas lentas e rápidas
# alternadas a cada ~10 s e que o p95/p50 passa de um piso.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init latencia-intermitente 3
ex_up

KEY=wwz-key-demo
URL="http://127.0.0.1:$EX_PORT"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/wwz-ex-latencia.XXXXXX")"
trap '_rc=$?; rm -rf "$TMP"; (exit $_rc); ex_down' EXIT

N=300
echo "== $N chamadas a ~10/s; janelas lentas de 3 s a cada 10 s"
# Disparo em segundo plano: uma chamada lenta (1 s) não atrasa as seguintes.
for ((i = 0; i < N; i++)); do
	(
		t0="$(date +%s)"
		r="$(curl -sS --max-time 20 -o /dev/null -w '%{time_total} %{http_code}' \
			-H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
			-d "{\"model\":\"gpt-4o-mini\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"teste $i\"}]}" \
			"$URL/v1/chat/completions" 2>/dev/null)" || r="99 000"
		printf '%s %s %s\n' "$i" "$t0" "$r" >"$TMP/r.$i"
	) &
	sleep 0.1
done
wait

cat "$TMP"/r.* | sort -n >"$TMP/all.txt" # índice, epoch de disparo, tempo total, código
total="$(wc -l <"$TMP/all.txt" | tr -d ' ')"
ex_assert_between "$total" "$N" "$N" "chamadas concluídas"

# Percentis (em ms) do tempo total.
read -r p50 p95 pmax <<<"$(awk '{printf "%d\n", $3*1000}' "$TMP/all.txt" | sort -n | awk '{a[NR]=$1} END{printf "%d %d %d", a[int(NR*0.5)+1], a[int(NR*0.95)+1], a[NR]}')"
echo "p50=${p50} ms  p95=${p95} ms  max=${pmax} ms"
[ "$p50" -ge 1 ] || p50=1
ratio=$((p95 / p50))
ex_assert_between "$ratio" 5 100000 "razão p95/p50 (piso 5; esperado ~20)"

# Lenta = >= 800 ms (hang_duration 1 s); rápida = < 400 ms.
slow="$(awk '$3>=0.8' "$TMP/all.txt" | wc -l | tr -d ' ')"
fast="$(awk '$3<0.4' "$TMP/all.txt" | wc -l | tr -d ' ')"
mid=$((total - slow - fast))
echo "rápidas=$fast lentas=$slow intermediárias=$mid"
# 3 s de cada 10 s = 30 % do tempo: 90 +- 4 sigma de uma janela (n*p*(1-p) com
# n=300, p=0,3: sigma=7,9, 4 sigma=32), com folga para as bordas da janela.
ex_assert_between "$slow" 40 140 "chamadas lentas (~30 %)"
ex_assert_between "$fast" 140 260 "chamadas rápidas (~70 %)"
# Rápidas dão 200; a falha injetada na janela lenta nunca sai como 200 no meio dela.
bad_fast="$(awk '$3<0.4 && $4!=200' "$TMP/all.txt" | wc -l | tr -d ' ')"
ex_assert_between "$bad_fast" 0 0 "rápidas que não deram 200"

# Alternância: início de cada janela lenta = primeira chamada lenta a mais de
# 3 s da lenta anterior (em segundos de relógio); o espaço entre inícios é o período.
starts="$(awk '$3>=0.8 {print $2}' "$TMP/all.txt" | sort -n | awk 'NR==1 || $1-prev>3 {print $1} {prev=$1}')"
nwin="$(printf '%s\n' "$starts" | grep -c . || true)"
# O disparo de 300 chamadas leva 30-50 s (fork do curl); em S segundos cabem
# S/10 ciclos, mais até duas janelas cortadas nas pontas.
span="$(awk 'NR==1{a=$2} {b=$2} END{print b-a}' "$TMP/all.txt")"
echo "janelas lentas observadas: $nwin em ${span} s"
ex_assert_between "$nwin" $((span / 10)) $((span / 10 + 2)) "janelas lentas (uma por ciclo de 10 s)"
bad_period=0
prev=""
for s in $starts; do
	if [ -n "$prev" ]; then
		d=$((s - prev))
		echo "período entre janelas: ${d} s"
		{ [ "$d" -ge 8 ] && [ "$d" -le 12 ]; } || bad_period=$((bad_period + 1))
	fi
	prev="$s"
done
ex_assert_between "$bad_period" 0 0 "períodos fora de 10 s +- 2 s"

echo "OK latencia-intermitente"

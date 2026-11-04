#!/usr/bin/env bash
# Smoke de benchmark-vazao-maxima (spec 019, ADR 0016): carga curta do wwz-load
# contra o wwz sem latência nem limites. Exige o JSON da spec 015 com
# error_rate = 0 e req/s > 0; o req/s só é registrado (sem meta numérica).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init benchmark-vazao-maxima 15

# Imagem do cliente: acompanha a do wwz (:verify do verify-full, senão :dev).
if [ -z "${WWZ_LOAD_IMAGE:-}" ]; then
	case "$WWZ_IMAGE" in
	*:verify) WWZ_LOAD_IMAGE=white-wizard-load:verify ;;
	*) WWZ_LOAD_IMAGE=white-wizard-load:dev ;;
	esac
fi
docker image inspect "$WWZ_LOAD_IMAGE" >/dev/null 2>&1 ||
	ex_fail "imagem $WWZ_LOAD_IMAGE ausente (make image-load)"
export WWZ_LOAD_IMAGE
export WWZ_LOAD_DURATION="${WWZ_LOAD_DURATION:-5s}" # curto; o padrão do compose é 10s
export WWZ_LOAD_WARMUP="${WWZ_LOAD_WARMUP:-1s}"

OUT="$(mktemp)"
trap 'rc=$?; rm -f "$OUT"; (exit $rc); ex_down' EXIT

ex_up
# --wait não sobe o serviço do perfil load; o run sobe só ele, com o wwz já saudável.
ex_compose --profile load run --rm -T wwz-load >"$OUT" || ex_fail "wwz-load saiu com erro"

jq -e '.version == 1 and (.total | has("requests") and has("errors") and has("rps"))
	and (.total.ttft_ms | has("p50") and has("p99"))' "$OUT" >/dev/null ||
	ex_fail "saída do wwz-load fora do esquema da spec 015: $(head -c 300 "$OUT")"

req="$(jq -r '.total.requests' "$OUT")"
err="$(jq -r '.total.errors' "$OUT")"
dropped="$(jq -r '.total.dropped' "$OUT")"
rps="$(jq -r '.total.rps | floor' "$OUT")"
p50="$(jq -r '.total.ttft_ms.p50' "$OUT")"
p99="$(jq -r '.total.ttft_ms.p99' "$OUT")"
echo "requisições $req, erros $err, descartadas $dropped, vazão $rps req/s, TTFT p50 ${p50} ms p99 ${p99} ms (duração ${WWZ_LOAD_DURATION})"

# error_rate = 0: sem erro configurado e sem limite, nenhuma requisição pode falhar.
ex_assert_between "$err" 0 0 "erros"
[ "$req" -gt 0 ] || ex_fail "nenhuma requisição medida"
# Sem meta numérica: só exige vazão positiva (o teto depende da máquina).
ex_assert_between "$rps" 1 100000000 "vazão (req/s, só registrada)"
# O wwz é quem conta: o /admin/stats anexado ao relatório confirma que ele recebeu a carga.
jq -e '.admin_stats != null' "$OUT" >/dev/null || ex_fail "relatório sem admin_stats"
echo "OK benchmark-vazao-maxima"

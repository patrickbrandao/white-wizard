#!/usr/bin/env bash
# Checagem estática do deploy/grafana (task 122, spec 013, ADR 0007).
#
# Uso: scripts/check-grafana.sh
#
# 1. Todo deploy/grafana/dashboards/*.json é JSON válido, com uid e painéis.
# 2. Toda métrica wwz_* citada numa PromQL (targets e variáveis) existe no
#    código Go (não teste) de internal/ ou cmd/: wwz e wwz-panel. Sufixos
#    _bucket/_sum/_count valem se a métrica-base existir.
# 3. O datasource dos painéis é a variável ${datasource} ou o uid provisionado.
# 4. Com Docker CLI: `docker compose config` do deploy/grafana (sem pull);
#    sem Docker, esse passo é pulado.
#
# Não sobe nada nem baixa imagem. Saída: 0 ok; 1 com falhas.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

app_bin=/Applications/Docker.app/Contents/Resources/bin
if [ -d "$app_bin" ]; then
	PATH="$PATH:$app_bin"
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "grafana: jq ausente" >&2
	exit 1
fi

fail=0
err() {
	echo "grafana: $*" >&2
	fail=1
}

DS_UID="$(sed -nE 's/^[[:space:]]*uid:[[:space:]]*([A-Za-z0-9_-]+).*/\1/p' deploy/grafana/provisioning/datasources/prometheus.yml | head -n1)"
[ -n "$DS_UID" ] || err "uid do datasource não encontrado em provisioning/datasources/prometheus.yml"

# Fontes Go que definem métricas (sem testes e sem linhas só de comentário).
src="$(find internal cmd -name '*.go' ! -name '*_test.go' -print0 |
	xargs -0 grep -h 'wwz_' | grep -vE '^[[:space:]]*//')"

shopt -s nullglob
dashboards=(deploy/grafana/dashboards/*.json)
[ "${#dashboards[@]}" -gt 0 ] || err "nenhum dashboard em deploy/grafana/dashboards"

total=0
for d in "${dashboards[@]}"; do
	if ! jq -e 'type == "object" and (.uid | type == "string") and (.panels | length > 0)' "$d" >/dev/null 2>&1; then
		err "$d: JSON inválido ou sem uid/painéis"
		continue
	fi
	exprs="$(jq -r '[.. | objects | (.expr?, .definition?) | strings] | .[]' "$d")"
	names="$(grep -oE 'wwz_[a-z0-9_]+' <<<"$exprs" | sort -u)"
	for n in $names; do
		total=$((total + 1))
		ok=0
		for cand in "$n" "${n%_bucket}" "${n%_sum}" "${n%_count}"; do
			if grep -qwF "$cand" <<<"$src"; then
				ok=1
				break
			fi
		done
		[ "$ok" -eq 1 ] || err "$d: métrica $n não existe em internal/ nem cmd/"
	done
	bad="$(jq -r --arg uid "$DS_UID" '[.. | objects | select(has("datasource")) | .datasource
		| select(type == "object") | .uid | select(. != "${datasource}" and . != $uid)] | unique | .[]' "$d")"
	[ -z "$bad" ] || err "$d: datasource desconhecido: $bad"
done

if command -v docker >/dev/null 2>&1; then
	if ! GRAFANA_ADMIN_PASSWORD=check docker compose -f deploy/grafana/docker-compose.yml config -q; then
		err "docker compose config falhou"
	fi
else
	echo "grafana: docker compose config pulado (sem Docker CLI)"
fi

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo "grafana: ok (${#dashboards[@]} dashboard(s), $total métrica(s) conferida(s))"

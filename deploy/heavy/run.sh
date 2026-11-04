#!/usr/bin/env bash
# Rodadas do verify-heavy no servidor pesado (spec 018, ADRs 0017 e 0037).
#
# Uso (dentro de $WWZ_HEAVY_DIR, chamado por scripts/heavy.sh via ssh):
#   HEAVY_TAG=heavy-<sha7> HEAVY_RUN=<carimbo> ./run.sh cluster|throughput|streams|streams-max
#   ./run.sh cleanup        # derruba o projeto wwz-heavy e remove as imagens :heavy-*
#
# Cada rodada:
#   - recusa começar com load average ≥ HEAVY_MAX_LOAD (48) ou com o compose da
#     rodada mais os containers wwz* da produção ainda de pé acima de
#     HEAVY_MAX_CPUS (32) cpus / HEAVY_MAX_MEMORY_GB (64) de memória;
#   - roda sob `timeout HEAVY_ROUND_TIMEOUT` (600 s);
#   - tem um monitor que grava `docker stats` a cada ~5 s em stats.jsonl e aborta
#     a rodada (docker compose kill) se o load average passar de HEAVY_MAX_LOAD;
#   - grava em runs/<HEAVY_RUN>/<rodada>/: report.json (wwz-load) ou limits.log,
#     stats.jsonl, meta.json e status (a throughput grava também overhead.json,
#     da fase em malha aberta); derruba o compose com down -v ao fim.
#
# Saída: 0 ok; 1 a rodada falhou; 2 uso/orçamento; 3 abortada pelo load average;
# 4 load average alto antes de começar; 124 timeout.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE" || exit 2
if [ -f .env ]; then
	set -a
	# shellcheck disable=SC1091
	. ./.env
	set +a
fi

ROUND="${1:-}"
HEAVY_MAX_LOAD="${HEAVY_MAX_LOAD:-48}"
HEAVY_ROUND_TIMEOUT="${HEAVY_ROUND_TIMEOUT:-600}"
HEAVY_MAX_CPUS="${HEAVY_MAX_CPUS:-32}"
HEAVY_MAX_MEMORY_GB="${HEAVY_MAX_MEMORY_GB:-64}"
HEAVY_STREAMS="${HEAVY_STREAMS:-7000}"
# Fase de overhead da rodada throughput (task 409): malha aberta a
# HEAVY_OVERHEAD_RPS req/s (padrão HEAVY_MIN_RPS, a meta da spec 014), abaixo
# da saturação, para o e2e p99 com latência zero ser o overhead próprio do wwz
# e não a fila da malha fechada de 512 conexões.
HEAVY_OVERHEAD_RPS="${HEAVY_OVERHEAD_RPS:-${HEAVY_MIN_RPS:-10000}}"
HEAVY_MAX_SCHEDULE_LAG_MS="${HEAVY_MAX_SCHEDULE_LAG_MS:-1}"
HEAVY_STREAMS_MAX="${HEAVY_STREAMS_MAX:-20000}"
HEAVY_VALKEY_IMAGE="${HEAVY_VALKEY_IMAGE:-valkey/valkey:8.1-alpine}"
export HEAVY_VALKEY_IMAGE
PROJECT=wwz-heavy
DC=(docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml")
ALL_PROFILES=(--profile cluster --profile throughput --profile streams)
PULLED_FILE="$HERE/.pulled-images"

log() { printf '[run.sh %s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

loadavg() { cut -d' ' -f1 /proc/loadavg; }

# above <a> <b>: sucesso se a > b.
above() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'; }

# compose_down: derruba tudo do projeto, em qualquer perfil, com volumes.
compose_down() {
	WWZ_HEAVY_IMAGE=x WWZ_HEAVY_LOAD_IMAGE=x "${DC[@]}" "${ALL_PROFILES[@]}" \
		down -v --remove-orphans --timeout 10 >/dev/null 2>&1
	local ids
	ids="$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT")"
	[ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
	ids="$(docker ps -aq --filter "name=^wwz-heavy")"
	[ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
	docker network ls -q --filter "name=^wwz-heavy" | xargs -r docker network rm >/dev/null 2>&1
	docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT" | xargs -r docker volume rm >/dev/null 2>&1
	return 0
}

# cleanup: projeto, imagens :heavy-* (todas, salvo HEAVY_KEEP_IMAGES=1 que
# mantém as de HEAVY_TAG), imagens base puxadas por este script, binário de teste
# e relatórios já coletados.
do_cleanup() {
	compose_down
	local img keep=""
	[ "${HEAVY_KEEP_IMAGES:-0}" = 1 ] && keep="${HEAVY_TAG:-}"
	for img in $(docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^white-wizard[a-z-]*:heavy-'); do
		[ -n "$keep" ] && [ "${img##*:}" = "$keep" ] && continue
		docker image rm "$img" >/dev/null 2>&1 || log "não removi $img"
	done
	if [ -f "$PULLED_FILE" ]; then
		while read -r img; do
			[ -n "$img" ] && docker image rm "$img" >/dev/null 2>&1
		done <"$PULLED_FILE"
		rm -f "$PULLED_FILE"
	fi
	[ "${HEAVY_KEEP_IMAGES:-0}" = 1 ] || rm -f "$HERE/bin/limits.test"
	rm -rf "$HERE/runs"
	docker image prune -f --filter "label=org.opencontainers.image.title=white-wizard" >/dev/null 2>&1
	docker image prune -f --filter "label=org.opencontainers.image.title=white-wizard-load" >/dev/null 2>&1
	local left
	left="$(docker ps -a --format '{{.Names}}' | grep -E '^wwz-heavy' | tr '\n' ' ')"
	left="$left$(docker network ls --format '{{.Name}}' | grep -E '^wwz-heavy' | tr '\n' ' ')"
	left="$left$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^white-wizard[a-z-]*:heavy-' | grep -v ":${keep:-^}\$" | tr '\n' ' ')"
	if [ -n "$left" ]; then
		echo "sobrou: $left"
		return 1
	fi
	echo "limpo: nenhum container, rede ou imagem wwz-heavy"
}

# ensure_image <imagem>: puxa se faltar e anota para o cleanup remover.
ensure_image() {
	docker image inspect "$1" >/dev/null 2>&1 && return 0
	log "docker pull $1"
	docker pull -q "$1" >/dev/null || return 1
	echo "$1" >>"$PULLED_FILE"
}

# prod_usage: "cpus memória_GiB" somados dos limites dos containers wwz* de pé
# que não são da rodada pesada (o que a produção manteve rodando).
prod_usage() {
	docker ps -q --filter "name=^wwz" |
		xargs -r docker inspect --format '{{.Name}} {{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}' 2>/dev/null |
		awk '$1 !~ /^\/wwz-heavy/ { c += $2 / 1e9; m += $3 / 1073741824 } END { printf "%g %g\n", c, m }'
}

# budget <perfil>: confere a soma de cpus e memória dos serviços do perfil mais
# a da produção ainda de pé (ADR 0017: teto de 32 núcleos e 64 GB somados).
budget() {
	local json cpus mem pc pm tc tm
	json="$("${DC[@]}" --profile "${1%-max}" config --format json)" || return 1
	cpus="$(jq '[.services[] | .deploy.resources.limits.cpus // "0" | tonumber] | add' <<<"$json")"
	mem="$(jq '[.services[] | .deploy.resources.limits.memory // 0 | tonumber] | add / 1073741824' <<<"$json")"
	read -r pc pm <<<"$(prod_usage)"
	pc="${pc:-0}" pm="${pm:-0}"
	tc="$(awk -v a="$cpus" -v b="$pc" 'BEGIN { printf "%g", a + b }')"
	tm="$(awk -v a="$mem" -v b="$pm" 'BEGIN { printf "%g", a + b }')"
	printf '{"cpus":%s,"memory_gb":%s,"prod_cpus":%s,"prod_memory_gb":%s,"total_cpus":%s,"total_memory_gb":%s}\n' \
		"$cpus" "$mem" "$pc" "$pm" "$tc" "$tm" >"$OUT/budget.json"
	if above "$tc" "$HEAVY_MAX_CPUS" || above "$tm" "$HEAVY_MAX_MEMORY_GB"; then
		log "orçamento estourado: rodada $cpus cpus/$mem GiB + produção $pc cpus/$pm GiB (teto $HEAVY_MAX_CPUS / $HEAVY_MAX_MEMORY_GB)"
		return 1
	fi
	log "orçamento: rodada $cpus cpus/$mem GiB + produção $pc cpus/$pm GiB = $tc cpus/$tm GiB"
}

# monitor <pid>: stats e load average enquanto <pid> vive.
monitor() {
	local pid="$1" l ids
	while kill -0 "$pid" 2>/dev/null; do
		l="$(loadavg)"
		if above "$l" "$HEAVY_MAX_LOAD"; then
			log "load average $l > $HEAVY_MAX_LOAD: abortando a rodada"
			echo "load average $l > $HEAVY_MAX_LOAD" >"$OUT/aborted"
			"${DC[@]}" "${ALL_PROFILES[@]}" kill >/dev/null 2>&1
			docker ps -q --filter "name=^wwz-heavy" | xargs -r docker kill >/dev/null 2>&1
			kill "$pid" 2>/dev/null
			return
		fi
		ids="$(docker ps -q --filter "name=^wwz-heavy")"
		if [ -n "$ids" ]; then
			# shellcheck disable=SC2086
			docker stats --no-stream --format '{{json .}}' $ids 2>/dev/null |
				jq -c --argjson t "$(date +%s)" --argjson load "$l" '. + {t: $t, load: $load}' >>"$OUT/stats.jsonl" 2>/dev/null
		fi
		sleep 3
	done
}

round_cluster() {
	ensure_image "$HEAVY_VALKEY_IMAGE" || return 1
	[ -x "$HERE/bin/limits.test" ] || {
		log "bin/limits.test ausente (o verify copia no passo store-test)"
		return 1
	}
	"${DC[@]}" --profile cluster up -d --wait valkey-1 valkey-2 valkey-3 || return 1
	"${DC[@]}" --profile cluster run --rm -T cluster-init >"$OUT/cluster-init.log" 2>&1 || {
		log "cluster-init falhou (ver cluster-init.log)"
		return 1
	}
	"${DC[@]}" --profile cluster run --rm -T limits-test >"$OUT/limits.log" 2>&1
}

# round_load <perfil> <flags do wwz-load run...>
round_load() {
	local profile="$1"
	shift
	"${DC[@]}" --profile "$profile" up -d --wait wwz || return 1
	"${DC[@]}" --profile "$profile" run --rm -T --no-deps load run \
		--target http://wwz:8000 --output json --progress 10s "$@" \
		>"$OUT/report.json" 2>"$OUT/load.log"
}

# round_overhead: segunda fase da throughput, com o mesmo wwz já de pé:
# --rps HEAVY_OVERHEAD_RPS (malha aberta, abaixo da saturação) → overhead.json.
# Se o próprio cliente travou (schedule_lag p99 ≥ HEAVY_MAX_SCHEDULE_LAG_MS, 1 ms;
# visto uma vez com lag máximo de 277 ms, tmp/heavy/20261031T232121Z), a medida
# não é do wwz: a fase repete uma vez e a primeira fica em overhead-1.json. O
# verify reprova se a medida final ainda tiver lag alto (check overhead_lag).
round_overhead() {
	local try lag
	for try in 1 2; do
		"${DC[@]}" --profile throughput run --rm -T --no-deps load run \
			--target http://wwz:8000 --output json --progress 10s \
			--rps "$HEAVY_OVERHEAD_RPS" --stream=false --warmup 5s --duration 30s --timeout 30s \
			>"$OUT/overhead.json" 2>"$OUT/overhead.log" || return 1
		lag="$(jq -r '.total.schedule_lag_ms.p99 // 1e9' "$OUT/overhead.json" 2>/dev/null || echo 1e9)"
		above "$HEAVY_MAX_SCHEDULE_LAG_MS" "$lag" && return 0
		[ "$try" = 2 ] && return 0
		log "overhead: schedule_lag p99 $lag ms ≥ $HEAVY_MAX_SCHEDULE_LAG_MS (cliente travou); repetindo a fase"
		mv "$OUT/overhead.json" "$OUT/overhead-1.json"
		mv "$OUT/overhead.log" "$OUT/overhead-1.log"
	done
}

round_body() {
	case "$ROUND" in
	cluster) round_cluster ;;
	throughput)
		round_load throughput --concurrency 512 --stream=false \
			--warmup 10s --duration 60s --timeout 30s || return 1
		round_overhead
		;;
	streams)
		round_load streams --concurrency "$HEAVY_STREAMS" --stream=true --max-tokens 100 \
			--warmup 15s --duration 120s --timeout 60s
		;;
	streams-max)
		# Informativa (ADR 0067): mesma configuração, HEAVY_STREAMS_MAX streams.
		round_load streams --concurrency "$HEAVY_STREAMS_MAX" --stream=true --max-tokens 100 \
			--warmup 15s --duration 120s --timeout 60s
		;;
	esac
}

run_round() {
	: "${HEAVY_TAG:?defina HEAVY_TAG (heavy-<sha7>)}"
	HEAVY_RUN="${HEAVY_RUN:-$(date -u +%Y%m%dT%H%M%SZ)}"
	OUT="$HERE/runs/$HEAVY_RUN/$ROUND"
	mkdir -p "$OUT"
	export WWZ_HEAVY_IMAGE="white-wizard:$HEAVY_TAG" WWZ_HEAVY_LOAD_IMAGE="white-wizard-load:$HEAVY_TAG"
	export HEAVY_ROUND="$ROUND"
	[ "$ROUND" = streams-max ] && export HEAVY_ROUND=streams
	if [ "$ROUND" = streams ] || [ "$ROUND" = streams-max ]; then
		# ADR 0067: o ITL p99 ≤ 22 ms vale com o wwz a ≤ 80 % da CPU dada
		# (HEAVY_STREAMS=7000, task 409; eram 8000 pela ADR 0069); o wwz ganha 12 núcleos nestas rodadas e a
		# streams-max (HEAVY_STREAMS_MAX=20000) é informativa.
		export HEAVY_WWZ_CPUS="${HEAVY_STREAMS_WWZ_CPUS:-12}" HEAVY_WWZ_MEMORY="${HEAVY_STREAMS_WWZ_MEMORY:-16g}"
		export HEAVY_LOAD_CPUS="${HEAVY_STREAMS_LOAD_CPUS:-14}" HEAVY_LOAD_MEMORY="${HEAVY_STREAMS_LOAD_MEMORY:-24g}"
	fi
	local started l rc=0 pid
	started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	l="$(loadavg)"
	if above "$l" "$HEAVY_MAX_LOAD" || [ "$l" = "$HEAVY_MAX_LOAD" ]; then
		echo "load average $l ≥ $HEAVY_MAX_LOAD antes de começar" | tee "$OUT/status" >&2
		return 4
	fi
	if [ "$ROUND" != cluster ]; then
		for img in "$WWZ_HEAVY_IMAGE" "$WWZ_HEAVY_LOAD_IMAGE"; do
			docker image inspect "$img" >/dev/null 2>&1 || {
				echo "imagem $img ausente (o verify envia no passo images)" | tee "$OUT/status" >&2
				return 1
			}
		done
	elif ! docker image inspect "$WWZ_HEAVY_LOAD_IMAGE" >/dev/null 2>&1; then
		echo "imagem $WWZ_HEAVY_LOAD_IMAGE ausente (o verify envia no passo images)" | tee "$OUT/status" >&2
		return 1
	fi
	budget "$ROUND" || {
		echo "orçamento de cpus/memória acima do teto" >"$OUT/status"
		return 2
	}
	compose_down

	trap 'log "sinal recebido: derrubando"; [ -n "${pid:-}" ] && kill "$pid" 2>/dev/null; compose_down; echo "interrompida (timeout ${HEAVY_ROUND_TIMEOUT}s?)" >"$OUT/status"; exit 124' TERM INT
	log "rodada $ROUND ($HEAVY_TAG, run $HEAVY_RUN, load average $l)"
	round_body &
	pid=$!
	monitor "$pid" &
	local mon=$!
	wait "$pid"
	rc=$?
	kill "$mon" 2>/dev/null
	wait "$mon" 2>/dev/null
	trap - TERM INT

	"${DC[@]}" --profile "${ROUND%-max}" logs --no-color --timestamps >"$OUT/compose.log" 2>&1
	compose_down
	if [ -f "$OUT/aborted" ]; then
		rc=3
		echo "abortada: $(cat "$OUT/aborted")" >"$OUT/status"
	elif [ "$rc" -eq 0 ]; then
		echo ok >"$OUT/status"
	else
		echo "falhou (código $rc)" >"$OUT/status"
		rc=1
	fi
	jq -n --arg round "$ROUND" --arg tag "$HEAVY_TAG" --arg started "$started" \
		--arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg status "$(cat "$OUT/status")" \
		--argjson rc "$rc" --argjson load_before "$l" --argjson load_after "$(loadavg)" \
		--argjson budget "$(cat "$OUT/budget.json")" --argjson streams "$([ "$ROUND" = streams-max ] && echo "$HEAVY_STREAMS_MAX" || echo "$HEAVY_STREAMS")" \
		--argjson wwz_cpus "${HEAVY_WWZ_CPUS:-8}" --argjson load_cpus "${HEAVY_LOAD_CPUS:-8}" \
		'{round: $round, tag: $tag, started: $started, finished: $finished, status: $status, rc: $rc,
		  load_before: $load_before, load_after: $load_after, budget: $budget, streams: $streams,
		  wwz_cpus: $wwz_cpus, load_cpus: $load_cpus,
		  host: {nproc: '"$(nproc)"', mem_gb: '"$(awk '/MemTotal/ { printf "%.0f", $2 / 1048576 }' /proc/meminfo)"'}}' >"$OUT/meta.json"
	log "rodada $ROUND: $(cat "$OUT/status")"
	return "$rc"
}

case "$ROUND" in
cluster | throughput | streams | streams-max)
	command -v jq >/dev/null || {
		log "jq ausente no servidor"
		exit 2
	}
	if [ -z "${HEAVY_IN_TIMEOUT:-}" ]; then
		export HEAVY_IN_TIMEOUT=1
		exec timeout --kill-after=30 "$HEAVY_ROUND_TIMEOUT" "$0" "$@"
	fi
	run_round
	;;
cleanup) do_cleanup ;;
*)
	sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac

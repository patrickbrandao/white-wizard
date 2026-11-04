#!/usr/bin/env bash
# verify-heavy (specs/018, tabela "verify-heavy"; ADRs 0017 e 0037).
#
# Biblioteca carregada por `scripts/verify.sh heavy` (usa step_begin/step_end/
# print_summary de lá). Executado direto, delega ao verify.sh.
#
# Passos (no Mac; as rodadas rodam no servidor, pela rede interna do compose):
#   preflight   ferramentas locais, Docker, disco local (VERIFY_MIN_FREE_GB), ssh,
#               load average remoto < HEAVY_MAX_LOAD, ≥ 5 GiB livres em /opt,
#               docker/jq/timeout no servidor.
#   prod-pause  a produção (/opt/white-wizard, projeto compose wwz, ADR 0017) usa até
#               32 núcleos/64 GB, o mesmo teto somado de produção e rodada pesada. Para
#               as instâncias de papel (8000-8006) que estiverem rodando (sobram 12
#               núcleos: wwz-admin, wwz-ports, Valkey, Prometheus, Grafana e painel).
#               O cleanup (trap) as religa. HEAVY_PROD_PAUSE=0 desliga a coordenação
#               (o run.sh recusa as rodadas que não couberem no teto).
#   images      docker buildx --platform linux/amd64 --load dos alvos wwz e wwz-load
#               com tag :heavy-<sha7>; docker save | gzip | ssh docker load;
#               remove as imagens locais logo depois.
#   store-test  go test -c de ./internal/limits (linux/amd64) para
#               $WWZ_HEAVY_DIR/bin/limits.test.
#   sync        deploy/heavy/ → $WWZ_HEAVY_DIR (nunca o .env).
#   cluster     run.sh cluster: Valkey Cluster com 3 mestres + limits.test -test.run Valkey.
#   throughput  run.sh throughput: total.ok_rps ≥ HEAVY_MIN_RPS e erros < 0,1 % na malha
#               fechada (vazão máxima). O overhead p99 (< HEAVY_MAX_OVERHEAD_MS, 2) vem de
#               uma segunda fase, overhead.json: --rps HEAVY_OVERHEAD_RPS (padrão
#               HEAVY_MIN_RPS, a meta da spec 014) em malha aberta, abaixo da saturação;
#               overhead = e2e p99 − latência configurada (zero). Na vazão máxima o e2e
#               inclui a fila de 512 conexões e não mede o wwz (task 409). A fase passa
#               também com ok_rps ≥ 95 % da taxa pedida, erros < 0,1 % e schedule_lag
#               p99 do cliente < HEAVY_MAX_SCHEDULE_LAG_MS (1; se o cliente travou a
#               medida não é do wwz: o run.sh repete a fase uma vez, overhead-1.json).
#   streams     run.sh streams: erros < 0,1 % e total.itl_ms.p99 ≤ HEAVY_MAX_ITL_P99_MS,
#               wwz com HEAVY_STREAMS_WWZ_CPUS (12) e wwz-load com
#               HEAVY_STREAMS_LOAD_CPUS (14) (ADR 0067). Com HEAVY_STREAMS (7000;
#               8000 pela ADR 0069 deu 82 %, task 409) streams o wwz precisa ficar
#               ≤ HEAVY_MAX_WWZ_CPU_PCT (80) % da CPU dada (média das amostras com o
#               wwz-load de pé; a amostra antes da carga não entra). Para caber no teto,
#               para também HEAVY_PROD_PAUSE_STREAMS (wwz-admin) só durante a
#               rodada e o religa logo depois (6 + 12 + 14 = 32 núcleos).
#               Também: overhead p99 do ITL (p99 - HEAVY_CONFIGURED_ITL_MS) < HEAVY_MAX_OVERHEAD_MS
#               e memória do wwz por 10 000 streams (MiB, pico) ≤ HEAVY_MAX_MEM_PER_10K_MB (2048, ADR 0070).
#   streams-max informativa (ADR 0067): mesma configuração com
#               HEAVY_STREAMS_MAX (20000) streams; grava ITL, TTFT, tokens/s,
#               chunks/s por núcleo e CPU, e nunca reprova (só falha sem report.json).
#               Para o wwz-admin como a streams.
#   collect     tmp/heavy/<carimbo>/summary.json com limiares e valores medidos
#               (cada rodada já trouxe seus relatórios e o docker stats).
#   drift       wwz-drift check local; pulado sem ~/.config/white-wizard/real-keys.env
#               e sem as variáveis de chave (ou com HEAVY_DRIFT=0, para repetir
#               rodadas sem gastar chamadas reais); código 3 falha o passo.
#   cleanup     sempre (trap): run.sh cleanup no servidor (projeto wwz-heavy,
#               imagens :heavy-*, base puxada, binário de teste) e imagens
#               :heavy-* locais; falha se sobrar algo.
#
# Servidor (ADR 0076): WWZ_HEAVY_SSH ("ssh [opções] destino") vem do ambiente ou de
# autor/prod.env (fora do git; WWZ_AUTHOR_ENV troca o caminho; modelo em
# deploy/prod/autor.env.example), sem padrão no código: sem ele o preflight falha.
# Variáveis: WWZ_HEAVY_SSH, WWZ_HEAVY_DIR (/opt/white-wizard/heavy), HEAVY_MIN_RPS (10000), HEAVY_STREAMS
# (7000), HEAVY_OVERHEAD_RPS (HEAVY_MIN_RPS), HEAVY_STREAMS_MAX (20000), HEAVY_MAX_WWZ_CPU_PCT (80),
# HEAVY_MAX_ITL_P99_MS (22), HEAVY_MAX_OVERHEAD_MS (2), HEAVY_CONFIGURED_ITL_MS (20),
# HEAVY_MAX_MEM_PER_10K_MB (2048), HEAVY_ROUNDS (cluster,throughput,streams,streams-max),
# HEAVY_MAX_ERROR_PCT (0.1), HEAVY_MAX_LOAD (48), HEAVY_KEEP_IMAGES (0; 1 mantém
# as imagens desta execução no servidor para repetir rodadas), HEAVY_PROD_PAUSE (1),
# HEAVY_PROD_PAUSE_STREAMS (wwz-admin), HEAVY_STREAMS_WWZ_CPUS (12),
# HEAVY_STREAMS_LOAD_CPUS (14), WWZ_PROD_DIR (/opt/white-wizard).

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	exec "$(dirname "$0")/verify.sh" heavy "$@"
fi

# heavy_author_env ARQUIVO: as variáveis WWZ_* de autor/prod.env (sintaxe de
# shell, lida num bash limpo) que o ambiente ainda não tem (vazia conta como
# ausente). Arquivo ausente ou inválido: nada muda (o preflight acusa a falta).
heavy_author_env() {
	local f=$1 k v
	[ -n "$f" ] && [ -f "$f" ] || return 0
	env -i "${BASH:-bash}" -n "$f" 2>/dev/null || return 0
	while IFS= read -r -d '' k && IFS= read -r -d '' v; do
		[ -n "${!k:-}" ] || printf -v "$k" '%s' "$v"
	done < <(env -i "${BASH:-bash}" -c 'source "$1" >/dev/null || exit 1
		for k in $(compgen -v WWZ_); do printf "%s\0%s\0" "$k" "${!k}"; done' _ "$f")
}
heavy_author_env "${WWZ_AUTHOR_ENV:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/autor/prod.env}"
WWZ_HEAVY_SSH="${WWZ_HEAVY_SSH:-}"
WWZ_HEAVY_DIR="${WWZ_HEAVY_DIR:-/opt/white-wizard/heavy}"
HEAVY_MIN_RPS="${HEAVY_MIN_RPS:-10000}"
HEAVY_STREAMS="${HEAVY_STREAMS:-7000}"
HEAVY_OVERHEAD_RPS="${HEAVY_OVERHEAD_RPS:-$HEAVY_MIN_RPS}"
# Medida de overhead só vale com o cliente em dia: schedule_lag p99 do wwz-load
# abaixo disto (ms). O run.sh repete a fase uma vez se o cliente travou.
HEAVY_MAX_SCHEDULE_LAG_MS="${HEAVY_MAX_SCHEDULE_LAG_MS:-1}"
HEAVY_STREAMS_MAX="${HEAVY_STREAMS_MAX:-20000}"
HEAVY_MAX_WWZ_CPU_PCT="${HEAVY_MAX_WWZ_CPU_PCT:-80}"
HEAVY_MAX_ITL_P99_MS="${HEAVY_MAX_ITL_P99_MS:-22}"
HEAVY_ROUNDS="${HEAVY_ROUNDS:-cluster,throughput,streams,streams-max}"
HEAVY_MAX_ERROR_PCT="${HEAVY_MAX_ERROR_PCT:-0.1}"
# Overhead próprio do wwz (specs 014/008): p99 observado menos o configurado,
# estritamente abaixo de HEAVY_MAX_OVERHEAD_MS. A rodada throughput configura
# latência zero (overhead = e2e p99 da fase em malha aberta a HEAVY_OVERHEAD_RPS,
# overhead.json); a streams configura ITL de 20 ms
# (HEAVY_CONFIGURED_ITL_MS; overhead = ITL p99 - 20) e TTFT zero, cujo p99 sai só
# como informação (na saturação o wwz atrasa o TTFT, spec 014).
HEAVY_MAX_OVERHEAD_MS="${HEAVY_MAX_OVERHEAD_MS:-2}"
HEAVY_CONFIGURED_ITL_MS="${HEAVY_CONFIGURED_ITL_MS:-20}"
# Memória do wwz por 10 000 streams (MiB, pico das amostras do docker stats ÷
# streams × 10 000). Limiar 2048 (ADR 0070, spec 018); vazio desliga a checagem.
HEAVY_MAX_MEM_PER_10K_MB="${HEAVY_MAX_MEM_PER_10K_MB:-2048}"
HEAVY_MAX_LOAD="${HEAVY_MAX_LOAD:-48}"
HEAVY_MIN_REMOTE_GB="${HEAVY_MIN_REMOTE_GB:-5}"
HEAVY_KEEP_IMAGES="${HEAVY_KEEP_IMAGES:-0}"
HEAVY_ALL_ROUNDS="cluster throughput streams streams-max"
HEAVY_PROD_PAUSE="${HEAVY_PROD_PAUSE:-1}"
WWZ_PROD_DIR="${WWZ_PROD_DIR:-/opt/white-wizard}"
# Serviços do compose de produção (deploy/prod) que a rodada pesada para.
HEAVY_PROD_ROLES="wwz-openai-perfeito wwz-falhas-20pct wwz-latencia-intermitente wwz-indisponibilidade wwz-anthropic wwz-openrouter wwz-opencode"
HEAVY_PAUSED=""
# Parados só durante a rodada streams (o wwz ganha 12 núcleos; ADR da task 269).
HEAVY_PROD_PAUSE_STREAMS="${HEAVY_PROD_PAUSE_STREAMS-wwz-admin}"
HEAVY_PAUSED_ROUND=""
HEAVY_STREAMS_WWZ_CPUS="${HEAVY_STREAMS_WWZ_CPUS:-12}"
HEAVY_STREAMS_LOAD_CPUS="${HEAVY_STREAMS_LOAD_CPUS:-14}"

HEAVY_STAMP=""
HEAVY_OUT=""
HEAVY_TAG=""
HEAVY_IMAGES=""
HEAVY_SYNCED=0
HEAVY_SETUP_FAILED=""

# hssh <comando remoto>: ssh não interativo no servidor pesado. As opções vão
# antes do destino ($WWZ_HEAVY_SSH = "ssh [opções] destino").
hssh() {
	local -a base
	if [ -z "$WWZ_HEAVY_SSH" ]; then
		echo "sem WWZ_HEAVY_SSH: defina no ambiente ou em autor/prod.env (modelo em deploy/prod/autor.env.example)" >&2
		return 255
	fi
	read -ra base <<<"$WWZ_HEAVY_SSH"
	"${base[0]}" -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30 "${base[@]:1}" "$@"
}

# heavy_q <args...>: argumentos citados para a linha de comando remota.
heavy_q() { printf '%q ' "$@"; }

# heavy_rounds: rodadas pedidas, na ordem canônica; sai 1 se houver nome inválido.
heavy_rounds() {
	local r ok=0 want=",${HEAVY_ROUNDS// /},"
	for r in ${HEAVY_ROUNDS//,/ }; do
		case " $HEAVY_ALL_ROUNDS " in *" $r "*) ;; *) ok=1 ;; esac
	done
	for r in $HEAVY_ALL_ROUNDS; do
		case "$want" in *",$r,"*) printf '%s\n' "$r" ;; esac
	done
	return "$ok"
}

# heavy_eval_load <round> <report.json> <stats.jsonl|""> <rc> [overhead.json]:
# imprime o resultado da rodada (JSON) com os valores medidos, os limiares e
# "pass". Na throughput o overhead vem do overhead.json (malha aberta).
heavy_eval_load() {
	local round="$1" report="$2" stats="$3" rc="$4" ohrep="${5:-}" st="[]"
	[ -s "$stats" ] && st="$(jq -s -c '.' "$stats" 2>/dev/null || echo '[]')"
	jq -n -c --arg round "$round" --argjson rc "$rc" --argjson st "$st" \
		--argjson min_rps "$HEAVY_MIN_RPS" --argjson max_itl "$HEAVY_MAX_ITL_P99_MS" \
		--argjson max_err "$HEAVY_MAX_ERROR_PCT" --argjson streams "$HEAVY_STREAMS" \
		--argjson wwz_cpus "$HEAVY_STREAMS_WWZ_CPUS" --argjson max_cpu_pct "$HEAVY_MAX_WWZ_CPU_PCT" \
		--argjson streams_max "$HEAVY_STREAMS_MAX" \
		--argjson max_oh "$HEAVY_MAX_OVERHEAD_MS" --argjson cfg_itl "$HEAVY_CONFIGURED_ITL_MS" \
		--arg max_mem "$HEAVY_MAX_MEM_PER_10K_MB" --argjson oh_rps "$HEAVY_OVERHEAD_RPS" \
		--argjson max_lag "$HEAVY_MAX_SCHEDULE_LAG_MS" --slurpfile o <(if [ -n "$ohrep" ] && [ -s "$ohrep" ]; then jq -c . "$ohrep" 2>/dev/null || echo null; else echo null; fi) \
		--slurpfile r <(if [ -s "$report" ]; then jq -c . "$report" 2>/dev/null || echo null; else echo null; fi) '
		def pick: if . == null then null else {p50, p90, p99, max, mean} end;
		# Amostras do wwz só enquanto o wwz-load está de pé (mesmo carimbo .t): a
		# amostra de antes da carga (~0 %) baixava a média (task 409). Sem .t
		# (ou sem amostra do load), usa todas.
		def busy($s): ([$s[] | select(.Name | test("-load(-|$)")) | .t | select(. != null)] | unique) as $lt
			| if ($lt | length) == 0 then $s
			else $s | map(select((.Name | test("-load(-|$)")) or ((.t as $t | $lt | index($t)) != null))) end;
		def cpu($s0): busy($s0) as $s | ($s | map(select(.Name | test("-(wwz|load)(-|$)")))
			| group_by(.Name | if test("-wwz") then "wwz" else "load" end)
			| map({key: (.[0].Name | if test("-wwz") then "wwz" else "load" end),
				value: (map(.CPUPerc | rtrimstr("%") | tonumber / 100)
					| {max_cores: (max * 100 | round / 100), mean_cores: ((add / length) * 100 | round / 100), samples: length})})
			| from_entries);
		def mib($s): ($s | capture("^(?<n>[0-9.]+)(?<u>[A-Za-z]*)$")) as $c
			| ({"B":1,"kB":1000,"KB":1000,"MB":1000000,"GB":1000000000,"KiB":1024,"MiB":1048576,"GiB":1073741824}[$c.u]) as $f
			| if $f == null then null else ($c.n | tonumber) * $f / 1048576 end;
		def mem($s): ($s | map(select(.Name | test("-(wwz|load)(-|$)")))
			| map({key: (.Name | if test("-wwz") then "wwz" else "load" end), value: (.MemUsage | split(" / ")[0])})
			| group_by(.key) | map(max_by(mib(.value) // 0)) | from_entries);
		($r[0]) as $rep
		| ($o[0]) as $ohr
		| ($rep.total // {}) as $t
		| (if ($t.requests // 0) > 0 then ($t.errors / $t.requests * 100) else null end) as $err
		| {
			round: $round,
			rc: $rc,
			requests: $t.requests, ok: $t.ok, errors: $t.errors,
			error_pct: (if $err == null then null else ($err * 1000 | round / 1000) end),
			errors_by_type: $t.errors_by_type,
			rps: $t.rps, ok_rps: $t.ok_rps,
			concurrency: $rep.config.concurrency, stream: $rep.config.stream, max_tokens: $rep.config.max_tokens,
			duration_ms: $rep.config.duration_ms, warmup_ms: $rep.config.warmup_ms,
			output_tokens_per_s: $t.output_tokens_per_s,
			ttft_ms: ($t.ttft_ms | pick), itl_ms: ($t.itl_ms | pick), e2e_ms: ($t.e2e_ms | pick),
			cpu: cpu($st), mem_last: mem($st),
			thresholds: (if $round == "throughput"
				then {min_ok_rps: $min_rps, max_error_pct: $max_err, overhead_rps: $oh_rps, max_overhead_schedule_lag_p99_ms: $max_lag}
				elif $round == "streams-max"
				then {streams: $streams_max, informative: true, wwz_cpus: $wwz_cpus}
				else {streams: $streams, max_itl_p99_ms: $max_itl, max_error_pct: $max_err, wwz_cpus: $wwz_cpus,
					max_wwz_cpu_pct: $max_cpu_pct} end)
		}
		| (if ($round == "streams" or $round == "streams-max") and .cpu.wwz != null
			then .wwz_cpu_util_mean = ((.cpu.wwz.mean_cores / $wwz_cpus) * 100 | round / 100) else . end)
		| (if $round == "throughput" then
			.overhead = (if $ohr == null then null else ($ohr.total // {}) as $u
				| {target_rps: $ohr.config.rps, requests: $u.requests, ok: $u.ok, errors: $u.errors, dropped: $u.dropped,
				   error_pct: (if ($u.requests // 0) > 0 then ($u.errors / $u.requests * 100 * 1000 | round / 1000) else null end),
				   rps: $u.rps, ok_rps: $u.ok_rps, e2e_ms: ($u.e2e_ms | pick), schedule_lag_ms: ($u.schedule_lag_ms | pick)} end)
			| .overhead_p99_ms = (if .overhead.e2e_ms.p99 == null then null else (.overhead.e2e_ms.p99 * 1000 | round / 1000) end)
			else . end)
		| (if ($round == "streams" or $round == "streams-max") and .itl_ms.p99 != null
			then .overhead_p99_ms = ((.itl_ms.p99 - $cfg_itl) * 1000 | round / 1000)
				| .ttft_overhead_p99_ms = .ttft_ms.p99 else . end)
		| (if ($round == "streams" or $round == "streams-max") and .mem_last.wwz != null and (.concurrency // 0) > 0
			then .mem_per_10k_streams_mb = ((mib(.mem_last.wwz) / .concurrency * 10000) * 10 | round / 10) else . end)
		| .thresholds.max_overhead_p99_ms = (if $round == "streams-max" or $round == "cluster" then null else $max_oh end)
		| .thresholds.max_mem_per_10k_mb = (if $max_mem == "" then null else ($max_mem | tonumber) end)
		| (if $round == "streams-max" then
			.informative = true
			| .chunks_per_core_s = (if .cpu.wwz != null and .cpu.wwz.mean_cores > 0 and .output_tokens_per_s != null
				then (.output_tokens_per_s / .cpu.wwz.mean_cores | round) else null end)
			else . end)
		| .checks = (if $rep == null then {report: false}
			elif $round == "throughput" then
				{ok_rps: ((.ok_rps // 0) >= $min_rps), error_pct: (.error_pct != null and .error_pct < $max_err),
				 overhead_p99: (.overhead_p99_ms != null and .overhead_p99_ms < $max_oh),
				 overhead_rate: (.overhead != null and (.overhead.ok_rps // 0) >= 0.95 * $oh_rps),
				 overhead_errors: (.overhead != null and .overhead.error_pct != null and .overhead.error_pct < $max_err),
				 overhead_lag: (.overhead != null and .overhead.schedule_lag_ms.p99 != null and .overhead.schedule_lag_ms.p99 < $max_lag)}
			elif $round == "streams-max" then
				{report: true}
			else
				{itl_p99: (.itl_ms != null and .itl_ms.p99 != null and .itl_ms.p99 <= $max_itl),
				 error_pct: (.error_pct != null and .error_pct < $max_err),
				 wwz_cpu: (.wwz_cpu_util_mean != null and .wwz_cpu_util_mean <= ($max_cpu_pct / 100)),
				 concurrency: ((.concurrency // 0) == $streams),
				 overhead_p99: (.overhead_p99_ms != null and .overhead_p99_ms < $max_oh)}
				+ (if $max_mem == "" then {}
					else {mem_per_10k: (.mem_per_10k_streams_mb != null and .mem_per_10k_streams_mb <= ($max_mem | tonumber))} end)
			end)
		| .pass = (if $round == "streams-max" then ($rep != null) else ($rc == 0 and ([.checks[]] | all)) end)'
}

# heavy_eval_cluster <limits.log> <rc>: resultado da rodada cluster (JSON).
heavy_eval_cluster() {
	local log="$1" rc="$2" passed=0 failed=0 skipped=0 cskip=0 final=""
	if [ -s "$log" ]; then
		passed="$(grep -c -- '^--- PASS: ' "$log")"
		failed="$(grep -c -- '^--- FAIL: ' "$log")"
		skipped="$(grep -c -- '^--- SKIP: ' "$log")"
		cskip="$(grep -c -- '^--- SKIP: TestValkeyCluster' "$log")"
		final="$(grep -E '^(PASS|FAIL)$' "$log" | tail -n1)"
	fi
	jq -n -c --argjson rc "$rc" --argjson p "$passed" --argjson f "$failed" --argjson s "$skipped" \
		--argjson cs "$cskip" --arg final "$final" '
		{round: "cluster", rc: $rc, tests_passed: $p, tests_failed: $f, tests_skipped: $s,
		 cluster_tests_skipped: $cs, result: $final,
		 checks: {exit: ($rc == 0), result: ($final == "PASS"), failed: ($f == 0), ran: ($p > 0), cluster_only_ran: ($cs == 0)}}
		| .pass = ([.checks[]] | all)'
}

# heavy_note <result.json>: uma linha para o resumo do passo.
heavy_note() {
	jq -r '
		def n($x): if $x == null then "-" else ($x | tostring) end;
		if .round == "cluster" then
			"\(.tests_passed) PASS, \(.tests_failed) FAIL, \(.tests_skipped) SKIP (\(n(.result)))"
		elif .round == "throughput" then
			"ok_rps \(n(.ok_rps)) (mín \(.thresholds.min_ok_rps)), erros \(n(.error_pct))%, e2e p99 \(n(.e2e_ms.p99)) ms na saturação, overhead p99 \(n(.overhead_p99_ms)) ms a \(n(.overhead.ok_rps)) req/s (lag p99 \(n(.overhead.schedule_lag_ms.p99)) ms), wwz \(n(.cpu.wwz.max_cores)) cpus"
		elif .round == "streams-max" then
			"informativa: \(n(.concurrency)) streams, \(n(.chunks_per_core_s)) chunks/s/núcleo, wwz \(n(.wwz_cpu_util_mean)), itl p99 \(n(.itl_ms.p99)) ms, ttft p99 \(n(.ttft_ms.p99)) ms, erros \(n(.error_pct))%"
		else
			"\(n(.concurrency)) streams, itl p99 \(n(.itl_ms.p99)) ms (máx \(.thresholds.max_itl_p99_ms)), ttft p99 \(n(.ttft_ms.p99)) ms, overhead itl p99 \(n(.overhead_p99_ms)) ms, mem/10k \(n(.mem_per_10k_streams_mb)) MiB, erros \(n(.error_pct))%, wwz \(n(.cpu.wwz.max_cores))/\(n(.thresholds.wwz_cpus)) cpus (média \(n(.wwz_cpu_util_mean)))"
		end' "$1" 2>/dev/null
}

heavy_step_preflight() {
	step_begin preflight
	local missing="" t gb remote
	for t in go docker jq gzip tar git; do
		command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
	done
	if [ -n "$missing" ]; then
		step_end fail "faltando:$missing"
		return
	fi
	if ! heavy_rounds >/dev/null; then
		step_end fail "HEAVY_ROUNDS=$HEAVY_ROUNDS inválido (use $HEAVY_ALL_ROUNDS)"
		return
	fi
	if ! docker info >/dev/null 2>&1; then
		step_end fail "Docker sem daemon (abra o Docker Desktop)"
		return
	fi
	gb="$(free_gb "$ROOT")"
	if gb_below "$gb" "$VERIFY_MIN_FREE_GB"; then
		docker builder prune -f >/dev/null 2>&1
		gb="$(free_gb "$ROOT")"
		if gb_below "$gb" "$VERIFY_MIN_FREE_GB"; then
			step_end fail "disco local: ${gb} GiB livres (mínimo VERIFY_MIN_FREE_GB=${VERIFY_MIN_FREE_GB})"
			return
		fi
	fi
	if [ -z "$WWZ_HEAVY_SSH" ]; then
		step_end fail "sem WWZ_HEAVY_SSH: defina no ambiente ou em autor/prod.env (WWZ_AUTHOR_ENV troca o caminho; modelo em deploy/prod/autor.env.example)"
		return
	fi
	# shellcheck disable=SC2016
	if ! remote="$(hssh 'cut -d" " -f1 /proc/loadavg; df -Pk /opt | awk '"'"'NR == 2 { printf "%.1f\n", $4 / 1048576 }'"'"'; command -v docker jq timeout | wc -l | tr -d " "; nproc' 2>&1)"; then
		step_end fail "ssh ao servidor falhou ($WWZ_HEAVY_SSH): $(printf '%s' "$remote" | tail -n1)"
		return
	fi
	local load rgb tools ncpu
	{
		read -r load
		read -r rgb
		read -r tools
		read -r ncpu
	} <<<"$remote"
	if [ "$tools" != 3 ]; then
		step_end fail "servidor sem docker, jq ou timeout"
		return
	fi
	if ! gb_below "$load" "$HEAVY_MAX_LOAD"; then
		step_end fail "load average do servidor $load ≥ $HEAVY_MAX_LOAD"
		return
	fi
	if gb_below "$rgb" "$HEAVY_MIN_REMOTE_GB"; then
		step_end fail "servidor: ${rgb} GiB livres em /opt (mínimo $HEAVY_MIN_REMOTE_GB)"
		return
	fi
	step_end ok "disco local ${gb} GiB; servidor load $load, ${rgb} GiB em /opt, $ncpu CPUs; rodadas $(heavy_rounds | tr '\n' ' ')"
}

# heavy_prod_filter: lê nomes de containers (um por linha) e imprime, numa linha,
# os que são instâncias de papel da produção.
heavy_prod_filter() {
	local n r out=""
	while read -r n; do
		for r in $HEAVY_PROD_ROLES; do
			[ "$n" = "$r" ] && out="$out${out:+ }$n"
		done
	done
	printf '%s\n' "$out"
}

# heavy_prod_compose <subcomando e serviços...>: docker compose do projeto wwz no servidor.
# Os WWZ_HOST_* de autor/prod.env (o compose de produção não tem padrão) vão no
# ambiente do comando; sem eles, valem os do .env do servidor.
heavy_prod_compose() {
	local k hosts=""
	for k in WWZ_HOST_INFERENCIA WWZ_HOST_ADMIN WWZ_HOST_PANEL WWZ_HOST_GRAFANA; do
		[ -z "${!k:-}" ] || hosts="$hosts$k=$(heavy_q "${!k}")"
	done
	hssh "cd $(heavy_q "$WWZ_PROD_DIR") && ${hosts}docker compose -p wwz --env-file .env --env-file images.env -f docker-compose.yml $*"
}

heavy_step_prod_pause() {
	step_begin prod-pause
	local running out
	if [ "$HEAVY_PROD_PAUSE" = 0 ]; then
		step_end skip "HEAVY_PROD_PAUSE=0"
		return
	fi
	if ! running="$(hssh 'docker ps --format "{{.Names}}" --filter name=^wwz-' 2>&1)"; then
		step_end fail "ssh/docker no servidor: $(printf '%s' "$running" | tail -n1)"
		return
	fi
	running="$(printf '%s\n' "$running" | heavy_prod_filter)"
	if [ -z "$running" ]; then
		step_end ok "produção sem instâncias de papel rodando"
		return
	fi
	# Registra antes de parar: o cleanup religa mesmo se a parada falhar pela metade.
	HEAVY_PAUSED="$running"
	if ! out="$(heavy_prod_compose stop "$running" 2>&1)"; then
		step_end fail "compose stop: $(printf '%s' "$out" | tail -n1)"
		return
	fi
	step_end ok "paradas (religadas no cleanup): $running"
}

# heavy_prod_pause_round <serviços>: para, só durante uma rodada, os serviços da
# produção que estiverem rodando; heavy_prod_resume_round os religa.
heavy_prod_pause_round() {
	local running out
	[ "$HEAVY_PROD_PAUSE" = 0 ] || [ -z "$1" ] && return 0
	running="$(hssh 'docker ps --format "{{.Names}}" --filter name=^wwz-' 2>/dev/null)" || return 1
	running="$(printf '%s\n' "$running" | HEAVY_PROD_ROLES="$1" heavy_prod_filter)"
	[ -n "$running" ] || return 0
	HEAVY_PAUSED_ROUND="$running"
	if ! out="$(heavy_prod_compose stop "$running" 2>&1)"; then
		echo "    compose stop $running: $(printf '%s' "$out" | tail -n1)"
		return 1
	fi
	echo "    produção parada durante a rodada: $running"
}

heavy_prod_resume_round() {
	local out
	[ -n "$HEAVY_PAUSED_ROUND" ] || return 0
	if ! out="$(heavy_prod_compose start "$HEAVY_PAUSED_ROUND" 2>&1)"; then
		printf 'produção (%s) não religada (rode scripts/prod-deploy.sh up): %s' "$HEAVY_PAUSED_ROUND" "$(printf '%s' "$out" | tail -n1)"
		return 1
	fi
	HEAVY_PAUSED_ROUND=""
}

# heavy_prod_resume: religa o que o prod-pause parou; imprime o problema, se houver.
heavy_prod_resume() {
	local out
	heavy_prod_resume_round || return 1
	[ -n "$HEAVY_PAUSED" ] || return 0
	if ! out="$(heavy_prod_compose start "$HEAVY_PAUSED" 2>&1)"; then
		printf 'produção não religada (rode scripts/prod-deploy.sh up): %s' "$(printf '%s' "$out" | tail -n1)"
		return 1
	fi
	HEAVY_PAUSED=""
}

heavy_step_images() {
	step_begin images
	local gb img
	gb="$(free_gb "$ROOT")"
	if gb_below "$gb" "$VERIFY_MIN_FREE_GB"; then
		step_end fail "disco local: ${gb} GiB livres antes do buildx"
		return
	fi
	HEAVY_IMAGES="white-wizard:$HEAVY_TAG white-wizard-load:$HEAVY_TAG"
	if ! docker buildx build --platform linux/amd64 --load --target wwz \
		--build-arg VERSION="$HEAVY_TAG" -t "white-wizard:$HEAVY_TAG" . ||
		! docker buildx build --platform linux/amd64 --load --target wwz-load \
			--build-arg VERSION="$HEAVY_TAG" -t "white-wizard-load:$HEAVY_TAG" .; then
		step_end fail "docker buildx linux/amd64"
		return
	fi
	# shellcheck disable=SC2086
	if ! docker save $HEAVY_IMAGES | gzip -1 | hssh docker load; then
		step_end fail "docker save | ssh docker load"
		return
	fi
	for img in $HEAVY_IMAGES; do
		docker image rm "$img" >/dev/null 2>&1
	done
	step_end ok "$HEAVY_IMAGES enviadas e removidas do Mac"
}

heavy_step_store_test() {
	step_begin store-test
	local bin="$ROOT/tmp/heavy/limits.test"
	mkdir -p "$ROOT/tmp/heavy"
	if ! CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o "$bin" ./internal/limits; then
		step_end fail "go test -c ./internal/limits (linux/amd64)"
		return
	fi
	if ! hssh "$(heavy_q mkdir -p "$WWZ_HEAVY_DIR/bin") && cat > $(heavy_q "$WWZ_HEAVY_DIR/bin/limits.test") && chmod 755 $(heavy_q "$WWZ_HEAVY_DIR/bin/limits.test")" <"$bin"; then
		step_end fail "cópia do limits.test para o servidor"
		return
	fi
	rm -f "$bin"
	step_end ok "$WWZ_HEAVY_DIR/bin/limits.test"
}

heavy_step_sync() {
	step_begin sync
	if [ -f deploy/heavy/.env ]; then
		echo "deploy/heavy/.env local é ignorado (o .env vive só no servidor)"
	fi
	if ! COPYFILE_DISABLE=1 tar -C deploy/heavy --exclude .env --exclude runs --exclude bin -czf - . |
		hssh "$(heavy_q mkdir -p "$WWZ_HEAVY_DIR") && tar -C $(heavy_q "$WWZ_HEAVY_DIR") --no-same-owner -xzf - && chmod 755 $(heavy_q "$WWZ_HEAVY_DIR/run.sh")"; then
		step_end fail "cópia de deploy/heavy/ para $WWZ_HEAVY_DIR"
		return
	fi
	HEAVY_SYNCED=1
	local env
	env="$(hssh "test -f $(heavy_q "$WWZ_HEAVY_DIR/.env") && echo com || echo sem")"
	step_end ok "deploy/heavy/ → $WWZ_HEAVY_DIR (.env remoto: ${env:-?}, intocado)"
}

# heavy_step_round <rodada>
heavy_step_round() {
	local r="$1" rc dir res
	step_begin "$r"
	dir="$HEAVY_OUT/$r"
	mkdir -p "$dir"
	case "$r" in streams | streams-max) heavy_prod_pause_round "$HEAVY_PROD_PAUSE_STREAMS" ;; esac
	hssh "cd $(heavy_q "$WWZ_HEAVY_DIR") && $(heavy_q env HEAVY_TAG="$HEAVY_TAG" HEAVY_RUN="$HEAVY_STAMP" \
		HEAVY_STREAMS="$HEAVY_STREAMS" HEAVY_OVERHEAD_RPS="$HEAVY_OVERHEAD_RPS" HEAVY_MAX_SCHEDULE_LAG_MS="$HEAVY_MAX_SCHEDULE_LAG_MS" HEAVY_STREAMS_MAX="$HEAVY_STREAMS_MAX" HEAVY_MAX_LOAD="$HEAVY_MAX_LOAD" \
		HEAVY_STREAMS_WWZ_CPUS="$HEAVY_STREAMS_WWZ_CPUS" HEAVY_STREAMS_LOAD_CPUS="$HEAVY_STREAMS_LOAD_CPUS" ./run.sh "$r")" \
		2>&1 | tee "$dir/run.log" | sed 's/^/    /'
	rc="${PIPESTATUS[0]}"
	local resume
	resume="$(heavy_prod_resume_round)" || echo "    $resume"
	if ! hssh "$(heavy_q tar -C "$WWZ_HEAVY_DIR/runs/$HEAVY_STAMP" -czf - "$r")" | tar -C "$HEAVY_OUT" -xzf - 2>/dev/null; then
		echo "    sem relatórios no servidor para $r"
	fi
	res="$dir/result.json"
	if [ "$r" = cluster ]; then
		heavy_eval_cluster "$dir/limits.log" "$rc" >"$res"
	else
		heavy_eval_load "$r" "$dir/report.json" "$dir/stats.jsonl" "$rc" "$dir/overhead.json" >"$res"
	fi
	if [ -s "$dir/overhead-1.json" ]; then
		# Primeira tentativa descartada pelo run.sh (cliente travou): fica registrada.
		jq -c --slurpfile f "$dir/overhead-1.json" \
			'.overhead_discarded = {schedule_lag_p99_ms: $f[0].total.schedule_lag_ms.p99, e2e_p99_ms: $f[0].total.e2e_ms.p99}' \
			"$res" >"$res.tmp" && mv "$res.tmp" "$res"
	fi
	if [ -f "$dir/meta.json" ]; then
		jq -c --slurpfile m "$dir/meta.json" '. + {meta: $m[0]}' "$res" >"$res.tmp" && mv "$res.tmp" "$res"
	fi
	case "$rc" in
	3) step_end fail "abortada: load average > $HEAVY_MAX_LOAD; $(heavy_note "$res")" ;;
	4) step_end fail "não começou: load average do servidor ≥ $HEAVY_MAX_LOAD" ;;
	124 | 137) step_end fail "timeout de 600 s; $(heavy_note "$res")" ;;
	*)
		if [ "$(jq -r .pass "$res" 2>/dev/null)" = true ]; then
			step_end ok "$(heavy_note "$res")"
		else
			step_end fail "rc $rc; $(heavy_note "$res"); checks $(jq -c .checks "$res" 2>/dev/null)"
		fi
		;;
	esac
}

heavy_step_collect() {
	step_begin collect
	local r rounds=() f summary="$HEAVY_OUT/summary.json"
	while read -r r; do rounds+=("$r"); done < <(heavy_rounds)
	local files=()
	for r in "${rounds[@]}"; do
		f="$HEAVY_OUT/$r/result.json"
		if [ -s "$f" ]; then
			files+=("$f")
		else
			jq -n -c --arg r "$r" '{round: $r, pass: false, skipped: true}' >"$HEAVY_OUT/$r.missing.json"
			files+=("$HEAVY_OUT/$r.missing.json")
		fi
	done
	if ! jq -s --arg stamp "$HEAVY_STAMP" --arg tag "$HEAVY_TAG" --arg commit "$(git rev-parse HEAD)" \
		--arg dirty "$(git status --porcelain --untracked-files=no | head -c1)" \
		--arg server "${WWZ_HEAVY_SSH##* }" --arg dir "$WWZ_HEAVY_DIR" \
		--argjson min_rps "$HEAVY_MIN_RPS" --argjson streams "$HEAVY_STREAMS" --argjson streams_max "$HEAVY_STREAMS_MAX" \
		--argjson max_cpu_pct "$HEAVY_MAX_WWZ_CPU_PCT" --argjson max_itl "$HEAVY_MAX_ITL_P99_MS" --argjson max_err "$HEAVY_MAX_ERROR_PCT" \
		--argjson max_load "$HEAVY_MAX_LOAD" --argjson wwz_cpus "$HEAVY_STREAMS_WWZ_CPUS" \
		--argjson load_cpus "$HEAVY_STREAMS_LOAD_CPUS" --argjson max_oh "$HEAVY_MAX_OVERHEAD_MS" \
		--argjson oh_rps "$HEAVY_OVERHEAD_RPS" --argjson max_lag "$HEAVY_MAX_SCHEDULE_LAG_MS" \
		--arg max_mem "$HEAVY_MAX_MEM_PER_10K_MB" '
		{stamp: $stamp, tag: $tag, commit: $commit, dirty: ($dirty != ""), server: $server, dir: $dir,
		 thresholds: {min_ok_rps: $min_rps, streams: $streams, streams_max: $streams_max, max_wwz_cpu_pct: $max_cpu_pct, max_itl_p99_ms: $max_itl,
			max_error_pct: $max_err, max_load_average: $max_load, round_timeout_s: 600,
			max_cpus: 32, max_memory_gb: 64, streams_wwz_cpus: $wwz_cpus, streams_load_cpus: $load_cpus,
			max_overhead_p99_ms: $max_oh, overhead_rps: $oh_rps, max_overhead_schedule_lag_p99_ms: $max_lag, max_mem_per_10k_mb: (if $max_mem == "" then null else ($max_mem | tonumber) end)},
		 rounds: (map({key: .round, value: .}) | from_entries),
		 pass: (length > 0 and all(.[]; .pass == true))}' "${files[@]}" >"$summary"; then
		step_end fail "não consegui gravar $summary"
		return
	fi
	rm -f "$HEAVY_OUT"/*.missing.json
	step_end ok "${summary#"$ROOT"/} (pass=$(jq -r .pass "$summary"))"
}

heavy_step_drift() {
	step_begin drift
	local keys="$HOME/.config/white-wizard/real-keys.env" v have=0 bin="$ROOT/tmp/heavy/wwz-drift" rc
	if [ "${HEAVY_DRIFT:-1}" = 0 ]; then
		step_end skip "HEAVY_DRIFT=0"
		return
	fi
	[ -f "$keys" ] && have=1
	for v in OPENAI_API_KEY ANTHROPIC_API_KEY OPENROUTER_API_KEY OPENCODE_API_KEY; do
		[ -n "${!v:-}" ] && have=1
	done
	if [ "$have" -eq 0 ]; then
		step_end skip "sem ~/.config/white-wizard/real-keys.env nem *_API_KEY"
		return
	fi
	if ! go build -o "$bin" ./cmd/wwz-drift; then
		step_end fail "go build ./cmd/wwz-drift"
		return
	fi
	"$bin" check --real --output json --out "$HEAVY_OUT/drift.json"
	rc=$?
	rm -f "$bin"
	case "$rc" in
	0) step_end ok "${HEAVY_OUT#"$ROOT"/}/drift.json" ;;
	3) step_end fail "divergência de forma (código 3); ${HEAVY_OUT#"$ROOT"/}/drift.json" ;;
	*) step_end fail "wwz-drift check saiu com $rc" ;;
	esac
}

# heavy_cleanup: passo 8; roda sempre (trap EXIT).
heavy_cleanup() {
	step_begin cleanup
	local problems="" out img left
	out="$(heavy_prod_resume)" || problems="$problems $out"
	if [ -n "$HEAVY_TAG" ] && docker_ok; then
		for img in white-wizard:$HEAVY_TAG white-wizard-load:$HEAVY_TAG; do
			docker image inspect "$img" >/dev/null 2>&1 && docker image rm "$img" >/dev/null 2>&1
		done
		docker builder prune -f --filter until=24h >/dev/null 2>&1
	fi
	if docker_ok; then
		left="$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^white-wizard[a-z-]*:heavy-' | tr '\n' ' ')"
		[ -n "$left" ] && problems="$problems imagens locais: $left"
	fi
	rm -f "$ROOT/tmp/heavy/limits.test" "$ROOT/tmp/heavy/wwz-drift"
	if [ "$HEAVY_SYNCED" -eq 1 ] || hssh "test -x $(heavy_q "$WWZ_HEAVY_DIR/run.sh")" 2>/dev/null; then
		if ! out="$(hssh "cd $(heavy_q "$WWZ_HEAVY_DIR") && $(heavy_q env HEAVY_TAG="$HEAVY_TAG" HEAVY_KEEP_IMAGES="$HEAVY_KEEP_IMAGES" ./run.sh cleanup)" 2>&1)"; then
			problems="$problems servidor: $(printf '%s' "$out" | tail -n1)"
		fi
	elif ! out="$(hssh 'docker ps -aq --filter name=^wwz-heavy | xargs -r docker rm -f >/dev/null; docker images --format "{{.Repository}}:{{.Tag}}" | grep -E "^white-wizard[a-z-]*:heavy-" | xargs -r docker image rm >/dev/null; docker ps -a --format "{{.Names}}" | grep -E "^wwz-heavy"; true' 2>&1)"; then
		problems="$problems servidor inalcançável"
	elif [ -n "$out" ]; then
		problems="$problems servidor: $out"
	fi
	if [ -n "$problems" ]; then
		step_end fail "sobrou:$problems"
	else
		step_end ok "servidor sem wwz-heavy-*; nenhuma imagem :heavy-* local; produção religada"
	fi
}

heavy_finish() {
	local rc=$?
	trap - EXIT INT TERM
	[ "$STEP_OPEN" -eq 1 ] && step_end fail "interrompido"
	heavy_cleanup
	print_summary heavy
	[ -n "$HEAVY_OUT" ] && printf 'Relatórios: %s\n' "${HEAVY_OUT#"$ROOT"/}"
	[ -n "$HEAVY_SETUP_FAILED" ] && printf 'Parou no passo %s.\n' "$HEAVY_SETUP_FAILED"
	[ "$FAILED" -ne 0 ] && exit 1
	exit "$rc"
}

verify_heavy() {
	docker_path
	HEAVY_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
	HEAVY_OUT="$ROOT/tmp/heavy/$HEAVY_STAMP"
	HEAVY_TAG="heavy-$(git rev-parse --short=7 HEAD)"
	mkdir -p "$HEAVY_OUT"
	trap heavy_finish EXIT
	trap 'exit 130' INT TERM
	printf 'verify heavy: %s → %s (%s); rodadas %s; HEAVY_MIN_RPS=%s HEAVY_STREAMS=%s HEAVY_MAX_ITL_P99_MS=%s\n' \
		"$HEAVY_TAG" "$WWZ_HEAVY_SSH" "$WWZ_HEAVY_DIR" "$HEAVY_ROUNDS" "$HEAVY_MIN_RPS" "$HEAVY_STREAMS" "$HEAVY_MAX_ITL_P99_MS"
	local s r
	# Preparação: para na primeira falha (as rodadas dependem dela).
	for s in preflight prod-pause images store-test sync; do
		if [ -n "$HEAVY_SETUP_FAILED" ]; then
			step_begin "$s"
			step_end skip "não rodou: $HEAVY_SETUP_FAILED falhou"
			continue
		fi
		"heavy_step_${s//-/_}"
		[ "$FAILED" -ne 0 ] && HEAVY_SETUP_FAILED="$s"
	done
	# Rodadas: independentes entre si; uma falha não impede as outras.
	for r in $HEAVY_ALL_ROUNDS; do
		if ! heavy_rounds 2>/dev/null | grep -qx "$r"; then
			step_begin "$r"
			step_end skip "fora de HEAVY_ROUNDS"
		elif [ -n "$HEAVY_SETUP_FAILED" ]; then
			step_begin "$r"
			step_end skip "não rodou: $HEAVY_SETUP_FAILED falhou"
		else
			heavy_step_round "$r"
		fi
	done
	heavy_step_collect
	heavy_step_drift
	[ "$FAILED" -eq 0 ]
}

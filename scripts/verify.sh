#!/usr/bin/env bash
# Verificação do White Wizard (specs/018-verificacao-ci-release.md, ADR 0008).
#
# Uso:
#   scripts/verify.sh quick [-- <pacotes> [flags do go test]]
#   scripts/verify.sh full
#   scripts/verify.sh heavy
#   scripts/verify.sh examples [nome...]
#   scripts/verify.sh selftest
#
# quick
#   Olha só o que mudou desde `git merge-base HEAD <base>` (base = $VERIFY_BASE ou
#   `main`): commits da branch, alterações no working tree e arquivos novos não
#   ignorados. Passos:
#     gofumpt   `gofumpt -l` nos .go alterados.
#     lint      golangci-lint nos pacotes alterados com --new-from-rev=<merge-base>:
#               só acusa problemas em linhas novas/alteradas, para não falhar por
#               dívida antiga (a reformatação do código herdado é a tarefa 006).
#               O verify full roda o lint completo.
#     build     `go build ./...` (rápido com cache; pega quebra em qualquer pacote).
#     vet       `go vet` dos pacotes afetados e dos que dependem deles.
#     test      `go test` dos mesmos pacotes (dependentes via `go list -deps`
#               invertido, incluindo imports de teste).
#     drivertest contrato dos drivers quando internal/driver/ ou internal/core/ mudou.
#     valkey    quando algum pacote a testar usa o store (é internal/limits, depende
#               dele direta ou transitivamente, inclusive por import de teste, ou
#               tem _test.go que lê WWZ_TEST_VALKEY, como cmd/wwz): confere o Valkey local
#               (127.0.0.1:$VALKEY_PORT, padrão 16379); se fora do ar, sobe só o
#               compose do Valkey (cache/valkey, com o .env da raiz; o compose raiz
#               exige WWZ_ADMIN_TOKEN) e o deixa no ar para as próximas rodadas
#               (pare com: docker compose --project-directory . -f
#               cache/valkey/docker-compose.yml down). Exporta WWZ_TEST_VALKEY=local,
#               WWZ_TEST_VALKEY_ADDR e WWZ_VERIFY_DBDIR; o test roda com `-exec scripts/valkey-db-exec.sh`,
#               que dá a cada binário de teste um Valkey próprio (valkey-server local ou container
#               efêmero; sem os dois, um banco 1..14 do compartilhado): FLUSHDB, SCRIPT FLUSH e o
#               canal pub/sub são do servidor, então pacotes em paralelo e verify de worktrees
#               concorrentes não se atrapalham (task 300).
#     grafana   scripts/check-grafana.sh (dashboards válidos, métricas wwz_* das
#               PromQL existentes no código, datasource) quando mudou algo em
#               deploy/grafana/, internal/metrics/, scripts/check-grafana.sh ou um
#               .go não teste de internal/ ou cmd/ cujo diff toca um nome wwz_*
#               (métricas do wwz, do painel e do wwz-tools); pulado caso contrário.
#   Mudança em go.mod/go.sum afeta todos os pacotes.
#   Alvos explícitos depois de `--` substituem o escopo de vet/test, ex.:
#     make verify-quick -- ./internal/scenario -run TestFailFirst
#   Roda todos os passos e sai ≠0 se algum falhar.
#   Quando o próprio scripts/verify.sh muda, roda também o passo selftest.
#
# selftest
#   Testes do próprio script: detecção dos pacotes que precisam do Valkey, escopo
#   (pacotes afetados), gatilho do passo grafana, as funções do full (gate de
#   cobertura, contagem do passo fixtures, "pulado" sem insumo, lista dos 16
#   examples, node_modules, disco, porta livre, venv e node_modules do checkout
#   principal em worktrees), a limpeza do full (tmp/release/*.tar e, com Docker,
#   falha forçada em images), as funções do release-dryrun.sh, o heavy_eval_load
#   (overhead p99 e memória por 10 000 streams) e um quick sem mudanças num
#   checkout limpo do HEAD (todo passo ✓ ou pulado, em segundos; WWZ_VERIFY_NESTED=1
#   evita a recursão).
#
# full (sob pedido; specs/018 "verify-full", ADR 0037)
#   Passos em ordem, parando no primeiro ✗ (os seguintes saem "pulado (não
#   rodou)"); cleanup e resumo rodam sempre, por trap EXIT, inclusive com ^C:
#     preflight  go, gofumpt, golangci-lint, docker (com daemon), node, npm, curl,
#                jq e $VERIFY_PYTHON com pytest (padrão tmp/venv/bin/python, depois o do
#                checkout principal; sem nenhum, falha com o comando de criação);
#                disco livre ≥ VERIFY_MIN_FREE_GB (padrão 1.5): abaixo, roda
#                go clean -testcache e docker builder prune -f e mede de novo.
#     gofumpt    gofumpt -l nos .go do repositório (versionados e novos).
#     lint       golangci-lint run ./... (completo, sem --new-from-rev).
#     vet/build  go vet ./... e go build ./...
#     valkey     docker run --rm -d --name wwz-verify-valkey-<pid> -p 127.0.0.1::6379
#                valkey/valkey:<VALKEY_VERSION do .env>-alpine; exporta
#                WWZ_TEST_VALKEY=local, WWZ_TEST_VALKEY_ADDR (porta efêmera) e
#                WWZ_TEST_VALKEY_DB=15.
#     test       go test -race -covermode=atomic -coverprofile=tmp/coverage/cover.out ./...
#                + go test -covermode=atomic só de internal/driver/drivertest (sem -race,
#                que pula o sweep de mutações); perfis fundidos (máximo por bloco).
#     coverage  statements por pacote de internal/ e cmd/; falha listando os
#                pacotes abaixo de VERIFY_COVERAGE_MIN (padrão 100, ADR 0013).
#                Relatórios: tmp/coverage/{summary.txt,cover.html,func.txt}.
#     fixtures   go test -v -run '^TestRealFixtures' ./... (pulado sem testdata/real/);
#                falha se nenhum TestRealFixtures* rodou.
#     workflows  scripts/lint-workflows.sh (pulado sem .github/workflows/ ou sem
#                Docker para o actionlint).
#     images     make image image-panel image-load com tag :verify.
#     smoke      scripts/smoke.sh com as imagens :verify, projeto
#                wwz-verify-smoke-<pid>; portas sorteadas em 20000-29999 (bind em
#                0.0.0.0 e 127.0.0.1) e, com "address already in use", até 3 tentativas.
#                VERIFY_ONLY="images smoke" roda só esses passos do full.
#     sdk-python bin/wwz serve --config tests/sdk/wwz.yaml em portas livres;
#                WWZ_SDK_BASE_URL/WWZ_SDK_KEY; $VERIFY_PYTHON -m pytest -q tests/sdk/python.
#     sdk-node   npm ci --prefer-offline só se node_modules falta ou o
#                node_modules/.package-lock.json difere do package-lock.json (conteúdo,
#                sem a entrada raiz ""; o mtime não conta); nunca através de um
#                node_modules em symlink (falha pedindo npm ci no destino);
#                node --test tests/sdk/node/.
#     examples   cada examples/<nome>/ (ignora _*) precisa de config.yaml, docker-compose.yml,
#                README.md e smoke.sh (falha listando o que falta, antes de subir qualquer um);
#                roda `wwz validate` no config.yaml e depois o smoke.sh, um por vez (pulado sem
#                examples/). `scripts/verify.sh examples [nome...]` roda só este passo.
#     cleanup    para o wwz do SDK e o Valkey, derruba o projeto do smoke, remove
#                as imagens :verify e roda docker builder prune -f --filter
#                until=24h; falha se sobrar container wwz-verify-*-<pid> ou imagem.
#   Ex.: VERIFY_COVERAGE_MIN=0 make verify-full (relatório sem gate, fase 4).
#   VERIFY_FAIL_AT=<passo> força ✗ naquele passo (teste do resumo e da limpeza).
#
# heavy
#   Benchmarks no servidor pesado (ADRs 0017 e 0037); passos e variáveis em
#   scripts/heavy.sh: preflight, prod-pause (para as instâncias de papel da produção
#   e as religa no cleanup), images (amd64 :heavy-<sha7> via docker save |
#   ssh docker load), store-test (limits.test linux/amd64), sync (deploy/heavy/,
#   sem o .env remoto), rodadas cluster/throughput/streams/streams-max (deploy/heavy/run.sh,
#   timeout 600 s, aborta com load average > 48), collect
#   (tmp/heavy/<carimbo>/summary.json), drift (local, pulado sem chaves) e
#   cleanup (sempre: nada wwz-heavy no servidor nem imagem :heavy-* local).
#   Ex.: HEAVY_ROUNDS=throughput HEAVY_MIN_RPS=20000 make verify-heavy

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

if [ -f .env ]; then
	VALKEY_PORT="${VALKEY_PORT:-$(sed -n 's/^VALKEY_PORT=//p' .env | tail -n1)}"
fi
VALKEY_PORT="${VALKEY_PORT:-16379}"

now_ms() {
	local t
	t="$(date +%s%3N 2>/dev/null)"
	case "$t" in
	*[!0-9]* | "") ;;
	*)
		echo "$t"
		return
		;;
	esac
	if command -v perl >/dev/null 2>&1; then
		perl -MTime::HiRes=time -e 'printf("%d\n", time()*1000)'
	else
		echo $(($(date +%s) * 1000))
	fi
}

SUMMARY=""
FAILED=0
STEP_NAME=""
STEP_START=0
STEP_OPEN=0

step_begin() {
	STEP_NAME="$1"
	STEP_OPEN=1
	STEP_START="$(now_ms)"
	printf '\n==> %s\n' "$STEP_NAME"
}

# step_end ok|fail|skip [nota]
step_end() {
	local status="$1" note="${2:-}" elapsed mark
	STEP_OPEN=0
	elapsed=$(($(now_ms) - STEP_START))
	case "$status" in
	ok) mark="✓     " ;;
	fail)
		mark="✗     "
		FAILED=1
		;;
	*) mark="pulado" ;; # marcas com 6 colunas visíveis (printf conta bytes)
	esac
	SUMMARY="${SUMMARY}$(printf '  %s  %-11s %6d.%01ds  %s' "$mark" "$STEP_NAME" $((elapsed / 1000)) $(((elapsed % 1000) / 100)) "$note")
"
	[ -n "$note" ] && printf '    %s\n' "$note"
}

# run_step <comando...>: roda e fecha o passo com ok/fail.
run_step() {
	if "$@"; then
		step_end ok
	else
		step_end fail
	fi
}

print_summary() {
	printf '\nResumo (%s):\n%s' "$1" "$SUMMARY"
	if [ "$FAILED" -ne 0 ]; then
		printf 'Resultado: ✗ falhou\n'
	else
		printf 'Resultado: ✓ ok\n'
	fi
}

need_tool() {
	if ! command -v "$1" >/dev/null 2>&1; then
		step_end fail "$1 não encontrado; instale com: $2"
		return 1
	fi
}

# ---------------------------------------------------------------------------
# Escopo
# ---------------------------------------------------------------------------

BASE_REF="${VERIFY_BASE:-main}"
MERGE_BASE=""
CHANGED_FILES=""

compute_scope() {
	local ref
	for ref in "$BASE_REF" "origin/$BASE_REF"; do
		if MERGE_BASE="$(git merge-base HEAD "$ref" 2>/dev/null)"; then
			break
		fi
		MERGE_BASE=""
	done
	if [ -z "$MERGE_BASE" ]; then
		echo "verify: não achei a base '$BASE_REF'; usando escopo completo." >&2
		CHANGED_FILES="$(git ls-files)"
		return
	fi
	CHANGED_FILES="$(
		{
			git diff --name-only "$MERGE_BASE" --
			git ls-files --others --exclude-standard
		} | sort -u
	)"
}

MODULE=""
# Uma linha por pacote do módulo: "importpath<TAB>deps (não teste, transitivas)<TAB>imports de teste".
pkg_graph() {
	go list -e -f '{{.ImportPath}}{{"\t"}}{{join .Deps " "}}{{"\t"}}{{join .TestImports " "}} {{join .XTestImports " "}}' ./...
}

# Pacotes afetados: os alterados, o fecho dos dependentes (pelos imports de
# produção) e os pacotes cujos *testes* importam um desses. O teste de um
# pacote não é importado por ninguém: ele afeta o próprio pacote, mas não
# propaga para quem o importa (specs/018, escopo do quick).
affected_packages() {
	local changed="$1"
	[ -z "$changed" ] && return
	pkg_graph | awk -F'\t' -v changed="$changed" '
		BEGIN { n = split(changed, c, " "); for (i = 1; i <= n; i++) aff[c[i]] = 1 }
		{ pkg[NR] = $1; deps[NR] = $2; tests[NR] = $3 }
		END {
			do {
				added = 0
				for (r = 1; r <= NR; r++) {
					if (pkg[r] in aff) continue
					m = split(deps[r], f, " ")
					for (j = 1; j <= m; j++) if (f[j] in aff) { aff[pkg[r]] = 1; added = 1; break }
				}
			} while (added)
			for (r = 1; r <= NR; r++) {
				hit = (pkg[r] in aff)
				if (!hit) { m = split(tests[r], f, " "); for (j = 1; j <= m; j++) if (f[j] in aff) { hit = 1; break } }
				if (hit) print pkg[r]
			}
		}'
}

# needs_valkey <pacotes ou padrões...>: sucesso se algum pacote precisa do Valkey
# de teste: é internal/limits, depende dele (Deps inclui transitivos; imports de
# teste diretos também contam) ou tem _test.go que lê WWZ_TEST_VALKEY.
needs_valkey() {
	[ "$#" -eq 0 ] && return 1
	local module limits deps pkg dir files f
	module="$(go list -m)"
	limits="$module/internal/limits"
	while IFS='|' read -r pkg dir deps files; do
		[ -z "$pkg" ] && continue
		case " $pkg $deps " in *" $limits "*) return 0 ;; esac
		for f in $files; do
			grep -q 'WWZ_TEST_VALKEY' "$dir/$f" 2>/dev/null && return 0
		done
	done <<EOF
$(go list -e -f '{{.ImportPath}}|{{.Dir}}|{{join .Deps " "}} {{join .TestImports " "}} {{join .XTestImports " "}}|{{join .TestGoFiles " "}} {{join .XTestGoFiles " "}}' "$@" 2>/dev/null)
EOF
	return 1
}

# grafana_relevant_file <caminho> <diff>: sai 0 se a mudança no arquivo pede o
# scripts/check-grafana.sh. <diff> é o texto alterado (diff ou conteúdo do
# arquivo novo); só conta para .go não teste de internal/ ou cmd/.
grafana_relevant_file() {
	case "$1" in
	deploy/grafana/* | internal/metrics/* | scripts/check-grafana.sh) return 0 ;;
	*_test.go) return 1 ;;
	internal/*.go | cmd/*.go) grep -qE 'wwz_[a-z0-9_]+' <<<"$2" ;;
	*) return 1 ;;
	esac
}

# file_change <caminho>: linhas alteradas desde $MERGE_BASE (inclui arquivo
# apagado); arquivo não versionado ou sem base sai inteiro.
file_change() {
	if [ -n "$MERGE_BASE" ] && { [ ! -f "$1" ] || git ls-files --error-unmatch -- "$1" >/dev/null 2>&1; }; then
		git diff -U0 "$MERGE_BASE" -- "$1" | grep -E '^[+-]' | grep -vE '^(\+\+\+|---) '
	elif [ -f "$1" ]; then
		cat "$1"
	fi
}

# grafana_relevant <arquivos...>: algum arquivo pede o check-grafana.
grafana_relevant() {
	local f
	for f in "$@"; do
		case "$f" in
		deploy/grafana/* | internal/metrics/* | scripts/check-grafana.sh) return 0 ;;
		internal/*.go | cmd/*.go)
			grafana_relevant_file "$f" "$(file_change "$f")" && return 0
			;;
		esac
	done
	return 1
}

# selftest: testes automatizados do próprio script.
selftest_expect() { # selftest_expect yes|no <pacote...>
	local want="$1" got=no
	shift
	needs_valkey "$@" && got=yes
	if [ "$got" = "$want" ]; then
		printf '  ok    needs_valkey %s = %s\n' "${*:-(nenhum)}" "$got"
	else
		printf '  FALHA needs_valkey %s = %s (esperado %s)\n' "${*:-(nenhum)}" "$got" "$want"
		SELFTEST_FAILS=$((SELFTEST_FAILS + 1))
	fi
}

verify_selftest() {
	SELFTEST_FAILS=0
	selftest_expect yes ./internal/limits
	selftest_expect yes ./cmd/wwz
	selftest_expect yes "$(go list -m)/cmd/wwz"
	selftest_expect yes ./internal/latency ./cmd/wwz
	selftest_expect yes ./...
	selftest_expect no ./internal/latency
	selftest_expect no ./internal/clientip
	selftest_expect no
	selftest_dbexec
	selftest_grafana
	selftest_full
	selftest_heavy
	selftest_examples
	selftest_scope
	selftest_inputs_missing
	selftest_cleanup
	selftest_release
	selftest_quick_clean
	[ "$SELFTEST_FAILS" -eq 0 ]
}

not() { ! "$@"; }
is_number() { case "$1" in "" | *[!0-9.]*) return 1 ;; esac; }
is_port() { is_number "$1" && [ "${1%.*}" = "$1" ] && [ "$1" -gt 1024 ] && [ "$1" -lt 65536 ]; }

# selftest_dbexec: no modo compartilhado o wrapper reserva bancos distintos, libera ao sair e rouba de dono morto.
selftest_dbexec() {
	local d w="$ROOT/scripts/valkey-db-exec.sh" a b
	d="$(mktemp -d)"
	export WWZ_TEST_VALKEY=local WWZ_VERIFY_DBDIR="$d/db" WWZ_VERIFY_SHARED=1
	a="$("$w" sh -c 'echo $WWZ_TEST_VALKEY_DB; "$0" sh -c "echo \$WWZ_TEST_VALKEY_DB"' "$w")"
	b="$(echo "$a" | tr '\n' ' ')"
	selftest_check "dbexec: dois simultâneos usam bancos diferentes ($b)" [ "$(echo "$a" | sort -u | wc -l | tr -d ' ')" = 2 ]
	selftest_check "dbexec: reserva liberada ao sair" [ -z "$(ls "$d/db")" ]
	mkdir -p "$d/db/1"
	echo 999999 >"$d/db/1/pid"
	selftest_check "dbexec: dono morto é roubado" [ "$("$w" sh -c 'echo $WWZ_TEST_VALKEY_DB')" = 1 ]
	selftest_check "dbexec: propaga o código de saída" test "$("$w" sh -c 'exit 3'; echo $?)" = 3
	unset WWZ_TEST_VALKEY WWZ_VERIFY_DBDIR WWZ_VERIFY_SHARED
	rm -rf "$d"
}

# selftest_check <descrição> <comando...>: o comando deve sair 0.
selftest_check() {
	local what="$1"
	shift
	if "$@"; then
		printf '  ok    %s\n' "$what"
	else
		printf '  FALHA %s\n' "$what"
		SELFTEST_FAILS=$((SELFTEST_FAILS + 1))
	fi
}

# Gatilho do passo grafana do quick.
selftest_grafana() {
	local def='+	calls: reg.NewCounter("wwz_tools_calls_total", "Tool calls", "tool")'
	selftest_check "grafana: dashboard alterado" grafana_relevant_file deploy/grafana/dashboards/wwz.json ""
	selftest_check "grafana: provisioning alterado" grafana_relevant_file deploy/grafana/provisioning/datasources/prometheus.yml ""
	selftest_check "grafana: internal/metrics alterado" grafana_relevant_file internal/metrics/metrics.go ""
	selftest_check "grafana: check-grafana.sh alterado" grafana_relevant_file scripts/check-grafana.sh ""
	selftest_check "grafana: métrica do wwz-tools no diff" grafana_relevant_file cmd/wwz-tools/serve.go "$def"
	selftest_check "grafana: métrica do painel removida" grafana_relevant_file internal/panel/source/prom.go '-	"wwz_panel_source_up"'
	selftest_check "grafana: .go sem wwz_ no diff" not grafana_relevant_file cmd/wwz-tools/serve.go '+	return nil'
	selftest_check "grafana: _test.go com wwz_ não conta" not grafana_relevant_file cmd/wwz-tools/serve_test.go "$def"
	selftest_check "grafana: .go fora de internal/ e cmd/" not grafana_relevant_file tests/x.go "$def"
	selftest_check "grafana: deploy/prometheus não conta" not grafana_relevant_file deploy/prometheus/prometheus.yml ""
	selftest_check "grafana: grafana_relevant sem arquivos" not grafana_relevant
	selftest_check "grafana: grafana_relevant com dashboard" grafana_relevant README.md deploy/grafana/dashboards/wwz.json
	selftest_check "grafana: grafana_relevant só com docs" not grafana_relevant README.md docs/uso.md
}

# fixtures_verdict_probe <rc> <n>: roda fixtures_verdict e imprime FAILED:nota.
fixtures_verdict_probe() {
	local note
	note="$(fixtures_verdict "$1" "$2" | sed -n 's/^    //p')"
	fixtures_verdict "$1" "$2" >/dev/null
	printf '%s:%s' "$FAILED" "$note"
}

# Funções do verify full: gate de cobertura, "pulado" sem insumo, node_modules,
# disco e porta livre.
selftest_full() {
	local d m="example.com/m" out rc
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	cat >"$d/cover.out" <<EOF
mode: atomic
$m/internal/a/a.go:1.1,2.2 3 1
$m/internal/a/a.go:3.1,4.2 1 0
$m/internal/a/a.go:3.1,4.2 1 5
$m/internal/b/b.go:1.1,2.2 1 0
$m/internal/b/b.go:3.1,4.2 3 2
$m/cmd/c/main.go:1.1,2.2 2 7
$m/other/o.go:1.1,2.2 9 0
EOF
	out="$(coverage_gate "$d/cover.out" "$m" 100 "$d/summary.txt")"
	rc=$?
	selftest_check "coverage_gate 100: falha listando só internal/b (75%)" [ "$rc:$out" = "1:75.00% $m/internal/b" ]
	selftest_check "coverage_gate: bloco repetido coberto conta uma vez (internal/a 100%)" \
		grep -Eq "100\.00%.*4/4.*$m/internal/a\$" "$d/summary.txt"
	selftest_check "coverage_gate: total 90% (9/10) no summary.txt" grep -Eq '90\.00%.*9/10.*total$' "$d/summary.txt"
	selftest_check "coverage_gate: pacote fora de internal/ e cmd/ não entra" not grep -q "$m/other" "$d/summary.txt"
	out="$(coverage_gate "$d/cover.out" "$m" 0 "$d/summary.txt")"
	rc=$?
	selftest_check "coverage_gate 0: passa" [ "$rc:$out" = "0:" ]
	out="$(coverage_gate "$d/cover.out" "$m" 75 "$d/summary.txt")"
	rc=$?
	selftest_check "coverage_gate 75: passa no limite" [ "$rc:$out" = "0:" ]
	cat >"$d/norace.out" <<EOF
mode: atomic
$m/internal/b/b.go:1.1,2.2 1 4
$m/internal/b/b.go:3.1,4.2 3 0
EOF
	merge_profiles "$d/merged.out" "$d/cover.out" "$d/norace.out"
	out="$(coverage_gate "$d/merged.out" "$m" 100 "$d/summary.txt")"
	rc=$?
	selftest_check "merge_profiles: máximo por bloco leva internal/b a 100%" [ "$rc:$out" = "0:" ]
	selftest_check "merge_profiles: um registro por bloco" [ "$(grep -c 'a.go:3.1,4.2' "$d/merged.out")" -eq 1 ]
	coverage_gate "$d/ausente.out" "$m" 0 "$d/summary.txt" >/dev/null
	rc=$?
	selftest_check "coverage_gate sem perfil: código 2" [ "$rc" -eq 2 ]

	cat >"$d/fixtures.log" <<'EOF2'
=== RUN   TestRealFixturesValidation
=== RUN   TestRealFixturesValidation/openai/x.json
=== PAUSE TestRealFixturesValidation/openai/x.json
--- PASS: TestRealFixturesValidation (0.01s)
=== RUN   TestRealFixturesStreaming
=== RUN   TestOutra
ok  	example.com/m/internal/server	0.1s
EOF2
	selftest_check "fixtures_count: 2 testes de topo, sem subtestes" [ "$(fixtures_count "$d/fixtures.log")" = 2 ]
	printf 'testing: warning: no tests to run\nPASS\n' >"$d/fixtures-vazio.log"
	selftest_check "fixtures_count: nenhum teste rodou" [ "$(fixtures_count "$d/fixtures-vazio.log")" = 0 ]
	# Em subshell: FAILED/SUMMARY do selftest não mudam.
	out="$(FAILED=0 SUMMARY= fixtures_verdict_probe 0 3)"
	selftest_check "fixtures_verdict: 3 testes passa ($out)" [ "$out" = "0:3 teste(s) TestRealFixtures*" ]
	out="$(FAILED=0 SUMMARY= fixtures_verdict_probe 0 0)"
	selftest_check "fixtures_verdict: 0 testes falha ($out)" [ "$out" = "1:nenhum teste TestRealFixtures* rodou com testdata/real/ presente" ]
	out="$(FAILED=0 SUMMARY= fixtures_verdict_probe 1 2)"
	selftest_check "fixtures_verdict: go test falhou ($out)" [ "$out" = "1:go test falhou (2 teste(s) TestRealFixtures*)" ]

	selftest_check "input_missing_reason: diretório ausente vira pulado" \
		[ "$(input_missing_reason "$d/examples" "fase 5")" = "$d/examples/ ausente (fase 5)" ]
	selftest_check "input_missing_reason: diretório existente roda" [ -z "$(input_missing_reason "$d" "fase 5")" ]
	selftest_check "input_missing_reason: caminho relativo à raiz" \
		[ "$(input_missing_reason "$ROOT/testdata/nao-existe" "fase 5")" = "testdata/nao-existe/ ausente (fase 5)" ]

	mkdir -p "$d/node"
	printf '{\n  "packages": {\n    "": {\n      "name": "x",\n      "dependencies": {\n        "a": "^1.0.0"\n      }\n    },\n    "node_modules/a": {\n      "version": "1.0.0"\n    }\n  }\n}\n' >"$d/node/package-lock.json"
	selftest_check "node_modules_stale: sem node_modules" node_modules_stale "$d/node"
	mkdir -p "$d/node/node_modules"
	printf '{\n  "packages": {\n    "node_modules/a": {\n      "version": "1.0.0"\n    }\n  }\n}\n' >"$d/node/node_modules/.package-lock.json"
	touch -t 202001010000 "$d/node/package-lock.json"
	selftest_check "node_modules_stale: em dia" not node_modules_stale "$d/node"
	touch -t 203001010000 "$d/node/package-lock.json"
	selftest_check "node_modules_stale: lock mais novo, mesmo conteúdo (checkout de worktree)" not node_modules_stale "$d/node"
	sed 's/"1.0.0"/"1.0.1"/' "$d/node/package-lock.json" >"$d/node/lock.tmp" && mv "$d/node/lock.tmp" "$d/node/package-lock.json"
	selftest_check "node_modules_stale: versão diferente no lock" node_modules_stale "$d/node"
	# Worktree com node_modules em symlink (tests/sdk/README.md): lock recém-saído do
	# checkout com o mesmo conteúdo fica em dia; desatualizado, recusa o npm ci e
	# não toca o destino.
	printf '#!/bin/sh\necho "$@" >"%s/npm-args"\n' "$d" >"$d/npm-falso"
	chmod +x "$d/npm-falso"
	mkdir -p "$d/nm-main/node_modules/a" "$d/nm-wt"
	touch "$d/nm-main/node_modules/a/index.js"
	cp "$d/node/node_modules/.package-lock.json" "$d/nm-main/node_modules/.package-lock.json"
	sed 's/"1.0.1"/"1.0.0"/' "$d/node/package-lock.json" >"$d/nm-wt/package-lock.json"
	touch -t 202001010000 "$d/nm-main/node_modules/.package-lock.json"
	ln -s "$d/nm-main/node_modules" "$d/nm-wt/node_modules"
	selftest_check "node_modules_stale: symlink com lock novo e mesmo conteúdo" not node_modules_stale "$d/nm-wt"
	sed 's/"1.0.0"/"2.0.0"/' "$d/nm-wt/package-lock.json" >"$d/nm-wt/lock.tmp" && mv "$d/nm-wt/lock.tmp" "$d/nm-wt/package-lock.json"
	selftest_check "node_modules_stale: symlink desatualizado" node_modules_stale "$d/nm-wt"
	out="$(VERIFY_NPM="$d/npm-falso" node_modules_install "$d/nm-wt" 2>&1; echo "rc=$?")"
	selftest_check "node_modules_install: symlink recusa com código 3" [ "${out##*rc=}" = 3 ]
	selftest_check "node_modules_install: symlink não chama o npm" [ ! -e "$d/npm-args" ]
	selftest_check "node_modules_install: destino do symlink intacto" [ -f "$d/nm-main/node_modules/a/index.js" ]
	out="$(VERIFY_NPM="$d/npm-falso" node_modules_install "$d/node" 2>&1; echo "rc=$?")"
	selftest_check "node_modules_install: diretório real roda o npm" [ "${out##*rc=}" = 0 ]
	selftest_check "node_modules_install: npm ci --prefix <dir>" grep -q "^ci .*--prefix $d/node\$" "$d/npm-args"

	# venv e node_modules do checkout principal (worktrees).
	mkdir -p "$d/a/bin" "$d/b/bin"
	printf '#!/bin/sh\n' >"$d/b/bin/python"
	chmod +x "$d/b/bin/python"
	selftest_check "pick_exec: o primeiro executável" [ "$(pick_exec "$d/a/bin/python" "$d/b/bin/python")" = "$d/b/bin/python" ]
	selftest_check "pick_exec: o do worktree vence" [ "$(pick_exec "$d/b/bin/python" "$d/a/bin/python")" = "$d/b/bin/python" ]
	selftest_check "pick_exec: nenhum cai no primeiro" [ "$(pick_exec "$d/a/bin/python" "$d/c/python")" = "$d/a/bin/python" ]
	selftest_check "main_checkout: raiz existente" [ -d "$MAIN_ROOT/.git" ]
	mkdir -p "$d/main/node/node_modules" "$d/wt/node"
	echo '{}' >"$d/main/node/package-lock.json"
	echo '{}' >"$d/wt/node/package-lock.json"
	touch -t 202001010000 "$d/main/node/package-lock.json" "$d/wt/node/package-lock.json"
	echo '{}' >"$d/main/node/node_modules/.package-lock.json"
	selftest_check "link_main_node_modules: liga o do principal" link_main_node_modules "$d/wt/node" "$d/main/node"
	selftest_check "link_main_node_modules: link aponta ao principal" [ -L "$d/wt/node/node_modules" ]
	selftest_check "link_main_node_modules: não religa se existe" not link_main_node_modules "$d/wt/node" "$d/main/node"
	rm -f "$d/wt/node/node_modules"
	echo '{"x":1}' >"$d/wt/node/package-lock.json"
	selftest_check "link_main_node_modules: lock diferente não liga" not link_main_node_modules "$d/wt/node" "$d/main/node"
	echo '{"x":1}' >"$d/main/node/package-lock.json"
	selftest_check "link_main_node_modules: principal desatualizado não liga" not link_main_node_modules "$d/wt/node" "$d/main/node"
	selftest_check "link_main_node_modules: sem node_modules no principal" not link_main_node_modules "$d/wt/node" "$d/nada"
	selftest_check "link_main_node_modules: o mesmo diretório não liga" not link_main_node_modules "$d/main/node" "$d/main/node"

	selftest_check "gb_below 1.2 < 1,5" gb_below 1.2 1,5
	selftest_check "gb_below 12.0 >= 1.5" not gb_below 12.0 1.5
	out="$(free_gb "$ROOT")"
	selftest_check "free_gb numérico ($out)" is_number "$out"
	out="$(free_port)"
	selftest_check "free_port > 1024 ($out)" is_port "$out"
	selftest_check "free_port fora da faixa efêmera ($out)" bash -c '[ "$1" -ge 20000 ] && [ "$1" -le 29999 ]' _ "$out"
	out="$(free_ports 5 | sort | uniq | wc -l | tr -d ' ')"
	selftest_check "free_ports: 5 portas distintas ($out)" [ "$out" = 5 ]
	# Porta ocupada em 0.0.0.0 não é devolvida.
	local held hpid
	held="$(free_port)"
	"${VERIFY_PYTHON:-python3}" -c 'import socket,sys,time; s=socket.socket(); s.bind(("0.0.0.0",int(sys.argv[1]))); s.listen(1); time.sleep(5)' "$held" &
	hpid=$!
	sleep 0.5
	out="$(free_port)"
	selftest_check "free_port não devolve porta ocupada em 0.0.0.0" [ "$out" != "$held" ]
	kill "$hpid" 2>/dev/null || true
	wait "$hpid" 2>/dev/null || true
	rm -rf "$d"
}

# Passo examples (ADR 0016): exemplos sintéticos em diretório temporário e um
# docker falso no PATH que registra as chamadas (nada sobe de verdade).
selftest_examples() {
	local d out rc mk
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	mkdir -p "$d/bin" "$d/ex/_lib-dir" "$d/ex/ok"
	printf '#!/bin/sh\necho "$*" >>"%s/docker.log"\nexit 0\n' "$d" >"$d/bin/docker"
	chmod +x "$d/bin/docker"
	mk() { # mk <nome> <corpo do smoke.sh> [arquivo a omitir]
		mkdir -p "$d/ex/$1"
		touch "$d/ex/$1/config.yaml" "$d/ex/$1/docker-compose.yml" "$d/ex/$1/README.md"
		printf '%s\n' "$2" >"$d/ex/$1/smoke.sh"
		[ -n "${3:-}" ] && rm -f "$d/ex/$1/$3"
		return 0
	}
	mk ok 'exit 0'
	selftest_check "examples_missing: exemplo completo" [ -z "$(examples_missing "$d/ex/ok")" ]
	mk sem-readme 'exit 0' README.md
	selftest_check "examples_missing: lista o que falta" [ "$(examples_missing "$d/ex/sem-readme")" = "README.md" ]
	rm -rf "$d/ex/sem-readme"

	out="$(EXAMPLES_EXPECTED=any EXAMPLES_VALIDATE=true EXAMPLES_DIR="$d/ex" step_examples 2>&1)"
	selftest_check "examples: exemplo válido passa e _* é ignorado" \
		bash -c '[[ "$1" == *"1 exemplo(s): ok"* ]]' _ "$out"
	mk falha 'exit 3'
	out="$(EXAMPLES_EXPECTED=any EXAMPLES_VALIDATE=true EXAMPLES_DIR="$d/ex" step_examples 2>&1)"
	selftest_check "examples: smoke falhando cita o exemplo" \
		bash -c '[[ "$1" == *"falha: smoke.sh falhou"* ]]' _ "$out"
	mk sem-readme 'exit 0' README.md
	out="$(EXAMPLES_EXPECTED=any EXAMPLES_VALIDATE=true EXAMPLES_DIR="$d/ex" step_examples 2>&1)"
	selftest_check "examples: arquivo faltando lista o que falta" \
		bash -c '[[ "$1" == *"sem-readme: falta README.md"* ]]' _ "$out"
	rm -rf "$d/ex/sem-readme"
	out="$(EXAMPLES_EXPECTED=any EXAMPLES_VALIDATE=false EXAMPLES_DIR="$d/ex" step_examples ok 2>&1)"
	selftest_check "examples: wwz validate recusando falha" \
		bash -c '[[ "$1" == *"ok: wwz validate recusou"* ]]' _ "$out"
	out="$(EXAMPLES_EXPECTED=any EXAMPLES_VALIDATE=true EXAMPLES_DIR="$d/ex" step_examples ok 2>&1)"
	selftest_check "examples: filtro por nome roda só o pedido" \
		bash -c '[[ "$1" == *"1 exemplo(s): ok"* ]]' _ "$out"
	out="$(EXAMPLES_EXPECTED=any EXAMPLES_DIR="$d/nao-existe" step_examples 2>&1)"
	selftest_check "examples: diretório ausente é pulado" \
		bash -c '[[ "$1" == *"ausente (fase 5)"* ]]' _ "$out"

	mkdir -p "$d/vazio/_lib-dir"
	out="$(EXAMPLES_EXPECTED=any EXAMPLES_VALIDATE=true EXAMPLES_DIR="$d/vazio" step_examples 2>&1)"
	selftest_check "examples: só _* e arquivos soltos é pulado" \
		bash -c '[[ "$1" == *"sem nenhum exemplo"* ]]' _ "$out"

	# _lib.sh: falha forçada no smoke ainda derruba o compose.
	printf '%s\n' '#!/usr/bin/env bash' \
		"export PATH=\"$d/bin:\$PATH\" WWZ_IMAGE=x" \
		"source \"$ROOT/examples/_lib.sh\"" \
		'ex_init ok 0' 'ex_up' 'ex_fail "falha forçada"' >"$d/ex/ok/smoke.sh"
	: >"$d/docker.log"
	bash "$d/ex/ok/smoke.sh" >/dev/null 2>&1
	rc=$?
	selftest_check "_lib.sh: smoke que falha sai 1" [ "$rc" -eq 1 ]
	selftest_check "_lib.sh: down -v --remove-orphans após a falha" \
		grep -q -- 'compose -p wwz-ex-ok .* down -v --remove-orphans' "$d/docker.log"
	selftest_check "_lib.sh: ex_assert_between aceita o limite" \
		bash -c 'source "$1"; ex_assert_between 5 5 5 x >/dev/null' _ "$ROOT/examples/_lib.sh"
	selftest_check "_lib.sh: ex_assert_between recusa fora da faixa" \
		not bash -c 'source "$1"; ex_assert_between 6 1 5 x >/dev/null 2>&1' _ "$ROOT/examples/_lib.sh"
	selftest_check "_lib.sh: ex_count_codes monta o histograma" \
		bash -c 'source "$1"; k=0; fake() { k=$((k+1)); if [ $((k % 4)) -eq 0 ]; then EX_HTTP_CODE=429; else EX_HTTP_CODE=200; fi; }; ex_count_codes 8 fake >/dev/null; [ "$(ex_hist 200)/$(ex_hist 429)/$(ex_hist 500)" = 6/2/0 ]' _ "$ROOT/examples/_lib.sh"
	selftest_check "_lib.sh: bash -n" bash -n "$ROOT/examples/_lib.sh"
	if command -v shellcheck >/dev/null 2>&1; then
		selftest_check "_lib.sh: shellcheck" shellcheck -x -s bash "$ROOT/examples/_lib.sh"
	fi
	rm -rf "$d"
}

# Funções puras do verify heavy: rodadas pedidas e avaliação contra os limiares.
# selftest_scope: o escopo do quick pelo grafo de imports (spec 018).
selftest_scope() {
	local out m
	m="$(go list -m)"
	out="$(affected_packages "$m/internal/scenario")"
	selftest_check "scope: internal/scenario entra" grep -qx "$m/internal/scenario" <<<"$out"
	selftest_check "scope: dependente (cmd/wwz) entra" grep -qx "$m/cmd/wwz" <<<"$out"
	selftest_check "scope: internal/latency fica de fora" not grep -qx "$m/internal/latency" <<<"$out"
	selftest_check "scope: sem alterações, nenhum pacote" [ -z "$(affected_packages "")" ]
}

# selftest_inputs_missing: sem testdata/real/ e sem examples/ os passos saem
# "pulado" e não reprovam (ADR 0037); a lista de examples vale a da ADR 0016.
selftest_inputs_missing() {
	local d out n
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	mkdir -p "$d/raiz" "$d/ex-vazio/_lib-dir"
	out="$(ROOT="$d/raiz"; SUMMARY=""; FAILED=0; step_fixtures >/dev/null 2>&1; printf '%s|%s' "$FAILED" "$SUMMARY")"
	selftest_check "fixtures sem testdata/real: FAILED=0 e pulado" bash -c '[[ "$1" == 0\|*pulado*fixtures* ]]' _ "$out"
	out="$(ROOT="$d/raiz"; EXAMPLES_DIR="$d/raiz/examples"; SUMMARY=""; FAILED=0; step_examples >/dev/null 2>&1; printf '%s|%s' "$FAILED" "$SUMMARY")"
	selftest_check "examples ausente: FAILED=0 e pulado" bash -c '[[ "$1" == 0\|*pulado*examples* ]]' _ "$out"
	out="$(ROOT="$d/raiz"; EXAMPLES_DIR="$d/ex-vazio"; SUMMARY=""; FAILED=0; step_examples >/dev/null 2>&1; printf '%s|%s' "$FAILED" "$SUMMARY")"
	selftest_check "examples vazio (só _*): FAILED=0 e pulado" bash -c '[[ "$1" == 0\|*pulado*examples* ]]' _ "$out"

	# ADR 0016: 18 exemplos, os da lista.
	selftest_check "examples: a ADR 0016 lista 18 nomes" [ "$(printf '%s\n' $EXAMPLES_ADR0016 | grep -c .)" = 18 ]
	selftest_check "examples: examples/ do repositório é a lista da ADR 0016" examples_list_check "$ROOT/examples"
	mkdir -p "$d/ex18/_lib"
	for n in $EXAMPLES_ADR0016; do mkdir -p "$d/ex18/$n"; done
	selftest_check "examples: 18 da ADR passam (e _* é ignorado)" examples_list_check "$d/ex18"
	rmdir "$d/ex18/tool-calls-quebradas"
	out="$(examples_list_check "$d/ex18")"
	selftest_check "examples: 17 reprova e cita o que falta ($out)" bash -c '[[ "$1" == "17 exemplo(s); a ADR 0016 manda 18; faltam: tool-calls-quebradas" ]]' _ "$out"
	mkdir -p "$d/ex18/tool-calls-quebradas" "$d/ex18/extra"
	out="$(examples_list_check "$d/ex18")"
	selftest_check "examples: 19 reprova e cita o excedente ($out)" bash -c '[[ "$1" == "19 exemplo(s); a ADR 0016 manda 18; fora da ADR 0016: extra" ]]' _ "$out"
	rmdir "$d/ex18/extra"
	mv "$d/ex18/openai-perfeito" "$d/ex18/openai-outro"
	selftest_check "examples: 18 com nome diferente da ADR reprova" not examples_list_check "$d/ex18" >/dev/null
	out="$(ROOT="$d/raiz"; EXAMPLES_DIR="$d/ex18"; SUMMARY=""; FAILED=0; step_examples >/dev/null 2>&1; printf '%s|%s' "$FAILED" "$SUMMARY")"
	selftest_check "step_examples: lista fora da ADR 0016 falha antes de subir qualquer exemplo" bash -c '[[ "$1" == 1\|*✗*fora\ da\ ADR\ 0016* ]]' _ "$out"
	rm -rf "$d"
}

# selftest_cleanup: o cleanup do full confere tmp/release/*.tar (sem Docker) e
# deixa o disco limpo depois de uma falha forçada (com Docker; pula sem ele).
selftest_cleanup() {
	local d out rc
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	mkdir -p "$d/tmp/release"
	out="$(ROOT="$d"; docker_ok() { return 1; }; SUMMARY=""; FAILED=0; full_cleanup >/dev/null 2>&1; printf '%s|%s' "$FAILED" "$SUMMARY")"
	selftest_check "full_cleanup: sem tar em tmp/release passa" bash -c '[[ "$1" == 0\|* ]]' _ "$out"
	: >"$d/tmp/release/wwz.tar"
	out="$(ROOT="$d"; docker_ok() { return 1; }; SUMMARY=""; FAILED=0; full_cleanup >/dev/null 2>&1; printf '%s|%s' "$FAILED" "$SUMMARY")"
	selftest_check "full_cleanup: tar sobrando em tmp/release reprova" bash -c '[[ "$1" == 1\|*sobrou:*wwz.tar* ]]' _ "$out"
	rm -rf "$d"

	docker_path
	if ! docker_ok; then
		printf '  pulado full: disco limpo após falha forçada (sem Docker)\n'
		return
	fi
	if [ -n "${WWZ_VERIFY_NESTED:-}" ] || [ -n "$(docker ps -aq --filter name=wwz-verify-)" ] ||
		docker image inspect $VERIFY_IMAGES >/dev/null 2>&1; then
		# Outra execução do full pode estar usando as imagens e os containers.
		printf '  pulado full: disco limpo após falha forçada (há containers/imagens wwz-verify em uso)\n'
		return
	fi
	out="$(WWZ_VERIFY_NESTED=1 VERIFY_FAIL_AT=images VERIFY_ONLY="images smoke" "$ROOT/scripts/verify.sh" full 2>&1)"
	rc=$?
	selftest_check "full com falha forçada em images: sai 1" [ "$rc" -eq 1 ]
	selftest_check "full com falha forçada: images ✗, smoke pulado, cleanup ✓" \
		bash -c 'grep -Eq "^  ✗ +images " <<<"$1" && grep -Eq "^  pulado +smoke " <<<"$1" && grep -Eq "^  ✓ +cleanup " <<<"$1"' _ "$out"
	selftest_check "full com falha forçada: nenhum container wwz-verify-* sobrou" [ -z "$(docker ps -aq --filter name=wwz-verify-)" ]
	selftest_check "full com falha forçada: nenhuma imagem :verify sobrou" not docker image inspect $VERIFY_IMAGES >/dev/null 2>&1
	selftest_check "full com falha forçada: nenhum tar em tmp/release" [ -z "$(ls "$ROOT"/tmp/release/*.tar 2>/dev/null)" ]
}

# selftest_release: funções do scripts/release-dryrun.sh (RELEASE_SOURCE_ONLY=1).
selftest_release() {
	local d out wf="$ROOT/.github/workflows/release.yml" m t
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	m="$(
		RELEASE_SOURCE_ONLY=1
		export RELEASE_SOURCE_ONLY
		# shellcheck source=scripts/release-dryrun.sh
		. "$ROOT/scripts/release-dryrun.sh"
		declare -f release_platforms dockerfile_final_base final_base_static binary_static_ok
	)"
	eval "$m"
	selftest_check "release: PLATFORMS lido do release.yml" [ "$(release_platforms "$wf")" = "linux/amd64,linux/arm64" ]
	printf 'env:\n  PLATFORMS: linux/amd64\n' >"$d/a.yml"
	selftest_check "release: PLATFORMS do arquivo dado" [ "$(release_platforms "$d/a.yml")" = linux/amd64 ]
	printf 'env:\n  X: 1\n' >"$d/b.yml"
	selftest_check "release: sem PLATFORMS devolve vazio (o dry-run falha)" [ -z "$(release_platforms "$d/b.yml")" ]
	selftest_check "release: arquivo ausente devolve vazio" [ -z "$(release_platforms "$d/nao-existe.yml")" ]
	for t in wwz wwz-panel wwz-load; do
		out="$(dockerfile_final_base "$ROOT/Dockerfile" "$t")"
		selftest_check "release: estágio final $t estático ($out)" final_base_static "$out"
	done
	printf 'FROM golang AS build\nFROM alpine:3 AS wwz\nFROM scratch AS wwz-load\nFROM --platform=$BUILDPLATFORM gcr.io/distroless/base AS wwz-panel\n' >"$d/Dockerfile"
	selftest_check "release: alpine não é estático" not final_base_static "$(dockerfile_final_base "$d/Dockerfile" wwz)"
	selftest_check "release: scratch é estático" final_base_static "$(dockerfile_final_base "$d/Dockerfile" wwz-load)"
	selftest_check "release: distroless/base não é estático" not final_base_static "$(dockerfile_final_base "$d/Dockerfile" wwz-panel)"
	selftest_check "release: estágio inexistente devolve vazio e reprova" not final_base_static "$(dockerfile_final_base "$d/Dockerfile" outro)"
	mkdir -p "$d/prog"
	printf 'package main\n\nfunc main() {}\n' >"$d/prog/main.go"
	if CGO_ENABLED=0 go build -o "$d/estatico" "$d/prog/main.go" 2>/dev/null; then
		selftest_check "release: binário com CGO_ENABLED=0 passa" binary_static_ok "$d/estatico"
		if CGO_ENABLED=1 go build -o "$d/dinamico" "$d/prog/main.go" 2>/dev/null; then
			selftest_check "release: binário com CGO_ENABLED=1 reprova" not binary_static_ok "$d/dinamico"
		fi
	else
		SELFTEST_FAILS=$((SELFTEST_FAILS + 1))
		printf '  FALHA release: go build do binário de teste\n'
	fi
	selftest_check "release: arquivo que não é binário Go reprova" not binary_static_ok "$d/a.yml"
	rm -rf "$d"
}

# selftest_quick_clean: verify-quick numa árvore sem mudanças (checkout limpo do
# HEAD, VERIFY_BASE=HEAD): todo passo sai ✓ ou "pulado", em segundos.
selftest_quick_clean() {
	local d out rc t0 ms max="${VERIFY_QUICK_CLEAN_MAX_S:-180}" bad n
	if [ -n "${WWZ_VERIFY_NESTED:-}" ]; then
		return
	fi
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	if ! git -C "$ROOT" worktree add -q --detach "$d/wt" HEAD >/dev/null 2>&1; then
		printf '  pulado quick sem mudanças: git worktree add falhou\n'
		rm -rf "$d"
		return
	fi
	t0="$(now_ms)"
	out="$(cd "$d/wt" && WWZ_VERIFY_NESTED=1 VERIFY_BASE=HEAD scripts/verify.sh quick 2>&1)"
	rc=$?
	ms=$(($(now_ms) - t0))
	git -C "$ROOT" worktree remove --force "$d/wt" >/dev/null 2>&1
	git -C "$ROOT" worktree prune >/dev/null 2>&1
	rm -rf "$d"
	bad="$(printf '%s\n' "$out" | sed -n '/^Resumo/,$p' | grep -E '^  [^ ]' | grep -vE '^  (✓|pulado) ' || true)"
	n="$(printf '%s\n' "$out" | sed -n '/^Resumo/,$p' | grep -cE '^  (✓|pulado) ' || true)"
	selftest_check "quick sem mudanças: sai 0" [ "$rc" -eq 0 ]
	selftest_check "quick sem mudanças: nenhum passo ✗ ou fora de ✓/pulado (${bad:-nenhum})" [ -z "$bad" ]
	selftest_check "quick sem mudanças: $n passos no resumo" [ "$n" -ge 8 ]
	selftest_check "quick sem mudanças: nenhum arquivo alterado" grep -q 'arquivos alterados: 0' <<<"$out"
	selftest_check "quick sem mudanças: $((ms / 1000)) s (máximo ${max} s)" [ "$ms" -lt $((max * 1000)) ]
}

selftest_heavy() {
	local d res
	d="$(mktemp -d "${TMPDIR:-/tmp}/wwz-verify-selftest.XXXXXX")"
	selftest_check "heavy_rounds: padrão nas 4 rodadas" [ "$(heavy_rounds | tr '\n' ,)" = "cluster,throughput,streams,streams-max," ]
	selftest_check "heavy_rounds: streams-max isolada" [ "$(HEAVY_ROUNDS=streams-max,streams heavy_rounds | tr '\n' ,)" = "streams,streams-max," ]
	selftest_check "heavy_rounds: ordem canônica" [ "$(HEAVY_ROUNDS=streams,cluster heavy_rounds | tr '\n' ,)" = "cluster,streams," ]
	selftest_check "heavy_prod_filter: só instâncias de papel da produção" \
		[ "$(printf '%s\n' wwz-admin wwz-anthropic wwz-valkey wwz-opencode wwz-heavy-wwz | heavy_prod_filter)" = "wwz-anthropic wwz-opencode" ]
	selftest_check "heavy_rounds: nome inválido" not eval 'HEAVY_ROUNDS=cluster,xyz heavy_rounds >/dev/null'

	# Malha fechada na saturação: e2e p99 de 54,9 ms (fila de 512 conexões) não reprova
	# o overhead, que vem do oh.json (malha aberta a HEAVY_OVERHEAD_RPS, task 409).
	cat >"$d/tp.json" <<'EOF'
{"config":{"concurrency":512,"stream":false,"max_tokens":256,"duration_ms":60000,"warmup_ms":10000},
 "total":{"requests":1000000,"ok":999500,"errors":500,"rps":16666.6,"ok_rps":16658.3,"errors_by_type":{"http_5xx":500},
  "ttft_ms":{"p50":2.9,"p90":28,"p99":54.911,"max":167,"mean":9.9},"itl_ms":{"count":0},"e2e_ms":{"p50":2.9,"p90":28,"p99":54.911,"max":167,"mean":9.9}}}
EOF
	cat >"$d/oh.json" <<'EOF'
{"config":{"rps":10000,"concurrency":0,"stream":false},
 "total":{"requests":300000,"ok":300000,"errors":0,"dropped":0,"rps":9999.8,"ok_rps":9999.8,
  "ttft_ms":{"p50":0.3,"p90":0.6,"p99":1.5,"max":9,"mean":0.4},"e2e_ms":{"p50":0.3,"p90":0.6,"p99":1.5,"max":9,"mean":0.4},
  "schedule_lag_ms":{"p50":0.01,"p90":0.02,"p99":0.1,"max":2,"mean":0.01}}}
EOF
	printf '%s\n' '{"Name":"wwz-heavy-wwz","CPUPerc":"700.00%","MemUsage":"100MiB / 16GiB"}' \
		'{"Name":"wwz-heavy-wwz","CPUPerc":"500.00%","MemUsage":"120MiB / 16GiB"}' \
		'{"Name":"wwz-heavy-load-run-1a2b","CPUPerc":"300.00%","MemUsage":"50MiB / 16GiB"}' >"$d/stats.jsonl"
	res="$(HEAVY_MIN_RPS=10000 heavy_eval_load throughput "$d/tp.json" "$d/stats.jsonl" 0 "$d/oh.json")"
	selftest_check "heavy_eval_load throughput: passa (16658 req/s, 0,05 % de erros, overhead da malha aberta)" [ "$(jq -r .pass <<<"$res")" = true ]
	selftest_check "heavy_eval_load: CPU em núcleos (wwz máx 7 e média 6; load 3)" \
		[ "$(jq -r '"\(.cpu.wwz.max_cores) \(.cpu.wwz.mean_cores) \(.cpu.load.max_cores)"' <<<"$res")" = "7 6 3" ]
	selftest_check "heavy_eval_load throughput: overhead p99 = e2e p99 da malha aberta (1,5 ms < 2), não o da saturação" \
		[ "$(jq -c '[.checks.overhead_p99, .overhead_p99_ms, .overhead.ok_rps, .overhead.schedule_lag_ms.p99, .thresholds.overhead_rps]' <<<"$res")" = "[true,1.5,9999.8,0.1,10000]" ]
	res="$(heavy_eval_load throughput "$d/tp.json" "" 0)"
	selftest_check "heavy_eval_load throughput: sem overhead.json reprova (não cai no e2e da saturação)" \
		[ "$(jq -c '[.pass, .checks.overhead_p99, .checks.overhead_rate, .overhead_p99_ms]' <<<"$res")" = "[false,false,false,null]" ]
	res="$(HEAVY_MAX_OVERHEAD_MS=1.5 heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh.json")"
	selftest_check "heavy_eval_load throughput: overhead igual ao limite reprova (desigualdade estrita)" \
		[ "$(jq -c '[.pass, .checks.overhead_p99]' <<<"$res")" = "[false,false]" ]
	jq '.total.e2e_ms.p99 = 3' "$d/oh.json" >"$d/oh-lento.json"
	res="$(heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh-lento.json")"
	selftest_check "heavy_eval_load throughput: overhead p99 3 ms reprova" \
		[ "$(jq -c '[.pass, .checks.overhead_p99, .checks.ok_rps]' <<<"$res")" = "[false,false,true]" ]
	jq '.total.ok_rps = 9000' "$d/oh.json" >"$d/oh-lag.json"
	res="$(heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh-lag.json")"
	selftest_check "heavy_eval_load throughput: malha aberta abaixo de 95 % da taxa pedida reprova" \
		[ "$(jq -c '[.pass, .checks.overhead_rate, .checks.overhead_p99]' <<<"$res")" = "[false,false,true]" ]
	jq '.total.schedule_lag_ms.p99 = 24.9 | .total.e2e_ms.p99 = 58.7' "$d/oh.json" >"$d/oh-trava.json"
	res="$(heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh-trava.json")"
	selftest_check "heavy_eval_load throughput: cliente travado (schedule_lag p99 24,9 ms) reprova pela medida, não só pelo overhead" \
		[ "$(jq -c '[.pass, .checks.overhead_lag, .thresholds.max_overhead_schedule_lag_p99_ms]' <<<"$res")" = "[false,false,1]" ]
	jq '.total.errors = 3000' "$d/oh.json" >"$d/oh-err.json"
	res="$(heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh-err.json")"
	selftest_check "heavy_eval_load throughput: erros ≥ limite na malha aberta reprova" \
		[ "$(jq -c '[.pass, .checks.overhead_errors, .overhead.error_pct]' <<<"$res")" = "[false,false,1]" ]
	res="$(HEAVY_MIN_RPS=20000 heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh.json")"
	selftest_check "heavy_eval_load throughput: abaixo de HEAVY_MIN_RPS falha" [ "$(jq -c '[.pass, .checks.ok_rps]' <<<"$res")" = "[false,false]" ]
	res="$(HEAVY_MAX_ERROR_PCT=0.01 heavy_eval_load throughput "$d/tp.json" "" 0 "$d/oh.json")"
	selftest_check "heavy_eval_load throughput: erros ≥ limite falha" [ "$(jq -r .checks.error_pct <<<"$res")" = false ]
	res="$(heavy_eval_load throughput "$d/tp.json" "" 1 "$d/oh.json")"
	selftest_check "heavy_eval_load: rc ≠ 0 falha" [ "$(jq -r .pass <<<"$res")" = false ]
	res="$(heavy_eval_load throughput "$d/ausente.json" "" 0)"
	selftest_check "heavy_eval_load: sem relatório falha" [ "$(jq -c '[.pass, .checks.report]' <<<"$res")" = "[false,false]" ]

	cat >"$d/st.json" <<'EOF'
{"config":{"concurrency":9000,"stream":true,"max_tokens":100},
 "total":{"requests":900000,"ok":900000,"errors":0,"rps":7500,"ok_rps":7500,
  "ttft_ms":{"p50":1,"p90":2,"p99":5,"max":30,"mean":1},"itl_ms":{"p50":20.1,"p90":20.5,"p99":21.4,"max":40,"mean":20.2},"e2e_ms":{"p50":2000,"p90":2010,"p99":2050,"max":2100,"mean":2000}}}
EOF
	printf '%s\n' '{"Name":"wwz-heavy-wwz","CPUPerc":"900.00%","MemUsage":"1GiB / 16GiB"}' >"$d/st-ok.jsonl"
	printf '%s\n' '{"Name":"wwz-heavy-wwz","CPUPerc":"1100.00%","MemUsage":"1GiB / 16GiB"}' >"$d/st-hot.jsonl"
	res="$(HEAVY_STREAMS=9000 HEAVY_MAX_ITL_P99_MS=22 heavy_eval_load streams "$d/st.json" "$d/st-ok.jsonl" 0)"
	selftest_check "heavy_eval_load streams: itl p99 21,4 ≤ 22 passa" [ "$(jq -r .pass <<<"$res")" = true ]
	res="$(HEAVY_STREAMS=9000 heavy_eval_load streams "$d/st.json" "$d/st-ok.jsonl" 0)"
	selftest_check "heavy_eval_load streams: overhead ITL p99 1,4 ms < 2 passa (TTFT só informado)" \
		[ "$(jq -c '[.checks.overhead_p99, .overhead_p99_ms, .ttft_overhead_p99_ms]' <<<"$res")" = "[true,1.4,5]" ]
	selftest_check "heavy_eval_load streams: 1 GiB / 9000 streams = 1137,8 MiB por 10 000, limiar padrão 2048 passa" \
		[ "$(jq -c '[.mem_per_10k_streams_mb, .checks.mem_per_10k, .thresholds.max_mem_per_10k_mb]' <<<"$res")" = "[1137.8,true,2048]" ]
	printf '%s\n' '{"Name":"wwz-heavy-wwz","CPUPerc":"900.00%","MemUsage":"3GiB / 16GiB"}' '{"Name":"wwz-heavy-wwz","CPUPerc":"900.00%","MemUsage":"1GiB / 16GiB"}' >"$d/st-peak.jsonl"
	res="$(HEAVY_STREAMS=9000 heavy_eval_load streams "$d/st.json" "$d/st-peak.jsonl" 0)"
	selftest_check "heavy_eval_load streams: usa o pico de memória (3 GiB / 9000 = 3413,3 MiB por 10 000 > 2048 reprova)" \
		[ "$(jq -c '[.pass, .checks.mem_per_10k, .mem_per_10k_streams_mb]' <<<"$res")" = "[false,false,3413.3]" ]
	res="$(HEAVY_STREAMS=9000 HEAVY_MAX_MEM_PER_10K_MB= heavy_eval_load streams "$d/st.json" "$d/st-peak.jsonl" 0)"
	selftest_check "heavy_eval_load streams: HEAVY_MAX_MEM_PER_10K_MB vazio desliga a checagem" \
		[ "$(jq -c '[(.checks | has("mem_per_10k")), .thresholds.max_mem_per_10k_mb]' <<<"$res")" = "[false,null]" ]
	jq '.total.itl_ms.p99 = 22' "$d/st.json" >"$d/st-22.json"
	res="$(HEAVY_STREAMS=9000 HEAVY_MAX_ITL_P99_MS=22 heavy_eval_load streams "$d/st-22.json" "$d/st-ok.jsonl" 0)"
	selftest_check "heavy_eval_load streams: ITL p99 22 (overhead 2 ms) reprova mesmo com itl_p99 ≤ 22" \
		[ "$(jq -c '[.pass, .checks.itl_p99, .checks.overhead_p99]' <<<"$res")" = "[false,true,false]" ]
	res="$(HEAVY_STREAMS=9000 HEAVY_MAX_MEM_PER_10K_MB=1000 heavy_eval_load streams "$d/st.json" "$d/st-ok.jsonl" 0)"
	selftest_check "heavy_eval_load streams: memória acima de HEAVY_MAX_MEM_PER_10K_MB reprova" \
		[ "$(jq -c '[.pass, .checks.mem_per_10k]' <<<"$res")" = "[false,false]" ]
	res="$(HEAVY_STREAMS=9000 HEAVY_MAX_MEM_PER_10K_MB=2000 heavy_eval_load streams "$d/st.json" "$d/st-ok.jsonl" 0)"
	selftest_check "heavy_eval_load streams: memória dentro do limite passa" \
		[ "$(jq -c '[.pass, .checks.mem_per_10k]' <<<"$res")" = "[true,true]" ]
	res="$(HEAVY_STREAMS=9000 HEAVY_MAX_ITL_P99_MS=21 heavy_eval_load streams "$d/st.json" "" 0)"
	selftest_check "heavy_eval_load streams: itl p99 acima do limite falha" [ "$(jq -c '[.pass, .checks.itl_p99]' <<<"$res")" = "[false,false]" ]
	res="$(HEAVY_STREAMS=9000 heavy_eval_load streams "$d/st.json" "$d/st-ok.jsonl" 0)"
	selftest_check "heavy_eval_load streams: wwz a 75 % da CPU passa" [ "$(jq -c '[.pass, .checks.wwz_cpu, .wwz_cpu_util_mean]' <<<"$res")" = "[true,true,0.75]" ]
	res="$(HEAVY_STREAMS=9000 heavy_eval_load streams "$d/st.json" "$d/st-hot.jsonl" 0)"
	selftest_check "heavy_eval_load streams: wwz acima de 80 % da CPU reprova" [ "$(jq -c '[.pass, .checks.wwz_cpu]' <<<"$res")" = "[false,false]" ]
	printf '%s\n' '{"Name":"wwz-heavy-wwz","CPUPerc":"0.00%","MemUsage":"7MiB / 16GiB","t":1}' \
		'{"Name":"wwz-heavy-wwz","CPUPerc":"1000.00%","MemUsage":"1GiB / 16GiB","t":2}' \
		'{"Name":"wwz-heavy-load-run-1a2b","CPUPerc":"900.00%","MemUsage":"1GiB / 24GiB","t":2}' \
		'{"Name":"wwz-heavy-wwz","CPUPerc":"1000.00%","MemUsage":"1GiB / 16GiB","t":3}' \
		'{"Name":"wwz-heavy-load-run-1a2b","CPUPerc":"900.00%","MemUsage":"1GiB / 24GiB","t":3}' >"$d/st-idle.jsonl"
	res="$(HEAVY_STREAMS=9000 heavy_eval_load streams "$d/st.json" "$d/st-idle.jsonl" 0)"
	selftest_check "heavy_eval_load streams: amostra do wwz antes do wwz-load não entra na média (0,83 reprova, não 0,56)" \
		[ "$(jq -c '[.pass, .checks.wwz_cpu, .wwz_cpu_util_mean, .cpu.wwz.samples]' <<<"$res")" = "[false,false,0.83,2]" ]
	res="$(HEAVY_STREAMS_MAX=20000 heavy_eval_load streams-max "$d/st.json" "$d/st-hot.jsonl" 0)"
	selftest_check "heavy_eval_load streams-max: informativa, passa a 92 %" \
		[ "$(jq -c '[.pass, .informative, .checks.report]' <<<"$res")" = "[true,true,true]" ]
	res="$(heavy_eval_load streams-max "$d/ausente.json" "" 0)"
	selftest_check "heavy_eval_load streams-max: sem relatório falha" [ "$(jq -r .pass <<<"$res")" = false ]
	res="$(HEAVY_STREAMS=30000 heavy_eval_load streams "$d/st.json" "" 0)"
	selftest_check "heavy_eval_load streams: concorrência diferente de HEAVY_STREAMS falha" [ "$(jq -r .checks.concurrency <<<"$res")" = false ]

	printf '%s\n' '=== RUN   TestValkeyX' '--- PASS: TestValkeyX (0.01s)' '--- PASS: TestValkeyClusterKeySlots (0.02s)' 'PASS' >"$d/ok.log"
	selftest_check "heavy_eval_cluster: PASS" [ "$(heavy_eval_cluster "$d/ok.log" 0 | jq -c '[.pass, .tests_passed]')" = "[true,2]" ]
	printf '%s\n' '--- PASS: TestValkeyX (0.01s)' '--- SKIP: TestValkeyClusterKeySlots (0.00s)' 'PASS' >"$d/skip.log"
	selftest_check "heavy_eval_cluster: teste de cluster pulado falha" [ "$(heavy_eval_cluster "$d/skip.log" 0 | jq -r .pass)" = false ]
	printf '%s\n' '--- FAIL: TestValkeyX (0.01s)' 'FAIL' >"$d/fail.log"
	selftest_check "heavy_eval_cluster: FAIL" [ "$(heavy_eval_cluster "$d/fail.log" 1 | jq -r .pass)" = false ]
	selftest_check "heavy_eval_cluster: sem log falha" [ "$(heavy_eval_cluster "$d/nada.log" 0 | jq -r .pass)" = false ]
	rm -rf "$d"
}

# ---------------------------------------------------------------------------
# quick
# ---------------------------------------------------------------------------

valkey_up() {
	if command -v valkey-cli >/dev/null 2>&1; then
		valkey-cli -h 127.0.0.1 -p "$VALKEY_PORT" ping >/dev/null 2>&1
	elif command -v nc >/dev/null 2>&1; then
		nc -z 127.0.0.1 "$VALKEY_PORT" >/dev/null 2>&1
	else
		(exec 3<>"/dev/tcp/127.0.0.1/$VALKEY_PORT") >/dev/null 2>&1
	fi
}

# valkey_compose_up: sobe só o serviço valkey (cache/valkey/docker-compose.yml) no
# projeto da raiz, sem passar pelo compose raiz, que exige WWZ_ADMIN_TOKEN.
valkey_compose_up() {
	local app_bin=/Applications/Docker.app/Contents/Resources/bin
	if ! command -v docker >/dev/null 2>&1 && [ -x "$app_bin/docker" ]; then
		PATH="$PATH:$app_bin"
	fi
	command -v docker >/dev/null 2>&1 || return 1
	local env_args=()
	[ -f "$ROOT/.env" ] && env_args=(--env-file "$ROOT/.env")
	docker compose --project-directory "$ROOT" -f "$ROOT/cache/valkey/docker-compose.yml" \
		${env_args[@]+"${env_args[@]}"} up -d --wait valkey >/dev/null 2>&1
}

# Isolamento entre execuções (task 300): o wrapper scripts/valkey-db-exec.sh (go test
# -exec) dá a cada binário de teste um Valkey próprio; só sem valkey-server nem docker cai
# no compartilhado com um banco reservado em VALKEY_DBDIR (fixo em /tmp, não em $TMPDIR,
# que muda entre sessões, para valer entre worktrees paralelos).
VALKEY_DBDIR="${WWZ_VERIFY_DBDIR:-/tmp/wwz-verify-valkey-$VALKEY_PORT.db}"

verify_quick() {
	local explicit_args=() explicit_pkgs=()
	if [ "$#" -gt 0 ]; then
		# O make tira o "./" dos alvos (./internal/x vira internal/x); recoloca para
		# o go não tratar como pacote da biblioteca padrão.
		local a in_pkgs=1
		for a in "$@"; do
			case "$a" in -*) in_pkgs=0 ;; esac
			if [ "$in_pkgs" -eq 1 ]; then
				case "$a" in
				.* | /*) ;;
				*) [ -d "${a%/...}" ] && a="./$a" ;;
				esac
				explicit_pkgs+=("$a")
			fi
			explicit_args+=("$a")
		done
	fi

	compute_scope
	MODULE="$(go list -m)"

	local go_files="" changed_pkgs="" all_scope=0 f d
	while IFS= read -r f; do
		[ -z "$f" ] && continue
		case "$f" in
		go.mod | go.sum) all_scope=1 ;;
		*.go)
			[ -f "$f" ] && go_files="$go_files $f"
			d="$(dirname "$f")"
			if [ -d "$d" ] && ls "$d"/*.go >/dev/null 2>&1; then
				changed_pkgs="$changed_pkgs $MODULE/${d#./}"
			fi
			;;
		esac
	done <<EOF
$CHANGED_FILES
EOF
	changed_pkgs="$(echo "$changed_pkgs" | tr ' ' '\n' | sed "s#^$MODULE/\.\$#$MODULE#" | sort -u | tr '\n' ' ')"
	changed_pkgs="${changed_pkgs# }"

	local affected
	if [ "$all_scope" -eq 1 ]; then
		affected="$(go list ./... | tr '\n' ' ')"
	else
		affected="$(affected_packages "$changed_pkgs" | tr '\n' ' ')"
	fi
	affected="${affected% }"

	printf 'verify quick: base %s (%s)\n' "$BASE_REF" "${MERGE_BASE:-sem base}"
	printf '  arquivos alterados: %d\n' "$(printf '%s' "$CHANGED_FILES" | grep -c . || true)"
	printf '  pacotes alterados:  %s\n' "${changed_pkgs:-nenhum}"
	printf '  pacotes afetados:   %s\n' "${affected:-nenhum}"

	# gofumpt
	step_begin gofumpt
	if [ -z "$go_files" ]; then
		step_end skip "nenhum .go alterado"
	elif need_tool gofumpt "go install mvdan.cc/gofumpt@latest"; then
		local out
		# shellcheck disable=SC2086
		out="$(gofumpt -l $go_files)"
		if [ -n "$out" ]; then
			echo "$out"
			step_end fail "arquivos fora do gofumpt (rode: gofumpt -w <arquivo>)"
		else
			step_end ok
		fi
	fi

	# golangci-lint
	step_begin lint
	if [ -z "$changed_pkgs" ] && [ "$all_scope" -eq 0 ]; then
		step_end skip "nenhum pacote alterado"
	elif need_tool golangci-lint "https://golangci-lint.run/welcome/install/"; then
		local lint_targets=() p
		if [ "$all_scope" -eq 1 ]; then
			lint_targets=(./...)
		else
			for p in $changed_pkgs; do
				lint_targets+=("./${p#"$MODULE"/}")
			done
		fi
		if [ -n "$MERGE_BASE" ]; then
			run_step golangci-lint run --allow-parallel-runners --new-from-rev="$MERGE_BASE" "${lint_targets[@]}"
		else
			run_step golangci-lint run --allow-parallel-runners "${lint_targets[@]}"
		fi
	fi

	# build
	step_begin build
	run_step go build ./...

	# Só flags depois de `--` (ex.: `-- -run TestX`): aplica aos pacotes afetados.
	# `${arr[@]+...}` evita "unbound variable" com array vazio no bash 3.2 do macOS.
	local test_pkgs=() test_args=()
	if [ "${#explicit_pkgs[@]}" -gt 0 ]; then
		test_pkgs=("${explicit_pkgs[@]}")
		test_args=("${explicit_args[@]}")
	else
		# shellcheck disable=SC2206
		test_pkgs=($affected)
		# shellcheck disable=SC2206
		test_args=(${affected} ${explicit_args[@]+"${explicit_args[@]}"})
	fi

	# vet
	step_begin vet
	if [ "${#test_pkgs[@]}" -eq 0 ]; then
		step_end skip "nenhum pacote afetado"
	else
		run_step go vet "${test_pkgs[@]}"
	fi

	# valkey (antes do test para exportar o ambiente)
	step_begin valkey
	if [ "${#test_pkgs[@]}" -gt 0 ] && needs_valkey "${test_pkgs[@]}"; then
		if valkey_up; then
			step_end ok "Valkey em 127.0.0.1:$VALKEY_PORT"
		elif valkey_compose_up && valkey_up; then
			step_end ok "Valkey iniciado com docker compose em 127.0.0.1:$VALKEY_PORT"
		else
			step_end skip "Valkey fora do ar; suba com: docker compose --project-directory . -f cache/valkey/docker-compose.yml up -d"
		fi
		if valkey_up; then
			export WWZ_TEST_VALKEY=local
			export WWZ_TEST_VALKEY_ADDR="127.0.0.1:$VALKEY_PORT"
			export WWZ_TEST_VALKEY_DB="${WWZ_TEST_VALKEY_DB:-15}"
			export WWZ_VERIFY_DBDIR="$VALKEY_DBDIR"
			local vv="${VALKEY_VERSION:-$(sed -n 's/^VALKEY_VERSION=//p' .env 2>/dev/null | tail -n1)}"
			export WWZ_VERIFY_VALKEY_IMAGE="valkey/valkey:${vv:-8.1}-alpine"
			test_args=(-exec "$ROOT/scripts/valkey-db-exec.sh" "${test_args[@]}")
		fi
	else
		step_end skip "nenhum pacote a testar usa o store"
	fi

	# test
	step_begin test
	if [ "${#test_pkgs[@]}" -eq 0 ]; then
		step_end skip "nenhum pacote afetado"
	else
		run_step go test "${test_args[@]}"
	fi

	# drivertest
	step_begin drivertest
	if ! printf '%s\n' "$CHANGED_FILES" | grep -qE '^internal/(driver|core)/' && [ "$all_scope" -eq 0 ]; then
		step_end skip "internal/driver e internal/core sem mudança"
	elif [ ! -d internal/driver/drivertest ]; then
		step_end skip "internal/driver/drivertest ainda não existe"
	else
		run_step go test ./internal/driver/...
	fi

	# grafana: dashboards e métricas citadas (scripts/check-grafana.sh)
	step_begin grafana
	local changed_list=()
	while IFS= read -r f; do
		[ -n "$f" ] && changed_list+=("$f")
	done <<EOF
$CHANGED_FILES
EOF
	if ! grafana_relevant ${changed_list[@]+"${changed_list[@]}"}; then
		step_end skip "deploy/grafana, internal/metrics e métricas wwz_* sem mudança"
	elif [ ! -x scripts/check-grafana.sh ]; then
		step_end fail "scripts/check-grafana.sh ausente ou sem permissão de execução"
	else
		run_step scripts/check-grafana.sh
	fi

	# prod: deploy/prod/test.sh (sem acessar o servidor) quando deploy/prod ou scripts/prod-* mudam
	step_begin prod
	if ! printf '%s\n' "$CHANGED_FILES" | grep -qE '^(deploy/prod/|scripts/prod-(deploy|smoke)\.sh$)' && [ "$all_scope" -eq 0 ]; then
		step_end skip "deploy/prod e scripts/prod-* sem mudança"
	elif [ ! -x deploy/prod/test.sh ]; then
		step_end skip "deploy/prod/test.sh ainda não existe"
	elif ! docker_ok; then
		step_end skip "Docker sem daemon (o teste usa docker compose config)"
	else
		run_step deploy/prod/test.sh
	fi

	# selftest do próprio script
	step_begin selftest
	if printf '%s\n' "$CHANGED_FILES" | grep -qx 'scripts/verify.sh'; then
		run_step verify_selftest
	else
		step_end skip "scripts/verify.sh sem mudança"
	fi

	print_summary quick
	[ "$FAILED" -eq 0 ]
}

# ---------------------------------------------------------------------------
# full (specs/018, tabela "verify-full"; ADR 0037)
# ---------------------------------------------------------------------------

# main_checkout: raiz do checkout principal (o mesmo ROOT fora de um worktree),
# onde ficam tmp/venv e node_modules, que são fora do Git e não existem nos worktrees.
main_checkout() {
	local common
	common="$(cd "$ROOT" && git rev-parse --git-common-dir 2>/dev/null)" || {
		echo "$ROOT"
		return
	}
	(cd "$ROOT" && cd "$common/.." 2>/dev/null && pwd) || echo "$ROOT"
}

# pick_exec <candidato>...: o primeiro executável; sem nenhum, o primeiro candidato
# (o preflight então reclama dele).
pick_exec() {
	local c
	for c in "$@"; do
		if [ -x "$c" ]; then
			echo "$c"
			return
		fi
	done
	echo "$1"
}

# link_main_node_modules <node-dir> <node-dir-do-principal>: num worktree sem
# node_modules, liga o do checkout principal quando ele está em dia e o
# package-lock.json é idêntico. Sucesso = ligou. O chamador remove o link.
link_main_node_modules() {
	local dst="$1" src="$2"
	if [ -e "$dst/node_modules" ] || [ -L "$dst/node_modules" ]; then
		return 1
	fi
	[ -d "$src/node_modules" ] || return 1
	[ "$(cd "$dst" && pwd -P)" != "$(cd "$src" && pwd -P)" ] || return 1
	if node_modules_stale "$src"; then
		return 1
	fi
	cmp -s "$dst/package-lock.json" "$src/package-lock.json" || return 1
	ln -s "$src/node_modules" "$dst/node_modules"
}

MAIN_ROOT="$(main_checkout)"
VERIFY_PYTHON="${VERIFY_PYTHON:-$(pick_exec "$ROOT/tmp/venv/bin/python" "$MAIN_ROOT/tmp/venv/bin/python")}"
VERIFY_MIN_FREE_GB="${VERIFY_MIN_FREE_GB:-1.5}"
VERIFY_COVERAGE_MIN="${VERIFY_COVERAGE_MIN:-100}"
COVER_DIR="$ROOT/tmp/coverage"
FULL_RUN_DIR="$ROOT/tmp/verify"
FULL_ID="$$"
VERIFY_VALKEY_NAME="wwz-verify-valkey-$FULL_ID"
VERIFY_SMOKE_PROJECT="wwz-verify-smoke-$FULL_ID"
VERIFY_IMAGES="white-wizard:verify white-wizard-panel:verify white-wizard-load:verify"
SDK_WWZ_PID=""
SDK_WWZ_LOG=""
HALTED=""

# docker_path: o Docker Desktop no macOS deixa o CLI (e o credential helper)
# fora do PATH padrão.
docker_path() {
	local app_bin=/Applications/Docker.app/Contents/Resources/bin
	case ":$PATH:" in
	*":$app_bin:"*) ;;
	*) [ -d "$app_bin" ] && PATH="$PATH:$app_bin" ;;
	esac
	export PATH
}

docker_ok() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }

# free_gb <dir>: GiB livres no volume de <dir>, com uma casa decimal.
free_gb() {
	df -Pk "$1" 2>/dev/null | awk 'NR == 2 { printf "%.1f\n", $4 / 1048576 }'
}

# gb_below <medido> <mínimo>: sucesso se medido < mínimo (aceita vírgula decimal).
gb_below() {
	awk -v a="${1//,/.}" -v b="${2//,/.}" 'BEGIN { exit !(a + 0 < b + 0) }'
}

# free_port: uma porta TCP livre (sempre > 1024), sorteada em 20000-29999, fora
# da faixa efêmera do macOS/Linux (32768+/49152+), e conferida com bind em
# 0.0.0.0 e em 127.0.0.1 (o compose publica em 0.0.0.0). FREE_PORT_AVOID lista
# portas já escolhidas (separadas por espaço) para não repetir.
free_port() {
	local py code='
import os, random, socket
avoid = set(os.environ.get("FREE_PORT_AVOID", "").split())
for _ in range(200):
    p = random.randint(20000, 29999)
    if str(p) in avoid:
        continue
    ok = True
    for addr in ("0.0.0.0", "127.0.0.1"):
        s = socket.socket()
        try:
            s.bind((addr, p))
        except OSError:
            ok = False
        finally:
            s.close()
        if not ok:
            break
    if ok:
        print(p)
        raise SystemExit(0)
raise SystemExit(1)
'
	for py in "$VERIFY_PYTHON" python3; do
		if [ -x "$py" ] || command -v "$py" >/dev/null 2>&1; then
			"$py" -c "$code" 2>/dev/null && return 0
		fi
	done
	perl -MIO::Socket::INET -e '
my %avoid = map { $_ => 1 } split / /, ($ENV{FREE_PORT_AVOID} // "");
for (1 .. 200) {
  my $p = 20000 + int(rand(10000));
  next if $avoid{$p};
  my $ok = 1;
  for my $a ("0.0.0.0", "127.0.0.1") {
    my $s = IO::Socket::INET->new(Listen => 1, LocalAddr => $a, LocalPort => $p, ReuseAddr => 1);
    if ($s) { close $s } else { $ok = 0; last }
  }
  if ($ok) { print "$p\n"; exit 0 }
}
exit 1'
}

# free_ports <n>: n portas livres distintas, uma por linha.
free_ports() {
	local n="$1" i p avoid=""
	for ((i = 0; i < n; i++)); do
		p="$(FREE_PORT_AVOID="$avoid" free_port)" || return 1
		avoid="$avoid $p"
		printf '%s\n' "$p"
	done
}

# input_missing_reason <dir> <quando>: imprime o motivo do "pulado" quando o
# insumo do passo ainda não existe no repositório (ADR 0037); vazio se existe.
input_missing_reason() {
	[ -d "$1" ] || printf '%s/ ausente (%s)' "${1#"$ROOT"/}" "$2"
}

# node_lock_body <arquivo>: o lockfile sem a entrada raiz "" de "packages", que o
# npm ci não copia para node_modules/.package-lock.json.
node_lock_body() {
	awk 'skip { if ($0 == "    },") skip = 0; next } $0 == "    \"\": {" { skip = 1; next } { print }' "$1"
}

# node_modules_stale <dir>: sucesso se <dir>/node_modules falta ou o
# node_modules/.package-lock.json (gravado pelo npm ci) difere do package-lock.json.
# Compara conteúdo, não mtime: o checkout de um worktree dá ao lock um mtime novo.
node_modules_stale() {
	[ -d "$1/node_modules" ] || return 0
	[ -f "$1/node_modules/.package-lock.json" ] || return 0
	[ -f "$1/package-lock.json" ] || return 0
	[ "$(node_lock_body "$1/package-lock.json")" != "$(node_lock_body "$1/node_modules/.package-lock.json")" ]
}

# node_modules_install <dir>: npm ci em <dir> ($VERIFY_NPM substitui o npm, para o
# selftest). Recusa (código 3) quando <dir>/node_modules é symlink: o npm ci
# esvaziaria o destino (o node_modules do checkout principal) seguindo o link.
node_modules_install() {
	if [ -L "$1/node_modules" ]; then
		echo "$1/node_modules é symlink para $(readlink "$1/node_modules") e está desatualizado: rode npm ci --prefer-offline no checkout de destino ou remova o link" >&2
		return 3
	fi
	"${VERIFY_NPM:-npm}" ci --prefer-offline --no-audit --no-fund --prefix "$1"
}

# coverage_gate <cover.out> <módulo> <mínimo> <summary.txt>: soma os statements
# por pacote de internal/ e cmd/ (bloco repetido conta uma vez e está coberto se
# alguma ocorrência tem contagem > 0), grava o relatório e imprime, um por linha,
# os pacotes abaixo do mínimo ("pct% pacote"). Sai 1 se houver algum; 2 sem perfil.
coverage_gate() {
	local profile="$1" module="$2" min="${3//,/.}" summary="$4" below
	[ -s "$profile" ] || return 2
	awk -v module="$module" '
		NR == 1 && /^mode:/ { next }
		NF == 3 {
			key = $1
			file = key
			sub(/:[^:]*$/, "", file)
			sub(/\/[^\/]*$/, "", file)
			stmts[key] = $2
			pkg[key] = file
			if ($3 + 0 > 0) hit[key] = 1
		}
		END {
			for (k in stmts) {
				p = pkg[k]
				if (index(p, module "/internal/") != 1 && index(p, module "/cmd/") != 1) continue
				tot[p] += stmts[k]
				T += stmts[k]
				if (k in hit) { cov[p] += stmts[k]; C += stmts[k] }
			}
			for (p in tot) {
				pct = tot[p] ? 100 * cov[p] / tot[p] : 100
				printf "%.2f %s %d %d\n", int(pct * 100) / 100, p, cov[p], tot[p]
			}
			if (T) printf "%.2f total %d %d\n", int(10000 * C / T) / 100, C, T
		}' "$profile" | sort -k1,1n -k2,2 >"$summary.raw" || return 2
	{
		printf 'Cobertura de statements por pacote (internal/ e cmd/), mínimo %s%%\n\n' "$min"
		awk '$2 != "total" { printf "  %7.2f%%  %6d/%-6d  %s\n", $1, $3, $4, $2 }' "$summary.raw"
		awk '$2 == "total" { printf "\n  %7.2f%%  %6d/%-6d  total\n", $1, $3, $4 }' "$summary.raw"
	} >"$summary"
	below="$(awk -v min="$min" '$2 != "total" && $1 + 0 < min + 0 { printf "%s%% %s\n", $1, $2 }' "$summary.raw")"
	rm -f "$summary.raw"
	[ -z "$below" ] && return 0
	printf '%s\n' "$below"
	return 1
}

# merge_profiles <saída> <perfil>...: funde perfis de cobertura (modo da 1ª
# linha do primeiro), um registro por bloco com a maior contagem.
merge_profiles() {
	local out="$1" mode
	shift
	mode="$(sed -n '1s/^mode: //p' "$1")"
	{
		printf 'mode: %s\n' "${mode:-atomic}"
		awk 'FNR == 1 && /^mode:/ { next }
			NF == 3 { if (!($1 in n) || $3 + 0 > n[$1] + 0) n[$1] = $3; s[$1] = $2 }
			END { for (k in s) print k, s[k], n[k] }' "$@" | sort
	} >"$out.tmp" && mv "$out.tmp" "$out"
}

step_preflight() {
	step_begin preflight
	local missing="" t gb
	for t in go gofumpt golangci-lint docker node npm curl jq; do
		command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
	done
	if [ ! -x "$VERIFY_PYTHON" ]; then
		missing="$missing python($VERIFY_PYTHON ausente; crie com: python3 -m venv tmp/venv && tmp/venv/bin/pip install -r tests/sdk/python/requirements.txt, ou defina VERIFY_PYTHON)"
	elif ! "$VERIFY_PYTHON" -m pytest --version >/dev/null 2>&1; then
		missing="$missing pytest(em $VERIFY_PYTHON)"
	fi
	if [ -n "$missing" ]; then
		step_end fail "faltando:$missing"
		return
	fi
	if ! docker info >/dev/null 2>&1; then
		step_end fail "Docker sem daemon (abra o Docker Desktop)"
		return
	fi
	gb="$(free_gb "$ROOT")"
	if gb_below "$gb" "$VERIFY_MIN_FREE_GB"; then
		echo "disco: ${gb} GiB livres < ${VERIFY_MIN_FREE_GB}; rodando go clean -testcache e docker builder prune -f"
		go clean -testcache
		docker builder prune -f >/dev/null 2>&1
		gb="$(free_gb "$ROOT")"
		if gb_below "$gb" "$VERIFY_MIN_FREE_GB"; then
			step_end fail "disco: ${gb} GiB livres depois da limpeza (mínimo VERIFY_MIN_FREE_GB=${VERIFY_MIN_FREE_GB})"
			return
		fi
	fi
	step_end ok "disco ${gb} GiB livres; python $VERIFY_PYTHON"
}

step_gofumpt() {
	step_begin gofumpt
	local out
	# Arquivos do repositório (versionados e novos não ignorados): `gofumpt -l .`
	# desceria em .claude/worktrees/ e tmp/.
	out="$(git ls-files -co --exclude-standard -- '*.go' | xargs gofumpt -l)"
	if [ -n "$out" ]; then
		echo "$out"
		step_end fail "arquivos fora do gofumpt (rode: gofumpt -w <arquivo>)"
	else
		step_end ok
	fi
}

step_lint() {
	step_begin lint
	run_step golangci-lint run --allow-parallel-runners ./...
}

step_vet() {
	step_begin vet
	# Os arquivos só-Linux de internal/server (listen_linux.go, watch_unix.go) não
	# entram no gate de cobertura rodado no macOS (docs/verificacao.md); o vet
	# cruzado ao menos garante que compilam e passam no vet.
	run_step vet_all
}

vet_all() {
	go vet ./... && GOOS=linux go vet ./internal/server/...
}

step_build() {
	step_begin build
	run_step go build ./...
}

step_valkey() {
	step_begin valkey
	local version image addr _
	version="${VALKEY_VERSION:-$(sed -n 's/^VALKEY_VERSION=//p' .env 2>/dev/null | tail -n1)}"
	image="valkey/valkey:${version:-8.1}-alpine"
	if ! docker run --rm -d --name "$VERIFY_VALKEY_NAME" -p 127.0.0.1::6379 \
		--read-only --tmpfs /data --ulimit nofile=65536:65536 --memory 384m --cpus 1 \
		"$image" valkey-server --save "" --appendonly no --maxmemory 256mb >/dev/null; then
		step_end fail "docker run $image falhou"
		return
	fi
	addr="$(docker port "$VERIFY_VALKEY_NAME" 6379/tcp 2>/dev/null | grep '^127\.0\.0\.1:' | head -n1)"
	for _ in $(seq 1 50); do
		[ "$(docker exec "$VERIFY_VALKEY_NAME" valkey-cli ping 2>/dev/null)" = PONG ] && break
		sleep 0.2
	done
	if [ -z "$addr" ] || [ "$(docker exec "$VERIFY_VALKEY_NAME" valkey-cli ping 2>/dev/null)" != PONG ]; then
		step_end fail "Valkey $VERIFY_VALKEY_NAME não respondeu"
		return
	fi
	export WWZ_TEST_VALKEY=local WWZ_TEST_VALKEY_ADDR="$addr" WWZ_TEST_VALKEY_DB=15
	step_end ok "$image em $addr ($VERIFY_VALKEY_NAME)"
}

# test_and_merge: go test -race de tudo e, em seguida, sem -race só para os
# pacotes que pulam trabalho sob race (drivertest: sweep de mutações, ~500 s sob
# race); os perfis são fundidos (máximo por bloco) antes do gate.
test_and_merge() {
	local xe="$ROOT/scripts/valkey-db-exec.sh"
	WWZ_VERIFY_DBDIR="$(mktemp -d)/db" go test -exec "$xe" -race -covermode=atomic -coverprofile="$COVER_DIR/cover.out" ./... || return 1
	rm -f "$COVER_DIR/norace.out"
	WWZ_VERIFY_DBDIR="$(mktemp -d)/db" go test -exec "$xe" -covermode=atomic -coverprofile="$COVER_DIR/norace.out" ./internal/driver/drivertest || return 1
	merge_profiles "$COVER_DIR/cover.out" "$COVER_DIR/cover.out" "$COVER_DIR/norace.out"
}

step_test() {
	step_begin test
	mkdir -p "$COVER_DIR"
	rm -f "$COVER_DIR/cover.out"
	run_step test_and_merge
}

step_coverage() {
	step_begin coverage
	local below rc
	go tool cover -func="$COVER_DIR/cover.out" >"$COVER_DIR/func.txt" 2>&1
	go tool cover -html="$COVER_DIR/cover.out" -o "$COVER_DIR/cover.html" 2>/dev/null
	below="$(coverage_gate "$COVER_DIR/cover.out" "$(go list -m)" "$VERIFY_COVERAGE_MIN" "$COVER_DIR/summary.txt")"
	rc=$?
	case "$rc" in
	0) step_end ok "todos ≥ ${VERIFY_COVERAGE_MIN}% ($(awk '$NF == "total" { print $1 }' "$COVER_DIR/summary.txt") no total); tmp/coverage/summary.txt e cover.html" ;;
	1)
		printf 'Pacotes abaixo de %s%%:\n' "$VERIFY_COVERAGE_MIN"
		printf '%s\n' "$below" | sed 's/^/  /'
		step_end fail "$(printf '%s\n' "$below" | grep -c .) pacote(s) abaixo de ${VERIFY_COVERAGE_MIN}%; tmp/coverage/summary.txt"
		;;
	*) step_end fail "sem perfil de cobertura em tmp/coverage/cover.out" ;;
	esac
}

step_fixtures() {
	step_begin fixtures
	local why
	why="$(input_missing_reason "$ROOT/testdata/real" "fase 5")"
	if [ -n "$why" ]; then
		step_end skip "$why"
	else
		# Comparação byte a byte (ADR 0014): testes TestRealFixtures* leem testdata/real/.
		local log rc n
		log="$(mktemp "${TMPDIR:-/tmp}/wwz-verify-fixtures.XXXXXX")"
		go test -count=1 -v -run '^TestRealFixtures' ./... >"$log" 2>&1
		rc=$?
		# Só o essencial: sem RUN/PAUSE/CONT/PASS de cada (sub)teste nem os
		# pacotes sem TestRealFixtures*.
		grep -Ev '^[[:space:]]*(=== (RUN|PAUSE|CONT|NAME)|--- PASS)|^PASS$|no tests to run|no test files' "$log"
		n="$(fixtures_count "$log")"
		rm -f "$log"
		fixtures_verdict "$rc" "$n"
	fi
}

# fixtures_count <log do go test -v>: quantos testes TestRealFixtures* de topo
# (sem subtestes) rodaram.
fixtures_count() {
	grep -cE '^=== RUN[[:space:]]+TestRealFixtures[^/[:space:]]*$' "$1"
}

# fixtures_verdict <código do go test> <testes rodados>: fecha o passo; sem
# nenhum TestRealFixtures* rodado o passo falha (specs/018 "Fixtures reais").
fixtures_verdict() {
	if [ "$1" -ne 0 ]; then
		step_end fail "go test falhou ($2 teste(s) TestRealFixtures*)"
	elif [ "$2" -eq 0 ]; then
		step_end fail "nenhum teste TestRealFixtures* rodou com testdata/real/ presente"
	else
		step_end ok "$2 teste(s) TestRealFixtures*"
	fi
}

step_workflows() {
	step_begin workflows
	local why out rc
	why="$(input_missing_reason "$ROOT/.github/workflows" "sem workflows")"
	if [ -n "$why" ]; then
		step_end skip "$why"
		return
	fi
	out="$(scripts/lint-workflows.sh 2>&1)"
	rc=$?
	printf '%s\n' "$out"
	if [ "$rc" -ne 0 ]; then
		step_end fail "actionlint com erros"
	elif printf '%s' "$out" | grep -q 'pulado'; then
		step_end skip "$(printf '%s' "$out" | grep 'pulado' | tail -n1 | sed 's/^workflows: //')"
	else
		step_end ok "$(printf '%s' "$out" | tail -n1 | sed 's/^workflows: //')"
	fi
}

step_images() {
	step_begin images
	run_step make --no-print-directory image image-panel image-load \
		IMAGE=white-wizard:verify PANEL_IMAGE=white-wizard-panel:verify LOAD_IMAGE=white-wizard-load:verify
}

step_smoke() {
	step_begin smoke
	local ports p1 p2 p3 p4 p5 attempt log
	log="$FULL_RUN_DIR/smoke.log"
	# As portas são escolhidas antes de o compose ligá-las; outro processo pode
	# pegá-las no intervalo (TOCTOU). Com "address already in use" derruba o
	# projeto parcial e repete (até 3 vezes) com portas novas.
	for attempt in 1 2 3; do
		ports="$(free_ports 5)" || {
			step_end fail "sem portas livres"
			return
		}
		{ read -r p1; read -r p2; read -r p3; read -r p4; read -r p5; } <<<"$ports"
		if env SMOKE_PROJECT="$VERIFY_SMOKE_PROJECT" \
			WWZ_IMAGE=white-wizard:verify WWZ_PANEL_IMAGE=white-wizard-panel:verify WWZ_LOAD_IMAGE=white-wizard-load:verify \
			WWZ_PORT="$p1" WWZ_ADMIN_PORT="$p2" WWZ_PANEL_PORT="$p3" VALKEY_PORT="$p4" PROMETHEUS_PORT="$p5" \
			scripts/smoke.sh 2>&1 | tee "$log"; then
			step_end ok "projeto $VERIFY_SMOKE_PROJECT, portas $p1/$p2/$p3/$p4/$p5 (tentativa $attempt)"
			return
		fi
		if [ "$attempt" -lt 3 ] && grep -qiE 'address already in use|port is already allocated' "$log"; then
			printf 'smoke: porta tomada por outro processo; tentativa %s/3 com portas novas\n' "$((attempt + 1))" >&2
			docker compose -p "$VERIFY_SMOKE_PROJECT" down -v --remove-orphans >/dev/null 2>&1 || true
			continue
		fi
		break
	done
	step_end fail "scripts/smoke.sh falhou (projeto $VERIFY_SMOKE_PROJECT)"
}

# sdk_wwz_up: bin/wwz com tests/sdk/wwz.yaml (store memory) em portas livres;
# exporta WWZ_SDK_BASE_URL e WWZ_SDK_KEY. Idempotente (sdk-python e sdk-node).
sdk_wwz_up() {
	if [ -n "$SDK_WWZ_PID" ] && kill -0 "$SDK_WWZ_PID" 2>/dev/null; then
		return 0
	fi
	mkdir -p bin "$FULL_RUN_DIR"
	go build -trimpath -o bin/wwz ./cmd/wwz || return 1
	local port aport _
	port="$(free_port)" aport="$(free_port)"
	SDK_WWZ_LOG="$FULL_RUN_DIR/wwz-sdk-$FULL_ID.log"
	bin/wwz serve --config tests/sdk/wwz.yaml --listen "127.0.0.1:$port" \
		--admin-listen "127.0.0.1:$aport" >"$SDK_WWZ_LOG" 2>&1 &
	SDK_WWZ_PID=$!
	for _ in $(seq 1 100); do
		curl -fsS --max-time 1 "http://127.0.0.1:$aport/health/ready" >/dev/null 2>&1 && break
		kill -0 "$SDK_WWZ_PID" 2>/dev/null || break
		sleep 0.2
	done
	if ! curl -fsS --max-time 2 "http://127.0.0.1:$aport/health/ready" >/dev/null 2>&1; then
		echo "wwz do SDK não ficou pronto; log:" >&2
		tail -n 30 "$SDK_WWZ_LOG" >&2
		return 1
	fi
	export WWZ_SDK_BASE_URL="http://127.0.0.1:$port" WWZ_SDK_KEY=sk-wwz-sdk
	echo "wwz do SDK em $WWZ_SDK_BASE_URL (admin 127.0.0.1:$aport, pid $SDK_WWZ_PID)"
}

step_sdk_python() {
	step_begin sdk-python
	if ! sdk_wwz_up; then
		step_end fail "bin/wwz serve --config tests/sdk/wwz.yaml não subiu"
		return
	fi
	if PYTHONDONTWRITEBYTECODE=1 "$VERIFY_PYTHON" -m pytest -q -p no:cacheprovider tests/sdk/python; then
		step_end ok "contra $WWZ_SDK_BASE_URL"
	else
		step_end fail "pytest tests/sdk/python"
	fi
}

step_sdk_node() {
	step_begin sdk-node
	if ! sdk_wwz_up; then
		step_end fail "bin/wwz serve --config tests/sdk/wwz.yaml não subiu"
		return
	fi
	local linked=0
	if node_modules_stale tests/sdk/node && link_main_node_modules tests/sdk/node "$MAIN_ROOT/tests/sdk/node"; then
		linked=1
		echo "tests/sdk/node/node_modules ligado ao do checkout principal ($MAIN_ROOT)"
	fi
	if node_modules_stale tests/sdk/node; then
		echo "tests/sdk/node/node_modules ausente ou diferente do package-lock.json: npm ci"
		if ! node_modules_install tests/sdk/node; then
			[ "$linked" -eq 1 ] && rm -f tests/sdk/node/node_modules
			step_end fail "npm ci em tests/sdk/node (node_modules em symlink não é reinstalado)"
			return
		fi
	fi
	run_step node --test tests/sdk/node/
	[ "$linked" -eq 1 ] && rm -f tests/sdk/node/node_modules
}

# examples_missing <dir>: imprime, separados por espaço, os arquivos obrigatórios
# que faltam em <dir> (ADR 0016; o smoke.sh é o passo em si).
examples_missing() {
	local f out=""
	for f in config.yaml docker-compose.yml README.md smoke.sh; do
		[ -f "$1/$f" ] || out="$out $f"
	done
	printf '%s' "${out# }"
}

# examples_validate <config.yaml>: wwz validate (EXAMPLES_VALIDATE substitui o
# comando, para o selftest).
examples_validate() {
	if [ -n "${EXAMPLES_VALIDATE:-}" ]; then
		$EXAMPLES_VALIDATE "$1"
	else
		"$ROOT/bin/wwz" validate -config "$1"
	fi
}

# examples_run <dir-dos-exemplos> [nome...]: confere a estrutura de todos antes
# de subir qualquer um, valida o config.yaml e roda cada smoke.sh. Ignora _*.
# Devolve 0 (ok, nota em EXAMPLES_NOTE), 1 (falha, nota) ou 3 (nenhum exemplo).
examples_run() {
	local root="$1" d name n=0 bad="" miss names="" only=" ${*:2} "
	EXAMPLES_NOTE=""
	for d in "$root"/*/; do
		d="${d%/}"
		name="$(basename "$d")"
		case "$name" in _*) continue ;; esac
		if [ "$#" -gt 1 ]; then
			case "$only" in *" $name "*) ;; *) continue ;; esac
		fi
		miss="$(examples_missing "$d")"
		[ -n "$miss" ] && bad="$bad; $name: falta $miss"
		n=$((n + 1))
		names="$names $name"
	done
	if [ -n "$bad" ]; then
		EXAMPLES_NOTE="estrutura incompleta${bad}"
		return 1
	fi
	[ "$n" -eq 0 ] && return 3
	for name in $names; do
		d="$root/$name"
		printf -- '--- %s: wwz validate\n' "$name"
		if ! examples_validate "$d/config.yaml"; then
			EXAMPLES_NOTE="$name: wwz validate recusou config.yaml"
			return 1
		fi
		printf -- '--- %s: smoke.sh\n' "$name"
		if ! bash "$d/smoke.sh"; then
			EXAMPLES_NOTE="$name: smoke.sh falhou"
			return 1
		fi
	done
	EXAMPLES_NOTE="$n exemplo(s):$names"
}

# ADR 0016: os 18 exemplos (EXAMPLES_EXPECTED substitui a lista; "any" desliga a
# conferência, só para o selftest).
EXAMPLES_ADR0016="agente-de-arquivos anthropic-perfeito ban-por-abuso benchmark-vazao-maxima claude-code-assinatura cluster-3-instancias-valkey cota-5h-semana creditos-openrouter-esgotando falhas-20pct indisponibilidade-janelas latencia-intermitente multi-provedor-failover openai-perfeito opencode-completo raciocinio-longo rate-limit-rpm stream-corta-no-meio tool-calls-quebradas"

# examples_list_check <dir>: imprime a diferença entre os diretórios sem "_" e a
# lista da ADR 0016; sai 1 se diferirem (inclusive em quantidade).
examples_list_check() {
	local want="${EXAMPLES_EXPECTED:-$EXAMPLES_ADR0016}" d name have="" extra="" absent="" n=0 w
	[ "$want" = any ] && return 0
	for d in "$1"/*/; do
		[ -d "$d" ] || continue
		name="$(basename "$d")"
		case "$name" in _*) continue ;; esac
		have="$have $name"
		n=$((n + 1))
		case " $want " in *" $name "*) ;; *) extra="$extra $name" ;; esac
	done
	for w in $want; do
		case " $have " in *" $w "*) ;; *) absent="$absent $w" ;; esac
	done
	[ "$n" -eq 0 ] && return 0 # nada a conferir: o passo sai "pulado" (fase 5)
	[ -z "$extra" ] && [ -z "$absent" ] && return 0
	printf '%s exemplo(s); a ADR 0016 manda %s' "$n" "$(printf '%s\n' $want | grep -c .)"
	[ -n "$absent" ] && printf '; faltam:%s' "$absent"
	[ -n "$extra" ] && printf '; fora da ADR 0016:%s' "$extra"
	return 1
}

step_examples() {
	step_begin examples
	local why root="${EXAMPLES_DIR:-$ROOT/examples}" rc diff
	why="$(input_missing_reason "$root" "fase 5")"
	if [ -n "$why" ]; then
		step_end skip "$why"
		return
	fi
	# Rodada completa (sem nomes): a lista tem de ser a da ADR 0016.
	if [ "$#" -eq 0 ] && ! diff="$(examples_list_check "$root")"; then
		step_end fail "$diff"
		return
	fi
	if [ -z "${EXAMPLES_VALIDATE:-}" ]; then
		mkdir -p bin
		if ! go build -trimpath -o bin/wwz ./cmd/wwz; then
			step_end fail "go build ./cmd/wwz (wwz validate)"
			return
		fi
	fi
	examples_run "$root" "$@"
	rc=$?
	case "$rc" in
	0) step_end ok "$EXAMPLES_NOTE" ;;
	3) step_end skip "examples/ sem nenhum exemplo (fase 5)" ;;
	*) step_end fail "$EXAMPLES_NOTE" ;;
	esac
}

# verify_examples [nome...]: só o passo examples (make examples / make example).
verify_examples() {
	docker_path
	step_begin examples-preflight
	if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
		step_end fail "exige curl, jq e docker com daemon"
	else
		step_end ok
	fi
	[ "$FAILED" -eq 0 ] && step_examples "$@"
	print_summary examples
	[ "$FAILED" -eq 0 ]
}

# full_cleanup: passo 13; roda sempre (trap EXIT), também depois de falha ou ^C.
# Só mexe no que é desta execução (sufixo -$FULL_ID) e nas imagens :verify.
full_cleanup() {
	step_begin cleanup
	local problems="" img left
	if [ -n "$SDK_WWZ_PID" ]; then
		kill "$SDK_WWZ_PID" 2>/dev/null
		wait "$SDK_WWZ_PID" 2>/dev/null
		SDK_WWZ_PID=""
	fi
	if docker_ok; then
		docker rm -f "$VERIFY_VALKEY_NAME" >/dev/null 2>&1
		if [ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$VERIFY_SMOKE_PROJECT")" ]; then
			WWZ_ADMIN_TOKEN=cleanup docker compose -p "$VERIFY_SMOKE_PROJECT" --profile load \
				down -v --remove-orphans >/dev/null 2>&1
		fi
		for img in $VERIFY_IMAGES; do
			docker image inspect "$img" >/dev/null 2>&1 && docker image rm "$img" >/dev/null 2>&1
		done
		docker builder prune -f --filter until=24h >/dev/null 2>&1
		left="$(docker ps -a --format '{{.Names}}' | grep -E "^($VERIFY_VALKEY_NAME\$|$VERIFY_SMOKE_PROJECT-)" | tr '\n' ' ')"
		[ -n "$left" ] && problems="$problems containers: $left"
		for img in $VERIFY_IMAGES; do
			docker image inspect "$img" >/dev/null 2>&1 && problems="$problems imagem $img"
		done
	fi
	# Tarballs OCI do ensaio de release (scripts/release-dryrun.sh) não podem ficar no disco.
	left="$(ls "$ROOT"/tmp/release/*.tar 2>/dev/null | tr '\n' ' ')"
	[ -n "$left" ] && problems="$problems tmp/release: $left"
	if [ -n "$problems" ]; then
		step_end fail "sobrou:$problems"
	else
		step_end ok "sem containers wwz-verify-*-$FULL_ID nem imagens :verify"
	fi
}

FULL_STEPS="preflight gofumpt lint vet build valkey test coverage fixtures workflows images smoke sdk-python sdk-node examples"

# full_finish: trap EXIT do full; fecha passo interrompido, limpa, resume e sai.
full_finish() {
	local rc=$?
	trap - EXIT INT TERM
	[ "$STEP_OPEN" -eq 1 ] && step_end fail "interrompido"
	full_cleanup
	print_summary full
	[ -n "$HALTED" ] && printf 'Parou no passo %s.\n' "$HALTED"
	[ "$FAILED" -ne 0 ] && exit 1
	exit "$rc"
}

verify_full() {
	docker_path
	mkdir -p "$FULL_RUN_DIR"
	trap full_finish EXIT
	trap 'exit 130' INT TERM
	printf 'verify full: VERIFY_COVERAGE_MIN=%s VERIFY_MIN_FREE_GB=%s VERIFY_PYTHON=%s\n' \
		"$VERIFY_COVERAGE_MIN" "$VERIFY_MIN_FREE_GB" "$VERIFY_PYTHON"
	local s
	for s in ${VERIFY_ONLY:-$FULL_STEPS}; do
		if [ -n "$HALTED" ]; then
			step_begin "$s"
			step_end skip "não rodou: $HALTED falhou"
			continue
		fi
		if [ "${VERIFY_FAIL_AT:-}" = "$s" ]; then
			# Gancho de teste: falha forçada para conferir o resumo e a limpeza.
			step_begin "$s"
			step_end fail "falha forçada (VERIFY_FAIL_AT=$s)"
		else
			"step_${s//-/_}"
		fi
		[ "$FAILED" -ne 0 ] && HALTED="$s"
	done
	[ "$FAILED" -eq 0 ]
}

# heavy: a orquestração vive em scripts/heavy.sh (biblioteca; usa os passos e
# as funções de disco/Docker daqui).
# shellcheck source=scripts/heavy.sh
. "$ROOT/scripts/heavy.sh"

usage() {
	sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
}

mode="${1:-quick}"
[ "$#" -gt 0 ] && shift
if [ "${1:-}" = "--" ]; then
	shift
fi

case "$mode" in
quick) verify_quick "$@" ;;
selftest) verify_selftest ;;
full) verify_full ;;
examples) verify_examples "$@" ;;
heavy) verify_heavy ;;
-h | --help | help) usage ;;
*)
	usage >&2
	exit 64
	;;
esac

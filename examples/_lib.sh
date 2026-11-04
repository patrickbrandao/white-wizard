#!/usr/bin/env bash
# Biblioteca comum dos smokes de examples/ (ADR 0016, spec 019). Só funções:
# `source` a partir de examples/<nome>/smoke.sh; não executa nada ao ser lida.
# Diretórios que começam com `_` não são exemplos.
#
# Uso típico (veja examples/README.md):
#   source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"
#   ex_init falhas-20pct 3          # nome, índice (porta = 18100 + índice)
#   ex_up
#   (ex_up --wait já espera o healthcheck; o data plane não tem /healthz)
#   ex_count_codes 300 ex_post_chat "$EX_PORT" wwz-key-1   # histograma
#   ex_assert_between "$(ex_hist 200)" 200 280 "respostas 200"
#
# Variáveis: EX_DIR (diretório do exemplo), EX_NAME, EX_PROJECT (wwz-ex-<nome>),
# EX_PORT, WWZ_IMAGE (padrão white-wizard:verify se existir, senão :dev),
# EX_HTTP_CODE/EX_HTTP_BODY (último ex_post_chat), ex_hist <código> (histograma).
# Compatível com bash 3.2 (macOS): sem arrays associativos.

# Docker Desktop no macOS não põe o CLI no PATH de shells não interativos.
if [ -d /Applications/Docker.app/Contents/Resources/bin ]; then
	case ":$PATH:" in
	*:/Applications/Docker.app/Contents/Resources/bin:*) ;;
	*) PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin" ;;
	esac
	export PATH
fi

EX_DIR="${EX_DIR:-}"
EX_NAME=""
EX_PROJECT=""
EX_PORT=""
EX_UP=0
EX_HTTP_CODE=""
EX_HTTP_BODY=""
EX_CODES=""
EX_DEFAULT_BODY='{"model":"gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}'

# ex_fail <msg>: imprime em stderr e sai 1 (o trap EXIT derruba o compose).
ex_fail() {
	printf 'FALHA [%s]: %s\n' "${EX_NAME:-examples}" "$*" >&2
	exit 1
}

# ex_compose <args...>: docker compose do exemplo.
ex_compose() {
	docker compose -p "$EX_PROJECT" -f "$EX_DIR/docker-compose.yml" "$@"
}

# ex_init <nome> <índice>: valida dependências, define projeto e porta e
# registra ex_down em trap EXIT. O diretório do exemplo é o do smoke.sh que chamou.
ex_init() {
	[ $# -eq 2 ] || ex_fail "uso: ex_init <nome> <índice>"
	local src="${BASH_SOURCE[1]:-$0}"
	command -v curl >/dev/null 2>&1 || ex_fail "curl não encontrado"
	command -v jq >/dev/null 2>&1 || ex_fail "jq não encontrado"
	command -v docker >/dev/null 2>&1 || ex_fail "docker não encontrado"
	case "$2" in "" | *[!0-9]*) ex_fail "índice inválido: $2" ;; esac
	EX_NAME="$1"
	EX_PROJECT="wwz-ex-$1"
	EX_PORT="$((18100 + $2))"
	EX_DIR="${EX_DIR:-$(cd "$(dirname "$src")" && pwd)}"
	if [ -z "${WWZ_IMAGE:-}" ]; then
		if docker image inspect white-wizard:verify >/dev/null 2>&1; then
			WWZ_IMAGE=white-wizard:verify
		else
			WWZ_IMAGE=white-wizard:dev
		fi
	fi
	export WWZ_IMAGE EX_PORT EX_PROJECT
	trap ex_down EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
}

# ex_up: sobe o compose do exemplo e espera os healthchecks.
ex_up() {
	[ -n "$EX_PROJECT" ] || ex_fail "ex_up antes de ex_init"
	EX_UP=1 # marca antes: um up parcial também precisa de down
	ex_compose up -d --wait --wait-timeout "${EX_UP_TIMEOUT:-90}" >&2 || {
		ex_compose logs --no-color --tail 40 >&2 || true
		ex_fail "docker compose up falhou"
	}
}

# ex_down: down -v --remove-orphans; idempotente e sem falhar (trap EXIT).
ex_down() {
	local rc=$?
	trap - EXIT
	if [ "$EX_UP" -eq 1 ] && [ -n "$EX_PROJECT" ]; then
		ex_compose down -v --remove-orphans >/dev/null 2>&1 || true
		EX_UP=0
	fi
	exit "$rc"
}

# ex_wait_http <url> [timeout_s=30]: espera um 2xx.
ex_wait_http() {
	local url="$1" t="${2:-30}" i=0
	while [ "$i" -lt "$((t * 2))" ]; do
		curl -fsS -o /dev/null --max-time 2 "$url" 2>/dev/null && return 0
		sleep 0.5
		i=$((i + 1))
	done
	ex_fail "timeout de ${t}s esperando $url"
}

# ex_post_chat <porta> <chave> [corpo] [caminho]: POST de chat (OpenAI por
# padrão, /v1/chat/completions). Preenche EX_HTTP_CODE e EX_HTTP_BODY; devolve 0
# se houve resposta HTTP (qualquer código), 1 em erro de transporte (código 000).
ex_post_chat() {
	local port="$1" key="$2" body="${3:-$EX_DEFAULT_BODY}" path="${4:-/v1/chat/completions}" out
	out="$(curl -sS --max-time "${EX_CURL_TIMEOUT:-30}" -w $'\n%{http_code}' \
		-H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
		-d "$body" "http://127.0.0.1:$port$path" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
	[ "$EX_HTTP_CODE" != 000 ]
}

# ex_hist <código>: contagem do último ex_count_codes para esse código (0 se nenhuma).
# (bash 3.2 do macOS não tem array associativo: o histograma vive em EX_HIST_<código>.)
ex_hist() {
	local v="EX_HIST_$1"
	printf '%s' "${!v:-0}"
}

# ex_count_codes <n> <cmd...>: roda o comando n vezes (ele deve chamar
# ex_post_chat ou preencher EX_HTTP_CODE), acumula o histograma (ex_hist) e
# imprime "código=contagem" ordenado. EX_CODES lista os códigos vistos.
ex_count_codes() {
	local _n="$1" _i _c
	shift
	for _c in $EX_CODES; do unset "EX_HIST_$_c"; done
	EX_CODES=""
	for ((_i = 0; _i < _n; _i++)); do
		"$@" || true
		_c="$EX_HTTP_CODE"
		case " $EX_CODES " in *" $_c "*) ;; *) EX_CODES="$EX_CODES $_c" ;; esac
		printf -v "EX_HIST_$_c" '%s' "$(($(ex_hist "$_c") + 1))"
	done
	for _c in $EX_CODES; do echo "$_c=$(ex_hist "$_c")"; done | sort
}

# ex_assert_between <valor> <min> <max> <rótulo>: inteiros, limites inclusivos.
ex_assert_between() {
	local v="$1" lo="$2" hi="$3" what="$4"
	case "$v$lo$hi" in "" | *[!0-9-]*) ex_fail "$what: valor não inteiro ($v, $lo, $hi)" ;; esac
	if [ "$v" -lt "$lo" ] || [ "$v" -gt "$hi" ]; then
		ex_fail "$what = $v fora de [$lo, $hi]"
	fi
	printf 'ok  %s = %s em [%s, %s]\n' "$what" "$v" "$lo" "$hi"
}

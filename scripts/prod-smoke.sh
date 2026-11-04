#!/usr/bin/env bash
# Smoke externo da produção (ADR 0017, spec 019). Roda de qualquer máquina:
# chama cada URL HTTPS e cada porta 8000-8008 com uma chave wwz-key-*.
#
#   WWZ_PROD_KEYS_FILE=~/prod-credentials.md scripts/prod-smoke.sh
#   scripts/prod-smoke.sh --ssh              # lê CREDENTIALS.md do servidor por ssh, só em memória
#
# O arquivo/ssh precisa ter linhas NOME=valor para WWZ_KEY_1 e WWZ_ADMIN_TOKEN
# (formato do CREDENTIALS.md). As chaves nunca vão ao disco local, ao stdout nem
# à linha de comando do curl (vão por `curl -K -`); a saída tem só nome, código e tempo.
#
# Opções: --ssh, --only-https, --only-ports, --only-claude-code (só as checagens
# do Claude Code na 8004 e na 8008), --no-pending (gemini/azure não montados
# contam como falha).
#
# Dados do servidor (ADR 0076): vêm de autor/prod.env (fora do git; WWZ_AUTHOR_ENV
# troca o caminho; modelo em deploy/prod/autor.env.example), sem padrão no código;
# variável já no ambiente vence o arquivo. WWZ_PROD_SSH e WWZ_PROD_SSH_PORT (só com
# --ssh), WWZ_PROD_PUBLIC_IP (portas 8000-8008) e WWZ_HOST_INFERENCIA/ADMIN/PANEL/
# GRAFANA (HTTPS). Cada bloco exige só o que usa.
#
# Outras variáveis: SMOKE_IP (WWZ_PROD_PUBLIC_IP), SMOKE_HOST_WWZ/ADMIN/PANEL/GRAFANA
# (WWZ_HOST_*), SMOKE_MODEL (wwz-fast),
# SMOKE_AZURE_DEPLOYMENT (wwz-fast), SMOKE_ERR_N (200 requisições na 8001),
# SMOKE_CC_URL (raiz com a assinatura, http://$SMOKE_IP:8004), SMOKE_CC_STRICT_URL
# (base do modo estrito, http://$SMOKE_IP:8008), SMOKE_CC_MODEL (claude-sonnet-5-5), WWZ_LOAD_BIN
# (wwz-load pronto; sem ela, o do PATH ou `go build ./cmd/wwz-load` num diretório
# temporário).
#
# Claude Code (ADR 0074, spec 020): `wwz-load claude-code login --manual
# --setup-token` emite um token sk-ant-oat01-… do próprio wwz (o segredo fica no
# servidor; o token vai só ao --state num diretório temporário 0700, apagado na
# saída, e ao curl por `-K -`). Na 8004 (raiz; pode estar em `mode http` num
# haproxy próprio do servidor): hello, login, `ask` e o anthropic-ratelimit-unified-status do
# assinante (também em /anthropic no HTTPS). Na 8008 (mesma instância, TCP puro):
# `ask` em /claude-code (sdk-cli e cli) e o curl comum recusado com 403 e
# x-wwz-client-verification: failed.
# Sai 0 se nada falhou (pendentes não falham); 1 com a lista de URLs que quebraram.
set -uo pipefail

# author_env ARQUIVO: as variáveis WWZ_* de autor/prod.env (sintaxe de shell, lida
# num bash limpo) que o ambiente ainda não tem (vazia conta como ausente).
author_env() {
	local f=$1 k v
	[[ -n $f && -f $f ]] || return 0
	env -i "${BASH:-bash}" -n "$f" || { echo "prod-smoke: $f: erro de sintaxe" >&2; exit 2; }
	while IFS= read -r -d '' k && IFS= read -r -d '' v; do
		[[ -n ${!k:-} ]] || printf -v "$k" '%s' "$v"
	done < <(env -i "${BASH:-bash}" -c 'source "$1" >/dev/null || exit 1
		for k in $(compgen -v WWZ_); do printf "%s\0%s\0" "$k" "${!k}"; done' _ "$f")
}
author_env "${WWZ_AUTHOR_ENV:-$(cd "$(dirname "$0")/.." && pwd)/autor/prod.env}"

# need NOME VALOR...: falha citando autor/prod.env quando falta valor do autor.
need() {
	local miss=""
	while (($# >= 2)); do
		[[ -n $2 ]] || miss="$miss $1"
		shift 2
	done
	[[ -z $miss ]] || {
		echo "prod-smoke: sem$miss: defina no ambiente ou em autor/prod.env (WWZ_AUTHOR_ENV troca o caminho; modelo em deploy/prod/autor.env.example)" >&2
		exit 2
	}
}

SSH_HOST="${WWZ_PROD_SSH:-}"
SSH_PORT="${WWZ_PROD_SSH_PORT:-}"
IP="${SMOKE_IP:-${WWZ_PROD_PUBLIC_IP:-}}"
H_WWZ="${SMOKE_HOST_WWZ:-${WWZ_HOST_INFERENCIA:-}}"
H_ADMIN="${SMOKE_HOST_ADMIN:-${WWZ_HOST_ADMIN:-}}"
H_PANEL="${SMOKE_HOST_PANEL:-${WWZ_HOST_PANEL:-}}"
H_GRAFANA="${SMOKE_HOST_GRAFANA:-${WWZ_HOST_GRAFANA:-}}"
MODEL="${SMOKE_MODEL:-wwz-fast}"
AZ_DEPLOY="${SMOKE_AZURE_DEPLOYMENT:-wwz-fast}"
ERR_N="${SMOKE_ERR_N:-200}"
CC="${SMOKE_CC_URL:-http://$IP:8004}"
CCS="${SMOKE_CC_STRICT_URL:-http://$IP:8008}"
CC_MODEL="${SMOKE_CC_MODEL:-claude-sonnet-5-5}"
USE_SSH=0 DO_HTTPS=1 DO_PORTS=1 DO_CC=1 PENDING_OK=1
for a in "$@"; do
	case $a in
	--ssh) USE_SSH=1 ;;
	--only-https) DO_PORTS=0 DO_CC=0 ;;
	--only-ports) DO_HTTPS=0 ;;
	--only-claude-code) DO_HTTPS=0 DO_PORTS=0 DO_CC=1 ;;
	--no-pending) PENDING_OK=0 ;;
	-h | --help) sed -n '2,/^set -uo/p' "$0" | sed '$d;s/^# \{0,1\}//'; exit 0 ;;
	*) echo "prod-smoke: opção desconhecida: $a" >&2; exit 2 ;;
	esac
done
command -v curl >/dev/null || { echo "prod-smoke: curl ausente" >&2; exit 2; }
((USE_SSH)) && need WWZ_PROD_SSH "$SSH_HOST" WWZ_PROD_SSH_PORT "$SSH_PORT"
((DO_HTTPS)) && need WWZ_HOST_INFERENCIA "$H_WWZ" WWZ_HOST_ADMIN "$H_ADMIN" WWZ_HOST_PANEL "$H_PANEL" WWZ_HOST_GRAFANA "$H_GRAFANA"
if ((DO_PORTS)) || { ((DO_CC)) && [[ -z ${SMOKE_CC_URL:-} || -z ${SMOKE_CC_STRICT_URL:-} ]]; }; then
	need WWZ_PROD_PUBLIC_IP "$IP"
fi

# ---- segredos só em memória
if ((USE_SSH)); then
	CREDS="$(ssh -p "$SSH_PORT" -o BatchMode=yes -o ConnectTimeout=15 "$SSH_HOST" 'cat /opt/white-wizard/CREDENTIALS.md')" || { echo "prod-smoke: falha ao ler CREDENTIALS.md por ssh" >&2; exit 2; }
elif [[ -n ${WWZ_PROD_KEYS_FILE:-} && -r $WWZ_PROD_KEYS_FILE ]]; then
	CREDS="$(cat "$WWZ_PROD_KEYS_FILE")"
else
	echo "prod-smoke: defina WWZ_PROD_KEYS_FILE ou use --ssh" >&2
	exit 2
fi
cred() { grep -E "^$1=" <<<"$CREDS" | head -n1 | cut -d= -f2-; }
KEY="$(cred WWZ_KEY_1)"
ADMIN="$(cred WWZ_ADMIN_TOKEN)"
[[ -n $KEY && -n $ADMIN ]] || { echo "prod-smoke: WWZ_KEY_1/WWZ_ADMIN_TOKEN ausentes nas credenciais" >&2; exit 2; }
unset CREDS

FAILED=()
PENDING=()
RESP_BODY="" RESP_CODE="" RESP_TIME="" RESP_HEAD="" TOKEN=""
WANT_HEAD=0 # 1: req também guarda os headers da resposta em RESP_HEAD

# Diretório temporário só deste processo (0700): binário do wwz-load e o
# --state do login do Claude Code. Apagado na saída.
SMOKE_TMP="$(mktemp -d)" || { echo "prod-smoke: mktemp falhou" >&2; exit 2; }
chmod 700 "$SMOKE_TMP"
trap 'rm -rf "$SMOKE_TMP"' EXIT

# req METHOD URL AUTHKIND BODY [extra header ...]
# AUTHKIND: bearer | xapikey | google | apikey | admin | oauth (token do login
# do Claude Code) | none. HEAD usa `curl --head` (sem esperar corpo).
# A chave vai a curl por `-K -` (stdin), nunca na linha de comando nem em arquivo.
req() {
	local method=$1 url=$2 kind=$3 body=${4:-} cfg="" out h
	local -a mflag=(-X "$method") hflag=()
	shift $(($# < 4 ? $# : 4))
	case $kind in
	bearer) cfg+="header = \"Authorization: Bearer $KEY\""$'\n' ;;
	xapikey) cfg+="header = \"x-api-key: $KEY\""$'\n' ;;
	google) cfg+="header = \"x-goog-api-key: $KEY\""$'\n' ;;
	apikey) cfg+="header = \"api-key: $KEY\""$'\n' ;;
	admin) cfg+="header = \"Authorization: Bearer $ADMIN\""$'\n' ;;
	oauth) cfg+="header = \"Authorization: Bearer $TOKEN\""$'\n' ;;
	esac
	for h in "$@"; do cfg+="header = \"$h\""$'\n'; done
	[[ $method == HEAD ]] && mflag=(--head)
	((WANT_HEAD)) && [[ $method != HEAD ]] && hflag=(-D -)
	if [[ -n $body ]]; then
		cfg+='header = "content-type: application/json"'$'\n'
		out="$(printf '%s' "$cfg" | curl -sS -K - --connect-timeout 5 --max-time 60 "${mflag[@]}" ${hflag[@]+"${hflag[@]}"} --data "$body" -w $'\n%{http_code} %{time_total}' "$url" 2>/dev/null)"
	else
		out="$(printf '%s' "$cfg" | curl -sS -K - --connect-timeout 5 --max-time 60 "${mflag[@]}" ${hflag[@]+"${hflag[@]}"} -w $'\n%{http_code} %{time_total}' "$url" 2>/dev/null)"
	fi
	RESP_CODE="$(tail -n1 <<<"$out" | awk '{print $1}')"
	RESP_TIME="$(tail -n1 <<<"$out" | awk '{print $2}')"
	RESP_BODY="$(sed '$d' <<<"$out")"
	RESP_HEAD=""
	if ((WANT_HEAD)); then # cabeça até a primeira linha em branco (CRLF)
		RESP_HEAD="${RESP_BODY%%$'\r\n\r\n'*}"
		RESP_BODY="${RESP_BODY#*$'\r\n\r\n'}"
	fi
	[[ $RESP_CODE =~ ^[0-9]{3}$ ]] || { RESP_CODE=000; RESP_TIME=0; }
}

# has_header NAME [VALUE_PREFIX]: RESP_HEAD tem o header (nome sem caixa).
has_header() {
	local line name val
	while IFS= read -r line; do
		line=${line%$'\r'}
		name=$(tr '[:upper:]' '[:lower:]' <<<"${line%%:*}")
		val=${line#*:}
		val=${val# }
		[[ $name == "$(tr '[:upper:]' '[:lower:]' <<<"$1")" && $val == "${2:-}"* ]] && return 0
	done <<<"$RESP_HEAD"
	return 1
}

report() { printf '%-8s %-44s %s %ss\n' "$1" "$2" "$RESP_CODE" "$RESP_TIME"; }
pass() { report PASS "$1"; }
fail() { report FAIL "$1"; FAILED+=("$1 ($2 -> $RESP_CODE)"); }
pend() { report PENDENTE "$1"; PENDING+=("$1"); }

# expect NAME URL WANT_CODE METHOD KIND BODY [BODY_SUBSTRING] [header...]
expect() {
	local name=$1 url=$2 want=$3 method=$4 kind=$5 body=${6:-} sub=${7:-}
	shift $(($# < 7 ? $# : 7))
	req "$method" "$url" "$kind" "$body" "$@"
	if [[ $RESP_CODE == "$want" && ( -z $sub || $RESP_BODY == *"$sub"* ) ]]; then pass "$name"; else fail "$name" "$url"; fi
}

# expect_head NAME URL WANT_CODE METHOD KIND BODY BODY_SUBSTRING HEADER VALUE_PREFIX [header...]
# Como expect, e exige o header de resposta HEADER começando por VALUE_PREFIX.
expect_head() {
	local name=$1 url=$2 want=$3 method=$4 kind=$5 body=$6 sub=$7 hname=$8 hval=$9
	shift 9
	WANT_HEAD=1
	req "$method" "$url" "$kind" "$body" "$@"
	WANT_HEAD=0
	if [[ $RESP_CODE == "$want" && ( -z $sub || $RESP_BODY == *"$sub"* ) ]] && has_header "$hname" "$hval"; then
		pass "$name"
	else fail "$name" "$url"; fi
}

# expect_pending: 404/405 (rota do driver ainda não montada) vira PENDENTE.
expect_pending() {
	local name=$1 url=$2 method=$3 kind=$4 body=$5 sub=$6
	req "$method" "$url" "$kind" "$body"
	if [[ $RESP_CODE == 200 && $RESP_BODY == *"$sub"* ]]; then pass "$name"
	elif ((PENDING_OK)) && [[ $RESP_CODE == 404 || $RESP_CODE == 405 || $RESP_CODE == 501 ]]; then pend "$name"
	else fail "$name" "$url"; fi
}

CHAT="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":16}"
MSG="{\"model\":\"$MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}"
GEM='{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}'
CCMSG="{\"model\":\"$CC_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}"

# now: segundos com fração (bash 3.2 do macOS não tem EPOCHREALTIME).
now() {
	if [[ -n ${EPOCHREALTIME:-} ]]; then echo "$EPOCHREALTIME"
	else perl -MTime::HiRes=time -e 'printf "%.6f\n", time' 2>/dev/null || date +%s; fi
}

# wwz_load: caminho do wwz-load (WWZ_LOAD_BIN, PATH ou go build do repositório).
WL=""
wwz_load() {
	[[ -n $WL ]] && return 0
	if [[ -n ${WWZ_LOAD_BIN:-} ]]; then WL=$WWZ_LOAD_BIN
	elif command -v wwz-load >/dev/null; then WL=$(command -v wwz-load)
	elif command -v go >/dev/null; then
		(cd "$(dirname "$0")/.." && go build -o "$SMOKE_TMP/wwz-load" ./cmd/wwz-load) >/dev/null 2>&1 && WL="$SMOKE_TMP/wwz-load"
	fi
	[[ -n $WL && -x $WL ]]
}

# cc NAME ACTION TARGET [flag...]: roda `wwz-load claude-code ACTION` com o
# --state temporário; RESP_CODE = 200 com saída 0, senão o HTTP do erro (ou
# exit N). Stdout e stderr ficam em memória e não são impressos.
cc() {
	local name=$1 action=$2 target=$3 t0 err rc
	shift 3
	if ! wwz_load; then
		RESP_CODE="sem-wwz-load" RESP_TIME=0
		fail "$name" "defina WWZ_LOAD_BIN ou instale Go"
		return 1
	fi
	t0=$(now)
	err="$("$WL" claude-code "$action" --target "$target" --state "$SMOKE_TMP/claude-code.json" "$@" 2>&1 >/dev/null)"
	rc=$?
	RESP_TIME=$(awk -v a="$t0" -v b="$(now)" 'BEGIN{printf "%.3f", b-a}')
	if ((rc == 0)); then RESP_CODE=200; pass "$name"; return 0; fi
	RESP_CODE="$(grep -oE 'HTTP [0-9]{3}' <<<"$err" | head -n1 | cut -c6-)"
	[[ -n $RESP_CODE ]] || RESP_CODE="exit$rc"
	fail "$name" "$target"
	return 1
}

# cc_login NAME TARGET: login --setup-token (só user:inference, como o
# `claude setup-token`) e TOKEN = access token do --state, só em memória.
cc_login() {
	TOKEN=""
	rm -f "$SMOKE_TMP/claude-code.json"
	cc "$1" login "$2" --manual --setup-token --timeout 30s || return 1
	TOKEN="$(grep -o '"access_token": *"[^"]*"' "$SMOKE_TMP/claude-code.json" | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')"
	[[ $TOKEN == sk-ant-oat01-* ]] || { RESP_CODE=sem-token; fail "$1 (token no --state)" "$2"; return 1; }
}

or_model() { # id do catálogo do OpenRouter para $MODEL (exige id com autor)
	local base=$1 id
	req GET "$base/models" bearer ""
	# grep -o: o corpo vem numa linha só; o sed guloso pegava o último id (wwz-flaky).
	id="$(grep -o "\"id\": *\"[^\"]*/$MODEL\"" <<<"$RESP_BODY" | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')"
	echo "${id:-white-wizard/$MODEL}"
}

if ((DO_HTTPS)); then
	echo "== HTTPS"
	W="https://$H_WWZ"
	expect "openai chat (sem chave -> 401)" "$W/openai/v1/chat/completions" 401 POST none "$CHAT"
	expect "openai chat" "$W/openai/v1/chat/completions" 200 POST bearer "$CHAT" '"choices"'
	expect "anthropic messages" "$W/anthropic/v1/messages" 200 POST xapikey "$MSG" '"content"' "anthropic-version: 2023-06-01"
	ORM="$(or_model "$W/openrouter/api/v1")"
	expect "openrouter chat" "$W/openrouter/api/v1/chat/completions" 200 POST bearer "{\"model\":\"$ORM\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":16}" '"choices"'
	expect "opencode chat" "$W/opencode/zen/go/v1/chat/completions" 200 POST bearer "$CHAT" '"choices"' "x-opencode-session: ses-smoke"
	expect_pending "gemini generateContent" "$W/gemini/v1beta/models/$MODEL:generateContent" POST google "$GEM" '"candidates"'
	expect_pending "azure chat (deployment $AZ_DEPLOY)" "$W/azure/openai/deployments/$AZ_DEPLOY/chat/completions?api-version=2024-10-21" POST apikey "$CHAT" '"choices"'
	expect "admin /admin/config sem token" "https://$H_ADMIN/admin/config" 401 GET none
	expect "admin /admin/config com token" "https://$H_ADMIN/admin/config" 200 GET admin
	expect "painel /health/ready" "https://$H_PANEL/health/ready" 200 GET none
	expect "grafana /login" "https://$H_GRAFANA/login" 200 GET none
	# Assinatura OAuth em /anthropic (sem modo estrito: atrás do Traefik).
	if cc_login "anthropic login OAuth --setup-token" "$W/anthropic"; then
		expect_head "anthropic assinante (unified-status)" "$W/anthropic/v1/messages" 200 POST oauth "$CCMSG" '"content"' \
			anthropic-ratelimit-unified-status allowed "anthropic-version: 2023-06-01" "anthropic-beta: oauth-2025-04-20"
	fi
fi

# Pré-checagem: porta fechada (wwz-ports parado ou firewall externo) falha uma vez só.
if ((DO_PORTS)) && ! curl -s --connect-timeout 5 -m 8 -o /dev/null "http://$IP:8000/"; then
	fail "portas 8000-8008: $IP:8000 não conecta (confira o container wwz-ports e o firewall externo)" "http://$IP:8000"
	DO_PORTS=0
	[[ -n ${SMOKE_CC_URL:-} && -n ${SMOKE_CC_STRICT_URL:-} ]] || DO_CC=0 # 8004 e 8008 passam pelo mesmo wwz-ports
fi

if ((DO_PORTS)); then
	echo "== Portas diretas em $IP"
	P() { echo "http://$IP:$1"; }
	expect "8000 openai-perfeito" "$(P 8000)/v1/chat/completions" 200 POST bearer "$CHAT" '"choices"'

	# 8001: ~20% de erros (binomial n=200, p=0,2: sigma ~2,8%; margem de 10% a 30%).
	ok=0 bad=0 i=0
	while ((i < ERR_N)); do
		req POST "$(P 8001)/v1/chat/completions" bearer "$CHAT"
		if [[ $RESP_CODE == 200 ]]; then ok=$((ok + 1)); else bad=$((bad + 1)); fi
		i=$((i + 1))
	done
	pct=$((bad * 100 / ERR_N))
	RESP_CODE="$pct%"; RESP_TIME=0
	if ((pct >= 10 && pct <= 30)); then pass "8001 falhas-20pct (erros: $bad/$ERR_N)"; else fail "8001 falhas-20pct (erros: $bad/$ERR_N)" "$(P 8001)"; fi

	# 8002: latência de cauda pesada; exige 200 em todas e informa o mais lento.
	slow=0 allok=1 i=0 maxt=0
	while ((i < 20)); do
		req POST "$(P 8002)/v1/chat/completions" bearer "$CHAT"
		[[ $RESP_CODE == 200 ]] || allok=0
		maxt=$(awk -v a="$maxt" -v b="$RESP_TIME" 'BEGIN{print (b>a)?b:a}')
		i=$((i + 1))
	done
	RESP_CODE=$([[ $allok == 1 ]] && echo 200 || echo erro); RESP_TIME=$maxt
	if ((allok)); then pass "8002 latencia-intermitente (20 req, max)"; else fail "8002 latencia-intermitente" "$(P 8002)"; fi

	# 8003: janela de 2 min a cada 10 min em UTC (minutos 0-1 de cada dezena).
	nowm=$(date -u +%-M) nows=$(date -u +%-S)
	cyc=$(((nowm % 10) * 60 + nows))
	req POST "$(P 8003)/v1/chat/completions" bearer "$CHAT"
	near=0
	for edge in 0 120 600; do d=$((cyc - edge)); ((d < 0)) && d=$((-d)); ((d <= 4)) && near=1; done
	if ((cyc < 120)); then want="5xx"; else want=200; fi
	if ((near)); then
		pass "8003 indisponibilidade (limite da janela, qualquer resposta)"
	elif [[ $want == 200 && $RESP_CODE == 200 ]] || [[ $want == 5xx && $RESP_CODE == 5?? ]]; then
		pass "8003 indisponibilidade (esperado $want)"
	else fail "8003 indisponibilidade (esperado $want)" "$(P 8003)"; fi

	expect "8004 anthropic" "$(P 8004)/v1/messages" 200 POST xapikey "$MSG" '"content"' "anthropic-version: 2023-06-01"
	ORM="$(or_model "$(P 8005)/api/v1")"
	expect "8005 openrouter" "$(P 8005)/api/v1/chat/completions" 200 POST bearer "{\"model\":\"$ORM\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":16}" '"choices"'
	expect "8006 opencode" "$(P 8006)/zen/go/v1/chat/completions" 200 POST bearer "$CHAT" '"choices"' "x-opencode-session: ses-smoke"
	expect "8007 admin sem token" "$(P 8007)/admin/config" 401 GET none
	expect "8007 admin com token" "$(P 8007)/admin/config" 200 GET admin
	expect "8008 anthropic (TCP, modo estrito em /claude-code)" "$(P 8008)/v1/messages" 200 POST xapikey "$MSG" '"content"' "anthropic-version: 2023-06-01"
fi

# Claude Code (ADR 0074, spec 020): a raiz com a assinatura na 8004 e o modo
# estrito em /claude-code na 8008. A 8004 não serve o estrito: um haproxy
# próprio do servidor pode atendê-la em `mode http` (outro serviço pelo Host),
# que tira `Connection` e reescreve a cabeça. As duas portas vão à mesma instância, então o token vale nas duas.
if ((DO_CC)); then
	echo "== Claude Code: raiz em $CC, estrito em $CCS/claude-code"
	expect "8004 GET /api/hello" "$CC/api/hello" 200 GET none "" '"hello"'
	expect "8008 HEAD /claude-code/api/hello (sonda do Claude Code)" "$CCS/claude-code/api/hello" 200 HEAD none
	if cc_login "8004 login OAuth --setup-token" "$CC"; then
		cc "8004 ask (raiz, sdk-cli)" ask "$CC" --retries 0 --timeout 60s ping
		expect_head "8004 assinante (unified-status)" "$CC/v1/messages" 200 POST oauth "$CCMSG" '"content"' \
			anthropic-ratelimit-unified-status allowed "anthropic-version: 2023-06-01" "anthropic-beta: oauth-2025-04-20"
		cc "8008 ask /claude-code (estrito, sdk-cli)" ask "$CCS/claude-code" --retries 0 --timeout 60s ping
		cc "8008 ask /claude-code (estrito, cli)" ask "$CCS/claude-code" --entrypoint cli --retries 0 --timeout 60s ping
	fi
	# Um cliente comum (curl, SDK) no mount estrito: 403 antes da credencial.
	expect_head "8008 /claude-code curl comum -> 403" "$CCS/claude-code/v1/messages" 403 POST xapikey "$CCMSG" forged_request_error \
		x-wwz-client-verification failed "anthropic-version: 2023-06-01"
fi
TOKEN=""

echo
if ((${#PENDING[@]})); then
	echo "Pendentes (rota do driver não montada: 404/405):"
	printf '  - %s\n' "${PENDING[@]}"
fi
if ((${#FAILED[@]})); then
	echo "FALHOU:"
	printf '  - %s\n' "${FAILED[@]}"
	exit 1
fi
echo "smoke de produção: ok"

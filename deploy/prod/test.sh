#!/usr/bin/env bash
# Testes do deploy de produção (task 207). Não acessa o servidor.
#   deploy/prod/test.sh            (de qualquer pasta)
# Requer docker (compose), jq e Go; sem docker, os testes do compose são pulados.
# WWZ_BIN=/caminho/wwz usa um binário pronto no lugar de `go build`.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
DEPLOY="$REPO/scripts/prod-deploy.sh"
SMOKE="$REPO/scripts/prod-smoke.sh"
export PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILS=0
ok() { echo "ok    $*"; }
bad() { echo "FAIL  $*"; FAILS=$((FAILS + 1)); }
skip() { echo "skip  $*"; }
check() { local name=$1; shift; if "$@"; then ok "$name"; else bad "$name"; fi; }

# ---- sintaxe e shellcheck
check "bash -n prod-deploy.sh" bash -n "$DEPLOY"
check "bash -n prod-smoke.sh" bash -n "$SMOKE"
check "bash -n test.sh" bash -n "$HERE/test.sh"
if command -v shellcheck >/dev/null; then
	check "shellcheck" shellcheck -S warning "$DEPLOY" "$SMOKE"
else
	skip "shellcheck ausente"
fi

# ---- dados do autor (ADR 0076): autor/prod.env fora do git, lido antes de
# isolar o resto do teste nos valores de exemplo.
AUTHOR_ENV="${WWZ_AUTHOR_ENV:-$REPO/autor/prod.env}"
AUTHOR_HA=""
AUTHOR_VALS=""
# author_get ARQUIVO VAR: valor de VAR num arquivo em sintaxe de shell (bash limpo).
author_get() { env -i "${BASH:-bash}" -c 'source "$1" >/dev/null 2>&1; eval "printf %s \"\${$2:-}\""' _ "$1" "$2"; }
if [[ -f $AUTHOR_ENV ]]; then
	check "autor/prod.env: sintaxe de shell ($AUTHOR_ENV)" env -i "${BASH:-bash}" -n "$AUTHOR_ENV"
	for k in WWZ_PROD_SSH WWZ_PROD_SSH_PORT WWZ_PROD_PUBLIC_IP WWZ_HOST_INFERENCIA WWZ_HOST_ADMIN WWZ_HOST_PANEL WWZ_HOST_GRAFANA WWZ_HEAVY_SSH; do
		check "autor/prod.env define $k" test -n "$(author_get "$AUTHOR_ENV" "$k")"
	done
	for k in WWZ_PROD_SSH WWZ_PROD_PUBLIC_IP WWZ_PROD_PRIVATE_IP WWZ_HOST_INFERENCIA WWZ_HOST_ADMIN WWZ_HOST_PANEL WWZ_HOST_GRAFANA; do
		v="$(author_get "$AUTHOR_ENV" "$k")"
		v="${v#*@}"
		[[ -z $v ]] || AUTHOR_VALS+="$v"$'\n'
	done
	p="$(author_get "$AUTHOR_ENV" WWZ_PROD_HAPROXY)"
	if [[ -n $p && $p != /* ]]; then
		if [[ -f $REPO/$p ]]; then p="$REPO/$p"; else p="$(cd "$(dirname "$AUTHOR_ENV")/.." && pwd)/$p"; fi
	fi
	if [[ -n $p ]]; then
		check "WWZ_PROD_HAPROXY de autor/prod.env existe ($p)" test -f "$p"
		[[ -f $p ]] && AUTHOR_HA=$p
	fi
else
	skip "autor/prod.env ausente ($AUTHOR_ENV): conferências dos dados do autor puladas"
fi
check "deploy/prod/autor.env.example: sintaxe de shell" env -i "${BASH:-bash}" -n "$HERE/autor.env.example"
check "autor.env.example: hosts de exemplo (*.example.com, 203.0.113.10)" \
	test "$(author_get "$HERE/autor.env.example" WWZ_HOST_INFERENCIA)$(author_get "$HERE/autor.env.example" WWZ_PROD_PUBLIC_IP)" = wwz.example.com203.0.113.10
check "/autor/ no .gitignore" grep -qxF '/autor/' "$REPO/.gitignore"
# Daqui em diante, só os valores de exemplo: nada do ambiente nem do autor.
unset WWZ_PROD_SSH WWZ_PROD_SSH_PORT WWZ_PROD_PUBLIC_IP WWZ_PROD_PRIVATE_IP WWZ_HEAVY_SSH WWZ_PROD_HAPROXY \
	WWZ_HOST_INFERENCIA WWZ_HOST_ADMIN WWZ_HOST_PANEL WWZ_HOST_GRAFANA
export WWZ_AUTHOR_ENV="$HERE/autor.env.example"

# ---- haproxy (wwz-ports): o genérico do repositório e, se houver, o do autor
HA="$HERE/haproxy/haproxy.cfg"
# Só os blocos `listen`: porta do bind -> servidor padrão.
want="8000:198.18.128.11:8000 8001:198.18.128.12:8000 8002:198.18.128.13:8000 8003:198.18.128.14:8000 8004:198.18.128.15:8000 8005:198.18.128.16:8000 8006:198.18.128.17:8000 8007:198.18.128.18:9000 8008:198.18.128.15:8000"
listens() { awk '/^listen /{l=1} /^backend /{l=0} l&&/bind 0.0.0.0:/{split($2,a,":");p=a[2]} l&&/server s /{print p":"$3}' "$1" | tr '\n' ' '; }
block() { awk -v n="$1" '$1=="listen"||$1=="backend"{on=($2==n)} on' "${2:-$HA}"; }
check "haproxy: 8000-8008 -> IPs/portas dos papéis" test "$(listens "$HA")" = "$want "
check "haproxy: p8008 em mode tcp (modo estrito precisa da cabeça crua)" test "$(block p8008 | grep -c '^ *mode tcp$')" = 1 -a -z "$(block p8008 | grep -E 'mode http|option' || true)"
check "haproxy genérico: só blocos listen do wwz (nenhum backend que não é do wwz)" \
	test "$(grep -cE '^(backend|frontend) ' "$HA")$(grep -cE '^listen ' "$HA")" = 09
check "haproxy genérico: tudo em TCP (nenhum mode http, acl ou use_backend)" \
	test -z "$(grep -vE '^[[:space:]]*#' "$HA" | grep -E 'mode http|acl |use_backend|option forwardfor' || true)"
if [[ -n $AUTHOR_HA ]]; then
	check "haproxy do autor: 8000-8008 -> mesmos IPs/portas dos papéis" test "$(listens "$AUTHOR_HA")" = "$want "
	check "haproxy do autor: p8008 em mode tcp, sem opção de http" \
		test "$(block p8008 "$AUTHOR_HA" | grep -c '^ *mode tcp$')" = 1 -a -z "$(block p8008 "$AUTHOR_HA" | grep -E 'mode http|option' || true)"
	check "haproxy do autor: só as portas 8000-8008 em bind" \
		test "$(grep -oE 'bind 0\.0\.0\.0:[0-9]+' "$AUTHOR_HA" | cut -d: -f2 | sort -n | tr '\n' ' ')" = "8000 8001 8002 8003 8004 8005 8006 8007 8008 "
else
	skip "sem haproxy.cfg do autor (WWZ_PROD_HAPROXY): conferência pulada"
fi
if command -v haproxy >/dev/null; then
	check "haproxy -c (genérico)" haproxy -c -q -f "$HA"
	[[ -z $AUTHOR_HA ]] || check "haproxy -c (autor)" haproxy -c -q -f "$AUTHOR_HA"
else
	skip "haproxy ausente: haproxy -c pulado (o sync confere no servidor)"
fi

# ---- compose
if command -v docker >/dev/null && docker compose version >/dev/null 2>&1 && command -v jq >/dev/null; then
	if docker compose --project-directory "$HERE" -f "$HERE/docker-compose.yml" --env-file "$HERE/.env.example" config --format json >"$TMP/c.json" 2>"$TMP/err"; then
		ok "docker compose config válido"
	else
		bad "docker compose config: $(cat "$TMP/err")"
	fi
	C="$TMP/c.json"
	if [[ -s $C ]]; then
		check "projeto compose chama wwz" test "$(jq -r .name "$C")" = wwz
		check "nenhum serviço usa ports: (Docker sem iptables/userland-proxy)" test -z "$(jq -r '.services|to_entries[]|select((.value.ports//[])|length>0)|.key' "$C")"
		check "wwz-ports em network_mode host" test "$(jq -r '.services["wwz-ports"].network_mode' "$C")" = host
		check "IPs fixos do compose cobrem os destinos do haproxy" test "$(tr ' ' '\n' <<<"$want" | cut -d: -f2 | sort -u | tr '\n' ' ')" = \
			"$(jq -r '.services|to_entries[]|select(.value.networks|has("internal"))|.value.networks.internal.ipv4_address//empty' "$C" | sort | tr '\n' ' ')"
		check "valkey e prometheus sem ports" test "$(jq -r '[.services["wwz-valkey"],.services["wwz-prometheus"]|(.ports//[])|length]|add' "$C")" = 0
		check "só admin, grafana e painel entram na network_public" test "$(jq -r '[.services|to_entries[]|select(.value.networks|has("network_public"))|.key]|sort|join(",")' "$C")" = "wwz-admin,wwz-grafana,wwz-panel"
		check "nomes de container começam com wwz" test -z "$(jq -r '.services|to_entries[]|select((.value.container_name//"")|startswith("wwz")|not)|.key' "$C")"
		check "nomes de serviço começam com wwz" test -z "$(jq -r '.services|keys[]|select(startswith("wwz")|not)' "$C")"
		check "volumes começam com wwz" test -z "$(jq -r '.volumes|to_entries[]|select(.value.name|startswith("wwz")|not)|.key' "$C")"
		check "redes: wwz-internal e network_public externa" test "$(jq -r '[.networks|to_entries[]|select((.value.name|startswith("wwz"))or(.key=="network_public" and .value.external==true))]|length' "$C")" = 2
		cpus="$(jq '[.services[]|.cpus|tonumber]|add' "$C")"
		mem="$(jq '[.services[]|.mem_limit|tonumber]|add/1073741824' "$C")"
		check "soma de cpus <= 32 ($cpus)" awk -v c="$cpus" 'BEGIN{exit !(c<=32)}'
		check "soma de memória <= 64 GiB ($mem)" awk -v m="$mem" 'BEGIN{exit !(m<=64)}'
		check "todos os serviços com cpus e mem_limit" test -z "$(jq -r '.services|to_entries[]|select((.value.cpus//null)==null or (.value.mem_limit//null)==null)|.key' "$C")"
		check "todos read_only" test -z "$(jq -r '.services|to_entries[]|select(.value.read_only!=true)|.key' "$C")"
		check "haproxy só escuta portas > 1024" test -z "$(grep -oE 'bind 0.0.0.0:[0-9]+' "$HERE/haproxy/haproxy.cfg" | cut -d: -f2 | awk '$1<=1024')"
		check "routers Traefik wwz-* com letsencrypt" test "$(jq -r '[.services[].labels//{}|to_entries[]|select(.key|test("^traefik\\.http\\.routers\\.wwz-[a-z]+\\.tls\\.certresolver$"))|select(.value=="letsencrypt")]|length' "$C")" = 4
		check "admin.debug fora da produção (compose não monta o de dev)" test -z "$(grep -l 'deploy/wwz/wwz.yaml' "$HERE/docker-compose.yml" || true)"
		check "routers Traefik com os hosts do .env.example (WWZ_HOST_*)" \
			test "$(jq -r '[.services[].labels//{}|to_entries[]|select(.key|test("^traefik\\.http\\.routers\\.wwz-[a-z]+\\.rule$"))|.value]|sort|join(" ")' "$C")" = \
			'Host(`admin-wwz.example.com`) Host(`grafana-wwz.example.com`) Host(`panel-wwz.example.com`) Host(`wwz.example.com`)'
		grep -v '^WWZ_HOST_' "$HERE/.env.example" >"$TMP/nohost.env"
		check "compose sem WWZ_HOST_* falha (nenhum host padrão)" eval '! env -u WWZ_HOST_INFERENCIA -u WWZ_HOST_ADMIN -u WWZ_HOST_PANEL -u WWZ_HOST_GRAFANA \
			docker compose --project-directory "$HERE" -f "$HERE/docker-compose.yml" --env-file "$TMP/nohost.env" config -q >/dev/null 2>&1'
	fi
else
	skip "docker compose ou jq ausentes: testes do compose pulados"
fi

# ---- wwz validate em todas as configs (chaves fictícias)
WWZ="${WWZ_BIN:-}"
if [[ -z $WWZ ]]; then
	WWZ="$TMP/wwz"
	(cd "$REPO" && go build -o "$WWZ" ./cmd/wwz) || bad "go build ./cmd/wwz"
fi
mkdir -p "$TMP/root"
cp -R "$HERE/config" "$TMP/root/config"
grep -E '^(WWZ_KEY_[123]|WWZ_OAUTH_KEY|WWZ_ADMIN_TOKEN)=' "$HERE/.env.example" >"$TMP/root/.env"
OAUTH_V="$(grep '^WWZ_OAUTH_KEY=' "$HERE/.env.example" | cut -d= -f2-)"
if WWZ_PROD_ROOT="$TMP/root" "$DEPLOY" _render 2>/dev/null; then
	ok "render das configs"
else
	bad "render das configs"
fi
for f in "$TMP"/root/run/config/*.yaml; do
	n="$(basename "$f")"
	if [[ -x $WWZ ]] && "$WWZ" validate -config "$f" >"$TMP/v.out" 2>&1; then
		ok "wwz validate $n"
	else
		bad "wwz validate $n: $(tail -2 "$TMP/v.out" 2>/dev/null)"
	fi
done
check "render sem placeholder restante" test -z "$(grep -l '\${' "$TMP"/root/run/config/*.yaml || true)"
check "templates versionados sem chave literal" test -z "$(grep -hE 'key: wwz-key-' "$HERE"/config/*.yaml || true)"
check "templates usam \${WWZ_KEY_n}" test "$(grep -c 'key: \${WWZ_KEY_[123]}' "$HERE/config/todos.yaml")" = 3
# Segredo próprio da assinatura (task 466): só nas configs que servem o Claude Code.
check "\${WWZ_OAUTH_KEY} só em anthropic.yaml e todos.yaml" \
	test "$(grep -l '\${WWZ_OAUTH_KEY}' "$HERE"/config/*.yaml | xargs -n1 basename | sort | tr '\n' ' ')" = "anthropic.yaml todos.yaml "
check "render: valor de WWZ_OAUTH_KEY só nos dois arquivos renderizados" \
	test "$(grep -lF -- "$OAUTH_V" "$TMP"/root/run/config/*.yaml | xargs -n1 basename | sort | tr '\n' ' ')" = "anthropic.yaml todos.yaml "
check "nenhuma config liga admin.debug" test -z "$(grep -hE '^\s*debug:\s*true' "$HERE"/config/*.yaml || true)"

# ---- Claude Code (ADR 0074, spec 020): assinatura OAuth na 8004, modo estrito na 8008
A="$HERE/config/anthropic.yaml"
T="$HERE/config/todos.yaml"
check "anthropic.yaml usa \${WWZ_KEY_n} nas 3 chaves" test "$(grep -c 'key: \${WWZ_KEY_[123]}' "$A")" = 3
check "oauth_key é o label claude-code-oauth (nunca prod-n nem valor)" \
	test -z "$(grep -hoE 'oauth_key: [^,}]+' "$A" "$T" | grep -vE '^oauth_key: claude-code-oauth$' || true)"
check "nenhuma config usa prod-n como oauth_key" test -z "$(grep -hE 'oauth_key: *prod-' "$HERE"/config/*.yaml || true)"
oauth_entry='^  - \{key: \$\{WWZ_OAUTH_KEY\}, label: claude-code-oauth, account: acct-claude-max, project: proj-claude-code, models: \[claude-sonnet-5-5, claude-opus-5-5, claude-haiku-5-5\]\}$'
for f in "$A" "$T"; do
	n="$(basename "$f")"
	check "$n: entrada claude-code-oauth com \${WWZ_OAUTH_KEY}, conta própria e só claude-*" test "$(grep -cE "$oauth_entry" "$f")" = 1
	check "$n: WWZ_KEY_1..3 na acct-prod (nenhuma na conta da assinatura)" \
		test "$(grep -cE '^  - \{key: \$\{WWZ_KEY_[123]\}, label: prod-[123], account: acct-prod, ' "$f")" = 3
	check "$n: conta da assinatura com quota_usd" grep -qE 'quota_usd: \{monthly_usd: [0-9]+' "$f"
done
check "anthropic.yaml: raiz com assinatura e /claude-code estrito" \
	test "$(grep -cE '^  - \{name: anthropic, (prefix: /claude-code, )?options: \{oauth_key: claude-code-oauth, oauth_plan: max_20x(, client_verification: claude-code)?\}\}$' "$A")" = 2
check "anthropic.yaml: client_verification só em /claude-code" \
	test "$(grep -c 'client_verification: claude-code' "$A")" = 1 -a "$(grep -c 'prefix: /claude-code, .*client_verification: claude-code' "$A")" = 1
check "todos.yaml: /anthropic com oauth_key e sem modo estrito (atrás do Traefik)" \
	test "$(grep -c 'prefix: /anthropic, options: {oauth_key: claude-code-oauth' "$HERE/config/todos.yaml")" = 1 -a -z "$(grep -vE '^[[:space:]]*#' "$HERE/config/todos.yaml" | grep 'client_verification' || true)"
for m in claude-sonnet-5-5 claude-opus-5-5 claude-haiku-5-5; do
	check "modelo $m em anthropic.yaml e todos.yaml" test "$(grep -lE "^  - id: $m\$" "$A" "$HERE/config/todos.yaml" | wc -l | tr -d ' ')" = 2
done
check "prod-smoke.sh não passa token nem chave na linha de comando" \
	test -z "$(grep -nE -- '--(token|key)[ =]|echo[^#]*\$\{?(TOKEN|KEY|ADMIN)\b' "$SMOKE" || true)"

# ---- smoke do Claude Code contra um wwz local com o anthropic.yaml renderizado
# (chaves fictícias; o mesmo bloco que roda na 8004 e na 8008 depois do deploy).
CCPORT=$((20000 + $$ % 20000))
if [[ -x $WWZ && -f $TMP/root/run/config/anthropic.yaml ]] && command -v go >/dev/null; then
	WL="${WWZ_LOAD_BIN:-$TMP/wwz-load}"
	[[ -x $WL ]] || (cd "$REPO" && go build -o "$WL" ./cmd/wwz-load) || bad "go build ./cmd/wwz-load"
	"$WWZ" serve -config "$TMP/root/run/config/anthropic.yaml" -listen "127.0.0.1:$CCPORT" \
		-admin-listen "127.0.0.1:$((CCPORT + 1))" -log-level error >"$TMP/cc-wwz.log" 2>&1 &
	ccpid=$!
	for _ in $(seq 1 50); do curl -s -o /dev/null "http://127.0.0.1:$CCPORT/api/hello" && break; sleep 0.2; done
	grep -E '^(WWZ_KEY_1|WWZ_ADMIN_TOKEN)=' "$HERE/.env.example" >"$TMP/cc-creds"
	if WWZ_LOAD_BIN="$WL" WWZ_PROD_KEYS_FILE="$TMP/cc-creds" SMOKE_CC_URL="http://127.0.0.1:$CCPORT" SMOKE_CC_STRICT_URL="http://127.0.0.1:$CCPORT" \
		"$SMOKE" --only-claude-code >"$TMP/cc-smoke.out" 2>&1; then
		ok "prod-smoke.sh --only-claude-code contra wwz local ($(grep -c '^PASS' "$TMP/cc-smoke.out") verificações)"
	else
		bad "prod-smoke.sh --only-claude-code contra wwz local: $(grep -E '^(FAIL|prod-smoke)' "$TMP/cc-smoke.out" | tr '\n' ';')"
	fi
	leak=0
	while IFS='=' read -r k v; do [[ -n $v ]] && grep -qF -- "$v" "$TMP/cc-smoke.out" && leak=1; done <"$TMP/cc-creds"
	grep -q 'sk-ant-oat01-' "$TMP/cc-smoke.out" && leak=1
	check "smoke do Claude Code não imprime chave nem token" test "$leak" = 0
	# create_api_key (consentimento automático) devolve o segredo da assinatura:
	# precisa ser WWZ_OAUTH_KEY, nunca uma WWZ_KEY_n, e só valer nos claude-*.
	CCB="http://127.0.0.1:$CCPORT"
	raw=""
	if "$WL" claude-code login --manual --target "$CCB" --state "$TMP/cc-full.json" --timeout 30s >/dev/null 2>&1; then
		tok="$(grep -o '"access_token": *"[^"]*"' "$TMP/cc-full.json" | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')"
		raw="$(printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -s -K - -X POST -H 'anthropic-beta: oauth-2025-04-20' \
			"$CCB/api/oauth/claude_cli/create_api_key" | sed -n 's/.*"raw_key": *"\([^"]*\)".*/\1/p')"
		tok=""
	fi
	check "create_api_key devolve WWZ_OAUTH_KEY" test -n "$raw" -a "$raw" = "$OAUTH_V"
	same=0
	for i in 1 2 3; do [[ $raw == "$(grep "^WWZ_KEY_$i=" "$HERE/.env.example" | cut -d= -f2-)" ]] && same=1; done
	check "create_api_key não devolve nenhuma WWZ_KEY_n" test "$same" = 0
	ccmsg() { printf 'header = "x-api-key: %s"\n' "$raw" | curl -s -o /dev/null -w '%{http_code}' -K - -X POST \
		-H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' "$CCB/v1/messages" \
		-d "{\"model\":\"$1\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}"; }
	check "chave do create_api_key vale em claude-sonnet-5-5" test "$(ccmsg claude-sonnet-5-5)" = 200
	code="$(ccmsg wwz-fast)"
	check "chave do create_api_key recusada em wwz-fast ($code)" test "$code" != 200 -a "$code" != 000
	raw=""
	kill "$ccpid" 2>/dev/null
	wait "$ccpid" 2>/dev/null
else
	skip "smoke do Claude Code local (sem wwz ou Go)"
fi

# ---- credentials: arquivos 600, idempotente, sem imprimir valores
CR="$TMP/cred"
out1="$(WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$CR" bash -c 'source "$1"; _credentials' _ "$DEPLOY" 2>&1)"
sum1="$(cksum "$CR/.env" "$CR/CREDENTIALS.md")"
out2="$(WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$CR" bash -c 'source "$1"; _credentials' _ "$DEPLOY" 2>&1)"
sum2="$(cksum "$CR/.env" "$CR/CREDENTIALS.md")"
check "credentials idempotente (segunda rodada não reescreve)" test "$sum1" = "$sum2"
perm() { stat -f %Lp "$1" 2>/dev/null || stat -c %a "$1"; }
check ".env com chmod 600" test "$(perm "$CR/.env")" = 600
check "CREDENTIALS.md com chmod 600" test "$(perm "$CR/CREDENTIALS.md")" = 600
leak=0
while IFS='=' read -r k v; do
	[[ $k =~ ^(WWZ_ADMIN_TOKEN|WWZ_KEY_[123]|WWZ_OAUTH_KEY|WWZ_PANEL_ADMIN_PASSWORD|GRAFANA_ADMIN_PASSWORD)$ ]] || continue
	[[ $out1$out2 == *"$v"* ]] && leak=1
done <"$CR/.env"
check "credentials não imprime valores" test "$leak" = 0
check "chaves wwz-key- com >= 24 bytes (32 chars base64url)" test "$(env_len=$(grep '^WWZ_KEY_1=' "$CR/.env" | cut -d= -f2- | sed 's/^wwz-key-//' | tr -d '\n' | wc -c); echo $((env_len >= 32)))" = 1
check "chaves distintas (WWZ_KEY_1..3 e WWZ_OAUTH_KEY)" test "$(grep -E '^WWZ_(KEY_[123]|OAUTH_KEY)=' "$CR/.env" | cut -d= -f2 | sort -u | wc -l | tr -d ' ')" = 4
check "WWZ_OAUTH_KEY wwz-key- com >= 32 chars base64url" grep -qE '^WWZ_OAUTH_KEY=wwz-key-[A-Za-z0-9_-]{32,}$' "$CR/.env"
check "CREDENTIALS.md lista WWZ_OAUTH_KEY" grep -qE '^WWZ_OAUTH_KEY=wwz-key-' "$CR/CREDENTIALS.md"
check "token admin e senhas preenchidos" test "$(grep -cE '^(WWZ_ADMIN_TOKEN|WWZ_PANEL_ADMIN_PASSWORD|GRAFANA_ADMIN_PASSWORD|WWZ_PANEL_API_TOKEN|WWZ_PANEL_SESSION_KEY)=.{20,}$' "$CR/.env")" = 5
check "credentials: WWZ_HOST_* de autor.env.example no .env" \
	test "$(grep -E '^WWZ_HOST_' "$CR/.env" | sort | tr '\n' ' ')" = "WWZ_HOST_ADMIN=admin-wwz.example.com WWZ_HOST_GRAFANA=grafana-wwz.example.com WWZ_HOST_INFERENCIA=wwz.example.com WWZ_HOST_PANEL=panel-wwz.example.com "
check "credentials: CREDENTIALS.md com os hosts e o IP dos dados do autor" \
	test "$(grep -cE 'https://admin-wwz\.example\.com/admin/\.\.\. e 203\.0\.113\.10:8007|https://panel-wwz\.example\.com |https://grafana-wwz\.example\.com ' "$CR/CREDENTIALS.md")" = 3
sum0="$(cksum "$CR/.env")"
WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$CR" WWZ_HOST_PANEL=painel.example.org bash -c 'source "$1"; _credentials' _ "$DEPLOY" >/dev/null 2>&1
check "credentials: host novo troca só a linha dele no .env e regrava o CREDENTIALS.md" \
	test "$(grep -c '^WWZ_HOST_PANEL=painel.example.org$' "$CR/.env")$(grep -c '^WWZ_HOST_' "$CR/.env")$(grep -c 'https://painel.example.org ' "$CR/CREDENTIALS.md")" = 141
WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$CR" bash -c 'source "$1"; _credentials' _ "$DEPLOY" >/dev/null 2>&1
check "credentials: de volta ao host do autor.env.example, .env igual ao de antes" test "$(cksum "$CR/.env")" = "$sum0"
NOH="$TMP/cred-nohost"
outn="$(WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$NOH" WWZ_AUTHOR_ENV=/nonexistent bash -c 'source "$1"; _credentials' _ "$DEPLOY" 2>&1)"
check "credentials sem dados do autor falha citando autor/prod.env e o modelo" \
	test $? -ne 0 -a ! -e "$NOH/CREDENTIALS.md" -a -n "$(grep -F 'autor/prod.env' <<<"$outn" | grep -F 'deploy/prod/autor.env.example')"
# valor já existente nunca é reescrito
sed -i.bak 's/^WWZ_KEY_2=.*/WWZ_KEY_2=wwz-key-fixa-do-operador/' "$CR/.env" && rm -f "$CR/.env.bak"
WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$CR" bash -c 'source "$1"; _credentials' _ "$DEPLOY" >/dev/null 2>&1
check "valor existente preservado" grep -q '^WWZ_KEY_2=wwz-key-fixa-do-operador$' "$CR/.env"
# .env de antes da task 466 (sem WWZ_OAUTH_KEY e sem \n final): credentials só
# acrescenta a variável nova; as outras linhas ficam byte a byte.
OLD="$TMP/cred-old"
mkdir -p "$OLD"
grep -v '^WWZ_OAUTH_KEY=' "$CR/.env" | sed '/^GRAFANA_ADMIN_USER=/d' >"$OLD/.env"
printf 'GRAFANA_ADMIN_USER=operador' >>"$OLD/.env"
chmod 644 "$OLD/.env"
cp "$OLD/.env" "$TMP/old.env"
outo="$(WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$OLD" bash -c 'source "$1"; _credentials' _ "$DEPLOY" 2>&1)"
check ".env existente: só WWZ_OAUTH_KEY acrescentada" \
	test "$(diff <(printf '%s\n' "$(cat "$TMP/old.env")") "$OLD/.env" | grep -E '^[<>]' | sed -E 's/=.*//' | tr '\n' ' ')" = "> WWZ_OAUTH_KEY "
check ".env existente: WWZ_OAUTH_KEY nova e distinta das WWZ_KEY_n" \
	test "$(grep -E '^WWZ_(KEY_[123]|OAUTH_KEY)=wwz-key-' "$OLD/.env" | cut -d= -f2 | sort -u | wc -l | tr -d ' ')" = 4
check ".env existente: volta a chmod 600" test "$(perm "$OLD/.env")" = 600
v="$(grep '^WWZ_OAUTH_KEY=' "$OLD/.env" | cut -d= -f2-)"
check ".env existente: valor novo não impresso" test -n "$v" -a "${outo/"$v"/}" = "$outo"
v=""

# ---- sync do haproxy.cfg (task 469): no lugar, mesmo inode, recusa edição à mão
# _haproxy_apply num diretório temporário; haproxy -c e o restart do wwz-ports
# (docker no servidor) trocados por funções de teste.
SY="$TMP/sync"
mkdir -p "$SY/run"
inode() { stat -f %i "$1" 2>/dev/null || stat -c %i "$1"; }
sha() { shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1 || sha256sum "$1" | cut -d' ' -f1; }
# apply [ACCEPT] [CHECK_RC]: roda _haproxy_apply; reloads contados em $SY/reloads.
apply() {
	WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_ROOT="$SY" WWZ_PROD_HAPROXY_ACCEPT="${1:-none}" CHECK_RC="${2:-0}" bash -c '
		source "$1"
		haproxy_check() { return "$CHECK_RC"; }
		haproxy_reload() { echo "$1" >>"$WWZ_PROD_ROOT/reloads"; }
		_haproxy_apply' _ "$DEPLOY" >"$TMP/apply.out" 2>&1
}
reloads() { [[ -f $SY/reloads ]] && wc -l <"$SY/reloads" | tr -d ' ' || echo 0; }
cp "$HA" "$SY/run/haproxy.cfg.next"
check "sync: primeira instalação grava e recarrega" apply
check "sync: primeira instalação: conteúdo, sha256 em run/ e 1 reload" \
	test "$(sha "$SY/haproxy/haproxy.cfg")" = "$(sha "$HA")" -a "$(cat "$SY/run/haproxy.cfg.sha256")" = "$(sha "$HA")" -a "$(reloads)" = 1
ino="$(inode "$SY/haproxy/haproxy.cfg")"
cp "$HA" "$SY/run/haproxy.cfg.next"
apply
check "sync: mesmo conteúdo não recarrega" test "$(reloads)" = 1 -a ! -e "$SY/run/haproxy.cfg.next"
{ cat "$HA"; echo "# mudança do repositório"; } >"$SY/run/haproxy.cfg.next"
apply
check "sync: mudança do repositório grava no mesmo inode e recarrega" \
	test "$(inode "$SY/haproxy/haproxy.cfg")" = "$ino" -a "$(tail -n1 "$SY/haproxy/haproxy.cfg")" = "# mudança do repositório" -a "$(reloads)" = 2
check "sync: run/haproxy.cfg.sha256 = último sincronizado" test "$(cat "$SY/run/haproxy.cfg.sha256")" = "$(sha "$SY/haproxy/haproxy.cfg")"
echo "    acl manual hdr(host) -i outro.exemplo" >>"$SY/haproxy/haproxy.cfg"
manual="$(cat "$SY/haproxy/haproxy.cfg")"
cp "$HA" "$SY/run/haproxy.cfg.next"
check "sync: edição à mão no servidor é recusada" eval '! apply'
check "sync: recusa não toca o arquivo nem recarrega" test "$(cat "$SY/haproxy/haproxy.cfg")" = "$manual" -a "$(reloads)" = 2
check "sync: recusa explica e mostra a diferença" grep -q 'editado à mão' "$TMP/apply.out"
check "sync: recusa diz como aceitar (WWZ_PROD_HAPROXY_ACCEPT=<sha256>)" grep -q "WWZ_PROD_HAPROXY_ACCEPT=$(sha "$SY/haproxy/haproxy.cfg")" "$TMP/apply.out"
check "sync: ACCEPT com outro sha256 continua recusando" eval '! apply 0000000000000000000000000000000000000000000000000000000000000000'
check "sync: ACCEPT com o sha256 do arquivo do servidor sobrescreve no mesmo inode" apply "$(sha "$SY/haproxy/haproxy.cfg")"
check "sync: depois do ACCEPT, conteúdo do repositório e 3 reloads" \
	test "$(sha "$SY/haproxy/haproxy.cfg")" = "$(sha "$HA")" -a "$(inode "$SY/haproxy/haproxy.cfg")" = "$ino" -a "$(reloads)" = 3
{ cat "$HA"; echo "listen quebrado"; } >"$SY/run/haproxy.cfg.next"
check "sync: haproxy -c recusando não grava" eval '! apply none 1'
check "sync: haproxy -c recusando: arquivo e reloads intactos" test "$(sha "$SY/haproxy/haproxy.cfg")" = "$(sha "$HA")" -a "$(reloads)" = 3
# restart que não terminou: a próxima rodada sem mudança recarrega de novo
: >"$SY/run/haproxy.cfg.reload-pending"
cp "$HA" "$SY/run/haproxy.cfg.next"
apply
check "sync: restart pendente é refeito mesmo sem mudança" test "$(reloads)" = 4 -a ! -e "$SY/run/haproxy.cfg.reload-pending"
check "sync: o tar não leva o haproxy.cfg (o _haproxy_apply grava no lugar)" \
	test -z "$(WWZ_PROD_SOURCE_ONLY=1 bash -c 'source "$1"; sync_tar' _ "$DEPLOY" | tar -tf - | grep -E 'haproxy\.cfg$' || true)" -a \
	-n "$(WWZ_PROD_SOURCE_ONLY=1 bash -c 'source "$1"; sync_tar' _ "$DEPLOY" | tar -tf - | grep -E 'docker-compose\.yml$' || true)"

# ---- guarda dos comandos remotos
guard() { WWZ_PROD_SOURCE_ONLY=1 bash -c 'source "$1"; guard_cmd "$2"' _ "$DEPLOY" "$1" >/dev/null 2>&1; }
check "guarda aceita compose -p wwz em /opt/white-wizard" guard "cd /opt/white-wizard && docker compose -p wwz --env-file /opt/white-wizard/.env up -d --wait"
check "guarda aceita docker load" guard "gunzip | docker load"
check "guarda aceita o envio do haproxy.cfg para run/" guard "cat > /opt/white-wizard/run/haproxy.cfg.next"
check "guarda aceita o _haproxy_apply com ACCEPT" guard "WWZ_PROD_ROOT=/opt/white-wizard WWZ_PROD_SKIP_DRIVERS=none WWZ_PROD_HAPROXY_ACCEPT=$(printf 'a%.0s' {1..64}) bash -s -- _haproxy_apply"
check "guarda aceita restart do wwz-ports no projeto wwz" guard "cd /opt/white-wizard && docker compose -p wwz --env-file /opt/white-wizard/.env -f /opt/white-wizard/docker-compose.yml restart wwz-ports"
# ssh trocado por um stub: nada sai da máquina mesmo se a validação falhar.
self_accept() { WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_HAPROXY_ACCEPT="$1" bash -c 'source "$1"; ssh() { echo "$@"; }; remote_self _haproxy_apply' _ "$DEPLOY" </dev/null 2>/dev/null; }
check "remote_self recusa ACCEPT que não é sha256" test -z "$(self_accept 'x; rm -rf /')"
check "remote_self repassa ACCEPT sha256" grep -q "WWZ_PROD_HAPROXY_ACCEPT=$(printf 'b%.0s' {1..64}) bash -s -- _haproxy_apply" <(self_accept "$(printf 'b%.0s' {1..64})")
check "remote_self leva os WWZ_HOST_* e o IP público, e a guarda aceita" \
	test "$(self_accept none | grep -c 'WWZ_HOST_INFERENCIA=wwz.example.com WWZ_HOST_ADMIN=admin-wwz.example.com WWZ_HOST_PANEL=panel-wwz.example.com WWZ_HOST_GRAFANA=grafana-wwz.example.com WWZ_PROD_PUBLIC_IP=203.0.113.10 WWZ_PROD_ROOT=/opt/white-wizard ')" = 1
check "ssh vai ao destino e à porta de autor.env.example" \
	grep -q -- '-p 22 .*root@prod.example.com' <(self_accept none)
check "compose_remote leva os WWZ_HOST_* no ambiente do comando" \
	grep -q 'cd /opt/white-wizard && WWZ_HOST_INFERENCIA=wwz.example.com .* docker compose -p wwz ' \
	<(WWZ_PROD_SOURCE_ONLY=1 bash -c 'source "$1"; ssh() { echo "$@"; }; compose_remote ps' _ "$DEPLOY" </dev/null 2>/dev/null)
noenv="$(WWZ_PROD_SOURCE_ONLY=1 WWZ_AUTHOR_ENV=/nonexistent bash -c 'source "$1"; ssh() { echo SSH-CHAMADO; }; remote uptime' _ "$DEPLOY" 2>&1 </dev/null)"
check "sem dados do autor, nada vai ao ssh e o erro cita autor/prod.env e o modelo" \
	test -z "$(grep SSH-CHAMADO <<<"$noenv" || true)" -a -n "$(grep -F 'autor/prod.env' <<<"$noenv" | grep -F 'deploy/prod/autor.env.example')"
check "ambiente vence autor/prod.env" \
	test "$(WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_SSH=root@outro.example.net bash -c 'source "$1"; echo "$WWZ_PROD_SSH:$WWZ_PROD_SSH_PORT"' _ "$DEPLOY")" = root@outro.example.net:22
check "show-env lê os dados do autor sem conectar" \
	test "$("$DEPLOY" show-env 2>/dev/null | grep -cE '^(WWZ_PROD_SSH=root@prod\.example\.com|WWZ_HOST_ADMIN=admin-wwz\.example\.com|haproxy do sync: .*/deploy/prod/haproxy/haproxy\.cfg )')" = 3
# sync: o haproxy.cfg enviado é o de WWZ_PROD_HAPROXY (relativo à raiz) ou o genérico.
sync_sent() { # WWZ_PROD_HAPROXY -> arquivo recebido pelo "servidor"
	rm -f "$TMP/sent.cfg"
	WWZ_PROD_SOURCE_ONLY=1 WWZ_PROD_HAPROXY="$1" SENT="$TMP/sent.cfg" bash -c '
		source "$1"
		ssh() { if [[ $* == *"haproxy.cfg.next"* ]]; then cat >"$SENT"; else cat >/dev/null; fi; }
		cmd_sync' _ "$DEPLOY" </dev/null >/dev/null 2>&1
}
sync_sent ""
check "sync sem WWZ_PROD_HAPROXY envia o genérico" test "$(sha "$TMP/sent.cfg" 2>/dev/null)" = "$(sha "$HA")"
mkdir -p "$TMP/autor-ha"
{ cat "$HA"; echo "# cfg próprio do servidor"; } >"$TMP/autor-ha/haproxy.cfg"
sync_sent "$TMP/autor-ha/haproxy.cfg"
check "sync com WWZ_PROD_HAPROXY envia o cfg do autor" test "$(sha "$TMP/sent.cfg" 2>/dev/null)" = "$(sha "$TMP/autor-ha/haproxy.cfg")"
sync_sent "autor/nao-existe.cfg"
check "sync com WWZ_PROD_HAPROXY ausente falha sem enviar nada" test ! -e "$TMP/sent.cfg"
refuse() { ! guard "$1"; }
check "guarda recusa traefik" refuse "docker restart traefik-app"
check "guarda recusa haproxy.cfg fora de /opt/white-wizard" refuse "cat > /etc/haproxy/haproxy.cfg"
check "guarda recusa /opt/opencode" refuse "ls /opt/opencode"
check "guarda recusa rm fora de /opt/white-wizard" refuse "rm -rf /var/lib/docker"
check "guarda recusa docker rm / stop" refuse "docker rm -f wwz-valkey"
check "guarda recusa docker system prune" refuse "docker system prune -af"
check "guarda recusa compose sem -p wwz" refuse "docker compose -f /opt/white-wizard/docker-compose.yml down"
check "guarda recusa apt/systemctl" refuse "systemctl restart docker"

# ---- nenhum segredo versionado
scan=("$HERE" "$REPO/scripts/prod-deploy.sh" "$REPO/scripts/prod-smoke.sh")
check "sem .env versionado em deploy/prod" test -z "$(find "$HERE" -maxdepth 1 \( -name '.env' -o -name 'images.env' -o -name 'CREDENTIALS.md' \) | head -n1)"
check "sem chave wwz-key- real (>= 24 chars) em deploy/prod e scripts" test -z "$(grep -rEh 'wwz-key-[A-Za-z0-9_-]{24,}' "${scan[@]}" --exclude=test.sh --exclude=.env.example | head -n1)"
if [[ -n $AUTHOR_VALS ]]; then
	leak=""
	while IFS= read -r v; do
		[[ -n $v ]] || continue
		grep -rqF -- "$v" "$HERE" "$REPO/scripts" "$REPO/.gitignore" 2>/dev/null && leak=1
	done <<<"$AUTHOR_VALS"
	check "nenhum host, IP ou destino ssh de autor/prod.env em deploy/prod e scripts" test -z "$leak"
fi
check "sem token longo atribuído em deploy/prod e scripts" test -z "$(grep -rEhi '(token|password|senha|secret)[A-Za-z_]*[:=] *[A-Za-z0-9_-]{32,}' "${scan[@]}" --exclude=test.sh --exclude=.env.example | head -n1)"

echo
if ((FAILS)); then echo "deploy/prod/test.sh: $FAILS falha(s)"; exit 1; fi
echo "deploy/prod/test.sh: ok"

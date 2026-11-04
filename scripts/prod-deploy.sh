#!/usr/bin/env bash
# Deploy de produção do White Wizard (ADR 0017, spec 019). Roda no Mac; os
# comandos remotos passam por uma guarda que só aceita /opt/white-wizard e
# recursos Docker `wwz*`.
#
#   scripts/prod-deploy.sh build            imagens amd64 :prod-<commit> (wwz, wwz-panel, wwz-load)
#   scripts/prod-deploy.sh push-images      docker save | gzip | ssh docker load + images.env (sem registry)
#   scripts/prod-deploy.sh push-bases       idem para valkey, prometheus e grafana (se o servidor não puder puxar)
#   scripts/prod-deploy.sh sync             deploy/prod/ -> /opt/white-wizard (nunca .env, images.env, run/);
#                                           haproxy.cfg no lugar (mesmo inode) + restart do wwz-ports se mudou
#   scripts/prod-deploy.sh credentials      gera segredos NO servidor (idempotente, não imprime valores)
#   scripts/prod-deploy.sh render           renderiza config/*.yaml -> run/config/ no servidor
#   scripts/prod-deploy.sh up               render + docker compose -p wwz up -d --wait
#   scripts/prod-deploy.sh status           estado dos containers wwz*
#   scripts/prod-deploy.sh down [--purge]   derruba o projeto wwz; --purge também apaga os volumes wwz-*
#   scripts/prod-deploy.sh deploy           build, push-images, sync, credentials, up
#   scripts/prod-deploy.sh show-env         mostra os dados do autor lidos e o haproxy.cfg do sync (não conecta)
#
# Dados do servidor (ADR 0076): vêm de autor/prod.env (fora do git; WWZ_AUTHOR_ENV
# troca o caminho; modelo em deploy/prod/autor.env.example), sem padrão no código;
# variável já no ambiente vence o arquivo. WWZ_PROD_SSH e WWZ_PROD_SSH_PORT (ssh),
# WWZ_HOST_INFERENCIA/ADMIN/PANEL/GRAFANA (gravados no .env do servidor, de onde o
# compose os lê), WWZ_PROD_PUBLIC_IP (CREDENTIALS.md) e WWZ_PROD_HAPROXY (haproxy.cfg
# próprio do servidor, relativo à raiz do repositório; sem ele, o genérico
# deploy/prod/haproxy/haproxy.cfg). Outras variáveis: PROD_MIN_FREE_GB (4, disco
# local), PROD_REMOTE_MIN_FREE_GB (6, no servidor), WWZ_PROD_HAPROXY_ACCEPT (sha256
# do haproxy.cfg editado à mão no servidor que o operador conferiu e já levou ao
# cfg do sync; só então o sync o sobrescreve).
# Não acessa Traefik, /opt/opencode nem outros containers; não usa credenciais
# do Docker Hub (as bases são puxadas anonimamente ou enviadas por push-bases).
# Os comandos internos _credentials e _render rodam no servidor (via `bash -s`)
# ou localmente num diretório de teste (WWZ_PROD_ROOT).
set -euo pipefail

REMOTE_ROOT=/opt/white-wizard
ROOT="${WWZ_PROD_ROOT:-$REMOTE_ROOT}"
PROJECT=wwz
REPO="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." 2>/dev/null && pwd || true)"
HOST_VARS="WWZ_HOST_INFERENCIA WWZ_HOST_ADMIN WWZ_HOST_PANEL WWZ_HOST_GRAFANA"

die() { echo "prod-deploy: $*" >&2; exit 1; }
log() { echo "prod-deploy: $*" >&2; }

# ---------------------------------------------------------------- dados do autor
# author_env ARQUIVO: lê as variáveis WWZ_* de um arquivo em sintaxe de shell
# (autor/prod.env, ADR 0076) num bash limpo e assume só as que o ambiente ainda
# não tem (vazia conta como ausente). Arquivo ausente: nada muda.
author_env() {
	local f=$1 k v
	[[ -n $f && -f $f ]] || return 0
	env -i "${BASH:-bash}" -n "$f" || die "$f: erro de sintaxe"
	while IFS= read -r -d '' k && IFS= read -r -d '' v; do
		[[ -n ${!k:-} ]] || printf -v "$k" '%s' "$v"
	done < <(env -i "${BASH:-bash}" -c 'source "$1" >/dev/null || exit 1
		for k in $(compgen -v WWZ_); do printf "%s\0%s\0" "$k" "${!k}"; done' _ "$f")
}

# need VAR...: falha citando autor/prod.env quando falta valor do autor.
need() {
	local k miss=""
	for k in "$@"; do [[ -n ${!k:-} ]] || miss="$miss $k"; done
	[[ -z $miss ]] || die "sem$miss: defina no ambiente ou em autor/prod.env (WWZ_AUTHOR_ENV troca o caminho; modelo em deploy/prod/autor.env.example)"
}

# need_target: valores que vão ao ssh e à linha de comando remota, validados.
need_target() {
	local k
	need WWZ_PROD_SSH WWZ_PROD_SSH_PORT WWZ_PROD_PUBLIC_IP $HOST_VARS
	[[ $WWZ_PROD_SSH =~ ^([A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+$ ]] || die "WWZ_PROD_SSH inválido: $WWZ_PROD_SSH"
	[[ $WWZ_PROD_SSH_PORT =~ ^[0-9]+$ ]] || die "WWZ_PROD_SSH_PORT inválida: $WWZ_PROD_SSH_PORT"
	[[ $WWZ_PROD_PUBLIC_IP =~ ^[0-9A-Fa-f.:]+$ ]] || die "WWZ_PROD_PUBLIC_IP inválido: $WWZ_PROD_PUBLIC_IP"
	for k in $HOST_VARS; do
		[[ ${!k} =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "$k inválido: ${!k}"
	done
}

# Rodando por `bash -s` no servidor (remote_self) não há arquivo: os valores
# chegam pela linha de comando.
AUTHOR_ENV=""
if [[ -n ${BASH_SOURCE[0]:-} ]]; then
	AUTHOR_ENV="${WWZ_AUTHOR_ENV:-$REPO/autor/prod.env}"
	author_env "$AUTHOR_ENV"
fi

# rssh ARG...: ssh ao servidor do autor (BatchMode, timeout de conexão).
rssh() {
	need_target
	ssh -p "$WWZ_PROD_SSH_PORT" -o BatchMode=yes -o ConnectTimeout=15 "$WWZ_PROD_SSH" "$@"
}

# haproxy_src: o cfg que o sync envia. WWZ_PROD_HAPROXY (relativo à raiz do
# repositório, ou à pasta que contém autor/) quando definido; sem ele, o genérico.
# Definido e ausente é erro: o genérico sobrescreveria o cfg próprio do servidor.
haproxy_src() {
	local p=${WWZ_PROD_HAPROXY:-} base
	if [[ -z $p ]]; then
		echo "$REPO/deploy/prod/haproxy/haproxy.cfg"
		return 0
	fi
	if [[ $p != /* ]]; then
		base=$REPO
		if [[ ! -f $base/$p && -n $AUTHOR_ENV ]]; then
			base="$(cd "$(dirname "$AUTHOR_ENV")/.." 2>/dev/null && pwd || echo "$REPO")"
		fi
		p=$base/$p
	fi
	[[ -f $p ]] || die "WWZ_PROD_HAPROXY=$WWZ_PROD_HAPROXY não existe ($p); sem a variável o sync usa deploy/prod/haproxy/haproxy.cfg"
	echo "$p"
}

# ---------------------------------------------------------------- guarda
# guard_cmd recusa comando remoto que toque algo fora de /opt/white-wizard e dos
# recursos wwz*. Defesa em profundidade: os comandos já são montados aqui.
guard_cmd() {
	local cmd=$1 p sub
	local nb='(^|[^[:alnum:]_.-])'
	local nb2='($|[^[:alnum:]_.-])'
	if [[ $cmd =~ ${nb}(traefik|iptables|ufw|firewall-cmd|apt|apt-get|dpkg|yum|dnf|systemctl|reboot|shutdown)${nb2} ]] ||
		[[ $cmd == *"/opt/opencode"* || $cmd == *"--privileged"* || $cmd == *"daemon.json"* ]]; then
		die "guarda: comando remoto recusado (recurso proibido): $cmd"
	fi
	# Todo caminho absoluto precisa ficar sob /opt/white-wizard (ou /dev/null).
	while IFS= read -r p; do
		[[ -n $p ]] || continue
		case $p in
		"$REMOTE_ROOT" | "$REMOTE_ROOT"/* | /dev/null) ;;
		*) die "guarda: caminho fora de $REMOTE_ROOT: $p" ;;
		esac
	done < <(grep -oE "(^|[[:space:]=:\"'])/[A-Za-z0-9._/-]*" <<<"$cmd" | sed -E "s/^[^/]*//")
	# Só estes subcomandos do docker; compose sempre no projeto wwz.
	while IFS= read -r sub; do
		[[ -n $sub ]] || continue
		case $sub in
		load | ps | images | inspect | compose | "network inspect" | "network ls") ;;
		*) die "guarda: subcomando docker recusado: docker $sub" ;;
		esac
	done < <(grep -oE 'docker +[a-z-]+( +(inspect|ls))?' <<<"$cmd" | sed -E 's/^docker +//')
	if [[ $cmd == *"docker compose"* && $cmd != *"docker compose -p $PROJECT "* ]]; then
		die "guarda: docker compose só com -p $PROJECT"
	fi
	if [[ $cmd =~ docker\ (ps|images|inspect)\  && $cmd != *"wwz"* && $cmd != *"network_public"* && $cmd != *"--format"* ]]; then
		die "guarda: consulta docker sem filtro wwz*"
	fi
}

remote() {
	[[ $ROOT == "$REMOTE_ROOT" ]] || die "WWZ_PROD_ROOT=$ROOT: comandos remotos só em $REMOTE_ROOT"
	guard_cmd "$1"
	rssh "$1"
}

# remote_self roda uma função deste script no servidor, lendo-o pela entrada padrão.
remote_self() {
	local fn=$1 skip=${2:-} accept=${WWZ_PROD_HAPROXY_ACCEPT:-none}
	[[ $accept == none || $accept =~ ^[0-9a-f]{64}$ ]] || die "WWZ_PROD_HAPROXY_ACCEPT precisa ser um sha256 (64 hex)"
	local k hosts=""
	need_target
	for k in $HOST_VARS WWZ_PROD_PUBLIC_IP; do hosts+="$k=${!k} "; done
	local cmd="${hosts}WWZ_PROD_ROOT=$REMOTE_ROOT WWZ_PROD_SKIP_DRIVERS=${skip:-none} WWZ_PROD_HAPROXY_ACCEPT=$accept bash -s -- $fn"
	guard_cmd "$cmd"
	rssh "$cmd" <"${BASH_SOURCE[0]}"
}

# compose_remote: os WWZ_HOST_* vão no ambiente do comando (o compose não tem
# padrão e o ambiente vence o --env-file), então status e down funcionam mesmo
# com um .env anterior a eles; `credentials` e `up` os gravam no .env (_hosts).
compose_remote() {
	local k hosts=""
	need_target
	for k in $HOST_VARS; do hosts+="$k=${!k} "; done
	remote "cd $REMOTE_ROOT && ${hosts}docker compose -p $PROJECT --env-file $REMOTE_ROOT/.env --env-file $REMOTE_ROOT/images.env -f $REMOTE_ROOT/docker-compose.yml $*"
}

# ---------------------------------------------------------------- build
commit_tag() {
	local c
	c="$(cd "$REPO" && git rev-parse --short=7 HEAD 2>/dev/null)" || die "sem commit (git rev-parse)"
	echo "prod-$c"
}

image_names() { # alvo|imagem
	cat <<EOF
wwz|white-wizard
wwz-panel|white-wizard-panel
wwz-load|white-wizard-load
EOF
}

local_free_gb() { df -Pk "${1:-$REPO}" | awk 'NR==2 {printf "%d", $4/1048576}'; }

cmd_build() {
	command -v docker >/dev/null || die "docker ausente (no Mac: PATH precisa de /Applications/Docker.app/Contents/Resources/bin)"
	local free min=${PROD_MIN_FREE_GB:-4} tag target img version commit
	free=$(local_free_gb)
	((free >= min)) || die "disco local com ${free} GiB livres (< ${min}); libere espaço"
	tag=$(commit_tag)
	version="${WWZ_VERSION:-$(cd "$REPO" && git describe --tags --always --dirty 2>/dev/null || echo dev)}"
	commit="$(cd "$REPO" && git rev-parse HEAD)"
	mkdir -p "$REPO/tmp/prod"
	: >"$REPO/tmp/prod/images.env"
	while IFS='|' read -r target img; do
		log "build $img:$tag (linux/amd64, alvo $target)"
		(cd "$REPO" && docker buildx build --platform linux/amd64 --target "$target" \
			--build-arg "VERSION=$version" --build-arg "COMMIT=$commit" \
			-t "$img:$tag" --load .)
	done < <(image_names)
	cat >"$REPO/tmp/prod/images.env" <<EOF
WWZ_IMAGE=white-wizard:$tag
WWZ_PANEL_IMAGE=white-wizard-panel:$tag
WWZ_LOAD_IMAGE=white-wizard-load:$tag
EOF
	log "imagens prontas; tags em tmp/prod/images.env"
}

cmd_push_images() {
	[[ -f $REPO/tmp/prod/images.env ]] || die "rode build antes (tmp/prod/images.env ausente)"
	remote "mkdir -p $REMOTE_ROOT"
	local free min=${PROD_REMOTE_MIN_FREE_GB:-6} img line
	free=$(remote "df -Pk $REMOTE_ROOT" | awk 'NR==2 {printf "%d", $4/1048576}')
	((free >= min)) || die "servidor com ${free} GiB livres em $REMOTE_ROOT (< ${min})"
	while IFS='=' read -r _ img; do
		[[ -n $img ]] || continue
		log "enviando $img"
		docker save "$img" | gzip | rssh "gunzip | docker load"
	done <"$REPO/tmp/prod/images.env"
	line="cat > $REMOTE_ROOT/images.env && chmod 600 $REMOTE_ROOT/images.env"
	guard_cmd "$line"
	rssh "$line" <"$REPO/tmp/prod/images.env"
	log "images.env atualizado no servidor"
}

cmd_push_bases() {
	local img
	remote "mkdir -p $REMOTE_ROOT"
	for img in "valkey/valkey:8.1-alpine" "prom/prometheus:v3.7.0" "grafana/grafana:12.2.0"; do
		log "enviando $img (amd64)"
		docker pull --platform linux/amd64 "$img" >/dev/null
		docker save "$img" | gzip | rssh "gunzip | docker load"
	done
}

# ---------------------------------------------------------------- sync
# sync_tar empacota deploy/prod/ sem segredos, estado nem o haproxy.cfg. tar -h
# segue os symlinks (grafana/, prometheus/alerts.yml).
sync_tar() {
	COPYFILE_DISABLE=1 tar -C "$REPO/deploy/prod" -chf - \
		--exclude='./.env' --exclude='./images.env' --exclude='./run' --exclude='./CREDENTIALS.md' \
		--exclude='./grafana/docker-compose.yml' --exclude='./test.sh' --exclude='./haproxy/haproxy.cfg' \
		.
}

# O haproxy.cfg não vai no tar: o wwz-ports monta esse ARQUIVO (bind de arquivo
# único), e o tar troca o inode, deixando o container com o conteúdo antigo. Vai
# para run/haproxy.cfg.next e _haproxy_apply (no servidor) confere com haproxy -c,
# recusa edição manual, grava no lugar (mesmo inode) e reinicia o wwz-ports.
cmd_sync() {
	[[ -d $REPO/deploy/prod ]] || die "deploy/prod ausente"
	local src
	src=$(haproxy_src) || exit 1
	need_target
	log "haproxy: enviando $src"
	remote "mkdir -p $REMOTE_ROOT/run"
	local line="cat > $REMOTE_ROOT/run/haproxy.cfg.next"
	guard_cmd "$line"
	rssh "$line" <"$src"
	# Recusa (edição manual, haproxy -c) para antes do tar: nada mais muda.
	remote_self _haproxy_apply
	line="tar -C $REMOTE_ROOT -xf -"
	guard_cmd "$line"
	sync_tar | rssh "$line"
	log "deploy/prod sincronizado em $REMOTE_ROOT"
}

# ------------------------------------------------- funções que rodam no servidor
rand_b64url() { # n bytes
	head -c "$1" /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n'
}

# env_get NAME FILE
env_get() { grep -E "^$1=" "$2" 2>/dev/null | head -n1 | cut -d= -f2- || true; }

# _hosts (no servidor): grava no .env os WWZ_HOST_* recebidos do Mac (o compose
# os lê de lá), cada um na linha que já tinha. Só reescreve o .env quando um
# valor mudou; HOSTS_CHANGED conta.
HOSTS_CHANGED=0
_hosts() {
	umask 077
	need $HOST_VARS
	mkdir -p "$ROOT"
	local env="$ROOT/.env" name v rest
	[[ -f $env ]] || : >"$env"
	chmod 600 "$env"
	HOSTS_CHANGED=0
	for name in $HOST_VARS; do
		v=${!name}
		[[ $v =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || die "$name inválido: $v"
		[[ "$(env_get "$name" "$env")" == "$v" ]] && continue
		# Troca a linha no lugar (ou acrescenta no fim); o .env continua o mesmo arquivo.
		rest=$(awk -v n="$name" -v v="$v" 'index($0, n "=") == 1 && !d { print n "=" v; d = 1; next }
			{ print } END { if (!d) print n "=" v }' "$env")
		printf '%s\n' "$rest" >"$env"
		HOSTS_CHANGED=$((HOSTS_CHANGED + 1))
	done
	((HOSTS_CHANGED == 0)) || log "hosts: $HOSTS_CHANGED WWZ_HOST_* gravado(s) no .env"
}

_credentials() {
	umask 077
	need WWZ_PROD_PUBLIC_IP $HOST_VARS
	_hosts
	local env="$ROOT/.env" cred="$ROOT/CREDENTIALS.md" added=$HOSTS_CHANGED spec name kind
	for spec in \
		WWZ_ADMIN_TOKEN:token WWZ_KEY_1:key WWZ_KEY_2:key WWZ_KEY_3:key WWZ_OAUTH_KEY:key \
		WWZ_PANEL_ADMIN_PASSWORD:token WWZ_PANEL_API_TOKEN:token WWZ_PANEL_SESSION_KEY:token \
		GRAFANA_ADMIN_PASSWORD:token WWZ_PANEL_ADMIN_USER:admin GRAFANA_ADMIN_USER:admin; do
		name=${spec%%:*}
		kind=${spec##*:}
		[[ -n "$(env_get "$name" "$env")" ]] && continue
		# .env antigo sem \n no fim: a linha nova não pode colar na última.
		[[ ! -s $env || -z "$(tail -c1 "$env")" ]] || echo >>"$env"
		case $kind in
		key) printf '%s=wwz-key-%s\n' "$name" "$(rand_b64url 24)" >>"$env" ;;
		token) printf '%s=%s\n' "$name" "$(rand_b64url 24)" >>"$env" ;;
		admin) printf '%s=admin\n' "$name" >>"$env" ;;
		esac
		added=$((added + 1))
	done
	if ((added > 0)) || [[ ! -f $cred ]]; then
		{
			echo "# Credenciais de produção do White Wizard"
			echo
			echo "Geradas no servidor por \`scripts/prod-deploy.sh credentials\`. Arquivo chmod 600; nunca vai ao git."
			echo
			echo '```'
			local n
			for n in WWZ_ADMIN_TOKEN WWZ_KEY_1 WWZ_KEY_2 WWZ_KEY_3 WWZ_OAUTH_KEY WWZ_PANEL_ADMIN_USER WWZ_PANEL_ADMIN_PASSWORD \
				WWZ_PANEL_API_TOKEN GRAFANA_ADMIN_USER GRAFANA_ADMIN_PASSWORD; do
				printf '%s=%s\n' "$n" "$(env_get "$n" "$env")"
			done
			echo '```'
			echo
			echo "- Inferência: \`Authorization: Bearer \$WWZ_KEY_1\` (OpenAI), \`x-api-key\` (Anthropic), \`x-goog-api-key\` (Gemini), \`api-key\` (Azure)."
			echo "- \`WWZ_OAUTH_KEY\`: segredo da assinatura OAuth do Claude Code (\`oauth_key: claude-code-oauth\`, conta \`acct-claude-max\`), só na 8004 e no \`/anthropic\` do HTTPS, só nos modelos \`claude-*\`. É o que \`create_api_key\` devolve; não é chave de carga."
			echo "- Admin: \`Authorization: Bearer \$WWZ_ADMIN_TOKEN\` em https://$WWZ_HOST_ADMIN/admin/... e $WWZ_PROD_PUBLIC_IP:8007."
			echo "- Painel: https://$WWZ_HOST_PANEL (usuário e senha acima)."
			echo "- Grafana: https://$WWZ_HOST_GRAFANA (usuário e senha acima)."
		} >"$cred"
		log "credenciais: $added variável(is) nova(s); CREDENTIALS.md gravado"
	else
		log "credenciais: já existem; nada reescrito"
	fi
	chmod 600 "$env" "$cred"
}

# _render troca ${WWZ_KEY_n} e ${WWZ_OAUTH_KEY} pelos valores do .env em
# config/*.yaml -> run/config/. Cada placeholder só é exigido no .env pelas
# configs que o usam (WWZ_OAUTH_KEY: anthropic.yaml e todos.yaml).
_render() {
	umask 077
	local env="$ROOT/.env" f out content name v pat d n=0
	[[ -f $env ]] || die "$env ausente; rode credentials antes"
	mkdir -p "$ROOT/run/config"
	for f in "$ROOT"/config/*.yaml; do
		[[ -e $f ]] || die "nenhum $ROOT/config/*.yaml (rode sync)"
		content=$(cat "$f")
		if [[ $(basename "$f") == todos.yaml && ${WWZ_PROD_SKIP_DRIVERS:-none} != none ]]; then
			for d in ${WWZ_PROD_SKIP_DRIVERS//,/ }; do
				content=$(grep -vE "^[[:space:]]*- \{name: $d," <<<"$content")
			done
		fi
		for name in WWZ_KEY_1 WWZ_KEY_2 WWZ_KEY_3 WWZ_OAUTH_KEY; do
			pat='${'$name'}'
			[[ $content == *"$pat"* ]] || continue
			v=$(env_get "$name" "$env")
			[[ -n $v ]] || die "$name ausente no .env (rode credentials)"
			content=${content//"$pat"/"$v"}
		done
		[[ $content != *'${WWZ_'* ]] || die "$(basename "$f"): placeholder \${WWZ_...} sem valor"
		out="$ROOT/run/config/$(basename "$f")"
		rm -f "$out"
		printf '%s\n' "$content" >"$out"
		chmod 400 "$out"
		# A imagem roda como nonroot (65532); em teste local o chown é ignorado.
		chown 65532:65532 "$out" 2>/dev/null || true
		n=$((n + 1))
	done
	chmod 755 "$ROOT/run" "$ROOT/run/config"
	log "render: $n config(s) em $ROOT/run/config"
}

sha256_of() { # arquivo -> hex
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# compose_local roda o compose do projeto wwz no próprio servidor (só o
# _haproxy_apply usa). Falha se faltar .env ou images.env.
compose_local() {
	[[ -f $ROOT/.env && -f $ROOT/images.env ]] || return 1
	docker compose -p "$PROJECT" --env-file "$ROOT/.env" --env-file "$ROOT/images.env" -f "$ROOT/docker-compose.yml" "$@"
}

# haproxy_check ARQUIVO: haproxy -c num container avulso do serviço wwz-ports
# (mesma imagem, arquivo montado só leitura). Sem .env/images.env (primeiro
# deploy, nada no ar) não há o que conferir aqui; o `up` acusa erro de sintaxe.
haproxy_check() {
	if ! compose_local config -q >/dev/null 2>&1; then
		log "haproxy: sem .env/images.env, haproxy -c fica para o up"
		return 0
	fi
	compose_local run --rm --no-deps -T --name wwz-ports-check --entrypoint haproxy \
		-v "$1:/usr/local/etc/haproxy/check.cfg:ro" wwz-ports -c -q -f /usr/local/etc/haproxy/check.cfg
}

# haproxy_reload SHA: restart do wwz-ports (só se o container existe). O restart
# refaz o bind mount, então o container passa a ver o arquivo atual mesmo que o
# mount antigo apontasse para um inode substituído. Confere o sha256 lido de
# dentro do container e espera cada porta do cfg aceitar conexão.
haproxy_reload() {
	local want=$1 got p i
	if [[ -z "$(compose_local ps -q wwz-ports 2>/dev/null)" ]]; then
		log "haproxy: wwz-ports não está no ar; o up carrega o arquivo novo"
		return 0
	fi
	log "haproxy: restart do wwz-ports (portas 8000-8008 fora por ~1-2 s; conexões abertas caem)"
	compose_local restart -t 5 wwz-ports >/dev/null
	got=$(compose_local exec -T wwz-ports sha256sum /usr/local/etc/haproxy/haproxy.cfg | cut -d' ' -f1)
	[[ $got == "$want" ]] || die "haproxy: o wwz-ports vê sha256 $got, esperado $want (bind mount desatualizado?)"
	for p in $(grep -oE 'bind 0\.0\.0\.0:[0-9]+' "$ROOT/haproxy/haproxy.cfg" | cut -d: -f2); do
		for i in $(seq 1 20); do
			compose_local exec -T wwz-ports nc -z 127.0.0.1 "$p" && break
			((i < 20)) || die "haproxy: porta $p não aceita conexão depois do restart"
			sleep 0.5
		done
	done
	log "haproxy: wwz-ports no ar com o cfg $want"
}

# _haproxy_apply (no servidor): run/haproxy.cfg.next -> haproxy/haproxy.cfg.
# Grava no lugar (`cat >`, mesmo inode: o bind de arquivo único do wwz-ports
# continua valendo) e guarda o sha256 em run/haproxy.cfg.sha256. Recusa quando o
# arquivo do servidor difere do último sincronizado e do repositório (edição à
# mão), salvo WWZ_PROD_HAPROXY_ACCEPT=<sha256 do arquivo do servidor>.
_haproxy_apply() {
	local live="$ROOT/haproxy/haproxy.cfg" next="$ROOT/run/haproxy.cfg.next" state="$ROOT/run/haproxy.cfg.sha256"
	local pending="$ROOT/run/haproxy.cfg.reload-pending"
	local accept=${WWZ_PROD_HAPROXY_ACCEPT:-none} want cur last=""
	[[ -s $next ]] || die "haproxy: $next ausente ou vazio"
	want=$(sha256_of "$next")
	[[ -f $state ]] && last=$(head -n1 "$state")
	if [[ -f $live ]]; then
		cur=$(sha256_of "$live")
		if [[ $cur == "$want" ]]; then
			echo "$want" >"$state"
			rm -f "$next"
			if [[ -f $pending ]]; then # restart anterior não terminou
				haproxy_reload "$want"
				rm -f "$pending"
			else
				log "haproxy: sem mudança ($want)"
			fi
			return 0
		fi
		if [[ $cur != "$last" && $cur != "$accept" ]]; then
			{
				echo "prod-deploy: haproxy: $live foi editado à mão (sha256 $cur;"
				echo "  último sincronizado: ${last:-nenhum registro em $state}). Nada foi sobrescrito."
				echo "  Diferença servidor -> repositório:"
				diff -u "$live" "$next" | sed 's/^/    /' || true
				echo "  Leve a edição ao cfg que o sync envia (WWZ_PROD_HAPROXY de autor/prod.env ou,"
				echo "  sem ele, deploy/prod/haproxy/haproxy.cfg) e rode de novo com"
				echo "  WWZ_PROD_HAPROXY_ACCEPT=$cur scripts/prod-deploy.sh sync"
			} >&2
			exit 1
		fi
	fi
	haproxy_check "$next" || die "haproxy: haproxy -c recusou o cfg do repositório; nada foi gravado"
	mkdir -p "$(dirname "$live")"
	cat "$next" >"$live"
	chmod 644 "$live"
	echo "$want" >"$state"
	: >"$pending"
	rm -f "$next"
	log "haproxy: $live gravado no lugar ($want)"
	haproxy_reload "$want"
	rm -f "$pending"
}

# ---------------------------------------------------------------- comandos
cmd_credentials() { remote_self _credentials; }

# skip_drivers lista os drivers do todos.yaml que o código deste commit ainda não
# tem (internal/driver/<nome> ausente); WWZ_PROD_SKIP_DRIVERS="a,b" força a lista
# (vazio-explícito "none" não pula nada). Só o run/config no servidor é filtrado.
skip_drivers() {
	local d out=""
	if [[ -n ${WWZ_PROD_SKIP_DRIVERS:-} ]]; then
		[[ $WWZ_PROD_SKIP_DRIVERS == none ]] || out=$WWZ_PROD_SKIP_DRIVERS
	else
		for d in $(grep -oE 'name: [a-z]+' "$REPO/deploy/prod/config/todos.yaml" | sed 's/name: //'); do
			[[ -d $REPO/internal/driver/$d ]] || out="${out:+$out,}$d"
		done
	fi
	echo "$out"
}
cmd_render() {
	local skip fn=_render
	skip=$(skip_drivers)
	[[ -z $skip ]] || { log "render: drivers ausentes neste commit, pulados em todos.yaml: $skip"; }
	remote_self "$fn" "$skip"
}

cmd_up() {
	remote "docker network inspect network_public --format ok" >/dev/null || die "rede externa network_public não existe no servidor"
	remote "test -f $REMOTE_ROOT/images.env" || die "images.env ausente; rode push-images"
	remote_self _hosts
	cmd_render
	compose_remote up -d --wait
	cmd_status
}

cmd_status() {
	compose_remote ps
	remote "uptime"
}

cmd_down() {
	case "${1:-}" in
	"") compose_remote down ;;
	--purge)
		log "--purge: apaga também os volumes wwz-* (Prometheus, Grafana, painel)"
		compose_remote down -v
		;;
	*) die "down: opção desconhecida: $1" ;;
	esac
}

# show-env: só local, nada conecta. Valores do autor não são segredo (hosts, IP,
# destino ssh); os segredos ficam no .env do servidor.
cmd_show_env() {
	local k src rc=0
	if [[ -n $AUTHOR_ENV && -f $AUTHOR_ENV ]]; then
		echo "dados do autor: $AUTHOR_ENV"
	else
		echo "dados do autor: ${AUTHOR_ENV:-?} ausente (só o ambiente)"
	fi
	for k in WWZ_PROD_SSH WWZ_PROD_SSH_PORT WWZ_PROD_PUBLIC_IP WWZ_PROD_PRIVATE_IP $HOST_VARS WWZ_PROD_HAPROXY; do
		echo "$k=${!k:-}"
	done
	if src=$(haproxy_src); then
		echo "haproxy do sync: $src (sha256 $(sha256_of "$src"))"
	else
		rc=1
	fi
	(need_target) || rc=1
	return "$rc"
}

cmd_deploy() {
	cmd_build
	cmd_push_images
	cmd_sync
	cmd_credentials
	cmd_up
}

main() {
	local c=${1:-}
	shift || true
	case $c in
	build) cmd_build ;;
	push-images) cmd_push_images ;;
	push-bases) cmd_push_bases ;;
	sync) cmd_sync ;;
	credentials) cmd_credentials ;;
	render) cmd_render ;;
	up) cmd_up ;;
	status) cmd_status ;;
	down) cmd_down "$@" ;;
	deploy) cmd_deploy ;;
	show-env) cmd_show_env ;;
	_credentials) _credentials ;;
	_hosts) _hosts ;;
	_render) _render ;;
	_haproxy_apply) _haproxy_apply ;;
	*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
	esac
}

[[ -n ${WWZ_PROD_SOURCE_ONLY:-} ]] || main "$@"

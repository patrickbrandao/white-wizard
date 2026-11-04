#!/usr/bin/env bash
# valkey-db-exec.sh: uso `go test -exec scripts/valkey-db-exec.sh` (task 300).
#
# Os testes que falam com o Valkey fazem FLUSHDB/SCRIPT FLUSH e assinam o canal
# pub/sub <prefixo>:cfg:changed; nada disso é isolado por banco (pub/sub e SCRIPT FLUSH
# valem para o servidor inteiro), então pacotes em paralelo e verify de worktrees
# concorrentes se atrapalhavam mesmo em bancos diferentes. Por isso cada binário de teste
# roda com um Valkey só dele:
#   1. valkey-server local (porta aleatória), se existir no PATH;
#   2. senão um container efêmero (WWZ_VERIFY_VALKEY_IMAGE, padrão valkey/valkey:8.1-alpine);
#   3. senão (sem binário nem docker) o Valkey compartilhado de WWZ_TEST_VALKEY_ADDR com um
#      banco reservado (1..14) em WWZ_VERIFY_DBDIR; isolamento parcial: pub/sub continua global.
# Sem WWZ_TEST_VALKEY=local, só executa o teste.
set -u

if [ "${WWZ_TEST_VALKEY:-}" != local ]; then
	exec "$@"
fi

cleanup_cmds=()
cleanup() {
	local c
	for c in ${cleanup_cmds[@]+"${cleanup_cmds[@]}"}; do eval "$c"; done
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# private_binary: sobe valkey-server numa porta livre; exporta WWZ_TEST_VALKEY_ADDR.
private_binary() {
	command -v valkey-server >/dev/null 2>&1 || return 1
	local port pid i
	for _ in 1 2 3 4 5 6 7 8; do
		port=$((20000 + RANDOM % 30000))
		valkey-server --port "$port" --bind 127.0.0.1 --save "" --appendonly no \
			--protected-mode no >/dev/null 2>&1 &
		pid=$!
		for i in $(seq 1 50); do
			kill -0 "$pid" 2>/dev/null || break
			if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
				cleanup_cmds+=("kill $pid 2>/dev/null; wait $pid 2>/dev/null")
				export WWZ_TEST_VALKEY_ADDR="127.0.0.1:$port"
				return 0
			fi
			sleep 0.1
		done
		kill "$pid" 2>/dev/null
		wait "$pid" 2>/dev/null
	done
	return 1
}

# private_docker: container efêmero com porta publicada em 127.0.0.1.
private_docker() {
	command -v docker >/dev/null 2>&1 || return 1
	local name="wwz-test-valkey-$$" image="${WWZ_VERIFY_VALKEY_IMAGE:-valkey/valkey:8.1-alpine}" addr i
	docker run --rm -d --name "$name" -p 127.0.0.1::6379 --tmpfs /data "$image" \
		valkey-server --save "" --appendonly no >/dev/null 2>&1 || return 1
	cleanup_cmds+=("docker rm -f $name >/dev/null 2>&1")
	for i in $(seq 1 100); do
		if [ "$(docker exec "$name" valkey-cli ping 2>/dev/null)" = PONG ]; then
			addr="$(docker port "$name" 6379/tcp 2>/dev/null | grep '^127\.0\.0\.1:' | head -n1)"
			[ -n "$addr" ] || return 1
			export WWZ_TEST_VALKEY_ADDR="$addr"
			return 0
		fi
		sleep 0.1
	done
	return 1
}

# shared_db: reserva um banco 1..14 do Valkey compartilhado (dono morto é roubado).
shared_db() {
	[ -n "${WWZ_VERIFY_DBDIR:-}" ] || return 1
	mkdir -p "$WWZ_VERIFY_DBDIR"
	local n d owner claimed="" waited=0
	while [ -z "$claimed" ]; do
		for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14; do
			d="$WWZ_VERIFY_DBDIR/$n"
			owner="$(cat "$d/pid" 2>/dev/null || true)"
			if [ -n "$owner" ] && ! kill -0 "$owner" 2>/dev/null; then
				rm -rf "$d" # dono morto
			fi
			if mkdir "$d" 2>/dev/null; then
				printf '%s\n' "$$" >"$d/pid"
				claimed="$n"
				cleanup_cmds+=("rm -rf '$d'")
				break
			fi
		done
		[ -n "$claimed" ] && break
		if [ "$waited" -ge "${WWZ_VERIFY_DB_TIMEOUT:-600}" ]; then
			echo "valkey-db-exec: nenhum banco livre em $WWZ_VERIFY_DBDIR" >&2
			exit 1
		fi
		sleep 1
		waited=$((waited + 1))
	done
	export WWZ_TEST_VALKEY_DB="$claimed"
}

if [ -z "${WWZ_VERIFY_SHARED:-}" ]; then
	private_binary || private_docker || shared_db
else
	shared_db
fi

"$@"

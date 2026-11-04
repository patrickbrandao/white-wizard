#!/usr/bin/env bash
# Smoke do exemplo cluster-3-instancias-valkey (ADR 0016): a conta acme tem rpm 60
# no CLUSTER. Uma rajada de 120 requisições espalhadas em rodízio pelas 3
# instâncias (e pelas 2 chaves da conta) soma 60 + ~1 por segundo de rajada respostas 200 (as fichas do
# bucket mais as repostas durante a rajada), o resto dá 429 e há 429 em TODAS as
# instâncias. Uma instância que ainda não viu a conta esgotada também nega, e a
# conta globex segue em 200 em qualquer instância.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init cluster-3-instancias-valkey 14
export EX_PORT2="$((EX_PORT + 1000))" EX_PORT3="$((EX_PORT + 2000))"
ex_up

PORTS="$EX_PORT $EX_PORT2 $EX_PORT3"
BODY='{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"oi"}]}'

# call <porta> <chave>: preenche EX_HTTP_CODE e EX_HTTP_BODY.
call() { ex_post_chat "$1" "$2" "$BODY" || true; }

# --- 1. Rajada em rodízio pelas 3 instâncias, alternando as chaves da conta acme.
echo "== rajada de 120 requisições da conta acme (rpm 60) nas 3 instâncias"
ok200=0 n429=0 start=$SECONDS
i=0
for _ in $(seq 1 120); do
	set -- $PORTS
	shift $((i % 3))
	port="$1"
	if [ $((i % 2)) -eq 0 ]; then key=wwz-key-acme-a; else key=wwz-key-acme-b; fi
	call "$port" "$key"
	case "$EX_HTTP_CODE" in
	200)
		ok200=$((ok200 + 1))
		eval "ok_$port=\$(( \${ok_$port:-0} + 1 ))"
		;;
	429)
		n429=$((n429 + 1))
		eval "r_$port=\$(( \${r_$port:-0} + 1 ))"
		;;
	*) ex_fail "rajada: HTTP $EX_HTTP_CODE inesperado: $EX_HTTP_BODY" ;;
	esac
	i=$((i + 1))
done
echo "total: 200=$ok200 429=$n429"
for p in $PORTS; do
	eval "echo \"instância :$p -> 200=\${ok_$p:-0} 429=\${r_$p:-0}\""
done
elapsed=$((SECONDS - start + 1))
ex_assert_between "$ok200" 60 $((62 + elapsed)) "respostas 200 da conta acme no cluster (60 fichas + 1 por segundo de rajada, ${elapsed}s)"
ex_assert_between "$n429" 1 $((120 - 60)) "respostas 429 da conta acme no cluster"
for p in $PORTS; do
	eval "v=\${r_$p:-0}"
	ex_assert_between "$v" 1 60 "respostas 429 na instância :$p"
done

# --- 2. Conta esgotada: o 429 vem de qualquer instância, com o envelope OpenAI.
for p in $PORTS; do
	call "$p" wwz-key-acme-a
	[ "$EX_HTTP_CODE" = 429 ] || ex_fail "instância :$p com a conta esgotada: HTTP $EX_HTTP_CODE"
	jq -e '.error.code == "rate_limit_exceeded"' >/dev/null <<<"$EX_HTTP_BODY" ||
		ex_fail "429 de :$p fora do formato OpenAI: $EX_HTTP_BODY"
done
echo "ok  as 3 instâncias negam a conta esgotada (429 rate_limit_exceeded)"

# --- 3. Outra conta não é afetada em nenhuma instância.
for p in $PORTS; do
	call "$p" wwz-key-globex
	[ "$EX_HTTP_CODE" = 200 ] || ex_fail "globex em :$p: HTTP $EX_HTTP_CODE"
done
echo "ok  conta globex segue em 200 nas 3 instâncias"

echo "OK cluster-3-instancias-valkey"

#!/usr/bin/env bash
# Smoke do exemplo opencode-completo (ADR 0016; ADR 0072, item 18): as quatro
# listagens do wwz batem campo a campo com as fixtures dos catálogos públicos
# (79 modelos pay-as-you-go, 28 Go; fora created e o host do base_url); o modo
# public atende gratuito só com User-Agent opencode/, recusa pago com 401 e
# gratuito fora do OpenCode com 429; a chave Go sai billing_source go com cost
# 0; o budget por modelo nega com BudgetLimitError coverage models; a conta
# sem Go recebe 403 na base Go; /zen/go/v1/usage e o Console respondem.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init opencode-completo 16
export EX_ADMIN_PORT="$((EX_PORT + 1000))"
CATALOG="$EX_DIR/../../testdata/real/opencode/catalog"
[ -d "$CATALOG" ] || ex_fail "fixtures ausentes: $CATALOG"
ex_up

BASE="http://127.0.0.1:$EX_PORT"
SESSION=(-H 'x-opencode-session: ses-exemplo')
OC_UA='opencode/latest/2.2.5/cli' # User-Agent do cliente oficial (só no wwz)
TMP="$(mktemp -d)"
trap 'rc=$?; set +e; rm -rf "$TMP"; (exit $rc); ex_down' EXIT

# req <método> <caminho> <chave|-> [corpo] [user-agent]: preenche EX_HTTP_CODE
# e EX_HTTP_BODY.
req() {
	local args=(-sS --max-time 30 -X "$1" -w $'\n%{http_code}' "${SESSION[@]}") out
	[ "$3" = - ] || args+=(-H "Authorization: Bearer $3")
	[ -z "${4:-}" ] || args+=(-H 'Content-Type: application/json' -d "$4")
	[ -z "${5:-}" ] || args+=(-A "$5")
	out="$(curl "${args[@]}" "$BASE$2" 2>/dev/null)" || out=$'\n000'
	EX_HTTP_CODE="${out##*$'\n'}"
	EX_HTTP_BODY="${out%$'\n'*}"
}

# expect <código> <filtro jq> <rótulo>: confere o último req.
expect() {
	[ "$EX_HTTP_CODE" = "$1" ] || ex_fail "$3: HTTP $EX_HTTP_CODE, esperado $1: $EX_HTTP_BODY"
	jq -e "$2" >/dev/null 2>&1 <<<"$EX_HTTP_BODY" || ex_fail "$3: corpo inesperado: $EX_HTTP_BODY"
	echo "ok  $3"
}

chat() { printf '{"model":"%s","messages":[{"role":"user","content":"oi"}]}' "$1"; }

# --- 1. Listagens × fixtures, campo a campo, por id.
# Itens sem created; base_url reduzido ao caminho depois do host (a origem do
# wwz não é https://opencode.ai, ADR 0073).
CMP='def norm: del(.created) | (.api // {}) |= with_entries(.value.base_url |= sub("^https?://[^/]+"; ""));
def byid: map({key: .id, value: norm}) | from_entries;
($w[0].data | byid) as $g | ($r.data | byid) as $e
| [($e | keys[]) as $id
   | if $g[$id] == null then "\($id): falta no wwz"
     else ($e[$id] | keys[]) as $f | select($g[$id][$f] != $e[$id][$f])
       | "\($id).\($f): wwz \($g[$id][$f] | tojson) real \($e[$id][$f] | tojson)" end]
+ [($g | keys[]) as $id | select($e[$id] == null) | "\($id): só no wwz"]
+ [($g | keys[]) as $id | select($e[$id] != null) | ($g[$id] | keys[]) as $f
   | select($e[$id] | has($f) | not) | "\($id).\($f): só no wwz"]
| .[]'
for pair in payg_models:/inference/v1/models go_models:/inference/go/v1/models \
	zen_models:/zen/v1/models zen_go_models:/zen/go/v1/models; do
	fixture="${pair%%:*}" path="${pair#*:}"
	req GET "$path" -
	[ "$EX_HTTP_CODE" = 200 ] || ex_fail "GET $path: HTTP $EX_HTTP_CODE"
	printf '%s' "$EX_HTTP_BODY" >"$TMP/wwz.json"
	jq -r '.response.body' "$CATALOG/$fixture.json" >"$TMP/real.json"
	want="$(jq '.data | length' "$TMP/real.json")"
	ex_assert_between "$(jq '.data | length' "$TMP/wwz.json")" "$want" "$want" "modelos em $path ($fixture)"
	jq -rn --slurpfile w "$TMP/wwz.json" --slurpfile real "$TMP/real.json" "\$real[0] as \$r | $CMP" >"$TMP/diff.txt"
	if [ -s "$TMP/diff.txt" ]; then
		head -20 "$TMP/diff.txt" >&2
		ex_fail "$path difere de $fixture em $(wc -l <"$TMP/diff.txt" | tr -d ' ') campos"
	fi
	echo "ok  $path igual a $fixture campo a campo (fora created e host do base_url)"
done

# --- 2. Modo public (sem conta) nas bases pay-as-you-go.
req POST /zen/v1/chat/completions public "$(chat big-pickle)" "$OC_UA"
expect 200 '.usage.cost == 0 and .usage.opencode.billing_source == "free"' \
	"public + User-Agent opencode/ em gratuito: 200 billing_source free"
req POST /zen/v1/chat/completions public "$(chat big-pickle)"
expect 429 '.error.type == "FreeUsageLimitError" and (.error.message | startswith("Free usage limit reached."))' \
	"public sem User-Agent opencode/: 429 FreeUsageLimitError (cota zero fora do OpenCode)"
req POST /zen/v1/chat/completions public "$(chat kimi-k3)" "$OC_UA"
expect 401 '.error.type == "AuthError" and .error.message == "Missing API key."' \
	"public com modelo pago: 401 Missing API key."

# --- 3. Base Go: chave do plano Go dentro da cota.
req POST /zen/go/v1/chat/completions wwz-key-go "$(chat kimi-k3)"
expect 200 '.usage.cost == 0 and .usage.opencode.billing_source == "go" and .usage.opencode.allowance > 0' \
	"chave Go: 200 billing_source go, cost 0, allowance > 0"
req POST /zen/go/v1/chat/completions wwz-key-go-plus "$(chat glm-5.3-flash)"
expect 200 '.usage.cost == 0 and .usage.opencode.billing_source == "go-plus"' "chave Go Plus: billing_source go-plus"
req POST /zen/go/v1/chat/completions wwz-key-teams-ci "$(chat glm-5.3-flash)"
expect 200 '.usage.cost > 0 and .usage.opencode.billing_source == "balance"' \
	"service account do Go Teams na base Go: fora da cota, billing_source balance"
req GET /zen/go/v1/usage wwz-key-go
expect 200 '.usage.rolling.status == "ok" and .usage.weekly and .usage.monthly' "GET /zen/go/v1/usage: 200"
req POST /zen/go/v1/chat/completions wwz-key-payg "$(chat kimi-k3)"
expect 403 '.error.type == "ForbiddenError"' "conta sem Go (plan: none) na base Go: 403"

# --- 4. Budget por modelo (opencode/claude-opus-5-5: US$ 0,01) na base pay-as-you-go.
ok=0
for ((i = 0; i < 20; i++)); do
	req POST /zen/v1/chat/completions wwz-key-payg "$(chat claude-opus-5-5)"
	[ "$EX_HTTP_CODE" = 200 ] || break
	jq -e '.usage.opencode.billing_source == "balance" and .usage.cost > 0' >/dev/null <<<"$EX_HTTP_BODY" ||
		ex_fail "pay-as-you-go: uso inesperado: $EX_HTTP_BODY"
	ok=$((ok + 1))
done
ex_assert_between "$ok" 1 19 "respostas 200 (billing_source balance) até estourar o budget do modelo"
expect 429 '.error.type == "BudgetLimitError" and .metadata.coverage == "models"' \
	"budget por modelo: 429 BudgetLimitError coverage models"
req GET /console/api/policies/current-budgets wwz-key-payg
expect 200 'any(.[]; .coverage == "models" and (.models | index("opencode/claude-opus-5-5")) and .spentDollars >= .limitDollars)' \
	"Console: current-budgets mostra o budget do modelo estourado"
req POST /console/auth/device/code - '{}'
expect 200 '.device_code and .user_code and (.verification_uri_complete | test("/console/device\\?code="))' \
	"Console: device code (console_key: payg)"

echo "smoke opencode-completo: ok"

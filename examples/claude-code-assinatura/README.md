# claude-code-assinatura

A assinatura do Claude Code (OAuth, contas Pro/Max/Team/Enterprise) no driver
`anthropic` e o modo estrito do cliente, em que o wwz só aceita requisições do
Claude Code 2.1.308. Serve para testar um roteador que encaminha o tráfego do
Claude Code: o login, os headers `anthropic-ratelimit-unified-*` da cota de 5 h e
da semana, o 429 de cota que o Claude Code não retenta e o 403 de quem não é o
Claude Code. Specs [006](../../specs/006-fidelidade-provedores.md) (16.2.1),
[015](../../specs/015-wwz-load.md) e [020](../../specs/020-cliente-claude-code.md);
[ADR 0074](../../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md).
Guia completo em [docs/claude-code.md](../../docs/claude-code.md).

## O que demonstra

- **Dois mounts do mesmo driver**, com a mesma assinatura (`oauth_key: assinante`,
  `oauth_plan: max_20x`): `/anthropic` é a API real e aceita qualquer cliente;
  `/claude-code` tem `client_verification: claude-code` e recusa com 403
  `forged_request_error` tudo o que não for byte a byte o Claude Code.
- **Login OAuth** pelo `wwz-load claude-code login`: PKCE, `GET /oauth/authorize`,
  callback local, `POST /v1/oauth/token`, `/api/oauth/profile` e
  `/api/oauth/claude_cli/roles`. O perfil diz `claude_max` /
  `default_claude_max_20x`. Os tokens `sk-ant-oat01-…` são assinados com o valor da
  chave `assinante` e representam essa chave (conta `assinatura`).
- **Cota de assinatura** na conta (`quota_usd: {monthly_usd: 60, rolling_5h_pct: 20,
  weekly_pct: 50}`): com token OAuth, as respostas da Messages trazem
  `anthropic-ratelimit-unified-status`, `-5h-utilization`, `-7d-utilization`,
  `-representative-claim`, `-reset` e `-overage-*` no lugar dos
  `anthropic-ratelimit-requests-*`. Estourou a janela: 429 `rate_limit_error` com
  `status: rejected` e `x-should-retry: true`, que o Claude Code assinante (e o
  `wwz-load claude-code`) não retenta.
- **Modo estrito**: o `ask` e o replay de uma fixture do Claude Code passam; um
  `curl` comum sai com `reason=query` (sem `?beta=true`, como os SDKs oficiais) e um
  `curl` com o token e `?beta=true` com `reason=header_missing`.

## Como o smoke cabe em segundos

O preço de saída dos modelos é inflado (100 tokens fixos por resposta a
US$ 15 000 por milhão, ~US$ 1,50 por chamada), então ~8 chamadas gastam os US$ 12
da janela de 5 h. Para repetir sem esperar 5 h, derrube e suba o compose de novo
(`down -v`; o store é em memória). A API admin (porta 9000, token
`wwz-ex-admin-token`, com `POST /admin/entities/account/assinatura/reset` e
`{"scope":["counters"]}`) não é publicada neste exemplo; publique-a como no
`cota-5h-semana` se quiser zerar só a conta.

## Como subir

```bash
make image                                       # uma vez: white-wizard:dev
cd examples/claude-code-assinatura
EX_PORT=18117 docker compose up -d --wait
B=http://127.0.0.1:18117

# login e um turno, como o Claude Code (o estado fica em /tmp/cc.json)
go run ../../cmd/wwz-load claude-code login --target $B/claude-code --state /tmp/cc.json
go run ../../cmd/wwz-load claude-code ask "say ok" --target $B/claude-code --state /tmp/cc.json

# um cliente comum no mount estrito: 403 forged_request_error
curl -s -H 'x-api-key: wwz-ex-claude-code-assinante' -H 'anthropic-version: 2023-06-01' \
  -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"oi"}]}' \
  $B/claude-code/v1/messages | jq .error

# o token no mount sem verificação: 200 com os headers de assinatura
T=$(jq -r .access_token /tmp/cc.json)
curl -s -D - -o /dev/null -H "Authorization: Bearer $T" -H 'anthropic-beta: oauth-2025-04-20' \
  -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"oi"}]}' \
  $B/anthropic/v1/messages | grep -i unified
docker compose down -v
```

O Claude Code real também fala com este exemplo: emita um token de um ano com
`login --setup-token` e passe-o em `CLAUDE_CODE_OAUTH_TOKEN`, com
`ANTHROPIC_BASE_URL=$B/claude-code`, num ambiente limpo (`env -i` e
`CLAUDE_CONFIG_DIR` vazio, para não usar a sua conta). Receita completa em
[docs/claude-code.md](../../docs/claude-code.md#claude-code-real-contra-o-wwz).

Chave: `wwz-ex-claude-code-assinante` (label `assinante`, conta `assinatura`; o valor
também é o segredo dos tokens). Modelos: `claude-sonnet-5-5` (padrão do Claude Code
com OAuth), `claude-opus-5-5` (com API key) e `claude-haiku-5-5`.

## Smoke

`./smoke.sh` (ou `make example NAME=claude-code-assinatura`, ~20 s). O cliente roda
no host: `WWZ_LOAD_BIN` aponta um `wwz-load` pronto; sem ele, o smoke compila
`./cmd/wwz-load` num diretório temporário (exige Go). Confere, nesta ordem:

1. `--target https://api.anthropic.com` sai com código 2 sem abrir conexão.
2. `HEAD /claude-code/api/hello` responde 200 (a sonda não é verificada).
3. `login` grava um token `sk-ant-oat01-…` e mostra o plano
   `max (default_claude_max_20x)`.
4. `ask "say ok"` no mount estrito: 200.
5. `replay --send` da fixture `testdata/real/claude-code/2.1.308/002-sdk-cli-oauth-turn.raw`
   com o token do wwz: `identical sha256=5644b70f…` e HTTP 200.
6. `curl` comum no mount estrito: 403 `forged_request_error`, `x-should-retry: false`,
   `x-wwz-client-verification: failed; reason=query`; com o token e `?beta=true`,
   `reason=header_missing`.
7. O token no `/anthropic`: 200 com `anthropic-ratelimit-unified-status: allowed`,
   `5h-utilization` 0.25 e `7d-utilization` 0.1 (US$ 3 gastos), `representative-claim:
   five_hour` e sem `anthropic-ratelimit-requests-*`.
8. 4 a 6 respostas 200 até o 429 `rate_limit_error` com `unified-status: rejected`,
   `representative-claim: five_hour` e `retry-after`.
9. `ask` com a cota esgotada sai com código 1 e o wwz registra um único 429: o
   cliente não retenta o 429 de assinatura, mesmo com `x-should-retry: true`.

Derruba o compose com `down -v`, inclusive em falha.

## Notas

- O modo estrito precisa dos headers como o cliente os mandou. O compose publica a
  porta direto; atrás de um proxy que reescreve headers (Traefik, Go `net/http`,
  haproxy em `mode http`) toda requisição sai com `header_order`.
- Em produção o modo estrito é `http://<ip público>:8008/claude-code` (haproxy em TCP
  puro; o IP fica em `autor/prod.env`, fora do git). A 8004 é a mesma instância, mas
  pode estar em `mode http` (outro serviço do host divide a porta), que tira
  `Connection`: lá só a raiz, com a assinatura, serve
  ([deploy/prod/README.md](../../deploy/prod/README.md#claude-code-adr-0074-specs-006-1621-e-020)).
- A recusa do modo estrito não debita cota nem conta em limite: o passo 7 vê só o
  gasto do `ask` e do replay.

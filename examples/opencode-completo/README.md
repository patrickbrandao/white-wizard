# opencode-completo

O driver `opencode` com os catálogos reais de opencode.ai, os planos Go, Go
Plus e Go Teams, o pay-as-you-go, os modelos gratuitos e os budgets do Console
([ADR 0072](../../specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md),
item 18). Serve para testar um roteador contra o catálogo que o OpenCode
realmente lista, com as regras de cobrança de cada base.

## O que demonstra

- **Catálogos reais.** `models[]` sai das listagens públicas de opencode.ai
  gravadas sem chave em **03/11/2026** (task 416):
  `testdata/real/opencode/catalog/payg_models.json` (79 modelos, 9 gratuitos) e
  `go_models.json` (28 modelos, 2 gratuitos: `beam-free` e
  `iceman-contributor-free`). São 91 entradas: 16 ids aparecem nos dois
  catálogos e o que difere no Go (preço, limites, capacidades, nome,
  descrição, protocolo nativo) fica em `catalogs.opencode-go`. Não entram os
  modelos que só existem no snapshot do cliente nem os `jev-*` (SystemOne).
  `/inference/v1/models` e `/zen/v1/models` listam o pay-as-you-go;
  `/inference/go/v1/models` e `/zen/go/v1/models`, o Go.
- **Preço real.** `pricing` com `multipliers` (horário de pico do DeepSeek,
  promoção por data do Gemini 4 Argon) e `overrides` por contexto, como na
  fixture; o mesmo id com preço diferente por catálogo (`deepseek-v4-pro`:
  1,74/3,48 e 0,66/1,98).
- **Gratuitos e `public`.** `Authorization: Bearer public` sem conta atende só
  modelos gratuitos (`pricing` com `input_per_m: 0`) nas bases pay-as-you-go,
  e só com `User-Agent: opencode/...`; fora do cliente OpenCode a cota é zero
  (429 `FreeUsageLimitError`); modelo pago dá 401 `Missing API key.`.
- **Planos** (`tiers.<n>.plan`): `go` com a cota Go por modelo publicada
  (US$ 100 DeepSeek V4.1 Flash; US$ 60 GLM-5.3-Flash, Kimi K2.7 Code,
  MiniMax M3; US$ 15 Kimi K3, GPT 6 Luna, Claude Haiku 5.5; 5 h = 20 %,
  semana = 50 %); `go-plus` com 4× esses valores (**estimativa**: o real só
  diz "~4×"); `go-teams` (cota por membro, Extra Usage ligado); `payg`
  (`plan: none`, 403 nas bases Go).
- **Contas**: Extra Usage (`ws-go-plus`), membro do Go Teams
  (`projects[]` `ana`), service account (`teams-ci`), budgets
  `spend_monthly_usd` (coverage `all`) e `spend_monthly_models_usd`
  (coverage `models`), e `console_key: payg` para o login do Console.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/opencode-completo
EX_PORT=18116 EX_ADMIN_PORT=19116 docker compose up -d --wait
curl -s -H 'x-opencode-session: ses-demo' http://127.0.0.1:18116/inference/go/v1/models | jq '.data | length'
curl -s -H 'x-opencode-session: ses-demo' -H 'Authorization: Bearer public' -A 'opencode/latest/2.2.5/cli' \
  -H 'Content-Type: application/json' -d '{"model":"big-pickle","messages":[{"role":"user","content":"oi"}]}' \
  http://127.0.0.1:18116/zen/v1/chat/completions | jq .usage.opencode
docker compose down -v
```

| Chave | Conta | Plano | Para |
| --- | --- | --- | --- |
| `public` | — | — | gratuitos sem conta (bases pay-as-you-go) |
| `wwz-key-go` | `ws-go` | `go` | base Go, `billing_source: go` |
| `wwz-key-go-plus` | `ws-go-plus` | `go-plus` | `billing_source: go-plus`; Extra Usage |
| `wwz-key-teams-ana` | `ws-teams` (membro `ana`) | `go-teams` | cota Go no membro |
| `wwz-key-teams-ci` | `ws-teams` (service account) | `go-teams` | fora da cota Go: `balance` |
| `wwz-key-payg` | `ws-payg` | `none` | saldo, budgets, Console (`console_key`) |

A latência é a do perfil `default` deste arquivo (TTFT 2 ms, sem atraso entre
tokens).

## Smoke

`./smoke.sh` (ou `make example NAME=opencode-completo`, poucos segundos):

- As quatro listagens têm 79 e 28 modelos e batem **campo a campo** com as
  fixtures, item por item pelo id, fora `created` e o host do `base_url` (a
  origem do wwz não é `https://opencode.ai`; o caminho é conferido).
- `public` + `User-Agent: opencode/...` em `big-pickle`: 200,
  `billing_source: free`, `cost: 0`; sem o header: 429 `FreeUsageLimitError`;
  `public` em `kimi-k3` (pago): 401 `Missing API key.`.
- `wwz-key-go` em `/zen/go`: 200, `billing_source: go`, `cost: 0`,
  `allowance > 0`; `wwz-key-go-plus`: `billing_source: go-plus`; a service
  account `wwz-key-teams-ci`: `billing_source: balance`, `cost > 0`;
  `GET /zen/go/v1/usage`: 200. `wwz-key-payg` em `/zen/go`:
  403 (sem Go).
- `wwz-key-payg` em `claude-opus-5-5` até o budget do modelo (US$ 0,01)
  estourar: 429 `BudgetLimitError` com `metadata.coverage: "models"`; o
  Console (`current-budgets`) mostra o budget gasto; `device/code` responde.
- Derruba o compose com `down -v`, inclusive em falha.

## Como o `config.yaml` foi gerado

Por um script descartável (fora do repositório) que lê as duas fixtures e,
para cada id, preenche os campos próprios (`display_name`, `context_window`,
`max_output`, `protocols`, `capabilities`, `pricing`, `owned_by`, `created`) e
põe em `extra` só o que o wwz não deriva, com `null` tirando o campo derivado
que o real não lista. O cabeçalho do `config.yaml` descreve as convenções.
Para atualizar o catálogo, grave fixtures novas (task 416) e gere de novo; o
smoke acusa qualquer campo que deixar de bater.

## Notas

- Não conferidos no real (exigem chave ou forjar o cliente): a cota Go por
  modelo do Go Plus e do Go Teams, o Extra Usage, a service account, os
  budgets e o Console (spec 006, "Não conferidos no real").
- A ordem dos itens segue `models[]`, que não pode seguir a ordem real dos
  dois catálogos ao mesmo tempo; o smoke compara por id.

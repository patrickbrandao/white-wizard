# Uso do wwz

Guia prático: subir o simulador, chamar cada driver, controlar a resposta por header, usar cenários, injetar caos e operar pela API admin. Referência normativa: specs [004](../specs/004-apis-e-endpoints.md), [009](../specs/009-erros-e-caos.md), [011](../specs/011-cenarios.md) e [013](../specs/013-observabilidade-admin.md). Todos os comandos abaixo foram executados contra o compose local.

## Subir

**Compose local** (wwz + Valkey + Prometheus + painel):

```sh
WWZ_ADMIN_TOKEN=troque-me docker compose up -d --build
B=http://127.0.0.1:18000      # data plane (inferência)
A=http://127.0.0.1:18001      # control plane (admin, /health, /metrics)
T='Authorization: Bearer troque-me'
```

O YAML montado é [deploy/wwz/wwz.yaml](../deploy/wwz/wwz.yaml); troque com `WWZ_CONFIG_FILE=./meu.yaml docker compose up -d wwz`. Portas publicadas: `WWZ_PORT`, `WWZ_ADMIN_PORT`, `WWZ_PANEL_PORT`, `PROMETHEUS_PORT`, `VALKEY_PORT`. Sem Valkey: `WWZ_STORE=memory WWZ_VALKEY_ADDR=`.

**Binário local** (store em memória):

```sh
go run ./cmd/wwz serve -scenarios-dir ./scenarios            # :8000 e 127.0.0.1:9000
go run ./cmd/wwz serve -config meu.yaml -port 8080 -admin-port 9090
```

Sem `WWZ_ADMIN_TOKEN` (ou `admin.token`) o control plane só escuta em loopback e não pede token; com token, escuta em `admin.listen` e todo `/admin/*` exige o bearer.

**Saúde e métricas** ficam no control plane:

```sh
curl -s $A/health            # {"status":"ok"}
curl -s $A/health/ready      # 503 durante server.startup_duration, depois 200
curl -s $A/metrics | grep '^wwz_requests_total'
```

### Padrões embutidos

Sem `models`, `keys` e `drivers` no YAML, o `wwz` sobe com:

- modelos `wwz-fast` (sem latência), `wwz-realistic` (latência parecida com um modelo hospedado) e `wwz-flaky` (latência de cauda longa e 5 % de erros);
- chaves `wwz-key-01` a `wwz-key-10` (com `server.auth: keys`);
- drivers `openai`, `anthropic`, `openrouter` e `opencode`, todos na raiz.

## Chamar cada driver

Cada driver atende as rotas do provedor real; detalhes em [drivers.md](drivers.md). Na raiz (sem prefixo), rotas que existem em mais de um driver (`/v1/models`) são resolvidas pela credencial: `x-api-key` é Anthropic, `Authorization: Bearer` é OpenAI.

**OpenAI** (Chat Completions):

```sh
curl -s $B/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":20}'

# streaming com usage no último chunk
curl -sN $B/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","stream":true,"stream_options":{"include_usage":true},
       "messages":[{"role":"user","content":"oi"}],"max_tokens":20}'

curl -s $B/v1/models -H 'Authorization: Bearer wwz-key-01'
```

**Anthropic** (Messages):

```sh
curl -sN $B/v1/messages \
  -H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"oi"}]}'

curl -s $B/v1/models -H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01'
```

**OpenRouter** (rotas sob `/api/v1`, `model` com o slug `autor/modelo`; os modelos do wwz usam o autor `white-wizard`):

```sh
curl -s $B/api/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"white-wizard/wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":20}'

curl -s $B/api/v1/models -H 'Authorization: Bearer wwz-key-01'
curl -s $B/api/v1/key -H 'Authorization: Bearer wwz-key-01'     # crédito e limites da chave
```

**OpenCode** (bases pay-as-you-go `/inference` e `/zen`, bases Go `/inference/go` e `/zen/go`; `Authorization: Bearer` em todas as rotas, inclusive Messages, onde `x-api-key` também vale; o header `x-opencode-session` é obrigatório, sem ele a resposta é 400 `MissingSessionID`, como no real; modo `public`, cobrança por base e Console [abaixo](#apontar-o-cliente-opencode-ou-um-roteador-para-o-wwz)):

```sh
curl -s $B/zen/go/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'x-opencode-session: ses_1' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":20}'

curl -s $B/inference/go/anthropic/v1/messages \
  -H 'Authorization: Bearer wwz-key-01' -H 'x-opencode-session: ses_1' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","max_tokens":20,"messages":[{"role":"user","content":"oi"}]}'

# Responses e rota Gemini (esta só em /inference, chave em x-goog-api-key ou bearer).
# Como no real, todo modelo do catálogo do opencode responde em todas as rotas,
# qualquer que seja protocols (ADR 0062); gemini-* volta na forma do Vertex, os demais traduzidos
curl -s $B/inference/go/openai/v1/responses \
  -H 'Authorization: Bearer wwz-key-01' -H 'x-opencode-session: ses_1' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","input":"oi","max_output_tokens":20}'
curl -s $B/inference/go/google/v1beta/models/wwz-fast:generateContent \
  -H 'x-goog-api-key: wwz-key-01' -H 'x-opencode-session: ses_1' -H 'content-type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"oi"}]}]}'

curl -s $B/zen/go/v1/usage -H 'Authorization: Bearer wwz-key-01'  # janelas 5 h, semana e mês
```

**Responses API** (OpenAI, Azure v1, OpenRouter e OpenCode; só a OpenAI guarda estado):

```sh
curl -s $B/v1/responses \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","input":"oi","max_output_tokens":20}'
curl -s $B/api/v1/responses \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"white-wizard/wwz-fast","input":"oi","max_output_tokens":20}'
```

**Gemini e Azure OpenAI** ([ADR 0043](../specs/decisions/0043-drivers-gemini-e-azure.md)) não fazem parte dos padrões embutidos: declare-os em `drivers:` (por exemplo `- {name: gemini, prefix: /gemini}` e `- {name: azure, prefix: /azure}`, ver abaixo) ou use o `deploy/prod/config/todos.yaml`. Os modelos embutidos falam só `openai` e `anthropic` (`protocols`), então a rota Gemini responde 404 `models/wwz-fast is not found` até um modelo declarar o dialeto, por exemplo `models: [{id: wwz-fast, protocols: ["*"]}]` (ver [configuracao.md](configuracao.md)); o `todos.yaml` já faz isso. Com os prefixos acima e esse modelo:

```sh
# Gemini: x-goog-api-key ou ?key=; stream com ?alt=sse
curl -s $B/gemini/v1beta/models/wwz-fast:generateContent \
  -H 'x-goog-api-key: wwz-key-01' -H 'content-type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"oi"}]}]}'
curl -sN "$B/gemini/v1beta/models/wwz-fast:streamGenerateContent?alt=sse" \
  -H 'x-goog-api-key: wwz-key-01' -H 'content-type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"oi"}]}]}'
curl -s $B/gemini/v1beta/models -H 'x-goog-api-key: wwz-key-01'

# Azure OpenAI: o deployment vai na rota (é o id do modelo) e api-version é obrigatório
curl -s "$B/azure/openai/deployments/wwz-fast/chat/completions?api-version=2024-10-21" \
  -H 'api-key: wwz-key-01' -H 'content-type: application/json' \
  -d '{"messages":[{"role":"user","content":"oi"}],"max_tokens":20}'
# API v1 (sem api-version): o deployment vai em "model"
curl -s $B/azure/openai/v1/responses \
  -H 'api-key: wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","input":"oi","max_output_tokens":20}'
```

### Apontar o cliente OpenCode (ou um roteador) para o wwz

O `opencode` imita o opencode.ai ([drivers.md](drivers.md#opencode), [ADR 0072](../specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)). O cliente OpenCode, ou o roteador sob teste, só precisa trocar a URL base do provedor (no cliente, a opção `baseURL` do provedor) por uma base do wwz:

| Provedor imitado | URL base (driver na raiz) | Com `prefix: /opencode` |
| --- | --- | --- |
| OpenCode pay-as-you-go e gratuitos | `http://host:8000/inference` (ou a legada `/zen`) | `http://host:8000/opencode/inference` |
| OpenCode Go | `http://host:8000/inference/go` (ou `/zen/go`) | `http://host:8000/opencode/inference/go` |

O cliente segue o `api.<protocolo>.base_url` absoluto que a listagem devolve. Por isso, atrás de um proxy, declare o proxy em `server.trusted_proxies` para que `X-Forwarded-Proto`, `X-Forwarded-Host` e `X-Forwarded-Prefix` entrem na URL; sem isso, o wwz usa `Host` e o próprio TLS ([drivers.md](drivers.md#origem-das-urls-absolutas)). Toda requisição de inferência leva `x-opencode-session`, como no real.

O [`examples/opencode-completo`](../examples/opencode-completo/README.md) tem os catálogos reais, os planos, a service account, os budgets e o Console prontos (porta 18116):

```sh
make image
cd examples/opencode-completo && EX_PORT=18116 EX_ADMIN_PORT=19116 docker compose up -d --wait
O=http://127.0.0.1:18116
```

**Modo `public` (sem conta).** Só os modelos gratuitos das bases pay-as-you-go atendem, e só com `User-Agent: opencode/...`. Os modelos embutidos (`wwz-fast`…) não têm `pricing` e por isso não são gratuitos; o exemplo traz os gratuitos reais, como `big-pickle`.

```sh
# 200, usage.opencode.billing_source: "free", cost 0; conta na cota diária do IP
curl -s $O/zen/v1/chat/completions -A 'opencode/latest/2.2.5/cli' \
  -H 'Authorization: Bearer public' -H 'x-opencode-session: ses_1' -H 'content-type: application/json' \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"oi"}]}' | jq .usage.opencode

# sem o User-Agent do OpenCode: 429 FreeUsageLimitError (a cota fora do cliente é zero)
# modelo pago com public: 401 "Missing API key."; base Go com public: 401 "Go requires a credential"
curl -si $O/zen/v1/chat/completions \
  -H 'Authorization: Bearer public' -H 'x-opencode-session: ses_1' -H 'content-type: application/json' \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"oi"}]}' | head -1
```

A cota diária é `drivers[].options.free_requests_per_day` (padrão 200), por IP sem chave e por conta com chave, e vira às 00:00 UTC.

**Com chave.** Cada chave do exemplo representa um plano. A origem da cobrança sai em `usage.opencode.billing_source`:

```sh
# base Go: billing_source "go", cost 0, allowance > 0 (a cota Go cai pelo allowance)
curl -s $O/zen/go/v1/chat/completions -H 'Authorization: Bearer wwz-key-go' -H 'x-opencode-session: ses_1' \
  -H 'content-type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"oi"}]}' | jq .usage.opencode
curl -s $O/zen/go/v1/usage -H 'Authorization: Bearer wwz-key-go'          # janelas 5 h, semana e mês

# pay-as-you-go: billing_source "balance", debita saldo, spend e budgets
curl -s $O/inference/v1/chat/completions -H 'Authorization: Bearer wwz-key-payg' -H 'x-opencode-session: ses_1' \
  -H 'content-type: application/json' \
  -d '{"model":"claude-opus-5-5","messages":[{"role":"user","content":"oi"}]}' | jq .usage.opencode
```

Um budget só nega depois que o gasto passa do limite: a primeira chamada passa, e as seguintes recebem 429 `BudgetLimitError`.

**Console.** Fica em `/console/...` na raiz do mount (não sob `/zen`). O login aprova a conta de `drivers[].options.console_key` (`wwz-key-payg` no exemplo) depois de 10 s:

```sh
curl -s -X POST $O/console/auth/device/code -H 'content-type: application/json' -d '{"client_id":"opencode"}'
# {"device_code":"…","user_code":"XXXX-XXXX","verification_uri_complete":"http://127.0.0.1:18116/console/device?code=…","expires_in":900,"interval":5}
curl -s -X POST $O/console/auth/device/token -H 'content-type: application/json' \
  -d '{"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"<device_code>"}'
# slow_down antes de 5 s, authorization_pending até 10 s, depois access_token e refresh_token
curl -s $O/console/api/policies/current-budgets -H 'Authorization: Bearer <access_token>'
```

O access token vale como a chave `console_key` também na inferência.

```sh
docker compose down -v
```

### Drivers montados em prefixos

Com `drivers[].prefix`, vários drivers convivem na mesma porta sem depender da credencial:

```yaml
drivers:
  - { name: openai, prefix: /openai }
  - { name: anthropic, prefix: /anthropic }
  - { name: openrouter, prefix: /openrouter }
  - { name: opencode, prefix: /opencode }
  - { name: gemini, prefix: /gemini }
  - { name: azure, prefix: /azure }
```

```sh
curl -s $B/openai/v1/chat/completions -H 'Authorization: Bearer <chave>' ...
curl -s $B/anthropic/v1/messages -H 'x-api-key: <chave>' -H 'anthropic-version: 2023-06-01' ...
curl -s $B/openrouter/api/v1/chat/completions -H 'Authorization: Bearer <chave>' ...
curl -s $B/opencode/zen/go/v1/chat/completions -H 'Authorization: Bearer <chave>' -H 'x-opencode-session: ses_1' ...
curl -s $B/gemini/v1beta/models/wwz-fast:generateContent -H 'x-goog-api-key: <chave>' ...
curl -s "$B/azure/openai/deployments/wwz-fast/chat/completions?api-version=2024-10-21" -H 'api-key: <chave>' ...
```

A URL base dos SDKs inclui o prefixo: `OpenAI(base_url="http://host:8000/openai/v1")`, `Anthropic(base_url="http://host:8000/anthropic")`.

## Headers `x-wwz-*`

Controle por requisição ([spec 017](../specs/017-recursos-importados.md), I4). Valem enquanto `server.request_headers` for `true` (padrão); com `false` são ignorados, para medir vazão pura. Valor inválido é ignorado.

| Header (requisição) | Efeito |
| --- | --- |
| `x-wwz-response: <texto>` | Texto literal da resposta |
| `x-wwz-response-base64: <b64>` | O mesmo, em base64 (para texto com quebras de linha ou não ASCII) |
| `x-wwz-tool-calls: <json>` | Lista de tool calls a emitir (`[{"name":"x","arguments":{...}}]`); ignora `tool_choice` |
| `x-wwz-tool-prob: <0–100>` | Probabilidade de emitir tool call nesta requisição |
| `x-wwz-content-filter: true` | Recusa de conteúdo na forma do provedor (`finish_reason: content_filter`, `stop_reason: refusal`) |
| `x-wwz-return-error: <tipo>` | Força um erro do catálogo da [spec 009](../specs/009-erros-e-caos.md), sem sorteio |
| `x-wwz-delay-ms: <ms>` | Atraso fixo no lugar da latência sorteada |
| `x-wwz-seed: <n>` | Semente desta requisição (mesma semente, mesma resposta) |
| `x-wwz-scenario: <nome>` | Escolhe o cenário |
| `x-wwz-conversation-id: <id>` | Identifica a conversa (variáveis do cenário) além do hash da 1.ª mensagem |
| `x-wwz-chaos-*` | Sobrescreve o perfil de erros (ver [Caos](#caos)) |
| `X-Test-Id: <id>` | Escopo de teste para caos one-shot e relatório de resiliência (nome em `chaos.scope_header`) |

| Header (resposta) | Quando |
| --- | --- |
| `x-wwz-scenario`, `x-wwz-step`, `x-wwz-attempt` | Um cenário casou |
| `x-wwz-seed` | Semente usada |
| `x-wwz-input-breakdown` | Com `usage.extended: true`: tokens de entrada por origem (JSON) |
| `x-wwz-pod` | Com `POD_NAME`/`POD_IP`: identifica a instância atrás do balanceador |

Exemplos:

```sh
curl -s $B/v1/chat/completions -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -H 'x-wwz-response: Olá do wwz' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}]}'

# 429 no formato OpenAI, com retry-after
curl -si $B/v1/chat/completions -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -H 'x-wwz-return-error: rate_limit' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}]}'

# 529 overloaded_error no formato Anthropic
curl -si $B/v1/messages -H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
  -H 'x-wwz-return-error: overloaded' \
  -d '{"model":"wwz-fast","max_tokens":10,"messages":[{"role":"user","content":"oi"}]}'
```

## Cenários

Um cenário é uma lista de passos; o passo é deduzido do histórico da própria requisição (quantos tool results, última tool chamada), então qualquer instância atende qualquer turno ([spec 011](../specs/011-cenarios.md)). Exemplos prontos em [scenarios/](../scenarios/): `file-agent-flow`, `search-then-answer` e `retry-on-error`.

**Carregar.** Pelo diretório (`-scenarios-dir ./scenarios`, `WWZ_SCENARIOS_DIR` ou `scenarios.dir`) ou em tempo de execução:

```sh
curl -s $A/admin/scenarios -H "$T" -H 'content-type: application/yaml' --data-binary @scenarios/search-then-answer.yaml
curl -s $A/admin/scenarios -H "$T"                                    # lista
curl -s -X DELETE "$A/admin/scenarios?name=search-then-answer" -H "$T"  # remove
```

**Escolher** (ordem de `scenarios.select`): header `x-wwz-scenario`, `metadata.scenario` (OpenAI) ou `metadata.user_id` com prefixo `scenario:` (Anthropic), marcador `[[wwz:nome]]` na última mensagem do usuário, sufixo `-scenario-<nome>` no `model`, e por fim o `match` dos cenários listados em `models[].scenarios`.

```sh
curl -si $B/v1/chat/completions -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -H 'x-wwz-scenario: search-then-answer' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"clima em SP"}],
       "tools":[{"type":"function","function":{"name":"web_search","parameters":{"type":"object",
         "properties":{"query":{"type":"string"}},"required":["query"]}}}]}'
# x-wwz-scenario: search-then-answer / x-wwz-step: search, tool call web_search

curl -si $B/v1/chat/completions -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast-scenario-retry-on-error","messages":[{"role":"user","content":"x"}],
       "tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}'
# 1.ª tentativa: 429 com retry-after (fail_first); a repetição passa
```

**Sem servidor:**

```sh
go run ./cmd/wwz validate scenarios/                                  # sintaxe, passos e templates
go run ./cmd/wwz simulate -scenario file-agent-flow -turns 6 -driver anthropic
```

## Caos

Erros são sorteados por requisição e saem no formato exato do provedor ([spec 009](../specs/009-erros-e-caos.md)). Camadas, da mais fraca à mais forte: perfil global → modelo → chave → passo de cenário → headers `x-wwz-chaos-*` → `POST /admin/chaos` → `POST /admin/chaos/next`.

**Perfil de erros** (YAML, por modelo ou por chave):

```yaml
profiles:
  errors:
    flaky: { rate: 5, types: [rate_limit, server_error, overloaded], mid_stream_rate: 2, mid_stream_types: [stream_truncated, stream_error_event] }
models:
  - { id: meu-modelo, protocols: [openai], errors: flaky }
```

Tipos antes da resposta: `invalid_request`, `context_length`, `invalid_api_key`, `permission`, `model_not_found`, `request_too_large`, `rate_limit`, `quota`, `server_error`, `bad_gateway`, `overloaded`, `gateway_timeout`, `content_filter`, `connection_reset`, `slow_headers`. No meio do stream: `stream_error_event`, `stream_truncated`, `stream_hang`, `stream_malformed`, `stream_reset`, `stream_slow_tail`, `finish_length`, `usage_missing`.

**Janelas de indisponibilidade** (`errors.outages`): cron de seis campos (com segundos), duração e tipo; dentro da janela a requisição recebe o erro sem sorteio e `retry-after` é o tempo até o fim da janela.

```yaml
models:
  - id: mock-outage
    protocols: [openai]
    errors:
      outages:
        - { schedule: "*/10 * * * * *", duration: 5s, type: overloaded }   # 5 s fora a cada 10 s
```

**Por header** (só a requisição que os traz):

```sh
curl -s -o /dev/null -w '%{http_code}\n' $B/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -H 'x-wwz-chaos-rate: 100' -H 'x-wwz-chaos-types: overloaded' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}]}'          # 503
```

Headers: `x-wwz-chaos-profile` (nome de `profiles.errors`), `x-wwz-chaos-rate`, `x-wwz-chaos-types`, `x-wwz-chaos-mid-stream-rate`, `x-wwz-chaos-mid-stream-types`.

**Por evento admin** (todo o tráfego, até `duration`):

```sh
curl -s $A/admin/chaos -H "$T" -H 'content-type: application/json' \
  -d '{"type":"overloaded","duration":"30s","models":["*"]}'
curl -s $A/admin/chaos -H "$T"                 # eventos ativos
curl -s -X DELETE $A/admin/chaos -H "$T"       # encerra todos
```

**One-shot por teste** (o próximo request com aquele `X-Test-Id`):

```sh
curl -s $A/admin/chaos/next -H "$T" -H 'content-type: application/json' -d '{"test_id":"t1","status":503}'
curl -s -o /dev/null -w '%{http_code}\n' $B/v1/chat/completions -H 'Authorization: Bearer wwz-key-01' \
  -H 'content-type: application/json' -H 'X-Test-Id: t1' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}]}'          # 503, o seguinte 200
curl -s "$A/admin/resilience" -H "$T"          # retries, backoff e respeito a retry-after por X-Test-Id
```

## Limites

Limites por chave, tier ou conta (`keys[].limits`, `tiers.<nome>.limits`; [spec 010](../specs/010-controle-de-taxa.md)) saem com os headers do provedor:

```yaml
keys:
  - { key: sk-test-limited, label: com-cota, limits: { rpm: 2 } }
```

```sh
curl -si $B/v1/chat/completions -H 'Authorization: Bearer sk-test-limited' ...
# x-ratelimit-limit-requests: 2, x-ratelimit-remaining-requests, x-ratelimit-reset-requests: 30s
# 3.ª requisição no minuto: 429 com retry-after
# no driver anthropic: anthropic-ratelimit-requests-* com reset em RFC 3339
```

## API admin

Control plane em `admin.listen` (padrão `:9000`; `18001` no compose). Com `WWZ_ADMIN_TOKEN`, todo `/admin/*` exige `Authorization: Bearer <token>` e devolve 401 sem ele. Contrato completo em [cmd/wwz/openapi.yaml](../cmd/wwz/openapi.yaml).

| Rota | Uso |
| --- | --- |
| `GET /admin/config` | Configuração efetiva |
| `POST /admin/config` | Patch parcial aplicado na hora, sem reiniciar |
| `GET /admin/stats` | Contadores por chave, modelo, erro e limite |
| `POST /admin/reset` | Zera contadores de limite e métricas |
| `GET /admin/requests?last=N` | Últimas requisições e decisões (exige `admin.debug: true`) |
| `GET`/`POST`/`DELETE /admin/scenarios` | Lista, carrega (corpo YAML) e remove cenários |
| `POST`/`GET`/`DELETE /admin/chaos`, `POST /admin/chaos/next` | Caos pontual e one-shot |
| `GET /admin/entities`, `GET /admin/entities/{tipo}/{id}` | Estado de chaves, contas, projetos e IPs |
| `POST /admin/entities/{tipo}/{id}/{block,unblock,ban,unban,credits,tier,reset}` | Ações de operador |
| `POST /admin/leak` | Simula chave vazada: bane a chave e bloqueia a conta |
| `GET /admin/resilience` | Relatório de resiliência do cliente por `X-Test-Id` |
| `POST /admin/conversations/reset` | Zera as tentativas (`attempts`) dos cenários |

```sh
curl -s -o /dev/null -w '%{http_code}\n' $A/admin/config          # 401 sem token
curl -s $A/admin/config -H "$T" -H 'content-type: application/json' \
  -d '{"models":{"wwz-fast":{"errors":{"rate":100,"types":["server_error"]}}}}'   # wwz-fast passa a dar 500
curl -s $A/admin/config -H "$T" -H 'content-type: application/json' \
  -d '{"models":{"wwz-fast":{"errors":"none"}}}'                                    # volta ao normal

# bloqueio temporário de uma chave (pelo label): 403 account_suspended no dialeto do driver
curl -s $A/admin/entities/key/test-02/block -H "$T" -H 'content-type: application/json' -d '{"duration":"30s","reason":"teste"}'
curl -s -X POST $A/admin/entities/key/test-02/unblock -H "$T"

curl -s "$A/admin/requests?last=5" -H "$T"
curl -s -X POST $A/admin/reset -H "$T"
```

A configuração também recarrega com `SIGHUP` (relê o arquivo). Configuração inválida é recusada por inteiro e a anterior continua valendo ([configuracao.md](configuracao.md#recarga)).

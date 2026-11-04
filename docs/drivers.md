# Drivers

Cada provedor simulado é um driver: rotas, autenticação, parsing, serialização de sucesso e de erro, headers de limite e listagem de modelos no dialeto do provedor. O núcleo (geração, latência, caos, limites, métricas) é o mesmo para todos. Fonte da verdade: [spec 005](../specs/005-drivers.md) (interface e registro) e [spec 006](../specs/006-fidelidade-provedores.md) (limites, bloqueios e erros de cada provedor, com fontes).

## Princípio de fidelidade

[ADR 0042](../specs/decisions/0042-fidelidade-total-ao-provedor-real.md): todo comportamento observável de um driver (aceitação e recusa de requisições, textos e envelopes de erro, headers, limites, cache de prompt, formas de resposta e de stream, ids de modelo) é idêntico ao do provedor real. Não há ajuste, atalho nem opção que faça o simulador divergir, com uma exceção pedida pelo humano: o modo estrito `client_verification: claude-code` do `anthropic`, opt-in por mount, que recusa clientes que não são o Claude Code ([ADR 0074](../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md), [Anthropic](#anthropic-assinatura-do-claude-code-e-modo-estrito)).

- Sem configuração de falha, o driver **é** o provedor real.
- O que o operador configura (latência, erros, limites, cenários, headers `x-wwz-*`) é o propósito do simulador e sai exatamente na forma em que o provedor real o produziria.
- A validação das requisições vale sempre: o que o real recusa, o wwz recusa com o mesmo status, envelope e texto ([spec 004](../specs/004-apis-e-endpoints.md)).
- Cada comportamento é conferido contra o provedor real e vira fixture ou teste ([ADR 0014](../specs/decisions/0014-fidelidade-bit-a-bit.md)); o que veio só de documentação ou relato público fica marcado "não conferido" na spec 006 (lista em [spec 006, "Não conferidos no real"](../specs/006-fidelidade-provedores.md#não-conferidos-no-real)) até ganhar fixture.

## Visão geral

| Driver | Estado | Rotas (sem prefixo) | Credencial |
| --- | --- | --- | --- |
| `openai` | implementado | `/v1/chat/completions`, `/v1/completions`, `/v1/responses` (com estado: `previous_response_id`, `store`), `/v1/models`, `/v1/models/{id}` | `Authorization: Bearer` |
| `anthropic` | implementado | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`, `/v1/models/{id}`, `/api/hello`, `/v1/oauth/hello`; com `oauth_key`, as rotas OAuth e de conta da assinatura do Claude Code ([Anthropic](#anthropic-assinatura-do-claude-code-e-modo-estrito)) | `x-api-key` (ou `Authorization: Bearer`) + `anthropic-version`; com `oauth_key`, também o token de assinatura `Authorization: Bearer sk-ant-oat01-…` com o beta `oauth-2025-04-20` |
| `openrouter` | implementado | `/api/v1/chat/completions`, `/api/v1/completions`, `/api/v1/messages`, `/api/v1/responses` (sem estado: `store: true` e `previous_response_id` dão 400, como no real), `/api/v1/models`, `/api/v1/model/{autor}/{slug}`, `/api/v1/key` | `Authorization: Bearer` |
| `opencode` | implementado | Sob `/inference`, `/inference/go`, `/zen` e `/zen/go`: `/openai/v1/chat/completions`, `/v1/chat/completions`, `/openai/v1/responses`, `/v1/responses` (roteamento "portável": sem estado e sem campos que exigem alvo nativo, como no real), `/anthropic/v1/messages`, `/v1/messages`, `/v1/models`, `/v1/usage`; só sob `/inference` e `/inference/go`: `/google/v1beta/models/{m}:generateContent` e `:streamGenerateContent`; na raiz do mount: o Console em `/console/...` ([OpenCode](#console)) | `Authorization: Bearer` em todas as rotas, inclusive Messages (também `x-api-key` na rota Messages; `x-goog-api-key` ou bearer na rota Gemini); `public` é o modo gratuito sem conta ([OpenCode](#modo-public-e-modelos-gratuitos)); `x-opencode-session` obrigatório (400 `MissingSessionID`). Todo modelo do catálogo da base responde em todas as rotas, qualquer que seja `protocols` (ADR 0062); modelo desconhecido ou fora do catálogo da base é 400 `Model is unavailable.` |
| `gemini` | implementado ([ADR 0043](../specs/decisions/0043-drivers-gemini-e-azure.md)) | `/v1beta/models`, `/v1beta/models/{m}`, `/v1beta/models/{m}:generateContent`, `:streamGenerateContent`, `:countTokens`, `/v1beta/cachedContents` e `/v1beta/cachedContents/{id}` (e o mesmo sob `/v1/`) | `x-goog-api-key` ou `?key=` |
| `azure` | implementado, não conferido ([ADR 0043](../specs/decisions/0043-drivers-gemini-e-azure.md)) | `/openai/deployments/{deployment}/chat/completions?api-version=…`, `/openai/deployments/{deployment}/completions`, `/openai/models`, `/openai/v1/chat/completions`, `/openai/v1/responses`, `/openai/v1/models` | `api-key` ou `Authorization: Bearer` |

Exemplos de chamada com `curl` em [uso.md](uso.md#chamar-cada-driver).

## Mounts e detecção na raiz

Os drivers ativos vêm de `drivers:` no YAML; sem a seção, `openai`, `anthropic`, `openrouter` e `opencode` sobem na raiz.

```yaml
drivers:
  - name: openai
    prefix: /openai          # vazio = raiz; permite vários drivers na mesma porta
  - name: anthropic
    prefix: /anthropic
  - name: openrouter
    prefix: /openrouter
  - name: opencode
    prefix: /opencode
  - name: gemini
    prefix: /gemini          # SDK: http_options.base_url = http://host:8000/gemini
  - name: azure
    prefix: /azure           # SDK: AzureOpenAI(azure_endpoint="http://host:8000/azure")
```

Sem `prefix`, cada driver atende seus caminhos canônicos na raiz. Caminhos exclusivos escolhem o driver sozinhos (`/api/v1/...` é OpenRouter; `/inference/...` e `/zen/...` são OpenCode; `/v1beta/...` é Gemini; `/openai/deployments/...` e `/openai/v1/...` são Azure). Caminhos compartilhados (`/v1/models`) são resolvidos pela credencial: `x-api-key` sem `Authorization` é Anthropic, `Authorization: Bearer sk-or-…` é OpenRouter, `x-goog-api-key` ou `?key=` é Gemini, `api-key` é Azure; `keys[].driver` desempata.

Com os seis drivers na raiz, em qualquer ordem, a requisição de cada SDK oficial cai no driver certo (teste `TestRootDetectionSixSDKs` em `internal/server`): `Authorization: Bearer` + `/v1/chat/completions` (OpenAI), `x-api-key` + `anthropic-version` + `/v1/messages` (Anthropic), `x-goog-api-key` + `/v1beta/models/{m}:generateContent` (Gemini), `api-key` + `/openai/deployments/{d}/chat/completions?api-version=` (Azure), `/api/v1/chat/completions` (OpenRouter) e `/zen/go/v1/chat/completions` com `x-opencode-session` (OpenCode).

URL base dos SDKs oficiais (com prefixo `/<driver>`, ou sem ele na raiz):

| SDK | URL base |
| --- | --- |
| `openai` | `http://host:8000/openai/v1` |
| `anthropic` | `http://host:8000/anthropic` |
| `openai` contra o OpenRouter | `http://host:8000/openrouter/api/v1` |
| `openai` contra o OpenCode | `http://host:8000/opencode/zen/go/v1` (ou `/inference/go/openai/v1`) |
| `google-genai` (Python) e `@google/genai` (Node) | `http_options.base_url` / `httpOptions.baseUrl = http://host:8000/gemini` |
| `AzureOpenAI` (Python e Node) | `azure_endpoint = http://host:8000/azure`, `api_version = 2024-10-21` |

## Limites por driver

Nenhum provedor real limita por chave: todos contam por organização ou conta (com sub-limites por projeto ou workspace) e usam reposição contínua. A chave só resolve para essas entidades ([spec 005](../specs/005-drivers.md), `LimitPolicy`). Os valores vêm de `keys[].limits` e `tiers` ([configuracao.md](configuracao.md#limits)).

| Driver | Entidades | Unidades | Headers de limite |
| --- | --- | --- | --- |
| `openai` | conta (org), projeto | RPM, TPM (reserva `max_tokens`), RPD, TPD | `x-ratelimit-{limit,remaining,reset}-{requests,tokens}`, reset em duração Go (`6m0s`); `retry-after` em 429 e 503 |
| `anthropic` | conta (org), workspace | RPM, ITPM (sem leitura de cache), OTPM | `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{limit,remaining,reset}`, reset em RFC 3339; `retry-after`. Com token de assinatura, a cota de 5 h e da semana (`quota_usd`) nos `anthropic-ratelimit-unified-*` |
| `opencode` | workspace (conta), membro (projeto), sessão; IP sem chave | Cota Go em US$ (janelas de 5 h, semana e mês, inteira e por modelo) nas bases Go; saldo e budgets mensais (`all` e `models`) nas bases pay-as-you-go; requisições por dia nos gratuitos ([Cobrança por base](#cobrança-por-base)) | Nenhum; só `retry-after` (até o reset da janela) |
| `openrouter` | conta | Crédito por chave, orçamento em voo, RPM/RPD dos modelos `:free` | `X-RateLimit-*` só no 429 próprio |
| `gemini` | projeto | RPM, TPM de entrada, RPD (reset à meia-noite do Pacífico) | Nenhum |
| `azure` | deployment (`<conta>/<modelo>`); a conta guarda só os limites em dólar | TPM do deployment (reserva `max_tokens`) e RPM derivado, avaliado em janelas de 1 s ou 10 s | `x-ratelimit-*` quando há limite; `retry-after-ms` no 429 |

## Erros fiéis

Cada erro, injetado ou de limite, sai com o status, o envelope e os headers do provedor ([spec 006](../specs/006-fidelidade-provedores.md), catálogo de tipos na [spec 009](../specs/009-erros-e-caos.md)).

| Driver | Envelope | Sobrecarga | Cota esgotada | Erro no meio do stream |
| --- | --- | --- | --- | --- |
| `openai` | `{"error": {"message", "type", "param", "code"}}` | 503 `server_is_overloaded` | 429 `insufficient_quota` (não retentar) | Chunk com `error` no topo |
| `anthropic` | `{"type": "error", "error": {"type", "message"}, "request_id"}` | **529** `overloaded_error` | 429 sem `retry-after` (spend cap); 400 (spend limit do cliente) | `event: error` |
| `opencode` | `{"error": {"type": "server_error", "message"}}` (upstream) ou `{"type": "error", "error": {"type", "message"}, "metadata"}` (gateway) | 5xx do upstream | 429 `GoUsageLimitError` (cota Go), `BudgetLimitError` (budget) e `FreeUsageLimitError` (gratuitos); 402 sem saldo | Não documentado |
| `openrouter` | `{"error": {"code", "message", "metadata"}}`, `metadata.error_type` | 503 `provider_overloaded`, 502 `provider_unavailable` | 402 (crédito), 403 (budgets) | 200 + `data:` com `error` e `finish_reason: error` |
| `gemini` | `{"error": {"code", "message", "status", "details"}}` (`google.rpc.Status`) | 503 `UNAVAILABLE` | 429 `RESOURCE_EXHAUSTED` | — |
| `azure` | `{"error": {"code", "message"}}`; `{"error": {"message", "type", "param", "code"}}` no que o backend do modelo recusa; `innererror` no `content_filter` | 503 `ServiceUnavailable` | 429 com `retry-after-ms`; 404 `DeploymentNotFound` | Como a OpenAI |

Outros comportamentos fiéis, com textos e status completos na spec 006:

- **Chave desconhecida**: 401 no dialeto do driver (`invalid_api_key` na OpenAI, `authentication_error` na Anthropic; no Gemini, 400 `API_KEY_INVALID` e 403 sem chave). Bloqueio de entidade pela API admin vira 403 `account_suspended`, sem `retry-after`, como os reais (no `opencode`, `This workspace has been suspended`).
- **Recusa de conteúdo**: 200 com `finish_reason: content_filter` (OpenAI), `stop_reason: refusal` (Anthropic).
- **Raciocínio** ([spec 007](../specs/007-geracao-respostas.md), [ADR 0022](../specs/decisions/0022-raciocinio-fiel-ao-provedor.md)): oculto no `openai` (só `reasoning_tokens` no `usage`); bloco `thinking` com `signature` no `anthropic`; `delta.reasoning_content` no `opencode`; `delta.reasoning` e `reasoning_details[]` no `openrouter`; partes `thought: true` (só com `includeThoughts`) e `thoughtsTokenCount` no `gemini`.
- **Ids de modelo**: no OpenRouter o `model` é o slug `autor/modelo` (`white-wizard/<id>` para ids sem `/`); no Azure o deployment da rota é o `id` ou um alias e a resposta traz o `id`; no Gemini a listagem usa `name: "models/<id>"`.
- **Cache de prompt**: segue as regras reais de cada provedor (automático por prefixo na OpenAI, `cache_control` na Anthropic, as do upstream no OpenRouter e no OpenCode), com os campos de `usage` de cada dialeto ([spec 007](../specs/007-geracao-respostas.md), [ADR 0046](../specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)).

## Anthropic: assinatura do Claude Code e modo estrito

Driver `anthropic` com as opções da [ADR 0074](../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md) (spec 006, 16.2.1, e [spec 020](../specs/020-cliente-claude-code.md)). Guia completo, com a receita para usar o Claude Code real, em [claude-code.md](claude-code.md); exemplo pronto em [`examples/claude-code-assinatura`](../examples/claude-code-assinatura/README.md); campos em [configuracao.md](configuracao.md#drivers).

- **Assinatura OAuth** (`oauth_key`, `oauth_plan`): tokens `sk-ant-oat01-…` (access) e `sk-ant-ort01-…` (refresh) sem estado, assinados com o valor da chave `oauth_key`, que eles representam (conta, limites, cota, bloqueio). Rotas no mount: `/oauth/authorize`, `/v1/oauth/token`, `/v1/oauth/token/revoke`, `/api/oauth/profile`, `/api/oauth/usage`, `/api/claude_cli/bootstrap` e as demais de conta. Token inválido dá o 401 `OAuth access token is invalid.` conferido no real; vencido, sem o beta `oauth-2025-04-20` ou sem `user:inference`, os textos da spec 006 (não conferidos). `oauth_plan` só muda `organization_type` e `rate_limit_tier` do perfil.
- **Cota de assinatura**: a `quota_usd` da conta (5 h, semana). Com token OAuth, a Messages manda `anthropic-ratelimit-unified-{status,5h-utilization,5h-reset,7d-utilization,7d-reset,representative-claim,reset,overage-status,overage-disabled-reason}` no lugar dos `anthropic-ratelimit-requests-*`; cota estourada é 429 `rate_limit_error` com `status: rejected`, que o Claude Code assinante não retenta.
- **Modo estrito** (`client_verification: claude-code`): em `POST /v1/messages` e `/v1/messages/count_tokens` do mount, antes da credencial e dos limites, a requisição tem de ser byte a byte a do Claude Code 2.1.308 (linha com `?beta=true`, headers na ordem e caixa exatas, betas, corpo de `JSON.stringify`, billing header com fingerprint). O resto recebe 403 `forged_request_error` com `x-wwz-client-verification: failed; reason=<reason>` (15 códigos), sem débito nem contagem em limite, e incrementa `wwz_client_verification_failures_total{driver,reason}`. O Claude Code real em uso normal passa (15 modos, `tests/claudecode/default-mode.sh`); os SDKs oficiais saem com `reason=query`. Não funciona atrás de proxy que reescreve headers (`header_order`).
- **Rotas sempre presentes**: `GET|HEAD /api/hello` (`{"message": "hello"}`) e `GET /v1/oauth/hello` (HEAD, POST e OPTIONS 405), iguais ao real, com ou sem `oauth_key`, fora do modo estrito.

## OpenCode

Driver `opencode` ([spec 006](../specs/006-fidelidade-provedores.md), 16.3; [ADR 0072](../specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md) e [ADR 0073](../specs/decisions/0073-origem-das-urls-absolutas-do-wwz.md)). Foi conferido contra o OpenCode Go em 31/10/2026 e, sem chave e sem custo, contra os catálogos públicos e o modo `public` em 03/11/2026. As fixtures ficam em `testdata/real/opencode/`, com `catalog/` e `free/`. O [`examples/opencode-completo`](../examples/opencode-completo/README.md) monta tudo o que esta seção descreve, com os catálogos reais. Os campos do YAML estão em [configuracao.md](configuracao.md#opencode).

### Bases e catálogos

| Base | Catálogo | Origem da cobrança | No real (03/11/2026) |
| --- | --- | --- | --- |
| `/inference`, `/zen` | `opencode` (pay-as-you-go e gratuitos) | `balance` ou `free` | 79 modelos, 9 gratuitos |
| `/inference/go`, `/zen/go` | `opencode-go` | `go`/`go-plus` ou `free` | 28 modelos, 2 gratuitos (`beam-free`, `iceman-contributor-free`) |

- **Rotas públicas.** `GET <base>/v1/models` responde sem chave, como o real; chave inválida segue 401 `server_error`/`Invalid credential`. `GET <base>/v1/models/{id}` é 404 sem corpo com qualquer chave. `/zen/v1/models` é igual a `/inference/v1/models`, e `/zen/go/v1/models` igual a `/inference/go/v1/models`, fora `created`.
- **Catálogo do modelo.** Sem `models[].catalogs`, o modelo está nos dois catálogos com o mesmo preço, como antes. Com o campo, ele só existe nos catálogos listados. Fora do catálogo da base, o modelo some da listagem e responde 400 `Model is unavailable.` no mesmo passo de um id desconhecido, inclusive na rota Responses.
- **Modelo efetivo por catálogo.** Em cada catálogo, `catalogs.<c>` sobrepõe o modelo: `pricing` (o bloco inteiro), `allowance_factor` (só em `opencode-go`), `context_window`, `max_output`, `capabilities` (campo a campo) e `extra` (mesclado; `null` tira o campo). O resultado vale para tudo o que o wwz faz naquela base: listagem, 400 `context_length_exceeded`, teto de saída, recusas por capacidade e preço. `prompt_cache`, `tokenizer`, `latency`, `errors`, `generation`, `scenarios`, `aliases` e `protocols` não variam por catálogo.

### Listagem de modelos

`GET <base>/v1/models` sai na forma e na ordem dos catálogos públicos do real. O `examples/opencode-completo` bate byte a byte com `testdata/real/opencode/catalog/`, fora `created`.

- **Preço.** É o do catálogo da base. `cost.multipliers` e `cost.text.{input,output}.overrides` saem de `pricing.multipliers` e `pricing.overrides`, com o `when` na ordem real. Os gratuitos levam `cache.read: 0`.
- **`api`.** Traz exatamente um item, o protocolo nativo do modelo. Por padrão é o primeiro dialeto de `protocols`: `openai` → `chat-completions`, `anthropic` → `messages`, `gemini` → `generate-content`; ausente ou `*` → `chat-completions`. Quando `extra.api` é declarado, as chaves não nulas dele são o protocolo. É assim que se declara `responses` ou um protocolo diferente num catálogo, por exemplo `catalogs.opencode-go.extra.api: {messages: {}}`. Cada item tem `base_url` e o `sdk` real do protocolo.
- **`base_url`.** É absoluto e fica sempre na família `/inference` do catálogo, mesmo quando a listagem vem de `/zen` ou `/zen/go`. Sai `<origem>/inference/openai/v1` (ou `/anthropic/v1`, `/google/v1beta`) no `opencode` e `<origem>/inference/go/...` no `opencode-go`. O real usa `https://opencode.ai/inference/...`; o wwz põe a própria origem, para o cliente que segue a URL voltar ao wwz.
- **`canonical_id`.** Sem `extra.canonical_id`, o wwz deriva `<autor>/<id>`. `extra.canonical_id: null`, como qualquer chave de `extra` com `null`, tira o campo do item, como o real faz em `big-pickle`.

### Origem das URLs absolutas

A origem do wwz ([ADR 0073](../specs/decisions/0073-origem-das-urls-absolutas-do-wwz.md), spec 004) é `<esquema>://<host><prefixo externo><prefixo de montagem>`. O servidor a calcula uma vez por requisição e o driver a lê por `driver.Origin`.

| Parte | Vem de |
| --- | --- |
| Esquema | `X-Forwarded-Proto` (`https` ou `http`) com o par TCP em `server.trusted_proxies`; senão `https` quando a conexão chegou por TLS (`server.tls`) e `http` nos demais casos |
| Host | `X-Forwarded-Host` com o par confiável; senão `Host`. Sem nenhum dos dois, a URL sai relativa à raiz |
| Prefixo externo | `X-Forwarded-Prefix` com o par confiável (só se começar com `/`, sem a `/` final); senão vazio |
| Prefixo de montagem | `drivers[].prefix` (vazio na raiz) |

- Os `X-Forwarded-*` só valem vindos de `server.trusted_proxies` (ou `TRUSTED_PROXIES`), a mesma regra do `X-Forwarded-For`. Vindos de outro cliente, são ignorados. `Forwarded` (RFC 7239) não é lido.
- Ex.: com `prefix: /opencode` atrás do Traefik com TLS, a listagem do Go traz `https://wwz.exemplo/opencode/inference/go/openai/v1`.
- Valem a regra o `base_url` da listagem e o `verification_uri_complete` do Console. Links de documentação dentro dos textos de erro reais (`https://opencode.ai/go` no `FreeUsageLimitError`) continuam os do real.

### Cobrança por base

Cada requisição tem uma origem de cobrança, que o driver decide e que sai em `usage.opencode.billing_source` e no campo `billing_source` do evento `request`.

| Origem | Quando | O que é debitado |
| --- | --- | --- |
| `free` | Modelo gratuito, em qualquer base | Nada. `cost: 0`; nas bases pay-as-you-go, só a cota diária de gratuitos conta |
| `balance` | Base pay-as-you-go com modelo pago; base Go com service account ou com Extra Usage depois da cota | O saldo (`credits_usd`), `spend_*` e os budgets. `quota_usd` não vale |
| `go`, `go-plus` | Base Go, conforme o plano (`go` também no `go-teams`) | Só a cota Go, pelo `allowance`. `cost: 0`; saldo, `spend` e budgets não andam |

- `allowance = list_cost × allowance_factor`. Sem `catalogs.opencode-go.allowance_factor`, vale 2 nos modelos `gemini-*` e 0,6 nos demais, a razão conferida em dois modelos. O evento `request` leva `allowance_usd` quando a cota é debitada.
- Os limites de contagem (`rpm`, `tpm`, concorrência...) valem em todas as bases.
- **Mudança de comportamento.** Antes, a cota Go caía pelo preço de lista e toda requisição debitava saldo e `spend` em qualquer base. Agora, na base Go, a cota cai pelo `allowance` (0,6× a lista, 2× nos `gemini-*`) e o saldo e o `spend` não andam; na base pay-as-you-go, `quota_usd` não vale. O formato do YAML não muda. Para manter os números antigos da cota, use `allowance_factor: 1`, como o `examples/cota-5h-semana`. Declare também `opencode: {}` se o modelo deve continuar no pay-as-you-go, porque com `catalogs` o modelo só existe nos catálogos listados:

```yaml
models:
  - id: meu-modelo
    drivers: [opencode]
    pricing: {input_per_m: 1, output_per_m: 4}
    catalogs:
      opencode: {}                        # continua em /inference e /zen
      opencode-go: {allowance_factor: 1}  # cota Go pelo preço de lista
```

### Modo `public` e modelos gratuitos

Um modelo é **gratuito** num catálogo quando o `pricing` efetivo dele ali está declarado e tem `input_per_m: 0`. Sem `pricing`, o modelo continua pago a US$ 0.

A credencial literal `public` é o uso sem conta do real: `Authorization: Bearer public`, `x-api-key: public` na rota Messages ou `x-goog-api-key: public` na rota Google. Ela nunca é procurada em `keys[]` (`keys[].key: public` torna a configuração inválida) e vale também com `server.auth: none`. Sem conta, a requisição só tem as entidades `ip` e `user`. O `x-opencode-session` continua obrigatório. Para o real, `public` é credencial ausente, que só os gratuitos das bases pay-as-you-go aceitam:

| Situação | Resposta |
| --- | --- |
| Base pay-as-you-go, modelo gratuito, `User-Agent: opencode/...` | 200 com `billing_source: free`; conta na cota diária |
| Modelo gratuito sem `User-Agent` começando em `opencode/`, com chave ou sem, em qualquer base em que a credencial passe | 429 `FreeUsageLimitError` (cota zero), com `retry-after` até 00:00 UTC; nada é contado no store |
| Base pay-as-you-go, modelo pago, com `public` ou sem credencial | 401 `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}` |
| Base Go, qualquer modelo, gratuito inclusive | 401 `{"error":{"type":"server_error","message":"Upstream request failed: Go requires a credential"}}` (na rota Messages, a tradução `api_error`) |
| `GET <base>/v1/usage` | 401 `{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}` |
| Cota diária esgotada | 429 `FreeUsageLimitError`, com `retry-after` até 00:00 UTC |

- O 429 é sempre `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`, no envelope do gateway, sem `metadata`, inclusive na rota Messages, com `x-opencode-log-id`. Não existe 403 de cliente: o real respondeu 429 a um cliente genérico, nunca o 403 que a documentação sugeria.
- **Cota diária.** É `drivers[].options.free_requests_per_day` (inteiro ≥ 1, padrão 200). O real não publica o número; o 200 é escolha do wwz. A unidade é o IP do cliente (`ip:<endereço>`, com `trusted_proxies`) sem chave e a conta com chave, na entidade `free_models`. Só conta nas bases pay-as-you-go: no Go, os gratuitos aparecem como ilimitados. O dia vira às 00:00 UTC.
- **Ordem das checagens:** sessão → corpo → modelo → credencial → cliente → cota diária.
- No wwz, o roteador sob teste pode mandar `User-Agent: opencode/...` à vontade. Nas conferências, esse header nunca é mandado ao real: isso contornaria um controle de acesso do provedor.
- `x-api-key` vale na rota Messages para qualquer chave, como o cliente oficial manda. `x-opencode-session-id`, `x-opencode-parent-session-id`, `x-opencode-project` e `x-opencode-client` são aceitos e ignorados; `x-opencode-session` continua sendo a entidade `user`.

### Planos, membros, service accounts e Extra Usage

- **Plano.** O plano é do tier: `tiers.<n>.plan` vale `go` (padrão), `go-plus`, `go-teams` ou `none`. As cotas de cada plano vão no `quota_usd` do tier. `billing_source` sai `go-plus` no `go-plus` e `go` no `go` e no `go-teams`.
- **Sem Go.** `plan: none` é um workspace sem Go. Nas bases Go e no `/v1/usage`, a resposta é 403 `{"type":"error","error":{"type":"ForbiddenError","message":"This workspace does not have an active OpenCode Go subscription"}}`. O `api/v2/config` do Console não lista `opencode-go`.
- **Bloqueio.** Uma conta suspensa (`account_suspended`, manual ou pela escada de abuso) recebe 403 `{"error":{"type":"permission_error","message":"This workspace has been suspended"}}`. Antes, o bloqueio usava o texto da falta de Go.
- **Membro.** O membro do workspace é a entidade `project` (`keys[].project`; uma chave sem projeto é o próprio membro). No Go Teams, a cota Go, a da conta inteira e a por modelo, conta no membro.
- **Service account.** `keys[].service_account: true`. Ela nunca é coberta pela cota Go, em plano nenhum: na base Go é cobrada como `balance`, e o Console responde 403 nos budgets dela.
- **Extra Usage.** `accounts[].extra_usage` (padrão `true` no `go-teams` e `false` nos demais). Com a cota Go estourada e o Extra Usage ligado, a requisição é atendida como `balance`; saldo zerado dá 402 `Upstream request failed: Insufficient account funds`. Com ele desligado, 429 `GoUsageLimitError`, com `metadata.workspace` = `wrk_` + 26 caracteres base32 do sha256 do id da conta.

### Cota Go e `GET <base>/v1/usage`

- **Cota** (`limits.quota_usd: {monthly_usd, rolling_5h_pct, weekly_pct, models}`). A janela de 5 h é deslizante; a semanal vira na segunda-feira 00:00 UTC e a mensal no dia 1º (UTC). Estourou uma janela: 429 `GoUsageLimitError` com `retry-after` até ela liberar, isto é, 5 h depois do gasto mais antigo na janela de 5 h, a próxima segunda 00:00 UTC na semanal e o próximo dia 1º na mensal.
- **Cota por modelo.** `quota_usd.models: {<id>: <US$ por mês>}` dá a cada modelo listado uma cota própria, com os mesmos `rolling_5h_pct` e `weekly_pct`, na entidade `model_quota` (`<dono>/<modelo>`; o dono é a conta, ou o membro no Go Teams). `monthly_usd` continua sendo a cota da conta inteira, e as duas valem juntas.
- **Uso da cota** (com chave): `{"usage":{"rolling":W,"weekly":W,"monthly":W}}`, cada `W = {status, percent, resetsAt}`.
  - `percent` é o débito da cota na janela sobre o limite dela, arredondado e limitado a 100: `rolling` = últimas 5 h sobre `rolling_5h_pct` da cota mensal, `weekly` = desde segunda 00:00 UTC sobre `weekly_pct`, `monthly` = mês UTC sobre `monthly_usd`. Conta sem cota lê 0.
  - Com cotas por modelo, cada janela mostra a maior porcentagem entre a cota inteira e as por modelo; no Go Teams, as do membro da chave.
  - `status` é `"rate-limited"` quando a janela nega, e `"ok"` nos demais casos.
  - `resetsAt`: o gasto mais antigo da janela de 5 h + 5 h (sem gasto, 5 h à frente), a próxima segunda 00:00 UTC e o próximo dia 1º.
  - Sem chave, com chave inválida ou com `public`: 401 `AuthError` `Unauthorized`. Com `plan: none`, 403.

### Budgets

- `spend_monthly_usd`, que já existia, é o budget de cobertura `all`. Na conta vale para o workspace, no projeto para o membro e na chave para a service account ou a chave.
- `spend_monthly_models_usd: {<id> | opencode/<id> | opencode-go/<id>: <US$>}`, em todo nível de `limits`, é o budget de cobertura `models`, contado na entidade `model_spend`. `opencode/` limita às bases pay-as-you-go, `opencode-go/` às bases Go, e o id puro vale nas duas.
- Só contam dólares debitados do saldo: gratuito e uso dentro da cota Go nunca contam. Os budgets empilham: qualquer um estourado nega.
- Um budget só nega depois que o gasto passa do limite. A chamada que cruza o limite passa, e a seguinte recebe 429 `BudgetLimitError` com `metadata.coverage` (`all` ou `models`) e `metadata.resetsAt` no dia 1º do mês seguinte, 00:00 UTC.
- Nos outros drivers, `spend_monthly_models_usd` vale como condição do operador, com o erro `spend_limit` do dialeto.

### Preço por horário e por contexto

`pricing.multipliers` (`[{when, factor}]`) e `pricing.overrides` (`[{when, <preços>}]`) usam a gramática `when` do catálogo real: `utc_days`, `utc_hours`, `date`, `context` (só em `overrides`), `any` e `all`. O primeiro override que casa substitui os preços que declara, e todos os multiplicadores que casam se multiplicam, avaliados no início da requisição. O preço efetivo vale para o custo de todo driver (débito, `usage.cost`, `list_cost`, `spend`, cota). No `opencode`, a listagem o mostra na forma real.

### Console

O Console fica em `<prefixo do driver>/console/...`, na raiz do mount, não sob `/inference` nem `/zen`. O real é `https://opencode.ai/console`. As formas estão na [spec 004](../specs/004-apis-e-endpoints.md).

| Rota | O que faz |
| --- | --- |
| `POST /console/auth/device/code` | Começa o device flow: `{device_code, user_code, verification_uri_complete, expires_in: 900, interval: 5}`; `verification_uri_complete` = `<origem>/console/device?code=<user_code>` |
| `POST /console/auth/device/token` | Poll do device code e refresh (JSON ou formulário) |
| `GET /console/api/user`, `/console/api/orgs` | Usuário (`usr_…`) e workspace (`wrk_…`, o mesmo de `metadata.workspace`) |
| `GET /console/api/v2/config` | `{"providers": {"opencode": {}, "opencode-go": {}}}`, sem `opencode-go` no plano `none` |
| `GET /console/api/policies/current-budgets` | Budgets que valem para a chave (escopo `workspace`, `member` ou `key`; cobertura `all` ou `models`); 403 `{"_tag":"Forbidden"}` para service account |

- **`drivers[].options.console_key`** é o label de uma entrada de `keys[]`. É a conta que o login do Console aprova, e o valor dessa chave é o segredo HMAC-SHA256 que assina `device_code`, `access_token` e `refresh_token`. Não há estado no store, então os tokens valem em toda instância. Só quem conhece a chave forja um token.
- **Tempos** (escolhidos pelo wwz): poll antes de 5 s (`interval`) → 400 `slow_down`; até 10 s (`2 × interval`) → 400 `authorization_pending`; depois, os tokens. O `device_code` vence em 900 s (`expired_token`); o access token em 1 h e o refresh em 30 dias. Refresh vencido, adulterado ou de outro segredo → 400 `invalid_grant`. Sem `console_key`, o fluxo nunca aprova e responde `expired_token` depois de 900 s.
- **Validade.** Os tokens morrem quando o valor da chave `console_key` muda, ou quando a opção sai. O access token vale como a chave `console_key` na inferência (bearer em todas as rotas, Messages inclusive) e no Console, então uma chave suspensa ou banida bloqueia também os tokens dela. Chaves de API também valem no Console; `x-org-id` é aceito e ignorado.
- Credencial ausente ou inválida nas rotas `api/*` → 401 `{"_tag":"Unauthorized"}`. Método errado ou caminho desconhecido sob `/console` → a página HTML 404 do site.

### Fora do escopo

Cada item volta quando o gatilho acontecer (ADR 0072, item 2):

| Item | Gatilho |
| --- | --- |
| SystemOne e modelos `decision` (`/inference/systemone/v1`, `jev-*`) | Fixture ou documentação do protocolo |
| GPT-Live (voz) e o `x-opencode-org-id` da voz | Realtime entrar no MVP ([ADR 0054](../specs/decisions/0054-escopo-de-rotas-do-mvp.md)) |
| `POST /console/api/websearch` | Fixture da resposta |
| Library (`/api/v2/library/skills/{slug}`) e SSO (`/auth/sso/...`, `403 SsoRequired`) | Um roteador sob teste que dependa deles |
| Top-up, auto-recharge, invoice e BYOK dos Providers do Console | Pedido de simular faturamento por invoice ou BYOK (o saldo já é `credits_usd` + `add_usd`) |
| `OPENCODE_DISABLE_FREE_MODELS` | Nenhum: é variável do cliente, não do servidor |

### Não conferido

Conferidos sem custo: os dois catálogos públicos (79 e 28 modelos, `/zen` igual a `/inference`), `public` com cliente genérico (429 nos gratuitos, 401 `Missing API key.` no pago, 401 `Go requires a credential` na base Go, 401 no `/v1/usage`), a `x-api-key` inválida na rota Messages e o envelope, o texto e o reset do `FreeUsageLimitError`. Continuam "não conferido" ([spec 006, "Não conferidos no real"](../specs/006-fidelidade-provedores.md#não-conferidos-no-real)), porque exigem chave ou forjar o cliente:

- **Gratuitos:** a regra "cota zero fora do cliente OpenCode" (só a primeira chamada de um IP foi gravada); a unidade e o número da cota diária; o status e o texto da cota esgotada dentro do cliente OpenCode; o 401 da credencial de fato ausente e a rota Messages com `public` na base Go; o 429 na rota Google.
- **Planos e contas:** o 403 do plano `none`; o texto do bloqueio; o `billing_source` do Go Teams; a service account fora da cota em todos os planos (o real só documenta o Teams); o Extra Usage; o formato de `metadata.workspace`; as cotas por modelo do Go Plus (4× é estimativa) e do Go Teams.
- **Cota e uso:** a razão `allowance`/`list_cost` fora dos modelos medidos; a janela que o `/v1/usage` mostra com cotas por modelo; o `percent` de 5 h e semana com gasto e o arredondamento; o `resetsAt` de 5 h pelo primeiro gasto; o mês pelo dia do ciclo da assinatura (o wwz usa o mês UTC).
- **Erros:** `GoUsageLimitError`, `BudgetLimitError` (o texto alternativo da documentação, "Upstream request failed: Policy budget exceeded", fica de fora), `CreditLimitExceeded`, 402/403/413/5xx e erro no meio do stream.
- **Preço e catálogo:** a regra de combinação de `overrides` e `multipliers`; o que conta como "contexto"; o 400 de modelo fora do catálogo da base; a recusa pelo limite do catálogo, e não pelo maior dos dois.
- **Console:** todas as formas e os tempos.

## Gemini

Driver `gemini` ([ADR 0043](../specs/decisions/0043-drivers-gemini-e-azure.md), spec 006 seção 16.5), conferido contra a Gemini API em 31/10/2026 com 72 fixtures em `testdata/real/gemini/shape/` comparadas byte a byte (`TestRealFixtures*` em `internal/driver/gemini`) e `wwz-drift check --providers gemini` sem divergência.

- **Rotas**: `models/{m}:generateContent`, `:streamGenerateContent` (SSE com `?alt=sse`, cada evento um `GenerateContentResponse` inteiro terminado em `\r\n\r\n`; sem `alt=sse`, array JSON entregue aos pedaços), `:countTokens`, `models` paginado (`pageSize` padrão 50, até 1000, `pageToken`/`nextPageToken`), `models/{m}` e `cachedContents` (criar, ler, listar, `PATCH` de `ttl`/`expireTime`, apagar). Método desconhecido e caminho fora das rotas sob `/v1beta/` respondem o 404 vazio `text/html` do real.
- **JSON**: `prettyPrint` do real (indentação de 2 espaços, `<` e `>` como `\u003c`/`\u003e`); no SSE, o mesmo JSON numa linha.
- **Ordem das checagens** (conferida): erros do transcodificador JSON (sintaxe, campo desconhecido, tipo errado, com `google.rpc.BadRequest`) vêm antes da chave; Bearer OAuth é 401 antes de tudo; chave inválida 400 `API_KEY_INVALID`, sem chave 403 `PERMISSION_DENIED`; depois as checagens semânticas, o modelo (404 com o método da rota) e as do backend do modelo (papel inválido, turno final do modelo, `cachedContent`), que levam `x-gemini-service-tier`.
- **Resposta**: `thoughtSignature` na primeira `functionCall` ou na última parte; chamada de função termina em `STOP` com `finishMessage`; raciocínio só em partes `thought: true` com `includeThoughts`, sempre contado em `thoughtsTokenCount`; `modelVersion` é o `id`; `responseId` com o instante nos 4 primeiros bytes.
- **Cache explícito**: `cachedContents` com mínimo de 1 024 tokens (o real do `gemini-3.5-flash-lite`; `models[].prompt_cache.min_tokens` troca) e TTL padrão de 1 h; a geração que o usa traz `cachedContentTokenCount` e `cacheTokensDetails`. As entradas (com o conteúdo, que a geração com `cachedContent` usa) ficam no store e valem em todas as instâncias atrás de um balanceador (chaves `P:gcc:*`, `cache/valkey/README.md` 2.6). Rota desconhecida dá 404 vazio antes da credencial, `candidateCount` fora de 1–8 é 400 e acima de 1 nenhum modelo aceita; `tokenizer: auto` conta com o `gtok` calibrado (erro médio 4,9%, ainda "não conferido" texto a texto).
- **Não conferido** (documentação ou relato público): 429 `RESOURCE_EXHAUSTED` com `QuotaFailure`/`RetryInfo`, 500/503/504, chave expirada, conta suspensa, geobloqueio, erro no meio do stream, cache implícito (nenhum acerto observado em 3 chamadas seguidas) e o tokenizador (o real conta 7 tokens na sonda; o simulado, 12).

## Azure

Driver `azure` ([ADR 0043](../specs/decisions/0043-drivers-gemini-e-azure.md), spec 006 seção 16.6). Segue só a documentação oficial e relatos públicos (exceção à ADR 0014): tudo fica "não conferido" até existir chave.

- **Rotas**: `/openai/deployments/{deployment}/chat/completions` e `/completions` exigem `api-version` de uma versão GA ou preview documentada (de `2022-12-01` a `2025-04-01-preview`); sem ela, ou com outra operação (`/embeddings`, fora do MVP), 404 `{"error":{"code":"404","message":"Resource not found"}}`. Na API v1 (`/openai/v1/chat/completions`, `/openai/v1/responses`, `/openai/v1/models`) o `api-version` é opcional (`v1` ou `preview`) e o deployment vai em `model`. `/openai/models` lista os modelos no formato Azure (`status`, `capabilities`, `lifecycle_status`, `deprecation` com `models[].retires_at`) e, como as rotas por deployment, exige `api-version` conhecido (depois da chave); `/openai/v1/models` aceita só `v1`, `preview` ou nenhum.
- **Deployment**: o `id` ou um alias de `models[]`; nas rotas por deployment, o `model` do corpo é ignorado. A resposta traz em `model` o `version` (ou o `id`), nunca o nome do deployment. Deployment desconhecido: 404 `DeploymentNotFound`.
- **Corpo**: o Chat Completions da OpenAI, compacto, com `system_fingerprint` e sem `service_tier`, `annotations` ou `obfuscation`; `prompt_filter_results` antes de `choices` e `content_filter_results` em cada choice (as quatro categorias e `protected_material_*` quando há texto; `{}` em tool calls). Raciocínio oculto, como na OpenAI. O stream começa com o evento `{"id":"","object":"","created":0,"model":"","prompt_filter_results":[...],"choices":[],"usage":null}`. O filtro nunca bloqueia por conta própria: o erro `content_filter` (400 com `innererror.code: ResponsibleAIPolicyViolation`) vem do caos.
- **Headers**: `apim-request-id` (UUID) em toda resposta; `x-ratelimit-*` quando a chave tem limite; `retry-after-ms` no 429 (sem `retry-after`, que é do PTU); `retry-after` no 503.
- **Validação**: campo de topo desconhecido → 400 `Unrecognized request argument supplied: <campo>`; `strict: true` em tools ou `response_format` com schema fora das regras → 400 `invalid_function_parameters`/`invalid_json_schema`.
- **Responses** (`/openai/v1/responses`): o motor do driver `openai` com a mesma validação (mesma ordem e textos; deployment desconhecido → 404 `DeploymentNotFound`; `api-version` fora de `v1`/`preview` → 404 antes do corpo) e `content_filters` no objeto final (corpo e evento `response.completed`): uma entrada `prompt` sempre e uma `completion` quando a saída tem texto, `{source_type, blocked, content_filter_results}`.
- **Limites**: cada deployment (`<conta>/<modelo>`) recebe os limites de contagem do tier da conta; com TPM, o RPM é derivado dele (6 por 1 000 TPM nos modelos de chat; 1 por 1 000 em o3/o4-mini; 1 por 6 000 no o1; 1 por 10 000 em o3-mini/o1-mini/o3-pro), e as requisições são avaliadas em 1 s (RPM ≥ 60; 600 RPM = 10 por segundo) ou 10 s. O `rps` do tier não existe no Azure. Gasto, créditos e cota em dólar ficam na conta; `keys[].limits` e `projects[].limits` seguem como tetos próprios.
- **Pendências**: `/openai/responses?api-version=` da API clássica; janelas curtas também no TPM (a doc só as descreve no RPM); `content_filter_offsets`.

## Criar um driver

Copiar `internal/driver/openai`, implementar a interface `driver.Driver`, registrar com `driver.Register("<nome>", New)` em `init()`, adicionar o dialeto de erro e rodar a suíte de contrato `drivertest.Run(t, New())`, que é a mesma para todos os drivers ([spec 005](../specs/005-drivers.md)). Um driver novo não altera o núcleo: `internal/server`, `internal/config`, `internal/engine` e `internal/core` não importam driver concreto (teste `TestCoreImportsNoConcreteDriver`) e os arquivos não-teste de `internal/engine` não citam nome de driver (`TestEngineNamesNoDriver`). Três interfaces opcionais levam ao núcleo o que é do provedor:

- `driver.RequestValidator` (`ValidateRequest(kind, body, headers, models)`): a validação do corpo como o real recusa, na ordem do real. O servidor a roda dentro de `ParseRequest`; `models` diz se um modelo é servido (para quem confere o modelo antes do corpo); `driver.RawError` responde byte a byte quando o envelope do driver não serve (gateways). Driver sem ela não valida. A validação no dialeto da OpenAI fica em `internal/driver/oai/oaicheck` (usada por `openai` e `azure`) e o leitor de JSON em ordem de documento em `internal/driver/jsonscan`.
- `driver.Declarer` (`Declaration() config.DriverDecl`): dialetos do catálogo, se serve todo modelo em toda rota, se o nome já valeu em `models[].protocols`, se aceita `errors.tool_misbehavior`, posição entre os drivers montados por padrão, `drivers[].options` com os valores aceitos e as chaves de `models[].extra`. `driver.Register` publica a declaração em `internal/config`, que valida a configuração sem conhecer driver por nome. Os testes de `internal/config` leem a cópia `internal/config/testdata/driver_decls.json`, conferida com os drivers por `TestDeclarationsMatchConfigFixture` (`WWZ_UPDATE_GOLDEN=1` regrava).
- `driver.EngineDeclarer` (`EngineDeclaration() driver.EngineDecl`): o que o motor aplica em volta do driver. `Text` é o gerador (e tokenizador) dos modelos em `tokenizer: auto` sem BPE próprio e `Template` o template de prompt do dialeto (`driver.PromptTemplate`: contagem e custo de declaração, chamada, resposta, turno e mídia, que o cache de prompt usa bloco a bloco); o `gemini` declara os de `internal/driver/gemini/gtok`. `ClaudeTokens` liga a contagem do Claude 4.7+; `PromptCache` diz o estilo do cache por modelo e endpoint (`driver.CacheAutomatic`, `CacheBreakpoints`, `CacheImplicit` ou `CacheNone`; o motor guarda mínimos e TTLs de cada estilo, e `models[].prompt_cache` sobrepõe); `ContentFilterAsFinish` faz o `content_filter` do caos virar 200 filtrado (`openai` e `anthropic`); `MalformedFrame` é o frame SSE cortado da falha `malformed` (padrão: chunk de chat). Driver sem ela tem o template de chat, o tokenizador simulado, nenhum cache e o `content_filter` como erro do próprio driver; um wrapper que só mantém `Name()` recebe a declaração do driver registrado com esse nome (`driver.EngineDeclOf`).

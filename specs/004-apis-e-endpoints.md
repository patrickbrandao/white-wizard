# 004 — APIs e endpoints de inferência

Status: rascunho
Depende de: 000, 001, 003
Origem: seções 3 do documento Claude Docs congelado em 2026-10-30

## Contexto
Rotas OpenAI, Anthropic e comuns imitadas pelo simulador, autenticação e cabeçalhos (seção 3).

## Requisitos
- R2, R3, R4, R22

## Interfaces e contratos
- Contratos oficiais dos provedores (não copiados para openapi.yaml, ver ADR 0006)

## Dados
- Nenhum

## Critérios de aceite
- [ ] SDKs oficiais `openai` (inclusive `AzureOpenAI`), `anthropic` e `google-genai` (Python e Node) funcionam apontando a URL base para o wwz
- [ ] Sem nenhuma opção, campo de topo desconhecido devolve 400 com o status, o envelope e o texto do provedor real em cada driver da tabela de validação, e passa onde o real deixa passar
- [ ] Sem nenhuma opção, tool `strict: true` sem `additionalProperties: false` devolve 400 `invalid_function_parameters` nos drivers `openai` e `azure`
- [ ] `POST /v1/responses` (driver `openai`) reproduz byte a byte, com ids e tempos normalizados, as fixtures de `testdata/real/openai/responses/` (corpo, ordem dos campos, sequência de eventos SSE, erros), com e sem streaming
- [ ] `previous_response_id` de resposta gravada soma a conversa anterior a `input_tokens` (fixture `previous_response_id`: 27) inclusive entre instâncias com `store: valkey`; id inexistente ou de resposta com `store: false` → 400 `previous_response_not_found`
- [ ] SDKs `openai` Python e Node: `responses.create` com e sem `stream`, com tool `function` e encadeado por `previous_response_id`
- [ ] Cada recusa da tabela de validação foi conferida contra o provedor real (fixture em `testdata/real/<provedor>/`) ou está marcada "não conferido" na spec 006, seção "Não conferidos no real" (ADR 0042)
- [ ] `anthropic` ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)): cada fixture de `testdata/real/claude-code/2.1.308/` enviada crua por TCP recebe 200 sem opção nenhuma; `GET|HEAD /api/hello` como o real; as rotas OAuth completam login e refresh; com `client_verification: claude-code`, as fixtures continuam 200 e as mutações da spec 020 recebem 403 `forged_request_error`
- [ ] `opencode` ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)): credencial `public` atende modelo gratuito nas bases pay-as-you-go com `User-Agent: opencode/…`, e responde como o real (fixtures `testdata/real/opencode/free/`) sem esse `User-Agent` (429 `FreeUsageLimitError`), com modelo pago (401 `AuthError` `Missing API key.`) e na base Go (401 `Go requires a credential`); as rotas do Console sob `<prefixo>/console` completam o device flow (`slow_down` → `authorization_pending` → tokens), o refresh e `invalid_grant`, e o access token emitido autentica a inferência como a chave `console_key`

## Fora de escopo
- Embeddings, imagens, áudio, fine-tuning, batches, arquivos e as rotas de recurso da Responses ([ADR 0054](decisions/0054-escopo-de-rotas-do-mvp.md))

## Decisões relacionadas
- [specs/decisions/0006-contratos-openapi.md](decisions/0006-contratos-openapi.md)
- [specs/decisions/0029-escopo-do-server-strict.md](decisions/0029-escopo-do-server-strict.md) (validação sempre ligada pela 0042)
- [specs/decisions/0042-fidelidade-total-ao-provedor-real.md](decisions/0042-fidelidade-total-ao-provedor-real.md)
- [specs/decisions/0043-drivers-gemini-e-azure.md](decisions/0043-drivers-gemini-e-azure.md)
- [specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)
- [specs/decisions/0054-escopo-de-rotas-do-mvp.md](decisions/0054-escopo-de-rotas-do-mvp.md)
- [specs/decisions/0055-contrato-da-responses-api.md](decisions/0055-contrato-da-responses-api.md)
- [specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)
- [specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### 3. APIs e endpoints suportados

O simulador expõe todos os dialetos no mesmo processo e porta; o formato da resposta é decidido pelo caminho da URL (e, na raiz sem prefixo, pela credencial, spec 005), nunca pelo nome do modelo. Cada rota se comporta como a do provedor real: o que o real aceita, recusa, devolve e cobra ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md)). As rotas de OpenRouter e OpenCode estão na spec 006.

**OpenAI**

| Endpoint | Método | Notas |
| --- | --- | --- |
| `/v1/chat/completions` | POST | Streaming SSE e não-streaming; `tools`, `tool_choice`, `parallel_tool_calls`, `response_format` (json\_object e json\_schema), `n`, `stream_options.include_usage`, `max_tokens`/`max_completion_tokens`, `reasoning_effort` |
| `/v1/models` | GET | Lista do YAML: `id`, `object: model`, `created`, `owned_by` |
| `/v1/models/{id}` | GET | 404 no formato OpenAI quando não declarado |
| `/v1/completions` | POST | Legado; `prompt` string ou lista, `echo`, `stream` |
| `/v1/responses` | POST | Responses API (seção "Responses API" abaixo, [ADR 0055](decisions/0055-contrato-da-responses-api.md)); ainda sem código (task de código da 0054) |

**Anthropic**

| Endpoint | Método | Notas |
| --- | --- | --- |
| `/v1/messages` | POST | Eventos `message_start`, `content_block_start/delta/stop`, `message_delta`, `message_stop`, `ping`, `error`; blocos `text`, `thinking` (com `signature`), `tool_use`; `tool_choice` (`auto`, `any`, `tool`, `none`); `thinking.budget_tokens` |
| `/v1/models` | GET | Lista do YAML no formato Anthropic: `id`, `display_name`, `created_at`, `type: model`, paginação `has_more`/`first_id`/`last_id` |
| `/v1/models/{id}` | GET | 404 no formato Anthropic |
| `/v1/messages/count_tokens` | POST | Contagem pelo tokenizador do modelo (`models[].tokenizer`, spec 007) |
| `/api/hello` | GET, HEAD | Sonda de conectividade do Claude Code (`HEAD` antes de cada sessão): 200 `{"message": "hello"}` sem credencial, como o real |
| Rotas OAuth e de conta | várias | Assinatura do Claude Code ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)): `/oauth/authorize`, `/v1/oauth/token`, `/api/oauth/profile` e as demais da spec 006, 16.2.1, com `drivers[].options.oauth_key` |

O Claude Code chama `POST /v1/messages?beta=true`: a query `beta` é aceita e ignorada em todas as rotas do `anthropic`, como no real. Com `client_verification: claude-code` (spec 020), as duas rotas de inferência recusam com 403 `forged_request_error` quem não segue o perfil do Claude Code, antes da credencial e do corpo.

**Gemini API** (driver `gemini`, [ADR 0043](decisions/0043-drivers-gemini-e-azure.md); detalhes na spec 006, 16.5)

| Endpoint | Método | Notas |
| --- | --- | --- |
| `/v1beta/models` e `/v1/models` | GET | `{"models": [...], "nextPageToken"}`; `pageSize` (padrão 50, máx. 1000) e `pageToken`; `nextPageToken` ausente na última página |
| `/v1beta/models/{model}` | GET | Recurso `Model` (`name: "models/<id>"`, `baseModelId`, `version`, `displayName`, `description`, `inputTokenLimit`, `outputTokenLimit`, `supportedGenerationMethods`, `thinking`, `temperature`, `maxTemperature`, `topP`, `topK`) |
| `/v1beta/models/{model}:generateContent` | POST | `contents`, `systemInstruction`, `tools[].functionDeclarations`, `toolConfig.functionCallingConfig`, `safetySettings`, `generationConfig` (inclui `thinkingConfig`), `cachedContent` |
| `/v1beta/models/{model}:streamGenerateContent` | POST | Com `?alt=sse`: SSE, um `GenerateContentResponse` completo por evento `data:`; sem `alt=sse`: array JSON entregue em pedaços |
| `/v1beta/models/{model}:countTokens` | POST | `{contents}` ou `{generateContentRequest}`; resposta `totalTokens`, `cachedContentTokenCount`, `promptTokensDetails`, `cacheTokensDetails` |
| `/v1beta/cachedContents` e `/v1beta/cachedContents/{id}` | POST/GET/PATCH/DELETE | Cache explícito (spec 007), nos modelos em que o real o oferece |

As mesmas rotas existem sob `/v1/`, como no real.

**Azure OpenAI** (driver `azure`, ADR 0043; detalhes na spec 006, 16.6)

| Endpoint | Método | Notas |
| --- | --- | --- |
| `/openai/deployments/{deployment}/chat/completions?api-version=<v>` | POST | Rota por deployment; `api-version` obrigatório (GA `2024-10-21`, preview `2025-04-01-preview` e as anteriores documentadas); corpo e stream do Chat Completions com `prompt_filter_results` e `content_filter_results` |
| `/openai/deployments/{deployment}/completions?api-version=<v>` | POST | Legado |
| `/openai/models?api-version=<v>` | GET | Listagem usada por `AzureOpenAI.models.list()`; exige `api-version` conhecido (o mesmo conjunto das rotas por deployment): sem ele, 404 `{"code":"404","message":"Resource not found"}` depois da chave (401 antes). `/openai/v1/models` aceita só `v1`, `preview` ou nenhum. Não conferido (ADR 0065) |
| `/openai/v1/chat/completions`, `/openai/v1/responses` | POST | API v1: `api-version` opcional (`v1`, `preview`); `model` é o nome do deployment |
| `/openai/v1/models` | GET | Listagem da API v1 |

**Comum**

| Endpoint | Método | Notas |
| --- | --- | --- |
| `/health` | GET | 200 sempre que o processo está vivo |
| `/health/ready` | GET | 503 durante `startup_duration` (simula carregamento de modelo), depois 200 |
| `/metrics` | GET | Prometheus |
| `/admin/config` | GET/POST | Lê e altera configuração em runtime |
| `/admin/stats` | GET | Contadores por chave, modelo e tipo de erro |
| `/admin/scenarios` | GET/POST/DELETE | Carrega e remove cenários sem reiniciar |

As rotas admin completas estão no contrato [cmd/wwz/openapi.yaml](../cmd/wwz/openapi.yaml) (spec 013).

**OpenCode: modo `public` e Console** ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md); limites e erros na spec 006, 16.3). As rotas de inferência do `opencode` estão na spec 006. Duas superfícies novas:
- **Modo `public`.** A credencial literal `public` (`Authorization: Bearer public`; `x-api-key: public` na rota Messages; `x-goog-api-key: public` na rota Google) é o uso sem conta do real. Para o real é credencial ausente: só os modelos gratuitos das bases pay-as-you-go (`/inference`, `/zen`) a aceitam, e só com `User-Agent` começando em `opencode/` são atendidos (fora disso, a cota de gratuitos vale zero: 429 `FreeUsageLimitError`). Modelo pago → 401 `AuthError` `Missing API key.`; base Go → 401 `Go requires a credential`; `/v1/usage` → 401 `AuthError` `Unauthorized` (conferidos; textos na spec 006, 16.3). Nunca é procurada em `keys[]`; vale também com `server.auth: none`.
- **Console**, sob `<prefixo do driver>/console` (o real é `https://opencode.ai/console`). Não há estado no store: `device_code` e tokens são assinados com a chave `drivers[].options.console_key` (spec 003). Formas e tempos não conferidos.

| Rota | Método | Resposta |
| --- | --- | --- |
| `/console/auth/device/code` | POST | Corpo `{client_id, supports_org_scope?}` → 200 `{device_code, user_code, verification_uri_complete, expires_in: 900, interval: 5}`; `user_code` `XXXX-XXXX`; `verification_uri_complete` é `<origem>/console/device?code=<user_code>`, com a origem do wwz (abaixo, "URLs do próprio wwz") |
| `/console/auth/device/token` | POST | `grant_type=urn:ietf:params:oauth:grant-type:device_code` com `device_code`: antes de `interval` → 400 `{"error":"slow_down"}`; até `2 × interval` → 400 `{"error":"authorization_pending"}`; depois → 200 `{access_token, refresh_token, token_type: "Bearer", expires_in: 3600, org_id}`; sem `console_key` ou depois de `expires_in` → 400 `{"error":"expired_token"}`. `grant_type=refresh_token` com `refresh_token` válido → 200 com tokens novos; vencido, adulterado ou de outro segredo → 400 `{"error":"invalid_grant"}`. Corpo JSON ou `application/x-www-form-urlencoded` |
| `/console/api/user` | GET | `{id: "usr_…", email: "<label>@wwz.invalid"}` da chave (ou do token) |
| `/console/api/orgs` | GET | `[{id: "wrk_…", name: "<conta>"}]` (o id é o `metadata.workspace` do `GoUsageLimitError`) |
| `/console/api/v2/config` | GET | `{"providers": {"opencode": {}, "opencode-go": {}}}`; sem `opencode-go` com plano `none` |
| `/console/api/policies/current-budgets` | GET | Lista dos budgets que valem para a chave: `{ruleID, limitDollars, spentDollars, scope (workspace, member ou key), per: "month", coverage (all ou models), models (["opencode/<id>"…]), vendors: [], picked, shared, resetsAt}`; service account → 403 `{"_tag":"Forbidden"}` |

As rotas `api/*` aceitam `Authorization: Bearer` com chave de `keys[]` ou com access token; `x-org-id` é aceito e ignorado. Credencial ausente ou inválida → 401 `{"_tag":"Unauthorized"}`. Método errado ou caminho desconhecido sob `/console` → a página HTML 404 do site, como as rotas inexistentes do `opencode` (spec 006). O access token autentica também a inferência (bearer em todas as rotas, inclusive Messages), valendo como a chave `console_key`.

**Fora do MVP** ([ADR 0054](decisions/0054-escopo-de-rotas-do-mvp.md)): embeddings (`/v1/embeddings` e a rota por deployment do Azure), batches (`/v1/batches`, `/v1/messages/batches`), arquivos, as rotas de recurso da Responses (`GET`/`DELETE /v1/responses/{id}`, `/v1/responses/{id}/cancel`, `/v1/responses/{id}/input_items`, `/v1/responses/compact`, `/v1/conversations`) e as auxiliares do OpenRouter (`GET /api/v1/generation`, `GET /api/v1/models/{author}/{slug}/endpoints`). Respondem como rota inexistente no provedor real: o 404 de rota desconhecida do driver.

**Autenticação.** OpenAI: `Authorization: Bearer <chave>`. Anthropic: `x-api-key: <chave>` e `anthropic-version`, ou `Authorization: Bearer sk-ant-oat01-…` com o beta `oauth-2025-04-20` (assinatura, spec 006 16.2.1). Gemini: `x-goog-api-key: <chave>` ou `?key=<chave>` (chave inválida → 400 `INVALID_ARGUMENT` `API_KEY_INVALID`; sem chave → 403 `PERMISSION_DENIED`; Bearer inválido → 401 `UNAUTHENTICATED`, textos na spec 006). Azure: `api-key: <chave>` ou `Authorization: Bearer <chave>` nas duas famílias de rota. Chaves vêm do YAML; chave desconhecida devolve o status e o corpo do provedor. Modo `auth: none` aceita qualquer chave para testes rápidos (uma condição de teste do operador: com chave, a resposta é a do real).

**Cabeça crua ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)).** Quando um driver declara que precisa dela (interface opcional, spec 005; hoje só o `anthropic` com `client_verification: claude-code`), o data plane entrega ao driver a linha da requisição e os headers exatamente como chegaram (bytes, ordem, grafia e repetições), além do corpo cru. Sem essa declaração nada é copiado: o caminho quente não muda (spec 014).

**Cabeçalhos de resposta.** `x-request-id` (OpenAI) e `request-id` (Anthropic); headers de rate limit de cada provedor (seção 8); `x-wwz-scenario` e `x-wwz-step` quando um cenário casou (ajuda a depurar o roteador).

**URLs do próprio wwz ([ADR 0073](decisions/0073-origem-das-urls-absolutas-do-wwz.md)).** Quando o real devolve uma URL absoluta da própria API ou do próprio site, o wwz devolve a dele: `<origem><caminho relativo ao mount>`, com `<origem>` = `<esquema>://<host><prefixo externo><prefixo de montagem>`.
- **Esquema:** com o par TCP em `server.trusted_proxies`, o primeiro valor de `X-Forwarded-Proto` (`https` ou `http`). Sem isso, `https` se a conexão chegou por TLS (`server.tls`) e `http` nos demais casos.
- **Host:** com o par confiável, o primeiro valor de `X-Forwarded-Host`; senão, `Host`. Sem host, a URL sai relativa à raiz (`<prefixo externo><prefixo de montagem><caminho>`).
- **Prefixo externo:** com o par confiável, o primeiro valor de `X-Forwarded-Prefix` que começa com `/`, sem a `/` final; senão, vazio.
- **Prefixo de montagem:** o `drivers[].prefix` do driver (vazio na raiz).
- Fora de `trusted_proxies`, os `X-Forwarded-*` são ignorados, como o `X-Forwarded-For`. `Forwarded` (RFC 7239) não é lido.
Hoje só o `opencode` devolve URLs assim: o `api.<protocolo>.base_url` da listagem (spec 006, 16.7) e o `verification_uri_complete` do Console (acima). Ex.: com o `opencode` em `prefix: /opencode`, atrás do Traefik com TLS, a listagem do Go traz `https://wwz.exemplo/opencode/inference/go/openai/v1`. Links de documentação dentro de textos de erro reais (`https://platform.openai.com/...`, `https://opencode.ai/go`) continuam os do real.

**Validação de requisição ([ADR 0029](decisions/0029-escopo-do-server-strict.md), [ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md)).** Sempre ligada, sem opção: cada driver recusa o que o provedor real recusa, com o status, o envelope e o texto reais, e aceita o que o real aceita. A verificação roda no `ParseRequest` do driver (mesma posição e contagem dos demais erros de parse); o custo de uma varredura extra do JSON entra na meta de desempenho (spec 014).

| Driver | Rotas | Recusa | Resposta |
| --- | --- | --- | --- |
| `openai` | `/v1/chat/completions`, `/v1/completions` | Campo de topo fora da lista da API real | 400 `invalid_request_error`, `param: null`, `code: null`, `Unrecognized request argument supplied: <campo>` (vários: `Unrecognized request arguments supplied: a, b`) — conferido (fixtures `testdata/real/openai/validation/chat_unknown_field.json`, `chat_unknown_fields.json`) |
| `openai`, `azure` | Chat Completions | `tools[].function` ou `response_format.json_schema` com `strict: true` em que algum `object` não tem `additionalProperties: false` ou `required` não lista todas as chaves de `properties` | 400 `invalid_request_error`, `param` `tools[<i>].function.parameters` / `response_format`, `code` `invalid_function_parameters` / `invalid_json_schema`, `Invalid schema for function '<nome>': In context=(<caminho>), …` (texto na ADR 0029) — não conferido |
| `openai`, `azure` | `/v1/responses`, `/openai/v1/responses` | Campo de topo desconhecido; `messages` no lugar de `input`; `model` ou `input` ausente; `max_output_tokens` < 16; JSON inválido; tool `function` com `strict: true` e schema fora da regra; `previous_response_id` inexistente | 400 `invalid_request_error` com `param` e `code` preenchidos (textos na seção "Responses API"); conferido no `openai` (fixtures `testdata/real/openai/responses/`), não conferido no `azure` |
| `azure` | Chat Completions (deployment e v1) | Campo de topo fora da lista da API | 400 com o envelope Azure (`code`, `message`, `param`, `type`) e `Unrecognized request argument supplied: <campo>` (texto relatado no Microsoft Q&A) — não conferido: segue só pela documentação, sem chave Azure ([ADR 0043](decisions/0043-drivers-gemini-e-azure.md)) |
| `anthropic` | `/v1/messages`, `/v1/messages/count_tokens` | Campo fora do schema, em qualquer nível | 400 `invalid_request_error`, `<caminho.com.pontos>: Extra inputs are not permitted` (o primeiro; ex.: `foo`, `messages.0.content.0.text.foo`, com o tipo do bloco no caminho) ; aceita o que o real aceita, inclusive a forma do Claude Code 2.1.308 ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)): `role: "system"` em `messages` (o texto real de papel inválido já lista `"system"`, fixture `a3_badrole_second`), `output_config` por mensagem com o beta `per-turn-control-2026-07-01`, `thinking` `adaptive` com `display`, `context_management.edits`, `output_config.effort`/`format`, `safeguards`, `tools[].defer_loading` e `cache_control.ttl`; o texto de cada recusa ligada a beta ausente é conferido no real (task 441) |
| `gemini` | `generateContent`, `streamGenerateContent`, `countTokens` | Campo desconhecido | 400 `INVALID_ARGUMENT` no envelope `google.rpc.Status`, `Invalid JSON payload received. Unknown name "<campo>"…` — conferido (fixtures `testdata/real/gemini/shape/error_field*.json`) |
| `openrouter`, `opencode` | todas | o que o real recusa (a conferir); campos desconhecidos passam | Gateways que repassam ou ignoram campos desconhecidos — não conferido |

"Não conferido" segue a ADR 0042: o texto vem de documentação ou relato público e só vale como pronto depois da fixture real. Rotas não listadas seguem a mesma regra quando o real valida. Schemas de tools fora do subconjunto que o gerador entende nunca geram 400: a API real os aceita (spec 007).

### Responses API (`POST /v1/responses`)

Contrato do driver `openai` ([ADR 0055](decisions/0055-contrato-da-responses-api.md)), conferido em 31/10/2026 com `gpt-4o-mini` (fixtures em `testdata/real/openai/responses/`, citadas entre parênteses). O `azure` (`/openai/v1/responses`), o `openrouter` (`/api/v1/responses`) e o `opencode` (`<base>/openai/v1/responses`) herdam este contrato com as diferenças da spec 006; o que não tem fixture está marcado "não conferido".

**Requisição.** `model` e `input` obrigatórios. `input` é string (uma mensagem `user`) ou lista de itens: `{type?: "message", role: user|assistant|system|developer, content: string | [input_text | input_image | input_file]}`, `{type: "function_call", call_id, name, arguments}`, `{type: "function_call_output", call_id, output}`, `{type: "reasoning", id, summary, encrypted_content?}` e `{type: "item_reference", id}`. Demais campos aceitos: `instructions` (vira mensagem `developer` antes da entrada; não é herdada por `previous_response_id`), `max_output_tokens` (≥ 16), `temperature`, `top_p`, `top_logprobs`, `tools`, `tool_choice` (`auto`, `none`, `required`, `{type: "function", name}`), `parallel_tool_calls`, `max_tool_calls`, `text.format` (`text`, `json_object`, `json_schema` com `name`, `schema`, `strict`), `text.verbosity`, `reasoning.{effort, summary}`, `store` (padrão `true`), `previous_response_id`, `include`, `metadata`, `prompt_cache_key`, `prompt_cache_retention`, `safety_identifier`, `service_tier`, `stream`, `stream_options.include_obfuscation`, `truncation`, `user`, `background`. Lacunas do MVP (ADR 0055): `background: true` é atendido de forma síncrona; `conversation` devolve 404 `Conversation with id '<id>' not found.` (o wwz não guarda conversas, `conversation_not_found`) e `prompt` recebe 400 com `param` do campo (texto não conferido).

**Tools.** `{type: "function", name, description?, parameters, strict?}` (formato plano, sem o `function` aninhado do Chat). `strict` omitido sai como `true` e o schema é devolvido normalizado, com `additionalProperties: false` e `required` listando todas as chaves (`strict_default_no_additional`); o `required` parcial é completado com todas as chaves de `properties` (`strict_default_partial_required`), mas um schema com `additionalProperties: true` fica como veio e a ferramenta volta com `strict: false` (`strict_default_open_schema`); outros schemas que não cabem no modo estrito não foram conferidos e seguem `strict: false`. `strict: false` explícito nunca altera o schema (`echo_fields`). `strict: true` explícito com schema fora da regra → 400 (`strict_true_no_additional`). Ferramentas nativas (`web_search`, `file_search`, `code_interpreter`, `image_generation`, `computer_use_preview`, `mcp`, `custom`…) são aceitas, ecoadas em `tools` e nunca chamadas. O eco de `text.format` com `json_schema` traz `type`, `description` (`null` se ausente), `name`, `schema`, `strict`, nessa ordem (`echo_fields`). Em `tools` da resposta, cada `function` sai com `type`, `description` (`null` se ausente), `name`, `output_schema: null`, `parameters`, `strict`, nessa ordem (`function_call_stream`).

**Resposta (não-stream).** Campos de topo, nessa ordem (`default_store`, `cache_automatic_prefix`): `id` (`resp_` + 50 hex), `object: "response"`, `created_at`, `status` (`completed` | `incomplete`), `access_programs: null`, `background`, `billing: {payer: "openai"}`, `completed_at`, `error: null`, `frequency_penalty: 0.0`, `incomplete_details` (`null` ou `{reason: "max_output_tokens"}`), `instructions`, `max_output_tokens`, `max_tool_calls`, `model` (`models[].version` ou `id`, ADR 0049), `moderation: null`, `output`, `parallel_tool_calls`, `presence_penalty: 0.0`, `previous_response_id`, `prompt_cache_key`, `prompt_cache_retention` (`"in_memory"` sem pedido), `reasoning: {context, effort, summary}`, `safety_identifier`, `service_tier` (`"default"`), `store`, `temperature`, `text: {format: {type}, verbosity: "medium"}`, `tool_choice`, `tool_usage: {image_gen: {input_tokens, input_tokens_details: {image_tokens, text_tokens}, output_tokens, output_tokens_details: {image_tokens, text_tokens}, total_tokens}, web_search: {num_requests}}`, `tools`, `top_logprobs`, `top_p`, `truncation: "disabled"`, `usage`, `user`, `metadata: {}`. Números de ponto flutuante com `.0` (`1.0`, `0.0`).

- Item de mensagem: `{id: "msg_…", type: "message", status, content: [{type: "output_text", annotations: [], logprobs: [], text}], role: "assistant"}`; `status: "incomplete"` quando o teto corta (`incomplete_stream`).
- Item de tool call: `{id: "fc_…", type: "function_call", status: "completed", arguments, call_id: "call_" + 24 alfanuméricos, name}` (`function_call_stream`).
- Item de raciocínio (modelos com `capabilities.reasoning`): `{id: "rs_…", type: "reasoning", summary: [{type: "summary_text", text}]}` antes da mensagem, com `encrypted_content` só com `include: ["reasoning.encrypted_content"]` — não conferido.
- Os ids da resposta e dos itens têm 50 hex e compartilham os 16 primeiros (observado).
- `usage`: `{input_tokens, input_tokens_details: {cache_write_tokens, cached_tokens}, output_tokens, output_tokens_details: {reasoning_tokens}, total_tokens}` — `cache_write_tokens` antes de `cached_tokens` (`cache_automatic_prefix`); `cache_write_tokens_details` só nos modelos gpt-5.6+ (spec 007). `output_tokens` = tokens do texto + 1 (o token de fim; `"Hello there!"` = 3 + 1), ou texto + 2 quando há funções em `tools` (`"Ok."` = 2 + 2); com tool call, `output_tokens` = tokens dos `arguments` + 1, sem overhead de template (vários tool calls não conferidos); a resposta que atinge `max_output_tokens` sai `status: "incomplete"` com `output_tokens` = `max_output_tokens` (`incomplete_stream`). `input_tokens` segue o overhead do Chat (3 + role por mensagem, 3 por requisição; `"Say hi in two words."` = 13).

**Stream (`stream: true`).** `content-type: text/event-stream; charset=utf-8`; cada evento é `event: <type>` + `data: <json>` + linha em branco, sem `data: [DONE]`. `sequence_number` começa em 0 e cresce de 1 em 1. Ordem (`text_stream`, `function_call_stream`, `incomplete_stream`):

1. `response.created` e `response.in_progress`: a resposta com `status: "in_progress"`, `output: []`, `completed_at: null`, `usage: null`, `service_tier: "auto"` (sem `service_tier` pedido) e **sem** `billing`.
2. Por item de saída, em `output_index` crescente: `response.output_item.added` (`item` com `status: "in_progress"` e conteúdo vazio).
   - Mensagem: `response.content_part.added` (`part` com `text: ""`), `response.output_text.delta`… (`content_index`, `delta`, `item_id`, `logprobs: []`, `obfuscation`, `output_index`, `sequence_number`, nessa ordem), `response.output_text.done` (`text` completo, depois de `sequence_number`), `response.content_part.done`.
   - Tool call: `response.function_call_arguments.delta`… (`delta`, `item_id`, `obfuscation`, `output_index`, `sequence_number`), `response.function_call_arguments.done` (`arguments`), sem `content_part`.
   - Raciocínio com `reasoning.summary`: `response.reasoning_summary_part.added`, `response.reasoning_summary_text.delta`…, `response.reasoning_summary_text.done`, `response.reasoning_summary_part.done` — não conferido.
   - `response.output_item.done` com o item final.
3. `response.completed` (`status: "completed"`) ou `response.incomplete` (`status: "incomplete"`, `incomplete_details.reason: "max_output_tokens"`), com `usage`, `completed_at` e `service_tier: "default"`.

`obfuscation` é uma cadeia aleatória que acolchoa o tamanho do evento; `stream_options.include_obfuscation: false` a omite (não conferido). Erros no meio do stream (`event: error`, `response.failed`) seguem a spec 006, 16.1 (não conferido).

**Estado e `store`.** Toda resposta com `store` diferente de `false` vira um registro de 30 dias no store (spec 012; ADR 0055). O registro conta a saída entregue ao cliente (com `finish_length`, a cortada). `previous_response_id` de resposta gravada da mesma conta soma a conversa anterior (mensagens de entrada e a saída como mensagem `assistant`) à entrada: `input_tokens` = 27 na fixture `previous_response_id` (10 + 7 + 7 + 3). Id inexistente, de outra conta, vencido ou gravado com `store: false` → 400 `previous_response_not_found` (`previous_response_not_found`, `previous_response_unstored`).

**Erros de validação** (400, `type: "invalid_request_error"`, `content-type: application/json` sem `charset`, corpo `{"error": {"message", "type", "param", "code"}}` com indentação de 2 espaços e sem quebra de linha final; é o leiaute "API" de `internal/driver/oai`, inclusive para JSON inválido, `model` ausente e modelo aposentado, que no Chat Completions saem no leiaute "borda", com 4 espaços, `charset=utf-8` e `\n` final):

| Caso | `param` | `code` | `message` | Fixture |
| --- | --- | --- | --- | --- |
| Campo de topo desconhecido | o campo | `unknown_parameter` | `Unknown parameter: '<campo>'.` | `unknown_param` |
| `messages` (forma do Chat) | `null` | `unsupported_parameter` | `Unsupported parameter: 'messages'. In the Responses API, this parameter has moved to 'input'. Try again with the new parameter. See the API documentation for more information: https://platform.openai.com/docs/api-reference/responses/create.` | `chat_messages_field` |
| `model` ausente | `model` | `missing_required_parameter` | `Missing required parameter: 'model'.` | `missing_model` |
| `input` ausente | `input` | `missing_required_parameter` | `Missing required parameter: 'input'.` | `missing_input` |
| `max_output_tokens` < 16 | `max_output_tokens` | `integer_below_min_value` | `Invalid 'max_output_tokens': integer below minimum value. Expected a value >= 16, but got <n> instead.` | `max_output_tokens_below_min` |
| JSON inválido | `null` | `invalid_json` | `Invalid body: failed to parse JSON value. Please check the value to ensure it is valid JSON. (Common errors include trailing commas, missing closing brackets, missing quotation marks, etc.)` | `invalid_json` |
| `strict: true` sem `additionalProperties: false` | `tools[<i>].parameters` | `invalid_function_parameters` | `Invalid schema for function '<nome>': In context=(), 'additionalProperties' is required to be supplied and to be false.` | `strict_true_no_additional` |
| `previous_response_id` não encontrado | `previous_response_id` | `previous_response_not_found` | `Previous response with id '<id>' not found.` | `previous_response_not_found` |
| `temperature` > 2 | `temperature` | `decimal_above_max_value` | `Invalid 'temperature': decimal above maximum value. Expected a value <= 2.0, but got 3 instead.` (o limite sai impresso com casa decimal: `2.0`) | `range_temperature` |
| `top_p` > 1 | `top_p` | `decimal_above_max_value` | `Invalid 'top_p': decimal above maximum value. Expected a value <= 1.0, but got 2 instead.` (`1.0`) | `range_top_p` |
| Tipo errado (`model`, `input`, `max_output_tokens`, `temperature`, `stream`) | o campo | `invalid_type` | `Invalid type for '<campo>': expected a string, but got an integer instead.` (o tipo esperado e o recebido mudam) | `type_*` |
| Modelo aposentado ou inexistente | `null` | `model_not_found` | 404, no leiaute da API (2 espaços, sem `\n` final): ``The model `<modelo>` has been deprecated, learn more here: https://platform.openai.com/docs/deprecations`` (aposentado) ou ``The model `<modelo>` does not exist or you do not have access to it.`` (inexistente) | `deprecated_model`, `unknown_model` |
| `conversation` | `null` | `null` | 404: `Conversation with id '<id>' not found.` (`type: invalid_request_error`) | `conversation_not_found` |

**Ordem das verificações** (conferida no real, fixtures `order_*`, `unknown_param_first`; [ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md)), depois da chave (ADR 0053); a primeira que falha responde:
1. corpo que não é JSON (`invalid_json`);
2. `messages` presente (`unsupported_parameter`), antes até de `model` ausente;
3. `model` ausente ou de tipo errado, depois `input`; `model`/`input` ausentes vencem qualquer campo desconhecido;
4. o primeiro campo desconhecido, na ordem do corpo (`zzz` antes de `aaa`);
5. `max_output_tokens` (`integer_below_min_value`), e só então os demais campos conhecidos (tipos e faixas);
6. `tools` com `strict: true` fora da regra (`invalid_function_parameters`);
7. o modelo (404), e por último `conversation` (404).

`previous_response_id` inexistente é verificado depois da validação do corpo e antes dos limites (ordem do wwz; não conferida no real).

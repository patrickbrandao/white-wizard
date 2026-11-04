# 0072 — Recursos do opencode.ai no driver `opencode`

Status: provisória (gatilhos de revisão em cada item)
Data: 2026-11-03
Revisada: 2026-11-03 (task 427: itens 4, 5, 6, 16, 18 e 19 alinhados às fixtures sem custo da task 416; task 434: item 16 com campos por catálogo além do preço, e origem do `base_url` e do `verification_uri_complete` pela [ADR 0073](0073-origem-das-urls-absolutas-do-wwz.md); ver "Revisão")
Atualiza: [0051](0051-limites-na-conta-e-promocao-de-tier.md) (no `opencode`, os limites em dólar dependem da base e a cota Go do Go Teams conta no membro) e [0062](0062-opencode-serve-todo-modelo-em-toda-rota.md) (o catálogo do `opencode` passa a ter dois catálogos, um por família de base).

## Contexto
Três relatórios de 02/11/2026 (`tmp/reports/provider-opencode.md`, `provider-opencode-go.md` e "OpenCode ofertas de inferência do site vs. software", sobre o repositório `anomalyco/opencode` no commit `cce323bf`, versão 2.2.5, e as páginas de opencode.ai) descrevem o que o driver ainda não imita: o modo gratuito sem conta (`Bearer public`), a cota diária dos gratuitos, o bloqueio de gratuitos fora do cliente OpenCode, os planos Go/Go Plus/Go Teams, o Extra Usage, a cota Go por modelo, os budgets por escopo e modelo, o preço com multiplicadores e overrides, os catálogos separados das bases pay-as-you-go e Go e o Console. A fixture conferida `testdata/real/opencode/shape/models.json` (28 modelos do `/inference/go/v1/models`) mostra a forma real de `cost.multipliers` e `cost.text.*.overrides`. É o fato novo exigido para mexer nas ADRs 0051 e 0062.

A conferência da matriz `tmp/opencode-lacunas.md` contra o código da `main` de 2026-11-02 achou sete divergências que o "já coberto" da matriz não dizia:
1. A cota Go (`quota_usd`) é debitada pelo preço de lista, não pelo `opencode.allowance` (0,6× ou 2× a lista) que a própria resposta mostra.
2. `credits_usd`, `spend_monthly_usd` e `quota_usd` valem em qualquer base: uma requisição na base Go debita saldo e budget (o real cobra 0 dentro da cota) e uma na base pay-as-you-go consome a cota Go.
3. `billing_source` sai só pelo caminho (`/go/` → `go`, senão `balance`); nunca `free` nem `go-plus`.
4. O texto "This workspace does not have an active OpenCode Go subscription" é o de **todo** bloqueio (`account_suspended`, manual ou da escada de abuso); não existe a recusa por falta de Go.
5. `GET /v1/usage` sempre responde `status: "ok"`, mesmo com a janela esgotada.
6. `BudgetLimitError` sai sempre com `coverage: "models"` e `resetsAt` = agora + 1 mês, não o dia 1º 00:00 UTC.
7. A listagem traz só `api.chat-completions`, com `base_url` relativo `/inference/go/openai/v1`; o real traz um item por protocolo nativo, com URL absoluta da base. Os catálogos ao vivo têm 28 modelos (Go, fixture) e "75+" (pay-as-you-go), contra 37 e 132 do snapshot embutido no cliente.

## Decisão

**Escopo.**
1. Entram as lacunas L1–L12 da matriz; L13 (headers do cliente) só vira texto: `x-opencode-session-id`, `x-opencode-parent-session-id`, `x-opencode-project` e `x-opencode-client` são aceitos e ignorados, e `x-opencode-session` continua sendo a entidade `user`.
2. Ficam fora, cada um com gatilho:
   - SystemOne / modelos `decision` (`/inference/systemone/v1`, `TypeSafe-Danger-Zone-Images`, `jev-*`): sem forma de requisição nem de resposta. Gatilho: fixture ou documentação do protocolo.
   - GPT-Live (voz) e `x-opencode-org-id` da voz: realtime está fora do MVP ([ADR 0054](0054-escopo-de-rotas-do-mvp.md)). Gatilho: realtime entrar no MVP.
   - `POST /console/api/websearch`: forma da resposta desconhecida. Gatilho: fixture.
   - Library (`/api/v2/library/skills/{slug}`) e SSO (`/auth/sso/...`, `403 SsoRequired`): não afetam a inferência. Gatilho: um roteador sob teste que dependa deles.
   - Top-up, auto-recharge, invoice e BYOK dos Providers do Console: o saldo já é `credits_usd` + `add_usd`; no BYOK o provedor original cobra. Gatilho: pedido de simular faturamento por invoice ou BYOK.
   - `OPENCODE_DISABLE_FREE_MODELS`: é variável do cliente, não do servidor.

**Gratuito e modo `public`.**
3. Um modelo é **gratuito** num catálogo quando o `pricing` efetivo dele naquele catálogo (16 abaixo) está declarado e tem `input_per_m: 0`, como o real ("sem preço de entrada"). `pricing` ausente continua pago a US$ 0, como hoje: nenhuma configuração existente muda de comportamento.
4. `public` como credencial (`Authorization: Bearer public`, e também `x-api-key` na rota Messages e `x-goog-api-key` na rota Google, onde o cliente põe a mesma `apiKey`) é o modo gratuito sem conta. Nunca é procurado em `keys[]` (`keys[].key: public` torna a configuração inválida enquanto o `opencode` estiver montado) e vale também com `server.auth: none`. Sem conta, a requisição só tem as entidades `ip` e `user`. Para o real, `public` é **credencial ausente** que só os gratuitos das bases pay-as-you-go aceitam (conferido, fixtures `testdata/real/opencode/free/`):
   - base pay-as-you-go, modelo gratuito: aceito, segue para o cliente e a cota (itens 5 e 6) (`public_generic_client`, `public_messages`: nenhum 401);
   - base pay-as-you-go, modelo pago: 401 `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}` no envelope do gateway (`public_paid_model`, rota Chat). O wwz dá a mesma resposta à credencial de fato ausente nas bases pay-as-you-go e na rota Messages delas (não conferidos: sem header nenhum e Messages);
   - base Go, qualquer modelo, gratuito inclusive (`beam-free`): o 401 de credencial ausente do Go, `{"error":{"type":"server_error","message":"Upstream request failed: Go requires a credential"}}` (`public_go_base`, igual a `shape/error_nokey.json`); na rota Messages, a tradução `api_error` da spec 006 (não conferido);
   - `GET <base>/v1/usage`: 401 `{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}`, o mesmo da chave inválida (`public_usage`, em `/zen/go/v1/usage`).
   O cliente oficial manda a chave da rota Messages em `x-api-key` (relatório, §4.2). O `opencode` aceita `x-api-key` na rota Messages para qualquer chave. Conferido: `x-api-key` inválida, sem `Authorization`, dá o 401 `api_error` `Upstream request failed: Invalid credential`, não o de credencial ausente (`messages_x_api_key`).
5. **Gratuitos só no cliente OpenCode: fora dele, a cota é zero.** Um modelo gratuito só é atendido quando o `User-Agent` começa com `opencode/` (a forma `opencode/{canal}/{versão}/{nome}` do cliente oficial). Fora disso, a cota diária de gratuitos (item 6) vale **zero**: a resposta é o 429 `FreeUsageLimitError` do item 6, com o mesmo envelope, texto e `retry-after` até 00:00 UTC, com chave ou sem e em qualquer base em que a credencial passe (Go inclusive, onde os gratuitos não entram na cota do item 6). Não há 403 de cliente. A ordem fica: sessão → corpo → modelo → credencial → cliente → cota diária; o passo "cliente" só nega com o 429 e não conta nada no store.
   - Fato novo (task 416): com `Bearer public`, `curl/8.7.1` e o gratuito `big-pickle`, o real respondeu 429 `FreeUsageLimitError` nas rotas Chat e Messages, nunca o 403 `ForbiddenError` que a documentação ("Free models require OpenCode") e o texto provisório anterior previam (`free/public_generic_client.json`, `free/public_messages.json`).
   - Por que "cota zero" e não "cota do IP já gasta": era a primeira chamada a um gratuito daquele IP na gravação; o texto real manda o usuário ao OpenCode ("Free models work in OpenCode") e ao Go; e a leitura concilia a documentação (gratuitos exigem o OpenCode) com a fixture (o bloqueio sai como cota esgotada). A chamada não foi repetida, então isso segue **não conferido** como regra.
   - Gatilhos de revisão: (a) um cliente genérico receber 200 num gratuito do real, que torna a cota fora do OpenCode maior que zero e deixa só o item 6; (b) um 403 de cliente numa fixture, que traz de volta o 403 com o texto dela; (c) uma gravação feita pelo humano com o cliente oficial (sem forjar nada) mostrar outro status ou texto na cota esgotada dentro do OpenCode.
   **Nunca** se manda `User-Agent: opencode/...` nem `x-opencode-client` ao real para passar pelo bloqueio: isso contorna um controle de acesso do provedor. No wwz, o roteador sob teste pode mandar esse `User-Agent` à vontade.
6. **Cota diária dos gratuitos.** A unidade é o IP do cliente (`ClientIP`, com `trusted_proxies`) sem chave, e a conta com chave. O real não documenta a unidade; o bot do projeto fala em "conta e workspace", e o IP é o único sinal que sobra sem conta. A cota conta requisições a modelos gratuitos nas bases pay-as-you-go. Nas bases Go os gratuitos não entram nela: o Go os mostra como "ilimitados por tempo limitado". O número é `drivers[].options.free_requests_per_day` (inteiro ≥ 1, padrão 200; o real não publica o número, então o padrão é escolha do wwz). O dia vira às 00:00 UTC. Estourou: 429 `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`, no envelope do gateway sem `metadata`, inclusive na rota Messages (não vira `api_error`), com `content-type: application/json`, `retry-after` em segundos até 00:00 UTC e `x-opencode-log-id`. Envelope, texto, headers e reset em 00:00 UTC conferidos com cliente genérico (`free/public_generic_client.json`: `retry-after: 34369` às 14:27:11Z; `free/public_messages.json`); o texto antigo `"Daily free usage limit exceeded."` não existe. Na rota Google, o envelope do Google da spec 006 (não conferido). A entidade do store é `free_models`, com id = conta ou `ip:<endereço>`. Não conferidos: a unidade, o número e o texto da cota esgotada dentro do cliente OpenCode (o wwz usa o único texto observado).

**Cobrança por base.**
7. Cada requisição tem uma origem de cobrança, decidida pelo driver:
   - **Modelo gratuito**, em qualquer base: `free`. `cost: 0`, nada é debitado, só a cota diária conta (item 6).
   - **Base pay-as-you-go** (`/inference`, `/zen`): `balance`. O custo é debitado do saldo (`credits_usd`) e entra em `spend_*` e nos budgets; a cota Go não é tocada.
   - **Base Go** (`/inference/go`, `/zen/go`): `go` ou `go-plus`, conforme o plano. `cost: 0`; a cota Go é debitada pelo `allowance`; saldo e budgets não são tocados, salvo pelo Extra Usage (item 12) e pela service account (item 10).
   - Os limites de contagem (`rpm`, `tpm`, concorrência…) valem em todas as bases.
   Isso corrige as divergências 1–3.
8. `allowance = list_cost × allowance_factor`, com `models[].catalogs.opencode-go.allowance_factor` (≥ 0). Sem o campo, vale a regra atual, conferida em dois modelos: 2 nos modelos `gemini-*` nativos e 0,6 nos demais.

**Planos, membros, service accounts e Extra Usage.**
9. O plano é do tier: `tiers.<nome>.plan` ∈ `go`, `go-plus`, `go-teams` ou `none`. Ausente vale `go`, o que mantém compatíveis as configurações atuais, que usam a base Go sem plano. `none` é um workspace sem Go: nas bases Go e no `/v1/usage`, 403 no envelope do gateway, `{"type":"error","error":{"type":"ForbiddenError","message":"This workspace does not have an active OpenCode Go subscription"}}` (não conferido; exige chave de workspace sem Go). Esse texto deixa de ser o do bloqueio, que passa a `{"error":{"type":"permission_error","message":"This workspace has been suspended"}}` (não conferido). `billing_source` sai `go-plus` no `go-plus` e `go` no `go` e no `go-teams` (o do Teams não está conferido). `plan` fica fora de `cfg:policies`, como `promote_when` e `next`. As cotas de cada plano são declaradas no `quota_usd` do tier.
10. O **membro** do workspace é a entidade `project` (`keys[].project`; uma chave sem projeto é o próprio membro). O `opencode` passa a declarar `ip, key, user, project, account`. **Service account** é `keys[].service_account: true`. Ela nunca é coberta pela cota Go, em plano nenhum (o real só documenta o Teams; generalizar é escolha do wwz, não conferida): na base Go é cobrada como `balance`, e o Console responde 403 nos budgets dela.
11. No **Go Teams**, a cota Go (a da conta inteira e a por modelo) conta no membro, não na conta.
12. **Extra Usage.** `accounts[].extra_usage` (booleano; padrão `true` no `go-teams` e `false` nos demais planos). Com a cota Go estourada e o Extra Usage ligado, a requisição é atendida como `balance`: o custo é debitado do saldo e entra nos budgets, e saldo zerado dá 402 `Insufficient account funds`. Com o Extra Usage desligado, 429 `GoUsageLimitError`. `metadata.workspace` passa a ser o id do workspace da conta (`wrk_` + 26 caracteres base32 do sha256 do id da conta; formato não conferido), não mais o fixo `wrk_wwz`.

**Cota Go por modelo e budgets.**
13. `quota_usd.models: {<id do modelo>: <US$ por mês>}` (campo novo e opcional). Cada modelo listado tem cota própria, com os mesmos `rolling_5h_pct` e `weekly_pct`, contada na entidade `model_quota` (`<dono>/<modelo>`; o dono é a conta, ou o membro no Go Teams). `monthly_usd` continua sendo a cota da conta inteira; as duas valem juntas. `GET /v1/usage` mostra em cada janela a maior porcentagem entre a cota da conta e as por modelo, e `status: "rate-limited"` quando a janela nega (corrige a divergência 5). Que janela o real mostra com cotas por modelo não foi conferido.
14. **Budgets.**
    - `spend_monthly_usd`, já existente, é o budget de cobertura `all`: na conta vale para o workspace, no projeto para o membro e na chave para a service account ou a chave.
    - `spend_monthly_models_usd: {<id> | opencode/<id> | opencode-go/<id>: <US$>}` (campo novo, em todo nível de `limits`) é o budget de cobertura `models`, contado na entidade `model_spend` (`<tipo>:<id>/<modelo>`). `opencode/` limita às bases pay-as-you-go, `opencode-go/` às bases Go, e o id puro vale nas duas.
    - Budgets só contam dólares debitados do saldo: gratuito e uso dentro da cota Go nunca contam.
    - Empilham: qualquer budget estourado nega.
    - Resposta: 429 `BudgetLimitError` com `metadata.coverage` (`all` ou `models`) e `resetsAt` = dia 1º do mês seguinte, 00:00 UTC (corrige a divergência 6). O texto continua o das fixtures do cliente; o da documentação, "Upstream request failed: Policy budget exceeded", fica como alternativa não conferida.
    - Nos outros drivers, `spend_monthly_models_usd` vale como condição do operador, com o `spend_limit` do dialeto.

**Preço.**
15. `pricing.multipliers: [{when, factor}]` e `pricing.overrides: [{when, input_per_m?, output_per_m?, cache_read_per_m?, cache_write_per_m?}]`.
    - A gramática de `when` é a da fixture real:
      - `utc_days: {in: [mon…sun]}`;
      - `utc_hours` e `date` com `gte`, `gt`, `lte` ou `lt` (`"HH:MM"` e `"YYYY-MM-DD"`, UTC);
      - `context` com os mesmos operadores e um número de tokens; só em `overrides`, sobre os tokens de entrada da requisição, cache incluído;
      - `any: [when…]` (OU) e `all: [when…]` (E);
      - as chaves de topo combinam com E.
    - Chave desconhecida torna a configuração inválida.
    - O primeiro override que casa substitui os preços que declara. Todos os multiplicadores que casam se multiplicam, avaliados no início da requisição.
    - O preço efetivo vale para o custo de todo driver (débito, `usage.cost`, `list_cost`, `spend`). A listagem do `opencode` o mostra na forma real (`cost.multipliers`, `cost.text.input.overrides[{when, price, cache}]`, `cost.text.output.overrides[{when, price}]`).
    - Não conferidos: a regra de combinação e o que conta como "contexto".
16. **Catálogos.** `models[].catalogs` só vale para modelos do `opencode`. É um mapa com as chaves `opencode` (bases `/inference` e `/zen`) e `opencode-go` (bases `/inference/go` e `/zen/go`), cada uma com `{pricing?, allowance_factor?, context_window?, max_output?, capabilities?, extra?}` (os quatro últimos desde a revisão da task 434, abaixo); `allowance_factor` só existe em `opencode-go`. O valor sobrepõe o do modelo naquele catálogo. Sem `catalogs`, o modelo está nos dois catálogos com o mesmo preço (compatível).
    - Modelo fora do catálogo da base: sai da listagem e responde 400 `Model is unavailable.`.
    - `api` traz **exatamente um** item, o protocolo nativo do modelo (`chat-completions`, `responses`, `messages` ou `generate-content`), com `base_url` absoluto e fixo na família `/inference` do catálogo, mesmo quando a listagem vem de `/zen` ou `/zen/go`: `<origem>/inference/openai/v1` (ou `/anthropic/v1`, `/google/v1beta`) no `opencode` e `<origem>/inference/go/...` no `opencode-go`, com a origem do wwz da [ADR 0073](0073-origem-das-urls-absolutas-do-wwz.md) (esquema, host, prefixo externo e prefixo de montagem); o real usa `https://opencode.ai/inference/...`. O protocolo nativo é o do primeiro dialeto de `models[].protocols` (`openai` → `chat-completions`, `anthropic` → `messages`, `gemini` → `generate-content`; ausente ou `*` → `chat-completions`); quando `extra.api` é declarado, as chaves dele são os protocolos do item (é assim que se declara `responses`), com `base_url` e `sdk` derivados e o valor de `extra` por cima. `/zen/v1/models` é igual a `/inference/v1/models`, e `/zen/go/v1/models` a `/inference/go/v1/models`, fora `created` (conferido, `testdata/real/opencode/catalog/`).
    - `canonical_id` é o id do upstream, que nem sempre é `<autor>/<id>` (`beam-free` → `reflection/beam-1.0-501b-a23b`) e falta em `big-pickle`, `pulse-alpha-free` e `iceman-contributor-free` (conferido). O wwz deriva `<autor>/<id>` quando `extra.canonical_id` não é declarado, e `extra.canonical_id: null` (como qualquer chave de `extra` com `null`) tira o campo do item.
    - Isso corrige a divergência 7 e atualiza a 0062 (antes, só `models[].drivers` definia o catálogo).

**Console.**
17. As rotas ficam em `<prefixo>/console`: `POST /auth/device/code`, `POST /auth/device/token` (device code e refresh), `GET /api/user`, `GET /api/orgs`, `GET /api/v2/config` e `GET /api/policies/current-budgets`.
    - Não há estado no store: `device_code`, `access_token` e `refresh_token` são assinados com HMAC-SHA256, tendo como segredo o valor da chave nomeada por `drivers[].options.console_key` (label de `keys[]`). Só quem conhece a chave forja um token, como já acontece com a própria chave.
    - Fluxo do device code, sem estado: poll antes de `interval` → `slow_down`; entre `interval` e `2 × interval` → `authorization_pending`; depois disso, os tokens. Sem `console_key`, o fluxo nunca aprova e responde `expired_token` depois de `expires_in`. Refresh vencido ou inválido → 400 `invalid_grant`.
    - O access token vale como a chave `console_key` na inferência e no Console. Chaves de API também valem no Console. `x-org-id` é aceito e ignorado.
    - Tempos escolhidos pelo wwz: `interval` 5 s, `expires_in` 900 s, access 3 600 s, refresh 30 dias.
    - Formas não conferidas: `{device_code, user_code, verification_uri_complete, expires_in, interval}`; `{access_token, refresh_token, token_type, expires_in, org_id}`; `{id, email}`; `[{id, name}]`. `GET /api/v2/config` devolve `{"providers": {"opencode": {}, "opencode-go": {}}}`, sem `opencode-go` quando o plano é `none`. `current-budgets` devolve uma lista de `{ruleID, limitDollars, spentDollars, scope, per, coverage, models, vendors, picked, shared, resetsAt}`, com 403 `{"_tag":"Forbidden"}` para service account. 401 é `{"_tag":"Unauthorized"}`.
    - Gatilho: chave do Console para conferir.

**Catálogo pronto e conferência.**
18. O `examples/opencode-completo/` sai das fixtures dos endpoints públicos (`testdata/real/opencode/catalog/payg_models.json` e `go_models.json`, gravadas sem chave em 03/11/2026: 79 modelos pay-as-you-go, 9 gratuitos, e 28 Go, 2 gratuitos, `beam-free` e `iceman-contributor-free`), não do snapshot do cliente. É o que o wwz imita. Modelos só do snapshot e `jev-*` ficam de fora.
19. Só se confere no real o que custa 0 e não exige chave nem forjar o cliente: os dois catálogos sem chave e `public` com cliente genérico (bases pay-as-you-go e Go, modelo gratuito e pago). Feito na task 416 (`testdata/real/opencode/catalog/` e `free/`); o resultado está nos itens 4, 5, 6 e 16. Todo o resto fica "não conferido" na spec 006, seção "Não conferidos no real".

**Contrato.** Todo campo YAML novo é opcional e compatível: `tiers.<n>.plan`, `accounts[].extra_usage`, `keys[].service_account`, `quota_usd.models`, `spend_monthly_models_usd`, `pricing.multipliers`, `pricing.overrides`, `models[].catalogs`, `drivers[].options.free_requests_per_day` e `drivers[].options.console_key`. A única recusa nova é `keys[].key: public`. O evento `request` ganha `billing_source` e `allowance_usd`, opcionais (asyncapi 0.5.0).

## Revisão (03/11/2026, task 427)
Fato novo: as fixtures sem custo da task 416 (`testdata/real/opencode/catalog/` e `free/`, cliente `curl/8.7.1`, US$ 0). O que mudou:
- Item 4: `public` com modelo pago dá 401 `AuthError` `Missing API key.` (não `Invalid credential`); na base Go, o 401 `Go requires a credential` da chave ausente, até com gratuito do catálogo Go; `x-api-key` lido na rota Messages e `public` no `/v1/usage` conferidos.
- Item 5: o 403 `ForbiddenError` de cliente sai; fora do cliente OpenCode a cota de gratuitos é zero (429 do item 6), com gatilhos próprios.
- Item 6: texto real `Free usage limit reached. …`, sem `metadata`, também na rota Messages; reset 00:00 UTC conferido.
- Item 16: um único protocolo em `api`, `base_url` sempre na família `/inference`, `/zen` igual a `/inference` e `canonical_id` do upstream (às vezes ausente).
- Conferidos nos catálogos: 79 e 28 modelos; 9 e 2 gratuitos; `cost.multipliers` também no pay-as-you-go (`gemini-4-argon`; no Go, `deepseek-v4.1-flash`, `deepseek-v4-pro` e `gemini-4-argon`); `overrides` por contexto em 14 e 6 modelos; o mesmo id com preços diferentes (`deepseek-v4-pro`: 1,74/3,48 e 0,66/1,98; `deepseek-v4.1-flash`: 0,3/1,2 e 0,15/0,6); os dois catálogos públicos sem chave.
O resto da decisão fica como estava. As tarefas abertas 421, 422, 423, 425 e 426 foram ajustadas na task 427.

## Revisão (03/11/2026, task 434)
Fato novo: a task 423 montou a listagem pelas fixtures `testdata/real/opencode/catalog/` e achou dois limites da decisão.

**1. Campos por catálogo além do preço (item 16).** Sete ids aparecem nos dois catálogos com campos diferentes além de `cost`:
- `minimax-m3` e `qwen3.8-max`: outro protocolo nativo (`chat-completions` no pay-as-you-go, `messages` no Go), outro `limit` (512 000 e 262 144 de contexto contra 1 000 000 no Go), `family`, `description` e, no `qwen3.8-max`, `modalities`;
- `deepseek-v4-pro`: `name` (`DeepSeek V4 Pro (New)` no Go) e `description`;
- `kimi-k3`, `claude-haiku-5-5`, `glm-5.3`, `deepseek-v4.1-flash` e outros: `capabilities` (no Go, sem `capabilities.cache` e com outro `reasoning`).
Uma entrada de `models[]` com só `pricing` e `allowance_factor` por catálogo não reproduz as duas listagens, e o `examples/opencode-completo` (task 426) não fecha. Decisão:
- `models[].catalogs.<c>` ganha `context_window`, `max_output`, `capabilities` e `extra`, todos opcionais.
  - `context_window` e `max_output` (inteiros > 0) substituem os do modelo naquele catálogo.
  - `capabilities` vale campo a campo: cada capacidade declarada (`tools`, `reasoning`, `vision`, `json_schema`) substitui a do modelo, e as demais ficam.
  - `extra` é mesclado por cima de `models[].extra` com a regra do `extra` do `opencode`: objetos chave a chave, recursivamente; listas e escalares substituem. Um `null` do catálogo substitui o valor do modelo e, no item, tira o campo, seja ele derivado ou vindo do `extra` do modelo.
- O resultado é o **modelo efetivo do catálogo**. Ele vale para tudo o que o wwz faz numa base daquele catálogo, não só para a listagem: `limit` e o 400 `context_length_exceeded` (`context_window`), o teto de saída (`max_output`), as recusas por capacidade e o preço (como já valia com `pricing`). Assim a listagem e o comportamento não se contradizem. Que o real recusa pelo limite do catálogo e não pelo maior dos dois é leitura das fixtures: **não conferido**.
- **Protocolo nativo por catálogo.** Não há `catalogs.<c>.protocols`. No `opencode`, `protocols` não muda o roteamento (ADR 0062) e só escolhe o item de `api`. Por isso o protocolo por catálogo é declarado em `catalogs.<c>.extra.api`: as chaves **não nulas** do `extra.api` efetivo são os protocolos do item. Como `extra` mescla, um `extra.api` no modelo e outro no catálogo somam chaves; para trocar o protocolo, o catálogo anula o do modelo (`api: {chat-completions: null, messages: {}}`) ou o modelo não declara `extra.api` (o protocolo vem do primeiro dialeto de `protocols`) e só o catálogo diferente declara o seu. Ex.: `minimax-m3` com `protocols: [openai]` e `catalogs.opencode-go.extra.api: {messages: {}}`.
- Sem campo próprio, só por `extra` (no modelo ou no catálogo):
  - `name` e `description` (`deepseek-v4-pro`);
  - `family`, `modalities`, `knowledge` (`knowledge: null` o tira);
  - os detalhes de `capabilities` que o wwz não deriva (`cache`, `reasoning.toggle`/`effort`/`budget`, `system_updates`);
  - o `sdk` próprio do `mistral-large-4` (`extra.api.chat-completions.sdk`, mesclado sobre o derivado);
  - o `cache.write: 0` explícito do `big-pickle` (`extra.cost.text.input.cache.write: 0`, mesclado sobre o `cost` derivado do `pricing`).
- Validação: as chaves de `catalogs.<c>.extra` seguem a regra de `models[].extra` (chave que nenhum dialeto do modelo tem → inválida, com o caminho `models[i].catalogs.<c>.extra`). `context_window` ou `max_output` ≤ 0 tornam a configuração inválida. `capabilities` com capacidade desconhecida é recusada pelo YAML estrito, como no modelo.
- Ficam por modelo, sem variar por catálogo: `prompt_cache`, `tokenizer`, `latency`, `errors`, `generation`, `scenarios`, `aliases` e `protocols`. Gatilho: fixture que mostre comportamento diferente por base num deles (por exemplo, `usage` sem cache no Go).
- Compatível: todo campo é novo e opcional, e sem eles o modelo efetivo é o próprio modelo com o `pricing` do catálogo, como antes.

**2. Origem do `base_url` (item 16) e do `verification_uri_complete` (item 17).** A montagem com `Host`/`X-Forwarded-*` no driver esquece o prefixo de montagem do driver e o TLS do próprio wwz. Passa a ser a regra geral da [ADR 0073](0073-origem-das-urls-absolutas-do-wwz.md).

Tarefas: 435 (campos por catálogo: config, engine e listagem) e 436 (origem das URLs: servidor, `driver.Origin`, listagem e Console). As tarefas 425, 426 e 428 passam a depender delas.

## Consequências
- Tarefas 416–428: fixtures sem custo; config em duas fatias; store (`model_quota`, `model_spend`, débito da cota separado do custo, campos do evento); engine em duas fatias (preço e catálogo; origem da cobrança e sujeitos); driver em três (cobrança e erros, listagem, Console); contrato e SDK; exemplo; alinhamento das specs às fixtures; docs.
- Configurações que já usam o `opencode` mudam de números sem mudar de forma: na base Go, a cota passa a cair pelo `allowance` (0,6× a lista nos modelos não Gemini) e o saldo e o `spend` param de ser debitados; na base pay-as-you-go, `quota_usd` deixa de valer. Quem quiser os números antigos da cota usa `catalogs.opencode-go.allowance_factor: 1`. O `examples/cota-5h-semana` faz isso na task 421. Registro no CHANGELOG como correção de fidelidade.
- Specs 003, 004, 005, 006, 010 e 012, o README do Valkey e o asyncapi foram atualizados nesta tarefa (415).

## Alternativas descartadas
- **Plano como campo da conta (`accounts[].plan`).** O plano define as cotas, e as cotas já moram no tier. Os planos Go, Go Plus e Go Teams são os tiers do OpenCode no quadro da spec 006.
- **Mudar o significado de `quota_usd.monthly_usd` para "por modelo".** Seria uma mudança incompatível; o campo novo `models` resolve sem quebrar.
- **Novo bloco `budgets[]` em `accounts[]`.** Os níveis conta, projeto e chave já existem com `spend_monthly_usd`; um mapa por modelo no mesmo `limits` reaproveita entidades, janelas e políticas.
- **Unidade da cota diária só por conta.** Sem chave não há conta.
- **Unidade por `x-opencode-project` ou por sessão.** O cliente controla esses valores e os troca à vontade.
- **Tokens do Console guardados no store.** Assinados, valem em todas as instâncias sem chave nova no Valkey.
- **Catálogo pronto a partir do snapshot do cliente.** O wwz imita o servidor, e os endpoints públicos dele listam menos modelos que o snapshot.

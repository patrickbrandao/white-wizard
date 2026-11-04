# Configuração do wwz

Espelho legível da [spec 003](../specs/003-configuracao.md), que é a fonte da verdade; os detalhes de cada área estão nas specs citadas. Toda a configuração vive num YAML; flags e variáveis `WWZ_*` sobrescrevem campos individuais, na ordem **flag > env > YAML > padrão**.

```sh
go run ./cmd/wwz validate -config meu.yaml          # confere configuração e cenários
go run ./cmd/wwz serve -config meu.yaml
```

O YAML é lido em modo estrito: campo desconhecido é erro com linha e coluna. Valor zero ou ausente significa "usar o padrão".

## Exemplo

```yaml
server:
  listen: ":8000"
  reuseport: false      # true = um listener SO_REUSEPORT por CPU
  auth: keys            # keys | none
  startup_duration: 0s
  seed: 0               # 0 = aleatório; qualquer outro valor reproduz a execução
  concurrency: 262144   # conexões simultâneas do fasthttp
  max_request_body_mb: 4
  max_rps: 0            # proteção global; 0 = sem limite

admin:
  listen: ":9000"

keys:
  - key: sk-test-fast
    label: provedor-rapido
    limits: { rpm: 60000, tpm: 5000000, concurrency: 5000 }
  - key: sk-test-limited
    label: provedor-com-cota
    limits: { rpm: 300, tpm: 40000, concurrency: 20 }

models:
  - id: gpt-5-sim
    protocols: [openai]
    owned_by: wwz
    context_window: 128000
    capabilities: { tools: true, reasoning: true, vision: false, json_schema: true }
    latency: fast
    errors: none
    generation: default
  - id: claude-sim-sonnet
    display_name: Claude Sim Sonnet
    protocols: [anthropic]
    capabilities: { tools: true, reasoning: true }
    latency: realistic-70b
    errors: flaky
  - id: mock-agent-files
    protocols: [openai, anthropic]
    scenarios: [file-agent-flow]
    latency: fast

profiles:
  latency:
    fast:          { ttft: 0ms, itl: 0ms }
    realistic-70b: { queue: {dist: lognormal, median: 20ms, p99: 300ms}, ttft: {dist: normal, mean: 450ms, stddev: 120ms}, itl: {dist: normal, mean: 18ms, stddev: 4ms}, reasoning_itl: {mean: 12ms, stddev: 3ms}, load_factor: 2.5 }
  errors:
    none:  { rate: 0 }
    flaky: { rate: 5, types: [rate_limit, server_error, stream_truncated], mid_stream_rate: 2 }
  generation:
    default: { mode: random, output_tokens: {dist: histogram, buckets: [[50, 0.4], [200, 0.4], [800, 0.2]]}, reasoning_tokens: {min: 30, max: 200, probability: 100}, tool_calls: { probability: 35, extra_call_probability: 45, optional_param_probability: 50 } }

scenarios:
  dir: ./scenarios

store:
  kind: memory          # memory | valkey
  addr: [valkey:6379]   # obrigatório com kind: valkey
  password: ""
  cluster: false
  read_timeout: 5ms
  on_down: local        # allow | deny | local
  prefix: wwz
  local_first: false    # buckets por instância com cota limit/N, sincronizados a cada 100 ms
  event_stream: false   # true para gravar eventos no ev:stream (painel)
  event_max_len: 1000000
  event_sample: 1       # 0 < s ≤ 1 (0 = padrão 1); amostra só eventos request e error
```

Sem `models`, `keys` ou `drivers`, valem os padrões embutidos descritos em [uso.md](uso.md#padrões-embutidos).

**Sem opção de desvio** ([ADR 0042](../specs/decisions/0042-fidelidade-total-ao-provedor-real.md)). Nenhum campo do YAML faz um driver se comportar diferente do provedor real: a validação das requisições vale sempre, com o texto real, e o cache de prompt segue as regras reais de cada provedor. O YAML configura as condições que o operador quer simular (latência, erros, limites, cenários, chaves, modelos); quando ativadas, saem na forma exata do provedor.

## `server` (data plane)

| Campo | Padrão | Valores e significado |
| --- | --- | --- |
| `listen` | `:8000` | `host:porta`; mudar exige restart |
| `reuseport` | `false` | Um listener `SO_REUSEPORT` por CPU |
| `concurrency` | 262 144 | Máximo de conexões simultâneas |
| `max_request_body_mb` | 4 | Corpo maior → 413 no formato do driver |
| `read_timeout`, `write_timeout`, `idle_timeout` | 60 s, sem limite, 120 s | Durações Go (`30s`, `2m`) |
| `auth` | `keys` | `keys` (só chaves do YAML) ou `none` (aceita qualquer chave) |
| `startup_duration` | 0 | `/health/ready` devolve 503 durante esse tempo |
| `seed` | 0 | 0 = aleatório |
| `request_headers` | `true` | Aceita os overrides `x-wwz-*` por requisição |
| `max_chunks_per_stream` | 100 000 | Teto de chunks de um stream |
| `ping_interval` | 15 s | Intervalo de `ping` SSE |
| `trusted_proxies` | vazio | CIDRs cujos `X-Forwarded-For` são aceitos, e também `X-Forwarded-Proto`, `X-Forwarded-Host` e `X-Forwarded-Prefix` na origem das URLs absolutas do wwz ([ADR 0073](../specs/decisions/0073-origem-das-urls-absolutas-do-wwz.md), [drivers.md](drivers.md#origem-das-urls-absolutas)); CIDR inválido é erro |
| `tls` | — | `cert_file` + `key_file`, ou `self_signed: true` |
| `max_rps` | 0 | Proteção global (503 no formato do driver); 0 = sem limite |

## `admin` (control plane)

| Campo | Padrão | Significado |
| --- | --- | --- |
| `listen` | `:9000` | Endereço do control plane; mudar exige restart |
| `token` | vazio | Bearer de `/admin/*`; `WWZ_ADMIN_TOKEN` tem precedência; sem token, só loopback |
| `debug` | `false` | Guarda as últimas requisições para `GET /admin/requests` |

## `store`

Estado de limites, bloqueios e eventos ([spec 012](../specs/012-estado-valkey.md), [ADR 0020](../specs/decisions/0020-nomes-config-store.md)); chaves do Valkey em [cache/valkey/README.md](../cache/valkey/README.md).

| Campo | Padrão | Valores |
| --- | --- | --- |
| `kind` | `memory` | `memory` (uma instância) ou `valkey` (cluster); mudar exige restart |
| `addr` | vazio | Lista `host:porta`; obrigatória com `kind: valkey` |
| `password`, `cluster`, `prefix` | `""`, `false`, `wwz` | — |
| `read_timeout` | 5 ms | ≥ 0 |
| `on_down` | `local` | `allow`, `deny` ou `local` (o que fazer com o Valkey fora) |
| `local_first` | `false` | Buckets por instância com cota `limit/N` |
| `event_stream` | `false` | Grava eventos no `ev:stream` (fonte do painel) |
| `event_max_len` | 1 000 000 | ≥ 0 |
| `event_sample` | 1 | 0 < s ≤ 1 (0 = padrão) |

## `drivers`

Lista de drivers ativos ([spec 005](../specs/005-drivers.md), [drivers.md](drivers.md)). `name` é obrigatório; `prefix`, quando presente, começa com `/`. Sem `prefix`, o driver atende seus caminhos canônicos na raiz e as colisões são resolvidas pela credencial.

```yaml
drivers:
  - { name: openai, prefix: /openai }
  - { name: anthropic, prefix: /anthropic }
  - { name: openrouter, prefix: /openrouter }
  - { name: opencode, prefix: /opencode, options: { reasoning_field: reasoning_content } }
```

`options` é um mapa de texto para texto com as opções do driver; opção desconhecida ou valor inválido torna a configuração inválida (`wwz validate`, boot e recarga). O nome antigo `opts` é recusado com `field "opts" renamed to "options"`, sem alias ([ADR 0048](../specs/decisions/0048-dialetos-catalogos-e-templates.md)).

| Driver | Opções | Valores |
| --- | --- | --- |
| `openai`, `azure` | `reasoning_field` | só `hidden` (a OpenAI real nunca devolve o texto do raciocínio, [ADR 0046](../specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)) |
| `opencode` | `reasoning_field`, `require_session`, `free_requests_per_day`, `console_key` | `reasoning_field`: `reasoning_content` (padrão), `reasoning`, `hidden`, `inline_tag`; `require_session`: só `true` (padrão; `false` torna a configuração inválida); `free_requests_per_day`: inteiro ≥ 1 (padrão `200`), a cota diária dos gratuitos; `console_key`: label de uma entrada de `keys[]`, a conta do login do Console ([OpenCode](#opencode)) |
| `openrouter` | `reasoning_field` | `reasoning` (padrão), `reasoning_content`, `hidden`, `inline_tag` |
| `anthropic` | `require_version`, `oauth_key`, `oauth_plan`, `client_verification` | `require_version`: só `true` (padrão; `false` torna a configuração inválida, [ADR 0068](../specs/decisions/0068-lote-de-specs-da-auditoria-do-done.md)); `oauth_key`: label de uma entrada de `keys[]`, a chave que a assinatura OAuth do Claude Code representa e o segredo dos tokens `sk-ant-oat01-…` (sem ela não há assinatura); `oauth_plan`: `pro`, `max_5x` (padrão), `max_20x`, `team` ou `enterprise`, só o `organization_type` e o `rate_limit_tier` do perfil; `client_verification`: `off` (padrão) ou `claude-code`, que recusa com 403 `forged_request_error` toda requisição de inferência que não é do Claude Code 2.1.308 ([claude-code.md](claude-code.md)) |
| `gemini` | — | formato de raciocínio fixo (partes `thought`) |

As opções do `anthropic` da [ADR 0074](../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md) são recusadas em qualquer outro driver (`config: drivers[0] (openai): unknown option "oauth_key"`) e com valor inválido: `option oauth_key="nada" is invalid: not the label of a keys[] entry`, `option oauth_plan="free" is invalid (allowed: [max_5x max_20x pro team enterprise])`, `option client_verification="on" is invalid (allowed: [off claude-code])`. `client_verification: claude-code` é a única opção de driver que diverge do real, por pedido do humano; não é o antigo `server.strict`. A cota da assinatura é a `quota_usd` da conta da chave `oauth_key` ([Contas, projetos e promoção de tier](#contas-projetos-e-promoção-de-tier)), e o modo estrito exige os headers sem proxy que os reescreva. Exemplo pronto em [`examples/claude-code-assinatura`](../examples/claude-code-assinatura/README.md):

```yaml
drivers:
  - name: anthropic
    prefix: /claude-code
    options: { oauth_key: assinante, oauth_plan: max_20x, client_verification: claude-code }
```

## `keys`

| Campo | Padrão | Significado |
| --- | --- | --- |
| `key` | — | A chave aceita |
| `label` | `key-<n>` (n a partir de 1) | Nome usado em métricas, painel e na API admin (`/admin/entities/key/<label>`) |
| `account` | `acct-<label>` | Conta dona: recebe o tier, os limites e o gasto (ver [Contas, projetos e promoção de tier](#contas-projetos-e-promoção-de-tier)) |
| `project` | — | Projeto ou workspace |
| `tier` | `default` | Tier da conta da chave, quando `accounts[]` não declara o da conta; nome de `tiers` (`[A-Za-z0-9._-]{1,64}`, [ADR 0031](../specs/decisions/0031-regra-unica-de-nome-de-tier.md)) |
| `driver` | — | Desempate do driver na raiz quando a credencial não basta |
| `limits` | — | Teto da própria chave (tabela abaixo), contado na chave; não sobe o limite da conta |
| `latency`, `errors` | — | Nome de perfil em `profiles.latency`/`profiles.errors`; nome inexistente torna a configuração inválida |
| `status` | `active` | `active`, `suspended`, `banned` ou `expired` |
| `expires_at` | — | Expiração da chave |
| `models` | todos | Allowlist de modelos |
| `service_account` | `false` | No `opencode`, a chave é de uma service account: fora da cota Go, cobrada no saldo e sem acesso aos budgets do Console ([OpenCode](#opencode)) |

O valor `public` é reservado pelo `opencode` (modo gratuito sem conta): `keys[].key: public` com o driver montado torna a configuração inválida.

## Contas, projetos e promoção de tier

Os provedores reais limitam por organização, projeto ou workspace, nunca por chave ([ADR 0051](../specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md), [spec 003](../specs/003-configuracao.md), [spec 010](../specs/010-controle-de-taxa.md)). Por isso o tier e todos os limites dele (taxa, concorrência, rampa, gasto, créditos, cota) contam na **conta**, divididos por todas as chaves dela.

```yaml
keys:
  - {key: sk-a, label: a, account: acme, project: acme-web}
  - {key: sk-b, label: b, account: acme, limits: {rpm: 10}}   # teto só desta chave
accounts:
  - id: acme
    tier: tier-1                  # padrão: o keys[].tier das chaves da conta, senão default
    limits: {tpm: 300000}         # por cima do tier
projects:
  - id: acme-web
    limits: {tpm: 50000}          # teto do projeto, abaixo do da conta
tiers:
  tier-1:
    limits: {rpm: 500, tpm: 200000, spend_monthly_usd: 100}
    promote_when: {purchased_usd: 50, age_days: 7}
    next: tier-2
  tier-2:
    limits: {rpm: 5000, tpm: 2000000}
```

- `accounts[]`: `id` (obrigatório, único), `tier`, `limits` e, no `opencode`, `extra_usage` ([OpenCode](#opencode)). `projects[]`: `id` (obrigatório, único) e `limits`; projeto não tem tier. Entrada sem chave que a cite gera só aviso; `keys[].account`/`keys[].project` sem entrada continuam válidos.
- Precedência na conta: `accounts[].limits` > tier atribuído em tempo de execução (`POST /admin/entities/account/{id}/tier`, `cfg:policies`, promoção) > `accounts[].tier` / `keys[].tier` > `default`. Projeto: só `projects[].limits`. Chave: só `keys[].limits`.
- Chaves da mesma conta com `tier` diferentes (ou diferente de `accounts[].tier`) tornam a configuração inválida: `config: keys[i].tier "x" conflicts with account "a" tier "y"`.
- Os headers de limite mostram, por medida, a entidade com limite declarado e menor `remaining` entre chave, projeto e conta.
- `tiers.<nome>.promote_when` (`purchased_usd`, `spend_usd`, `age_days`, `requests`; todas as condições não nulas valem juntas, pelo menos uma > 0) exige `next`, um tier existente; ciclo em `next` é inválido. A conta sobe um passo para `next` quando o acumulado dela satisfaz a condição, só para cima, sem reiniciar o wwz; métrica `wwz_tier_promotions_total{from,to}`. Com `store: memory` a regra é a mesma, local à instância.
- Compras simuladas: `POST /admin/entities/account/{id}/credits` com `{"add_usd": n}` (n > 0) soma ao saldo e ao acumulado `purchased_usd` (visível no `EntityState`); `{"usd": n}` só define o saldo, sem contar como compra. O painel faz as duas coisas na tela **Limites**: atribui o tier à conta da chave ou da conta consultada e compra créditos (`add_usd`), com auditoria. A tela mostra o tier da conta e o acumulado comprado; a chave só exibe o teto próprio.
- Um "Evaluation tier" é só um tier de limites menores com `promote_when: {age_days: N}` ou `{requests: N}`.

## `limits`

Em `tiers.<nome>.limits`, `accounts[].limits`, `projects[].limits` e `keys[].limits`; semântica na [spec 010](../specs/010-controle-de-taxa.md). Cada recusa sai com status, envelope e headers do provedor ([drivers.md](drivers.md)).

| Campo | Valores |
| --- | --- |
| `rps`, `burst`, `rpm`, `rpd`, `tpm`, `itpm`, `otpm`, `tpd`, `concurrency`, `max_queue`, `after_n` | Inteiros ≥ 0; 0 = sem limite |
| `queue_timeout` | Duração ≥ 0 |
| `spend_monthly_usd`, `spend_soft_usd`, `credits_usd` | ≥ 0 |
| `spend_monthly_models_usd` | Mapa `<modelo>` → US$ ≥ 0, o budget mensal por modelo. A chave é um `models[].id` ou, no `opencode`, `opencode/<id>` (só bases pay-as-you-go) e `opencode-go/<id>` (só bases Go) |
| `quota_usd` | `monthly_usd` ≥ 0; `rolling_5h_pct` e `weekly_pct` em [0, 100]; `models`: mapa `<models[].id>` → US$ ≥ 0, a cota mensal de cada modelo, com as mesmas porcentagens |
| `ramp` | `threshold_tpm`, `max_increase_pct` ≥ 0; `window` duração ≥ 0 |
| `algorithm` | `token_bucket` (padrão) ou `fixed_window` |
| `tpm_reserve` | `max_tokens` ou `none` |
| `exclude_cache_read` | booleano |
| `on_limit` | `reject` (429) ou `queue` (segura até caber, até `queue_timeout`) |
| `mode` | `always`, `after_n`, `window` ou `off` |
| `window` | `{every, duration}`, com `duration` ≤ `every`; obrigatórios e > 0 com `mode: window` |

## `models`

| Campo | Obrigatório | Significado |
| --- | --- | --- |
| `id` | sim | Nome devolvido nas listagens e aceito em `model` |
| `aliases` | não | Outros nomes que resolvem para este modelo |
| `protocols` | não | Ausente ou vazia vale `["*"]` (os três dialetos); senão, lista de `openai`, `anthropic` e `gemini` (ou `["*"]` = os três; valor fora do conjunto é inválido): os dialetos de wire em que o modelo é servido. O dialeto vem do tipo da rota (chat, completions, responses → `openai`; Messages e count_tokens → `anthropic`; rotas Gemini → `gemini`), não do nome do driver. Nos drivers diretos (`openai`, `azure`, `anthropic`, `gemini`) e no `opencode` (endpoint nativo de cada modelo, como o real), chamar pelo dialeto errado devolve o erro de modelo inexistente do driver; o `openrouter` serve todo modelo do seu catálogo em todas as rotas dele. Os nomes antigos `openrouter`/`opencode` são aceitos com aviso e traduzidos: a lista inteira vira `drivers` e os dialetos dela (nenhum → `openai`) viram `protocols` ([ADR 0048](../specs/decisions/0048-dialetos-catalogos-e-templates.md)) |
| `drivers` | não | Catálogo: drivers (`openai`, `azure`, `anthropic`, `gemini`, `openrouter`, `opencode`) que listam o modelo em `/v1/models` e o aceitam em `model`. Ausente = todos os que falam um dos `protocols` (ex.: `protocols: [openai]` aparece em `openai`, `azure`, `openrouter` e `opencode`, não em `anthropic`). Nome desconhecido, ou driver que não fala nenhum dos `protocols` (exceto `openrouter`), é configuração inválida. Use para imitar o catálogo real: `{id: glm-4.6, protocols: [openai], drivers: [opencode]}` |
| `display_name`, `owned_by`, `created` | não | Metadados de listagem |
| `context_window` | não | Padrão 128 000; excedido → 400 `context_length_exceeded` |
| `capabilities` | não | `tools`, `reasoning`, `vision`, `json_schema`; capacidade ausente → 400 como o provedor faria |
| `latency`, `errors`, `generation` | não | Nome de um perfil ou objeto inline; padrão = perfis `default` |
| `scenarios` | não | Cenários candidatos ([spec 011](../specs/011-cenarios.md)); sem casamento → `generation` |
| `tokenizer` | não | `auto` (padrão), `simulated`, `tiktoken:o200k_base` ou `tiktoken:cl100k_base`; outro valor é configuração inválida. `auto` usa o BPE real da OpenAI nos modelos OpenAI pelo `version`, `id` ou aliases (`gpt-4o*`, `chatgpt-4o*`, `gpt-4.1*`, `gpt-4.5*`, `gpt-5*`, `o1*`, `o3*`, `o4*`, `gpt-oss*` → `o200k_base`; `gpt-4`, `gpt-4-*`, `gpt-3.5-turbo*`, `text-embedding-*` → `cl100k_base`) e o simulado nos demais, que não têm tokenizador público. Um modelo de nome próprio que imita um OpenAI pede `tiktoken:o200k_base`; `simulated` troca fidelidade por custo (~2 µs/KB contra ~12 µs/KB do BPE, [ADR 0053](../specs/decisions/0053-tokenizador-bpe-e-ordem-chave-corpo.md)) |
| `max_output` | não | Teto de saída (padrão 16 384); listado como `max_tokens` (Anthropic), `limit.output` (OpenCode) e `top_provider.max_completion_tokens` (OpenRouter) |
| `pricing` | não | `input_per_m`, `output_per_m`, `cache_read_per_m`, `cache_write_per_m` em US$ por milhão de tokens, com `multipliers` e `overrides` opcionais ([OpenCode](#opencode)); valem para o custo de todo driver. No `opencode`, `pricing` declarado com `input_per_m: 0` torna o modelo gratuito; ausente, o modelo é pago a US$ 0 |
| `catalogs` | não | Só modelos do `opencode`: catálogos `opencode` e `opencode-go`, cada um com `pricing`, `allowance_factor` (só `opencode-go`), `context_window`, `max_output`, `capabilities` e `extra` ([OpenCode](#opencode)) |
| `extra` | não | Campos da listagem `/models` que o wwz não deriva, na forma real; chave que nenhum dialeto do modelo tem torna a configuração inválida. No `opencode`, `null` tira o campo do item e as chaves de `extra.api` são o protocolo nativo |
| `prompt_cache` | não | Regras de cache do modelo real imitado: `min_tokens`, `granularity`, `kind` (`automatic`, `explicit`, `none`); ausente = padrão do driver ([spec 007](../specs/007-geracao-respostas.md), [ADR 0046](../specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)) |

## OpenCode

Campos do `opencode` da [ADR 0072](../specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md) ([spec 003](../specs/003-configuracao.md)). O comportamento de cada um está em [drivers.md](drivers.md#opencode). Todos são opcionais: uma configuração sem eles continua válida e com o mesmo formato. Plano ausente vale `go`, e `pricing` ausente não torna o modelo gratuito. Os números mudam, porém, pela cobrança por base ([drivers.md](drivers.md#cobrança-por-base)).

| Campo | Onde | Valores e significado |
| --- | --- | --- |
| `plan` | `tiers.<n>` | `go` (padrão), `go-plus`, `go-teams` ou `none` (sem Go). Os outros drivers ignoram o campo, e ele não vai a `cfg:policies` |
| `extra_usage` | `accounts[]` | Booleano; padrão `true` no `go-teams` e `false` nos demais. Cota Go estourada passa a ser cobrada no saldo |
| `service_account` | `keys[]` | Booleano, padrão `false`. Fora da cota Go em qualquer plano; 403 nos budgets do Console |
| `project` | `keys[]` | O membro do workspace; sem projeto, a própria chave. No `go-teams`, a cota Go conta nele |
| `quota_usd.models` | `limits` | `{<models[].id>: US$}`, cota Go mensal por modelo, junto com `monthly_usd` |
| `spend_monthly_models_usd` | `limits` | `{<id> \| opencode/<id> \| opencode-go/<id>: US$}`, budget mensal por modelo (cobertura `models`); `spend_monthly_usd` é o de cobertura `all` |
| `pricing.multipliers` | `models[]`, `catalogs.<c>` | `[{when, factor}]`, `factor` > 0; todos os que casam se multiplicam |
| `pricing.overrides` | `models[]`, `catalogs.<c>` | `[{when, input_per_m?, output_per_m?, cache_read_per_m?, cache_write_per_m?}]`, com pelo menos um preço; vale o primeiro que casa |
| `catalogs` | `models[]` | `{opencode?, opencode-go?}`, cada um `{pricing?, allowance_factor?, context_window?, max_output?, capabilities?, extra?}`. O modelo só existe nos catálogos listados; sem o campo, nos dois |
| `allowance_factor` | `catalogs.opencode-go` | ≥ 0; razão `allowance / list_cost` da cota Go. Padrão 2 nos `gemini-*` e 0,6 nos demais |
| `free_requests_per_day` | `drivers[].options` | Inteiro ≥ 1, padrão `200`: requisições por dia aos gratuitos, por IP sem chave ou por conta com chave |
| `console_key` | `drivers[].options` | Label de `keys[]`: a conta que o login do Console aprova e o segredo dos tokens. Sem ela, o login nunca aprova |

**Gramática de `when`** (a do catálogo real; tudo em UTC, avaliado no início da requisição):
- `utc_days: {in: [mon, tue, wed, thu, fri, sat, sun]}`;
- `utc_hours: {gte|gt|lte|lt: "HH:MM"}` e `date: {gte|gt|lte|lt: "YYYY-MM-DD"}`;
- `context: {gte|gt|lte|lt: <tokens>}`, só em `overrides`: os tokens de entrada da requisição, cache incluído;
- `any: [<when>…]` (OU) e `all: [<when>…]` (E); as chaves de topo combinam com E.

**Campos por catálogo.** `pricing` substitui o bloco inteiro do modelo; `context_window` e `max_output` (> 0) substituem os do modelo; `capabilities` substitui campo a campo; `extra` é mesclado por cima de `models[].extra` (objetos chave a chave, listas e escalares substituem, `null` tira o campo do item). O protocolo nativo por catálogo vai em `extra.api`: não existe `catalogs.<c>.protocols`.

```yaml
tiers:
  go:
    plan: go                        # go | go-plus | go-teams | none (ausente = go)
    limits:
      quota_usd:
        rolling_5h_pct: 20
        weekly_pct: 50
        models: {deepseek-v4.1-flash: 100, glm-5.3-flash: 60, kimi-k3: 15}
  go-teams:
    plan: go-teams                  # cota por membro; Extra Usage ligado por padrão
    limits:
      quota_usd: {rolling_5h_pct: 20, weekly_pct: 50, models: {deepseek-v4.1-flash: 100}}
  payg:
    plan: none                      # sem Go: 403 nas bases Go
accounts:
  - id: acme
    tier: go-teams
    extra_usage: true
    limits:
      credits_usd: 50
      spend_monthly_usd: 40                                     # budget do workspace (coverage all)
      spend_monthly_models_usd: {opencode/claude-opus-5-5: 10}  # budget por modelo (coverage models)
projects:
  - id: ana                         # membro do workspace
    limits: {spend_monthly_usd: 15}
keys:
  - {key: oc_sk_ana, label: ana, account: acme, project: ana}
  - {key: oc_sk_ci, label: ci, account: acme, service_account: true}
drivers:
  - name: opencode
    options: {free_requests_per_day: "200", console_key: ana}
models:
  - id: big-pickle
    drivers: [opencode]
    pricing: {input_per_m: 0, output_per_m: 0}   # declarado com entrada 0 = gratuito
    extra: {canonical_id: null}                  # o real não lista canonical_id
    catalogs: {opencode: {}}                     # só no pay-as-you-go
  - id: deepseek-v4.1-flash
    drivers: [opencode]
    pricing:
      input_per_m: 0.3
      output_per_m: 1.2
      cache_read_per_m: 0.006
      multipliers:                               # 2× no pico dos dias úteis
        - when: {utc_days: {in: [mon, tue, wed, thu, fri]}, any: [{all: [{utc_hours: {gte: "01:00"}}, {utc_hours: {lt: "04:00"}}]}]}
          factor: 2
    catalogs:
      opencode: {}
      opencode-go: {pricing: {input_per_m: 0.15, output_per_m: 0.6, cache_read_per_m: 0.003}, allowance_factor: 0.6}
  - id: glm-5.3-flash               # mesmo preço nos dois catálogos (catalogs ausente)
    drivers: [opencode]
    pricing: {input_per_m: 0.15, output_per_m: 0.5, cache_read_per_m: 0.03}
  - id: kimi-k3
    drivers: [opencode]
    pricing: {input_per_m: 3, output_per_m: 15, cache_read_per_m: 0.3, cache_write_per_m: 3}
  - id: claude-opus-5-5
    drivers: [opencode]
    pricing: {input_per_m: 4, output_per_m: 20, cache_read_per_m: 0.2, cache_write_per_m: 5}
    catalogs: {opencode: {}}                     # só no pay-as-you-go
  - id: minimax-m3                  # outro protocolo nativo e outro limite no Go
    drivers: [opencode]
    protocols: [openai]             # chat-completions no pay-as-you-go
    context_window: 512000
    pricing: {input_per_m: 0.3, output_per_m: 1.2, cache_read_per_m: 0.06}
    catalogs:
      opencode: {}
      opencode-go:
        context_window: 1000000
        pricing:                    # o dobro acima de 512k de contexto
          input_per_m: 0.3
          output_per_m: 1.2
          cache_read_per_m: 0.06
          overrides:
            - {when: {context: {gt: 512000}}, input_per_m: 0.6, output_per_m: 2.4, cache_read_per_m: 0.12}
        extra: {api: {messages: {}}}  # messages no Go
```

O [`examples/opencode-completo`](../examples/opencode-completo/README.md) traz a configuração completa, gerada dos catálogos reais.

**Erros de configuração.** Recusados no boot, na recarga e em `wwz validate`, com o caminho do campo:

| Causa | Mensagem |
| --- | --- |
| `keys[].key: public` com o `opencode` montado | `config: keys[0].key "public" is reserved by the opencode driver` |
| Plano desconhecido | `config: tiers.x.plan: unknown plan "gold" (want go, go-plus, go-teams, none)` |
| Modelo desconhecido (ou prefixo fora de `opencode/`/`opencode-go/`) em `quota_usd.models` ou `spend_monthly_models_usd` | `config: accounts[0].limits.quota_usd.models: unknown model "nope"` |
| Condição desconhecida em `when` | `config: models[0].pricing.multipliers[0].when: unknown condition "weekday"` |
| `context` em `multipliers` | `config: models[0].pricing.multipliers[0].when: condition "context" is only allowed in overrides` |
| Hora ou data malformada | `config: models[0].pricing.multipliers[0].when.utc_hours.gte: invalid time "25:00" (want "HH:MM")` |
| Override sem preço | `config: models[0].pricing.overrides[0]: no price (want input_per_m, output_per_m, cache_read_per_m or cache_write_per_m)` |
| `factor` ≤ 0 | `config: models[0].pricing.multipliers[0].factor must be > 0 and finite, got 0` |
| Catálogo desconhecido | `config: models[0].catalogs: unknown catalog "opencode-x" (want opencode, opencode-go)` |
| `catalogs` num modelo que nenhum driver com catálogo serve | `config: models[0].catalogs: no driver serving the model has catalogs` |
| `allowance_factor` fora de `opencode-go` | `config: models[0].catalogs.opencode.allowance_factor: catalog "opencode" has no allowance` |
| `context_window` ou `max_output` ≤ 0 no catálogo | `config: models[0].catalogs.opencode-go.context_window must be > 0, got 0` |
| Chave de `extra` do catálogo que nenhum dialeto do modelo tem | `config: models[0].catalogs.opencode-go.extra: key "nada" unknown to dialects [anthropic gemini openai]` |
| `free_requests_per_day` que não é inteiro ≥ 1 | `config: drivers[0] (opencode): option free_requests_per_day="0" is invalid: want an integer >= 1` |
| `console_key` que não é label de `keys[]` | `config: drivers[0] (opencode): option console_key="ninguem" is invalid: not the label of a keys[] entry` |

## `profiles`

Perfis nomeados e reutilizáveis; modelos e chaves os citam pelo nome. Perfis prontos: latência `default`, `fast`, `gpt-4o-like`, `llama-70b-like`, `deepseek-r1-like`, `slow-flaky`; erros `default`, `none`, `flaky`; geração `default`.

- **`latency`** ([spec 008](../specs/008-latencia.md)): etapas `queue`, `ttft`, `prefill_overhead`, `prefill_per_token`, `reasoning_itl`, `itl`, `tool_call_delay`, `tool_args_itl`, `finish_delay`, cada uma com distribuição (`fixed`, `uniform {min, max}`, `normal {mean, stddev}`, `lognormal {median, p99}`, `exponential {mean}`, `pareto {scale, shape}`; um escalar como `300ms` é `fixed`). Mais `load_factor`, `concurrency`, `max_queue`, `queue_timeout`, `chunk_tokens`, `chunk_bytes`, `model` (`constant`, `per_token`, `polynomial`), `speedup_ratio`, `clock` (`real`, `logical`), `pacing` e `cache_ttft_reduction` (0–1, padrão 0,7: quanto o TTFT encolhe na proporção dos tokens lidos do cache).
- **`errors`** ([spec 009](../specs/009-erros-e-caos.md)): `rate` (0–100), `types`, `weights` (tipo → peso; toda chave está em `types`, ou na lista padrão de 8 tipos com `types` vazio, e todo peso é > 0 e finito; tipo sem peso pesa 1; outra coisa torna a configuração inválida, [ADR 0059](../specs/decisions/0059-lote-da-rodada-5-achados-de-spec.md)), `mid_stream_rate`, `mid_stream_types`, `after_chunks`, `after_ratio`, `hang_duration`, `timeout_delay`, `slow_headers_delay` (padrão 15 s, negativo inválido, ADR 0058), `retry_after`, `after_n`, `burst {every, duration, rate}`, `tool_misbehavior`, `moderation` e `outages`.
- **`generation`** ([spec 007](../specs/007-geracao-respostas.md)): `mode` (`random`, `echo`, `lorem`, `dataset`, `template`, `json`), `output_tokens`, `reasoning_tokens {min, max, probability}`, `tool_calls {probability, extra_call_probability, optional_param_probability, …}`, `text_before_tool_probability`, `sentences`, `dataset_path`, `template`. Outro `mode` torna a configuração inválida (`config: models[x].generation.mode: unknown mode "y"`, ou `profiles.generation.<nome>.mode`) no boot, na recarga e em `wwz validate`.

### Modo `dataset`

`mode: dataset` responde com textos de um arquivo JSONL em `dataset_path` (obrigatório; caminho relativo ao diretório de trabalho). Sem `dataset_path`, com o arquivo ilegível, com linha malformada ou sem entradas, a configuração é inválida. `dataset_path` com outro modo é ignorado. Só JSONL; SQLite fica fora ([ADR 0068](../specs/decisions/0068-lote-de-specs-da-auditoria-do-done.md)).

Uma entrada por linha (linhas em branco são ignoradas). `prompt` é um texto (um turno do usuário) ou a lista de turnos `{role, content}`; `response` é o texto devolvido:

```jsonl
{"prompt": "Qual é a capital da França?", "response": "Paris é a capital da França."}
{"prompt": [{"role": "user", "content": "oi"}, {"role": "assistant", "content": "Olá!"}, {"role": "user", "content": "conte uma história"}], "response": "Era uma vez..."}
```

- A chave é o hash do histórico normalizado: só turnos `user` e `assistant` com texto (o `model` do Gemini conta como `assistant`; prompt de sistema, resultados de ferramenta e turnos vazios ficam de fora), com espaços em sequência reduzidos a um e sem espaços nas pontas. Duas entradas com o mesmo histórico: vale a primeira.
- Sem casamento, uma entrada sorteada pela semente da requisição (`x-wwz-seed`/`server.seed`): mesma semente, mesma resposta.
- `max_tokens` continua valendo: o texto é cortado no teto, com o motivo de truncamento do dialeto (`length`, `max_tokens`, `MAX_TOKENS`).
- O arquivo é lido uma vez por versão (tamanho e data de modificação) e compartilhado entre requisições; a recarga relê só o que mudou.

### Janelas de indisponibilidade

`errors.outages` é uma lista de janelas com `schedule` (cron de seis campos, com segundos), `duration`, `type` (padrão `overloaded`), `rate` (0–100, padrão 100) e `timezone` (padrão UTC); vale no topo (`errors.outages`, global) e dentro de perfis de erro (modelo, chave).

```yaml
errors:
  outages:
    - {schedule: "*/30 * * * * *", duration: 5s, type: overloaded}
    - {schedule: "0 */10 * * * *", duration: 1m, type: server_error, rate: 50}
    - {schedule: "0 0 */6 * * *", duration: 1h, type: bad_gateway}
```

## Outras seções

- `tiers`: limites por tier, plano do `opencode` (`plan`, ver [OpenCode](#opencode)) e promoção automática (`promote_when`, `next`; ver [Contas, projetos e promoção de tier](#contas-projetos-e-promoção-de-tier)); políticas do cluster em `cfg:policies` ([spec 012](../specs/012-estado-valkey.md), [ADR 0025](../specs/decisions/0025-politicas-e-tier-por-entidade.md)).
- `scenarios`: `builtin`, `dir`, `files`, `select`, `on_no_match`, `conversation_id` ([spec 011](../specs/011-cenarios.md)). `builtin` (padrão `true`) carrega os seis cenários embutidos no binário e na imagem Docker: `file-agent-flow`, `long-reasoning`, `multi-tool-parallel`, `refusal`, `retry-on-error` e `search-then-answer`. Um arquivo de `dir`/`files` com o mesmo `name` substitui o embutido. `builtin: false` remove da recarga todo cenário com o nome de um embutido, mesmo o carregado por `POST /admin/scenarios` (o registro não guarda a origem; use `dir`/`files` para um cenário próprio com esse nome).
- `chaos`: `scope_header` (padrão `x-test-id`) ([spec 009](../specs/009-erros-e-caos.md)).
- `usage.extended` (padrão `false`): liga o header `x-wwz-input-breakdown`.
- `metrics`: `max_series` (padrão 5 000 por métrica) e `vllm_mirror` (padrão `false`), ambos lidos só no boot ([spec 013](../specs/013-observabilidade-admin.md)).
- `resilience.max_reports` em [0, 10 000] (0 = padrão 100).
- `abuse`, `geo`, `logging`, `vars`: tipos em `internal/config/config.go`, detalhes nas specs de cada área.

## Variáveis de ambiente e flags

| Variável | Flag de `wwz serve` | Campo |
| --- | --- | --- |
| `WWZ_CONFIG` | `-config` | Arquivo YAML |
| `WWZ_LISTEN` / `WWZ_PORT` | `-listen` / `-port` | `server.listen` (`WWZ_PORT` vence `WWZ_LISTEN`) |
| `WWZ_ADMIN_LISTEN` / `WWZ_ADMIN_PORT` | `-admin-listen` / `-admin-port` | `admin.listen` |
| `WWZ_ADMIN_TOKEN` | — | `admin.token` |
| `WWZ_SEED` | `-seed` | `server.seed` |
| `WWZ_AUTH` | `-auth` | `server.auth` |
| `WWZ_REUSEPORT` (`1`/`true`) | — | `server.reuseport` |
| `WWZ_LOG_LEVEL` | `-log-level` | `logging.level` |
| — | `-log-format` | `logging.format` (`json`, `text`) |
| `WWZ_SCENARIOS_DIR` | `-scenarios-dir` | `scenarios.dir` |
| `WWZ_STORE` | — | `store.kind` |
| `WWZ_VALKEY_ADDR` (lista separada por vírgula) | — | `store.addr`; força `store.kind: valkey`, salvo se `WWZ_STORE` estiver definida |
| `WWZ_VALKEY_PASSWORD` | — | `store.password` |
| `WWZ_VALIDATE_ON_LOAD` (`1`/`true`) | `-validate-on-load` | Validação completa no boot e na recarga (abaixo) |
| `POD_NAME` / `POD_IP` | — | Header `x-wwz-pod` na resposta |

O `wwz` também lê `WWZ_ADMIN_TOKEN` (bearer do control plane; ver [uso](uso.md)) e `WWZ_CONFIG` (caminho do YAML, como `-config`). Os demais binários têm as próprias variáveis: `wwz-panel` em [painel.md](painel.md), `wwz-tools` em [wwz-tools.md](wwz-tools.md) e `wwz-load` (`WWZ_LOAD_KEY`, `WWZ_LOAD_TOKEN`, `WWZ_TOOLS_TOKEN`, `WWZ_ADMIN_TOKEN`) em [wwz-load.md](wwz-load.md). Não existe variável `WWZ_TRACE`: o tracing segue a spec 013.

## Precedência por requisição

Latência, erros e `seed`: header `x-wwz-*` > passo de cenário > chave (`keys[].latency`, `keys[].errors`) > modelo > perfil `default` ([ADR 0026](../specs/decisions/0026-precedencia-chave-sobre-modelo.md)).

## Recarga

`SIGHUP` ou `POST /admin/config` recarregam modelos, perfis e chaves sem derrubar conexões abertas; requisições em andamento terminam com a configuração antiga. Configuração inválida aborta o boot e, na recarga, é recusada por inteiro (a anterior continua). Campos marcados "mudar exige restart" (`server.listen`, `admin.listen`, `store.kind`, `metrics.*`) só valem no boot.

Arquivo de cenário inválido é logado e pulado, salvo com `-validate-on-load` (`WWZ_VALIDATE_ON_LOAD=1`): aí o boot roda as mesmas verificações de `wwz validate` e sai com código 1 em qualquer erro, e a recarga com um cenário inválido é recusada por inteiro. Com a flag ligada, um cenário quebrado no disco bloqueia também as trocas de tier e de política vindas do cluster até ser corrigido.

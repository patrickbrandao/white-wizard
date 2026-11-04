# 003 — Configuração

Status: rascunho
Depende de: 000, 001
Origem: seções 4 do documento Claude Docs congelado em 2026-10-30

## Contexto
Schema YAML, variáveis `WWZ_*`, recarga e precedência de parâmetros (seção 4).

## Requisitos
- R13 (configuração por arquivo e runtime)
- I3 (precedência), I23 (hot reload seguro)

## Interfaces e contratos
- `internal/config`
- `POST /admin/config` em [013](013-observabilidade-admin.md)

## Dados
- Arquivo YAML do operador; nada persistido pelo servidor

## Critérios de aceite
- [ ] `wwz validate` rejeita YAML inválido com mensagem de campo
- [ ] `wwz validate` rejeita valor fora das faixas e enums das tabelas desta spec
- [ ] O exemplo YAML desta spec passa em `wwz validate`
- [ ] Recarga com erro mantém a configuração anterior
- [ ] `server.strict` ou `cache_hit_probability` no YAML: `wwz validate`, boot e recarga recusam como campo desconhecido, com linha e coluna
- [ ] `errors.outages` (raiz, perfil e inline) com `schedule`, `timezone`, `duration`, `rate`, `type` ou `drivers` inválido: boot abortado, recarga recusada e `wwz validate` com erro que cita o caminho da janela
- [ ] `models[].protocols` ausente ou vazio vale `["*"]` (o modelo é servido nos três dialetos)
- [ ] `models[].protocols` com valor fora de `openai`/`anthropic`/`gemini`/`*`/`openrouter`/`opencode`, ou `models[].drivers` com nome desconhecido: `wwz validate`, boot e recarga recusam citando `models[i]`; `openrouter`/`opencode` em `protocols` geram aviso e a tradução da ADR 0048
- [ ] Um modelo com `protocols: [openai]` e sem `drivers` é servido em `POST /v1/chat/completions` dos drivers `openai`, `openrouter` e `opencode`, aparece nos três `/v1/models` e recebe o erro de modelo inexistente em `POST /v1/messages` do `anthropic`
- [ ] `types` e `mid_stream_types` de perfil de erros (`profiles.errors.*` e `errors` inline do modelo) com nome desconhecido, `weights` com chave que não está em `types` (ou na lista padrão, com `types` vazio) ou com peso ≤ 0 ou não finito, e `models[].generation.reasoning_field` fora da regra do driver que serve o modelo: boot, recarga e `wwz validate` recusam citando o caminho (`profiles.errors.flaky.types[1]`, `models[x].generation.reasoning_field`)
- [ ] `drivers[].opts` é recusado com a mensagem de renomeação; `drivers[].options.reasoning_field` é aceito conforme a spec 007
- [ ] `accounts[]`/`projects[]` com `id` vazio ou repetido, tier de chaves da mesma conta em conflito, `tiers.<nome>.next` inexistente, `promote_when` sem `next` ou sem condição, ou ciclo de `next`: boot abortado, recarga recusada e `wwz validate` com erro que cita o caminho
- [ ] `keys[].latency`/`keys[].errors` com perfil inexistente: boot abortado, recarga recusada e `wwz validate` com erro que cita `keys[i]`
- [ ] `models[].deprecated_at`/`retires_at` fora de `YYYY-MM-DD`, `retires_at` anterior a `deprecated_at` ou chave de `models[].extra` desconhecida de todos os dialetos do modelo: boot abortado, recarga recusada e `wwz validate` com erro que cita `models[i]`
- [ ] Campos do OpenCode ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)) inválidos tornam a configuração inválida no boot, na recarga e em `wwz validate`, com o caminho: `keys[].key: public` com o `opencode` montado; `tiers.<n>.plan` fora de `go`/`go-plus`/`go-teams`/`none`; `quota_usd.models` ou `spend_monthly_models_usd` com modelo desconhecido, prefixo diferente de `opencode/`/`opencode-go/` ou valor < 0; `pricing.multipliers[j]` sem `when`, com condição desconhecida ou `factor` ≤ 0; `pricing.overrides[j]` sem `when`, sem preço ou com `context` fora de `overrides`; `models[].catalogs` com catálogo desconhecido, em modelo fora do catálogo do `opencode` ou com `allowance_factor` fora de `opencode-go` ou < 0; `models[].catalogs.<c>.context_window` ou `max_output` ≤ 0; chave de `models[].catalogs.<c>.extra` que nenhum dialeto do modelo tem; `drivers[].options.free_requests_per_day` que não é inteiro ≥ 1; `drivers[].options.console_key` que não é label de `keys[]`
- [ ] Opções do `anthropic` da [ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md) inválidas tornam a configuração inválida no boot, na recarga e em `wwz validate`, com o caminho: `drivers[i].options.oauth_key` que não é label de `keys[]`, `oauth_plan` fora de `pro`/`max_5x`/`max_20x`/`team`/`enterprise`, `client_verification` fora de `off`/`claude-code`; as três são recusadas como opção desconhecida em qualquer outro driver; sem elas o `anthropic` se comporta como antes
- [ ] Configuração sem nenhum campo novo da ADR 0072 continua válida e com o mesmo significado de antes (todo campo é opcional; plano ausente = `go`; `pricing` ausente não é gratuito)

## Fora de escopo
- Configuração distribuída por Valkey além de `cfg:policies` ([012](012-estado-valkey.md))

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0026-precedencia-chave-sobre-modelo.md](decisions/0026-precedencia-chave-sobre-modelo.md)
- [specs/decisions/0027-formas-finais-dos-recursos-importados.md](decisions/0027-formas-finais-dos-recursos-importados.md)
- [specs/decisions/0029-escopo-do-server-strict.md](decisions/0029-escopo-do-server-strict.md) (opção `server.strict` removida pela 0042)
- [specs/decisions/0042-fidelidade-total-ao-provedor-real.md](decisions/0042-fidelidade-total-ao-provedor-real.md)
- [specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)
- [specs/decisions/0049-formas-reais-do-primeiro-drift.md](decisions/0049-formas-reais-do-primeiro-drift.md)
- [specs/decisions/0031-regra-unica-de-nome-de-tier.md](decisions/0031-regra-unica-de-nome-de-tier.md)
- [specs/decisions/0048-dialetos-catalogos-e-templates.md](decisions/0048-dialetos-catalogos-e-templates.md)
- [specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md](decisions/0051-limites-na-conta-e-promocao-de-tier.md)
- [specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)
- [specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### 4. Configuração

Toda a configuração vive em um YAML; flags de linha de comando e variáveis `WWZ_*` sobrescrevem campos individuais, na ordem flag > env > YAML > padrão.

A unidade central é o **modelo**: cada modelo declara seu nome, os dialetos de wire que fala (`protocols`), os drivers em cujo catálogo aparece (`drivers`, opcional), seu perfil de latência, de erros e de geração. Chaves de API recebem limites próprios. Cenários são carregados de arquivos separados.

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
    limits: { rpm: 300, tpm: 40000, concurrency: 20 }   # teto da chave
  - key: sk-acme-a
    label: acme-a
    account: acme        # as chaves da conta dividem os limites do tier
    project: acme-web
  - key: sk-acme-b
    label: acme-b
    account: acme

accounts:
  - id: acme
    tier: tier-1

projects:
  - id: acme-web
    limits: { tpm: 50000 }

tiers:
  tier-1:
    limits: { rpm: 500, tpm: 200000, spend_monthly_usd: 100 }
    promote_when: { purchased_usd: 50, age_days: 7 }
    next: tier-2
  tier-2:
    limits: { rpm: 5000, tpm: 2000000, spend_monthly_usd: 500 }

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
    flaky: { rate: 5, types: [rate_limit, server_error, stream_truncated], mid_stream_rate: 2, outages: [{schedule: "0 */10 * * * *", duration: 30s, type: server_error}] }
  generation:
    default: { mode: random, output_tokens: {dist: histogram, buckets: [[50, 0.4], [200, 0.4], [800, 0.2]]}, reasoning_tokens: {min: 30, max: 200, probability: 100}, tool_calls: { probability: 35, extra_call_probability: 45, optional_param_probability: 50 } }

errors:                 # global: só janelas de indisponibilidade (spec 009)
  outages:
    - {schedule: "0 0 3 * * *", duration: 2m, type: overloaded, rate: 50, timezone: UTC, drivers: [openai]}

scenarios:
  builtin: true         # carrega os 6 cenários embutidos no binário (spec 011)
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

O YAML é lido em modo estrito: campo desconhecido é erro com linha e coluna. Valor zero ou ausente significa "usar o padrão" em todos os campos abaixo.

**Sem opção de desvio ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md)).** Nenhum campo do YAML faz um driver se comportar diferente do provedor real: a validação das requisições vale sempre, com o texto real (spec 004), e o cache de prompt segue as regras reais de cada provedor (spec 007). Por isso `server.strict` e `cache_hit_probability` não existem mais: as duas chaves são recusadas como desconhecidas (`wwz validate`, boot e recarga), mudança incompatível registrada no CHANGELOG. O que o YAML configura são as condições que o operador quer simular (latência, erros, limites, cenários, chaves, modelos); quando ativadas, saem na forma exata do provedor.

**Campos de `server` (data plane)**

| Campo | Padrão | Valores válidos / significado |
| --- | --- | --- |
| `listen` | `:8000` | Endereço `host:porta`; mudar exige restart |
| `reuseport` | `false` | Um listener `SO_REUSEPORT` por CPU |
| `concurrency` | 262 144 | Máximo de conexões simultâneas do fasthttp |
| `max_request_body_mb` | 4 | Corpo maior → 413 no formato do driver |
| `read_timeout`, `write_timeout`, `idle_timeout` | 60 s, sem limite, 120 s | Durações Go (`30s`, `2m`) |
| `auth` | `keys` | `keys` ou `none`; outro valor é erro |
| `startup_duration` | 0 | `/health/ready` devolve 503 durante esse tempo |
| `seed` | 0 | 0 = aleatório |
| `request_headers` | `true` | Aceita os overrides `x-wwz-*` por requisição |
| `max_chunks_per_stream` | 100 000 | Teto de chunks de um stream |
| `ping_interval` | 15 s | Intervalo de `ping` SSE |
| `trusted_proxies` | vazio | CIDRs cujos `X-Forwarded-For` são aceitos, e também `X-Forwarded-Proto`, `X-Forwarded-Host` e `X-Forwarded-Prefix` na origem das URLs absolutas do wwz (spec 004, [ADR 0073](decisions/0073-origem-das-urls-absolutas-do-wwz.md)); CIDR inválido é erro |
| `tls` | — | `cert_file` + `key_file`, ou `self_signed: true` (certificado em memória) |
| `max_rps` | 0 | Proteção global (GCRA, 503 no formato do driver); 0 = sem limite |

**Campos de `admin` (control plane):** `listen` (padrão `:9000`; mudar exige restart), `token` (bearer de `/admin/*`; `WWZ_ADMIN_TOKEN` tem precedência; sem token, só loopback), `debug` (guarda as últimas requisições para `/admin/requests`).

**Campos de `store`** ([ADR 0020](decisions/0020-nomes-config-store.md))

| Campo | Padrão | Valores válidos |
| --- | --- | --- |
| `kind` | `memory` | `memory` ou `valkey`; mudar exige restart |
| `addr` | vazio | Lista `host:porta`; obrigatória com `kind: valkey` |
| `password`, `cluster`, `prefix` | `""`, `false`, `wwz` | — |
| `read_timeout` | 5 ms | ≥ 0 |
| `on_down` | `local` | `allow`, `deny` ou `local` |
| `local_first` | `false` | Buckets por instância com cota `limit/N` |
| `event_stream` | `false` | Grava eventos no `ev:stream` |
| `event_max_len` | 1 000 000 | ≥ 0 |
| `event_sample` | 1 | 0 < s ≤ 1 (0 = padrão) |

**Campos de `limits`** (em `tiers.<nome>.limits`, `accounts[].limits`, `projects[].limits` e `keys[].limits`; semântica em [010](010-controle-de-taxa.md))

| Campo | Valores válidos |
| --- | --- |
| `rps`, `burst`, `rpm`, `rpd`, `tpm`, `itpm`, `otpm`, `tpd`, `concurrency`, `max_queue`, `after_n` | Inteiros ≥ 0; 0 = sem limite |
| `queue_timeout` | Duração ≥ 0 |
| `spend_monthly_usd`, `spend_soft_usd`, `credits_usd` | ≥ 0 |
| `spend_monthly_models_usd` | Mapa `<modelo>` → US$ ≥ 0 (budget mensal por modelo; ADR 0072). A chave é um `models[].id`, ou, no `opencode`, `opencode/<id>` (só bases pay-as-you-go) e `opencode-go/<id>` (só bases Go); id puro vale em todas as rotas. Modelo desconhecido ou outro prefixo: `config: <caminho>.spend_monthly_models_usd: unknown model "x"` |
| `quota_usd` | `monthly_usd` ≥ 0; `rolling_5h_pct` e `weekly_pct` em [0, 100]; `models`: mapa `<models[].id>` → US$ ≥ 0, a cota mensal de cada modelo, com as mesmas porcentagens de janela (ADR 0072; modelo desconhecido: `config: <caminho>.quota_usd.models: unknown model "x"`) |
| `ramp` | `threshold_tpm`, `max_increase_pct` ≥ 0; `window` duração ≥ 0 |
| `algorithm` | `token_bucket` ou `fixed_window` |
| `tpm_reserve` | `max_tokens` ou `none` |
| `exclude_cache_read` | booleano |
| `on_limit` | `reject` ou `queue` |
| `mode` | `always`, `after_n`, `window` ou `off` |
| `window` | `{every, duration}`, durações ≥ 0 com `duration` ≤ `every`; com `mode: window` os dois são obrigatórios e > 0 |

**Nomes de tier.** As chaves de `tiers` e os valores de `keys[].tier` seguem a mesma regra em todo o sistema: `[A-Za-z0-9._-]{1,64}` (a regra que o painel aplica ao gravar `cfg:policies`); a configuração com outro nome é inválida ([ADR 0031](decisions/0031-regra-unica-de-nome-de-tier.md)).

Os campos de `keys[]` são `key`, `label` (padrão `key-<n>`, n a partir de 1), `account` (padrão `acct-<label>`: toda chave tem conta, que recebe os limites do tier e o gasto e vai ao evento `request`, [ADR 0032](decisions/0032-campos-de-uso-no-ev-stream.md)), `project`, `tier` (tier da conta da chave quando `accounts[]` não declara tier; padrão `default`), `driver`, `limits` (teto da própria chave, contado na entidade `key`; não sobe o limite da conta), `latency`, `errors`, `status` (`active` padrão, `suspended`, `banned`, `expired`), `expires_at` e `models` (allowlist; vazio = todos) e `service_account` (booleano, padrão `false`; no `opencode`, a chave é de uma service account, fora da cota Go e sem acesso aos budgets do Console; ADR 0072). O valor `public` é reservado pelo `opencode` (modo gratuito sem conta, spec 006 16.3): `keys[].key: public` com o driver montado torna a configuração inválida (`config: keys[i].key "public" is reserved by the opencode driver`). `keys[].latency` e `keys[].errors` são nomes de perfil (`profiles.latency`/`profiles.errors`); nome inexistente torna a configuração inválida (`config: keys[i] references unknown latency profile "x"`), no boot, na recarga e em `wwz validate`, em vez de falhar cada requisição da chave com 500. `resilience.max_reports` fica em [0, 10 000] (0 = padrão 100). `drivers[].name` é obrigatório e `drivers[].prefix`, quando presente, começa com `/`. `drivers[].options` é um mapa de texto para texto com as opções do driver (`reasoning_field`, spec 007; `require_session`, só do `opencode`: o valor é `true`, o único que o real tem, e `false` torna a configuração inválida — [ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md); `require_version`, só do `anthropic`: também só `true`, porque o real sempre exige `anthropic-version`, e `false` torna a configuração inválida — [ADR 0068](decisions/0068-lote-de-specs-da-auditoria-do-done.md)); opção desconhecida para o driver torna a configuração inválida. Opções do `opencode` da [ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md): `free_requests_per_day` (inteiro ≥ 1, padrão `200`: a cota diária de requisições a modelos gratuitos por IP sem chave ou por conta com chave; o real não publica o número) e `console_key` (label de uma entrada de `keys[]`: a conta que o login do Console aprova e o segredo dos tokens assinados; sem ela o device flow nunca aprova; label desconhecido torna a configuração inválida). Opções do `anthropic` da [ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md): `oauth_key` (label de uma entrada de `keys[]`: a chave que a assinatura OAuth representa e o segredo dos tokens assinados; sem ela não há assinatura e todo Bearer `sk-ant-oat…` é token inválido; label desconhecido torna a configuração inválida), `oauth_plan` (`pro`, `max_5x`, `max_20x`, `team` ou `enterprise`, padrão `max_5x`: só o `organization_type` e o `rate_limit_tier` das rotas de perfil, spec 006 16.2.1) e `client_verification` (`off`, padrão, ou `claude-code`: recusa com 403 `forged_request_error` toda requisição de inferência que não é do Claude Code, spec 020; é a única opção de driver que diverge do real, por pedido do humano, e não é `server.strict`). Ex.: `options: {oauth_key: assinante, oauth_plan: max_20x, client_verification: claude-code}`. O nome antigo `opts` é recusado com `config: drivers[i]: field "opts" renamed to "options"` ([ADR 0048](decisions/0048-dialetos-catalogos-e-templates.md)).

**Contas, projetos e tiers** ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md); semântica em [010](010-controle-de-taxa.md)). `accounts[]`: `id` (obrigatório, único), `tier` (padrão: o `keys[].tier` das chaves da conta, senão `default`) e `limits` (campos não nulos por cima do tier). `projects[]`: `id` (obrigatório, único) e `limits`. `keys[].account`/`keys[].project` sem entrada continuam válidos (conta ou projeto implícito); entrada sem chave que a cite só gera aviso. Chaves da mesma conta com `tier` diferentes, ou `keys[].tier` diferente de `accounts[].tier`, tornam a configuração inválida (`config: keys[i].tier "x" conflicts with account "a" tier "y"`). `tiers.<nome>`: `limits`, `next` (nome de tier existente) e `promote_when: {purchased_usd, spend_usd, age_days, requests}` (≥ 0; pelo menos um > 0; exige `next`). `next` inexistente, `promote_when` sem `next` ou sem condição e ciclo na cadeia de `next` tornam a configuração inválida, com o caminho `tiers.<nome>`. `tier`, `next` e as chaves de `tiers` seguem a regra de nome de tier acima.

**Bloco `errors` e `outages`.** O bloco `errors:` da raiz tem só `outages` (lista de janelas de indisponibilidade que valem para toda requisição); `outages` também existe em `profiles.errors.<nome>` e no `errors` inline de um modelo. Campos de cada janela: `schedule` (cron de seis campos com segundos, obrigatório), `duration` (> 0, obrigatório), `type` (padrão `overloaded`; tipo injetável da spec 009), `rate` (0–100, padrão 100), `timezone` (IANA, padrão UTC) e `drivers` (nomes de `drivers[]`; vazio = todos). As janelas de raiz, modelo, chave (via perfil de `keys[].errors`) e perfil de `x-wwz-chaos-profile` se somam, em vez de seguir a precedência abaixo; semântica, precedência e sintaxe do cron em [009](009-erros-e-caos.md). Qualquer campo inválido torna a configuração inválida com o caminho da janela (`config: errors.outages[0]: schedule: ...`), no boot, na recarga e em `wwz validate`.

**`generation.mode` e `dataset_path` ([ADR 0068](decisions/0068-lote-de-specs-da-auditoria-do-done.md)).** `generation.mode` (inline ou via perfil) aceita `random` (padrão quando ausente), `echo`, `lorem`, `dataset`, `template` e `json` (spec 007); outro valor torna a configuração inválida (`config: models[x].generation.mode: unknown mode "y"`), no boot, na recarga e em `wwz validate`. `mode: dataset` exige `dataset_path` (arquivo `.jsonl` legível; ver spec 007); sem ele, ou com o arquivo ilegível ou malformado, a configuração é inválida. `dataset_path` com outro modo é aceito e ignorado.

**Perfil de erros: nomes e pesos ([ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md)).** Cada nome em `types` é um tipo injetável antes do primeiro byte e cada nome em `mid_stream_types` é um tipo do meio do stream (spec 009); nome desconhecido torna a configuração inválida (`config: profiles.errors.flaky.types[1]: unknown error type "x"`; `mid_stream_types[i]` → `unknown mid-stream type`). `weights` é um mapa tipo → peso: toda chave está em `types` (com `types` vazio, na lista padrão de 8 tipos da spec 009) e todo peso é > 0 e finito; tipo de `types` sem peso pesa 1. `models[].generation.reasoning_field` (inline ou via perfil) segue a regra de `drivers[].options.reasoning_field` para cada driver montado que serve o modelo (spec 007): valor fora da lista, ou diferente de `hidden` num driver `openai`/`azure` que serve o modelo, torna a configuração inválida.

**Atrasos de falha de transporte.** Em qualquer perfil de erros, `timeout_delay` (padrão 30 s) atrasa o `gateway_timeout`, `hang_duration` (padrão 10 s) o `stream_hang` e `slow_headers_delay` (duração Go, padrão 15 s) o `slow_headers`; 0 ou ausente = padrão e valor negativo torna a configuração inválida ([ADR 0058](decisions/0058-lote-do-painel-por-conta-e-achados-de-spec.md)).

`usage.extended` (booleano, padrão `false`) liga o header `x-wwz-input-breakdown` (spec 013, [ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)). `metrics.max_series` (padrão 5 000, por métrica) só é lido no boot. `metrics.vllm_mirror` (booleano, padrão `false`) publica no `/metrics` o espelho `vllm:num_requests_running`, `vllm:num_requests_waiting` e `vllm:time_to_first_token_seconds` (label `model_name`, spec 013); também só é lido no boot (recarga não o altera, mudar exige restart).

As demais seções (`drivers`, `abuse`, `metrics`, `logging`, `chaos`, `geo`, `vars`, `resilience`) seguem os tipos de `internal/config/config.go` e são detalhadas nas specs de cada área.

**Campos dos modelos**

| Campo | Obrigatório | Significado |
| --- | --- | --- |
| `id` | sim | Nome devolvido em `/v1/models` e aceito em `model`. No driver `openrouter` pode terminar em `:free` (variante gratuita, modelo próprio; spec 006, 16.4); os demais sufixos (`:nitro`, `:floor`…) não se declaram, resolvem para a base |
| `aliases` | não | Outros nomes aceitos que resolvem para este modelo |
| `version` | não | Nome do snapshot que o real devolve em `model` (ex.: `id: gpt-4o-mini`, `version: gpt-4o-mini-2024-07-18`). Nos drivers `openai`, `azure` e `anthropic`, o `model` de toda resposta é `version` ou, sem ele, `id`, nunca o alias pedido; no `openrouter`, vira o `canonical_slug` da listagem, com a mesma regra de slug do `id`. Não aparece como item próprio em `/models`: para listar o snapshot, declare outro modelo com esse `id` (spec 007, [ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)) |
| `deprecated_at`, `retires_at` | não | Datas `YYYY-MM-DD` do ciclo de vida. `retires_at` sai como `shutdown_date` (OpenAI), `retires_at` (Anthropic) e `expiration_date` (OpenRouter); `deprecated_at` sai na Anthropic, que também deriva `lifecycle`. A partir de `retires_at`, o modelo responde como inexistente. `retires_at` anterior a `deprecated_at` torna a configuração inválida (spec 006, 16.7, [ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)) |
| `protocols` | não | Lista de `openai`, `anthropic` e `gemini` (ou `["*"]` = os três); ausente ou `[]` vale `["*"]` ([ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md)): os dialetos de wire em que o modelo é servido. Rotas OpenAI (chat, completions, responses) dos drivers `openai`, `azure`, `openrouter` e `opencode`; rotas Messages de `anthropic`, `openrouter` e `opencode`; rotas Gemini de `gemini` e `opencode` (spec 005). O dialeto vem do tipo da rota, não do nome do driver. Nos drivers diretos (`openai`, `azure`, `anthropic`, `gemini`), chamar pelo dialeto errado devolve o erro de modelo inexistente do driver. O `openrouter` e o `opencode` normalizam como o real: um modelo do catálogo deles atende em todas as rotas, qualquer que seja `protocols` ([ADR 0048](decisions/0048-dialetos-catalogos-e-templates.md) para o `openrouter`, [ADR 0062](decisions/0062-opencode-serve-todo-modelo-em-toda-rota.md) para o `opencode`; o real serve até `gpt-*` na rota `google`). Valores antigos `openrouter`/`opencode` (nomes de driver) são aceitos com aviso e traduzidos: a lista inteira vira `drivers` e os dialetos dela (`openai`, `anthropic`, `gemini`; nenhum → `openai`) viram `protocols`; outro valor torna a configuração inválida |
| `drivers` | não | Nomes de driver (`openai`, `azure`, `anthropic`, `gemini`, `openrouter`, `opencode`) em cujo catálogo o modelo aparece (listagem `/v1/models` do driver e aceitação em `model`). Ausente = todos os drivers que falam pelo menos um dos `protocols`. Nome desconhecido, ou driver que não fala nenhum dos `protocols` (exceto `openrouter`), torna a configuração inválida. Modelo fora do catálogo do driver → erro de modelo inexistente do driver |
| `display_name`, `owned_by`, `created` | não | Metadados de listagem (Anthropic usa `display_name` e `created_at`) |
| `context_window` | não | Padrão 128 000; excedido → 400 `context_length_exceeded` |
| `max_output` | não | Teto de saída do modelo (padrão 16 384); listado como `max_tokens` (Anthropic), `limit.output` (OpenCode) e `top_provider.max_completion_tokens` (OpenRouter) |
| `pricing` | não | `input_per_m`, `output_per_m`, `cache_read_per_m`, `cache_write_per_m` em USD por milhão de tokens; alimentam `cost` e a listagem de preços (OpenRouter por token, OpenCode por milhão). `multipliers` e `overrides` (ADR 0072, abaixo) mudam o preço por horário, data ou tamanho de contexto em todo driver. No `opencode`, `pricing` declarado com `input_per_m: 0` (ou sem ele) torna o modelo **gratuito** naquele catálogo (spec 006 16.3); `pricing` ausente continua pago a US$ 0 |
| `catalogs` | não | Só modelos do `opencode` (ADR 0072): mapa com as chaves `opencode` (bases `/inference` e `/zen`) e `opencode-go` (bases `/inference/go` e `/zen/go`), cada uma `{pricing?, allowance_factor?, context_window?, max_output?, capabilities?, extra?}`. O modelo só existe nos catálogos listados. Em cada um vale o **modelo efetivo do catálogo**: `pricing` substitui o bloco inteiro do modelo; `context_window` e `max_output` (inteiros > 0) substituem os do modelo; `capabilities` substitui campo a campo; `extra` é mesclado por cima de `models[].extra` (objetos chave a chave, listas e escalares substituem, `null` tira o campo do item). O modelo efetivo vale para a listagem, o 400 `context_length_exceeded`, o teto de saída, as recusas por capacidade e o preço nas bases daquele catálogo. O protocolo nativo por catálogo se declara em `extra.api` do catálogo (as chaves não nulas do `extra.api` efetivo; não existe `catalogs.<c>.protocols`). `allowance_factor` (≥ 0, só em `opencode-go`) é a razão `opencode.allowance / list_cost` (padrão 2 nos `gemini-*` nativos, 0,6 nos demais). Ausente = os dois catálogos com o modelo como declarado. Catálogo desconhecido, modelo fora do catálogo do `opencode`, `allowance_factor` em `opencode`, `context_window`/`max_output` ≤ 0 ou chave de `extra` fora dos dialetos do modelo: `config: models[i].catalogs.<c>...: ...` (revisão da task 434 na ADR 0072) |
| `capabilities` | não | `tools`, `reasoning`, `vision`, `json_schema`; capacidade ausente → 400 como o provedor faria |
| `latency`, `errors`, `generation` | não | Nome de um perfil ou objeto inline; padrão = perfis `default` |
| `scenarios` | não | Lista de cenários candidatos (seção 9); sem casamento → `generation` |
| `tokenizer` | não | `auto` (padrão), `simulated`, `tiktoken:o200k_base` ou `tiktoken:cl100k_base`; outro valor torna a configuração inválida. `auto` escolhe pelo `version`, `id` e aliases (sem o prefixo de autor): família OpenAI atual (`gpt-4o*`, `chatgpt-4o*`, `gpt-4.1*`, `gpt-4.5*`, `gpt-5*`, `o1*`, `o3*`, `o4*`, `gpt-oss*`) → `o200k_base`; `gpt-4` e todo `gpt-4-*` (`gpt-4-turbo`, `gpt-4-0613`, `gpt-4-32k`, `gpt-4-1106-preview`; `gpt-4.1*` e `gpt-4o*` são `o200k_base`), `gpt-3.5-turbo*`, `text-embedding-*` → `cl100k_base`; modelo servido no dialeto `gemini` → o tokenizador calibrado do Gemini (`gtok`, [ADR 0063](decisions/0063-lote-da-rodada-6-b-cachedcontents-tools-no-cache-e-endpoint.md)); os demais (sem tokenizador público) → `simulated`. A contagem exata do real (com o overhead do template, spec 007) exige o encoding do modelo real; num modelo de nome próprio (`wwz-fast`) que imita um OpenAI, use `tiktoken:o200k_base` ([ADR 0053](decisions/0053-tokenizador-bpe-e-ordem-chave-corpo.md)) |
| `extra` | não | Campos da listagem `/models` específicos do dialeto que o wwz não deriva (ex.: `capabilities` da Anthropic, `description`/`family`/`modalities` do OpenCode, `benchmarks`/`knowledge_cutoff` do OpenRouter), com a forma real de spec 006, 16.7. O driver põe cada chave na posição real; chave que nenhum dos dialetos do modelo tem torna a configuração inválida. Campo sem valor sai derivado dos demais campos ou com o valor neutro observado ([ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)). No `opencode`, uma chave com `null` tira o campo derivado do item (ex.: `canonical_id: null`), e as chaves de `extra.api` definem o protocolo do item (ex.: `api: {responses: {}}`; ADR 0072, item 16) |
| `prompt_cache` | não | Regras de cache de prompt do modelo real que este modelo imita: `min_tokens` (≥ 0), `granularity` (≥ 0; 0 = fronteira exata), `kind` (`automatic`, `explicit`, `none`); ausente = padrão do driver para a geração atual de modelos (spec 007, [ADR 0046](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)) |

**Preço por horário e por contexto ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)).** `pricing.multipliers` é uma lista de `{when, factor}` (`factor` > 0 e finito): o preço da requisição é multiplicado por todo `factor` cujo `when` casa no início dela (UTC). `pricing.overrides` é uma lista de `{when, input_per_m?, output_per_m?, cache_read_per_m?, cache_write_per_m?}` com pelo menos um preço: o primeiro que casa substitui os preços que declara. `when` usa a gramática do real (spec 006, 16.7): `utc_days: {in: [mon, tue, wed, thu, fri, sat, sun]}`; `utc_hours: {gte|gt|lte|lt: "HH:MM"}`; `date: {gte|gt|lte|lt: "YYYY-MM-DD"}`; `context: {gte|gt|lte|lt: <tokens>}` (só em `overrides`: tokens de entrada da requisição, cache incluído); `any: [<when>…]` (casa se um casar) e `all: [<when>…]` (casa se todos casarem); chaves de topo combinam com E. Condição desconhecida, operador desconhecido, hora ou data malformada, ou `context` em `multipliers` tornam a configuração inválida (`config: models[i].pricing.multipliers[j].when: unknown condition "x"`). O preço efetivo vale para `cost`, `list_cost`, débito de saldo, `spend` e cota.

```yaml
# OpenCode (ADR 0072): planos, Extra Usage, membro, service account, budgets e catálogos
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
      spend_monthly_usd: 40                                   # budget do workspace (coverage all)
      spend_monthly_models_usd: {opencode/claude-opus-5-5: 10}  # budget por modelo (coverage models)
projects:
  - id: ana                         # membro do workspace
    limits: {spend_monthly_usd: 15}
keys:
  - {key: oc_sk_ana, account: acme, project: ana}
  - {key: oc_sk_ci, account: acme, service_account: true}
drivers:
  - name: opencode
    options: {free_requests_per_day: "200", console_key: key-1}
models:
  - id: big-pickle
    drivers: [opencode]
    pricing: {input_per_m: 0, output_per_m: 0}   # declarado com entrada 0 = gratuito
    catalogs: {opencode: {}}                     # só no pay-as-you-go
  - id: deepseek-v4.1-flash
    drivers: [opencode]
    pricing:
      input_per_m: 0.3
      output_per_m: 1.2
      cache_read_per_m: 0.006
      multipliers:
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
```

**Campos por catálogo (ADR 0072, revisão da task 434).** Os ids presentes nos dois catálogos do real diferem em mais que o preço (`testdata/real/opencode/catalog/`). O trecho abaixo reproduz três deles; complementa o bloco anterior e usa os campos de `models[].catalogs` acima.

```yaml
# OpenCode: campos por catálogo (ADR 0072, revisão da task 434)
models:
  - id: minimax-m3                  # outro protocolo nativo e outro limite no Go
    drivers: [opencode]
    protocols: [openai]             # chat-completions no pay-as-you-go
    context_window: 512000
    max_output: 128000
    pricing: {input_per_m: 0.3, output_per_m: 1.2, cache_read_per_m: 0.06}
    extra: {family: minimax, capabilities: {cache: {implicit: true}}}
    catalogs:
      opencode: {}
      opencode-go:
        context_window: 1000000
        max_output: 131072
        pricing:                    # o Go cobra o dobro acima de 512k de contexto
          input_per_m: 0.3
          output_per_m: 1.2
          cache_read_per_m: 0.06
          overrides:
            - {when: {context: {gt: 512000}}, input_per_m: 0.6, output_per_m: 2.4, cache_read_per_m: 0.12}
        extra:
          api: {messages: {}}       # messages no Go
          family: minimax-m3
          knowledge: "2025-01"
          capabilities: {cache: null}
  - id: deepseek-v4-pro             # outro nome e outra descrição no Go
    drivers: [opencode]
    pricing: {input_per_m: 1.74, output_per_m: 3.48, cache_read_per_m: 0.145}
    extra:
      description: Open MoE flagship with million-token context for coding and long agent runs
      capabilities: {reasoning: {toggle: true, effort: [high, max]}, cache: {implicit: true}}
    catalogs:
      opencode: {}
      opencode-go:
        pricing:
          input_per_m: 0.66
          output_per_m: 1.98
          cache_read_per_m: 0.022
          multipliers:
            - when: {utc_days: {in: [mon, tue, wed, thu, fri]}, any: [{all: [{utc_hours: {gte: "01:00"}}, {utc_hours: {lt: "04:00"}}]}, {all: [{utc_hours: {gte: "06:00"}}, {utc_hours: {lt: "10:00"}}]}]}
              factor: 2
        extra:
          name: DeepSeek V4 Pro (New)
          description: Flagship DeepSeek model for coding, reasoning, and agentic work
          capabilities: {cache: null, system_updates: false, reasoning: {toggle: null}}
  - id: big-pickle                  # cache.write 0 explícito, só por extra
    drivers: [opencode]
    pricing: {input_per_m: 0, output_per_m: 0}
    extra: {canonical_id: null, cost: {text: {input: {cache: {write: 0}}}}}
    catalogs: {opencode: {}}
```

**Contas do OpenCode ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)).** `tiers.<nome>.plan` é o plano de assinatura que o tier representa no `opencode`: `go`, `go-plus`, `go-teams` ou `none` (sem Go); ausente = `go`; outro valor torna a configuração inválida (`config: tiers.<nome>.plan: unknown plan "x"`). Os outros drivers ignoram o campo. `plan` não vai a `cfg:policies` (`tier:<nome>` só carrega limites). `accounts[].extra_usage` (booleano; padrão `true` com plano `go-teams`, `false` nos demais) liga a cobrança no saldo quando a cota Go acaba. O membro do workspace é o projeto da chave (`keys[].project`; sem projeto, a própria chave).

**Recarga.** `SIGHUP` ou `POST /admin/config` recarregam modelos, perfis e chaves sem derrubar conexões abertas; requisições em andamento terminam com a configuração antiga. Configuração inválida aborta o boot e, na recarga, é recusada por inteiro (a anterior continua). Arquivo de cenário inválido é logado e pulado, salvo com `wwz serve --validate-on-load` (env `WWZ_VALIDATE_ON_LOAD=1`, I23): aí o boot roda as mesmas verificações de `wwz validate` (configuração e todos os cenários) e sai com código 1 em qualquer erro, e a recarga com um cenário inválido é recusada por inteiro ([ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)). A verificação relê todos os arquivos de cenário do disco a cada troca de configuração, qualquer que seja a origem: `SIGHUP`, `POST /admin/config` (corpo vazio ou merge patch, spec 013) e as políticas do cluster aplicadas a partir de `cfg:policies` ([ADR 0025](decisions/0025-politicas-e-tier-por-entidade.md)). Por isso, com a flag ligada, um cenário quebrado no disco bloqueia também as trocas de tier e de política (a versão é rejeitada e a configuração anterior continua) até o arquivo ser corrigido.

**Precedência por requisição (I3, [ADR 0026](decisions/0026-precedencia-chave-sobre-modelo.md)).** Latência, erros e `seed`: header `x-wwz-*` > passo de cenário > chave (`keys[].latency`, `keys[].errors`) > modelo > perfil `default`. Exceção: `errors.outages` soma os níveis em vez de substituí-los (spec 009).

**Variáveis de ambiente.** `WWZ_CONFIG` (arquivo YAML), `WWZ_LISTEN` (→ `server.listen`), `WWZ_PORT` (atalho para `server.listen: ":<porta>"`, vence `WWZ_LISTEN`), `WWZ_ADMIN_LISTEN` (→ `admin.listen`), `WWZ_ADMIN_PORT` (atalho para `admin.listen`), `WWZ_ADMIN_TOKEN` (bearer de `/admin/*`), `WWZ_SEED`, `WWZ_AUTH`, `WWZ_REUSEPORT` (`1`/`true`), `WWZ_LOG_LEVEL` (→ `logging.level`), `WWZ_SCENARIOS_DIR` (→ `scenarios.dir`), `POD_NAME`/`POD_IP` (adicionam `x-wwz-pod` à resposta para identificar a instância atrás do balanceador), `WWZ_STORE` (→ `store.kind`), `WWZ_VALKEY_ADDR` (lista separada por vírgula → `store.addr`; força `store.kind: valkey`, salvo se `WWZ_STORE` estiver definida), `WWZ_VALKEY_PASSWORD` (→ `store.password`), `WWZ_VALIDATE_ON_LOAD` (`1`/`true`, mesmo efeito de `-validate-on-load`). Flags de `wwz serve` com o mesmo papel: `-config`, `-listen`/`-port`, `-admin-listen`/`-admin-port`, `-seed`, `-auth`, `-scenarios-dir`, `-log-level`, `-log-format`, `-validate-on-load`. Nomes do store: [ADR 0020](decisions/0020-nomes-config-store.md); chaves e scripts: [cache/valkey/README.md](../cache/valkey/README.md).

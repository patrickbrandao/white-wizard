# 013 — Observabilidade e API administrativa

Status: rascunho
Depende de: 001, 003
Origem: seções 10 do documento Claude Docs congelado em 2026-10-30

## Contexto
Métricas Prometheus, API admin, logs, tracing e dashboard embutido (seção 10). A autenticação da API admin foi decidida depois do documento original: ver abaixo.

## Requisitos
- R13, R14, R23
- RNF: `/admin/*` exige `Authorization: Bearer $WWZ_ADMIN_TOKEN`; `/metrics`, `/health`, `/health/ready` abertos; sem token configurado, o control plane só escuta em 127.0.0.1 (ADR 0004). Substitui o "sem autenticação por padrão" da tabela abaixo.

## Interfaces e contratos
- Contrato do control plane: [`cmd/wwz/openapi.yaml`](../cmd/wwz/openapi.yaml) (OpenAPI 3.1). Além das rotas da tabela da seção 10, descreve as citadas em 009, 012 e 017 (`/admin/chaos/next`, `/admin/entities/{tipo}/{id}` com `block`/`unblock`/`ban`/`unban`/`credits`, `/admin/leak`, `/admin/resilience`, `/admin/conversations/reset`) e, para operação, `GET`/`DELETE /admin/chaos` e `GET /admin/entities?status=`.
- `POST /admin/config` com JSON é merge patch (RFC 7396) em que listas nomeadas viram mapa (`models` pelo campo `id` do YAML, `keys` por `label`, `drivers` por `mount`); com corpo vazio relê o arquivo, como o SIGHUP. Em erro de validação nada muda (I23). (O modelo não tem campo `name`; a descrição "`models` por `name`" do openapi é corrigida para `id` na task 041.)
- `/dashboard` é HTML estático aberto; os dados vêm de `/admin/stats` com o token digitado pelo operador.
- Rotas por entidade da [ADR 0025](decisions/0025-politicas-e-tier-por-entidade.md), acrescentadas ao contrato na task 041:
  - `POST /admin/entities/{tipo}/{id}/tier` com `{"tier": "<nome>"}` (`""`/`null` remove): `tipo = account`; `tipo = key` é resolvido para a conta da chave (chave desconhecida → 400); outros tipos → 400; tier inexistente → 400; grava `ent:account:<id>` em `cfg:policies` (com `version` +1 e `PUBLISH cfg:changed`) e o campo `tier` de `ent` da conta; devolve o `EntityState` da conta ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md)).
  - `POST /admin/entities/{tipo}/{id}/credits` aceita `{"usd": n}` (define o saldo) ou `{"add_usd": n}` (n > 0: soma ao saldo e ao acumulado `purchased`, simulando compra de créditos, e dispara a avaliação de `promote_when`); exatamente um dos dois. `EntityState` ganha `purchased_usd`. Com `tipo = key`, `add_usd` é creditado à conta da chave e a resposta é o estado da conta; `usd` age na entidade nomeada ([ADR 0058](decisions/0058-lote-do-painel-por-conta-e-achados-de-spec.md)).
  - `POST /admin/entities/{tipo}/{id}/reset` com `{"scope": ["counters", "strikes"]}` (corpo vazio = `["counters"]`): zera contadores e/ou strikes de uma entidade, preservando bloqueio, banimento, créditos e tier; devolve `EntityState`.
  - Ambas: 401 sem token, 400 para tipo/corpo inválido, 503 quando o store não suporta a operação ou o circuito do Valkey está aberto.
- Métricas novas da ADR 0025: `wwz_policies_version` (gauge, última versão de `cfg:policies` aplicada; 0 sem políticas) e `wwz_policies_errors_total` (counter, políticas recusadas na validação).

## Dados
- `/admin/requests` mantém só as últimas N requisições em memória

## Critérios de aceite
- [ ] `/admin/*` sem token devolve 401
- [ ] Todas as rotas do control plane descritas no openapi.yaml
- [ ] `tier` e `reset` por entidade respondem conforme a ADR 0025 nos dois stores
- [ ] Recusa da verificação estrita do cliente incrementa `wwz_client_verification_failures_total{driver,reason}` e `wwz_requests_total{status="403"}` e publica o evento `error` com `error: forged_request` e `fields.reason` ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md))
- [ ] Com os baldes padrão, um conjunto de E2E cujo máximo é 1 412 ms dá p99 ≤ 1 500 ms em `/admin/stats`, e nenhum percentil passa do teto derivado de `_sum`

## Fora de escopo
- Armazenamento de longo prazo de métricas (fica no Prometheus)
- Edição de limite arbitrário por entidade (ADR 0025)

## Decisões relacionadas
- [specs/decisions/0004-autenticacao-admin.md](decisions/0004-autenticacao-admin.md)
- [specs/decisions/0006-contratos-openapi.md](decisions/0006-contratos-openapi.md)
- [specs/decisions/0025-politicas-e-tier-por-entidade.md](decisions/0025-politicas-e-tier-por-entidade.md)
- [specs/decisions/0027-formas-finais-dos-recursos-importados.md](decisions/0027-formas-finais-dos-recursos-importados.md)
- [specs/decisions/0050-baldes-de-latencia-e-percentis-do-admin-stats.md](decisions/0050-baldes-de-latencia-e-percentis-do-admin-stats.md)
- [specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### 10. Observabilidade e API administrativa (R13, R14)

Tudo que o simulador decide (latência sorteada, erro injetado, passo de cenário) é exposto em métricas e headers, para que a comparação com as métricas do roteador seja exata.

**Métricas Prometheus** (`/metrics`). Cada métrica tem só os labels listados na tabela (os que o engine publica); nenhuma leva `protocol` nem `key_label`, e `model` só aparece onde indicado, para manter a cardinalidade dentro de `metrics.max_series`. O label `model` é sempre o `models[].id` do modelo resolvido, nunca o alias, o slug do OpenRouter nem o sufixo `-scenario-<nome>`; requisição cujo modelo não resolve (404 `model_not_found`) leva `model=""`, para que o texto do cliente não crie séries ([ADR 0040](decisions/0040-label-de-modelo-e-get-de-modelo-openrouter.md)). O mesmo `models[].id` vale para `/admin/stats`, o log e o `model` dos eventos do `ev:stream` (no evento `error` de modelo não resolvido, o nome pedido):

| Métrica | Tipo | Labels | O que mede |
| --- | --- | --- | --- |
| `wwz_requests_total` | counter | `driver`, `model`, `endpoint`, `status` | Requisições de inferência atendidas, por código HTTP |
| `wwz_requests_in_flight` | gauge | `driver` | Requisições em atendimento agora |
| `wwz_streams_open` | gauge | `driver` | Respostas em streaming abertas agora |
| `wwz_errors_injected_total` | counter | `driver`, `kind`, `phase` | Erros injetados; `kind` é o tipo do erro (ex.: `rate_limit`, `overloaded`), `phase` é `pre` ou `mid` |
| `wwz_rate_limited_total` | counter | `driver`, `limit` | Recusas por limite; `limit` é o nome do limite que recusou (`rps`, `rpm`, `rpd`, `tpm`, `itpm`, `otpm`, `tpd`, `concurrency`, `ramp`, `credits`, `quota_weekly`, `quota_monthly`, `spend_monthly`, `block`, `store_down`) |
| `wwz_blocked_total` | counter | `driver`, `status` | Recusas por conta banida ou suspensa; `status` é o tipo do erro (`account_banned`, `account_suspended`) |
| `wwz_client_verification_failures_total` | counter | `driver`, `reason` | Recusas da verificação estrita do cliente (`client_verification: claude-code`, spec 020); `reason` é um dos 15 códigos da spec 020. A requisição também conta em `wwz_requests_total` com `status="403"` |
| `wwz_ttft_seconds` | histogram | `driver`, `model` | TTFT efetivo (sorteado + fator de carga) |
| `wwz_itl_seconds` | histogram | `driver`, `model` | Inter-token efetivo |
| `wwz_e2e_seconds` | histogram | `driver`, `model` | Duração total da requisição |
| `wwz_overhead_seconds` | histogram | `driver` | Tempo de processamento do engine, sem a latência simulada |
| `wwz_tokens_total` | counter | `driver`, `model`, `kind` | Tokens por `kind` (`prompt`, `completion` sem raciocínio, `reasoning`, `cached`) |
| `wwz_tool_calls_total` | counter | `driver`, `tool` | Tool calls emitidas, por nome da ferramenta |
| `wwz_scenario_steps_total` | counter | `scenario`, `step` | Passos de cenário servidos |
| `wwz_store_circuit_open` | gauge | — | 1 enquanto o Valkey está inacessível e vale `store.on_down` |
| `wwz_policies_version` | gauge | — | Última versão de `cfg:policies` aplicada (0 sem políticas; ADR 0025) |
| `wwz_policies_errors_total` | counter | — | Versões de `cfg:policies` recusadas na validação |
| `wwz_metrics_series_dropped_total` | counter | — | Séries descartadas pelo teto `metrics.max_series` |
| `wwz_queue_depth` | gauge | `driver` | Requisições esperando slot: as que esperam em `on_limit: queue` (durante a espera) e as admitidas com a `concurrency` de latência do modelo já cheia, dentro de `max_queue` (contadas até o fim da requisição; aproximação, ver abaixo) |
| `wwz_connections_open` | gauge | — | Conexões TCP abertas no data plane, somadas entre todos os listeners (contadas pelos estados de conexão, certo com vários `SO_REUSEPORT`) |
| `vllm:num_requests_running` | gauge | `model_name` | Espelho vLLM (só com `metrics.vllm_mirror`): requisições admitidas dentro da `concurrency` do modelo |
| `vllm:num_requests_waiting` | gauge | `model_name` | Espelho vLLM (só com `metrics.vllm_mirror`): as mesmas requisições de `wwz_queue_depth`, por modelo |
| `vllm:time_to_first_token_seconds` | histogram | `model_name` | Espelho vLLM (só com `metrics.vllm_mirror`): o mesmo valor de `wwz_ttft_seconds` (fila simulada + TTFT), buckets de `metrics.buckets` |

**Fila e espelho vLLM.** `wwz_queue_depth` é uma aproximação da fila de um servidor real: o wwz não segura a requisição até abrir um slot, ele a admite (com `max_queue` > 0, a que passa de `concurrency + max_queue` é rejeitada) e simula a espera na latência; por isso a requisição que entrou com a `concurrency` cheia conta como esperando até terminar, e não passa a "running" no meio. Sem `concurrency` no perfil de latência, só a espera de `on_limit: queue` conta. O espelho `vllm:*` existe para ferramentas que já leem os nomes do vLLM: com `metrics.vllm_mirror: true` o `/metrics` publica as três séries acima; com `false` (padrão), nenhuma série `vllm:*`. O label `model_name` é o mesmo `models[].id` do label `model` ([ADR 0040](decisions/0040-label-de-modelo-e-get-de-modelo-openrouter.md)). `metrics.vllm_mirror` é lido só no boot, como `metrics.max_series`: recarga não liga nem desliga o espelho. `wwz simulate` não publica o espelho.

**Teto de séries (I22).** `metrics.max_series` (padrão 5 000) limita as combinações de labels de **cada métrica** separadamente; uma série nova acima do teto é descartada e contada em `wwz_metrics_series_dropped_total`. O valor é lido só no boot: recarga (SIGHUP, `POST /admin/config`) não o altera, mudar exige restart.

**Baldes e percentis ([ADR 0050](decisions/0050-baldes-de-latencia-e-percentis-do-admin-stats.md), provisória).** Os histogramas de latência (`wwz_ttft_seconds`, `wwz_itl_seconds`, `wwz_e2e_seconds` e o espelho `vllm:*`) usam `metrics.buckets`; o padrão (`metrics.DefaultBuckets`, também usado por `wwz_tools_call_duration_seconds` do `wwz-tools`) segue a série 1–1,5–2–3–5–7,5 por década de 1 ms a 10 s, mais 15, 20, 30 e 60 s (29 limites); `wwz_overhead_seconds` tem baldes próprios de 0,1 ms a 100 ms. `metrics.buckets` é lido só no boot. Não há métrica de máximo: o máximo exato vem do `wwz-load` (spec 015). Os percentis p50/p99 de TTFT, ITL e E2E em `GET /admin/stats` são estimados dos baldes do período (desde o boot ou o último `POST /admin/reset`): interpolação linear dentro do balde que contém o posto, como o `histogram_quantile` do Prometheus (posto no primeiro balde interpola a partir de 0; posto no balde `+Inf` devolve o maior limite finito), limitada por um teto do maior valor observado derivado de `_sum` (cada observação vale ao menos o limite inferior do seu balde, então o máximo é no máximo `_sum` menos os limites inferiores das demais). O erro da estimativa é de até um balde. Abaixo do primeiro limite (1 ms com os baldes padrão) o teto de `_sum` vale como em qualquer balde e é o esperado: TTFT e ITL registram o valor sorteado pelo plano (fila + TTFT, ITL), então com latência zero toda observação é 0, `_sum` é 0 e p50/p99 saem exatamente 0; o E2E é medido no relógio e, com observações reais abaixo de 1 ms, sai entre 0 e o menor de `_sum` e da interpolação no primeiro balde, dentro do erro de um balde. Não se devolve o meio do primeiro balde, que inventaria 0,5 ms onde nada foi observado (task 197).

**Decomposição da entrada (I16, [ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)).** Com `usage.extended: true` (padrão `false`, recarregável), toda resposta de inferência leva o header `x-wwz-input-breakdown` com JSON compacto dos tokens de entrada por origem: `{"system":N,"tool_schemas":N,"user":N,"assistant":N,"tool_calls":N,"tool_results":N,"images":N,"cached":N,"total":N}` (`total` é a contagem de entrada que alimenta `usage` e os limites, antes de descontar o cache; `cached` é a parte lida do cache). O corpo e o `usage` do provedor não mudam (ADR 0014). O painel exibe a mesma decomposição a partir do evento `request` do `ev:stream`.

**API administrativa** (porta separada opcional, `admin_port`, sem autenticação por padrão em rede confiável):

| Chamada | Efeito |
| --- | --- |
| `GET /admin/config` | Configuração efetiva, já com sobrescritas e com os `tier:<nome>` publicados em `cfg:policies` (spec 012). O tier atribuído a uma conta (`ent:*`, `POST .../tier`) não é gravado nela: lê-se em `tier` do `GET /admin/entities/{tipo}/{id}` ([ADR 0058](decisions/0058-lote-do-painel-por-conta-e-achados-de-spec.md)) |
| `POST /admin/config` | Patch parcial: `{"models": {"gpt-5-sim": {"errors": {"rate": 30}}}}` aplica na hora, sem reiniciar |
| `POST /admin/reset` | Zera contadores de rate limit e métricas |
| `GET /admin/stats` | JSON com os mesmos números das métricas, por chave e modelo; percentis estimados dos baldes (ver "Baldes e percentis") |
| `GET/POST/DELETE /admin/scenarios` | Lista, carrega (corpo YAML) e remove cenários |
| `POST /admin/chaos` | Liga um evento pontual: `{"type": "overloaded", "duration": "30s", "models": ["*"]}` |
| `GET /admin/requests?last=100` | Últimas N requisições com decisões tomadas (modo `debug`) |

**Logs.** JSON estruturado (`slog`), um registro por requisição com `request_id`, modelo, chave, status, cenário/passo, latências sorteadas, erro injetado, bytes. Registro com erro leva `error` (tipo) e `chaos` com a camada que decidiu o erro ([009](009-erros-e-caos.md) "Camadas"): `profile` (perfil global, de modelo ou de chave), `step` (passo de cenário), `header` (headers `x-wwz-chaos-*` mudaram o perfil resolvido) ou `admin` (`POST /admin/chaos` ou `/admin/chaos/next`); vazio quando o erro não veio do caos (limite, validação). Os nomes de campos do log são informativos, não contrato público versionado. Nível `debug` inclui corpo da requisição (sem a chave). `log_sample: 0.01` registra 1 % sob carga.

**Cliente falsificado ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)).** A recusa do modo estrito (spec 020) gera o registro de log da requisição com `error: forged_request`, `client_verification_reason` (o `reason`) e `client_verification_detail` (o detalhe, até 200 bytes), e o evento `error` do `ev:stream` com `error: forged_request`, `status: 403` e `fields.reason` = o `reason` (sem `denied`, que é só de limite; contrato em [asyncapi.yaml](../eventbus/valkey/asyncapi.yaml)). Nada vai ao store além do evento.

**Tracing.** OpenTelemetry opcional (`otel_endpoint`), um span por requisição com os mesmos atributos; propaga `traceparent` recebido do roteador.

**Dashboard.** `GET /dashboard` com página HTML estática lendo `/admin/stats` a cada 2 s: req/s, erros por tipo, streams abertos, histograma de TTFT. Painel Grafana pronto em `deploy/grafana/`.

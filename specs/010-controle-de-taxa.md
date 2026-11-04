# 010 — Controle de taxa

Status: rascunho
Depende de: 005, 012
Origem: seções 8 do documento Claude Docs congelado em 2026-10-30; reescrita na task 168 conforme o código e a spec 012 ([ADR 0048](decisions/0048-dialetos-catalogos-e-templates.md))

## Contexto
Limites de taxa, cota e concorrência por entidade (IP, chave, usuário, projeto, conta), modos de aplicação, comportamento sob limite, headers por dialeto e a proteção global `server.max_rps` (seção 8). O estado vive no `limits.Store` (`memory` ou `valkey`, spec 012), então os limites valem para o cluster inteiro com `store.kind: valkey`.

## Requisitos
- R10

## Interfaces e contratos
- `internal/limits` (`Store`)

## Dados
- Contadores em [012](012-estado-valkey.md)

## Critérios de aceite
- [ ] 429 com headers `x-ratelimit-*`/`anthropic-ratelimit-*` corretos ao estourar
- [ ] `store: memory` e `store: valkey` passam a mesma suíte (`internal/limits/storetest`), inclusive `mode`, `on_limit: queue` e `algorithm`
- [ ] Com `store: valkey`, duas instâncias somam o mesmo limite (o limite é do cluster, não da instância)
- [ ] Duas chaves da mesma conta dividem os limites do tier da conta (o 2º pedido acima do `rpm` da conta é recusado, qualquer que seja a chave); `keys[].limits` e `projects[].limits` também recusam, como tetos próprios
- [ ] `POST .../credits` com `add_usd` em `tipo = key` credita a conta da chave (saldo e `purchased`) e avalia `promote_when` dela; chave sem conta → 400
- [ ] Headers de limite mostram a entidade mais restritiva (menor `remaining`) por medida
- [ ] Conta com `promote_when` satisfeito passa a `next` em todas as instâncias (`store: valkey`) sem reinício, um passo por vez, nunca rebaixa
- [ ] `server.max_rps` excedido responde o erro de sobrecarga no dialeto do driver com `retry-after` ≥ 1 s
- [ ] `opencode` ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)): na base Go a requisição debita só a cota Go, pelo `allowance`, e nunca saldo nem budget; na base pay-as-you-go debita só saldo e budgets, nunca a cota Go; gratuito não debita nada e conta na cota diária (IP sem chave, conta com chave); cota por modelo (`quota_usd.models`) e budget por modelo (`spend_monthly_models_usd`) negam sem consumir os demais; com `extra_usage`, a cota estourada cai no saldo; no Go Teams a cota conta no membro; memory e valkey passam a mesma suíte

## Fora de escopo
- Cobrança real

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0020-nomes-config-store.md](decisions/0020-nomes-config-store.md), [0021](decisions/0021-chaves-valkey-e-cluster.md), [0025](decisions/0025-politicas-e-tier-por-entidade.md)
- [specs/decisions/0042-fidelidade-total-ao-provedor-real.md](decisions/0042-fidelidade-total-ao-provedor-real.md)
- [specs/decisions/0048-dialetos-catalogos-e-templates.md](decisions/0048-dialetos-catalogos-e-templates.md)
- [specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md](decisions/0051-limites-na-conta-e-promocao-de-tier.md)
- [specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)

## Comportamento

### 8. Controle de taxa (R10)

O simulador impõe limites por entidade e devolve o erro que o provedor imitado devolveria, no dialeto dele; é o mecanismo que exercita retry, backoff e failover do roteador. Tudo passa por `limits.Store.Check` na entrada e `Finish` na saída (spec 012); o data plane não guarda contador próprio.

**Onde os limites são declarados** ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md)). Num bloco `limits` em `tiers.<nome>.limits`, `accounts[].limits`, `projects[].limits` e `keys[].limits` (spec 003). O tier pertence à **conta** (`accounts[].tier`, ou `keys[].tier` das chaves da conta quando ela não declara tier). Efetivo de cada entidade:
- Conta: `accounts[].limits` > tier da entidade `account` em `cfg:policies` (atribuído ou promovido) > `accounts[].tier` / `keys[].tier` > `default`.
- Projeto: só `projects[].limits`. Chave: só `keys[].limits` (teto da chave, não sobe o da conta).

**Em que entidade contam.** Como no real, os limites são da organização, nunca da chave: OpenAI limita org e projeto, Anthropic org e workspace ("Organization-wide limits always apply"), OpenRouter "govern[s] capacity globally" (chaves extras não mudam o limite; teto de gasto opcional por chave) e OpenCode Go cobra a cota na assinatura do workspace (fontes na ADR 0051). Cada driver declara em `LimitPolicy.Entities` os níveis que o provedor real tem (spec 005, seção 17): `openai` e `anthropic` → `ip`, `key`, `project`, `account`; `openrouter` → `ip`, `key`, `account`; `opencode` → `ip`, `key`, `user`, `project` (o membro do workspace), `account` ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)).
- Todos os limites do tier (`rps`, `rpm`, `rpd`, `tpm`, `itpm`, `otpm`, `tpd`, `concurrency`, `ramp`, `spend_*`, `credits_usd`, `quota_usd`) contam na `account` (`keys[].account`, padrão `acct-<label>`; as 10 chaves embutidas compartilham `acct-test`). Driver sem o nível `account` conta na chave.
- `project` (projeto OpenAI, workspace Anthropic) e `key` contam só os limites declarados para eles, como tetos adicionais; sem declaração, carregam só bloqueio e banimento, como `ip` e `user` (spec 012).
- `TPMReserve` e `ExcludeCacheRead` do driver valem em toda entidade com limite de tokens.

**Limites**

| Limite | Campo | Algoritmo | Erro ao exceder (`core.ErrorKind` → forma do driver, spec 009) | `retry-after` |
| --- | --- | --- | --- | --- |
| Requisições por segundo | `rps`, `burst` (padrão = `rps`) | Token bucket | `rate_limit` | Até o próximo token, mín. 100 ms |
| Requisições por minuto | `rpm` | Token bucket contínuo (`algorithm: token_bucket`, padrão) ou janela fixa de 60 s (`algorithm: fixed_window`) | `rate_limit` | Até o próximo token / fim da janela |
| Tokens por minuto | `tpm` | Idem; reserva `prompt + max_tokens` na entrada (`tpm_reserve: max_tokens`, padrão do `openai` e do `openrouter`) ou só o prompt (`tpm_reserve: none`, padrão do `anthropic` e do `opencode`) e acerta pelo real no `Finish` | `rate_limit` | Até caber |
| Tokens de entrada por minuto | `itpm` | Token bucket; `exclude_cache_read: true` (forçado pelo `anthropic`) não conta cache read | `rate_limit` | Até caber |
| Tokens de saída por minuto | `otpm` | Token bucket; conta o gerado no `Finish`, a entrada só verifica folga | `rate_limit` | 1 s |
| Requisições / tokens por dia | `rpd`, `tpd` | Contador diário, reset à meia-noite UTC | `rate_limit` | Até a meia-noite UTC |
| Gasto mensal | `spend_monthly_usd` (bloqueia), `spend_soft_usd` (só avisa) | Soma do custo (`models[].pricing`) no mês UTC | `spend_limit` (status por driver, spec 006) | Até o próximo mês UTC |
| Créditos | `credits_usd` | Saldo debitado pelo custo | `quota` (402 no OpenRouter) | — |
| Cota em dólares em janelas | `quota_usd: {monthly_usd, rolling_5h_pct, weekly_pct, models}` | Janela deslizante de 5 h, semana UTC a partir da segunda 00:00 e mês UTC (OpenCode Go; spec 006). `monthly_usd` é a cota da entidade inteira; `models` dá a cada modelo listado uma cota própria, com as mesmas porcentagens, na entidade `model_quota` (`<dono>/<modelo>`) | `quota` | 5 h depois do gasto mais antigo; próxima segunda 00:00 UTC; dia 1º |
| Gasto mensal por modelo | `spend_monthly_models_usd: {<modelo>: US$}` | Soma do custo do modelo no mês UTC, na entidade `model_spend` (`<tipo>:<id>/<modelo>`), um por nível que declara ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)) | `spend_limit` (no `opencode`, `BudgetLimitError` com `coverage: models`) | Até o próximo mês UTC |
| Cota diária de gratuitos (`opencode`) | `drivers[].options.free_requests_per_day` | Contador diário de requisições a modelos gratuitos nas bases pay-as-you-go, na entidade `free_models` (id da conta, ou `ip:<endereço>` sem chave), reset 00:00 UTC. Fora do cliente OpenCode (`User-Agent` sem `opencode/`) a cota é zero: o driver recusa antes do `Check` e nada é contado ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md), item 5) | `quota` (`FreeUsageLimitError` 429) | Até a meia-noite UTC |
| Rampa | `ramp: {threshold_tpm, max_increase_pct, window}` | Acima de `threshold_tpm`, recusa crescimento maior que `max_increase_pct` dentro de `window` | `slow_down` (429 `slow_down` na OpenAI) | `window / 4` |
| Concorrência | `concurrency` | Semáforo na entidade | `rate_limit`, ou fila com `on_limit: queue` | 1 s |

**Limite por deployment (Azure, task 295, ADR 0065).** Os limites de contagem do tier contam no deployment (`<conta>/<modelo>`); com `tpm`, o RPM é derivado dele por família de modelo (spec 006 16.6) e o `rpm` do tier é ignorado. O RPM é avaliado em janela de 1 s (RPM ≥ 60) ou 10 s; o TPM segue no balde de 1 min.

**Origem da cobrança no `opencode` ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)).** O driver diz ao engine, por requisição, de onde sai o dinheiro, e o engine monta os sujeitos e o débito a partir disso:
- `free` (modelo gratuito no catálogo da base): sem nenhum limite em dólar e sem débito. Nas bases pay-as-you-go entra a entidade `free_models` com `rpd` = `free_requests_per_day`. Com a credencial `public`, não há chave nem conta: só `ip`, `user` e `free_models` (`ip:<endereço>`).
- `balance` (base pay-as-you-go): `credits_usd`, `spend_monthly_usd`, `spend_soft_usd` e `spend_monthly_models_usd` de conta, membro e chave; `quota_usd` não entra. O débito é o custo (`usage.cost`).
- `go`/`go-plus` (base Go): `quota_usd` (inteira e `models`) entra no **dono da cota** (a conta; no plano `go-teams`, o membro: `keys[].project`, ou a chave sem projeto). Saldo e budgets não entram. O débito da cota é o `allowance` (`list_cost × allowance_factor`), passado ao `Finish` separado do custo (`cost` = 0).
- Service account (`keys[].service_account`) na base Go sai sempre como `balance`.
- Cota Go estourada com `accounts[].extra_usage`: o engine refaz o `Check` com os sujeitos de `balance` (a primeira negação não consumiu nada) e a requisição sai como `balance`. Sem Extra Usage, a negação da cota responde (429 `GoUsageLimitError`).
- Os limites de contagem (`rps`, `rpm`, `tpm`, concorrência, `ramp`…) valem em todas as origens. Plano `none` na base Go é recusado antes do `Check` (403).

Bans e bloqueios são verificados antes de qualquer limite; o primeiro nível que nega decide a resposta, e uma negação não consome os demais (spec 012). Valores negativos tornam a configuração inválida; 0 = sem limite.

**Modos de limite.** `mode: always` (padrão), `after_n` com `after_n: N` (os limites só valem depois de N requisições da entidade), `window` com `window: {every, duration}` (os limites valem durante os primeiros `duration` de cada `every` e ficam suspensos no resto; a fase é `agora mod every` desde a época Unix, então instâncias e stores concordam sem estado) e `off`.

**Comportamento sob limite.** `on_limit: reject` (padrão) recusa na hora. `on_limit: queue` com `queue_timeout > 0` vale para `concurrency`: a requisição espera `queue_timeout / 2` (contada em `wwz_queue_depth`), tenta de novo uma vez e, se ainda não couber, recebe o erro de sobrecarga do driver (503 OpenAI, 529 Anthropic). Os demais limites sempre recusam.

**Concorrência por modelo.** Separada dos limites por entidade: `concurrency`, `max_queue` e `queue_timeout` do perfil de latência do modelo (spec 008) definem quantas requisições o modelo atende ao mesmo tempo, quando começa a fila (que entra no TTFT) e quando `concurrency + max_queue` ativos devolvem sobrecarga ("Model queue is full", `retry-after` 1 s).

**Headers.** Formatados pelo driver a partir do estado do limite (`core.RateLimitState`), na forma do real (spec 006). Por medida (requests, tokens, input, output), o estado é o da entidade com limite declarado e **menor `remaining`** entre `key`, `project` e `account`, com `limit` e `reset` dela (a Anthropic mostra "the most restrictive limit currently in effect"); com `tpm` no `project`, o `openai` acrescenta `x-ratelimit-{limit,remaining,reset}-project-tokens`.
- `openai` (e `azure`): `x-ratelimit-{limit,remaining,reset}-{requests,tokens}` em toda resposta da chave com limite, reset em duração Go (`6m0s`); `retry-after` no 429.
- `anthropic`: `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{limit,remaining,reset}`, reset RFC 3339; `retry-after` no 429 (não no spend cap).
- `openrouter`: `X-RateLimit-Limit`, `-Remaining`, `-Reset` (epoch em ms) só no 429 próprio; `retry-after` em segundos.
- `opencode`: nenhum header de limite; só `retry-after` em segundos.
- `gemini`: sem headers de limite; o 429 traz `RetryInfo` no corpo (spec 006, 16.5).

**Proteção global.** `server.max_rps` (0 = sem limite) é um token bucket local à instância, com rajada de um segundo, aplicado no data plane antes do driver: excedido, responde o erro de sobrecarga do driver da rota com `retry-after` ≥ 1 s. É proteção do processo, não simulação de provedor; para limite de cluster, use `limits` com `store.kind: valkey`.

**Store e multi-instância.** Com `store.kind: valkey` todo limite vale para o cluster inteiro (scripts Lua atômicos por entidade, hash tag `{<tipo>:<id>}`); `store.local_first: true` troca precisão por latência com buckets locais sincronizados; Valkey fora do ar segue `store.on_down: allow | deny | local` (padrão `local`). Com `store.kind: memory` (padrão), os limites valem só na instância. Detalhes, chaves e scripts na spec 012 e em [cache/valkey/README.md](../cache/valkey/README.md).

**Tiers e promoção automática** ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md)). `tiers.<nome>: {limits, promote_when, next}`. `promote_when: {purchased_usd, spend_usd, age_days, requests}` é avaliado na conta, como o real (OpenAI sobe de tier pelo total de créditos comprados; Anthropic pelo histórico de uso, inclusive a saída do Evaluation tier): todas as condições não nulas valem juntas, comparadas com os acumulados da conta no store (`purchased` = soma de `add_usd` em `POST /admin/entities/account/{id}/credits`, `spent` = gasto total, `since` = idade, `requests`). Satisfeitas, a conta passa a `next` (gravado como tier da entidade em `cfg:policies`, comparar-e-trocar, `cfg:changed`), um passo por avaliação, só para cima; avaliação depois do `finish` da conta e de cada `add_usd`; métrica `wwz_tier_promotions_total{from,to}`. `promote_when` vazio, sem `next`, `next` inexistente ou ciclo na cadeia tornam a configuração inválida. Um tier de avaliação é um tier com limites menores e `promote_when: {age_days: N}` ou `{requests: N}`. Tier por entidade e reset por entidade: spec 012 e ADRs [0025](decisions/0025-politicas-e-tier-por-entidade.md)/0051.

**Mudança de tier e baldes ([ADR 0058](decisions/0058-lote-do-painel-por-conta-e-achados-de-spec.md)).** Ao subir (ou descer) o tier, os contadores e os baldes de `rps`, `rpm`, `tpm`, `itpm` e `otpm` da conta ficam como estão: o balde mantém os tokens atuais, a capacidade e a taxa de reabastecimento passam a ser os do novo tier na próxima avaliação (um balde meio vazio não enche de uma vez; com capacidade menor, o excesso é cortado). O real pode aplicar o limite novo de imediato; isso não se confere sem subir de tier numa conta real (a OpenAI sobe por gasto acumulado e a Anthropic por compra de créditos, acima do teto de US$ 20 do ADR 0042), então fica "não conferido" na spec 006, seção "Não conferidos no real". `add_usd` numa chave compra para a conta da chave e é a conta que pode ser promovida (cria-se o saldo e o `purchased` na conta, não na chave).

**Removido.** `server.target_rps`, `server.rps_shape`, `limit_share` e `shared_limiter` do texto original nunca existiram no código: variar a capacidade no tempo é papel de `mode: window`, das janelas `errors.outages` (spec 009) e do `/admin/chaos`; o limite de cluster é o store `valkey`.

# 012 — Estado de controle em Valkey

Status: rascunho
Depende de: 001
Origem: seções 18 do documento Claude Docs congelado em 2026-10-30

## Contexto
Entidades, esquema de chaves, scripts de avaliação, política de abuso e operação do store (seção 18).

## Requisitos
- R17

## Interfaces e contratos
- Esquema de chaves: [cache/valkey/](../cache/valkey/README.md)
- Eventos `ev:stream`: [eventbus/valkey/asyncapi.yaml](../eventbus/valkey/asyncapi.yaml)

## Dados
- Contadores, bloqueios e banimentos descartáveis (sem persistência nem backup, ADR 0009)

## Critérios de aceite
- [ ] `store: memory` e `store: valkey` passam a mesma suíte de `limits.Store`
- [ ] Valkey fora do ar aplica `store.on_down` sem derrubar o data plane
- [ ] Em `store.cluster: true`, uma requisição negada por uma entidade não deixa consumo nas demais (`release`, ADR 0021)
- [ ] Políticas publicadas em `cfg:policies` + `cfg:changed` valem em todas as instâncias; versão menor ou igual à aplicada é ignorada (ADR 0025)
- [ ] Tier atribuído a uma conta (ou a uma chave, resolvido para a conta dela) muda o limite aplicado a todas as chaves da conta em todas as instâncias; reset de uma entidade não toca as demais (ADRs 0025 e 0051)
- [ ] `add_usd` soma ao saldo e a `purchased`; conta que satisfaz `promote_when` sobe para `next` uma vez só, mesmo com várias instâncias avaliando ao mesmo tempo (comparar-e-trocar do `set_policy`, ADR 0051)
- [ ] Entidade bloqueada recebe 403 `account_suspended` sem `retry-after` em todos os drivers
- [ ] Registro da Responses API gravado por uma instância é lido por outra com `store: valkey`; com TTL de 30 dias e sem texto da conversa (ADR 0055)
- [ ] Evento `request` atendido traz `reasoning_tokens`, `cached_tokens` e `cost_usd` mesmo com valor 0; negação não traz nenhum campo de uso; `input_breakdown` só com `usage.extended: true` e igual ao header (ADR 0032)
- [ ] `Finish` com `QuotaMicroUSD` informado debita as janelas da cota por ele e o gasto/saldo por `CostMicroUSD`; sem ele, as duas pelo custo (como antes); `model_quota` e `model_spend` se comportam como qualquer entidade nos dois stores; o evento leva `billing_source` e `allowance_usd` quando o engine os informa (ADR 0072)

## Fora de escopo
- Persistência durável de contadores
- Limite arbitrário por entidade alterado em tempo de execução, fora de tiers (ADR 0025); no YAML, `accounts[]`, `projects[]` e `keys[].limits` (ADR 0051)

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0009-backup.md](decisions/0009-backup.md)
- [specs/decisions/0019-formato-ev-stream.md](decisions/0019-formato-ev-stream.md)
- [specs/decisions/0020-nomes-config-store.md](decisions/0020-nomes-config-store.md)
- [specs/decisions/0021-chaves-valkey-e-cluster.md](decisions/0021-chaves-valkey-e-cluster.md)
- [specs/decisions/0025-politicas-e-tier-por-entidade.md](decisions/0025-politicas-e-tier-por-entidade.md)
- [specs/decisions/0032-campos-de-uso-no-ev-stream.md](decisions/0032-campos-de-uso-no-ev-stream.md)
- [specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md](decisions/0051-limites-na-conta-e-promocao-de-tier.md)
- [specs/decisions/0055-contrato-da-responses-api.md](decisions/0055-contrato-da-responses-api.md)
- [specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)

## Comportamento

### 18. Estado de controle em Valkey

Todo contador que precisa valer para o cluster inteiro — limites, bloqueios, banimentos, saldo — vive no Valkey; a instância do simulador continua sem estado próprio e pode ser criada ou destruída a qualquer momento. Com `store.kind: memory` (padrão), o mesmo contrato `limits.Store` é atendido por um limitador em memória (só para uma instância).

**Entidades.** Cinco níveis, cada um com seu status, ligados em árvore: **IP** → **chave** → **usuário** → **projeto** (projeto OpenAI, workspace Anthropic) → **conta** (organização OpenAI/Anthropic, conta OpenRouter, workspace OpenCode). Uma requisição é avaliada em todos; o primeiro que bloqueia decide a resposta. Cada driver declara quais níveis existem no provedor que imita (seção 17); níveis ausentes são ignorados. O driver `azure` tem ainda o nível **deployment** (`<conta>/<modelo>`, spec 006 16.6): os limites de contagem do tier (RPS/RPM/TPM/ITPM/OTPM/RPD/TPD, concorrência, fila, rampa e algoritmo) contam no deployment; a conta fica com os limites em dólar (gasto, créditos, cota), o modo (`mode`, `after_n`, `window`), bloqueio e banimento. Os limites do tier contam na conta; chave e projeto só levam os tetos declarados em `keys[].limits` e `projects[].limits` ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md), spec 010). Unidades auxiliares, que não são da árvore e não recebem tier nem promoção: `free_models` (requisições a modelos gratuitos: `:free` do OpenRouter, id da conta; gratuitos do OpenCode, id da conta ou `ip:<endereço>` sem chave), `model_quota` (cota em dólares de um modelo, id `<dono>/<modelo>`, dono = conta ou membro) e `model_spend` (gasto mensal de um modelo num nível, id `<tipo>:<id>/<modelo>`) ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)).

**Esquema de chaves.** O esquema literal (chaves, tipos, campos, unidades, TTL, scripts Lua com KEYS/ARGV/retorno e o mapeamento de cada método de `limits.Store`) é normativo em [cache/valkey/README.md](../cache/valkey/README.md) ([ADR 0021](decisions/0021-chaves-valkey-e-cluster.md)). Resumo: toda chave de entidade tem a forma `wwz:<sub>:{<tipo>:<id-escapado>}` (hash tag por entidade); por entidade há `ent` (status, bloqueio, banimento, créditos), `tb` (token buckets), `win` (janelas fixas), `q5h`/`qwk`/`ramp` (janelas deslizantes em baldes), `conc` e `strikes`; globais são `idx:ent`, `idx:status:*`, `idx:inst`, `att:{conv:c}`, `inst:{i}`, `cfg:policies` (com pub/sub `cfg:changed`) e `ev:stream` (contrato em [asyncapi.yaml](../eventbus/valkey/asyncapi.yaml), [ADR 0019](decisions/0019-formato-ev-stream.md)). O relógio é o da instância, passado em ARGV.

**Registro de respostas da Responses API ([ADR 0055](decisions/0055-contrato-da-responses-api.md)).** Cada resposta de `POST /v1/responses` (e das rotas Responses dos outros drivers que guardam estado) com `store` diferente de `false` grava um registro compacto: conta dona, `models[].id`, número de mensagens e tokens da conversa acumulada (com o overhead por mensagem) e o identificador do prefixo para o cache de prompt; nunca o texto. Os tokens são os da saída entregue: com `finish_length` (spec 009) vale a saída cortada, não a inteira. TTL de 30 dias (a retenção do real). A chave leva hash tag no próprio id da resposta (`{resp:<id>}`), não na conta, para não concentrar um slot; o schema literal está em [cache/valkey/README.md](../cache/valkey/README.md) 2.5 (`P:resp:{resp:<id>}`, hash `acct`/`drv`/`model`/`msgs`/`toks`/`pfx`/`at`, `HSET`+`PEXPIRE` sem script; [ADR 0057](decisions/0057-schema-resp-e-ajustes-da-anthropic.md)). `previous_response_id` lê o registro: ausente, vencido ou de outra conta → `previous_response_not_found`. Com `store: memory`, o registro vale só na instância e é limitado por LRU (1 000 000 registros); a capacidade é uma proteção interna sem chave YAML (`Memory.SetResponseCapacity` só para testes, ADR 0058); com Valkey, a memória é do operador (`maxmemory` com política `volatile-*` descarta registros, que têm TTL, antes de contadores sem TTL). Valkey fora do ar segue `store.on_down` (`local`: registro em memória até voltar).

**Avaliação por requisição** (ADR 0021; em standalone um único `EVALSHA check` com as chaves de todas as entidades, \~0,1 ms; em cluster um `check_entity` por entidade no mesmo pipeline):

0. Antes do Valkey, a instância consulta um cache local de 1 s com as entidades que sabe estarem banidas ou bloqueadas; se alguma estiver, nega sem tocar o Valkey (e sem contar a requisição em `count`).
1. Incrementa `count` (base de `mode: after_n`) e lê as marcas de banimento e bloqueio da entidade. Banimento e bloqueio são marcas independentes no hash `ent` (campos `ban_*` e `blk_*`): `Ban` não apaga um bloqueio e `Unban` não mexe nele. Se houver banimento, ou bloqueio indefinido ou ainda não vencido, devolve o motivo e o `until`. Bloqueio vencido é ignorado sem ser apagado.
2. Para cada limite aplicável ao tier da entidade: verifica e incrementa (token bucket ou janela). Se exceder, devolve qual limite, `remaining` e `reset`; em standalone o script só grava se todas as entidades permitirem (numa negação, só `count` das entidades avaliadas até a que negou); em cluster todas as entidades são avaliadas (todas incrementam `count`) e o store envia `release` às que tinham permitido. O `retry_after` de `rpd`/`tpd` (até a meia-noite UTC) e de gasto/cota mensais (até o próximo mês UTC) é calculado em Go, não no script.
3. Incrementa `conc`; devolve o estado completo (`limit/remaining/reset` de cada unidade) para o driver preencher os headers.
4. Ao terminar a resposta, um segundo script (`finish`) devolve a reserva não usada de `tpm` (bucket ou janela fixa) e `tpd`, retira os tokens reais de `otpm`, debita `spend`/`credits` pelo custo e a cota pelo valor de cota da requisição (o `allowance` do OpenCode Go; sem ele, o custo; ADR 0072), soma o custo ao acumulado `spent`, decrementa `conc` e cria a entidade se ela não existir; devolve `requests`, `since`, `purchased` e `spent` da entidade para a avaliação de `promote_when` em Go (spec 010); o evento vai a `ev:stream` no mesmo pipeline. Saldo negativo põe a entidade no índice `limited`.

**Evento da requisição** ([ADR 0019](decisions/0019-formato-ev-stream.md), [ADR 0032](decisions/0032-campos-de-uso-no-ev-stream.md); contrato no [asyncapi](../eventbus/valkey/asyncapi.yaml) 0.5.0). `Finish` publica `request` (ou `error`) com `model` = `models[].id` do modelo resolvido ([ADR 0040](decisions/0040-label-de-modelo-e-get-de-modelo-openrouter.md)), `key`, `account` (toda chave tem conta: sem `account` no YAML vale `acct-<label>`) e `ip` no topo e, em `fields`, `duration_ms`, `prompt_tokens` e `output_tokens` sempre. Requisição atendida (inclusive erro injetado) traz também `reasoning_tokens`, `cached_tokens` e `cost_usd` sempre, inclusive 0; `tool_calls` só quando houve tool call; `input_breakdown` (I16, mesmo JSON do header `x-wwz-input-breakdown`) só com `usage.extended: true`. Negação de `Check` traz `denied: true` e `limit`, sem nenhum desses campos de uso. Quando o driver informa a origem da cobrança (`opencode`), a requisição atendida traz também `billing_source` (`free`, `balance`, `go`, `go-plus`) e, se houve débito de cota, `allowance_usd` (asyncapi 0.5.0, ADR 0072).

Os índices de status (`idx:status:banned|suspended|limited`) significam "tem a marca", não "status efetivo": uma entidade banida e bloqueada está nos dois; o status efetivo segue a precedência ban > bloqueio vigente > saldo negativo > ativo, e `List` remove do índice quem perdeu a marca.

**Janelas suportadas** (todas configuráveis por tier e por entidade): segundo (`rps`, token bucket), minuto (`rpm`, `tpm`, `itpm`, `otpm` — token bucket com reposição contínua, como a Anthropic, ou janela fixa, como a OpenAI; selecionável), janela deslizante de N minutos, hora, janela deslizante de N horas (as 5 horas do Claude Code/Codex), dia (reset em UTC ou em hora local configurável), semana (reset em dia da semana configurável), mês (orçamento em dólares com `soft_limit` que só avisa e `hard_limit` que bloqueia).

**Política de abuso** (`abuse.rules`, avaliadas pelo mesmo script):

| Gatilho | Ação padrão | Configurável |
| --- | --- | --- |
| N respostas 429 em M minutos para a mesma chave | `block` temporário com backoff exponencial (1 min, 5 min, 30 min, 6 h) | limiar, sequência |
| N chaves inválidas do mesmo IP em M minutos | `block` do IP por 1 h | limiar, duração |
| Mesma chave de mais de N IPs distintos em 1 h | `block` da chave e marcação `shared_key` | limiar |
| Chave marcada como vazada (`POST /admin/leak`) | `ban` da chave, `block` da conta até rotação | — |
| Strikes acumulados ≥ N em 30 dias | `ban` permanente da conta | limiar |
| Conteúdo sinalizado (simulado por `moderation_flag_probability`) | strike + resposta 403/`content_filter` | probabilidade |
| Saldo negativo | `limited` até crédito | — |

**Escada de abuso (`abuse.ladder`, task 250, ADR 0060).** O degrau aplicado é `strikes − strike_429.count` (índice 0 = `ladder[0]`) para o número de strikes da janela, limitado ao último degrau, que se repete: o strike que alcança `strike_429.count` abre o primeiro degrau, cada strike seguinte sobe um. `ban_after_strikes` bane qualquer que seja o degrau; sem `ladder`, o bloqueio é de 1 minuto. `unblock` (manual ou por expiração) **mantém** os strikes, de modo que a escalada continua de onde parou; só `reset` com `scope: ["strikes"]` os perdoa. O bloqueio e o ban da escada guardam `by = abuse-ladder` e `reason = "abuse ladder: <ação>"` ou `"abuse ladder: <n> strikes"` no store (visíveis em `until`/`reason` da API admin e do painel), mas o motivo **nunca chega ao cliente**: o 403 sai com o texto de conta suspensa do provedor (por exemplo, o do OpenRouter), igual a um bloqueio real.

Bloqueio (manual ou da política de abuso) devolve 403 de conta suspensa no dialeto do driver (`account_suspended`), **sem** `retry-after`: provedores reais não mandam esse header em 403 e os SDKs não retentam 403; o fim do bloqueio fica visível em `until` na API admin e no painel. Banimento devolve 401/403 com mensagem de conta desativada no dialeto do driver (seção 16 detalha o que cada provedor faz). Tudo pode ser aplicado e desfeito manualmente pelo painel ou por `/admin/entities/{tipo}/{id}`.

**Operação.** Cliente `valkey-go` com pipeline; `store.cluster: true` para Valkey Cluster com hash tag `{<tipo>:<id>}` para manter as chaves de uma entidade no mesmo slot; `store.read_timeout` (padrão 5 ms) e circuito aberto: se o Valkey não responde, política `store.on_down: allow | deny | local` (padrão `local`: limitador em memória até voltar). O circuito abre após 3 falhas seguidas de `Check`/`Finish`; aberto, uma sondagem por segundo faz `PING` e toca as conexões do pool (para que conexões quebradas se refaçam ali, não no data plane) e fecha o circuito no primeiro sucesso. Scripts Lua carregados no boot e recarregados em `NOSCRIPT`.

**Políticas e tier por entidade** ([ADR 0025](decisions/0025-politicas-e-tier-por-entidade.md)). `cfg:policies` é um hash com `version`, `tier:<nome>` (JSON de `limits` com os nomes do YAML) e `ent:<tipo>:<id>` (tier atribuído à entidade). Cada instância lê o hash no boot e a cada `cfg:changed` (reassinando na reconexão) e aplica só `version` maior que a última aplicada: `tier:<nome>` substitui o tier de mesmo nome do YAML, nomes novos acrescentam tiers (o tier do YAML mantém `promote_when` e `next`: `tier:<nome>` carrega só limites), e a configuração resultante passa pela validação e troca atômica do `POST /admin/config` (inválida: log, `wwz_policies_errors_total`, versão anterior mantida; gauge `wwz_policies_version`). Todo escritor altera campos e faz `HINCRBY version 1` no mesmo `MULTI` e depois `PUBLISH cfg:changed {"version": n}`. O tier por entidade vale para `account` ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md), que substitui a regra "só `key`" da 0025): `tipo = key` é aceito e resolvido para a conta da chave na configuração vigente; campos legados `ent:key:<id>` valem como tier da conta da chave, abaixo de `ent:account:<id>`, e o wwz não grava mais `ent:key:*`. Precedência dos limites da conta: `accounts[].limits` > tier da entidade > `accounts[].tier`/`keys[].tier` > `default`. Os `ent:*` são só validados (o tier citado precisa existir) e não são gravados na configuração efetiva: o tier da conta é lido por requisição, `GET /admin/config` mostra os `tier:<nome>` mas não as atribuições, e a atribuição aparece no `tier` do estado da entidade ([ADR 0058](decisions/0058-lote-do-painel-por-conta-e-achados-de-spec.md)). O tier é gravado em `cfg:policies` (propagação sem custo no caminho quente) e no campo `tier` do hash `ent` (exibição). A promoção automática (`promote_when`, spec 010) usa o mesmo campo, gravado com comparar-e-trocar (`set_policy` só troca se o valor ainda for o lido), de modo que instâncias concorrentes promovem uma vez só. O reset por entidade apaga `tb`, `win`, `q5h`, `qwk`, `ramp`, `conc`, `count`, o gasto da entidade e o acumulado `spent` (`counters`) e/ou `strikes`, preservando bloqueio, banimento, créditos, `purchased` e tier. Com `store: memory`, políticas e tiers valem só na instância local.

**Testes.** Os testes de `internal/limits` usam o banco `WWZ_TEST_VALKEY_DB` (padrão 15) e os de `cmd/wwz` usam `WWZ_TEST_VALKEY_DB − 1`, porque os pacotes rodam em paralelo e ambos fazem `FLUSHDB`; o banco 0 nunca é usado. As diferenças aceitas entre os stores em memória e Valkey estão na seção 7 do [README](../cache/valkey/README.md).

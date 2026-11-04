# 0051 — Limites na conta, `accounts[]`/`projects[]` e promoção automática de tier

Status: provisória
Data: 2026-10-31
Gatilho de revisão: conferência com chave real (ADR 0042) mostrar limite de taxa por chave em algum dos quatro provedores, limite de projeto/workspace com semântica diferente de "teto abaixo do da org", ou critério de promoção diferente dos abaixo; alguém depender de `keys[].limits` para **subir** o limite de uma chave acima do tier antes da `v0.1.0`.
Substitui em parte: [0025](0025-politicas-e-tier-por-entidade.md) (tier por entidade só em `key` e precedência dos limites de uma chave). Fecha as pendências da [0048](0048-dialetos-catalogos-e-templates.md) (item 4).

## Contexto
Hoje os limites de taxa e concorrência contam na entidade `key` e só gasto, créditos e cota em dólares contam na `account` (spec 010). A configuração padrão dá uma conta por chave (`acct-<label>`), mas as 10 chaves de teste embutidas compartilham `acct-test`, e qualquer YAML que repita `account` diverge do real. `tiers.*.promote_when` é aceito no YAML e nunca avaliado. A spec 006 já registra a regra real ("Limite por org/projeto/workspace, nunca por chave"; "Tiers com upgrade automático por compras ou histórico"). Fatos pesquisados em 31/10/2026:

- **OpenAI:** "Rate limits are defined at the organization level and at the project level, not user level"; limites por modelo; o projeto pode ter limites próprios abaixo dos da org. Tier: "Your organization's usage tier upgrades automatically as its total credit purchases reach each threshold" (Free, Build US$ 5, Launch US$ 100, Grow US$ 500). Fonte: [rate limits](https://developers.openai.com/api/docs/guides/rate-limits).
- **Anthropic:** "The API enforces service-configured limits at the organization level, but you can also set user-configurable limits for your organization's workspaces"; "Organization-wide limits always apply, even if Workspace limits add up to more"; "If not set, Workspace limits match the Organization's limit"; os headers `anthropic-ratelimit-tokens-*` mostram "the most restrictive limit currently in effect" (workspace ou org). Tier: "Organizations are placed on a tier automatically based on usage history and account standing and can move to a higher tier over time"; Evaluation tier para orgs novas, que "increase automatically as your organization builds usage history"; "Request tier increase" manual. Spend limit do workspace definido pelo cliente → 400 `invalid_request_error` "You have reached your specified workspace API usage limits". Fonte: [rate limits](https://platform.claude.com/docs/en/api/rate-limits).
- **OpenRouter:** "Making additional accounts or API keys will not affect your rate limits, as we govern capacity globally"; créditos no saldo da conta; teto de gasto opcional por chave ("an optional spending cap configured on an individual API key"); limites de modelos `:free` (50 ou 1 000 por dia) por conta, conforme créditos comprados (≥ 10). Fonte: [limits](https://openrouter.ai/docs/api-reference/limits).
- **OpenCode Go:** cotas em dólares de 5 h / semana / mês da assinatura; "only one member per workspace can subscribe"; nenhum limite por chave documentado. Fonte: [Go](https://opencode.ai/docs/go/).

## Decisão

### 1. Entidades
- `account` = a organização (OpenAI, Anthropic), a conta (OpenRouter) ou o workspace assinante (OpenCode); `project` = projeto OpenAI ou workspace Anthropic. A chave só resolve para elas.
- **Todos os limites do tier** (taxa, concorrência, rampa, gasto, créditos, cota) contam na `account`. Driver sem o nível `account` em `LimitPolicy.Entities` (nenhum hoje; o drivertest exige) conta na chave.
- `project` e `key` contam só limites declarados para eles (tetos adicionais, abaixo); sem declaração carregam só bloqueio e banimento, como hoje. O primeiro nível que nega decide (ordem `ip`, `key`, `user`, `project`, `account`), sem mudança.

### 2. YAML (acréscimos compatíveis, spec 003)
- `accounts[]`: `{id, tier, limits}`. `id` obrigatório e único; `tier` (padrão: o de `keys[].tier` das chaves da conta, senão `default`); `limits` = campos não nulos por cima do tier.
- `projects[]`: `{id, limits}` (sem tier, como o real: o workspace/projeto só tem tetos próprios).
- `keys[].tier` continua aceito e passa a significar **o tier da conta da chave** quando a conta não tem `tier` em `accounts[]`. Chaves da mesma conta com `tier` diferentes entre si, ou diferente de `accounts[].tier`, tornam a configuração inválida (`config: keys[i].tier "x" conflicts with account "a" tier "y"`). As chaves embutidas (`acct-test`, todas `default`) seguem válidas.
- `keys[].limits` passa a ser **teto da chave**, contado na entidade `key` (modela o teto de gasto por chave do OpenRouter e testes de uma chave só); não sobe o limite da conta. Mudança de semântica, sem caso no repositório (o único uso, `tests/sdk/wwz.yaml`, baixa `rpm` para 1) e antes da `v0.1.0`: registrada em "Mudanças incompatíveis" do CHANGELOG.
- `accounts[].id` e `projects[].id` sem chave que os cite geram só aviso; `keys[].account`/`keys[].project` sem entrada continuam válidos (conta implícita).

### 3. Precedência
- Conta: `accounts[].limits` > tier da entidade `account` (políticas `ent:account:<id>`, inclusive promoção) > `accounts[].tier` / `keys[].tier` > `default`.
- Chave: só `keys[].limits`. Projeto: só `projects[].limits`.
- O `TPMReserve` e o `ExcludeCacheRead` do driver valem em toda entidade com limite de tokens.

### 4. Headers de limite
Por medida (requests, tokens, input, output), o driver formata o estado da entidade com limite declarado que tem o **menor `remaining`** entre `key`, `project` e `account` (como a Anthropic descreve), com `limit` e `reset` dessa entidade. Os headers de projeto da OpenAI (`x-ratelimit-*-project-tokens`, spec 006) saem quando a entidade `project` tem `tpm`.

### 5. Tier por entidade (substitui a parte da 0025)
- `POST /admin/entities/{tipo}/{id}/tier` vale para `tipo = account`. `tipo = key` continua aceito por compatibilidade e é resolvido para a conta da chave na configuração vigente (chave desconhecida → 400); grava `ent:account:<conta>` e devolve o `EntityState` da conta. Outros tipos → 400.
- Campos legados `ent:key:<id>` de `cfg:policies` são lidos como tier da conta da chave, com precedência menor que `ent:account:<id>`; o wwz não grava mais `ent:key:*`.

### 6. Promoção automática (`promote_when`): implementada
- `tiers.<nome>.promote_when: {purchased_usd, spend_usd, age_days, requests}` — campo novo `spend_usd` (gasto acumulado, o "usage history" da Anthropic). Todas as condições não nulas valem juntas (E). `promote_when` sem nenhuma condição, `next` inexistente, `promote_when` sem `next` ou ciclo na cadeia `next` tornam a configuração inválida.
- Dados por conta no store (campos novos de `P:ent:E`): `purchased` (microdólares comprados, acumulado), `spent` (microdólares gastos, acumulado, nunca zerado por janela; zerado por `reset` `counters`); `since` (idade) e `requests` já existem.
- Compra: `POST /admin/entities/{tipo}/{id}/credits` aceita `{"add_usd": n}` (n > 0): soma ao saldo e a `purchased` (simula a compra de créditos). `{"usd": n}` segue definindo o saldo sem contar como compra. Exatamente um dos dois.
- Avaliação: em Go, depois do `finish` da conta (o script devolve `requests`, `since`, `purchased`, `spent`) e depois de `add_usd`. Se o tier efetivo da conta tem `promote_when` satisfeito, o wwz grava `ent:account:<id> = next` com o script `set_policy` em modo comparar-e-trocar (só se o campo ainda tem o valor lido; `""` = ausente) e publica `cfg:changed`; um passo por avaliação (a cadeia sobe nas seguintes). Cada instância tenta no máximo uma vez por (conta, tier de origem, versão das políticas). Só sobe, nunca rebaixa; tier atribuído à mão continua sujeito à promoção a partir dele. `accounts[].limits` continuam por cima.
- Observabilidade: `wwz_tier_promotions_total{from, to}` e log `info`; sem evento novo no `ev:stream`.
- `store: memory`: mesma regra, local à instância.
- Um tier "Evaluation" (spec 006) é só um tier com limites menores e `promote_when: {age_days: N}` ou `{requests: N}`.

## Alternativas consideradas
- **Manter limites na chave e documentar:** contraria a ADR 0042 e a spec 006 quando contas são compartilhadas (incluindo as chaves embutidas).
- **Escopo do limite configurável por driver (`key`/`account`):** nenhum dos quatro provedores limita por chave; um interruptor só criaria um modo divergente do real.
- **Tier só em `keys[]` aplicado à conta sem `accounts[]`:** obriga repetir o tier em todas as chaves e não tem onde declarar limites da conta.
- **Remover `promote_when`:** perde um comportamento real documentado (OpenAI por compras, Anthropic por histórico) que roteadores precisam ver (limites mudando sem reinício).
- **Avaliar a promoção no Lua do `check`:** pôr a tabela de tiers no script; a avaliação em Go depois do `finish` não custa ida extra ao Valkey.

## Consequências
- Specs 003, 010, 012 e 013, `cache/valkey/README.md` (campos `purchased`/`spent`, `ent:account:*`, `set_policy` com comparação) e `cmd/wwz/openapi.yaml` (tier por conta, `add_usd`, `purchased_usd` no `EntityState`) atualizados (task 179).
- Tarefas de código: config (`accounts[]`, `projects[]`, validação, `TierLimits` por conta), engine (sujeitos e headers do mais restrito), store (campos novos, `set_policy` CAS, retorno do `finish`, promoção) e admin/painel (tier por conta, `add_usd`).
- O texto exato do teto por chave do OpenRouter e do spend limit de workspace da Anthropic (400) fica "não conferido" até a chave real.

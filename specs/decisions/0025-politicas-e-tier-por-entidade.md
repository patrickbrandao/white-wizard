# 0025 — Políticas do cluster, tier por entidade e reset por entidade

Status: aceita (revisão com o humano em 2026-10-31); tier por entidade só em `key` e precedência dos limites da chave substituídos pela [0051](0051-limites-na-conta-e-promocao-de-tier.md)
Data: 2026-10-31

## Contexto
O schema do Valkey (README, ADR 0021) e o asyncapi já descrevem `cfg:policies` e o canal `cfg:changed`, e o hash `ent:` reserva o campo `tier`, mas o wwz não consome nada disso. A API admin não altera tier, limite nem contadores de uma entidade, e a tela "Limites" do painel (spec 016) pede "alterar tier, zerar contador, editar limite da entidade". Ler o tier da entidade no Valkey a cada requisição custaria uma ida a mais no caminho quente (spec 014 permite só duas).

## Decisão
- **Políticas:** `cfg:policies` é um hash com `version` (inteiro), `tier:<nome>` (JSON de `limits` com os nomes do YAML) e `ent:<tipo>:<id>` (nome do tier atribuído à entidade). O wwz lê o hash no boot e a cada mensagem de `cfg:changed`, aplicando só `version` maior que a última aplicada. `tier:<nome>` substitui por inteiro o tier de mesmo nome do YAML (nomes novos acrescentam tiers). A configuração resultante passa pela mesma validação e troca atômica do `POST /admin/config`; política inválida é logada, contada em `wwz_policies_errors_total` e ignorada (a versão anterior continua). Gauge `wwz_policies_version`. Recarga do YAML (SIGHUP, `POST /admin/config`) reaplica as políticas por cima.
- **Escrita atômica:** todo escritor (painel ou wwz) altera campos e faz `HINCRBY version 1` no mesmo `MULTI`/script e em seguida `PUBLISH cfg:changed {"version": n}`. O painel publica com concorrência otimista (`PUT /api/v1/policies` leva a `version` lida; divergente → 409).
- **Tier por entidade:** `POST /admin/entities/{tipo}/{id}/tier` com `{"tier": "<nome>"}` (`""` ou `null` remove a sobreposição). Na fase 3 vale para `tipo = key` (o único com tier no YAML); outros tipos → 400. Tier inexistente nos tiers efetivos → 400. O wwz grava `ent:<tipo>:<id>` em `cfg:policies` (propagação a todas as instâncias sem custo no caminho quente) e o campo `tier` do hash `ent:` (exibição). Precedência dos limites de uma chave: campos de `keys[].limits` do YAML > tier da entidade (políticas) > `keys[].tier` do YAML > `default`. Com `store: memory`, vale só na instância local. Resposta: `EntityState` com `tier`.
- **Reset por entidade:** `POST /admin/entities/{tipo}/{id}/reset` com `{"scope": [...]}`: `counters` (padrão: token buckets, janelas, `q5h`/`qwk`/`ramp`, `conc`, `count` e gasto da entidade) e/ou `strikes` (perdoar strikes). Preserva bloqueio, banimento, créditos e tier. Resposta: `EntityState`. Store sem suporte ou circuito aberto → 503.
- **Editar limite por entidade fica fora do MVP:** exigiria um layout novo de chaves por entidade e leitura no caminho quente. Substituto: criar/editar um tier em políticas e atribuí-lo à entidade.
- As rotas novas entram em `cmd/wwz/openapi.yaml` só como acréscimos (task 041); código na task 052.

## Alternativas consideradas
- Ler `ent.tier` no script `check`: tier precisa ser conhecido antes de montar os limites (ARGV); exigiria uma ida extra ao Valkey ou mover a tabela de tiers para dentro do Lua.
- Canal pub/sub próprio para tiers de entidade: segundo mecanismo para o mesmo problema; `cfg:policies` já tem versão e assinatura.
- Limites arbitrários por entidade (`lim:{E}`): flexível, mas custa uma leitura por requisição e um formato novo de chave.

## Consequências
- Specs 012, 013 e 016 atualizadas; `cache/valkey/README.md` documenta os campos `ent:<tipo>:<id>` de `cfg:policies` e o fim do "reservado" de `tier` (task 041).
- `Reset` global continua preservando `cfg:policies`; reset global não remove tiers atribuídos.
- O painel escreve no Valkey só `cfg:policies` (e publica `cfg:changed`); ações sobre entidades seguem pela API admin.

# Valkey — estado de controle

Schema normativo das chaves usadas pela implementação Valkey de `limits.Store` (`internal/limits/store.go`). A spec dona é [specs/012-estado-valkey.md](../../specs/012-estado-valkey.md). Decisões: [ADR 0019](../../specs/decisions/0019-formato-ev-stream.md) (eventos), [0020](../../specs/decisions/0020-nomes-config-store.md) (nomes de configuração), [0021](../../specs/decisions/0021-chaves-valkey-e-cluster.md) (layout e cluster), [0025](../../specs/decisions/0025-politicas-e-tier-por-entidade.md) (políticas, tier e reset por entidade), [0046](../../specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md) (prefixos do cache de prompt, seção 2.4), [0055](../../specs/decisions/0055-contrato-da-responses-api.md) (registro de respostas, seção 2.5), [0051](../../specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md) (tier na conta, `purchased`/`spent`, promoção), [0072](../../specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md) (entidades `model_quota`/`model_spend`, débito da cota separado do custo). Mantido pelo guardião das specs. Quem implementa o store segue este arquivo; o que não estiver aqui segue a semântica de `internal/limits/memory.go`.

Persistência desligada: os dados são de teste e descartáveis ([ADR 0009](../../specs/decisions/0009-backup.md)).

## 1. Convenções

- **Prefixo:** `P` = `store.prefix` (padrão `wwz`). Toda chave começa com `P:`.
- **Entidade:** `E` = `{<tipo>:<id-escapado>}`, com `tipo` ∈ `ip`, `key`, `user`, `project`, `account`, `deployment` (Azure; id `<conta>/<id do modelo>`, a conta é `keys[].account` ou `acct-<label>`; `/` não é escapado) `free_models` (requisições a modelos gratuitos; id = conta, ou `ip:<endereço>` no modo `public` do OpenCode), `model_quota` (cota em dólares de um modelo; id `<dono>/<id do modelo>`, dono = conta ou membro) e `model_spend` (gasto mensal de um modelo num nível; id `<tipo>:<id>/<id do modelo>`, ex. `account:acme/glm-5.3`) (`limits.EntityKind`; as três últimas pela [ADR 0072](../../specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md) no OpenCode). Nenhuma delas tem chave nova: usam `ent`, `win` (`rpd`, `spend`, `qmo`), `q5h` e `qwk` como qualquer entidade, e o `:` do id é escapado (`%3A`). Exemplo de deployment: `wwz:tb:{deployment:contoso/gpt-4o-sim}`; membro de índice `deployment:<conta>/<modelo>` em `P:idx:ent`. As chaves sempre trazem as chaves `{}`: é a hash tag, e todas as chaves de uma entidade caem no mesmo slot. Exemplo: `wwz:ent:{key:sk-abc}`.
- **Escape do id:** todo byte fora de `[A-Za-z0-9._~@+=/-]` vira `%XX` (hex maiúsculo). Assim `:`, `{`, `}`, `%`, espaço e bytes não ASCII nunca aparecem crus. Exemplos: `10.0.0.1` → `10.0.0.1`; `a:b{c}` → `a%3Ab%7Bc%7D`; IPv6 `::1` → `%3A%3A1`. Se o id escapado passar de 200 bytes, usa-se `~` + os 32 primeiros caracteres hex de `sha256(id)`; o id original fica sempre no campo `id` do hash `ent`. A mesma regra vale para ids de conversa e de instância.
- **Membro de índice:** `<tipo>:<id cru>`. Para separar, corte no primeiro `:`, porque `tipo` não tem `:`.
- **Unidades:** tempo em **ms desde a época Unix** (inteiro); dinheiro em **microdólares** (inteiro; US$ 1 = 1 000 000); tokens e requisições como número (o token bucket guarda fração, gravada com `string.format('%.6f')`).
- **Relógio:** `now_ms` vem sempre em ARGV, a partir de `CheckRequest.Now`/`FinishRequest.Now` (ou `time.Now()` se vazio). Os scripts não usam `TIME` (ADR 0021).
- **Janelas alinhadas:** minuto = `floor(now/60000)*60000`; dia = `floor(now/86400000)*86400000` (UTC); mês = 1º dia do mês corrente em UTC, calculado em Go e passado em ARGV (`month_start_ms`, `next_month_ms`).
- **Retorno de script:** o Valkey trunca números Lua para inteiro na resposta. Os scripts devolvem inteiros (`math.floor`) e strings; nunca frações.

## 2. Chaves

### 2.1 Por entidade (hash tag `E`)

| Chave | Tipo | Conteúdo | TTL | Lida por | Escrita por |
| --- | --- | --- | --- | --- | --- |
| `P:ent:E` | hash | Estado da entidade (campos abaixo) | 30 d, renovado a cada escrita; sem TTL (`PERSIST`) enquanto houver banimento (`ban_since`), bloqueio indefinido (`blk_since` com `blk_until = 0`) ou o campo `credits`; com bloqueio temporário, `max(30 d, blk_until − now + 1 d)` | `check`, `check_entity`, `Get`, `List` | `check`, `check_entity`, `release`, `finish`, `set_status`, `strike`, `sync_local`, `SetCredits`, `AddCredits`, `SetTier`, `reset_counters`, `reset_entity` |
| `P:tb:E` | hash | Token buckets: `<u>:t` (tokens), `<u>:ts` (último refill, ms), `<u>:cap`, `<u>:rate` (tokens/s), com `u` ∈ `rps`, `rpm`, `tpm`, `itpm`, `otpm` | `max(120 s, 2 × burst/rps s)`, renovado a cada escrita | `check*`, `release`, `finish`, `sync_local` | idem |
| `P:win:E` | hash | Janelas fixas: `<w>:s` (início, ms) e `<w>:c` (contagem), com `w` ∈ `rpm`, `tpm` (só com `algorithm: fixed_window`), `rpd`, `tpd`, `spend` (microdólares, mês), `qmo` (cota mensal em microdólares, mês), `sday` e `swk` (gasto em microdólares do dia UTC e da semana UTC, que começa na segunda 00:00), `free` (requisições a modelos gratuitos no dia UTC; task 258). Minuto e dia de `rpm`/`tpm`/`rpd`/`tpd` recomeçam em 0 quando `now − <w>:s ≥` duração da janela; as janelas de calendário (`spend`, `qmo`, `sday`, `swk`, `free`) recomeçam quando o início do período (`month_start_ms`, `day_start_ms` ou `week_start_ms` do `finish`) é maior que `<w>:s` (nunca volta) | 32 d, renovado a cada escrita | `check*`, `finish`, `Get` e `List` (`spend`, `sday`, `swk`, `free`, `qmo`: `SpendUSD`, `SpendDailyUSD`, `SpendWeeklyUSD`, `FreeRequestsToday`, `QuotaMonthlyUSD`; task 432) | `check*`, `release`, `finish` (gasto, cota, gratuitos e devolução de `tpm`/`tpd`), `reset_counters`, `reset_entity` |
| `P:q5h:E` | hash | Baldes de 1 min da cota de 5 h: campo = início do minuto (ms), valor = microdólares; `Get` também o lê (`Quota5hUSD`, `Quota5hSince`) | 5 h + 1 min | `check*` | `finish`; `check*` apaga baldes vencidos |
| `P:qwk:E` | hash | Baldes de 1 h da cota semanal: campo = início da hora (ms), valor = microdólares; o `check*` soma só os baldes desde a segunda 00:00 UTC da semana de `now` (task 296); `Get` também o lê (`QuotaWeeklyUSD`, task 432): soma os baldes com início ≥ segunda 00:00 UTC da semana de `now` ou, se houver balde com início posterior a `now` (um `finish` com relógio adiantado), da semana do balde mais novo (a semana nunca volta, como as janelas de calendário de `win`); soma negativa sai 0; `List` não lê `qwk` | 7 d + 1 h | `check*`, `Get` | `finish`; `check*` apaga baldes anteriores à segunda corrente |
| `P:ramp:E` | hash | Baldes de 1 min dos tokens reservados, para o limite de aceleração: campo = início do minuto (ms), valor = tokens | `ramp_window` + 1 min | `check*` | `check*`, `release` |
| `P:conc:E` | string | Requisições em voo (inteiro) | 120 s, renovado a cada INCR/DECR (segurança contra instância morta) | `check*` | `check*` (INCR), `release`, `finish` (DECR, mínimo 0), `reset_entity` (DEL) |
| `P:strikes:E` | zset | Violações: score = ms, membro = `<ms>:<8 hex aleatórios>` | `ttl` passado ao `Strike` (hoje a janela da regra de abuso; 30 d na política padrão), renovado | `strike` | `strike`, `reset_entity` (`strikes`) |

**Campos de `P:ent:E`**

| Campo | Significado |
| --- | --- |
| `kind`, `id` | Tipo e id cru (gravados na criação) |
| `since` | ms da criação (ordena o `List` de `idx:ent`; o `Get` mostra o `since` da marca vigente) |
| `ban_since`, `ban_reason`, `ban_by` | Banimento: presente = banido. Escrito por `Ban`, apagado por `Unban` |
| `blk_since`, `blk_reason`, `blk_by`, `blk_until` | Bloqueio: presente com `blk_until = 0` (até `Unblock`) ou `now < blk_until` = suspenso. Escrito por `Block`, apagado por `Unblock` |
| `status` | Cache do status efetivo, gravado por `set_status` (`active` na criação). Não é lido pelos scripts de avaliação nem pelo `Get`: o status real sai das marcas, com a precedência `ban` > bloqueio vigente > `credits < 0` (`limited`) > `active` |
| `tier` | Tier atribuído por `POST /admin/entities/{tipo}/{id}/tier` (ADR 0025); ausente = sem sobreposição. **Só exibição** (`Get`/`List`, painel): a avaliação usa o campo `ent:<tipo>:<id>` de `P:cfg:policies` (seção 2.3), não este. Escrito por `SetTier` (`HSET`, ou `HDEL` com tier vazio); preservado por `reset_counters`, `reset_entity` e `set_status` |
| `parent` | Reservado para a árvore ip → chave → usuário → conta |
| `strikes` | Contagem do último `Strike` |
| `requests` | Requisições aceitas (`EntityState.Requests`) |
| `count` | Requisições avaliadas pelo script, inclusive negadas, para `mode: after_n` |
| `credits` | Saldo em microdólares; ausente = entidade sem saldo. Criado por `SetCredits`, `AddCredits` ou na primeira avaliação admitida com `credits_usd` configurado |
| `purchased` | Microdólares comprados, acumulado (`AddCredits`, `POST .../credits` com `add_usd`); ausente = 0. Base de `promote_when.purchased_usd`. Preservado por `reset_counters`, `reset_entity` e `SetCredits` (ADR 0051) |
| `spent` | Microdólares gastos, acumulado sem janela (`finish` soma `cost_micro`); ausente = 0. Base de `promote_when.spend_usd`. Zerado por `reset_counters` e por `reset_entity` com `counters` (ADR 0051) |

Banimento e bloqueio são marcas **independentes**, como no store em memória: `Ban` não apaga um bloqueio e `Unban` não o restaura nem o remove; `Unblock` só mexe nos campos `blk_*`. Bloqueio vencido (`blk_until ≤ now`) é **ignorado** por `check*`, `sync_local`, `Get` e `List`, sem ser apagado: os campos `blk_*` ficam até o próximo `Block` ou `Unblock` (o TTL de `ent` cobre o fim do bloqueio + 1 d).

### 2.2 Globais

| Chave | Tipo | Conteúdo | TTL | Dono |
| --- | --- | --- | --- | --- |
| `P:idx:ent` | zset | Todas as entidades conhecidas: membro `<tipo>:<id>`, score = `since` | — | `List`, `ResetCounters`; escrito quando um script devolve `created` e por `set_status` |
| `P:idx:status:<status>` | zset | Entidades que têm a marca do status: `banned` = `ban_*`, `suspended` = `blk_*` (bloqueio), `limited` = `credits < 0`. Uma entidade banida e bloqueada fica nos dois. Mesmo membro; score = ms da escrita | — | `List(status)`; `Ban`/`Block` fazem `ZADD`, `Unban`/`Unblock` fazem `ZREM`; `Finish` faz `ZADD …:limited` quando o saldo fica negativo; `SetCredits` faz `ZADD` ou `ZREM` em `…:limited` |
| `P:att:{conv:<conv-escapado>}` | hash | Tentativas de passo de cenário (`fail_first`): campo = id do passo, valor = contagem | `ttl` do `Attempts` (1 h), renovado a cada incremento, por conversa | `Attempts`, `ResetConversations` |
| `P:inst:{<instância-escapada>}` | hash | Heartbeat: `id`, `version`, `addr`, `started` (ms), `beat` (ms), `rps`, `conns`, `inflight`, `store` (`ok`, `open` ou `local`) | 15 s; renovado a cada 5 s | instância; lido pelo painel |
| `P:idx:inst` | zset | Instâncias vivas: membro = id da instância, score = `beat` | — (podado a cada heartbeat: `ZREMRANGEBYSCORE -inf now-15000`) | instância, painel, `local_first` |
| `P:cfg:policies` | hash | Políticas do cluster (seção 2.3): `version`, `tier:<nome>` e `ent:<tipo>:<id>` | — (preservado por `Reset`) | painel escreve `tier:*`; `wwz` escreve `ent:*` (`SetTier`); `wwz` lê no boot e a cada `P:cfg:changed`; painel lê |
| `P:ev:stream` | stream | Eventos ([asyncapi](../../eventbus/valkey/asyncapi.yaml)): um campo `d` com JSON | — (`MAXLEN ~ store.event_max_len`) | `Finish`, `Block`, `Unblock`, `Ban`, `Unban`, `Strike` escrevem; `Events` e o painel leem |
| `P:cfg:changed` | canal pub/sub | `{"version": n}` | — | quem alterou `P:cfg:policies` publica (painel ou `wwz`); instâncias assinam |

Os índices globais não têm hash tag, então não entram em scripts.

### 2.3 Políticas (`P:cfg:policies`, ADR 0025)

**Campos do hash**

| Campo | Valor | Escrito por |
| --- | --- | --- |
| `version` | Inteiro ≥ 1, incrementado (`HINCRBY version 1`) a cada alteração; ausente = sem políticas (versão 0) | todo escritor, no mesmo `MULTI` da alteração |
| `tier:<nome>` | JSON de um objeto `limits` com os nomes do YAML (`{"rpm": 600, "tpm": 400000, ...}`, spec 003). `<nome>` segue as regras de nome de tier do YAML. Carrega só limites: ao aplicar, o tier de mesmo nome do YAML mantém `promote_when` e `next` (ADR 0058). | painel (`PUT /api/v1/policies`) |
| `ent:<tipo>:<id>` | Nome do tier atribuído à entidade. `<id>` cru (sem escape; o campo de hash aceita qualquer byte); para separar, corte `ent:`, depois no primeiro `:`. O wwz grava só `tipo = account` (ADR 0051); `ent:key:<id>` legado é lido como tier da conta da chave, abaixo de `ent:account:<id>` | `wwz` (`POST /admin/entities/{tipo}/{id}/tier`, operação `SetTier`; promoção automática por `promote_when`) |

Cada campo tem um único tipo de escritor: o painel nunca grava `ent:*` e o wwz nunca grava `tier:*`, para que duas escritas concorrentes não se sobreponham no mesmo campo.

**Escrita atômica.** Toda alteração é uma transação que altera os campos e incrementa a versão; depois dela, `PUBLISH P:cfg:changed {"version": n}` com o `n` devolvido pelo `HINCRBY`:

- Painel (concorrência otimista, `PUT /api/v1/policies` leva a `version` lida): `WATCH P:cfg:policies`, `HGET version`; se diferente da enviada, `UNWATCH` e 409; senão `MULTI`, `HDEL` dos `tier:*` removidos, `HSET` dos `tier:*` novos ou alterados, `HINCRBY version 1`, `EXEC` (EXEC nulo = outro escritor no meio → 409). O painel recusa (400) remover um `tier:<nome>` ainda citado por algum `ent:*` quando o nome não existe também no YAML.
- `wwz` (`SetTier` e promoção automática): script `set_policy` (3.12: `HSET P:cfg:policies ent:<tipo>:<id> <tier>` ou `HDEL` com tier vazio, mais `HINCRBY version 1`, atômicos); em seguida, em outro pipeline (outro slot), script `set_tier` (3.13: campo `tier` de `P:ent:E`, criação de `ent` e regra de TTL), `ZADD NX P:idx:ent` e o `PUBLISH`. A política já gravada é publicada mesmo se a escrita de exibição falhar.

**Leitura e aplicação pelo wwz.** No boot (antes de aceitar tráfego), a cada mensagem de `P:cfg:changed` e a cada (re)assinatura do canal, a instância faz `HGETALL P:cfg:policies` e aplica se `version` for maior que a última aplicada (versão igual ou menor é ignorada; a mensagem é só um aviso). Aplicar = partir da configuração do YAML (com patches do admin), substituir por inteiro cada tier de mesmo nome por `tier:<nome>` (nomes novos acrescentam tiers), validar as sobreposições `ent:*` (o tier citado precisa existir) sem gravá-las na configuração efetiva (o tier da conta é lido por requisição, e `GET /admin/config` não o mostra; o `tier` aparece no `EntityState`, ADR 0058) e passar o resultado pela validação e troca atômica do `POST /admin/config`. Política inválida (JSON quebrado, tier citado inexistente, limite inválido): log, `wwz_policies_errors_total` +1 e nada muda (a versão anterior continua e a versão recusada não é tentada de novo). Gauge `wwz_policies_version` = última versão aplicada. Recarga do YAML (SIGHUP, `POST /admin/config`) reaplica as políticas vigentes por cima. Precedência dos limites da conta: `accounts[].limits` do YAML > tier de `ent:account:<id>` > tier de `ent:key:<id>` legado (de uma chave da conta) > `accounts[].tier`/`keys[].tier` do YAML > `default`; chave e projeto só levam `keys[].limits`/`projects[].limits` (spec 010). Valkey fora do ar no boot: sobe só com o YAML e aplica as políticas ao reconectar.

Com `store: memory` não há hash: `SetTier` guarda a sobreposição na instância (vale só nela) e o painel trata as políticas como indisponíveis (409). São atualizados por comandos comuns depois do script e podem ficar obsoletos (ADR 0021); `List` corrige: remove do índice quem não tem `ent` ou não tem mais a marca (bloqueio vencido, saldo não negativo).

### 2.4 Prefixos do cache de prompt (ADR 0046)

Registro dos prefixos de prompt que cada conta já "gravou" no cache do provedor simulado (specs [007](../../specs/007-geracao-respostas.md) "Cache de prompt" e [008](../../specs/008-latencia.md)). Só hashes; o conteúdo do prompt nunca vai ao store.

| Chave | Tipo | Conteúdo | TTL | Lida por | Escrita por |
| --- | --- | --- | --- | --- | --- |
| `P:pc:A` | hash | Campo = hash do prefixo (32 hex); valor = `"<ttl_ms>:<deadline_ms>"` (TTL do provedor renovado a cada leitura; `deadline_ms` = instante máximo de vida desde a criação, `0` = sem teto) | `PEXPIRE … GT` com a maior expiração das entradas + 1 s | `pc_commit` | `pc_commit` |
| `P:pcx:A` | zset | Membro = o mesmo hash; score = expiração da entrada (ms) | idem | `PromptCacheLookup` (`ZMSCORE`), `pc_commit` | `pc_commit` |

**Conta (`A`).** `A` = a entidade `{account:<id-escapado>}` da chave da requisição (`keys[].account`, padrão `acct-<label>`; spec 003), com a mesma hash tag de `P:ent:{account:…}`: em cluster, as duas chaves de prefixo e o estado da conta caem no mesmo slot, e o script toca um slot só. Sem chave (`server.auth: none`), `A` = `{ip:<ip>}` do cliente (`clientip`). Contas diferentes nunca compartilham acertos.

**Hash encadeado.** O driver tokeniza o prompt na ordem do provedor (spec 007: OpenAI/Azure e gateways no dialeto `openai`: tools primeiro, depois system e mensagens, como o cabeçalho do prompt real; Gemini na ordem do template do `gtok`; Anthropic tools → system → messages) com o tokenizador do modelo e calcula:
- `h₀ = SHA-256("wwz-pc/2" ‖ 0x00 ‖ <nome do driver> ‖ 0x00 ‖ <id do modelo>)`: driver (`openai`, `azure`, ...; não o prefixo de rota) e `id` canônico (alias resolve para o `id`). Mesmo prompt em outro modelo ou driver não acerta, como no real. Quando o corpo traz `prompt_cache_key`, ele entra no hash inicial depois do `id` (cada elemento precedido de `0x00`): `h₀ = SHA-256("wwz-pc/2" ‖ 0x00 ‖ driver ‖ 0x00 ‖ id [‖ 0x00 ‖ prompt_cache_key])`. Mesmo prompt com outra chave (ou com e sem chave) é miss, como observado no real; o prefixo gravado com uma chave só acerta com a mesma chave.
- `hᵢ = SHA-256(hᵢ₋₁ ‖ segmentoᵢ)`, em que o segmento é a sequência de tokens entre o ponto de corte `i−1` e o `i`, cada token como `uvarint(len) ‖ bytes UTF-8`.
- A cadeia passa por **todos os pontos de corte** da regra do modelo (não só pelos candidatos consultados nem pelos limitados a 1 024), de modo que o hash de um prefixo é o mesmo qualquer que seja o resto do prompt; os candidatos são o subconjunto cujo hash é emitido. Em `kind: explicit`, os pontos de corte são o fim de cada bloco ≥ `min_tokens`, e os candidatos, os dos até 20 blocos antes de cada breakpoint.
- O campo é `hex(hᵢ[0:16])` (32 caracteres minúsculos). O hash do ponto `i` identifica o prefixo inteiro até ele.

**Pontos de corte (candidatos)**, em ordem crescente de comprimento, calculados pelo driver a partir de `min_tokens`, `granularity` e `kind` do modelo (padrões do driver ou `models[].prompt_cache`, spec 003):
- `kind: automatic` (OpenAI, Azure, Gemini implícito; OpenRouter e OpenCode quando o upstream é desses): todo comprimento `L = min_tokens + k·granularity ≤ tokens do prompt` (k ≥ 0). Com `granularity: 0` (fronteira exata), os candidatos são os múltiplos de 128 ≥ `min_tokens`, o fim de cada bloco de conteúdo (mensagem, parte, definição de ferramenta) ≥ `min_tokens` e o fim do prompt. Aproximação registrada: o real devolve o comprimento exato do maior prefixo comum; o wwz devolve o maior candidato comum.
- **Responses API (`POST /v1/responses`, ADR 0055/0059):** o candidato nunca cobre o último bloco de `granularity` tokens do prompt: os comprimentos válidos são `min_tokens + k·granularity ≤ total − granularity` (com `granularity: 0`, a cauda excluída é de 128 tokens). Observado três vezes em 31/10/2026 (`cache_automatic_prefix`, `previous_response_cache`): prompts de 1 695, 1 697 e 1 707 tokens são lidos até 1 536, nunca 1 664, enquanto o Chat Completions lê 1 280 de 1 400. A conversa gravada de `previous_response_id` (seção 2.5) segue a mesma regra: lê-se até o maior corte que ela cobre, menos essa cauda (1 536 de 1 698 em `previous_response_cache`).
- `kind: explicit` (Anthropic e quem a imita): o fim de cada bloco cacheável. Leitura: para cada breakpoint `cache_control`, os candidatos dos até 20 blocos anteriores a ele (inclusive); gravação: só os breakpoints, cada um com seu `ttl` (`5m` ou `1h`). Prefixo abaixo de `min_tokens` não é candidato nem gravado.
- `kind: none`: nenhum candidato; o store não é chamado.
- No máximo **1 024 candidatos** por requisição; acima disso ficam os 1 024 maiores.

**Operações** (seção 4):
1. **Leitura (`PromptCacheLookup`)**, antes do `Check`, porque os tokens em cache entram na reserva de `tpm`/`itpm` (`cached`, seção 3.3): `ZMSCORE P:pcx:A h₁ … hₙ`. O acerto é o maior candidato com score `> now_ms`; os tokens em cache são o comprimento dele. Somente leitura, sem script: uma requisição negada depois (limite, ban, caos) não renova nada.
2. **Gravação e renovação (`PromptCacheCommit`, script `pc_commit`, 3.14)**, quando a resposta envia o **primeiro byte** (headers + primeiro chunk no stream; início do corpo sem stream): renova as entradas lidas (o acerto e os candidatos menores vivos) e grava as novas (automático: todos os candidatos ≥ `min_tokens`; explícito: os breakpoints). Antes do primeiro byte a entrada não existe, como no real: duas requisições idênticas simultâneas são ambas misses e ambas gravam. Requisição que falha antes do primeiro byte (erro pré-resposta, cliente sumiu, negação) não grava.

**TTL por provedor** (spec 007): o driver passa `ttl_ms` e `max_life_ms` de cada entrada. OpenAI/Azure `in_memory`: 600 000 renovados, teto 3 600 000; `24h`: 86 400 000 com teto igual (a leitura não estende além de 24 h da criação); `prompt_cache_options.ttl` `30m`/`12h`: 1 800 000/43 200 000 com teto igual. Anthropic: 300 000 ou 3 600 000 renovados, sem teto. Gemini implícito: 300 000 renovados, sem teto (não conferido). OpenRouter e OpenCode: os do upstream.

**Limite de memória.** No máximo **65 536 entradas por conta** (`ZCARD P:pcx:A`); ao passar, `pc_commit` remove as de menor expiração (`ZPOPMIN`) e os campos correspondentes do hash. Cada `pc_commit` também remove até 64 entradas já vencidas (`ZRANGEBYSCORE -inf now_ms LIMIT 0 64`). As duas chaves expiram sozinhas quando a conta para de usar o cache (TTL da chave = maior expiração + 1 s). `Reset` apaga as duas (estão em `P:*`); `ResetCounters` e `ResetEntity` não mexem no cache de prompt.

**Store `memory`** (equivalente, mesma semântica e mesmos limites): por conta, um mapa `hash → {exp_ms, ttl_ms, deadline_ms}`, guardado num mapa de contas sob trava fragmentada; leitura percorre os candidatos do maior para o menor e devolve o primeiro com `exp > now`; `PromptCacheCommit` aplica a regra de 3.14; poda das vencidas a cada commit (até 64) e, acima de 65 536 entradas, remove as de menor expiração. Conta sem entradas vivas é removida do mapa. Vale só na instância.

**Falhas.** Erro ou timeout na leitura = miss (a requisição segue sem cache) e conta para o circuito como `Check`; falha do `pc_commit` só vai ao log (debug) e não conta. Com o circuito aberto: `local` usa o `limits.Memory` interno (descartado ao fechar); `allow` e `deny` = miss e sem gravação. `store.local_first` não muda nada aqui.

### 2.5 Registro de respostas da Responses API (ADR 0055)

Estado mínimo para `previous_response_id` (specs [004](../../specs/004-apis-e-endpoints.md) "Responses API" e [012](../../specs/012-estado-valkey.md)). Nunca guarda texto, tools nem `instructions`; só o necessário para somar tokens e casar o prefixo do cache de prompt.

| Chave | Tipo | Conteúdo | TTL | Lida por | Escrita por |
| --- | --- | --- | --- | --- | --- |
| `P:resp:{resp:<id-escapado>}` | hash | Um registro por resposta com `store` diferente de `false` (campos abaixo) | 30 d (2 592 000 000 ms) desde a criação; **não** renovado na leitura (retenção do real) | `ResponseGet` | `ResponsePut` |

- **Hash tag:** `{resp:<id-escapado>}`, com `<id-escapado>` pela regra da seção 1. O slot depende só do id da resposta, não da conta: milhões de registros se espalham pelo cluster e nenhum slot concentra uma conta. Cada operação toca uma chave só, então não há script nem restrição de slot.
- **Sem índice global:** não existe zset nem listagem de respostas (a rota `GET /v1/responses/{id}` está fora do MVP, ADR 0054). Nenhum `SCAN` fora do `Reset`.

**Campos**

| Campo | Significado |
| --- | --- |
| `acct` | Id cru da conta dona (`keys[].account`). `previous_response_id` de outra conta é tratado como inexistente (comparação em Go) |
| `drv` | Nome do driver que gravou (`openai`, `azure`, `openrouter`, `opencode`) |
| `model` | `models[].id` canônico (não o snapshot nem o alias pedido) |
| `msgs` | Número de mensagens da conversa acumulada (a anterior + a nova entrada + a saída como mensagem `assistant`) |
| `toks` | Tokens da conversa acumulada, com o overhead por mensagem (ADR 0049) e sem os 3 de priming da requisição; a próxima resposta soma `toks` à entrada nova e aos 3 de priming |
| `pfx` | Hash do prefixo (32 hex, mesma cadeia da seção 2.4) no fim da conversa acumulada; a requisição encadeada o considera já escrito no cache de prompt da conta. Vazio quando o modelo não tem cache (`kind: none`) |
| `at` | ms da criação (diagnóstico; a expiração é só o TTL da chave) |

**Operações** (seção 4):
1. **Gravação (`ResponsePut`)**, quando a resposta envia o primeiro byte, como o `pc_commit`: `MULTI` / `HSET P:resp:{resp:id} acct … drv … model … msgs … toks … pfx … at …` / `PEXPIRE P:resp:{resp:id} 2592000000` / `EXEC`. Requisição que falha antes do primeiro byte não grava; id repetido (colisão praticamente impossível) sobrescreve.
2. **Leitura (`ResponseGet`)**, antes do `Check` da requisição encadeada: `HGETALL P:resp:{resp:id}`. Chave ausente (nunca gravada, `store: false`, vencida ou descartada por `maxmemory`) ou `acct` diferente da conta da requisição = não encontrada; o driver responde 400 `previous_response_not_found`.

**Memória.** Cerca de 250 bytes por registro, sem teto no Valkey (a memória é do operador). Com `maxmemory-policy` `volatile-lru` ou `volatile-ttl`, o Valkey descarta registros (todos têm TTL) antes de contadores sem TTL (`P:ent:E` com `credits` ou banimento), que é o efeito desejado. Sob carga, o operador pode mandar `store: false`.

**Store `memory`** (mesma semântica): mapa `id → registro` com LRU de **1 000 000 registros** (padrão; descarta o menos recentemente gravado ou lido) e expiração de 30 d verificada na leitura; vale só na instância. `Reset` esvazia o mapa.

**Falhas.** Erro ou timeout em `ResponseGet` = não encontrado (o cliente recebe `previous_response_not_found`, como um registro perdido) e conta para o circuito como `Check`; falha em `ResponsePut` só vai ao log (debug) e não conta. Com o circuito aberto: `local` usa o `limits.Memory` interno (descartado ao fechar); `allow` e `deny` tratam a gravação como no-op e a leitura como não encontrada. `Reset` apaga as chaves (estão em `P:*`); `ResetCounters`, `ResetEntity` e `ResetConversations` não mexem nelas.

### 2.6 `cachedContents` da Gemini compartilhados entre instâncias (ADR 0063, task 265)

Estado do cache explícito do driver `gemini` (spec [006](../../specs/006-fidelidade-provedores.md) 16.5 e spec [007](../../specs/007-geracao-respostas.md)), compartilhado entre instâncias. Ao contrário do registro da Responses API (2.5), guarda o **conteúdo** (`systemInstruction`, `contents` e `tools`), porque a geração que lê o cache precisa dele: as tools do cache habilitam chamadas e a contagem do prompt soma o cache. Código: `internal/limits/cachedcontents.go`.

| Chave | Tipo | Conteúdo | TTL | Lida por | Escrita por |
| --- | --- | --- | --- | --- | --- |
| `P:gcc:{gcc:<dono-escapado>}` | zset | Membro = `cachedContents/<id>`; score = `createTime` em µs | `PEXPIREAT` da entrada que vence por último (`NX` e depois `GT` a cada gravação) | `CachedContentList` | `CachedContentPut`, `CachedContentDelete` |
| `P:gcc:{gcc:<dono-escapado>}:<nome-escapado>` | hash | Uma entrada (campos abaixo) | `PEXPIREAT` no `expireTime`, arredondado para cima ao ms | `CachedContentGet`, `CachedContentList` | `CachedContentPut`, `CachedContentDelete` |

- **Dono:** o escopo do driver (`p:<conta>/<projeto>`, `a:<conta>` ou `k:<chave>`), escapado pela regra da seção 1. As duas chaves de um dono têm a mesma hash tag, então `MULTI` e o pipeline da listagem tocam um slot só. Ler o nome de outro dono é "não encontrado".
- **Campos do hash:** `model` (`models/<id>`), `disp` (`displayName`), `toks` (`usageMetadata.totalTokenCount`), `ct` e `ut` (`createTime` e `updateTime` em µs), `exp` (`expireTime` em ns, a precisão do real) e `body` (JSON opaco do driver: `{"s": systemInstruction, "m": mensagens, "t": tools}`).
- **Operações** (seção 4): `CachedContentPut` = `MULTI` / `HSET` / `PEXPIREAT hash` / `ZADD idx` / `PEXPIREAT idx NX` / `PEXPIREAT idx GT` / `EXEC` (criação e `PATCH` da expiração). `CachedContentGet` = `HGETALL` (vazio, malformado ou `exp` ≤ agora = não encontrado). `CachedContentList` = `ZRANGE idx 0 -1` e `HGETALL` de cada membro num pipeline, do mais antigo ao mais novo; membro sem hash sai com `ZREM`. `CachedContentDelete` = `MULTI` / `DEL` / `ZREM` / `EXEC`; nome inexistente não é erro no store (o driver devolve o 404). Todas são **interfaces opcionais** (`limits.CachedContentStore`, como `ResponseRegistry`).
- **Compartilhamento:** `engine.New` chama `limits.ShareCachedContents(store)`; o driver usa `limits.SharedCachedContents()` (sem store compartilhado, um `limits.Memory` do processo).
- **Store `memory`:** mapa dono → nome → entrada, expiração verificada na leitura; vale só na instância; `Reset` esvazia.
- **Falhas:** erro de leitura = não encontrado (403 `CachedContent not found (or permission denied)`) e conta para o circuito como `Check`; erro de escrita = 500 `INTERNAL` e não conta (o `POST` não finge sucesso). Com o circuito aberto: `local` usa o `limits.Memory` interno; `allow` e `deny` não gravam e não encontram. `Reset` apaga as chaves (estão em `P:*`); `ResetCounters`, `ResetEntity` e `ResetConversations` não mexem nelas.
- **Memória:** o corpo guardado pesa o tamanho do conteúdo enviado; não há teto por dono no Valkey (o `TTL` limita a vida, padrão 1 h).

## 3. Scripts Lua

Todos são registrados com `valkey.NewLuaScript` (`valkey-go`), que usa `EVALSHA` e cai para `EVAL` em `NOSCRIPT`. No boot o store faz `SCRIPT LOAD` de cada um em todos os masters para falhar cedo. A lógica comum fica numa biblioteca Lua (`lua/lib.lua`), concatenada na frente de todos os scripts (`go:embed` de `lua/lib.lua` + `lua/<nome>.lua`). Scripts: `check` (3.3), `check_entity` e `release` (3.4), `finish` (3.5), `set_status` (3.6), `strike` (3.7), `reset_counters` (3.8), `reset_entity` (3.9), `sync_local` (3.10), `peek` (3.11), `set_policy` (3.12), `set_tier` (3.13) e `pc_commit` (3.14). A numeração 3.9–3.10 mudou com a task 041 (`reset_entity` entrou como 3.9; `sync_local` passou a 3.10).

### 3.1 Vetor de limites de uma entidade (`LIM`, 25 posições em ARGV)

| # | Valor | # | Valor |
| --- | --- | --- | --- |
| 1 | `kind` (string) | 14 | `concurrency` |
| 2 | `id` cru (string) | 15 | `queue_timeout_ms` (só se `on_limit: queue`; senão 0) |
| 3 | `mode`: 0 = `always`/`window`, 1 = `after_n`, 2 = `off` | 16 | `spend_monthly_usd` em microdólares |
| 4 | `after_n` | 17 | `credits_usd` em microdólares; `-1` = sem saldo |
| 5 | algoritmo: 0 = `token_bucket`, 1 = `fixed_window` | 18 | `quota_usd.monthly_usd` em microdólares (0 = sem cota) |
| 6 | `rps`; com janela curta (deployment do Azure, `Subject.Window` > 0), `rpm/60` fracionário | 19 | `quota_usd.rolling_5h_pct` |
| 7 | `burst` (0 → `rps`); com janela curta, `max(1, rpm × janela_s / 60)` | 20 | `quota_usd.weekly_pct` |
| 8 | `rpm` | 21 | `ramp.threshold_tpm` |
| 9 | `tpm` | 22 | `ramp.max_increase_pct` |
| 10 | `itpm` | 23 | `ramp.window` em ms (0 → 900 000) |
| 11 | `otpm` | 24 | `exclude_cache_read` (0/1) |
| 12 | `rpd` | 25 | máscara `local_first`: bit 0 = rps local, bit 1 = rpm local (o script pula essas unidades) |
| 13 | `tpd` | | |

Zero em qualquer limite = não aplicado. As chaves de uma entidade (`EK`, 7 posições em KEYS) vêm sempre nesta ordem: `ent`, `tb`, `win`, `q5h`, `qwk`, `ramp`, `conc`.

### 3.2 Ordem de avaliação de uma entidade

Igual a `Memory.Check`: `count += 1` → `ban` → `block` (vencido é ignorado, sem limpar) → `mode` (`off` ou `after_n` com `count ≤ after_n` pula os limites) → `rps` → `rpm` → `tpm` (reserva `prompt + output − cached?`) → `itpm` (`prompt − cached?`) → `otpm` (só folga: nega se `tokens < 1`) → `rpd` → `tpd` → `spend_monthly` (nega se `gasto ≥ limite`) → `credits` (inicializa o campo se ausente; nega se `≤ 0`) → `quota_5h` → `quota_weekly` → `quota_monthly` → `ramp` → `concurrency` (nega se `conc ≥ limite`; com `queue_timeout_ms > 0` devolve `queue=1`) → `requests += 1`.

Token bucket: no primeiro uso começa cheio (`tokens = cap`); refill contínuo `min(cap, tokens + Δs × rate)`; se `cap`/`rate` mudaram, adota os novos e corta `tokens` em `cap`. Para `rps`: `cap = burst`, `rate = rps`. Para `rpm`, `tpm`, `itpm` e `otpm`: `cap = limite`, `rate = limite/60`. `retry_after` e as mensagens seguem `memory.go` (as mensagens são montadas em Go a partir de `limit` e dos números devolvidos). O script devolve `retry_after_ms = 0` para `rpd`, `tpd`, `spend_monthly`, `credits` e `quota_monthly`, e o Go calcula: `rpd`/`tpd` até a próxima meia-noite UTC, `spend_monthly`/`quota_monthly` até o 1º dia do próximo mês UTC, `credits` = 0. Para `block` o script devolve `blk_until − now` (0 se indefinido); para `ban`, 0.

**Janelas de cota (task 296).** `quota_5h` soma os baldes de `q5h` vivos (`início + 1 min > now − 5 h`) e devolve `retry_after_ms = balde mais antigo + 5 h − now`; `quota_weekly` soma os baldes de `qwk` com `início ≥ segunda 00:00 UTC` (`now − (now − 4 d) mod 7 d`; a época é uma quinta) e devolve `retry_after_ms = próxima segunda 00:00 UTC − now`.

**Entidade com janela curta (deployment do Azure, task 295).** Quando `Subject.Window` > 0, o balde `rps` é a janela do RPM (posições 6 e 7 acima); o `rps`/`burst` do tier não vale. A negação `rps` reporta `RequestsLimit = rpm` (o script devolve a taxa truncada), com a mensagem `rate limit of <rpm> requests per minute exceeded (evaluated every <janela>)`. Com `store.local_first`, a entidade é avaliada só no Valkey (dividida por N instâncias, a janela não admitiria nenhuma requisição).

**Mapa `limit` → `core.ErrorKind`:** `ban` → `account_banned`; `block` → `account_suspended`; `rps`, `rpm`, `tpm`, `itpm`, `otpm`, `rpd`, `tpd` e `concurrency` sem fila → `rate_limit`; `concurrency` com fila → `overloaded` + `Queue`; `spend_monthly` → `spend_limit`; `credits`, `quota_5h`, `quota_weekly` e `quota_monthly` → `quota`; `ramp` → `slow_down`; `store_down` (circuito aberto com `on_down: deny`) → `overloaded`.

**Duas fases:** cada script primeiro lê e calcula tudo; só grava se todas as entidades permitirem. Numa negação, grava apenas `count` (e `kind`, `id`, `since`, `status=active` de uma entidade nova) das entidades avaliadas até a que negou (inclusive). Não há reversão dentro do script. O campo `credits` só é inicializado quando a entidade é admitida.

### 3.3 `check` (standalone, `store.cluster: false`)

- **KEYS:** `EK` da entidade 1, …, `EK` da entidade n (7·n chaves, na ordem de `CheckRequest.Subjects`).
- **ARGV:** `[1] now_ms`, `[2] est_prompt`, `[3] est_output` (reserva), `[4] cached`, `[5] month_start_ms`, `[6] next_month_ms`, `[7] n`, depois `LIM` da entidade i em `8 + 25·(i−1)`.
- **Retorno** (array de 32 itens: os 19 abaixo, os itens 20–29 sempre 0 no `check`, que não devolve `taken`, e os itens 30–32 do projeto): `1 allowed` (0/1), `2 idx` (1-based da entidade que negou; 0 se permitiu), `3 limit` (string; `""` se permitiu), `4 retry_after_ms`, `5 queue` (0/1), `6 req_limit`, `7 req_remaining`, `8 req_reset_ms`, `9 tok_limit`, `10 tok_remaining`, `11 tok_reset_ms`, `12 in_limit`, `13 in_remaining`, `14 out_limit`, `15 out_remaining`, `16 reason` (ban/block), `17 created_mask` (bit i−1 = `ent` da entidade i criado agora), `18 reserve` (tokens reservados), `19 extra` (ex.: tokens restantes na negação por `tpm`); `30 proj_tok_limit`, `31 proj_tok_remaining`, `32 proj_tok_reset_ms` (task 234: a unidade de tokens da entidade `project` com `tpm`; 0 sem ela). O parser aceita a resposta a partir de 19 itens.
- Os itens 6–15 viram `core.RateLimitState` com a menor `remaining` entre as entidades avaliadas que têm a unidade (task 189; a regra "última entidade vence" foi substituída), igual ao store em memória; `Present = true` se alguma unidade foi preenchida. Os itens 30–32 são `ProjectTokens{Limit,Remain,Reset}` (task 234): a unidade de tokens (`tpm`) da entidade `project`, para `x-ratelimit-*-project-tokens` da OpenAI; ficam 0 sem projeto com `tpm`. Pela documentação; não conferido no real.
- **Estado na negação (task 064):** quando a entidade nega por `rpm`, `tpm` ou `itpm`, o script ainda lê, **sem consumir**, as unidades de tokens seguintes **da própria entidade que negou** (`rpm` → `tpm`, `itpm`, `otpm`; `tpm` → `itpm`, `otpm`; `itpm` → `otpm`), como `memory.go` (`peekTokens`), para o 429 trazer também os headers de tokens. Com `algorithm: fixed_window`, `tpm` é lido da janela `win`; nos demais casos, do bucket (refill até `now`, nada gravado). O item 11 (`tok_reset_ms`) passa a ser escrito também por `otpm`: com `tpm` ou `itpm` na entidade, vale o maior entre o reset deles e o de `otpm`; sem eles, o reset de `otpm`. Vale igualmente para `check_entity` (3.4) e para o `peek` (3.11).
- Depois do script, no mesmo pipeline: `ZADD P:idx:ent NX <since> <membro>` para cada bit de `created_mask`.

### 3.4 `check_entity` e `release` (cluster, `store.cluster: true`)

`check_entity`:
- **KEYS:** `EK` de uma entidade. **ARGV:** `[1..6]` como em `check`, `LIM` a partir de 7.
- **Retorno:** os 19 itens de `check` (com `idx = 1` se negou), seguidos de 10 itens `taken`: `20 rps`, `21 rpm_tb`, `22 tpm_tb`, `23 itpm_tb`, `24 rpm_win`, `25 tpm_win`, `26 rpd`, `27 tpd`, `28 ramp`, `29 conc` (o que a entidade consumiu; tudo 0 se negou), e pelos itens `30–32` do projeto, como no `check` (32 itens no total).
- O store envia um `check_entity` por sujeito no mesmo pipeline, todos com o mesmo `now_ms`. A decisão é a primeira negação na ordem dos sujeitos; o estado (itens 6–15) é combinado com a mesma regra do `check` (até a entidade que negou, inclusive).
- Como todos os `check_entity` rodam, **`count` é incrementado em todas as entidades da requisição**, inclusive nas posteriores à que negou (em standalone, só até a que negou). `release` não desfaz `count` nem a inicialização de `credits`.

`release` (só quando alguma entidade negou, para cada uma que permitiu, em pipeline):
- **KEYS:** `EK`. **ARGV:** `[1] now_ms` (o mesmo do `check_entity`), `[2..11]` os 10 `taken`.
- Devolve tokens aos buckets (`min(cap, tokens + n)`, com `cap` lido do hash), subtrai das janelas `win` cujo início ainda é o da janela de `now_ms`, subtrai do balde de rampa do minuto de `now_ms`, faz `DECR conc` (mínimo 0) e `requests −= 1`. Retorna `1`.
- Falha do `release` (timeout) é só registrada: os buckets se recompõem pelo refill e `conc` pelo TTL.

### 3.5 `finish` (por entidade, nos dois modos, em pipeline)

- **KEYS:** `ent`, `tb`, `win`, `q5h`, `qwk`, `conc` da entidade.
- **ARGV:** `[1] now_ms`, `[2] conc_flag` (1 se `concurrency > 0`), `[3] give` (`max(0, est_output − actual_output)`), `[4] otpm` (limite, 0 = sem), `[5] actual_output`, `[6] cost_micro`, `[7] quota_flag` (1 se `quota_usd` configurada), `[8] month_start_ms`, `[9] tpm_mode` (0 = sem `tpm`, 1 = token bucket, 2 = `fixed_window`), `[10] tpd_flag` (1 se `tpd > 0`), `[11] kind`, `[12] id`, `[13] quota_micro` (microdólares debitados das janelas da cota; `-1` = `cost_micro`, o que o store manda sem `FinishRequest.QuotaSet`; ADR 0072: o OpenCode Go manda o `allowance` com `cost_micro = 0`), `[14] day_start_ms` (início do dia UTC de `now_ms`), `[15] week_start_ms` (segunda 00:00 UTC da semana de `now_ms`), `[16] free_flag` (1 se o modelo da requisição é gratuito, `limits.FreeModel`: sufixo `:free`). As 16 posições vão sempre, nesta ordem (`Valkey.finishCall`).
- **Efeito:** cria `ent` (`kind`, `id`, `since`, `status=active`) se faltar; com `conc_flag`, `DECR conc` (mínimo 0) e `PEXPIRE 120000`; com `give > 0`: `tpm_mode = 1` devolve `give` ao bucket `tpm` (até `cap`), `tpm_mode = 2` subtrai `give` da janela `win tpm` se ela ainda for a de `now_ms`, e `tpd_flag` subtrai `give` da janela `win tpd` nas mesmas condições; com `otpm`, faz refill e retira `actual_output` se houver saldo; com `free_flag = 1`: `win free += 1` (janela do dia de `day_start_ms`); com `cost_micro > 0`: `win spend += cost` (mês), `win sday += cost` (dia de `day_start_ms`), `win swk += cost` (semana de `week_start_ms`), `ent spent += cost` e `credits −= cost` se o campo existir; com `quota_flag` e `q = quota_micro ≥ 0 ? quota_micro : cost_micro` > 0 (independente de `cost_micro`): `q5h[minuto] += q`, `qwk[hora] += q`, `win qmo += q`. Renova o TTL de `ent`.
- Com `cost_micro > 0`, também `HINCRBY ent spent cost`.
- **Retorno:** `{created (0/1), has_credits (0/1), credits, requests, since, purchased, spent}` (microdólares; `since` em ms). Os quatro últimos alimentam a avaliação de `promote_when` em Go (spec 010), feita só para a entidade `account` cujo tier efetivo tem `promote_when`. Depois, em outro pipeline: `ZADD NX P:idx:ent` se `created`; `ZADD P:idx:status:limited` se `has_credits` e `credits < 0`.
- Com `store.local_first`, o `finish` de uma entidade é omitido quando ela não tem `concurrency`, `tpm`, `tpd`, `otpm`, custo nem débito de cota (`quota_micro`) na requisição e o modelo não é gratuito (`free_flag = 0`).
- O `XADD` do evento `request`/`error` vai no mesmo pipeline, fora do script (seção 5).
- Negação (`FinishRequest.Denial`): nenhum `finish` roda; só o `XADD` do evento `error`, com `entity` = entidade que negou, os campos de sempre (`duration_ms`, `prompt_tokens` e `output_tokens` zerados) mais `fields.limit` = nome do limite (`rpm`, `ban`, `block`, ...) e `fields.denied = true`, sujeito a `store.event_sample`.

### 3.6 `set_status` (Block, Unblock, Ban, Unban)

- **KEYS:** `ent`. **ARGV:** `[1] op` (`block`, `unblock`, `ban`, `unban`), `[2] now_ms`, `[3] reason`, `[4] by`, `[5] until_ms` (block; 0 = indefinido), `[6] kind`, `[7] id`.
- Cria `ent` (`kind`, `id`, `since`, `status=active`) se faltar. Bloqueio e banimento são independentes:
  - `block`: grava `blk_since=now`, `blk_reason`, `blk_by`, `blk_until` (substitui um bloqueio anterior); `changed = 1`.
  - `ban`: grava `ban_since=now`, `ban_reason`, `ban_by`; `changed = 1`.
  - `unblock`: se houver `blk_since`, apaga os campos `blk_*`; `changed = 1` só se o bloqueio ainda estava vigente.
  - `unban`: se houver `ban_since`, apaga os campos `ban_*`; `changed = 1`.
- Grava em `status` o status efetivo resultante (ban > bloqueio vigente > `credits < 0` > `active`) e aplica a regra de TTL de `ent` (seção 2.1).
- **Retorno:** `{changed (0/1), status_anterior, created (0/1), status_novo}` (status efetivos antes e depois).
- Em seguida, no mesmo pipeline (comandos comuns, roteados pela própria chave): `ZADD P:idx:ent <now> <membro>`; `block` → `ZADD P:idx:status:suspended`, `ban` → `ZADD P:idx:status:banned`, `unblock` → `ZREM P:idx:status:suspended`, `unban` → `ZREM P:idx:status:banned`; `XADD` do evento (só se `changed = 1`). O cache local de ban/block é preenchido (`block`/`ban`) ou limpo (`unblock`/`unban`) na hora.

### 3.7 `strike`

- **KEYS:** `strikes`, `ent`. **ARGV:** `[1] now_ms`, `[2] ttl_ms`, `[3] membro` (`<ms>:<8 hex>`), `[4] kind`, `[5] id`.
- `ZREMRANGEBYSCORE strikes -inf (now − ttl)`, `ZADD now membro`, `n = ZCARD`, `PEXPIRE strikes ttl`, `HSET ent strikes n` (cria `ent` se faltar).
- **Retorno:** `{n, created}`. Depois: `ZADD P:idx:ent NX` se `created` e `XADD` do evento `strike`.

### 3.8 `reset_counters` (ResetCounters, por entidade)

- **KEYS:** `ent`, `tb`, `win`, `q5h`, `qwk`, `ramp`. **ARGV:** nenhum.
- `DEL tb win q5h qwk ramp`; `HSET ent requests 0 count 0 spent 0`. Mantém status, `strikes`, `credits`, `purchased` e `conc`. Retorna `1`.

### 3.9 `reset_entity` (`ResetEntity`, ADR 0025)

- **KEYS:** `ent`, `tb`, `win`, `q5h`, `qwk`, `ramp`, `conc`, `strikes`. **ARGV:** `[1] counters` (0/1), `[2] strikes` (0/1).
- `counters`: `DEL tb win q5h qwk ramp conc`; se `ent` existir, `HSET ent requests 0 count 0 spent 0`. Zera janelas, gasto (`win spend`) e cota mensal (`win qmo`) da entidade. Requisições em voo terminam normalmente: o `DECR` do `finish` para em 0.
- `strikes`: `DEL strikes`; se `ent` existir, `HSET ent strikes 0`.
- Nunca cria `ent` e preserva `ban_*`, `blk_*`, `credits`, `purchased`, `tier` e `status`. Retorna `1`.
- Em seguida o store faz `Get` da entidade para a resposta (`EntityState`). Não gera evento.

### 3.10 `sync_local` (só com `store.local_first`)

- **KEYS:** `ent`, `tb`. **ARGV:** `[1] now_ms`, `[2] used_rps`, `[3] used_rpm`, `[4] requests_delta`, `[5] rps_cap`, `[6] rps_rate`, `[7] rpm_cap`, `[8] rpm_rate`, `[9] kind`, `[10] id`.
- Cria `ent` se faltar, soma `requests_delta` em `requests` e renova o TTL de `ent`; para cada unidade com `cap > 0` e `rate > 0`, faz refill do bucket global, subtrai o consumo local (mínimo 0) e grava.
- **Retorno:** `{rps_tokens, rpm_tokens, status (0 = ativo, 1 = suspenso, 2 = banido), until_ms, created (0/1), reason}`; `rps_tokens`/`rpm_tokens` = `-1` quando a unidade não se aplica.
- O store usa o retorno para ajustar o bucket local, preencher (status 1/2) ou limpar (status 0) o cache de ban/block e fazer `ZADD NX P:idx:ent` se `created`. Entidades tocadas desde a última rodada são sincronizadas mesmo sem consumo (negadas), para aprender bloqueios e banimentos.

### 3.11 `peek` (só com `store.local_first`, task 064)

- Somente leitura: não grava nenhuma chave.
- **KEYS:** `tb`, `win` da entidade. **ARGV:** `[1] now_ms`, `[2] tpm`, `[3] itpm`, `[4] otpm` (limites; 0 = sem), `[5] algoritmo` (0 = `token_bucket`, 1 = `fixed_window`).
- Lê `tpm`, `itpm` e `otpm` como em 3.3 ("Estado na negação"): `tpm` da janela `win` com `fixed_window`, senão do bucket com refill até `now` (bucket ausente = cheio); `otpm` escreve o reset de tokens pela mesma regra.
- **Retorno:** os 10 números de estado na ordem dos itens 6–15 de `check` (`req_limit`, `req_remaining`, `req_reset_ms`, `tok_limit`, `tok_remaining`, `tok_reset_ms`, `in_limit`, `in_remaining`, `out_limit`, `out_remaining`); a unidade de requisições vem sempre 0.
- Uso: quando o bucket local de `rpm` nega no `local_first` (seção 6), o store chama `peek` só se a entidade tiver `tpm`, `itpm` ou `otpm` e funde no estado do 429 apenas as unidades configuradas. Falha ou timeout é só registrado em log (debug): o 429 sai sem os headers de tokens e o circuito não conta a falha como erro de `Check`.

### 3.12 `set_policy` (`SetTier`, ADR 0025)

- **KEYS:** `P:cfg:policies`. **ARGV:** `[1] campo` (`ent:<tipo>:<id>`), `[2] valor` (nome do tier; `""` = `HDEL`), `[3] cas` (0/1), `[4] esperado` (com `cas = 1`: valor atual esperado do campo; `""` = ausente).
- Com `cas = 1` e o valor atual diferente de `esperado`, não grava nada e retorna `0` (outra instância já promoveu ou alguém atribuiu outro tier). Senão, `HSET`/`HDEL` do campo e `HINCRBY version 1` atomicamente (equivale ao `MULTI` da seção 2.3). `SetTier` do admin usa `cas = 0`; a promoção automática usa `cas = 1` com o tier lido das políticas aplicadas (ADR 0051). Só o wwz usa; o painel continua com `WATCH`/`MULTI` para os `tier:*`.
- **Retorno:** a nova `version`, usada no `PUBLISH P:cfg:changed` (0 = nada gravado, sem `PUBLISH`).

### 3.13 `set_tier` (`SetTier`, campo de exibição)

- **KEYS:** `ent`. **ARGV:** `[1] tier` (`""` = `HDEL`), `[2] now_ms`, `[3] kind`, `[4] id`.
- Cria `ent` (`kind`, `id`, `since`, `status=active`) se faltar, grava ou apaga o campo `tier` e aplica a regra de TTL de `ent` (seção 2.1).
- **Retorno:** `created` (0/1). Vai no mesmo pipeline do `ZADD NX P:idx:ent` e do `PUBLISH` (seção 2.3).

### 3.14 `pc_commit` (`PromptCacheCommit`, ADR 0046)

- **KEYS:** `P:pc:A`, `P:pcx:A`. **ARGV:** `[1] now_ms`, `[2] max_entries` (65 536), `[3] n`, depois `n` trios `hash`, `ttl_ms`, `max_life_ms` (0 = sem teto) a partir de 4.
- Para cada trio, com `old` = score atual em `pcx` (ausente ou `≤ now` = morta):
  - **Viva:** lê `ttl_old:deadline` do hash; `ttl = max(ttl_old, ttl_ms)`; `deadline` mantido (o teto conta da criação); `exp = max(old, now + ttl)`, limitado a `deadline` se `deadline > 0`.
  - **Morta ou ausente:** `ttl = ttl_ms`; `deadline = now + max_life_ms` (0 se `max_life_ms = 0`); `exp = now + ttl`, limitado a `deadline` se `deadline > 0`.
  - `HSET pc hash "<ttl>:<deadline>"`, `ZADD pcx exp hash`.
- Poda: até 64 vencidas (`ZRANGEBYSCORE pcx -inf now LIMIT 0 64` → `ZREM` + `HDEL`); se `ZCARD pcx > max_entries`, `ZPOPMIN pcx <excesso>` + `HDEL`.
- `PEXPIRE pc <maior exp − now + 1000> GT` e o mesmo em `pcx` (com `PEXPIRE` simples quando a chave acabou de ser criada, porque `GT` não age em chave sem TTL).
- **Retorno:** número de entradas criadas (as renovadas não contam), para métrica e teste.

## 4. Operações do `limits.Store`

| Operação | Chaves e comandos |
| --- | --- |
| `Check` | Cache local de ban/block (seção 6): se algum sujeito está no cache, nega sem tocar o Valkey (e sem incrementar `count`); senão `check` (standalone) ou `check_entity` × n + `release` (cluster) e `ZADD NX P:idx:ent` para entidades criadas. Uma negação por `ban`/`block` vinda do script alimenta o cache |
| `Finish` | `finish` × n + `XADD P:ev:stream` (amostrado) em pipeline; depois `ZADD NX P:idx:ent` e `ZADD P:idx:status:limited` conforme o retorno (seção 3.5). Com `FinishRequest.Denial` (requisição negada por `Check`: 429 de limite, 403 de block/ban) só o `XADD` do evento `error`, sem `finish`: nada foi reservado |
| `Block` / `Unblock` / `Ban` / `Unban` | `set_status` + índices + `XADD` |
| `Strike` | `strike` + `ZADD NX P:idx:ent` + `XADD` |
| `Get` | Pipeline `HGETALL P:ent:E` + `HMGET P:win:E spend:s spend:c sday:s sday:c swk:s swk:c free:s free:c qmo:s qmo:c` + `HGETALL P:q5h:E` + `HGETALL P:qwk:E` (`Quota5hUSD` = soma dos baldes vivos de `q5h`, em dólares; `Quota5hSince` = início do mais antigo, zero sem gasto; `QuotaWeeklyUSD` = soma dos baldes de `qwk` da semana UTC, com a regra da seção 2.1; os três `json:"-"`, fora do contrato admin; `List` não lê `q5h` nem `qwk`). Sem `ent` → `{Entity, Status: active}`. Cada janela de `win` conta se `<w>:s ≥` início do período corrente: `SpendUSD` = `spend:c/1e6` e `QuotaMonthlyUSD` = `qmo:c/1e6` (task 432; `json:"-"`) no mês UTC corrente; `SpendDailyUSD` = `sday:c/1e6` e `FreeRequestsToday` = `free:c` no dia UTC corrente, `SpendWeeklyUSD` = `swk:c/1e6` na semana corrente (`json:"-"`, como `Quota5hUSD`); `Credits` = `credits/1e6` se o campo existir; `PurchasedUSD` = `purchased/1e6` (ADR 0051). Status pelas marcas (seção 2.1): ban > bloqueio vigente > saldo negativo > `active`; bloqueio vencido sai como `active`; `Reason`/`By`/`Since`/`Until` vêm da marca vigente |
| `List(status, limit)` | Sem `KEYS`. `status` vazio ou `active` → `ZREVRANGE P:idx:ent`; `suspended`/`banned`/`limited` → `ZREVRANGE P:idx:status:<status>`. Lê em páginas de `limit` (`limit ≤ 0` → 1000; máximo 10 000), faz `Get` em pipeline e descarta quem não tem `ent` ou cujo status real não casa; remove do índice (`ZREM`) só quem não tem `ent` ou não tem mais a marca do índice (um banido também bloqueado continua em `idx:status:suspended`, mas não sai em `List(suspended)` porque o status real é `banned`) e para ao completar `limit` ou após 10 páginas. Ordem: `since` decrescente |
| `Attempts(conv, step, ttl)` | `MULTI`/`HINCRBY P:att:{conv:c} <step> 1`/`PEXPIRE ttl`/`EXEC`; devolve o `HINCRBY` |
| `SetCredits(e, usd)` | `HSET P:ent:E credits <usd×1e6>` (+ `HSETNX` de `kind`, `id`, `since`, `status`) e `PERSIST`; `ZADD NX P:idx:ent`; `ZADD P:idx:status:limited` se `usd < 0`, senão `ZREM` |
| `AddCredits(e, usd)` (ADR 0051) | `usd > 0`. Pipeline (sem `MULTI`; cada comando é atômico e o conjunto pode ser interrompido no meio, o que só deixa `credits` sem `purchased` num caso de queda, aceito em dados de teste): `HINCRBY P:ent:E credits <usd×1e6>`, `HINCRBY P:ent:E purchased <usd×1e6>` (+ `HSETNX` de `kind`, `id`, `since`, `status`), `PERSIST`; depois `HGETALL` para o estado; `ZADD NX P:idx:ent`; `ZREM`/`ZADD P:idx:status:limited` pelo saldo resultante. Devolve o `EntityState`; o wwz avalia `promote_when` em seguida. Interface opcional |
| `Reset` | `SCAN 0 MATCH P:* COUNT 1000` em cada master e `UNLINK` em lotes, preservando `P:cfg:policies`, `P:inst:*` e `P:idx:inst`; limpa o cache local e o anel local de eventos |
| `ResetCounters` | `ZSCAN P:idx:ent` e `reset_counters` por entidade, em pipelines de 500 |
| `ResetEntity(e, counters, strikes)` | `reset_entity` da entidade (seção 3.9) + `Get`. Interface opcional (como `CounterResetter`); só toca `e` |
| `SetTier(e, tier)` | Seção 2.3: `set_policy` (3.12) em `P:cfg:policies`; depois `set_tier` (3.13) + `ZADD NX P:idx:ent` + `PUBLISH P:cfg:changed`; entrega a nova versão aos observadores locais antes de retornar. Interface opcional |
| Políticas | `HGETALL P:cfg:policies` no boot e em cada `P:cfg:changed`; `SUBSCRIBE P:cfg:changed` numa conexão dedicada, refeita na reconexão (seção 2.3) |
| `ResetConversations(conv)` | `conv` vazio → `SCAN MATCH P:att:* COUNT 1000` em cada master + `UNLINK`, devolve o número de chaves apagadas; senão `UNLINK P:att:{conv:c}`, devolve 0 ou 1 |
| `Events(limit)` | `XREVRANGE P:ev:stream + - COUNT limit` (`limit ≤ 0` → 1000; máximo 10 000), decodificando o campo `d`. Com `store.event_stream: false`, lê um anel local de 10 000 eventos (como o store em memória) |
| `PromptCacheLookup(conta, hashes)` | `ZMSCORE P:pcx:A h₁ … hₙ`; devolve o índice do maior candidato com score `> now` (−1 = miss). Seção 2.4. Interface opcional (como `CounterResetter`) |
| `PromptCacheCommit(conta, entradas)` | Script `pc_commit` (3.14), no primeiro byte da resposta. Interface opcional |
| `ResponsePut(id, registro)` (ADR 0055) | `MULTI`/`HSET P:resp:{resp:id} …`/`PEXPIRE … 2592000000`/`EXEC`. Seção 2.5. Interface opcional (como `CounterResetter`) |
| `ResponseGet(id)` (ADR 0055) | `HGETALL P:resp:{resp:id}`; vazio = não encontrado. Seção 2.5. Interface opcional |
| `CachedContentPut` / `Get` / `List` / `Delete` (ADR 0063) | Seção 2.6: hash `P:gcc:{gcc:dono}:nome` e zset `P:gcc:{gcc:dono}`, `MULTI` sem script. Interface opcional `limits.CachedContentStore` |
| `Kind` / `Close` | `"valkey"` / fecha o cliente e para heartbeat, sync e assinatura |

## 5. Eventos

`XADD P:ev:stream MAXLEN ~ <store.event_max_len> * d <json>`, com `json` = `limits.Event` + `instance`, `request_id` e `test_id` (contrato em [asyncapi.yaml](../../eventbus/valkey/asyncapi.yaml)). `store.event_sample` (0 < s ≤ 1) amostra só `request` e `error`; o evento amostrado com `s < 1` leva `"sample": s`. Com `store.event_stream: false` nada vai ao Valkey. O painel lê pelo grupo `panel`.

## 6. Cache local, `local_first` e Valkey fora do ar

- **Cache de ban/block:** cada instância guarda por 1 s (relógio de parede) as entidades que sabe estarem banidas ou bloqueadas (negação de `check`, retorno de `sync_local`, ou `Block`/`Ban` feitos por ela), com motivo e `until`. Durante esse 1 s a negação sai sem tocar o Valkey e, portanto, sem incrementar `count`; um bloqueio cujo `until` passou deixa de negar. `Unblock`/`Unban` feitos pela própria instância e `sync_local` com status 0 limpam a entrada; feitos por outra instância, valem em até 1 s.
- **`store.local_first: true`** (spec 014): `rps` e `rpm` em token bucket passam a ser controlados por buckets em memória por entidade, com `cap/N` e `rate/N`, onde `N = max(1, ZCOUNT P:idx:inst now−15000 +inf)`, relido a cada heartbeat. A cada 100 ms a instância envia `sync_local` em pipeline para cada entidade tocada e ajusta o bucket local para `min(cap/N, tokens_globais/N)`. Se a entidade só tem `rps`/`rpm` e não está no cache de ban/block, `Check` não toca o Valkey; caso contrário vai `check` com a máscara (posição 25) ligando as unidades locais. Entidades com `mode: after_n` ou `algorithm: fixed_window` não usam o modo local. `Finish` pula o `finish` de entidades sem `concurrency`, `tpm`, `otpm`, gasto, saldo ou cota. `requests` é acumulado localmente e enviado no `sync_local`. O uso reportado a `promote_when` (`requests`) soma as requisições ainda não sincronizadas do bucket local às do store (ADR 0058).
- **Heartbeat:** a cada 5 s, `HSET P:inst:{i} ...` + `PEXPIRE 15000` + `ZADD P:idx:inst <now> i` + `ZREMRANGEBYSCORE P:idx:inst -inf now−15000`.
- **Circuito (`store.on_down`):** cada comando usa `store.read_timeout`. O circuito abre após 3 falhas ou timeouts seguidos no data plane (`Check`/`Finish`; erro causado pelo cancelamento do próprio cliente não conta). Aberto, a cada segundo uma sondagem: `PING` e, se responder, 49 `EXISTS P:probe` para tocar as conexões do pool (comandos com chave escolhem uma conexão ao acaso), de modo que conexões quebradas durante a queda se refaçam na sondagem e não no data plane; a sondagem bem-sucedida fecha o circuito. Se a primeira conexão falhar no boot, o circuito já nasce aberto e a sondagem tenta conectar de novo. Com o circuito aberto: `allow` → `Check` permite sem estado de rate limit e `Finish` não faz nada; `deny` → `Check` nega com `limit = store_down`, `overloaded` e `RetryAfter` de 1 s; `local` (padrão) → `Check`/`Finish` vão para um `limits.Memory` interno, descartado quando o circuito fecha. Nos três modos `Attempts` e `Strike` usam o `limits.Memory` interno, e as operações de admin (`Block`, `Unblock`, `Ban`, `Unban`, `SetCredits`, `SetTier`, `Get`, `List`, `Reset*`) devolvem erro (o admin responde 503). As políticas já aplicadas continuam valendo; as novas são lidas quando o circuito fecha. O store expõe `wwz_store_circuit_open` (gauge 0/1) para o alerta da spec 016.

## 7. Diferenças aceitas em relação ao store em memória

A suíte comum de `limits.Store` (`internal/limits/storetest`) não deve depender destas diferenças; se depender, ajusta-se o store em memória:
1. Numa negação, nada além de `count` (e da identidade de uma entidade nova) é gravado (o store em memória também grava o balde de rampa).
2. `Attempts` expira por conversa, não por passo.
3. Gasto e cota mensais usam o mês civil UTC (o store em memória usa janelas de 30 dias).
4. Janelas deslizantes têm resolução de 1 min (5 h e rampa) e 1 h (semana).
5. Em cluster, uma negação pode consumir por alguns microssegundos a cota de outras entidades da mesma requisição (ADR 0021).
6. Em cluster, `count` (base de `mode: after_n`) é incrementado em todas as entidades da requisição, inclusive nas posteriores à que negou; o store em memória e o `check` standalone param na que negou.
7. Uma negação servida pelo cache de ban/block não incrementa `count` (o store em memória incrementa antes de olhar o banimento).
8. Bloqueio vencido não é apagado do Valkey (fica nos campos `blk_*` até o TTL ou o próximo `Block`/`Unblock`); o store em memória o descarta. O efeito observável é o mesmo: a entidade volta a `active`.
9. Cache de prompt: a poda de vencidas é preguiçosa nos dois stores (até 64 por commit), mas no Valkey a chave inteira só some pelo TTL, enquanto o store em memória remove a conta sem entradas vivas na poda. O limite de 65 536 conta também entradas vencidas ainda não podadas, nos dois.

## 8. Docker

`cache/valkey/docker-compose.yml` sobe um Valkey standalone sem persistência. O Valkey Cluster só roda no verify-heavy (ADR 0008).

**Bancos de teste.** Os testes nunca usam o banco 0 e fazem `FLUSHDB`. `scripts/verify.sh` exporta `WWZ_TEST_VALKEY=local`, `WWZ_TEST_VALKEY_ADDR` e `WWZ_TEST_VALKEY_DB` (padrão 15). `internal/limits` usa `WWZ_TEST_VALKEY_DB`; `cmd/wwz` usa `WWZ_TEST_VALKEY_DB − 1` (padrão 14) e `internal/panel/source` usa `WWZ_TEST_VALKEY_DB − 2` (padrão 13), porque `go test` roda pacotes em paralelo e cada pacote esvazia o seu banco antes dos subtestes; quando o banco calculado seria ≤ 0 o teste do pacote é pulado (com `WWZ_TEST_VALKEY_DB = 1`, os de `cmd/wwz` e `internal/panel/source`; com `2`, o de `internal/panel/source`). Um pacote novo que use o Valkey de teste pega o próximo banco abaixo. A avaliação de cluster (`check_entity` + `release`) é exercitada em standalone com `ValkeyOptions.PerEntity`.

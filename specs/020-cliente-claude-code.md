# 020 — Perfil do cliente Claude Code e verificação estrita

Status: rascunho
Depende de: 004, 005, 006
Origem: pedido do humano em 2026-11-03 ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)); capturas e análise em `tmp/claude-code/` (fora do git)

## Contexto
O Claude Code fala com a Claude API por um cliente próprio (Bun, SDK Stainless `0.128.0`) cujas requisições têm forma fixa por versão: linha, ordem e grafia dos headers, betas, corpo serializado por `JSON.stringify` e um billing header com fingerprint em `system[0]`. Esta spec descreve essa forma como um **perfil por versão** (pacote `internal/claudecode`), usado em dois lugares: o cliente de testes `wwz-load claude-code` (spec 015), que gera requisições byte a byte idênticas, e o modo `drivers[].options.client_verification: claude-code` do driver `anthropic`, que recusa quem não segue o perfil. A autenticação de assinatura (OAuth) está na spec 006, 16.2.1.

## Requisitos
- RF-01: o perfil `2.1.308` descreve exatamente as requisições capturadas do Claude Code 2.1.308 (fixtures `testdata/real/claude-code/2.1.308/`).
- RF-02: `internal/claudecode` gera, a partir dos insumos de uma requisição (versão, entrypoint, credencial, sessão, device, conta, modelo, mensagens, system prompt, tipo), os mesmos bytes que o Claude Code mandou.
- RF-03: com `client_verification: claude-code`, toda requisição de inferência que diverge do perfil recebe 403 `forged_request_error` com um `reason` da lista fechada; toda fixture de inferência do perfil é aceita.
- RNF-01: com `client_verification: off` (padrão) nada disto custa no caminho quente (spec 014).

## Interfaces e contratos
- `internal/claudecode`: `Profile` por versão (embutido), `Fingerprint(text, version)`, `FingerprintTexts(messages)` (candidatos do `Verify`), `BillingHeader(...)`, `UserID(device, account, session)`, encoder compatível com `JSON.stringify` sobre estruturas ordenadas, `Build(...)` (cabeça e corpo de uma requisição) e `Verify(head, body) (reason, detail)`.
- Recusa: corpo, headers e códigos `reason` abaixo; a lista de `reason` é contrato (código novo é mudança de spec).
- Opção `drivers[].options.client_verification` (spec 003).

## Dados
- Fixtures `testdata/real/claude-code/2.1.308/*.raw` (cabeça e corpo crus, CRLF) e `MANIFEST.md` (origem, modo, normalização e sha256 de cada uma).
- Nenhum estado no servidor.

## Critérios de aceite
- [ ] `Fingerprint` dá `7f2` para `say ok`, `9fe` para `Hello there, reply with ok`, `798` para `Second conversation, first turn` e `a39` para `quota` com a versão `2.1.308`
- [ ] O encoder reproduz byte a byte o corpo de cada fixture a partir da decodificação ordenada dela (inclusive `\n` dentro de strings, `<`, `>`, `&` e não-ASCII crus)
- [ ] `Build` com os insumos de cada fixture gera bytes com o mesmo sha256 da fixture (teste `TestRealFixturesClaudeCode`)
- [ ] `Verify` aceita todas as fixtures de inferência (002–014) e recusa cada mutação da tabela "Mutações de teste" com o `reason` indicado; a 001 (`HEAD /api/hello`) é reconstruída pelo `Build` (tipo `hello`) mas não é rota de inferência, e o `Verify` a recusa com `query`
- [ ] Driver `anthropic` com `client_verification: claude-code`: cada fixture enviada crua por TCP é aceita (200; a 001 pela rota pública `/api/hello`, que não é verificada); cada mutação recebe 403 com o corpo, os headers e o `reason` desta spec, sem débito de cota nem contagem em limite; `wwz_client_verification_failures_total{driver,reason}` incrementa
- [ ] Os SDKs oficiais `anthropic` (Python e Node) recebem 403 `forged_request_error` no modo estrito e 200 com `off`
- [ ] Com `off`, os benchmarks de `internal/server` não mudam alocações por operação
- [ ] `tests/claudecode/default-mode.sh` (15 modos do Claude Code em uso normal, com prompt e tools padrão: `-p` e interativo, OAuth e API key, `--resume`, tool, subagente, `CLAUDE.md`, classificador do modo auto, `/context` e `!comando` antes do prompt) termina sem nenhum 403 `forged_request_error` com o Claude Code 2.1.308 instalado; roteiro manual, fora do verify; o que o roteiro não alcança fica na spec 006, seção "Não conferidos no real"

## Fora de escopo
- Outras versões do Claude Code (entram por tarefa, com captura nova; ADR 0074, item 16).
- Requisições ao first-party oficial (`cch=`, `cc_workload` etc.) e às rotas de conta: o perfil cobre só o que o Claude Code manda a um `ANTHROPIC_BASE_URL` próprio.
- Verificação atrás de proxy que reescreve headers (ADR 0074, itens 15 e 19).

## Decisões relacionadas
- [0042](decisions/0042-fidelidade-total-ao-provedor-real.md), [0053](decisions/0053-tokenizador-bpe-e-ordem-chave-corpo.md), [0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md), [0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### Perfil `2.1.308`

**Linha.** `POST <prefixo>/v1/messages?beta=true HTTP/1.1`, ou `POST <prefixo>/v1/messages/count_tokens?beta=true HTTP/1.1` no tipo `count_tokens` (`<prefixo>` = caminho do `ANTHROPIC_BASE_URL`, o mount do driver no wwz). A query é sempre exatamente `beta=true`.

**Headers**, nesta ordem e grafia (Bun mantém a ordem de inserção e põe `Connection`, `Host`, `Accept-Encoding` e `Content-Length` no fim). Nunca `Transfer-Encoding`.

| # | Header | Valor | Notas |
| --- | --- | --- | --- |
| 1 | `Accept` | `application/json` | |
| 2 | `Authorization` | `Bearer sk-ant-oat01-…` | só com credencial OAuth |
| 3 | `Content-Type` | `application/json` | |
| 4 | `User-Agent` | `claude-cli/<VER> (external, <EP>)` | `<EP>` ∈ `sdk-cli` (`-p`), `cli` (interativo) |
| 5 | `X-Claude-Code-Session-Id` | UUID v4 minúsculo | igual a `metadata.user_id.session_id` |
| 6 | `X-Stainless-Arch` | `arm64` ou `x64` | `x64` não capturado |
| 7 | `X-Stainless-Lang` | `js` | |
| 8 | `X-Stainless-OS` | `MacOS`, `Linux` ou `Windows` | só `MacOS` capturado |
| 9 | `X-Stainless-Package-Version` | `0.128.0` | |
| 10 | `X-Stainless-Retry-Count` | inteiro decimal ≥ 0, sem zero à esquerda | 0 na primeira tentativa; +1 por retentativa |
| 11 | `X-Stainless-Runtime` | `node` | |
| 12 | `X-Stainless-Runtime-Version` | `v26.3.0` | |
| 13 | `X-Stainless-Timeout` | `600` | `300` no fallback não-stream; `60` só no tipo `classifier`; ausente no tipo `count_tokens` |
| 14 | `anthropic-beta` | lista por tipo (abaixo) | separada por vírgula, sem espaço |
| 15 | `anthropic-dangerous-direct-browser-access` | `true` | |
| 16 | `anthropic-version` | `2023-06-01` | |
| 17 | `x-api-key` | a chave | só com credencial de API key, e então sem `Authorization` |
| 18 | `x-app` | `cli` | |
| 18a | `x-claude-code-agent-id` | `a[<rótulo>-]<16 hex minúsculos>`, rótulo `[A-Za-z0-9_-]{1,63}` | opcional: só no subagente, junto de `cc_is_subagent=true;` no billing header; o `Build` nunca manda |
| 18b | `x-claude-code-parent-agent-id` | mesma forma | opcional: só no subagente aninhado; o `Build` nunca manda |
| 19 | `Connection` | `keep-alive` | |
| 20 | `Host` | qualquer | |
| 21 | `Accept-Encoding` | `gzip, deflate, br, zstd` | |
| 22 | `Content-Length` | tamanho do corpo | |

**Tipos de requisição** (ordem exata das chaves do topo; `system` sempre lista de blocos `text`):

| Tipo | Quando | Chaves | `system` |
| --- | --- | --- | --- |
| `turn` | turno de `-p` e turno interativo sem tools perigosas | `model, messages, system, tools, metadata, max_tokens, thinking, context_management, output_config, stream` | billing, identidade, system prompt |
| `turn_safeguards` | turno interativo com o beta `dangerous-tool-use-2026-09-03` | `model, messages, system, tools, metadata, max_tokens, thinking, context_management, safeguards, output_config, stream` | idem |
| `quota` | sonda de cota do interativo | `model, max_tokens, messages, metadata` | ausente (sem billing header) |
| `title` | título da sessão (interativo) | `model, messages, system, tools, metadata, max_tokens, output_config, stream` | billing, identidade sem `cache_control`, prompt de título |
| `classifier` | classificador do modo auto (interativo), ao decidir se uma tool roda | `model, max_tokens, system, messages, stop_sequences, thinking, metadata`, ou `model, max_tokens, system, messages, thinking, metadata` | billing, prompt do classificador (começa com `You are a security monitor for autonomous AI coding agents.`) com o `cache_control` da requisição, contexto da sessão |
| `count_tokens` | `POST /v1/messages/count_tokens`, antes de anexar um resultado de tool grande | `model, messages, tools`, ou `model, messages, tools, thinking` | ausente (sem billing header) |

- `turn`: `max_tokens: 128000`, `stream: true`; no fallback não-stream `max_tokens: 64000`, `stream: false` e `X-Stainless-Timeout: 300` (mesma ordem de chaves; não capturado). `thinking` `{"type":"adaptive","display":"omitted"}` (`sdk-cli`) ou `"display":"updates"` (`cli`); `context_management` `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`; `output_config` `{"effort":"medium"}`; `tools[]` pode ter `defer_loading: true`.
- `messages`: depois da primeira mensagem `user` vem uma mensagem `role: "system"` com o bloco de ambiente (texto do Claude Code com o diretório de trabalho, plataforma, shell, versão do SO, modelo e data) e `output_config` por mensagem `{"effort":"medium"}` (beta `per-turn-control-2026-07-01`); nos turnos seguintes, `assistant`, `user` e uma `system` curta (`<total_tokens>…</total_tokens>`). `cache_control` dos blocos: `{"type":"ephemeral","ttl":"1h"}` com OAuth e `extended-cache-ttl-2025-04-11`, `{"type":"ephemeral"}` com API key ou com OAuth sem esse beta (subagente, overage; ver "Betas por tipo"). O `Verify` não confere o `cache_control` dos blocos de `messages`, só o de `system[1]`.
- `classifier` (task 457, captura fora do git; tráfego real do modo auto): não-stream (sem a chave `stream`), `X-Stainless-Timeout: 60`, só do entrypoint `cli`. A primeira etapa traz `stop_sequences`, a segunda não. O corpo leva o prompt do classificador e o transcript: o perfil não o gera (`Build` recusa o tipo; sem fixture) e o `Verify` confere só a forma (chaves, betas, timeout, `metadata`, billing header, fingerprint e o começo de `system[1]`).
- `quota`: `{"model":…,"max_tokens":1,"messages":[{"role":"user","content":"quota"}],"metadata":{…}}`.
- `count_tokens` (capturado pela task 442, sem custo: `claude -p --tools Read`, com o servidor de captura respondendo um `tool_use` de `Read` de um arquivo de ~35 KB; o Claude Code conta os tokens antes de anexar o resultado grande): headers do perfil sem `X-Stainless-Timeout`; corpo `model, messages, tools`, sem `system`, `metadata` nem billing header; `thinking: {"type":"adaptive"}` no fim quando alguma mensagem `assistant` tem bloco `thinking` ou `redacted_thinking` (pelo código do binário; não capturado).
- Qualquer outra combinação de chaves de topo não é do perfil (`body_key_order`).

**Betas por tipo** (subconjunto da lista conhecida, sempre na ordem canônica). As listas "inferida" não têm captura: existem para `wwz-load claude-code` (`ask`, `run`) dar 200 no modo estrito em todos os modos e credenciais (spec 015) e ficam "não conferido" na spec 006, seção "Não conferidos no real". Regra da inferência: API key = OAuth sem `oauth-2025-04-20` e `extended-cache-ttl-2025-04-11`, com `inline-tools-2026-09-15` e `advisor-tool-2026-03-01` nos tipos em que essas duas são de flag (a diferença vista no `-p`, que vem das flags e não da credencial; ver "Betas de flag"); `cli` = `sdk-cli` + `thinking-display-updates-2026-08-18` (display `updates`). A tabela traz as listas como capturadas ou inferidas; o `Verify` aceita também as variações de "Betas de flag" e de "Tool search". Os valores estão em `internal/claudecode/profile_2_1_308.go`.

| Tipo, entrypoint e credencial | `anthropic-beta` | Origem |
| --- | --- | --- |
| `turn` `sdk-cli`, OAuth | `claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,effort-2025-11-24,extended-cache-ttl-2025-04-11` | captura 002, 006, 008 |
| `turn` `sdk-cli`, API key | `claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,effort-2025-11-24` | captura 004 |
| `turn` `cli`, OAuth | a de `turn` `sdk-cli` OAuth com `thinking-display-updates-2026-08-18` antes de `extended-cache-ttl-2025-04-11` | inferida, não conferida |
| `turn` `cli`, API key | a de `turn` `sdk-cli` API key + `thinking-display-updates-2026-08-18` | inferida, não conferida |
| `turn_safeguards` `cli`, OAuth | `claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,effort-2025-11-24,dangerous-tool-use-2026-09-03,thinking-display-updates-2026-08-18,afk-mode-2026-01-31,extended-cache-ttl-2025-04-11` | captura 012 |
| `turn_safeguards` `cli`, API key | `claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,effort-2025-11-24,dangerous-tool-use-2026-09-03,thinking-display-updates-2026-08-18,afk-mode-2026-01-31` | inferida, não conferida |
| `quota` `cli`, OAuth | `claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27` | captura 010 |
| `quota` `cli`, API key | a de `quota` OAuth sem `oauth-2025-04-20` | inferida, não conferida |
| `title` `cli`, OAuth | a de `quota` OAuth + `per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,effort-2025-11-24,structured-outputs-2025-12-15` | captura 011 (fora do git) |
| `title` `cli`, API key | `claude-code-20250219,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,effort-2025-11-24,structured-outputs-2025-12-15` | inferida, não conferida |
| `classifier` `cli`, OAuth | `claude-code-20250219,oauth-2025-04-20,context-1m-2025-08-07,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,extended-cache-ttl-2025-04-11` | captura da task 457 (fora do git) |
| `classifier` `cli`, API key | a de `classifier` OAuth sem `oauth-2025-04-20` e sem `extended-cache-ttl-2025-04-11` | inferida, não conferida |
| `count_tokens` `sdk-cli`, OAuth | `claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01` | captura 013 |
| `count_tokens` `sdk-cli`, API key | `claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01` | captura 014 |
| `count_tokens` `cli`, OAuth e API key | as mesmas do `sdk-cli` | inferida, não conferida |

Não há lista de `classifier` do `sdk-cli` (o modo auto é do interativo): esse classificador é recusado com `beta`.

**Betas de flag** (task 457, Claude Code 2.1.308 real em uso normal; conferido no tráfego e nas strings do binário). O mesmo comando manda ou não alguns betas conforme estado que o `Verify` não vê; em cada tipo listado o `Verify` aceita cada um deles presente (na posição canônica) ou ausente, comparando a lista sem eles:
- `inline-tools-2026-09-15` e `advisor-tool-2026-03-01` seguem flags do Claude Code (`tengu_brisk_meadow` e `tengu_sage_compass2`, ou as variáveis `CLAUDE_CODE_INLINE_TOOLS` e `CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL`), com qualquer credencial: com `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` (sem flags) o `claude -p` não manda nenhum dos dois, e as variáveis acrescentam cada um, também com OAuth. Tipos `turn`, `turn_safeguards` e `title`. A diferença OAuth × API key da captura 004 vem dessas flags, não da credencial.
- `extended-cache-ttl-2025-04-11` acompanha o TTL de 1 h do cache, que o assinante só tem na thread principal, no SDK e no classificador, não no subagente nem usando overage. Só com OAuth, nos tipos `turn`, `turn_safeguards` e `classifier`: lá é opcional; com API key segue recusado (`beta`). Sem ele, o `cache_control` de `system[1]` é `{"type":"ephemeral"}` (ver "Identidade").
- Fora desses tipos (`quota`, `count_tokens`; `extended-cache-ttl` em `title`) os betas de flag não são aceitos além da lista da tabela.

**Turno depois de um resultado de tool.** Um `turn` (sem a chave `safeguards`) pode levar a lista de `turn_safeguards` do mesmo entrypoint e credencial: os `safeguards` vão só na requisição de um turno do usuário, os betas ficam para a sessão inteira.

**Tool search.** Nos tipos `turn` e `turn_safeguards`, `advanced-tool-use-2025-11-20` entra na posição canônica (antes de `effort`) quando a ferramenta `ToolSearch` está ligada (captura `full-default-oauth`), não quando há tools deferidas: com `--tools Read` o corpo já traz `DeferredToolPlaceholder` com `defer_loading: true` e a lista não tem o beta. No perfil é o insumo `Request.ToolSearch`; o `Verify` aceita a lista do tipo com ou sem o beta, na posição canônica.

**Ordem canônica**, confirmada pela task 442 na função do binário que monta a lista (sem divergência da observada), com `context-1m` logo depois de `oauth` (task 457): `claude-code`, `oauth`, `context-1m`, `interleaved-thinking`, `redact-thinking`, `thinking-token-count`, `context-management`, `per-turn-control`, `mid-conversation-tool-changes`, `inline-tools`, `advisor-tool`, `advanced-tool-use`, `effort`, `structured-outputs`, `dangerous-tool-use`, `thinking-display-updates`, `afk-mode`, `extended-cache-ttl`, `token-counting`. A lista não é ordenada: é montada por inserção, nesta sequência: betas do modelo (`claude-code`, `oauth`, `context-1m`, `interleaved-thinking`, `redact-thinking`, `thinking-token-count`, `context-management`); `per-turn-control`, `mid-conversation-tool-changes`, `inline-tools`; `advisor-tool`; o beta de tool search; `effort`; `structured-outputs`; os de cada requisição (`dangerous-tool-use`, `thinking-display-updates`, `afk-mode`, `extended-cache-ttl`); e por último `token-counting-2024-11-01`, que o SDK acrescenta no `count_tokens`. Por isso os pares nunca vistos juntos ficam `inline-tools`/`advisor-tool` antes de `advanced-tool-use` e `structured-outputs` antes de `dangerous-tool-use`. `redact-thinking` sai quando a requisição define `thinking.display`, e por isso só aparece em `quota`, `title` e `classifier`. No perfil, `context-1m-2025-08-07` só aparece no `classifier`.

**Billing header (`system[0]`).** `{"type":"text","text":"x-anthropic-billing-header: cc_version=<VER>.<FP>; cc_entrypoint=<EP>;"}`, sem `cache_control`. O subagente acrescenta ` cc_is_subagent=true;` depois de `cc_entrypoint`, com qualquer destino (task 457), sempre junto do header `x-claude-code-agent-id`. Para um `ANTHROPIC_BASE_URL` próprio não há `cch=` nem os campos só do first-party ou de gateway (`cc_workload`, `cc_prev_req`, `cc_prompt_id`, `cc_turn_origin`, `cc_prompt_index`, `cc_turn_index`, `cc_gateway`, `cc_project`), que seguem fora do perfil; `cc_is_subagent` em outra forma ou lugar também.

**Fingerprint.** `<FP>` = os 3 primeiros caracteres hexadecimais minúsculos de `sha256("59cf53e54c78" + t[4] + t[7] + t[20] + VER)`, sobre os bytes UTF-8. `t` é o texto da primeira mensagem `user` do transcript do Claude Code que não é meta (a string, ou o `text` do primeiro bloco `text` dela, `""` sem nenhum); `t[i]` é a unidade UTF-16 de índice `i`, e índice inexistente vira `"0"`. Uma unidade que é metade de um par substituto vira U+FFFD na codificação UTF-8, como o `Buffer` do Node (inferido; sem captura).

No corpo, as mensagens meta do transcript que vêm antes do prompt (lembretes, `CLAUDE.md`, saída de comandos locais como `/context`) viram blocos `text` antes dele na mesma mensagem `user`, e nem sempre têm tag: `/context` gera blocos meta sem tag (`<command-name>/context…`, `<local-command-stdout>…`, `## Context Usage…`), e `!echo hi` gera um bloco `<bash-input>…` que não é meta e é o que entra no hash. Por isso uma regra "pula os blocos `<system-reminder>`" não basta, e o `Verify` aceita o fingerprint de qualquer candidato: a string, quando o `content` da primeira mensagem `user` do corpo é string; senão o `text` de cada bloco `text` dela, mais `""` quando ela tem um bloco que não é `text` (ou nenhum bloco `text`); `""` quando não há mensagem `user`. Nada mais do corpo entra; o `Build` usa o primeiro bloco `text` (as fixtures não têm blocos meta).

**Identidade (`system[1]`).** `You are a Claude agent, built on Anthropic's Claude Agent SDK.` (`sdk-cli`) ou `You are Claude Code, Anthropic's official CLI for Claude.` (`cli`), com o `cache_control` da credencial (sem `cache_control` no tipo `title`), exceto que com OAuth sem `extended-cache-ttl-2025-04-11` na lista ele é `{"type":"ephemeral"}`. No `classifier`, `system[1]` é um bloco `{"type":"text","text":…,"cache_control":…}` com esse mesmo `cache_control`, cujo texto começa com `You are a security monitor for autonomous AI coding agents.` (o resto é o prompt do classificador, fora do perfil).

**`metadata.user_id`.** String com o JSON compacto `{"device_id":"<64 hex minúsculos>","account_uuid":"<UUID ou vazio>","session_id":"<UUID v4>"}`, nessa ordem; `session_id` igual ao header `X-Claude-Code-Session-Id`. `account_uuid` é vazio com `CLAUDE_CODE_OAUTH_TOKEN` ou API key e o `account.uuid` do perfil depois de um login.

**Codificação do corpo (`JSON.stringify`).** JSON compacto, sem espaço fora de strings, chaves na ordem de inserção. Em strings escapa só `"` e `\` e os controles < U+0020 (`\b`, `\t`, `\n`, `\f`, `\r` curtos; os demais `\u00xx` com hexadecimal minúsculo). Não escapa `<`, `>`, `&`, `/`, U+2028 nem U+2029; não-ASCII sai cru em UTF-8. Números no formato `Number.prototype.toString` do ECMAScript (inteiros sem `.0`; expoente a partir de 1e21). O `encoding/json` do Go não serve (escapa `<>&` e U+2028/2029 e ordena mapas): o pacote tem encoder próprio e o cliente monta o corpo de estruturas ordenadas.

### Fixtures (`testdata/real/claude-code/2.1.308/`)
As capturas mínimas 001 (`HEAD /api/hello`), 002 (`-p` OAuth), 004 (`-p` API key), 006 e 008 (1º e 2º turno de uma conversa), 010 (sonda de cota), 012 (turno interativo com `safeguards`), 013 e 014 (`count_tokens` de `-p` com OAuth e com API key, capturadas com `--tools Read`: o corpo leva `tools: []` e o conteúdo do arquivo lido, `lorem ipsum…`, sem descrições de tools do Claude Code), normalizadas: o diretório de trabalho e os caminhos de configuração viram `/work` e `/cfg`, o diretório de scratchpad do bloco de ambiente (derivado do diretório de trabalho e da sessão, com o nome do usuário) vira `/private/tmp/claude-501/-work/<sessão>/scratchpad`, como o Claude Code o derivaria de `/work`, a home `/home/user`, `user_identity` `user`, o token OAuth falso e a chave falsa ficam como estão (não são segredos), e o `Content-Length` é recalculado. O fingerprint não muda (depende só da mensagem do usuário). A captura completa com o prompt padrão (`full-default-oauth.raw`) e a de título (011) nunca entram no git (ADR 0074, item 13). `MANIFEST.md` lista, por arquivo, o modo, o tipo, a normalização e o sha256. A 001 é reconstruída pelo `Build` (tipo `hello`, `User-Agent: Bun/1.4.4`), mas não passa pelo `Verify`: não é rota de inferência (dá `query`).

### Verificação estrita (`client_verification: claude-code`)
Vale nas rotas `POST /v1/messages` e `POST /v1/messages/count_tokens` da montagem do driver `anthropic` com a opção. É o primeiro passo da requisição, antes da credencial, do corpo e dos limites ([ADR 0053](decisions/0053-tokenizador-bpe-e-ordem-chave-corpo.md) vale depois). Precisa da linha e dos headers crus, na ordem e grafia recebidas (spec 004, "Cabeça crua"). A primeira checagem que falha dá a recusa:

| # | `reason` | Recusa quando |
| --- | --- | --- |
| 1 | `query` | a linha não é `POST <caminho>?beta=true HTTP/1.1` (query diferente, ausente ou com outro parâmetro; outra versão de HTTP) |
| 2 | `transfer_encoding` | há `Transfer-Encoding` (corpo chunked) |
| 3 | `header_missing` | não há `Authorization` nem `x-api-key` (o modo de credencial é OAuth com `Authorization`, senão API key com `x-api-key`), ou falta, sem olhar a caixa, algum header do perfil para o modo de credencial e a rota (`count_tokens` sem `X-Stainless-Timeout`; os opcionais 18a e 18b nunca faltam) |
| 4 | `header_order` | todos os headers do perfil estão presentes, mas não exatamente como no perfil, na ordem e grafia: header a mais (inclusive de proxy, ou `x-api-key` junto de `Authorization`), fora de ordem, repetido ou com outra caixa; headers opcionais (18a, 18b) fora do lugar ou com outra caixa |
| 5 | `user_agent` | `User-Agent` fora da forma `claude-cli/<VER> (external, <EP>)` ou `<EP>` fora de `cli`/`sdk-cli` |
| 6 | `unknown_version` | `<VER>` bem formado mas sem perfil (só `2.1.308`) |
| 7 | `header_value` | valor fora do perfil em `Accept`, `Content-Type`, `X-Stainless-*`, `anthropic-dangerous-direct-browser-access`, `anthropic-version`, `x-app`, `Connection`, `Accept-Encoding` ou `Content-Length`; `X-Claude-Code-Session-Id` que não é UUID v4 minúsculo; `x-claude-code-agent-id` ou `x-claude-code-parent-agent-id` fora da forma do agent id; depois da checagem 10 (e do segundo passo de `beta`), `X-Stainless-Timeout` `60` fora do `classifier`, ou outro valor no `classifier` |
| 8 | `beta` | beta desconhecido, fora da ordem canônica, sem `claude-code-20250219`, lista que não é a de nenhum tipo da rota para a credencial e o entrypoint (com as variações de "Betas de flag", "Tool search" e do turno depois de um resultado de tool), ou `oauth-2025-04-20` sem `Authorization: Bearer sk-ant-oat…` (ou o contrário); depois da checagem 10, lista que não é a do tipo do corpo (ver abaixo) |
| 9 | `body_encoding` | o corpo não é JSON válido em UTF-8, tem chave repetida ou difere byte a byte da re-serialização `JSON.stringify` dele mesmo |
| 10 | `body_key_order` | as chaves de topo não são as de um tipo do perfil, na ordem |
| 11 | `metadata` | `metadata.user_id` ausente ou fora da forma (ordem, `device_id` de 64 hex minúsculos, `account_uuid` UUID ou vazio, `session_id` UUID v4) |
| 12 | `session_mismatch` | `metadata.user_id.session_id` diferente de `X-Claude-Code-Session-Id` |
| 13 | `billing_header` | `system[0]` ausente ou fora da forma, `cc_version` com versão diferente da do `User-Agent`, `cc_entrypoint` diferente do `<EP>`, `cch=` ou campo de first-party presente (inclusive `cc_is_subagent` fora do sufixo ` cc_is_subagent=true;`); `cc_is_subagent=true;` sem `x-claude-code-agent-id`, ou o contrário |
| 14 | `fingerprint` | o `<FP>` de `cc_version` não é o de nenhum candidato da primeira mensagem `user` (ver "Fingerprint") |
| 15 | `identity` | `system[1]` ausente, ou não é a identidade do `<EP>` (no `classifier`, um bloco que começa com a frase do classificador) com o `cache_control` da credencial e do tipo, inclusive `cache_control` que não casa com a presença de `extended-cache-ttl-2025-04-11` |

**`beta` em dois passos.** O tipo só é conhecido no corpo (checagem 10). Na checagem 8 a lista tem de ser a de algum tipo da rota (`count_tokens` na rota de contagem; os demais em `/v1/messages`) para a credencial e o entrypoint; logo depois da 10, tem de ser a do tipo do corpo, e a recusa ainda é `beta`. Em seguida vem o `X-Stainless-Timeout` do tipo (`60` só no `classifier`), com `header_value`. No tipo `count_tokens` a verificação para aí: as checagens 11–15 não se aplicam (não há `metadata` nem `system`). No tipo `quota` ela para depois da 12 (não há `system`; um `system` presente já é `body_key_order`).

O `<detalhe>` diz o que divergiu (ex.: `header_order: expected "X-Stainless-Arch" at position 6, got "x-stainless-arch"`, `fingerprint: expected 9fe, got 000`; com vários candidatos, `fingerprint: expected 031 or 7f2, got 000`). O conteúdo das mensagens, o modelo e os valores de `thinking`/`output_config` não são conferidos além da forma do tipo (a validação real do driver segue depois).

**Recusa.** HTTP 403, headers `content-type: application/json`, `request-id: req_…`, `x-should-retry: false` e `x-wwz-client-verification: failed; reason=<reason>`; corpo:

```json
{"type":"error","error":{"type":"forged_request_error","message":"Client verification failed: this request does not match Claude Code 2.1.308; the client is forging Claude Code requests (<reason>: <detalhe>)."},"request_id":"req_…"}
```

`<versões>` são as versões do perfil, separadas por `, `. A recusa não debita cota nem crédito, não conta em limite, concorrência nem abuso, e sai com latência zero. Conta em `wwz_requests_total{status="403"}` e em `wwz_client_verification_failures_total{driver,reason}`, vai ao log e ao `ev:stream` como `error` (spec 013).

**Mutações de teste** (aplicadas a uma fixture aceita; cada uma dá o `reason` indicado):

| Mutação | `reason` |
| --- | --- |
| sem `?beta=true` | `query` |
| `Transfer-Encoding: chunked` no lugar de `Content-Length` | `transfer_encoding` |
| sem `X-Stainless-Timeout` (numa fixture de `/v1/messages`) | `header_missing` |
| `X-Stainless-Arch` trocado de lugar com `X-Stainless-Lang`; `user-agent` minúsculo; header `X-Forwarded-For` a mais | `header_order` |
| `User-Agent: claude-cli/2.1.308 (external, sdk-py)` | `user_agent` |
| `User-Agent: claude-cli/2.1.309 (external, sdk-cli)` | `unknown_version` |
| `X-Stainless-Package-Version: 0.129.0` | `header_value` |
| betas fora de ordem; beta inventado; `oauth-2025-04-20` com `x-api-key` | `beta` |
| espaço depois de `:` no corpo; `<` escapado como `\u003c` | `body_encoding` |
| `stream` antes de `model` | `body_key_order` |
| `device_id` com 63 hex | `metadata` |
| `session_id` diferente do header | `session_mismatch` |
| `cch=00000;` no billing header; `cc_version=2.1.307.9fe` | `billing_header` |
| fingerprint `000` | `fingerprint` |
| `system[1]` com a identidade de `cli` numa requisição `sdk-cli` (billing header intacto) | `identity` |
| SDK oficial `anthropic` (Python e Node): `messages.create` e `beta.messages.create` (nenhum dos dois acrescenta `?beta=true`) | `query` |
| a requisição do SDK oficial com `?beta=true` forçado (falta `X-Claude-Code-Session-Id`) | `header_missing` |
| a mesma, com os headers que faltam acrescentados | `header_order` |

Toda mutação de corpo recalcula o `Content-Length`, para a recusa sair pela checagem pretendida. Linhas dos SDKs capturadas em 2026-11-03 com o Python `anthropic` 2.3.0 e o Node `@anthropic-ai/sdk` 1.3.0; nas capturas, os dois mandaram a credencial como `Authorization: Bearer`, e por isso o modo de credencial do `Verify` é OAuth.

**Proxy.** O modo estrito exige um caminho que entregue a linha e os headers como o cliente os mandou: nem proxy L7 (Traefik, haproxy em `mode http`, Go `net/http`), que canoniza nomes, tira hop-by-hop como `Connection` e acrescenta `X-Forwarded-For` (recusa `header_missing` ou `header_order`). Só TCP puro (L4) ou conexão direta. Em produção ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), item 19), só a porta 8008 do IP público da produção (haproxy em `mode tcp` para a instância `anthropic`); por uma porta que o servidor atende em `mode http` (no servidor do autor, a 8004, dividida com outro serviço do host) ou pelo HTTPS do Traefik, o mount `/claude-code` recusa todo cliente.

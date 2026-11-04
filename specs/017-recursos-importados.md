# 017 — Recursos importados das pesquisas (I1–I24)

Status: rascunho
Depende de: 007, 009, 011
Origem: seções 22 do documento Claude Docs congelado em 2026-10-30; contratos do `wwz-tools` e do `wwz-drift` detalhados para a fase 4 em 2026-10-31 (task 109)

## Contexto
Cruzamento das pesquisas externas com a especificação e os 24 recursos que entraram no MVP (seção 22).

## Requisitos
- I1–I24 (tabela abaixo)

## Interfaces e contratos
- Ver coluna "Entra em" de cada recurso, mapeada para as specs pela tabela de correspondência do INDEX
- `wwz-tools`: seção "Contrato do `wwz-tools` (fase 4)" e `cmd/wwz-tools/openapi.yaml` ([ADR 0034](decisions/0034-contrato-do-wwz-tools.md))
- `wwz-drift`: seção "Contrato do `wwz-drift` (fase 4)" ([ADR 0035](decisions/0035-wwz-drift-comparacao-de-forma.md))

## Dados
- `attempts:` e escopo por `X-Test-Id` no store

## Critérios de aceite
- [ ] Cada recurso I1–I24 tem teste que o exercita
- [ ] `internal/tools`: mesma semente e mesma sequência de chamadas dão resultados idênticos; nenhum caminho sai da raiz virtual; nenhuma chamada toca disco ou rede do host (teste com `run_command` e `fetch_url`)
- [ ] `wwz-tools serve`: as rotas seguem `cmd/wwz-tools/openapi.yaml`; `x-wwz-tools-outcome` força `error`, `timeout` e `invalid_payload`; com `WWZ_TOOLS_TOKEN`, chamada sem bearer devolve 401
- [ ] `wwz-drift check` contra provedores falsos (`--base-url`) detecta campo faltante, campo extra, tipo trocado, ordem trocada, sequência SSE diferente e header faltante, com código 3; sem divergência, código 0; provedor sem chave sai `skipped`
- [ ] `wwz-drift check` com chave no ambiente, sem `--base-url` e sem `--real` não faz nenhuma chamada ao real: o provedor sai `skipped`/`real_not_confirmed`; com `--real` (ou com `--base-url`) sonda, e `providers[].base` traz a base usada
- [ ] `wwz-drift check` do `opencode` usa uma sessão por execução (`providers[].session`), trata o `id` como equivalente e sai `skipped`/`upstream_form_unreproduced` quando 3 sessões não devolvem `gw-` (só `chatcmpl-`, 32 hex ou id longo), com sondas de `max_tokens: 256`
- [ ] `wwz-drift check` nunca escreve a chave em stdout, stderr ou relatório (teste com chave sentinela) e respeita o teto de `tmp/real-spend.json`
- [ ] Regressão com as respostas reais de `tmp/drift/2026-10-31-raw/` (copiadas para `internal/drift/testdata/`, sem ids de conta): a sonda `models` compara só o topo e o item da sonda (sem falso positivo de fusão); `chat_stream` lista todas as diferenças de uma vez (ex.: `ping` ausente e `message_delta` diferente na mesma execução); comentário SSE antes do primeiro evento não gera divergência

## Fora de escopo
- Itens da lista "Descartado após verificação"

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [0026](decisions/0026-precedencia-chave-sobre-modelo.md) (I3), [0027](decisions/0027-formas-finais-dos-recursos-importados.md) (I2, I5, I8, I10, I12, I13, I16, I22, I23, I24), [0043](decisions/0043-drivers-gemini-e-azure.md) (I17, I18; substitui a [0028](decisions/0028-gemini-e-azure-fora-do-mvp.md))
- [0034](decisions/0034-contrato-do-wwz-tools.md) (`wwz-tools`), [0035](decisions/0035-wwz-drift-comparacao-de-forma.md) (I24, `wwz-drift check`), [0049](decisions/0049-formas-reais-do-primeiro-drift.md) (sondas, `/models` por item, SSE alinhado)

## Comportamento

### 22. Cruzamento com a especificação e recursos importados

Das \~70 capacidades levantadas nas cinco pesquisas, 41 já estavam cobertas pelas seções 1–20, 24 entram agora como recursos novos e 5 confirmam decisões já tomadas; nada do que foi verificado contradiz a arquitetura (Go, drivers, Valkey, cenários stateless). A tabela abaixo é a lista de importação: cada linha diz de onde vem, em que seção entra e qual chave de configuração ganha.

**Nome do projeto.** O projeto chama-se **White Wizard**; prefixos `wwz` (binários, headers, métricas, pacotes) e `WWZ` (variáveis de ambiente, siglas). O nome de trabalho anterior, `llmsim`, foi abandonado porque colide com um projeto Rust publicado ([chaliy/llmsim](https://github.com/chaliy/llmsim)); toda ocorrência de `wwz`/`WWZ` neste documento refere-se ao White Wizard, e `chaliy/llmsim` continua designando o projeto externo. Binários: `wwz` (servidor), `wwz-load`, `wwz-panel`, `wwz-tools`, `wwz-drift`; pacote de teste `wwztest`; módulo Go `github.com/<org>/white-wizard`.

**Recursos novos importados**

| # | Recurso | Origem | Entra em | Configuração |
| --- | --- | --- | --- | --- |
| I1 | Catálogo *tool misbehavior*: JSON inválido com estilo (`truncated`, `trailing_comma`, `single_quotes`), violação de schema (`missing_required`, `wrong_type`, `extra_property`, `enum_mismatch`, `not_object`), tool desconhecida, id duplicado, `finish_reason: length` no meio dos argumentos, resposta vazia, só raciocínio | AIMock, LLMock | 7 (erros) | `errors.tool_misbehavior: {rate, kinds: [...], tool, drivers}` |
| I2 | Caos em camadas com escopo de teste: global → modelo → chave → cenário/passo → headers `x-wwz-chaos-*` → `POST /admin/chaos` → `POST /admin/chaos/next`, todos escopados por `X-Test-Id`; erro one-shot para o próximo request de um `X-Test-Id` ([ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)) | AIMock, DotMock | 7, 10 | `chaos.scope_header: X-Test-Id`; `x-wwz-chaos-profile`, `x-wwz-chaos-rate`, `x-wwz-chaos-types`, `x-wwz-chaos-mid-stream-rate`, `x-wwz-chaos-mid-stream-types`; `POST /admin/chaos/next {test_id, status, body}` |
| I3 | Precedência explícita de parâmetros: header por requisição > passo de cenário > chave > modelo > global, inclusive para `seed` (`x-wwz-seed`) ([ADR 0026](decisions/0026-precedencia-chave-sobre-modelo.md)) | DotMock | 4, 6 | `precedence: [header, step, key, model, global]` (fixo, documentado) |
| I4 | Controle da resposta por header: `x-wwz-response`, `x-wwz-response-base64`, `x-wwz-tool-calls` (ignora `tool_choice`), `x-wwz-content-filter`, `x-wwz-return-error`, `x-wwz-delay-ms`, `x-wwz-tool-prob` | Axium, llm-d, ai-mock, daemon Go (21.5) | 3, 9 | `server.request_headers: true` (desligável para medir vazão pura) |
| I5 | Pacing explícito além das distribuições: `tokens_per_second`, `jitter`, `inter_chunk_delays: [...]`, `total_duration`, `chunk_runes`, `subword_streaming`, `dribble_chunks` + `dribble_total` (durações Go, ADR 0027) | DotMock, MockServer, WireMock | 6 | `latency.pacing: {...}` como alternativa a `ttft/itl` |
| I6 | Timing por passo de cenário: `ttft_ms`, `tokens_per_sec`, `stall {after_tokens, ms}`, `cut_after_tokens`, `speed` multiplicador | chaliy/llmsim | 9 | campo `latency` e `speed` no passo (já havia `latency`; ganha `stall`, `cut_after_tokens`, `speed`) |
| I7 | `fail_first`: erro só nas N primeiras tentativas de um passo, com `retry_after_ms`; rastreador de tentativas com TTL | chaliy/llmsim | 9, 18 | `fail_first: {times, error, retry_after_ms}`; chave Valkey `attempts:{conv}:{step}` (TTL 1 h) |
| I8 | Ids de tool call decodificáveis (`call_<cenário>_<passo>_<n>_<nonce>`, `toolu_…` nas rotas Messages), para o próximo turno identificar o passo pelo id; `native` devolve o id real do driver | chaliy/llmsim | 9 | `tool_call_id_format: encoded \| native` no cenário (padrão `encoded`, ADR 0027) |
| I9 | Tool placeholder por classe: `tool: any \| any_read \| any_write` escolhendo entre as tools do request; 400 quando o passo nomeia tool não oferecida | chaliy/llmsim | 9 | `name` em `tool_calls[]` do passo |
| I10 | Seleção de cenário por marcador na mensagem `[[wwz:nome seed=N speed=N]]` (apelidos `[[sim:` e `[[llmsim:`) e por model id `<modelo>-scenario-<nome>` | chaliy/llmsim | 9 | `scenarios.select: [header, metadata, marker, model_suffix, match]` |
| I11 | Conversa identificada por header `x-wwz-conversation-id` além do hash da 1.ª mensagem; `POST /admin/conversations/reset` | fakellm | 9 | `scenarios.conversation_id: header \| hash` |
| I12 | Predicados de conversa adicionais: `latest_message_role`, `tool_result_for: <tool>`, `semantic_match` (cosseno lexical sobre palavras, sem embeddings, ADR 0027) | MockServer | 9 | campos em `expect` |
| I13 | Turno misto (passo com `content` e `tool_calls`), `on_exhausted: repeat_last \| error \| fallback`, `step_limit` (guarda contra loop); `repeat_last` = último passo do arquivo (ADR 0027) | chaliy/llmsim, ManagedCode | 9 | campos do cenário |
| I14 | Timeline explícita de resposta (sequência de eventos `text`, `reasoning`, `tool_call`, `usage`, `ping`, `error`, `finish`) exposta em YAML para cenários avançados | zerob13 | 9, 17 | `timeline: [...]` no passo, mapeado 1:1 em `core.StreamWriter`; `usage` sem frame próprio (vai no `finish`), eventos após `finish` ignorados, `error` vira a resposta no não-stream, só placeholders simples no texto (spec 011) |
| I15 | Relatório de resiliência do cliente: o simulador registra, por `X-Test-Id`, se o roteador retentou, com que backoff, se respeitou `Retry-After`, se reenviou após consumir stream (`streams_consumed`, `resent_after_stream`: requisição com o mesmo corpo do stream imediatamente anterior do teste); `GET /admin/resilience?test_id=` | LLMock | 10, 19 | `resilience.track: true`; tela "Resiliência" no painel |
| I16 | Decomposição dos tokens de entrada por origem (system, schemas de tools, user, assistant, tool calls, tool results, imagens, cache) no painel e em header de resposta opcional; corpo e `usage` intactos (ADR 0027) | ManagedCode | 10, 19 | `usage.extended: true` → header `x-wwz-input-breakdown` (JSON) |
| I17 | Content filter no dialeto certo de cada provedor (OpenAI 400 `content_filter`/200 `finish_reason: content_filter`, Anthropic 200 `refusal`, Gemini `finishReason: SAFETY`/`promptFeedback.blockReason`, Azure 400 `content_filter` com `innererror.content_filter_result` e 200 com `content_filter_results`; [ADR 0043](decisions/0043-drivers-gemini-e-azure.md), formas na spec 006) | MockServer | 7, 17 | `moderation.mode` por driver |
| I18 | Raciocínio no campo nativo de cada dialeto, incluindo `signature` do bloco `thinking` (Anthropic) e `<thinking>` inline no `content` para modelos sem campo; partes `thought: true` do Gemini e raciocínio oculto do Azure como na OpenAI (ADR 0043, 0046) | DotMock, MockServer, zerob13 | 5, 17 | `reasoning_field: hidden \| reasoning_content \| reasoning \| inline_tag` |
| I19 | Faults de transporte nomeados: `empty_response`, `malformed_chunk`, `random_data_then_close`, `connection_reset`, `corrupt_body` (texto puro com 200), `duplicate_frames` | WireMock, llmposter | 7 | tipos novos em `errors.types` |
| I20 | Latência polinomial por carga (prefill ∝ tokens de entrada, decode ∝ KV ativo: `itl`, `reasoning_itl` e `tool_args_itl` × (1 + 0,05 × ativas); fórmulas e cache de prompt na spec 008) e `speedup_ratio` global | Dynamo Mocker | 6 | `latency.model: constant \| per_token \| polynomial`; `latency.speedup_ratio` |
| I21 | Relógio lógico: modo em que a latência é contabilizada mas não dormida, para testar agendamento do roteador sem gastar tempo real | SGLang Simulator | 6, 11 | `latency.clock: real \| logical` |
| I22 | Teto de chunks por stream e teto de séries de métricas (por métrica, lido só no boot); estatísticas finalizadas em drop de stream | Httpbun, chaliy/llmsim | 11 | `server.max_chunks_per_stream: 100000`; `metrics.max_series: 5000` |
| I23 | Hot reload que mantém a configuração anterior quando o novo YAML tem erro; `--validate-on-load` faz cenário inválido abortar o boot e recusar a recarga inteira (ADR 0027) | llmposter, AIMock | 4, 10 | padrão em SIGHUP e `/admin/config`; `wwz serve --validate-on-load` / `WWZ_VALIDATE_ON_LOAD=1` |
| I24 | Drift detection: job agendado (`.github/workflows/drift.yml`, inativo até o remoto existir; ADR 0035) que compara os dialetos dos drivers com as APIs reais (schema das respostas de `/v1/models` e de um chat mínimo) e acusa divergência | AIMock | 15 (fase 4; `record` e fixtures na fase 5), 17 | `cmd/wwz-drift` com chaves reais opcionais |

**Já coberto, confirmado pelas pesquisas** (sem mudança): tool calls aleatórias pelo JSON Schema (llm-d, chaliy/llmsim, DotMock), raciocínio em `reasoning_content` com `reasoning_tokens` (chaliy/llmsim, FakeAI, MockServer), distribuições de latência com desvio (llm-d, MockServer, WireMock), erros por taxa com `Retry-After` (todos), `x-ratelimit-*` consistentes com rpm/tpm (Roy), erros mid-stream como evento (dwmkerr, OpenRouter), record/replay (fase 4), benchmarks k6 empacotados (chaliy/llmsim → seção 14), `GET /…/stats` e Prometheus, uso como biblioteca de teste (`wwztest`), tipos oficiais dos SDKs Go (kagent → já na seção 11 como decisão de usar `openai-go` e `anthropic-sdk-go` para os tipos de requisição e resposta).

**Ideias de teste adotadas** (vão para a metodologia da seção 14):

- Fragmentar os argumentos das tool calls em pedaços pequenos (12–24 bytes) em todo teste com streaming, para expor roteadores que acumulam deltas.
- Teste de backpressure: simulador sem latência e clientes lentos, medindo memória do roteador.
- Teste de pool de conexões: acima de 2 000 req/s, verificar keep-alive/HTTP/2 do roteador para o upstream e TIME\_WAIT no host.
- Separar corrida de vazão (sem delay, corpo pequeno) de corrida de realismo (TTFT/ITL/jitter); nunca misturar conclusões.
- Servidor de ferramentas fake (`wwz-tools`, fase 4; contrato abaixo) com sucesso/timeout/erro/payload inválido, fechando o ciclo tool call → execução → resultado.
- Desativar retries do SDK/roteador ao medir injeção de falhas, e ligá-los ao medir resiliência (I15).
- Seed por cenário registrada em cada requisição (`x-wwz-seed` na resposta) para reproduzir falhas.

**Descartado após verificação**: EpicAI, "LLM Emulator (timcubb)" e openai-stream-mocker (inexistentes); DotMock como alvo local (SaaS); patrickjm/openai-mock-api (arquivado); números de desempenho do FakeAI (sem método); "adaptador sintético" entre roteador e mock (no nosso desenho o núcleo já é o adaptador).

## Contrato do `wwz-tools` (fase 4)

Normativo ([ADR 0034](decisions/0034-contrato-do-wwz-tools.md)). Servidor de ferramentas falsas que fecha o ciclo tool call → execução → resultado. O núcleo `internal/tools` é usado em processo pelo `wwz-load agents` (spec 015) e servido por HTTP pelo binário `wwz-tools`. Nada é executado de verdade: o sistema de arquivos é virtual, os comandos são simulados e não há acesso à rede.

### Catálogo

Perfis: `coder`, `pesquisa`, `chat` (sem ferramentas) e `all` (união). Todos os schemas são `type: object` com `additionalProperties: false`.

| Ferramenta | Perfis | Argumentos (obrigatórios em **negrito**) | Resultado de sucesso (`content`) |
| --- | --- | --- | --- |
| `read_file` | coder, pesquisa | **`path`** string, `offset` int ≥ 0, `limit` int 1–2000 (linhas) | Texto do arquivo (linhas `offset`…`offset+limit`) |
| `write_file` | coder | **`path`**, **`content`** string | `ok: <n> bytes escritos em <path>` |
| `edit_file` | coder | **`path`**, **`old`**, **`new`** string, `replace_all` bool | `ok: <k> substituições em <path>`; `old` ausente → erro natural |
| `list_dir` | coder | `path` (padrão `.`), `recursive` bool | Uma entrada por linha, diretórios com `/` final, ordem lexicográfica |
| `search_code` | coder | **`pattern`** string (substring, não regex), `path` | `arquivo:linha: trecho`, no máximo 50 linhas |
| `run_command` | coder | **`command`** string, `timeout_ms` int | JSON `{"exit_code", "stdout", "stderr"}`: `go test`/`npm test`/`pytest` → saída de teste plausível (falha com probabilidade `tools.run_command.fail_rate`); `ls`, `cat`, `grep`, `pwd` respondem pelo sistema virtual; outro comando → `exit_code: 127`, `command not found` |
| `web_search` | pesquisa | **`query`** string, `max_results` int 1–10 (padrão 5) | JSON `{"results": [{"title", "url", "snippet"}]}` determinístico por (semente, query), URLs em `https://example.invalid/…` |
| `fetch_url` | pesquisa | **`url`** string (http/https), `max_bytes` int | Texto de 200–4000 bytes determinístico por (semente, url) |

O catálogo sai em três formatos (`GET /v1/tools?format=`): `openai` (`[{"type": "function", "function": {"name", "description", "parameters"}}]`), `anthropic` (`[{"name", "description", "input_schema"}]`) e `raw` (`[{"name", "description", "schema", "profiles"}]`).

### Sandbox

- **Sessão** = um sistema de arquivos virtual em memória criado na primeira chamada com aquele `session` (ou por `POST /v1/sessions`). É semeado por `(seed, profile)` com uma árvore de projeto fixa de 12–30 arquivos (`README.md`, `go.mod`, `cmd/…`, `internal/…`, `docs/…`) com conteúdo lorem determinístico.
- Caminhos relativos à raiz `/`, normalizados (`path.Clean`); quem sai da raiz (`..`, absoluto fora de `/`) recebe o erro natural `path outside sandbox`.
- Tetos: arquivo ≤ 64 KiB, sessão ≤ 4 MiB e ≤ 1 000 arquivos (acima → erro natural `no space left on device`); `max_sessions` (padrão 100 000, despejo LRU) e `session_ttl` (padrão `30m` de inatividade).
- **Determinismo**: com a mesma semente, a mesma sessão e a mesma sequência de chamadas, conteúdo, latência e resultado injetado são idênticos. A semente vem de `x-wwz-seed`, do campo `seed` da sessão ou de `--seed`; `0` = aleatória.

### Resultados

| `outcome` | Efeito |
| --- | --- |
| `success` | Resultado da tabela do catálogo, `is_error: false` |
| `natural_error` | Erro que a ferramenta real daria: argumentos fora do schema, arquivo inexistente, `old` não encontrado, caminho fora da sandbox. `is_error: true`, mensagem no estilo POSIX (`ENOENT: no such file or directory, open 'x'`) |
| `error` (injetado) | `is_error: true` com mensagem sorteada entre `permission denied`, `EIO: i/o error`, `resource temporarily unavailable` e `internal tool error` |
| `timeout` (injetado) | Espera `timeout` (padrão `30s`). Em processo, devolve `is_error: true` e `content: "tool timed out after 30s"`; por HTTP, `504 {"error": {"type": "timeout", "message": …}}` |
| `invalid_payload` (injetado) | `is_error: false`, mas com `content` inválido para o formato prometido: JSON truncado (em `run_command`/`web_search`), bytes não UTF-8 ou texto vazio, sorteados por igual |

As taxas ficam em percentual (0–100) na configuração. O sorteio é por chamada, nesta ordem: `timeout`, `error`, `invalid_payload`, senão execução normal (que pode resultar em `natural_error`). `x-wwz-tools-outcome: success|error|timeout|invalid_payload` força o resultado de uma chamada por HTTP; em processo, o equivalente é o campo `ForceOutcome`.

### Configuração (`--config tools.yaml`)

```yaml
listen: ":9200"
seed: 0
timeout: 30s
max_sessions: 100000
session_ttl: 30m
latency: {min: 5ms, max: 50ms}        # uniforme; por ferramenta em tools.<nome>.latency
outcomes: {error: 0, timeout: 0, invalid_payload: 0}
tools:
  run_command: {fail_rate: 10, latency: {min: 200ms, max: 2s}}
  web_search: {latency: {min: 100ms, max: 800ms}}
  fetch_url: {latency: {min: 50ms, max: 500ms}, outcomes: {error: 2}}
```

Por ferramenta valem `latency`, `outcomes` (substitui as taxas globais campo a campo) e `fail_rate` (só `run_command`). Variáveis: `WWZ_TOOLS_LISTEN`, `WWZ_TOOLS_SEED` e `WWZ_TOOLS_TOKEN` (bearer; vazio = sem autenticação). Precedência: flag > env > arquivo > padrão. Campo desconhecido ou valor fora da faixa dá erro com o caminho do campo.

### HTTP (`wwz-tools serve`)

`net/http`, JSON, corpo ≤ 1 MiB. Com `WWZ_TOOLS_TOKEN`, toda rota exceto `/healthz` exige `Authorization: Bearer <token>` (senão 401). Contrato completo em `cmd/wwz-tools/openapi.yaml`.

| Rota | Corpo | Resposta |
| --- | --- | --- |
| `GET /v1/tools?profile=coder&format=openai` | — | Catálogo do perfil no formato pedido (padrão `profile=all`, `format=raw`); perfil ou formato inválido → 400 |
| `POST /v1/sessions` | `{"profile", "seed"}` | `201 {"session", "profile", "seed", "files"}` |
| `DELETE /v1/sessions/{id}` | — | `204`; inexistente → 404 |
| `POST /v1/call` | `{"session", "name", "arguments", "tool_call_id"}`; `arguments` é objeto JSON ou string com JSON (como chega da OpenAI) | `200 {"tool_call_id", "content", "is_error", "outcome", "latency_ms"}`; ferramenta desconhecida → 404; corpo inválido → 400; `timeout` → 504 |
| `GET /healthz` | — | `200 ok` |
| `GET /metrics` | — | Prometheus: `wwz_tools_calls_total{tool,outcome}`, `wwz_tools_call_duration_seconds{tool}` (histograma), `wwz_tools_sessions` |

Erros vão no envelope `{"error": {"type", "message"}}`. Sessão desconhecida em `POST /v1/call` é criada com o perfil `all`.

### CLI

| Subcomando | O que faz |
| --- | --- |
| `serve [--listen :9200] [--config ARQ] [--seed N]` | Sobe o servidor HTTP; `SIGINT`/`SIGTERM` encerram com graça (5 s) |
| `list [--profile P] [--format openai\|anthropic\|raw]` | Imprime o catálogo em JSON |
| `call --name N [--args JSON] [--profile P] [--seed N] [--outcome O] [--config ARQ]` | Executa uma chamada em processo numa sessão nova e imprime o resultado JSON; `--config` lê o mesmo `tools.yaml` do `serve` (latência, taxas de resultado, `timeout`) |
| `version` | Versão, commit e versão do Go |

Códigos de saída: `0` sucesso (inclusive `call` com `is_error: true`), `1` falha de execução (porta ocupada, configuração inválida), `2` uso inválido.

## Contrato do `wwz-drift` (fase 4)

Normativo ([ADR 0035](decisions/0035-wwz-drift-comparacao-de-forma.md)). Implementa I24: compara a **forma** das respostas dos drivers do wwz com a das APIs reais. A comparação byte a byte contra fixtures (`wwz-drift record`, `testdata/real/`) é da fase 5 (ADR 0014).

### Subcomandos

| Subcomando | Fase | O que faz |
| --- | --- | --- |
| `check` (padrão) | 4 | Sondas ao vivo, real × wwz, e relatório de divergências |
| `record` | 5 | Grava as respostas reais das sondas como fixtures normalizadas (seção "`record`" abaixo; ADR 0014) |
| `version` | 4 | Versão, commit e versão do Go |

### Flags de `check`

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--providers LISTA` | `openai,anthropic,openrouter,opencode,gemini,opencode-google` | Provedores a sondar. `opencode-google` é a rota `<base>/google/v1beta/models/{m}:generateContent` do OpenCode (driver `opencode`, chave `OPENCODE_API_KEY`). `azure` fica fora da lista padrão: sem `--base-url` não há base real e o provedor sai `skipped`/`no_key`; com `--base-url azure=URL` é sondado com `AZURE_OPENAI_API_KEY` e a rota de deployment (o deployment é o id do modelo), [ADR 0043](decisions/0043-drivers-gemini-e-azure.md) e [ADR 0063](decisions/0063-lote-da-rodada-6-b-cachedcontents-tools-no-cache-e-endpoint.md) |
| `--keys-file ARQ` | `~/.config/white-wizard/real-keys.env` | Arquivo `CHAVE=valor` com `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`, `OPENCODE_API_KEY`, `GEMINI_API_KEY` (alias aceito: `GEMENI_API_KEY`, a grafia do arquivo de chaves) e `AZURE_OPENAI_API_KEY`. A variável de ambiente de mesmo nome vence o arquivo. Arquivo ausente no caminho padrão não é erro; ausente quando passado explicitamente → código 1 |
| `--wwz URL` | — (em processo) | Base do data plane do wwz a comparar. Sem a flag, sobe o wwz em processo (`internal/server` com a configuração embutida `internal/drift/wwz.yaml`, que monta os drivers sondados com os ids de modelo das sondas) em `127.0.0.1:0` |
| `--wwz-key K` | `wwz-key-drift` | Chave enviada ao wwz |
| `--model P=M` | tabela abaixo | Modelo da sonda por provedor, repetível |
| `--base-url P=URL` | URLs reais (spec 006) | Troca a base do provedor real (os testes usam `httptest`) |
| `--real` | desligado | Confirma sondar a base real. Um provedor com chave no ambiente ou no arquivo e **sem** `--base-url` só chama o real com `--real`; sem ele sai `skipped` com o motivo `real_not_confirmed` e nenhuma chamada vai ao real (task 232; evita gasto por engano em teste e em CI sem chaves intencionais). Com `--base-url P=URL` o provedor é sondado sem `--real` (o destino é explícito) |
| `--probes LISTA` | todas | Subconjunto de `models,chat,chat_stream,error_auth,error_model,responses,responses_stream`. As duas últimas (`POST` da Responses sem e com stream, mesmo prompt e `max_output_tokens`; ADR 0055, task 239) valem nos provedores com rota Responses (`openai`, `openrouter`, `opencode`, `azure`); nos demais (`anthropic`, `gemini`, `opencode-google`) saem `skipped` com o motivo `not_supported` |
| `--ignore ARQ` | — | YAML com divergências aceitas, somadas à lista embutida: `[{provider, probe, kind, path, reason}]` (`reason` obrigatório) |
| `--budget-file ARQ` | `tmp/real-spend.json` | Gasto acumulado por provedor |
| `--budget-usd N` | `20` | Teto por provedor ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md), que sobe o teto da ADR 0014) |
| `--timeout D` | `60s` | Teto por sonda |
| `--save-raw DIR` | — | Grava as respostas brutas (sem headers de autenticação) para depuração; `DIR` precisa estar sob `tmp/` |
| `--output text\|json` | `text` | Formato do relatório |
| `--out ARQ` | stdout | Arquivo do relatório |

Modelos padrão das sondas: fixos, baratos, vivos e listados em `/models` dos dois lados ([ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)):

| Provedor | Modelo | No `internal/drift/wwz.yaml` |
| --- | --- | --- |
| `openai` | `gpt-4o-mini` | `id: gpt-4o-mini`, `version: gpt-4o-mini-2024-07-18`, `retires_at: 2027-01-14` |
| `anthropic` | `claude-haiku-4-5-20251001` | `id` datado, `aliases: [claude-haiku-4-5]` |
| `openrouter` | `openai/gpt-4.1-nano` | `id: openai/gpt-4.1-nano`, `version: openai/gpt-4.1-nano-2025-04-14` |
| `opencode` | `deepseek-v4.1-flash` | modelo com raciocínio e `reasoning_field: reasoning_content` |
| `gemini` | `gemini-3.5-flash-lite` | `id: gemini-3.5-flash-lite` (o 2.5 saiu da tabela; também é o modelo de `opencode-google`) |
| `azure` | `gpt-4o-mini` | deployment `gpt-4o-mini`; só com `--base-url` |

O mesmo id vai ao real e ao wwz; não há descoberta pelo primeiro da lista, e `--model` troca o modelo nos dois lados. O `gpt-4.1-nano` saiu da OpenAI em 2026-10-23 (`shutdown_date`). Quando o real desliga um modelo padrão, a troca é registrada no CHANGELOG, e o novo modelo precisa estar vivo na lista real.

### Sondas

Cada sonda vai ao real e ao wwz com o mesmo corpo e o mesmo modelo. Os caminhos reais seguem a spec 006 e os do wwz seguem o mount de cada driver (spec 004).

| Sonda | Requisição | Custo |
| --- | --- | --- |
| `models` | `GET` da lista de modelos | grátis |
| `chat` | Chat sem stream, `max_tokens: 16` (`opencode`: 256, ADR 0060), mensagem `Responda só: ok` | ~US$ 0,0001 |
| `chat_stream` | O mesmo com `stream: true` (na OpenAI, com `stream_options.include_usage`) | ~US$ 0,0001 |
| `error_auth` | Chat com a chave `sk-invalid-wwz-drift` | grátis |
| `error_model` | Chat com o modelo `wwz-drift-modelo-inexistente` | grátis |
| `responses` | `POST` da Responses API (`/v1/responses`; `opencode` e `azure`: `/openai/v1/responses`), `input` = `Responda só: ok`, `max_output_tokens` | ~US$ 0,0001 |
| `responses_stream` | O mesmo com `stream: true` | ~US$ 0,0001 |

O OpenCode Go recebe o `User-Agent` e o id de sessão que exige (spec 006).

### Assinatura de forma e divergências

- **JSON**: lista ordenada de `(caminho, tipo)` na ordem de aparição. Os caminhos seguem o estilo `choices[].message.content`; os itens de um array são fundidos num só (`[]`), com a união ordenada dos campos. Tipos: `string`, `number`, `bool`, `object`, `array`, `null`; `null` é compatível com qualquer tipo.
- **`/models`**: compara o topo (todas as chaves, inclusive `total_count`, `links`, `has_more`…, na ordem) e só o item do modelo da sonda, achado por `id` (Gemini: `name` = `models/<id>`), com caminhos `data[]`. Os demais itens não entram, porque a lista real tem dezenas de modelos heterogêneos e fundi-los gera falsos positivos. Item ausente num dos lados → `missing_field`/`extra_field` em `data[id=<modelo>]`.
- **SSE**: sequência de `(event, assinatura do data)`, colapsando repetições consecutivas iguais. O terminador (`[DONE]`/`message_stop`) entra explicitamente. Comentários (`:`) antes do primeiro evento são descartados dos dois lados, porque o real só os manda enquanto espera, como o `: OPENROUTER PROCESSING`; depois do primeiro evento entram como `comment`. As duas sequências são alinhadas pela maior subsequência comum da chave de cada evento (o `event:` ou, sem ele, o `type`/`object` do data). Cada par alinhado compara o data com as regras de JSON, com caminhos prefixados por `events[<i>:<chave>].`. Cada evento sem par vira um `event_sequence` próprio em `events[<i>]`. Assim todas as diferenças aparecem numa passada.
- **Headers**: só a presença dos nomes (minúsculos) com prefixo `x-ratelimit-`, `anthropic-ratelimit-`, `retry-after`, `request-id`, `x-request-id`, `openai-` ou `anthropic-`, mais os nomes exatos `x-should-retry`, `x-openai-authorization-error`, `x-openai-ide-error-code`, `x-error-json`, `x-generation-id`, `x-opencode-log-id` e `x-zen-model`, e o `content-type` (comparado sem parâmetros). Headers de infraestrutura nunca contam: `cf-*`, `server`, `date`, `set-cookie`, `via`, `x-openai-proxy-wasm` e `x-openai-internal-caller` ([ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)). A lista embutida de ignorados não inclui `openai-organization`, `openai-project`, `anthropic-organization-id` nem `anthropic-workspace-id`: o wwz manda esses headers com valores gerados, e só a presença é comparada.
- **Status**: o status HTTP de cada sonda.

| `kind` | Quando |
| --- | --- |
| `status_mismatch` | Status diferente |
| `missing_field` | Caminho presente no real e ausente no wwz |
| `extra_field` | Caminho presente no wwz e ausente no real |
| `type_mismatch` | Mesmo caminho com tipos incompatíveis |
| `order_mismatch` | Mesmos caminhos irmãos em ordem diferente |
| `event_sequence` | Evento SSE sem par no outro lado, depois do alinhamento; um por evento, todos listados |
| `missing_header` / `extra_header` | Header relevante só num dos lados |

Divergências que constam da lista de ignorados (embutida ou `--ignore`) aparecem no relatório como `ignored: true` e não contam.

### Orçamento e segredos

- Antes de cada provedor, o `check` lê `--budget-file` (`{"<provedor>": {"usd", "calls", "updated"}}`; arquivo ausente = zero). Com `usd ≥ --budget-usd`, o provedor sai `skipped` com o motivo `budget_exhausted`. Depois de cada sonda paga, soma o custo estimado (usage × tabela de preços em `internal/drift`; modelo sem preço = US$ 0,01 por chamada) e regrava o arquivo de forma atômica.
- A chave nunca aparece em stdout, stderr, relatório, `--save-raw` nem nas mensagens de erro; nelas é trocada por `***`.

### Relatório e códigos de saída

JSON (`--output json`):

```json
{
  "version": 1, "kind": "drift", "tool_version": "v0.1.0",
  "started": "2026-10-31T06:00:00Z", "finished": "2026-10-31T06:00:09Z",
  "wwz": {"url": "in-process", "version": "v0.1.0"},
  "providers": [
    {"provider": "openai", "status": "drift", "reason": "", "base": "https://api.openai.com", "model": "gpt-4o-mini", "spend_usd": 0.0002,
     "probes": [{"probe": "chat", "status": "drift", "real_status": 200, "wwz_status": 200,
                 "divergences": [{"kind": "missing_field", "path": "service_tier", "real": "string", "wwz": "", "ignored": false}]}]}
  ],
  "summary": {"ok": 2, "drift": 1, "skipped": 1, "error": 0}
}
```

O `status` do provedor e da sonda é `ok`, `drift`, `skipped` (motivos `no_key`, `real_not_confirmed`, `budget_exhausted`, `not_selected`, `not_supported` (sonda sem rota no provedor) e, no `opencode`, `upstream_form_unreproduced`) ou `error` (rede, timeout ou resposta ilegível, com `reason` trazendo a mensagem sem segredos). O texto lista um provedor por bloco e uma divergência por linha; não é contrato.

`providers[].base` é a base efetivamente sondada do real (a de `--base-url` ou a padrão da spec 006); vazio quando o provedor não foi selecionado. O texto do relatório mostra a base ao lado do nome do provedor, para que ninguém confunda um teste contra `httptest` com a sonda do real. O passo `drift` do `verify-heavy` e o workflow `drift.yml` passam `--real` (spec 018).

**OpenCode e o upstream da sessão ([ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md)).** O gateway encaminha cada `x-opencode-session` a um upstream e o `id` do Chat muda de forma com ele (`gw-<12 hex>-<6 hex>`, 32 hex ou `chatcmpl-<16 hex>`, spec 006, 16.3). Para a comparação ser estável, a sonda do `opencode` fixa uma sessão por execução (a mesma `ses_<64 hex>` em todas as sondas do provedor, informada no relatório em `providers[].session`) e o `id` compara como equivalente (regra de ignorar embutida `id` do Chat/Messages do `opencode`, sem consumir `--ignore`). Ids que não são `gw-` também foram vistos no real: 32 hex, `chatcmpl-<16 hex>` e ids longos de 53 caracteres de outros upstreams (ADR 0060). Se a resposta de `chat` do real vier em **qualquer** forma que não seja `gw-<12 hex>-<6 hex>`, o wwz não a reproduz: a sonda tenta até 3 sessões novas e, se nenhuma devolver `gw-`, o provedor sai `skipped` com o motivo `upstream_form_unreproduced` (e `exit 0`), em vez de acusar drift que o wwz não pode eliminar. As sondas do `opencode` usam `max_tokens: 256`: o modelo raciocina antes de responder e, com 16, o `finish_reason` do real variava entre `length` e `stop` (task 257). Qualquer divergência além do `id` com a forma `gw-` é drift normal.

Códigos de saída: `0` nenhuma divergência (só `ok`/`skipped`); `3` alguma divergência não ignorada; `1` nenhuma divergência, mas alguma sonda em `error` ou falha de execução (wwz inalcançável, `--keys-file` explícito ilegível, `--budget-file` não gravável); `2` uso inválido. O `3` vence o `1`.

### `record` (ADR 0014, ADR 0035; task 309)

Implementado em `internal/drift/record.go` e `cmd/wwz-drift/record.go`. Roda as mesmas sondas do `check` **só no real** e grava cada resposta como fixture normalizada.

- **Flags**: as do `check` que valem para o real (`--providers`, `--probes`, `--model`, `--base-url`, `--keys-file`, `--real`, `--budget-file`, `--budget-usd`, `--timeout`) mais `--out DIR` (padrão `testdata/real`). Não há `--wwz` nem relatório de divergência. Mesmo orçamento e mesmos segredos do `check`: a chave nunca aparece em fixture ou mensagem.
- **Layout**: `<out>/<provedor>/shape/<sonda>.json`; `opencode-google` grava em `<out>/opencode/google/<sonda>.json`. Formato da spec 018 "Fixtures reais" (campos `provider`, `name`, `recorded_at`, `note`, `request`, `response`).
- **Normalização**: credenciais viram `<key>` e `<session>`; ids e valores variáveis viram marcadores (spec 018). Cabeçalhos guardados: os relevantes do `check`, `content-type`, `cache-control` e `access-control-*` (e, no Gemini, os `x-goog-*` da lista do código). Lista de modelos maior que 64 KiB é reduzida ao item da sonda, como a comparação do `check`.
- **Códigos de saída**: `0` tudo gravado (sondas puladas não contam), `1` sonda em `error` ou falha de execução, `2` uso inválido. Não existe `3`.
- Os testes `TestRealFixtures*` leem o diretório gravado; fixtures `shape/` mostram o que o real devolve e servem de insumo da tarefa de regravação (313).

### Agendamento

- `.github/workflows/drift.yml`: cron diário às 06:00 UTC e `workflow_dispatch`, chaves em secrets e relatório JSON como artefato. Fica inativo até o remoto existir (ADR 0018).
- O passo `drift` do `verify-heavy` roda o `check` na máquina do humano (spec 018), nunca no servidor.

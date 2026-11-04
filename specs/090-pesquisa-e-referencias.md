# 090 — Pesquisa e referências (informativa)

Status: aprovada
Depende de: —
Origem: seções 12, 15, 21, Fontes do documento Claude Docs congelado em 2026-10-30

## Contexto
Material de pesquisa que fundamentou as decisões: inventário das ferramentas, análise das cinco pesquisas externas, roadmap original e fontes (seções 12, 15, 21 e Fontes). Informativa: não define comportamento; em conflito, as specs 000–018 prevalecem.

## Requisitos
- (nenhum)

## Interfaces e contratos
- (nenhum)

## Dados
- (nenhum)

## Critérios de aceite
- [ ] Nenhum (informativa)

## Fora de escopo
- Tudo que não estiver nas specs normativas

## Decisões relacionadas
- [specs/decisions/0002-fonte-da-verdade-specs.md](decisions/0002-fonte-da-verdade-specs.md)

## Comportamento

### 12. Inventário de recursos das ferramentas pesquisadas

Cada recurso abaixo foi lido da documentação pública da ferramenta em 30/10/2026 e entra no `wwz` na fase indicada; recursos marcados como fase 4 são extensões que não bloqueiam o uso principal.

**[llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim)** (Go, Apache-2.0, simulador de vLLM)

| Recurso | Fase no WWZ |
| --- | --- |
| Modos `echo` e `random` com histograma de tamanho de resposta | 1 |
| Respostas de dataset SQLite por hash do histórico (ShareGPT), download do HuggingFace, `dataset-in-memory` | 2 |
| Tool calls geradas do JSON Schema: faixas para inteiros/números, tamanho de arrays, probabilidade de parâmetros opcionais, `tool-call-extra-call-probability` (geométrica truncada), `skip-tool-validation` | 1 |
| `time-to-first-token`, `inter-token-latency` com desvio padrão (máx. 30 %) | 1 |
| Calculadora `per-token`: `prefill-overhead` + `prefill-time-per-token`, `kv-cache-transfer-latency` para P/D | 2 |
| `time-factor-under-load` (latência cresce com `max-num-seqs`) | 1 |
| `max-num-seqs`, `max-waiting-queue-length`, `max-model-len` | 1 |
| Injeção de falhas: `failure-injection-rate`, tipos `rate_limit`, `invalid_api_key`, `context_length`, `server_error`, `invalid_request`, `model_not_found` | 1 |
| `startup-duration` (503 em `/health/ready` simulando carga de modelo) | 1 |
| `served-model-name` (vários nomes para o mesmo modelo) | 1 (aliases) |
| Tokenizador simulado (regex) vs. HuggingFace real via `render-url` | 1 / 4 |
| `strict` (validação contra schema do vLLM 0.21) | 2 |
| Métricas Prometheus `vllm:*` e `fake-metrics` com funções `oscillate`, `ramp`, `squarewave` | 2 (espelho) / 3 (fake) |
| `enable-request-id-headers`, `log-http`, `POD_NAME`/`POD_IP` em headers | 1 |
| `POST /admin/config` para atualização parcial em runtime | 1 |
| LoRA (`lora-modules`, `max-loras`), KV cache simulado com eventos ZMQ, Data Parallel, sleep mode, modo `omni` com imagem sintética, engine `sglang`, gRPC vLLM | 4 |
| Configuração por YAML + flags + env com precedência definida; flags booleanas `--no-` | 1 |
| `seed` para reprodutibilidade | 1 |
| Helm chart, manifests Kubernetes, Kind | 3 |

**[FakeAI](https://pypi.org/project/fakeai/)** (Python, Apache-2.0)

| Recurso | Fase no WWZ |
| --- | --- |
| `/v1/chat/completions` com 62 parâmetros, `/v1/completions`, `/v1/models`, `/v1/models/{id}/capabilities` | 1 |
| Reasoning estilo O1: `reasoning_content`, `usage.reasoning_tokens` | 1 |
| Function calling paralelo, structured outputs com validação de JSON Schema | 1 |
| TTFT/ITL com `variance_percent`, 37 perfis de latência por modelo, ajuste por tamanho do prompt e por carga | 1 (perfis prontos) |
| Error injection: `rate`, tipos 500/502/503/504/429/400 | 1 |
| Rate limiting por chave: RPM, TPM, RPD, TPD, tiers | 1 |
| Autenticação por chave com hash SHA-256, abuse detection, input validation, CORS | 1 (chave) / 4 (abuse) |
| KV cache com prefix matching, `cached_tokens`, redução de TTFT em hit | 2 (`cache_hit_probability`) / 4 (radix). *Nota (2026-10-31): `cache_hit_probability` saiu pela [ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md); o cache segue as regras reais por registro de prefixos no store ([ADR 0046](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md), spec 007)* |
| Responses API, Realtime API (WebSocket) | 2 / 4 |
| Embeddings, imagens, TTS, STT, moderação, files, batches, fine-tuning, vector stores, organização/usage/costs | 3 (embeddings, batches) / 4 (resto) |
| Vision e vídeo (contagem de tokens por frame) | 2 (contagem fixa) |
| Predicted outputs (`accepted_prediction_tokens`) | 4 |
| Métricas JSON/Prometheus/CSV/WebSocket, por modelo, dashboard HTML, DCGM/Dynamo | 1 (Prometheus, dashboard) / 4 (DCGM) |
| Integração AIPerf e suíte de benchmark automatizada | 3 |
| HTTP/2 via hypercorn | 1 (h2c) |

**[llmposter](https://github.com/SkillDoAI/llmposter)** (Rust, fixtures determinísticas)

| Recurso | Fase no WWZ |
| --- | --- |
| 4 formatos: OpenAI Chat, Anthropic Messages, Gemini generateContent, OpenAI Responses | 1 (OpenAI, Anthropic) / 2 (Responses) / 4 (Gemini) |
| Streaming de tool calls nos dois formatos | 1 |
| Falhas: 429, 5xx, latência, corrupção de corpo, truncamento mid-stream, `ConnectionReset` TCP | 1 |
| Chaos com `latency_jitter_ms`, frames SSE duplicados, `probability`, `chaos_seed` reproduzível | 1 |
| Embeddings por fixture ou determinístico | 3 |
| Servidor OAuth 2.0 mock (PKCE, device flow) | 4 |
| Uso como biblioteca em testes (`ServerBuilder`, porta aleatória) | 2 (pacote Go `wwztest`) |

**[mock-llm-server do Phoenix](https://glama.ai/mcp/servers/@Arize-ai/phoenix/blob/0f3ef8b8e2c39f6c5608f236746790b2ce5927b1/scripts/mock-llm-server/README.md)** (TypeScript, Arize)

| Recurso | Fase no WWZ |
| --- | --- |
| Tool calls com argumentos falsos gerados do JSON Schema, `TOOL_CALL_PROBABILITY` | 1 |
| Rate limit com modos (`after_n`, etc.) | 1 |
| `STREAM_DELAY_MS`, `STREAM_CHUNK_SIZE` | 1 (`chunk_tokens`) |
| OpenAI Chat, Responses, Anthropic Messages, Gemini | 1 / 2 / 4 |
| Dashboard em tempo real para monitorar e controlar o servidor | 1 |

**[acus9345/mock-llm-server](https://github.com/acus9345/mock-llm-server)** (TypeScript)

| Recurso | Fase no WWZ |
| --- | --- |
| Respostas roteirizadas por chamada: texto, `toolCalls`, `json`, `error {status}`, `stopReason: length`, `reasoning`, `chunkDelayMs`/`delayMs`, `streamError {afterChunks}` | 1 (motor de cenários cobre todos) |
| Função de resposta com acesso à requisição (`req.messages.length`, `req.model`) | 1 (templates) |
| Embeddings, contagem de tokens Anthropic, Gemini | 2 (`count_tokens`) / 3 / 4 |

**[mockllm](https://pypi.org/project/mockllm/)** (Python, Stacklok), **[mocktopus](https://pypi.org/project/mocktopus/)**, **[fakellm](https://pypi.org/project/fakellm/)**, **[MockAI](https://pypi.org/project/ai-mock)** (Python)

| Recurso | Fase no WWZ |
| --- | --- |
| Respostas pré-definidas por prompt em YAML/JSON, `unknown_response` padrão | 1 (passo `content` literal + `fallback`) |
| Casamento por `messages_contains`, regex, glob de modelo; `times: N` (responde só N vezes) | 1 (`expect.user_message_matches`, `repeat`) |
| Hot reload do arquivo de respostas | 1 (`/admin/scenarios`, SIGHUP) |
| Lag simulado por caractere com variação aleatória | 1 |
| `delay_ms`, `chunk_size`, `usage` fixo por regra | 1 |
| Resposta de eco determinística por hash da requisição quando nada casa | 1 (`echo`) |
| Arquitetura de plugins por provedor (mockllm) | 2 (interface `Protocol` em Go) |
| Modo record/replay de chamadas reais (mocktopus, previsto) | 4 |
| Clientes mock drop-in para Python (MockAI) | — (fora do escopo; o SDK oficial aponta para o servidor) |

**[GuideLLM](https://github.com/vllm-project/guidellm)** e **AIPerf** (geradores de carga, não mocks): ver seção 14.

### 15. Roadmap de implementação

A fase 1 entrega um binário utilizável para o teste de carga do roteador; cada fase seguinte só começa quando o portão da anterior passa.

_O diagrama do roadmap original (4 fases, 3 portões) foi substituído pela decisão "MVP completo"; a ordem de implementação vigente está em [PLAN.md](PLAN.md)._

Os portões são os benchmarks de aceite da seção 11 mais a suíte de compatibilidade com os SDKs oficiais (`openai` Python e Node, `anthropic` Python e Node), rodada em CI contra o simulador a cada commit. Prazos ficam para o planejamento da fase 1.

**Decisão (30/10/2026): sem fases — MVP completo.** As quatro fases acima passam a ser a ordem interna de implementação, não entregas separadas: o MVP inclui todos os drivers (openai, anthropic, opencode, openrouter), o motor de cenários, Valkey ou memória, painel, `wwz-load` e os recursos I1–I24. Ver seção 23 para o processo.

**Estrutura do repositório (fase 1)**

```text
wwz/
  cmd/wwz/          # binário do servidor
  cmd/wwz-load/     # gerador de carga (fase 3)
  internal/config/     # YAML, env, flags, atomic.Pointer, /admin/config
  internal/protocol/
    openai/            # tipos, parsing, serializador SSE, erros
    anthropic/         # idem, eventos tipados
  internal/generate/   # texto, raciocínio, histograma, dataset
  internal/toolargs/   # JSON Schema → argumentos (cache por hash)
  internal/scenario/   # parser YAML, estado da conversa, templates
  internal/latency/    # distribuições, fator de carga, agendador de timers
  internal/chaos/      # injeção de erros pré e mid-stream
  internal/ratelimit/  # rps/rpm/tpm/concurrency por chave
  internal/metrics/    # Prometheus, stats, dashboard
  scenarios/           # templates prontos (file-agent-flow, ...)
  deploy/              # Dockerfile, docker-compose, Helm, Grafana
  tests/sdk/           # compatibilidade com SDKs oficiais
```

**Abertos** (decidir antes de codar a fase 1)

- [ ] `net/http` ou `fasthttp`: medir ambos no primeiro protótipo com 20 000 streams.
- [ ] Formato do campo de raciocínio OpenAI por padrão: `reasoning_content` (DeepSeek/vLLM) ou `reasoning` (alguns gateways).
- [ ] Se `tiktoken` em Go puro entra na fase 1 ou fica para a 2.
- [ ] Nome definitivo do projeto e licença (sugestão: Apache-2.0, mesma do llm-d-inference-sim, para permitir copiar o gerador de argumentos).

### 21. Análise das cinco pesquisas externas

As cinco pesquisas citam 31 sistemas distintos; 27 existem, 3 são inventados (EpicAI, "LLM Emulator / llm-mock (timcubb)", openai-stream-mocker) e 1 é SaaS pago (DotMock). Nenhum publica req/s com metodologia, então toda classificação de "carga alta" nelas é opinião. Cada subseção abaixo foi produzida por um agente que abriu as fontes e separou o confirmado do afirmado; verificação feita em 30/10/2026.

Três sistemas que não estavam no nosso inventário (seção 12) mudam o quadro:

| Sistema | Linguagem | Por que importa | Decisão |
| --- | --- | --- | --- |
| [chaliy/llmsim](https://github.com/chaliy/llmsim) v0.6.0 | Rust | Cenários sequenciais stateless (ids de tool call decodificáveis), tool placeholder preenchido pelo JSON Schema, raciocínio em stream, timing por passo, `fail_first`, `stall`, `cut_after_tokens` — cobre a seção 9 quase inteira | Referência de projeto para o motor de cenários; **não** substitui o nosso (Rust, sem Valkey, sem drivers de limites, sem painel); nome colide com o nosso nome de trabalho |
| [AIMock](https://github.com/CopilotKit/aimock) v1.47 | TypeScript | Catálogo *model misbehavior* (tool calls quebradas derivadas do schema), caos em quatro camadas escopado por `X-Test-Id`, drift detection contra APIs reais | Importar o catálogo e o escopo por teste (seções 7 e 10) |
| [MockServer `httpLlmResponse`](https://mock-server.com/mock_server/llm_response_mocking.html) | Java | `streamingPhysics`, `chaos` tipado por provedor, `conversationPredicates`, raciocínio e content filter no dialeto certo de cada provedor | Importar o modelo de "física de streaming" e o content filter por dialeto (seções 6 e 7) |

Também entram como referência secundária: **JulienRabault/LLMock** (relatório de resiliência do cliente), **ManagedCode LLM Test Server** (decomposição de tokens no painel, *conversation graphs*), **Axium llm-mock** (controle por header), **1dg618/fakellm** (conversa por header), **DotMock** (pacing explícito e precedência header > fixture > global), **NVIDIA Dynamo Mocker** (latência polinomial por carga, eventos KV) e **SGLang Simulator** (relógio lógico).

| Pesquisa | Sistemas citados | Inventados | Erro principal | Melhor achado |
| --- | --- | --- | --- | --- |
| 21.1 ds | 7 | EpicAI | Recomenda FakeAI por números de versão antiga sem método | Controles de pacing e precedência do DotMock |
| 21.2 gr | 12 | timcubb, openai-stream-mocker | Subestima o chaliy/llmsim, que já faz tool calls por schema e reasoning | chaliy/llmsim 0.6.0 inteiro |
| 21.3 mn | 7 | — (um endpoint inventado) | Cita páginas que não contêm o afirmado | Model misbehavior e caos em camadas do AIMock |
| 21.4 mt | 7 | — (uma API inventada) | Descreve o llm-d-inference-sim ao contrário | Dashboard de tokens e conversation graphs do ManagedCode |
| 21.5 gm | 7 | — | Atribuições trocadas; MockServer exagerado e ao mesmo tempo subaproveitado | `streamingPhysics` + `chaos` + predicados do MockServer; relatório de resiliência do LLMock |

O detalhe de cada ferramenta, os recursos a importar com origem e as ideias de teste estão nas subseções; a seção 22 consolida o que entra na especificação.

### 21.1 Pesquisa ds — FakeAI, EpicAI, llmsim, llmposter, mockllm, DotMock, mock-openai-api

A recomendação principal da pesquisa ("comece com FakeAI") apoia-se em números que só existem na página PyPI de uma versão antiga (0.0.5: 930 req/s, 10 000 concorrentes) sem hardware nem método; o **EpicAI** não existe em registro algum; e o **DotMock** existe mas é SaaS pago, não um servidor local. O que vale é o detalhamento do chaliy/llmsim (confirma 21.2) e o catálogo de controles de pacing e caos por header do DotMock.

| Sistema | Existe | Linguagem | Confirmado na fonte | Afirmado e não confirmado |
| --- | --- | --- | --- | --- |
| [FakeAI](https://github.com/ajcasagrande/fakeai) (v0.3.2, 2 estrelas, 16 commits) | Sim | Python | Ver seção 12; a verificação acrescentou: KV cache radix com blocos de 16 tokens; rate limit por chave com tiers; latência dependente do tamanho do prompt e da carga | Como escolhe tool calls; streaming de `reasoning_content` (só exemplo não-streaming); "API de configuração" para pool de tools; seed; "performance comprovada" |
| EpicAI | **Não encontrado** (GitHub, PyPI, npm, buscas EN/ZH) | — | — | Tudo ("100 M tokens/s", painel "Agent") |
| [chaliy/llmsim](https://github.com/chaliy/llmsim) | Sim | Rust | Ver 21.2; acrescenta: headers de resposta `x-llmsim-scenario/step/attempt`; 400 se o tool call nomear ferramenta não oferecida; cap de conexões WebSocket; stats finalizadas em drop de stream; stats por modelo com teto (anti-DoS de memória); tool calls em stream como "announce + args" | `fixed_tool_calls` não existe (é turno `tool_calls`); tool calling é documentado |
| [llmposter](https://github.com/SkillDoAI/llmposter) (v0.5.0) | Sim | Rust | Ver 21.2; acrescenta: `corrupt_body` (texto puro em vez de JSON), `truncate_after_frames`, `disconnect_after_ms`, `duplicate_frames`; matching por `tool_schema` e JSONPath RFC 9535; prioridade + `catch_all`; hot reload mantendo fixtures anteriores em caso de erro; redação de credenciais no record; templating Jinja; `/code/<status>` | "Streaming de tool calls" é parcial (args em um único frame); sem raciocínio |
| [kagent-dev/mockllm](https://github.com/kagent-dev/mockllm) | Sim (homônimo do `mockllm` Python da Stacklok) | Go | Ver 21.3 | — |
| [DotMock](https://dotmock.com/docs/features/llm-mocking) | Sim, **SaaS proprietário** (US$ 8–99/mês) | Não divulgada | 7 provedores; raciocínio no campo nativo de cada um (`reasoning_content`, bloco `thinking` com `signature`, `thought` parts); tool calls com args schema-valid, workflows em duas fases com `toolCallId`/`hasToolResult`/`turnIndex`/`sequenceIndex` e templating `{{tool.<name>.result}}`; streaming `ttftMs`, `tps`, `jitter`, `chunkSize` (runes), `interChunkDelaysMs`, `truncateAfterChunks`, `replaySpeed`, `totalDurationMs`; chaos drop/429/malformed/disconnect com taxa; headers `X-Dotmock-Seed`, `X-Dotmock-Chaos-*`, `-Disconnect-After`, `-Retry-After` com precedência header > fixture > global; journal | Omitiu que não é open source nem auto-hospedável |
| [zerob13/mock-openai-api](https://github.com/zerob13/mock-openai-api) | Sim | TypeScript | Ver 21.4; acrescenta: editor de **timelines** no admin (text, tool-call, usage, finish, error, ping) e transcodificação entre protocolos | `<thinking>` é texto, não campo estruturado |

**Recursos a importar** (não repetidos de 21.2–21.5)

- **Pacing explícito**: `interChunkDelaysMs` (lista de atrasos por chunk), `totalDurationMs` (o stream dura exatamente T), `chunkSize` em runes — DotMock (entra na seção 6 como alternativa à distribuição).
- **Precedência de controle** header > cenário/fixture > modelo > global para caos e seed, com `X-…-Seed` por chamada — DotMock (formaliza a ordem que a seção 6 já insinua).
- **Timeline editável** de uma resposta (sequência explícita de eventos: text, tool-call, usage, finish, error, ping) — zerob13 (vira o formato interno `core.StreamWriter` exposto em YAML para cenários avançados).
- **Raciocínio no campo nativo de cada dialeto** incluindo `signature` do bloco `thinking` da Anthropic e `thought` parts do Gemini — DotMock (confirma seção 5 e cobre o driver `gemini`, no MVP pela [ADR 0043](decisions/0043-drivers-gemini-e-azure.md)).
- `corrupt_body` (resposta não-JSON com status 200) e `duplicate_frames` — llmposter (entram na seção 7).
- Hot reload que **mantém a configuração anterior** quando o novo YAML tem erro — llmposter (regra para `/admin/scenarios` e SIGHUP).
- Redação de credenciais ao gravar tráfego real (record) — llmposter (fase 4).
- Teto de estatísticas por modelo e finalização de stats em drop de stream, para não vazar memória sob caos — chaliy/llmsim (vai para a seção 11).
- 400 quando o cenário nomeia uma tool que o request não oferece — chaliy/llmsim (vai para `wwz validate` e para o motor, seção 9).
- Latência dependente do tamanho do prompt e redução de TTFT em cache hit — FakeAI (já coberto por `prefill_per_token` e `cache_hit_probability`; nota de 2026-10-31: `cache_hit_probability` saiu pela ADR 0042, e o acerto vem do registro de prefixos da ADR 0046, com `cache_ttft_reduction` na spec 008).

**Ideias e soluções da pesquisa**

- Separar "gerador de volume" de "validador de sintaxe" e usar os dois.
- Pool de fixtures alternadas por requisição quando o mock não sorteia tools (desnecessário no `wwz`, que sorteia nativamente).
- Testar o roteador contra vários formatos de raciocínio (multi-provedor).
- Das fontes, não da pesquisa: desativar retries do SDK ao testar falhas; cenários de stall e corte para validar keep-alive e retentativa.

**Erros da pesquisa.** EpicAI inventado; FakeAI com números de versão antiga e sem método; llmsim com recurso inexistente (`fixed_tool_calls`) e tool calling "não destacado" (está documentado); DotMock apresentado como opção local; llmposter com "streaming de tool calls" exagerado; nenhuma ferramenta publica benchmark reproduzível.

### 21.2 Pesquisa gr — chaliy/llmsim, llmposter, Axium llm-mock, AIMock, fakellm, mock-ai, Roy, vLLM fake server

Dez dos doze sistemas existem; dois são inventados ("LLM Emulator / llm-mock (timcubb)" e "openai-stream-mocker"). O achado mais importante de toda a rodada é o **[chaliy/llmsim](https://github.com/chaliy/llmsim)** (Rust, v0.6.0 de 10/10/2026): é o projeto mais próximo da nossa especificação — cenários sequenciais stateless com tool placeholder preenchido pelo JSON Schema, raciocínio em stream, timing por passo e `fail_first` — e a pesquisa o subestimou. O nome coincide com o nosso nome de trabalho; ver decisão na seção 22.

| Sistema | Existe | Linguagem | Confirmado na fonte | Afirmado e não confirmado |
| --- | --- | --- | --- | --- |
| [chaliy/llmsim](https://github.com/chaliy/llmsim) (v0.6.0, 6 estrelas) | Sim | Rust | OpenAI Chat/Responses e Anthropic; SSE; TTFT e TBT com distribuição normal (`ttft_mean_ms/ttft_stddev_ms/tbt_mean_ms/tbt_stddev_ms`, perfis por modelo); geradores `lorem/echo/fixed/random/sequence`; erros 429/500/503/timeout por taxa (`rate_limit_rate`, `server_error_rate`, `timeout_rate`, `timeout_after_ms`); tiktoken-rs; TUI e `GET /llmsim/stats`; `GET /metrics` estilo vLLM; benchmarks k6 (smoke/load/stress/spike/soak) e oha; **scripts e scenarios** (`steps[]` com \`turn.type: assistant | tool\_calls |
| [llmposter](https://github.com/SkillDoAI/llmposter) (AGPL-3.0) | Sim | Rust | Matching por mensagem, modelo, headers, system, temperatura, metadata, schema de tools, JSONPath; **state machines nomeadas** (multi-turn); chaos `latency_jitter_ms`, `probability`, `chaos_seed`, frames SSE duplicados; 429/5xx/truncamento/ConnectionReset; record/replay e `record-on-miss`; `--watch`; `GET /code/{status}` | "VCR" não é termo do projeto; reasoning não documentado |
| [axium-lab/llm-mock](https://github.com/axium-lab/llm-mock) | Sim | TypeScript/Bun | Headers `x-llm-mock-response`, `x-llm-mock-response-base64`, `x-llm-mock-tool-calls` (ignora `tool_choice`), `x-llm-mock-content-filter` (Azure), `x-llm-mock-batch-status`; OpenAI/Anthropic/Gemini/Azure; thinking Anthropic; stateless com ids por hash; chaves fixas `sk-mock-key-01..10` rejeitadas no formato de erro de cada provedor; instância hospedada | — |
| [AIMock](https://github.com/CopilotKit/aimock) | Sim | TypeScript | `--chaos-latency`, `--chaos-ratelimit`, `--replay-speed`, `--strict` (resto em 21.3 e 21.4) | "Reasoning em vários providers" (só `reasoning-only` como misbehavior) |
| [1dg618/fakellm](https://github.com/1dg618/fakellm) | Sim | Python | Regras YAML `when:/respond:` first-match; **multi-turn por hash da 1.ª mensagem ou header `X-Fakellm-Conversation-Id`**; condições `turn`, `turn_in`, `tool_result_contains`; `POST /_fakellm/reset`; args de tool em chunks; `--workers N` (com aviso de estado dividido); tiktoken opcional | — |
| [ajac-zero/mock-ai](https://github.com/ajac-zero/mock-ai) | Sim | Python | Header `mock-response`; 1 char por chunk; tools paralelas via JSON | — |
| [masci/roy](https://github.com/masci/roy) (`roy-cli` 0.3.0) | Sim | Rust | `--rpm/--tpm` geram `x-ratelimit-limit/remaining/reset-*`; `--error-code`/`--error-rate`; `--timeout`; `--slowdown 0:1000`; `--response-length 10:100`; tokenizer embutido | — |
| [kagent-dev/mockllm](https://github.com/kagent-dev/mockllm) | Sim | Go | Ver 21.3 | "Stateful" (README nega) |
| [freakynit/mock-openai-server](https://github.com/freakynit/mock-openai-server) | Sim | Node | `config.yaml`; tools via regex no prompt; delay em ms | Streaming não mencionado (pesquisa inverteu) |
| [wkoszek/mockai](https://github.com/wkoszek/mockai) | Sim | Go | Fixtures JSON; modo `size` com contagem de tokens; uso como lib Go | — |
| [vLLM production-stack fake-openai-server](https://github.com/vllm-project/production-stack/blob/main/src/tests/perftest/fake-openai-server.py) | Sim (170 linhas) | Python | Só chat streaming + `/metrics` vLLM; `--speed`, `--ttft` | Bugs: `--max-tokens` ignorado, último chunk repete o anterior |
| "LLM Emulator (timcubb)", "openai-stream-mocker" | **Não encontrados** | — | — | Tudo |

**Recursos a importar** (não repetidos de 21.3–21.5)

- **Tool placeholder** `any_read|any_write|any` escolhendo entre as tools do request e preenchendo `required` pelo schema — chaliy/llmsim (entra na seção 9 como `args: schema` com filtro por classe de tool).
- **Ids de tool call decodificáveis** (`call_<cenário>_<passo>_<n>_<nonce>`): o próximo turno identifica o passo pelo id sem estado no servidor — chaliy/llmsim (melhora a seção 9: estado derivado também do id, não só da contagem de `tool_results`).
- **Seleção de cenário por marcador na mensagem** `[[llmsim:nome seed=N speed=N]]` e por model id `…-scenario-<nome>` — chaliy/llmsim (soma-se ao header e `metadata` da seção 9).
- **Timing por passo**: `ttft_ms`, `tokens_per_sec`, `stall {after_tokens, ms}`, `cut_after_tokens`, `speed` que escala tudo — chaliy/llmsim.
- **`fail_first`**: erro só nas N primeiras tentativas de um passo, com rastreador de tentativas com TTL — chaliy/llmsim (testa retry do roteador por passo; vai para o Valkey como `attempts:{conv}:{step}`).
- `turn.type: mixed` (texto + tool na mesma resposta) e `on_exhausted` — chaliy/llmsim.
- **Controle por header** `x-…-response`, `x-…-response-base64`, `x-…-tool-calls` — Axium (entra como `x-wwz-response`, `x-wwz-tool-calls`, `x-wwz-content-filter`).
- **Conversa por header** `X-Fakellm-Conversation-Id` ou hash da 1.ª mensagem, com `reset` administrativo — fakellm (a seção 9 já deriva `vars` do hash; o header entra como alternativa).
- `--rpm/--tpm` gerando headers `x-ratelimit-*` consistentes — Roy (já coberto na seção 8).
- `--slowdown 0:1000` e `--response-length 10:100` como faixas por flag — Roy.
- Chaves fixas de teste (`sk-mock-key-01..10`) com rejeição no dialeto do provedor — Axium.
- Perfis de benchmark k6 empacotados (smoke/load/stress/spike/soak/high-concurrency) com thresholds e mix 70/30 streaming — chaliy/llmsim (entra na seção 14).
- TUI com `GET /…/stats` JSON — chaliy/llmsim.
- Uso como biblioteca em testes (`mockai`, llmposter `ServerBuilder`) — já previsto como `wwztest`.

**Ideias e soluções da pesquisa**

- Usar o llmsim (Rust) como base de carga e migrar para fixtures YAML quando precisar de state machines.
- Controle por header para configurar sem fixtures.
- "Tool calls aleatórios dentro do schema: contornar no cliente ou contribuir com um gerador" — desatualizado, o llmsim 0.6.0 já faz.

**Erros da pesquisa.** Dois sistemas inventados; kagent mockllm não é stateful e é Go; wkoszek/mockai é Go; freakynit tem tools mas não streaming; AIMock não tem reasoning dedicado; llmsim subestimado em reasoning e tool calls; fake-openai-server do vLLM está em `src/tests/perftest/` com bugs; nenhuma ferramenta publica req/s.

### 21.3 Pesquisa mn — AIMock, piyook LLMock, dwmkerr mock-llm, kagent mockllm, WireMock, Mockoon, Speedscale

É a pesquisa mais bem fundamentada das cinco: os sete sistemas existem e os recursos centrais se confirmam; os erros são de citação (páginas desatualizadas ou que não contêm o afirmado) e um endpoint inventado (`/api/run/stream` do Speedscale). O achado principal é o módulo **model misbehavior** do AIMock — a lista mais completa de tool calls deliberadamente quebradas que encontramos — e o **chaos em quatro camadas** escopado por `X-Test-Id`.

| Sistema | Existe | Linguagem | Confirmado na fonte | Afirmado e não confirmado |
| --- | --- | --- | --- | --- |
| [AIMock](https://github.com/CopilotKit/aimock) (v1.47.0, \~1 000 estrelas, MIT) | Sim | TypeScript (+ pacote pytest) | Fixtures `{match, response}` com `match.{userMessage, turnIndex, hasToolResult, toolCallId, toolResultContains, sequenceIndex, systemMessage, toolNamespace}` e `response.{content, usage, finishReason, blocks, raw, misbehavior}`; `streamingProfile {ttft, tps, jitter 0–1}`, `recordedTimings`, `nonStreamingTiming`; chaos `dropRate/malformedRate/rateLimitRate/disconnectRate/latencyMs` em quatro camadas — servidor (`llm.chaos`), fixture, header `x-aimock-chaos-*` por requisição, runtime `POST /__aimock/chaos` escopado por `X-Test-Id`; `truncateAfterChunks`, `disconnectAfterMs`, `nextRequestError()`, `POST /__aimock/error`; **misbehavior** \`tool-args-invalid-json {style: truncated | trailing-comma |
| [piyook/llm-mock](https://github.com/piyook/llm-mock) (LLMock) | Sim | TypeScript (Fastify) | `responseType: lorem` (`maxLoremParas`) ou `stored` (`storedResponsesFile`, sorteio por requisição); `responseRules` com `files[]` sorteado, `match`, `stopReason`, `fail`; `responseDelay.min/max` aleatório + `/admin/delay`; \`chaos {enabled, frequency, mode, status, kind: http | stream-error |
| [dwmkerr/mock-llm](https://github.com/dwmkerr/mock-llm) | Sim | TypeScript | `/config` GET/POST/PATCH/DELETE; regras `path` (regex), `match` (JMESPath), `sequence` (contador por path que só incrementa se a regra vencedora tem `sequence`; reset via `DELETE /config`), última regra casada vence; `{{jmes request body.messages[-1].content}}`, `{{timestamp}}`; `streaming.chunkSize` 50 e `chunkIntervalMs` 50; erros ≥ 400 vão como um evento SSE com HTTP 200; Helm `oci://ghcr.io/dwmkerr/charts/mock-llm` | `src/streaming.ts` confirma: fatia só `message.content`, nunca `tool_calls` |
| [kagent-dev/mockllm](https://github.com/kagent-dev/mockllm) | Sim | Go | Reconstrói o stream Responses a partir da fixture final com `sequence_number` (`response.created → output_item.added → output_text.delta / function_call_arguments.delta → response.completed → [DONE]`); README diz "No latency simulation", "No error injection" | — |
| [WireMock](https://wiremock.org/docs/simulating-faults/) | Sim | Java | `fixedDelayMilliseconds`, `delayDistribution` lognormal (`median, sigma, maxValue`) e uniform, `chunkedDribbleDelay {numberOfChunks, totalDuration}`; faults `EMPTY_RESPONSE`, `MALFORMED_RESPONSE_CHUNK`, `RANDOM_DATA_THEN_CLOSE`, `CONNECTION_RESET_BY_PEER`; helpers `pickRandom`, `randomValue uuid`, `randomInt`, Faker `random 'Name.first_name'` | Sem SSE nativo |
| [Mockoon](https://mockoon.com/docs/latest/templating/fakerjs-helpers/) | Sim | TypeScript/Electron | `{{faker 'person.firstName'}}` com **seed e locale globais**; modos `random` e `sequential` por rota; WebSocket | Sem SSE de rota; "endpoint admin SSE" não encontrado |
| [Speedscale llm-simulation-demo](https://docs.speedscale.com/guides/mocking/llm-simulation/) | Sim | Python/FastAPI + Next.js | `tools-service` com latência artificial 80–220 ms e 15–60 ms; gravação com chaves reais e replay por assinatura (`proxymock record`, `proxymock mock`) | `/api/run/stream` não existe |

**Recursos a importar** (não repetidos de 21.4 e 21.5)

- **Model misbehavior** como catálogo de tool calls quebradas derivadas do schema do request: JSON inválido com estilo (`truncated`, `trailing-comma`, `single-quotes`), violação de schema (`missing-required`, `wrong-type`, `extra-property`, `enum-mismatch`, `not-object`), nome desconhecido, id duplicado, `finish_reason: length` no meio dos argumentos, resposta vazia, só raciocínio — AIMock. Vai para a seção 7 como classe `tool_misbehavior`.
- **Caos em camadas com escopo de teste**: servidor → modelo → fixture/cenário → header por requisição (`x-wwz-chaos-*`) → runtime via admin, tudo escopado por `X-Test-Id` para rodar testes paralelos no mesmo cluster sem interferência — AIMock.
- `sequence` por rota que só avança quando a regra vencedora pede, com reset administrativo — dwmkerr; `sequenceIndex` e `sequential|random` por rota — AIMock, Mockoon.
- Erro **one-shot** (`nextRequestError`) para o próximo request de um `X-Test-Id` — AIMock.
- Distribuição lognormal com `maxValue` e `chunkedDribbleDelay` (N chunks em T ms) — WireMock (a seção 6 já tem lognormal; `maxValue` e dribble entram).
- Faults de transporte com nome: `EMPTY_RESPONSE`, `MALFORMED_RESPONSE_CHUNK`, `RANDOM_DATA_THEN_CLOSE`, `CONNECTION_RESET_BY_PEER` — WireMock.
- Helpers de template `pickRandom`, `randomValue uuid`, `randomInt`, Faker com seed global — WireMock, Mockoon (entram no motor de templates da seção 9).
- `recordedTimings` + `--replay-speed`: reproduzir a cadência real gravada de um provedor — AIMock.
- Presets de latência por provedor (`chatgpt`, `claude`, `gemini`) com faixa min/max — piyook (os perfis da seção 6 ganham nomes de provedor).
- Erros ≥ 400 entregues como evento SSE com HTTP 200 — dwmkerr (já coberto como `stream_error_event`).
- Servidor de ferramentas fake com latência própria fechando o ciclo do agente — Speedscale.

**Ideias e soluções da pesquisa**

- Adaptador sintético fino entre roteador e mock escolhendo fixture por cenário + seed e validando schema antes de responder (no `wwz`, isso é o próprio núcleo).
- Servidor de ferramentas fake com sucesso/timeout/erro/payload inválido controláveis.
- Observabilidade separada roteador vs. mock; isolamento de contadores por processo/porta; bloquear fallback a provedor real; request sem fixture falha de forma segura.
- Seed derivada do id do cenário e registrada por requisição para reproduzir falhas.
- Separar teste de throughput (sem delay, corpo pequeno) de teste de streaming realista.
- Tratar o "stream de pensamento" como texto sintético rotulado, validando só o contrato observável (eventos, ordem, JSON, `finish_reason`, `usage`, cancelamento).

**Erros da pesquisa.** Referência \[4\] do LLMock desatualizada; delays do WireMock estão em outra página; `/api/run/stream` inventado; SSE do WireMock e do Mockoon inexistente; omitiu que o AIMock tem chaos probabilístico com seed e misbehavior com `rate`, o que relativiza o "não é gerador aleatório".

### 21.4 Pesquisa mt — AIMock, mock-openai-api, LLM Test Server, MockServer, llmfaker e um servidor FastAPI

As sete ferramentas existem, mas a pesquisa inventou a API `llm.onRequest()` do AIMock, subestimou o llm-d-inference-sim (que faz tool calls aleatórias por schema por padrão, o contrário do afirmado) e toda a coluna "RPS alto" é opinião sem fonte. Os achados que valem são o **ManagedCode LLM Test Server** (dashboard que decompõe os tokens de entrada e *conversation graphs*) e o detalhamento do MockServer.

| Sistema | Existe | Linguagem | Confirmado na fonte | Afirmado e não confirmado |
| --- | --- | --- | --- | --- |
| [AIMock (ex-LLMock, CopilotKit)](https://github.com/CopilotKit/aimock) | Sim; `@copilotkit/llmock` virou `@copilotkit/aimock` na 1.7.0, `LLMock` segue como classe | Node/TS, zero deps | "11 providers, 22 API surfaces" (OpenAI, Claude, Gemini, Bedrock, Azure, Vertex, Ollama, Cohere, OpenRouter, Grok…); CLI `npx -p @copilotkit/aimock llmock -p 4010 -f ./fixtures`; fixtures `{match, response}`, `mock.onMessage()`, `mock.on()`, `onToolCall`; streaming physics `ttft/tps/jitter`; chaos (500, JSON malformado, disconnect mid-stream, 429 com `Retry-After`, `--chaos-latency`); record/replay com `--replay-speed`; drift detection em CI diária contra APIs reais; WebSocket (Realtime, Responses WS, Gemini Live); journal `GET /__aimock/journal`; Prometheus; Docker/Helm; mocks de MCP/A2A/AG-UI; `--policy`, `--strict`, `--validate-on-load` | `llm.onRequest()`, `reasoning_content`, handler dinâmico que sorteia tools, "milhares de RPS" |
| [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim) | Sim | Go | Já inventariado na seção 12; a verificação acrescentou: `/v1/responses`, `/v1/messages` (Anthropic), `/tokenize`, gRPC `VllmEngine.Generate`, headers `X-Return-Error` e `X-Cache-Threshold-Finish-Reason`, `--latency-calculator per-token` | "Tool calling não é aleatório" (errado), "só chat e models" (desatualizado), "5–10 k RPS" (sem fonte) |
| [zerob13/mock-openai-api](https://github.com/zerob13/mock-openai-api) | Sim | TypeScript (Express) | Modelos `mock-gpt-thinking` (tags `<thinking>` no content), `gpt-4-mock` (function calling), `mock-gpt-markdown`, `gpt-4o-image`; OpenAI Chat, Responses, Anthropic Messages; admin web na porta 3001 com record/replay, playlists e cenários; Docker `zerob13/mock-openai-api:1.0.6` | "functionCall aleatório" é edição manual de `mockData.ts` |
| [ManagedCode LLM Test Server](https://github.com/managedcode/LlmTestServer) | Sim | C#/.NET (Aspire) | `ghcr.io/managedcode/llm-test-server:1.0.0`; 13–15 famílias de provedores; dashboard com aba **Tokens** separando system prompt / tool schemas / user / assistant / tool calls / tool results + cache reads/writes e reasoning à parte; request inspector; **conversation graphs** (passos condicionais, caminhos alternativos, loops, step limit); cenários com replies em fila, chunks scriptados, delays, erros, 429, content filter por provedor, failing tools; structured output; NuGet por provedor | Nome "InputComposition"; ".NET/Go" |
| [MockServer `httpLlmResponse`](https://mock-server.com/mock_server/llm_response_mocking.html) | Sim | Java | Além do listado em 21.5: `reasoningText` emitido como `reasoning_content` (DeepSeek/xAI) ou `reasoning` (Groq/OpenRouter); `reasoningTokens` como subconjunto de `outputTokens`; content filter correto por provedor (OpenAI 400 `content_filter`, Azure `innererror`, Anthropic 200 refusal, Gemini `SAFETY`, Anthropic 529); NDJSON Ollama, event-stream binário Bedrock | Netty e carga |
| [llmfaker](https://pypi.org/project/llmfaker/) | Sim (0.1.0, 10/05/2026, 0 estrelas) | Python | Patch in-process de openai/anthropic/litellm/langchain; `faker.when().respond()`; matching exato/regex/predicate/template; SSE; failure injection; TTFT e inter-token; cassettes record/replay; tokens e custo | Thinking; multi-turn tool sequences (só na lista, sem exemplo) |
| [ai-mock](https://pypi.org/project/ai-mock) | Sim (0.3.1, 03/2025) | Python | Servidor na porta 8100 **e** patch; uma letra por chunk; tool calls via JSON, paralelas; header `mock-response`; instância hospedada | — |

**Recursos a importar** (não repetidos de 21.5)

- **Decomposição dos tokens de entrada** no painel: system prompt, schemas de tools, user, assistant, tool calls, tool results, cache reads/writes, reasoning — ManagedCode.
- **Conversation graphs**: passos condicionais com caminhos alternativos, loops e `step limit` — ManagedCode (confirma o desenho da seção 9 e acrescenta o limite de passos).
- **Content filter no dialeto certo de cada provedor** — MockServer (400 `content_filter` OpenAI, 200 `refusal` Anthropic, `SAFETY` Gemini).
- `reasoning_field: reasoning_content|reasoning` por dialeto e `reasoningTokens ⊂ outputTokens` — MockServer (já previsto na seção 5, confirmado).
- Raciocínio **inline** como `<thinking>` no `content` (modelos que não têm campo separado) — zerob13.
- **Journal de requisições** (`/__aimock/journal`) com marca `interrupted` quando o cliente desconecta — AIMock.
- **Drift detection**: job diário que compara o mock com as APIs reais e acusa divergência de schema — AIMock.
- Record/replay com `--replay-speed` e `--validate-on-load` de fixtures — AIMock, zerob13, llmfaker.
- Headers por requisição `X-Return-Error`, `X-Cache-Threshold-Finish-Reason` — llm-d; `mock-response` — ai-mock.
- Métricas falsas em forma de onda (`oscillate/ramp/squarewave`) para testar autoscalers — llm-d (já em seção 12, fase 3).
- Admin web com playlists de cenários — zerob13.

**Ideias e soluções da pesquisa**

- Duas camadas: mock funcional (tools + thinking) e simulador de estresse atrás do roteador com k6/locust.
- Handler que lê `request.tools` e sorteia \~30 %; `json-schema-faker` para argumentos válidos (o llm-d já faz isso em Go).
- Servidor FastAPI de exemplo com 35 % de tool call, `reasoning_content` fixo e `uvicorn --workers 8`.

**Erros da pesquisa.** `llm.onRequest()` não existe; `reasoning_content` não aparece no AIMock; llm-d descrito ao contrário; "InputComposition" é nome inventado; llmfaker é projeto embrionário sem thinking; ai-mock é servidor, não só patch. O FastAPI de exemplo omite `id/object/created/model` nos chunks, `index` nos `tool_calls` (quebra SDKs que acumulam deltas), `usage`, `tool_choice` e `n`, e os argumentos (`fake_<campo>`) ignoram tipos.

### 21.5 Pesquisa gm — MockServer, kagent mockllm, LLMock, Httpbun, simuladores de motor e um daemon Go

As sete ferramentas existem; a pesquisa acerta em LLMock e Httpbun, exagera o MockServer ("Netty, altíssima carga" não consta na doc) e omite que o MockServer já faz raciocínio e caos, o que enfraquece a tese de que "nenhum software faz reasoning". O maior achado é o bloco `streamingPhysics` + `chaos` + `conversationPredicates` do MockServer, o mais parecido com o que a seção 9 e a seção 7 descrevem.

| Sistema | Existe | Linguagem | Confirmado na fonte | Afirmado e não confirmado |
| --- | --- | --- | --- | --- |
| [MockServer `httpLlmResponse`](https://mock-server.com/mock_server/llm_response_mocking.html) | Sim | Java | `provider` (OPENAI, OPENAI\_RESPONSES, ANTHROPIC, GEMINI, BEDROCK, AZURE\_OPENAI, OLLAMA, COHERE, DEEPSEEK, GROQ…); `completion.{text, stopReason, usage, toolCalls, toolChoice}`; `streamingPhysics {timeToFirstToken, tokensPerSecond 1–10000, jitter 0–1, seed, subwordStreaming}`; `reasoningText` → `reasoning_content`, `usage.reasoningTokens`; \`chaos {errorStatus, errorKind, retryAfter, errorProbability, truncateMode NONE | MID\_STREAM, truncateAtFraction, malformedSse, quotaName, quotaLimit, quotaWindowMillis, quotaErrorStatus, contentFilterBlockProbability, seed}` ;  `conversationPredicates {turnIndex, latestMessageContains, containsToolResultFor, semanticMatchAgainst}` ;  `llmFailover()\`; embeddings, rerank, moderação |
| [kagent-dev/mockllm](https://github.com/kagent-dev/mockllm) | Sim | Go | Tipos de `openai-go/v3` e `anthropic-sdk-go`; `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/messages`; SSE; tool calls; config JSON com `match_type` exact/contains, `headers`, `system_contains`, `tool_names`; 404 sem match | "Milhares de RPS" (sem benchmark); README diz: sem latência, sem erro, sem reasoning |
| [JulienRabault/LLMock](https://github.com/JulienRabault/LLMock) | Sim | Python | `rate_limit(times, retry_after)`, `limits(rpm, tpm)`, `disconnect(after_chunks)`, \`break\_tool\_call("malformed\_arguments" | "unknown\_tool")` , faults  `truncate |
| [dwmkerr/mock-llm](https://github.com/dwmkerr/mock-llm) | Sim | TypeScript | YAML com regras `path/method/match (JMESPath)/sequence/response`, template `{{jmes request …}}`, `streaming.chunkSize` e `chunkIntervalMs` (50), `/config` em runtime, mocks de MCP/OAuth/A2A, Helm, porta 6556 | `reasoning_content` via template; `/v1/responses` |
| [patrickjm/openai-mock-api](https://github.com/patrickjm/openai-mock-api) | Sim, **arquivado em 27/12/2025** | TypeScript | Matchers `exact/fuzzy/regex/contains/any`, SSE, `tool_calls` multi-turn, tokens via tiktoken | `chunkIntervalMs` e JMESPath (são do dwmkerr) |
| [Httpbun `/llm`](https://httpbun.com/) | Sim | Go | `/llm/v1/chat/completions`, `/llm/v1/completions`, `/llm/v1/responses`, `/llm/v1/messages`; stream de "algumas palavras" a cada 50 ms, máx. 600 chunks | — |
| [SGLang Simulator](https://docs.sglang.io/docs/advanced_features/sglang_simulator) | Sim | Python | Reusa scheduler e KV cache do SGLang com preditor de latência (`aiconfigurator`, `ml`, `replay`); \`SGLANG\_SIMULATOR\_OUTPUT\_MODE=OFFLINE | BLOCKING` ; saída  `metrics.json` ,  `request.jsonl` ,  `iteration.jsonl\` |
| [NVIDIA Dynamo Mocker](https://docs.dynamo.nvidia.com/dynamo/user-guides/dynosim/mocker) | Sim | Python/Rust | Schedulers estilo vLLM e SGLang, block manager LRU, chunked prefill, preempção; timing \`default | fixed(prefill\_ms, decode\_ms) |

**Recursos a importar** (com origem)

- Física de streaming com `tokensPerSecond`, `jitter`, `seed` e `subwordStreaming` por palavra/subpalavra — MockServer.
- Caos declarativo com `errorKind` por provedor, `truncateAtFraction`, `malformedSse`, `contentFilterBlockProbability` e `seed` — MockServer; `stall`, `disconnect(after_chunks)`, `break_tool_call(malformed_arguments|unknown_tool)` — LLMock.
- Cota real com janela (`quotaName/quotaLimit/quotaWindowMillis`) e `limits(rpm, tpm)` que só dá 429 ao exceder — MockServer, LLMock.
- **Relatório de resiliência do cliente**: o mock audita se o roteador retentou, fez backoff e respeitou `Retry-After` (`assert_resilient`, `llmock report`, achado `retry_after_ignored`) — LLMock.
- Predicados de conversa `turnIndex`, `containsToolResultFor`, `latestMessageRole`, `semanticMatchAgainst` — MockServer.
- `usage.reasoningTokens` e `cachedInputTokens` mapeados por provedor — MockServer.
- Tipos oficiais dos SDKs Go (`openai-go`, `anthropic-sdk-go`) para garantir schema — kagent.
- Match por header e `/config` GET/PATCH em runtime — kagent, dwmkerr.
- **Relógio lógico** (`OFFLINE` vs `BLOCKING`): simular latência sem dormir — SGLang Simulator.
- Latência `polynomial` (prefill ∝ tokens, decode ∝ KV ativo), `--speedup-ratio`, eventos KV para routers KV-aware — Dynamo Mocker.
- Teto de chunks por stream (600) como salvaguarda — Httpbun.

**Ideias e soluções da pesquisa**

- Daemon Go com `net/http` puro e zero dependências para o mock não ser o gargalo.
- Introspecção de `tools` com sorteio e argumentos do JSON Schema (respeita `enum`; tipos recursivos); `tool_choice: required` força.
- Parâmetros por requisição via headers `X-Mock-Delay-Ms` e `X-Mock-Tool-Prob`, controláveis pelo gerador de carga.
- `reasoning_content` em deltas antes do conteúdo; primeiro delta de tool call com `id/type/name`, depois `arguments` em pedaços de 12 bytes; chunk de `usage` com `choices: []`.
- Dica 1: fragmentar o JSON das tool calls expõe vazamentos em roteadores que acumulam deltas para telemetria ou guardrails.
- Dica 2: acima de \~2 000 req/s o gargalo vira o pool de conexões do roteador (`MaxIdleConnsPerHost`, TIME\_WAIT, conntrack); exigir keep-alive ou HTTP/2.
- Dica 3: backpressure — mock sem delay e clientes lentos para flagrar OOM nos buffers do roteador.

**Erros da pesquisa.** `chunkIntervalMs`/JMESPath atribuídos ao openai-mock-api (são do dwmkerr); openai-mock-api está arquivado; SGLang Simulator não expõe OpenAI nem serve a load balancing; "30 000 RPS" e "2 000 RPS" sem fonte. No código Go de exemplo: `tool_choice` só funciona como string (ignora `{type: function, …}`), `usage` fixo, `array` ignora `items`, sem `required`/`minimum`/`format`, sem Responses nem Anthropic.

### Fontes

- [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim) e sua [configuração](https://github.com/llm-d/llm-d-inference-sim/blob/main/docs/configuration.md)
- [FakeAI](https://pypi.org/project/fakeai/)
- [llmposter](https://github.com/SkillDoAI/llmposter)
- [mock-llm-server do Phoenix](https://glama.ai/mcp/servers/@Arize-ai/phoenix/blob/0f3ef8b8e2c39f6c5608f236746790b2ce5927b1/scripts/mock-llm-server/README.md)
- [acus9345/mock-llm-server](https://github.com/acus9345/mock-llm-server)
- [mockllm](https://pypi.org/project/mockllm/), [mocktopus](https://pypi.org/project/mocktopus/), [fakellm](https://pypi.org/project/fakellm/), [MockAI](https://pypi.org/project/ai-mock)
- [GuideLLM](https://github.com/vllm-project/guidellm)

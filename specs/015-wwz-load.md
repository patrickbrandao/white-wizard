# 015 — Gerador de carga wwz-load e metodologia

Status: rascunho
Depende de: 004, 011
Origem: seções 14 do documento Claude Docs congelado em 2026-10-30; detalhada para a fase 3 em 2026-10-31 (task 040) e para a fase 4 (task 109)

## Contexto
Cliente CLI de benchmark e a metodologia de teste do roteador (seção 14). A seção "Contrato do `wwz-load` (fase 3)", no fim deste arquivo, é normativa: flags, medidas, classificação de erros, formatos de saída, modo de conversa por cenário e protocolo de instâncias distribuídas. Os subcomandos `agents` e `top` (ADR 0015) são da fase 4, reutilizam este núcleo e têm contrato próprio na seção "Contrato de `agents` e `top` (fase 4)" ([ADR 0036](decisions/0036-agents-e-top-do-wwz-load.md)).

## Requisitos
- Binário `wwz-load` (ADR 0001), sem dependência nova além do que já está no `go.mod` (ADR 0024), exceto `golang.org/x/term` para a TUI do `top` (ADR 0036).
- RF-01: mede TTFT, ITL e E2E no cliente com as definições desta spec, em OpenAI e Anthropic, com e sem streaming.
- RF-02: modos de carga `--rps` (malha aberta), `--concurrency` (malha fechada), `--ramp` e `--sweep`, exatamente um por execução.
- RF-03: saída `text`, `json` e `csv` nos esquemas desta spec, com p50/p90/p99/max/média em ms.
- RF-04: erros classificados por tipo (tabela abaixo); requisições com erro não entram nos histogramas de latência.
- RF-05: `--scenario` joga o papel do agente (devolve `tool_result` inventado e continua a conversa).
- RF-06: `--instances N` por controlador + workers (HTTP/JSON), com agregação exata dos histogramas (ADR 0024).
- RNF-01: percentis por histograma log-linear mesclável com erro relativo ≤ 1 % (ADR 0024).
- RNF-02: em `--rps` a latência do servidor nunca reduz a taxa oferecida; o atraso de agendamento é medido e reportado.

## Interfaces e contratos
- `cmd/wwz-load` (binário) e `internal/loadgen` (núcleo reutilizável pelo `wwz-load agents`); `internal/loadgen/agents` e `internal/loadgen/top` (fase 4); `internal/claudecode/client` (`wwz-load claude-code`, ADR 0074); ferramentas por `internal/tools` (spec 017, ADR 0034).
- Saída texto, JSON e CSV conforme "Saída" abaixo; o JSON é versionado (`"version": 1`).
- Protocolo controlador/worker: HTTP/JSON interno entre binários da mesma versão (não é contrato público).

## Dados
- Relatórios locais do operador (`--out`); nada é persistido pelo `wwz-load`.

## Critérios de aceite
- [ ] Reporta TTFT, ITL, E2E p50/p90/p99, req/s, tokens/s e erros por tipo em OpenAI e Anthropic com streaming
- [ ] `--rps`, `--concurrency`, `--ramp` e `--sweep` atingem a taxa/concorrência pedida dentro de 5 % contra um `wwz` com latência zero, e `--ramp`/`--sweep` produzem uma linha por passo
- [ ] JSON e CSV seguem os esquemas desta spec (teste golden)
- [ ] `stream_truncated`, `timeout` e `http_429/<type>` aparecem classificados contra o caos do `wwz`
- [ ] `--scenario scenarios/file-agent-flow.yaml` (sem `--header`) conclui conversas preservando ordem e ids das tool calls: toda requisição leva `x-wwz-scenario: file-agent-flow`, `order_violations` = 0 e `scenario_not_applied` = 0 (ADR 0041)
- [ ] Com `--header 'x-wwz-scenario: outro'` vale o valor explícito; com `--header 'x-wwz-scenario:'` nenhuma requisição leva o header (ADR 0041)
- [ ] Contra um wwz que não aplica o cenário (resposta sem `x-wwz-scenario`, `stop` no primeiro turno), o relatório traz `scenario_not_applied` > 0, a linha de aviso no texto e as conversas em `failed_by_reason.incomplete`, nunca em `completed` (ADR 0041)
- [ ] Contra o driver `openai` com `reasoning_field: hidden`, um passo com `reasoning` cuja resposta traz `reasoning_tokens` > 0 sem texto não conta em `reasoning_missing`; sem texto e com `reasoning_tokens` = 0, conta (ADR 0041)
- [ ] Controlador + 3 workers: soma das requisições igual ao total e percentis agregados iguais aos de um histograma único com as mesmas amostras
- [ ] Com `--seed N` e 3 workers, os valores de `x-wwz-seed` enviados são distintos e iguais aos de uma execução numa máquina com o mesmo total (ADR 0033)
- [ ] `SIGINT` no controlador durante o run para os workers e gera relatório parcial com `interrupted: true` e os workers que responderam (ADR 0033)
- [ ] `wwz-load agents` conclui N conversas multi-turno com tool calls executadas e relatório por agente (fase 4)
- [ ] `wwz-load agents --retries 2` contra caos `rate_limit` com `retry-after` espera pelo menos o `retry-after` e conta `retries`; com `--retries 0` o mesmo erro aparece em `errors_by_type` (fase 4)
- [ ] `wwz-load agents` com `--tools-url` (servidor `wwz-tools`) e em processo dão o mesmo relatório de ferramentas para a mesma semente (fase 4)
- [ ] JSON e CSV do `agents` e JSON do `top --once` seguem os esquemas desta spec (teste golden) (fase 4)
- [ ] `wwz-load top` mostra `/admin/stats` ao vivo e sai com `q` (fase 4)
- [ ] `wwz-load top --once` com duas `--admin` soma os contadores e calcula req/s pela diferença entre as amostras (fase 4)
- [ ] `wwz-load claude-code replay` reproduz cada fixture de `testdata/real/claude-code/2.1.308/` com o mesmo sha256; `ask`, `login`, `refresh` e `run` funcionam contra o wwz com `oauth_key` e `client_verification: claude-code`; `--target` em host real da Anthropic sai com código 2 sem conexão ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md))

## Fora de escopo
- Interface gráfica.
- HTTP/2 e dialetos além de OpenAI e Anthropic. OpenRouter, OpenCode Go e Azure OpenAI falam o dialeto OpenAI: `--protocol openai` com `--path` (no Azure, `--path /azure/openai/v1/chat/completions` ou a rota por deployment com `?api-version=`, e `--header api-key:<chave>`). O dialeto Gemini (`generateContent`) não tem `--protocol` no MVP; medir o driver `gemini` pelo `wwz-load` fica para uma ADR futura.
- Sincronizar relógios entre máquinas (os workers assumem NTP).

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0015-wwz-load-agents-e-top.md](decisions/0015-wwz-load-agents-e-top.md)
- [specs/decisions/0024-percentis-e-cliente-do-wwz-load.md](decisions/0024-percentis-e-cliente-do-wwz-load.md)
- [specs/decisions/0033-sementes-e-parada-do-wwz-load-distribuido.md](decisions/0033-sementes-e-parada-do-wwz-load-distribuido.md)
- [specs/decisions/0041-wwz-load-scenario-header-e-conclusao.md](decisions/0041-wwz-load-scenario-header-e-conclusao.md)
- [specs/decisions/0034-contrato-do-wwz-tools.md](decisions/0034-contrato-do-wwz-tools.md)
- [specs/decisions/0036-agents-e-top-do-wwz-load.md](decisions/0036-agents-e-top-do-wwz-load.md)
- [specs/decisions/0047-regras-do-wwz-load-agents.md](decisions/0047-regras-do-wwz-load-agents.md)
- [specs/decisions/0045-regras-do-wwz-load-top.md](decisions/0045-regras-do-wwz-load-top.md)
- [specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### 14. Gerador de carga e metodologia de teste

O gerador de carga é uma ferramenta separada; para dezenas de milhares de req/s com streaming, as ferramentas prontas não bastam e o projeto inclui um gerador próprio em Go (`wwz-load`), no mesmo repositório.

**Ferramentas prontas e seus limites**

| Ferramenta | Fala SSE de LLM | Alcance típico | Uso recomendado |
| --- | --- | --- | --- |
| [GuideLLM](https://github.com/vllm-project/guidellm) (vLLM project) | Sim, OpenAI | Centenas a poucos milhares de req/s | Perfis `synchronous`, `concurrent`, `sweep`; relatórios de TTFT/ITL/throughput; benchmarks de tool calling |
| AIPerf (NVIDIA) | Sim, OpenAI | Semelhante ao GuideLLM | Mesmo papel, integrado ao FakeAI |
| k6 | Com script de parsing de SSE | Dezenas de milhares de req/s em modo distribuído | Testes de API genéricos, cenários de usuário |
| oha / wrk2 / vegeta | Não (só mede HTTP) | Centenas de milhares de req/s | Teto de req/s não-streaming do roteador e do simulador |

**`wwz-load`** (fase 3): cliente Go que fala OpenAI e Anthropic com streaming, gera requisições a partir dos mesmos cenários da seção 9 (joga o papel do agente: recebe a tool call, devolve um `tool_result` inventado, continua a conversa), e reporta TTFT, ITL, E2E, erros por tipo, req/s e tokens/s em p50/p90/p99. Modos de carga: taxa fixa (`--rps`), concorrência fixa (`--concurrency`), rampa (`--ramp 100:20000:5m`) e `--sweep`. Suporta `--instances N` coordenadas por um controlador simples para distribuir a carga por várias máquinas.

**Subcomandos `agents` e `top`** ([ADR 0015](decisions/0015-wwz-load-agents-e-top.md)).
- `wwz-load agents --agents N --profile coder|pesquisa|chat --turns M --target URL --retries R`: N agentes concorrentes multi-turno; cada tool call recebida é executada via `wwz-tools` em sandbox e volta como `tool_result`; retries com backoff respeitando `retry-after`; relatório por agente e agregado (turnos, tool calls, erros por tipo, TTFT/E2E p50/p99) em texto, JSON ou CSV.
- `wwz-load top --admin URL --token $WWZ_ADMIN_TOKEN --interval 1s`: TUI estilo htop sobre `/admin/stats` (req/s, streams abertos, erros por tipo, latência p50/p99, uso por chave e modelo); `q` sai.

**Metodologia**

1. **Teto do simulador sozinho.** `oha` direto no `wwz` com latência zero; registra o máximo de req/s da instância (meta ≥ 10 000) para garantir que ele nunca seja o gargalo.
2. **Teto do roteador.** Mesmo teste passando pelo roteador; a diferença é o overhead do roteador.
3. **Streaming em escala.** `wwz-load --concurrency 20000` com ITL de 20 ms; observa conexões abertas e memória nos dois lados.
4. **Failover.** Dois modelos no simulador com chaves diferentes: um com `errors.rate: 30` e `rate_limit`, outro saudável; mede quanto o roteador redireciona e com que atraso.
5. **Cota e backpressure.** Chave com `rpm: 300` e `on_limit: queue`; verifica se o roteador respeita `retry-after` e os headers `x-ratelimit-*`/`anthropic-ratelimit-*`.
6. **Agentes.** Cenário `file-agent-flow` por 1 000 conversas simultâneas; valida que o roteador preserva ordem de tool calls, `tool_call_id` e `reasoning_content` de ponta a ponta.
7. **Caos.** `stream_truncated`, `stream_hang` e `connection_reset` a 5 %; mede timeouts e vazamento de conexões no roteador.
8. **Reprodução.** Todo teste roda com `seed` fixo e salva `/admin/stats` ao final, para comparar execuções entre versões do roteador.

**Ambiente de referência.** `docker-compose` em `deploy/` com 3 instâncias do `wwz` com perfis diferentes (rápido, realista, instável) atrás de um `nginx` L4, Prometheus e Grafana com painel pronto; o roteador aponta para `http://lb:8000`.

## Contrato do `wwz-load` (fase 3)

### Subcomandos

| Subcomando | Fase | O que faz |
| --- | --- | --- |
| `run` (padrão: `wwz-load --rps 100` = `wwz-load run --rps 100`) | 3 | Gera carga e imprime o relatório |
| `controller` | 3 | Coordena N workers (ver "Instâncias distribuídas") |
| `worker` | 3 | Executa a fatia de carga recebida do controlador |
| `version` | 3 | Imprime versão, commit e versão do Go |
| `agents`, `top` | 4 | ADR 0015 e 0036; contrato em "Contrato de `agents` e `top` (fase 4)" (na fase 3 saíam com código 2 e "não implementado") |
| `claude-code` | ADR 0074 | Cliente que se comporta como o Claude Code 2.1.308 (`login`, `refresh`, `ask`, `replay`, `run`); contrato em "Contrato do `wwz-load claude-code`" |

### Flags de `run`

Pacote `flag` da stdlib (`-x` e `--x` equivalem). Durações no formato Go (`30s`, `5m`).

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--target URL` | `http://127.0.0.1:8000` | Base do alvo (wwz ou roteador), `http` ou `https` |
| `--protocol openai\|anthropic` | `openai` | Dialeto do corpo, da autenticação e do parser SSE |
| `--path P` | `/v1/chat/completions` (openai), `/v1/messages` (anthropic) | Caminho somado ao `--target` |
| `--model M` | `wwz-fast` | Campo `model` |
| `--key K` | `$WWZ_LOAD_KEY`, senão `wwz-key-01` | OpenAI: `Authorization: Bearer K`; Anthropic: `x-api-key: K` + `anthropic-version: 2023-06-01`. A flag vence a variável |
| `--stream` | `true` | `stream: true`; em OpenAI acrescenta `stream_options: {"include_usage": true}`. `--stream=false` para não-stream |
| `--max-tokens N` | `256` | `max_tokens` |
| `--prompt TEXTO` | `Escreva um parágrafo curto sobre testes de carga.` | Mensagem única de usuário |
| `--prompt-file ARQ` | — | Uma mensagem por linha não vazia; cada requisição sorteia uma linha (sorteio derivado da semente). Exclusiva com `--prompt` |
| `--scenario ARQ` | — | Modo conversa (ver abaixo). Exclusiva com `--prompt`/`--prompt-file`. Envia `x-wwz-scenario: <name do YAML>` em toda requisição, salvo `--header` explícito com o mesmo nome (valor vazio suprime) |
| `--max-turns N` | `step_limit` declarado no YAML do cenário, senão `12` | Teto de turnos por conversa (só com `--scenario`) |
| `--tool-result-error-rate P` | `0` | Percentual (0–100) de `tool_result` devolvidos como erro (só com `--scenario`) |
| `--seed N` | `0` | `0` = semente aleatória, registrada no relatório. Com `N ≠ 0`, toda requisição leva `x-wwz-seed: N + i` (i = índice global da requisição, a partir de 0; com `--scenario`, i = índice da conversa e todos os turnos dela levam a mesma semente, para as `vars` do cenário não mudarem no meio da conversa) e os sorteios locais derivam de N |
| `--header 'k: v'` | — | Header extra, repetível. Ex.: `--header 'x-test-id: noite-01'` (escopo de caos, spec 009) |
| `--test-id ID` | — | Atalho para `--header 'x-test-id: ID'` (valor padrão de `chaos.scope_header`) |
| `--duration D` | `30s` | Duração da medição, sem contar `--warmup`. `0` = sem prazo: para em `--requests` ou no sinal. Proibida com `--ramp`/`--sweep`, que definem a própria duração |
| `--requests N` | `0` (sem teto) | Encerra ao iniciar N requisições medidas (com `--scenario`, N conversas). Com `--duration`, vale o que ocorrer primeiro |
| `--timeout D` | `60s` | Teto da requisição inteira (envio até o fim do corpo); estourado vira `timeout` |
| `--warmup D` | `0s` | Período inicial com a mesma carga cujas requisições não entram em medida nem contagem |
| `--rps N` | — | Modo taxa fixa (malha aberta), N > 0, aceita fração |
| `--concurrency N` | — | Modo concorrência fixa (malha fechada), N ≥ 1 inteiro |
| `--ramp DE:ATE:DUR` | — | Taxa cresce linearmente de DE a ATE req/s ao longo de DUR (ex.: `100:20000:5m`) |
| `--sweep` | — | Varredura de taxas em passos (ver "Modos") |
| `--sweep-rates LISTA` | — | Taxas explícitas (`100,500,1000`); exclusiva com `--sweep-from/to/steps` |
| `--sweep-from N`, `--sweep-to N`, `--sweep-steps K` | `10`, `1000`, `8` | K taxas em progressão geométrica de `from` a `to`, inclusive |
| `--step-duration D` | `10s` (ramp), `30s` (sweep) | Duração de cada passo; o último passo da rampa pode ser menor |
| `--sweep-stop-error-pct P` | `50` | Interrompe o sweep depois de um passo com mais de P % de erros (`0` desliga) |
| `--max-inflight N` | `10000` | Teto de requisições em voo em `--rps`/`--ramp`/`--sweep`; acima dele a partida agendada é descartada e contada em `dropped` |
| `--output text\|json\|csv` | `text` | Formato do relatório |
| `--out ARQ` | stdout | Grava o relatório no arquivo (criado ou truncado); progresso continua no stderr |
| `--admin URL` | — | Control plane do wwz; ao fim, `GET URL/admin/stats` vai no relatório (`admin_stats`), metodologia item 8 |
| `--admin-token T` | `$WWZ_ADMIN_TOKEN` | Bearer do `--admin` |
| `--fail-on-error-pct P` | `0` (desligado) | Sai com código 3 se `errors / requests` passar de P % |
| `--progress D` | `5s` | Intervalo da linha de progresso no stderr (`0` desliga) |

Sem nenhum modo, vale `--concurrency 1` (síncrono). Dois modos juntos, flag desconhecida, valor inválido ou combinação proibida: mensagem que cita a flag e código 2.

**Códigos de saída:** `0` execução concluída (mesmo com erros de requisição); `1` falha de execução (arquivo de `--out` não gravável, cenário inválido, controlador inalcançável); `2` uso inválido; `3` `--fail-on-error-pct` estourado; `4` worker faltante no modo distribuído. `SIGINT`/`SIGTERM` param as partidas, esperam as requisições em voo até `--timeout` e emitem o relatório parcial com `"interrupted": true` (código 0, ou 3 pela regra acima).

### Modos

- **`--rps N`:** malha aberta. Partidas agendadas pelo relógio em intervalos constantes de `1/N` (sem Poisson), cada uma em goroutine própria até `--max-inflight`. O atraso entre o instante agendado e o envio real entra no histograma `schedule_lag_ms`. A latência do servidor nunca atrasa a próxima partida.
- **`--concurrency N`:** malha fechada. N workers; cada um envia a próxima requisição assim que a anterior termina (sucesso ou erro).
- **`--ramp DE:ATE:DUR`:** malha aberta com taxa instantânea `DE + (ATE − DE) · t/DUR`. Um passo por `--step-duration`; `target_rps` do passo é a taxa alvo média no intervalo.
- **`--sweep`:** malha aberta, um passo por taxa (`--sweep-rates`, ou `from · (to/from)^(k/(K−1))` para k = 0..K−1, arredondada a 3 casas), cada um por `--step-duration`, sem pausa entre passos. A requisição pertence ao passo em que partiu.
- `--warmup` antecede o primeiro passo com a carga dele (na rampa, `DE`).

### Medidas no cliente

`t0` = instante imediatamente antes de entregar a requisição ao cliente HTTP (fasthttp com `StreamResponseBody`, conexões keep-alive reutilizadas; numa conexão nova o estabelecimento entra no tempo, por isso o `--warmup` é recomendado). Com corpo de poucos KiB, `t0` equivale ao envio do último byte.

| Medida | Definição |
| --- | --- |
| TTFT | `t0` até o primeiro evento SSE com **conteúdo visível, raciocínio ou tool call**: OpenAI `choices[].delta.content` não vazio, `delta.reasoning_content`/`delta.reasoning` não vazio, ou `delta.tool_calls`; Anthropic `content_block_delta` (`text_delta`, `thinking_delta`, `input_json_delta`) ou `content_block_start` de `tool_use`. Não contam: comentários/keep-alive (`:`), `ping`, `message_start`, chunk só com `role`, `content_block_start` de texto/thinking vazio. Sem stream: TTFT = E2E |
| ITL | Intervalo entre dois eventos com conteúdo consecutivos da mesma resposta; **cada** intervalo é uma amostra (k eventos de conteúdo geram k−1 amostras). Sem stream: nenhuma amostra |
| E2E | `t0` até o último byte do corpo |
| Tokens | Do `usage`: OpenAI `prompt_tokens`, `completion_tokens`, `completion_tokens_details.reasoning_tokens`; Anthropic `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens` e `output_tokens` (do `message_delta` final). Sem `usage`: entrada 0 e saída = número de eventos com conteúdo, com `tokens_estimated: true` |
| req/s | Requisições concluídas (sucesso + erro) no passo ÷ duração do passo |
| tokens/s | `output_tokens_per_s` = tokens de saída das requisições com sucesso ÷ duração; idem `input_tokens_per_s` |

**Sem alocação por chunk ([ADR 0069](decisions/0069-pacing-pelo-fim-planejado-e-gogc.md)).** A medida de ITL não pode depender do GC do próprio cliente: o decodificador de SSE do `wwz-load` reusa o chunk decodificado e não aloca por evento no caminho OpenAI. O caminho Anthropic ainda aloca por evento (follow-up, task 400).

Sucesso = status 2xx com corpo completo (stream terminado por `data: [DONE]` em OpenAI ou `message_stop` em Anthropic; não-stream com JSON válido). Só sucessos entram em TTFT, ITL e E2E; toda requisição concluída entra em `requests`.

### Classificação de erros

| Tipo | Quando |
| --- | --- |
| `http_<status>/<error.type>` | Status ≥ 400 com envelope reconhecido: `error.type` (OpenAI, Anthropic) ou `error.code` (OpenRouter). Ex.: `http_429/rate_limit_error`, `http_529/overloaded_error` |
| `http_<status>` | Status ≥ 400 sem envelope reconhecível |
| `stream_error/<type>` | Status 200 e evento de erro no meio do stream (`event: error` da Anthropic, chunk com `error` em OpenAI) |
| `stream_truncated` | Corpo terminou limpo sem `[DONE]`/`message_stop` |
| `reset` | Conexão fechada com erro (RST, EOF inesperado) depois do envio |
| `connect` | Falha de DNS, conexão recusada ou handshake TLS |
| `timeout` | `--timeout` estourado (inclusive `stream_hang`) |
| `parse` | Status 2xx com SSE ou JSON inválido |

`dropped` (descarte por `--max-inflight`) não é erro de requisição: é contado à parte e indica cliente saturado.

### Saída

**JSON** (`--output json`), tempos em ms com 3 casas, taxas por segundo:

```json
{
  "version": 1,
  "tool_version": "v0.1.0",
  "started": "2026-10-31T03:00:00Z",
  "finished": "2026-10-31T03:05:00Z",
  "interrupted": false,
  "config": {"target": "http://127.0.0.1:8000", "protocol": "openai", "path": "/v1/chat/completions",
             "model": "wwz-fast", "stream": true, "max_tokens": 256, "mode": "ramp",
             "rps": 0, "concurrency": 0, "ramp": "100:20000:5m", "sweep_rates": [],
             "step_duration_ms": 10000, "duration_ms": 300000, "requests": 0, "warmup_ms": 0,
             "timeout_ms": 60000, "seed": 42, "scenario": "", "instances": 1, "headers": ["x-test-id"]},
  "steps": [{"step": 1, "target_rps": 431.667, "concurrency": 0, "start_ms": 0, "duration_ms": 10000}],
  "total": {"step": "total"},
  "conversations": null,
  "instances": null,
  "admin_stats": null
}
```

Cada passo e o `total` trazem, além dos campos do exemplo, o **resumo**: `requests`, `ok`, `errors`, `dropped`, `duration_ms`, `rps`, `ok_rps`, `input_tokens`, `output_tokens`, `reasoning_tokens`, `tokens_estimated`, `input_tokens_per_s`, `output_tokens_per_s`, `ttft_ms`, `itl_ms`, `e2e_ms`, `schedule_lag_ms` (zerada em `--concurrency`) e `errors_by_type` (mapa tipo → contagem). Cada distribuição é `{"count", "mean", "p50", "p90", "p99", "max"}`; sem amostras, tudo 0. `config.headers` traz só os nomes dos headers extras (valores podem ser segredos). `--rps` e `--concurrency` têm `steps: []`. `admin_stats` é o corpo cru de `/admin/stats` (ou `{"error": "..."}` se falhou, sem mudar o código de saída).

**CSV** (`--output csv`): cabeçalho fixo, uma linha por passo e a última com `step=total` (sem passos: só a linha `total`):

```
step,target_rps,concurrency,start_ms,duration_ms,requests,ok,errors,dropped,rps,output_tokens_per_s,ttft_p50_ms,ttft_p90_ms,ttft_p99_ms,ttft_max_ms,ttft_mean_ms,itl_p50_ms,itl_p90_ms,itl_p99_ms,itl_max_ms,itl_mean_ms,e2e_p50_ms,e2e_p90_ms,e2e_p99_ms,e2e_max_ms,e2e_mean_ms,schedule_lag_p99_ms,error_types
```

`error_types` = `tipo=n` separados por `;`, ordenados por tipo; aspas e vírgulas seguem a RFC 4180.

**Texto** (`--output text`, para humanos, não é contrato): cabeçalho com alvo, modo e semente; tabela por passo (req/s, ok, erros, TTFT/ITL/E2E p50 e p99); bloco `total` com todas as distribuições, tokens/s e erros por tipo em ordem decrescente.

### Modo conversa (`--scenario`)

O cliente joga o papel do agente com um cenário da spec 011 (lido por `internal/scenario`). Toda requisição de todos os turnos leva `x-wwz-scenario: <name do YAML>`, o primeiro critério de `scenarios.select` (spec 011), para o cenário valer mesmo quando o modelo não o lista em `scenarios` ([ADR 0041](decisions/0041-wwz-load-scenario-header-e-conclusao.md)). Um `--header` explícito com esse nome (sem diferenciar maiúsculas) vence. Com valor vazio (`--header 'x-wwz-scenario:'`), o header não é enviado e a seleção fica com `metadata`, marcador, sufixo ou `match`, através do roteador. Quando o header é enviado, `config.headers` inclui `x-wwz-scenario`.
1. Primeira requisição: uma ferramenta declarada para cada nome em `match.tools_include` e para cada nome literal de `tool_calls[].name` dos passos (sem repetir; classes `any`/`any_read`/`any_write` e nomes com template não contam; sem nenhum nome, `read_file` e `write_file`), todas com schema `{"type": "object", "additionalProperties": true}`, uma mensagem de sistema fixa e a mensagem de usuário que satisfaz `match.user_message_matches` (texto literal derivado do padrão; sem padrão, o `--prompt`).
2. Resposta com tool calls: acrescenta a mensagem do assistente exatamente como recebida (OpenAI `tool_calls` com `id`; Anthropic blocos `tool_use`, inclusive `thinking` com `signature`) e um resultado por chamada, na mesma ordem (`role: tool` + `tool_call_id`; ou bloco `tool_result` + `tool_use_id`). Conteúdo inventado e determinístico pela semente: `ok` para nomes que começam com `write`, `edit`, `create` ou `delete`; um parágrafo lorem para os demais; com probabilidade `--tool-result-error-rate` vai como erro (`is_error: true` na Anthropic; prefixo `error:` em OpenAI).
3. Repete até um `finish_reason` final sem tool calls (`stop`/`end_turn`/`length`/`max_tokens`), um erro ou `--max-turns` (conversa marcada `max_turns`).
4. A conversa que termina por `finish_reason` final só é **concluída** se a resposta final cair num **passo terminal**, ou seja, num passo sem `tool_calls` declaradas (no `file-agent-flow`, `done` ou `give-up`). O passo vem do `x-wwz-step` da resposta. Sem esse header, vem da previsão do cliente (a mesma de `order_violations`). Quando o passo pedia tool calls ou o `x-wwz-step` não existe no cenário, a conversa falha com o motivo `incomplete`. Quando o passo não é conhecido (sem header e sem previsão), a conversa conta como concluída.

Nos modos de carga a **unidade é a conversa**: `--rps` é taxa de início de conversas, `--concurrency` é conversas simultâneas, `--requests` conta conversas. Cada turno é uma requisição medida em TTFT/ITL/E2E. Uma conversa iniciada vai até o fim mesmo depois que o plano de carga termina (`--duration`, `--requests`, fim do `--ramp`/`--sweep`); `SIGINT`/`SIGTERM` a interrompem antes do turno seguinte, e um turno cancelado no meio também a marca como interrompida.

O relatório ganha `conversations`:
- `started` = `completed` + `failed` + `interrupted`.
- `failed_by_reason`: contagem por motivo — a classe de erro da requisição que falhou (a mesma de `errors`), `max_turns` (atingiu `--max-turns` com tool calls pendentes), `no_tool_calls` (`finish_reason` `tool_calls`/`tool_use` sem nenhuma chamada), `incomplete` (terminou antes de um passo terminal, item 4 acima) ou `finish_reason:<x>` (parou sem tool calls por um motivo não final, ex. `finish_reason:content_filter`). `max_turns` é o mesmo número de `failed_by_reason.max_turns`.
- `turns` (distribuição de turnos por conversa, sem as interrompidas), `tool_calls`, `e2e_ms` (distribuição da conversa inteira, só as concluídas).
- `order_violations`: respostas com tool calls fora do passo esperado (quantidade, nomes literais ou índice `<n>` do id fora da posição). O passo esperado vem do id do wwz (`tool_call_id_format: encoded`: `call_<cenário>_<passo>_<n>_<nonce>` e, nas rotas Messages, `toolu_<cenário>_<passo>_<n>_<nonce>`) ou, sem id decodificável, da previsão do cliente pelo próprio histórico (`turn`, `tool_results`, `last_tool`, `last_tool_result_matches`, `tool_result_for`…); quando o cliente não consegue prever (condição que ele não vê, empate com `weight`, `step_limit` atingido sem `repeat_last`), a resposta não é conferida. Passos com `tool_misbehavior` ou `timeline` não são conferidos.
- `id_mismatches`: respostas com `tool_call_id`/`tool_use_id` vazio ou repetido dentro da mesma resposta (repetir ids entre turnos é válido: um passo repetido reemite os mesmos ids).
- `scenario_not_applied`: respostas 2xx sem o header `x-wwz-scenario` ou com valor diferente do `name` do YAML. O motivo pode ser que o wwz não aplicou o cenário ou que o roteador removeu o header. Quando é > 0, a saída texto ganha a linha `aviso: N respostas sem x-wwz-scenario: <nome> (cenário não aplicado ou header removido pelo roteador)`. O código de saída não muda.
- `reasoning_missing`: o passo esperado pedia raciocínio e ele não veio. O raciocínio conta como presente quando vem texto (`reasoning_content`/`reasoning`, blocos `thinking` ou `redacted_thinking`) **ou** quando o `usage` da resposta traz `completion_tokens_details.reasoning_tokens` > 0. Esse segundo caso cobre o driver `openai` com `reasoning_field: hidden`, o padrão da ADR 0022, e o OpenRouter com `reasoning.exclude: true`. Esse laço, sem execução real de ferramentas, é a base do `wwz-load agents` (fase 4).

### Instâncias distribuídas (`--instances N`)

`wwz-load controller --listen :9100 --instances N [--token T] [--register-timeout 5m] <flags de run>` e, em cada máquina de carga, `wwz-load worker --controller http://host:9100 [--token T] [--id nome]`. Token também por `WWZ_LOAD_TOKEN`; com token, toda chamada leva `Authorization: Bearer T` e, sem ele, o controlador responde 401. Transporte `net/http` + JSON.

| Rota do controlador | Corpo | Resposta |
| --- | --- | --- |
| `POST /v1/register` | `{"id", "hostname", "version"}` | Long-poll até os N registros: `200 {"index", "instances", "start_at", "plan"}`. Sem `id`: `400`. Versão diferente da do controlador, `id` já registrado ou registro além de N: `409`. Worker que desiste do long-poll antes dos N registros libera a vaga |
| `POST /v1/result` | `{"index", "report", "histograms", "error", "late_start_ms"}` | `204`. Corpo inválido (sem histogramas e sem `error`): `400`. `index` desconhecido, resultado repetido ou coleta já encerrada: `409` |
| `GET /v1/status` | — | `{"state": "waiting\|running\|stopping\|collecting\|done", "registered", "results"}` |

Corpos limitados a 64 MiB. Os índices são atribuídos na ordem de registro (0 a N−1).

- **Plano:** as flags de `run` com a carga dividida por N: `rps`, extremos do `ramp` e taxas do `sweep` divididos igualmente (fração permitida); `concurrency` e `requests` inteiros, com o resto distribuído aos menores índices; `warmup`, `step_duration`, `duration` e `timeout` iguais. O controlador lê `--prompt-file` e `--scenario` (validando o cenário antes de escutar) e manda o conteúdo dentro do plano: as máquinas de carga não precisam de nenhum arquivo. `--concurrency` ou `--requests` (quando > 0) menor que `--instances` não divide em fatias não vazias: o controlador sai com código 2 antes de escutar.
- **Sementes ([ADR 0033](decisions/0033-sementes-e-parada-do-wwz-load-distribuido.md)):** a semente do relatório é a do controlador (sorteada por ele quando `--seed 0`). Com `--seed N ≠ 0`, o worker `k` manda `x-wwz-seed: N + k + instances · i` na sua requisição (ou conversa) local `i`: os workers usam conjuntos disjuntos cuja união é a de uma máquina só (`i` global = `k + instances · i`). Os sorteios locais (prompt, `tool_result`) usam a semente de sorteio + `k`.
- **Início:** `start_at` = instante do último registro + 2 s (RFC 3339 com nanossegundos). O worker espera até `start_at` pelo próprio relógio; atrasado (mais de 1 ms), começa na hora e informa `late_start_ms` no `POST /v1/result`.
- **Resultado:** o worker envia o relatório do próprio run, `late_start_ms` e os histogramas serializados (ADR 0024) de cada passo e do total (e as estatísticas de conversa com `--scenario`). O controlador soma contadores, mescla histogramas passo a passo (mesmo índice) e emite um relatório único no formato acima, com `instances: {"expected": N, "reported": n, "missing": [ids], "workers": [{"id", "hostname", "requests", "late_start_ms"}]}`. `workers` lista só quem reportou com sucesso; `reported` é o tamanho dessa lista.
- **Parada ([ADR 0033](decisions/0033-sementes-e-parada-do-wwz-load-distribuido.md)):** `SIGINT`/`SIGTERM` no controlador antes de todos registrarem: sai com código 1. Depois do registro, o controlador passa a `stopping` e espera os resultados por até `--timeout + 30 s`. Durante o run, cada worker consulta `GET /v1/status` a cada 1 s; ao ver `stopping`, para como num `SIGINT` próprio (para as partidas, espera as requisições em voo até `--timeout`) e envia o resultado parcial com `"interrupted": true`; falha na consulta não para o worker. O relatório sai com `"interrupted": true`.
- **Falhas:** sem os N registros em `--register-timeout`, o controlador sai com 1. Worker sem resultado até `duração prevista + timeout + 30 s` entra em `missing`; com `--duration 0` (sem prazo) não há esse limite e o controlador espera todos os resultados ou o sinal. Worker que não conseguiu rodar (plano inválido, cenário recusado, interrompido antes de `start_at`, erro do run) envia `error` em vez das medidas: entra em `missing` e o controlador escreve `wwz-load controller: worker <id>: <erro>` no stderr. Com algum `missing`, o relatório sai com quem respondeu e código 4. Worker que perde o controlador termina o run, tenta enviar 3 vezes (1 s entre tentativas, 30 s cada) e sai com 1; worker que enviou `error` sai com 1.

## Contrato de `agents` e `top` (fase 4)

Normativo ([ADR 0015](decisions/0015-wwz-load-agents-e-top.md), [ADR 0036](decisions/0036-agents-e-top-do-wwz-load.md), [ADR 0047](decisions/0047-regras-do-wwz-load-agents.md)).

### `wwz-load agents`

N agentes concorrentes, cada um conduzindo sessões multi-turno. As tool calls recebidas são executadas por `internal/tools` (spec 017, [ADR 0034](decisions/0034-contrato-do-wwz-tools.md)) e o resultado volta como `tool_result`. Núcleo em `internal/loadgen/agents`, que reaproveita o cliente, o parser SSE, a classificação de erros e os histogramas do `run`.

**Flags herdadas do `run`, com o mesmo significado e padrão:** `--target`, `--protocol`, `--path`, `--model`, `--key`, `--stream`, `--max-tokens`, `--header`, `--test-id`, `--timeout`, `--seed`, `--output`, `--out`, `--admin`, `--admin-token`, `--progress` e `--fail-on-error-pct` (regra própria abaixo). As flags de modo (`--rps`, `--concurrency`, `--ramp`, `--sweep`), `--prompt*`, `--scenario`, `--warmup`, `--requests` e `--instances` são recusadas com código 2.

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--agents N` | `10` | Agentes concorrentes (N ≥ 1) |
| `--profile coder\|pesquisa\|chat\|mixed` | `coder` | Perfil; `mixed` atribui `coder`, `pesquisa`, `chat` em rodízio pelo índice do agente |
| `--turns M` | `5` | Turnos de usuário por sessão (M ≥ 1) |
| `--max-tool-rounds K` | `8` | Requisições por turno enquanto vierem tool calls; estourado, a sessão falha com `max_tool_rounds` |
| `--sessions S` | `1` | Sessões por agente quando `--duration 0`; explícita com `--duration > 0`, é recusada com código 2 |
| `--duration D` | `0` | `0` = cada agente roda `--sessions` sessões e para. `D > 0` = os agentes abrem sessões novas até D; a sessão em curso vai até o fim (como no modo conversa) |
| `--think-time D` | `0s` | Pausa entre o fim de um turno e a próxima mensagem do usuário |
| `--spawn-interval D` | `0s` | Intervalo entre as partidas dos agentes (rampa de entrada) |
| `--tool-prob P` | — | Envia `x-wwz-tool-prob: P` (I4) para o wwz gerar tool calls sem cenário; não tem efeito contra um roteador que remova o header |
| `--tools-url URL` | — (em processo) | Executa as ferramentas num `wwz-tools serve` (`POST /v1/call`, bearer de `$WWZ_TOOLS_TOKEN`) em vez de em processo |
| `--tools-config ARQ` | — | YAML do `wwz-tools` (spec 017) para o executor em processo (taxas de erro e latência das ferramentas); exclusiva com `--tools-url` |
| `--retries R` | `2` | Novas tentativas por requisição em erro transitório; `0` desliga |
| `--backoff D` | `500ms` | Base do backoff: espera `rand(0, min(--backoff-max, D · 2^tentativa))` (jitter total) |
| `--backoff-max D` | `30s` | Teto do backoff |
| `--retry-after-max D` | `60s` | Maior `retry-after` aceito. Acima dele, desiste e a requisição falha com o tipo original |

**Perfis.** Cada perfil tem mensagem de sistema fixa, uma primeira mensagem e uma lista de mensagens de continuação (a de índice `t` vem da lista circular, sorteada pela semente), além das ferramentas do perfil no catálogo do `wwz-tools`, declaradas no dialeto do `--protocol` sem `tool_choice` (o padrão dos provedores, `auto`; [ADR 0047](decisions/0047-regras-do-wwz-load-agents.md)). `chat` não declara ferramentas: é conversa pura com histórico crescente.

**Sessão.**
1. O turno `t` acrescenta a mensagem de usuário ao histórico e envia a requisição.
2. Resposta com tool calls: acrescenta a mensagem do assistente exatamente como recebida (as mesmas regras do modo conversa, inclusive `thinking` com `signature`). Executa cada chamada na ordem pela sessão de ferramentas do agente (`session = <run>-a<agente>-s<sessão>`) e acrescenta os resultados (`is_error` vira `is_error: true` na Anthropic ou o prefixo `error:` na OpenAI). Depois repete, até `--max-tool-rounds`.
3. Resposta final (`stop`/`end_turn`/`length`/`max_tokens` sem tool calls): o turno termina. Após `--think-time`, vem o próximo turno, até `--turns`.
4. Requisição que falha depois das novas tentativas encerra a sessão como `failed` com a classe do erro como motivo.
5. Com `--tools-url`, a resposta do `wwz-tools` vira resultado assim: 200 = o resultado devolvido; 504 = resultado `timeout` (`is_error`); 404 = `natural_error` (ferramenta desconhecida, a mesma mensagem do executor em processo). Outro status, corpo inválido ou falha de conexão encerram a sessão como `failed` com motivo `tools_unavailable`, sem nova tentativa.
- Toda requisição da sessão leva `x-wwz-conversation-id: <run>-a<agente>-s<sessão>` (I11). `<run>` tem 8 hex: com `--seed N ≠ 0`, derivados de N (`agents.RunID`), para que a mesma semente reproduza as respostas do wwz e das ferramentas, que misturam o id da conversa/sessão no sorteio, em processo e com `--tools-url`; com `--seed 0`, aleatórios por execução ([ADR 0047](decisions/0047-regras-do-wwz-load-agents.md)). Com `--seed N ≠ 0`, leva também `x-wwz-seed: N + g`, em que `g = agente + agents · sessão_local` é o índice global da sessão. Com a mesma semente, a sessão de ferramentas usa `N + g`.
- `SIGINT`/`SIGTERM`: nenhum turno novo começa; as requisições em voo esperam até `--timeout`; as sessões abertas ficam `interrupted`; o relatório parcial sai com `"interrupted": true`. Interrupção durante a espera de uma nova tentativa: a sessão fica `interrupted` e a última tentativa conta como a requisição final (entra em `errors` e `errors_by_type`).

**Novas tentativas.** Classes repetidas: `http_408`, `http_429/*`, `http_5xx/*` (500–599, inclusive 529), `connect`, `reset`, `timeout`, `stream_truncated` e `stream_error/*`. As demais classes (outros 4xx, `parse`) falham na hora. A espera é `max(backoff, retry-after)`, com `retry-after-ms` (milissegundos) e depois `retry-after` (segundos ou data HTTP) lidos da resposta. Uma nova tentativa reenvia o mesmo corpo, inclusive depois de stream parcial (é o que o I15 mede como `resent_after_stream`). Só a tentativa final entra nas medidas de latência, e cada tentativa conta em `requests`. Os headers `retry-after-ms` e `retry-after` ficam em `loadgen.Result` (brutos) só nas respostas com status ≥ 400.

**Relatório JSON** (`version: 1`), com tempos em ms e distribuições `{"count", "mean", "p50", "p90", "p99", "max"}` como no `run`:

```json
{
  "version": 1, "kind": "agents", "tool_version": "v0.1.0",
  "started": "…", "finished": "…", "interrupted": false,
  "config": {"target": "…", "protocol": "openai", "path": "…", "model": "wwz-fast", "stream": true,
             "agents": 10, "profile": "coder", "turns": 5, "max_tool_rounds": 8, "sessions": 1,
             "duration_ms": 0, "retries": 2, "seed": 42, "tools": "in-process", "headers": []},
  "agents": [{"agent": 0, "profile": "coder", "sessions_started": 1, "sessions_completed": 1,
              "sessions_failed": 0, "turns": 5, "requests": 14, "retries": 1, "tool_calls": 9,
              "tool_errors": 1, "errors": 0, "errors_by_type": {}, "ttft_ms": {}, "e2e_ms": {}}],
  "total": {"sessions": {"started": 10, "completed": 9, "failed": 1, "interrupted": 0,
                         "failed_by_reason": {"http_429/rate_limit_error": 1}},
            "turns": 50, "requests": 140, "ok": 139, "errors": 1, "retries": 7,
            "retries_by_type": {"http_429/rate_limit_error": 7}, "errors_by_type": {},
            "retry_wait_ms": {}, "rps": 0, "input_tokens": 0, "output_tokens": 0, "reasoning_tokens": 0,
            "tokens_estimated": false, "ttft_ms": {}, "itl_ms": {}, "e2e_ms": {},
            "turn_e2e_ms": {}, "session_e2e_ms": {}, "tool_calls": 90,
            "tools": {"calls_by_name": {"read_file": 40}, "outcomes": {"success": 85, "natural_error": 3, "error": 2}}},
  "admin_stats": null
}
```

`errors` e `errors_by_type` contam requisições que falharam depois das novas tentativas; `tool_errors` conta os resultados de ferramenta com `is_error` devolvidos ao modelo (`natural_error`, `error`, `timeout`), sem incluir `tools_unavailable`; `retries_by_type` conta as tentativas repetidas por classe. `turn_e2e_ms` mede do envio do turno à resposta final (inclui a execução das ferramentas) e `session_e2e_ms` só considera as sessões concluídas.

**CSV:** cabeçalho fixo, uma linha por agente e a última com `agent=total`:

```
agent,profile,sessions_started,sessions_completed,sessions_failed,turns,requests,retries,tool_calls,tool_errors,errors,ttft_p50_ms,ttft_p99_ms,e2e_p50_ms,e2e_p99_ms,error_types
```

**Códigos de saída:** os do `run` (`0`, `1`, `2`). `--fail-on-error-pct P` dá `3` quando `sessions.failed / sessions.started` passa de P %.

### `wwz-load top`

TUI estilo htop sobre `GET /admin/stats` (schema `Stats` de `cmd/wwz/openapi.yaml`). Núcleo (amostragem, deltas, agregação, quadro em texto/JSON) em `internal/loadgen/top`, testável sem terminal.

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--admin URL` | `http://127.0.0.1:9000` | Control plane; repetível para somar várias instâncias |
| `--token T` | `$WWZ_ADMIN_TOKEN` | Bearer |
| `--interval D` | `1s` | Intervalo entre amostras (≥ 100ms) |
| `--once` | `false` | Faz duas amostras separadas por `--interval`, imprime um quadro e sai |
| `--output text\|json` | `text` | Formato do quadro em `--once` (`json` exige `--once`) |
| `--sort requests\|errors\|ttft\|e2e` | `requests` | Ordem da tabela por modelo (decrescente) |
| `--rows N` | `10` | Linhas por tabela (modelos, chaves, tipos de erro) |
| `--timeout D` | `2s` | Teto de cada `GET` |

**Quadro.**
- Cabeçalho com relógio, instâncias ok/total, `store`, `uptime` e `pod` (com uma instância só).
- Totais: req/s, `in_flight`, `streams_open`, erros injetados/s, rate limited/s, bloqueados/s.
- Erros por `kind` (contagem e taxa) e requisições por status.
- Tabela por modelo: `driver`, `model`, req/s, % de erro (`errors/requests` no intervalo), TTFT p50/p99 e E2E p50/p99.
- Tabela por chave: `key`, `status`, req/s e `spend_usd`.
- As taxas vêm da diferença entre duas amostras ÷ tempo decorrido. No primeiro quadro aparecem como `—` (JSON: `null`). Um contador que diminuiu (`POST /admin/reset` ou restart) dá taxa 0 naquele quadro.
- Com várias `--admin`, os contadores são somados e as linhas por modelo/chave fundidas por `(driver, model)`/`key`; os percentis são o máximo entre as instâncias. Instância que falha aparece como indisponível e o quadro continua com as demais.

**Regras de agregação, ordenação e pausa** ([ADR 0045](decisions/0045-regras-do-wwz-load-top.md)).
- **Tipo de store:** vem do campo `store` (`memory`|`valkey`) do `/admin/stats` de cada instância; o cabeçalho mostra os tipos distintos das instâncias que responderam, ordenados e separados por vírgula.
- **Linhas por chave:** de instância com `store: valkey` (store compartilhado, cada instância já informa os números do cluster), `requests`, `spend_usd` e req/s entram pelo máximo entre essas instâncias, porque somar contaria a mesma chave N vezes; de instância com `store: memory`, somam. Totais e linhas por modelo continuam somados em qualquer store (são contadores locais de cada processo).
- **Taxa por linha:** cada instância contribui com `delta ÷ tempo desde a sua última amostra boa`; o `% de erro` por modelo é `Σ Δerrors ÷ Σ Δrequests × 100` (0 sem requisições no intervalo).
- **`--sort`** (decrescente; empate por `driver` e depois `model`, crescente): `requests` = req/s (no primeiro quadro, sem taxa, o total acumulado de `requests`); `errors` = % de erro (no primeiro quadro, `errors/requests` acumulado); `ttft` = TTFT p99; `e2e` = E2E p99. A tabela por chave ordena por req/s, depois `requests` acumulado (decrescentes), depois `key`.
- **`--rows`:** no JSON limita só `by_model` e `by_key`; `totals.errors_by_kind` e `totals.by_status` saem completos. No texto limita também a lista de tipos de erro (ordenada por contagem decrescente, empate por nome); a linha de status sai completa.
- **Pausa (`p`):** enquanto pausado o `top` não amostra (os ticks são ignorados e o quadro fica congelado; `s` ainda reordena). Ao continuar, a próxima amostra é comparada com a última amostra boa, então a taxa cobre todo o tempo desde ela, inclusive o período pausado.

**Terminal.** Em TTY (sem `--once`): tela alternativa, modo raw (`golang.org/x/term`), redesenho a cada intervalo ajustado à largura. As teclas são `q` ou `Ctrl-C` (sai), `s` (próxima ordenação) e `p` (pausa/continua). O terminal é restaurado na saída, no sinal e em pânico. Com stdout fora de TTY e sem `--once`, imprime um quadro em texto sem ANSI por intervalo, separados por linha em branco, até `SIGINT`/`SIGTERM`.

**JSON** (`--once --output json`):

```json
{"version": 1, "kind": "top", "sampled_at": "…", "interval_ms": 1000,
 "instances": [{"admin": "http://127.0.0.1:9000", "pod": "wwz-1", "ok": true, "error": ""}],
 "totals": {"requests": 0, "rps": 0, "in_flight": 0, "streams_open": 0, "errors_per_s": 0,
            "rate_limited_per_s": 0, "blocked_per_s": 0,
            "errors_by_kind": {"overloaded": {"count": 0, "per_s": 0}}, "by_status": {"200": 0}},
 "by_model": [{"driver": "openai", "model": "wwz-fast", "requests": 0, "rps": 0, "error_pct": 0,
               "ttft_p50_ms": 0, "ttft_p99_ms": 0, "e2e_p50_ms": 0, "e2e_p99_ms": 0}],
 "by_key": [{"key": "wwz-key-01", "status": "active", "requests": 0, "rps": 0, "spend_usd": 0}]}
```

**Códigos de saída:** `0` saída normal (`q`, sinal, fim do `--once`); `1` nenhuma instância respondeu na primeira amostra, ou `401` (mensagem `token recusado por <admin>`); `2` uso inválido.

## Contrato do `wwz-load claude-code` ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md))

Cliente de testes que se comporta como o Claude Code 2.1.308 contra o wwz: as requisições de inferência saem byte a byte iguais às do Claude Code (perfil da spec 020), e o login OAuth segue o fluxo do Claude Code contra as rotas do driver `anthropic` (spec 006, 16.2.1). Núcleo em `internal/claudecode/client`, sobre o perfil `internal/claudecode`; sem dependência nova (ADR 0024).

**Transporte.** HTTP/1.1 escrito à mão sobre `net.Conn` (TLS por `crypto/tls` para `https://`), porque `net/http` canoniza e reordena headers. Cabeça e corpo vêm de `claudecode.Build`; `Content-Length` sempre, nunca chunked; keep-alive com uma conexão por sessão. Resposta: `Content-Length` ou chunked, `gzip`/`deflate` decodificados; `br`/`zstd` (que o wwz nunca manda) dão erro `parse`. SSE pelo parser do `run`. Retentativa até `--retries` (padrão 2), com `X-Stainless-Retry-Count` incrementado e o mesmo corpo, decidida como o `shouldRetry` do Claude Code 2.1.308 (`tmp/claude-code/oauth-shapes.md`, item 4), nesta ordem, valendo a primeira regra que casar: (1) `connect` e `reset` retentam; (2) 429 com `anthropic-ratelimit-unified-overage-disabled-reason` em `org_spend_cap_reached` ou `org_level_disabled_until`, ou com ela em `out_of_credits`, `org_level_disabled` ou `org_service_level_disabled` sem `representative-claim` nem `overage-status`, nunca retenta, em qualquer credencial; (3) **assinante** (credencial OAuth com o escopo `user:inference`, inclusive `--setup-token`, e plano do estado, vindo do `/api/oauth/profile`, diferente de `enterprise`; sem plano conta como assinante): 429 com qualquer dos headers de assinatura (`anthropic-ratelimit-unified-representative-claim`, `-overage-status` ou `-overage-disabled-reason`) nunca retenta, **mesmo com `x-should-retry: true`**; (4) `x-should-retry: true` retenta, exceto para o assinante, em que não força nada e a decisão segue a regra 6; (5) `x-should-retry: false` nunca retenta; (6) 408, 409, 429 e 5xx retentam (o 429 do assinante só sem os headers de assinatura); o resto não. Para API key e plano `enterprise`, 429 com os headers de assinatura retenta como um 429 comum, salvo a regra 2. Contra o wwz, o 429 de cota da assinatura (spec 006, 16.2.1) sai com `x-should-retry: true` e o `ask` do assinante manda uma requisição só. Ficam fora o 401 do Claude Code (que renova o token e retenta mesmo com `x-should-retry: false`; o `wwz-load claude-code` não retenta 401) e a retentativa do `overloaded_error` antes do header.

**Hosts reais recusados.** `--target` cujo host (sem caixa, sem ponto final) é `anthropic.com`, `claude.ai`, `claude.com` ou subdomínio deles → código 2, `wwz-load claude-code: refusing real Anthropic host <host>: this client only talks to the simulator`. Redirect para outro host nunca é seguido. As URLs de authorize e token saem do `--target`, nunca da configuração fixa do Claude Code; o cliente não lê `~/.claude`, o keychain nem `CLAUDE_CODE_OAUTH_TOKEN` (ADR 0074, item 12).

**Ações** (`wwz-load claude-code <ação> [flags]`):

| Ação | O que faz | Saída e código |
| --- | --- | --- |
| `login` | Fluxo OAuth do Claude Code contra o `--target`: PKCE (verifier e state de 32 bytes aleatórios em base64url), `GET /oauth/authorize` com os parâmetros na ordem do cliente, callback local em `127.0.0.1` porta efêmera (`redirect_uri` `http://localhost:<p>/callback`) ou `--manual` (redirect manual e `code#state` lido da resposta), `POST /v1/oauth/token` (JSON na ordem do cliente), `GET /api/oauth/profile` e `GET /api/oauth/claude_cli/roles`. `--setup-token` pede só `user:inference` e `expires_in: 31536000`, como o `claude setup-token` | grava o estado (`--state`); imprime conta, organização, plano e validade; 0 ok, 1 recusa do servidor, 2 uso |
| `refresh` | `POST /v1/oauth/token` com `refresh_token` e o `scope` salvo | atualiza o estado; 0/1/2 |
| `ask <texto>` | Um turno `turn` (spec 020) com o texto como primeira mensagem do usuário; `--resume` continua a conversa do estado (turno do assistente e o bloco `system` curto, como a captura 008) | texto montado em stdout; `--dump <arquivo>` grava os bytes enviados; 0 com 200, 1 com erro HTTP (corpo em stderr) |
| `replay <captura.raw>` | Lê uma captura (fixture), extrai os insumos (versão, entrypoint, credencial, `session_id`, `device_id`, `account_uuid`, modelo, mensagens, system prompt, tipo, `Host`), reconstrói com `Build` e compara byte a byte; `--send` também envia ao `--target` | `identical sha256=<h>` ou a primeira divergência (offset, linha da cabeça ou caminho JSON, esperado × obtido); 0 idêntico, 1 diferente, 2 erro |
| `run` | Carga: cada sessão é um `ask` com sessão nova (`--turns N` turnos por sessão, padrão 1), nos modos `--rps`/`--concurrency`/`--duration`/`--requests` do `run` | relatório do `run` (texto, JSON, CSV desta spec) com `config.protocol: "claude-code"`; a recusa do modo estrito aparece como `http_403/forged_request_error` |

**Flags comuns.**

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--target` | (obrigatória) | URL base do wwz (`ANTHROPIC_BASE_URL`), com o prefixo do mount |
| `--version` | `2.1.308` | Versão do perfil; outra → código 2 |
| `--entrypoint` | `sdk-cli` | `sdk-cli` (`claude -p`) ou `cli` (interativo: cota antes do primeiro turno, `turn_safeguards` e `thinking.display: updates`) |
| `--auth` | `oauth` | `oauth` (token do estado ou `--token`) ou `api-key` (`--key`) |
| `--token`, `--key` | — | Access token ou chave de API explícitos (sem estado) |
| `--state` | `$XDG_STATE_HOME/wwz-load/claude-code.json` (sem a variável, `~/.local/state/wwz-load/claude-code.json`) | Estado: tokens, validade, escopos, conta, `device_id` (64 hex gerados uma vez) e a última conversa; criado com modo 0600 |
| `--model` | `claude-sonnet-5-5` (`oauth`), `claude-opus-5-5` (`api-key`) | Como o Claude Code 2.1.308 sem `--model` |
| `--system-prompt` | `You are a terse test assistant.` | O `--system-prompt` das capturas |
| `--arch`, `--os` | `arm64`, `MacOS` | `X-Stainless-Arch` e `X-Stainless-OS` |
| `--stream` | `true` | `false` usa o fallback não-stream (`max_tokens` 64000, timeout 300) |
| `--retries`, `--timeout` | `2`, `600s` | Retentativas e tempo por requisição |

**Critérios.** Para cada fixture de `testdata/real/claude-code/2.1.308/`, `replay` dá `identical` (mesmo sha256); `ask` contra um wwz com `client_verification: claude-code` dá 200 nos modos `sdk-cli`/`cli` e nas credenciais OAuth/API key; `login` (callback e `--manual`), `refresh` e `--setup-token` completam contra o wwz com `oauth_key`; `--target https://api.anthropic.com` sai com 2 sem abrir conexão.

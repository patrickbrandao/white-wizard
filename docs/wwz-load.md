# wwz-load

Cliente de benchmark do White Wizard. Fala os dialetos OpenAI e Anthropic, com e sem streaming, e mede TTFT, ITL e E2E no cliente. Também joga o papel do agente num cenário, distribui a carga por várias máquinas, conduz agentes multi-turno com ferramentas falsas (`agents`) acompanha o `/admin/stats` ao vivo (`top`) e faz o papel do Claude Code contra a assinatura OAuth e o modo estrito do driver `anthropic` (`claude-code`). A referência normativa é a [spec 015](../specs/015-wwz-load.md), com as ADRs [0024](../specs/decisions/0024-percentis-e-cliente-do-wwz-load.md), [0033](../specs/decisions/0033-sementes-e-parada-do-wwz-load-distribuido.md), [0036](../specs/decisions/0036-agents-e-top-do-wwz-load.md), [0041](../specs/decisions/0041-wwz-load-scenario-header-e-conclusao.md), [0045](../specs/decisions/0045-regras-do-wwz-load-top.md) e [0047](../specs/decisions/0047-regras-do-wwz-load-agents.md) e [0074](../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md). Quando este guia e a spec divergirem, vale a spec.

Os binários saem de `make build` (em `bin/`) ou de `go run ./cmd/wwz-load`. Os exemplos abaixo foram executados contra um `wwz` local em portas próprias:

```sh
WWZ_ADMIN_TOKEN=tok go run ./cmd/wwz serve -scenarios-dir ./scenarios -port 18610 -admin-listen 127.0.0.1:18611
B=http://127.0.0.1:18610      # data plane
A=http://127.0.0.1:18611      # control plane
```

Contra o compose da raiz, use `B=http://127.0.0.1:18000` e `A=http://127.0.0.1:18001`, ou a imagem: `docker compose run --rm wwz-load --rps 50 --duration 10s`.

## Subcomandos

| Subcomando | O que faz |
| --- | --- |
| `run` (padrão) | Gera carga e imprime o relatório. `wwz-load --rps 100` é o mesmo que `wwz-load run --rps 100` |
| `controller` | Coordena N workers (carga distribuída) |
| `worker` | Executa a fatia de carga recebida do controlador |
| `agents` | N agentes concorrentes multi-turno com tool calls executadas |
| `top` | Painel de terminal sobre `/admin/stats` |
| `claude-code` | Cliente que se comporta como o Claude Code 2.1.308 (`login`, `refresh`, `ask`, `replay`, `run`) contra o driver `anthropic` |
| `version`, `help` | Versão (com commit e Go) e ajuda |

As flags usam o pacote `flag` da stdlib: `-x` e `--x` são equivalentes, e `wwz-load <subcomando> -h` lista as flags. Durações seguem o formato Go (`500ms`, `30s`, `5m`).

## `run`: modos de carga

Cada execução usa exatamente um modo. Sem nenhum, vale `--concurrency 1`. Dois modos juntos, flag desconhecida ou valor inválido dão uma mensagem que cita a flag e código 2.

| Modo | Malha | Comportamento |
| --- | --- | --- |
| `--rps N` | aberta | Partidas pelo relógio a cada `1/N` s, cada uma em goroutine própria até `--max-inflight`. A latência do servidor nunca atrasa a próxima partida; o atraso entre o instante agendado e o envio real vai para `schedule_lag_ms` |
| `--concurrency N` | fechada | N workers; cada um envia a próxima requisição assim que a anterior termina |
| `--ramp DE:ATE:DUR` | aberta | A taxa cresce linearmente de DE a ATE ao longo de DUR, com um passo por `--step-duration` (padrão `10s`) |
| `--sweep` | aberta | Um passo por taxa (`--sweep-rates 100,500,1000` ou `--sweep-from/--sweep-to/--sweep-steps` em progressão geométrica), cada um por `--step-duration` (padrão `30s`). Para depois de um passo com mais de `--sweep-stop-error-pct` (padrão 50) % de erros |

Acima de `--max-inflight` (padrão 10000), a partida agendada é descartada e contada em `dropped`: o cliente saturou, não é erro do alvo. `--warmup D` antecede o primeiro passo com a mesma carga, sem medir.

```sh
# Taxa fixa, streaming OpenAI (padrão), relatório em texto
wwz-load --target $B --rps 20 --duration 3s --progress 0
```

```text
wwz-load v0.0.0-…  target http://127.0.0.1:18610/v1/chat/completions  protocol openai  model wwz-fast  stream true
mode rps 20  seed 3208632515669675784  started 2026-10-31T13:14:13Z  finished 2026-10-31T13:14:16Z

total: 60 requests in 3000 ms, 20 req/s (20 ok/s)
  ok 60  errors 0 (0%)  dropped 0
  tokens/s: output 2646  input 360; reasoning tokens 0

            ms  count   mean    p50    p90    p99    max
          ttft     60  1.141  1.107  1.627  2.631  2.634
           itl   7878  0.009      0  0.024  0.127  0.974
           e2e     60  2.493  2.327  4.463  6.031  6.043
  schedule lag     60  0.954  1.051  1.083  1.995  1.999
```

```sh
# Concorrência fixa, sem stream, N requisições e CSV
wwz-load run --target $B --concurrency 4 --requests 20 --duration 0 --stream=false --output csv --progress 0

# Sweep no dialeto Anthropic (/v1/messages, x-api-key + anthropic-version)
wwz-load --target $B --protocol anthropic --sweep --sweep-rates 10,20 --step-duration 2s --progress 0

# Rampa com uma linha por passo no JSON
wwz-load --target $B --ramp 10:50:4s --step-duration 2s --output json --progress 0 \
  | jq -c '{mode:.config.mode, steps:[.steps[]|{step,target_rps,requests}]}'
# {"mode":"ramp","steps":[{"step":1,"target_rps":20,"requests":40},{"step":2,"target_rps":40,"requests":80}]}

# /admin/stats do wwz anexado ao fim (metodologia, item 8)
WWZ_ADMIN_TOKEN=tok wwz-load --target $B --concurrency 2 --duration 2s --admin $A --output json --out tmp/run.json
```

### Flags de `run`

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--target URL` | `http://127.0.0.1:8000` | Base do alvo (wwz ou roteador) |
| `--protocol openai\|anthropic` | `openai` | Dialeto do corpo, da autenticação e do parser SSE |
| `--path P` | `/v1/chat/completions` ou `/v1/messages` | Caminho somado ao `--target`. OpenRouter, OpenCode Go e Azure falam OpenAI: use `--protocol openai --path …` |
| `--model M` | `wwz-fast` | Campo `model` |
| `--key K` | `$WWZ_LOAD_KEY`, senão `wwz-key-01` | OpenAI: `Authorization: Bearer K`; Anthropic: `x-api-key: K` |
| `--stream` | `true` | `--stream=false` para não-stream |
| `--max-tokens N` | `256` | `max_tokens` |
| `--prompt TEXTO` / `--prompt-file ARQ` | prompt fixo / — | Mensagem única ou uma linha sorteada por requisição |
| `--scenario ARQ`, `--max-turns N`, `--tool-result-error-rate P` | — | Modo conversa (abaixo) |
| `--seed N` | `0` (aleatória, registrada) | Com `N ≠ 0`, cada requisição leva `x-wwz-seed: N + i` |
| `--header 'k: v'`, `--test-id ID` | — | Headers extras (repetível); `--test-id` = `x-test-id` (escopo de caos, spec 009) |
| `--duration D` | `30s` | Duração medida; `0` = até `--requests` ou o sinal. Proibida com `--ramp`/`--sweep` |
| `--requests N` | `0` | Teto de requisições (ou conversas) iniciadas |
| `--timeout D` | `60s` | Teto da requisição inteira; estourado vira `timeout` |
| `--warmup D` | `0s` | Período inicial sem medida |
| `--rps`, `--concurrency`, `--ramp`, `--sweep`, `--sweep-rates`, `--sweep-from`, `--sweep-to`, `--sweep-steps`, `--step-duration`, `--sweep-stop-error-pct`, `--max-inflight` | ver "Modos" | Modo de carga |
| `--output text\|json\|csv`, `--out ARQ` | `text`, stdout | Formato e destino do relatório; o progresso vai para o stderr |
| `--admin URL`, `--admin-token T` | —, `$WWZ_ADMIN_TOKEN` | Anexa `GET URL/admin/stats` ao relatório (`admin_stats`) |
| `--fail-on-error-pct P` | `0` (desligado) | Código 3 se `errors / requests` passar de P % |
| `--progress D` | `5s` | Linha de progresso no stderr; `0` desliga |

**Códigos de saída:** `0` concluído (mesmo com erros de requisição); `1` falha de execução (arquivo de `--out`, cenário inválido, controlador inalcançável); `2` uso inválido; `3` `--fail-on-error-pct` estourado; `4` worker faltante no modo distribuído. `SIGINT`/`SIGTERM` param as partidas, esperam as requisições em voo até `--timeout` e emitem o relatório parcial com `"interrupted": true`.

### Saída

- **JSON** (`version: 1`): `config` (só os **nomes** dos headers extras, porque os valores podem ser segredos), `steps` (vazio em `--rps`/`--concurrency`), `total`, `conversations`, `instances` e `admin_stats`. Cada passo e o total trazem `requests`, `ok`, `errors`, `dropped`, `rps`, `ok_rps`, tokens e tokens/s, as distribuições `ttft_ms`, `itl_ms`, `e2e_ms` e `schedule_lag_ms` (`{"count","mean","p50","p90","p99","max"}`) e `errors_by_type`.
- **CSV**: cabeçalho fixo, uma linha por passo e a última `step=total`; `error_types` como `tipo=n;…`.
- **Texto**: para humanos, não é contrato.

O esquema completo está na seção "Saída" da [spec 015](../specs/015-wwz-load.md).

## `--scenario`: conversa por cenário

O cliente joga o papel do agente com um cenário da [spec 011](../specs/011-cenarios.md): declara as ferramentas que o cenário usa, devolve um `tool_result` inventado e determinístico para cada tool call (na mesma ordem e com os mesmos ids) e continua até uma resposta final, um erro ou `--max-turns` (padrão: `step_limit` do YAML, senão 12). Nos modos de carga a unidade passa a ser a conversa: `--rps` é taxa de início de conversas, `--concurrency` é conversas simultâneas e `--requests` conta conversas.

**Header padrão ([ADR 0041](../specs/decisions/0041-wwz-load-scenario-header-e-conclusao.md)).** Toda requisição leva `x-wwz-scenario: <name do YAML>`, o primeiro critério de seleção do wwz, e o cenário vale mesmo sem constar em `models[].scenarios`. `--header 'x-wwz-scenario: outro'` vence; `--header 'x-wwz-scenario:'` (valor vazio) suprime o envio, para testar a seleção por `match`, `metadata`, marcador ou sufixo através do roteador.

```sh
wwz-load --target $B --scenario scenarios/file-agent-flow.yaml --concurrency 5 --requests 20 --duration 0 \
  --seed 42 --progress 0 --output json \
  | jq -c '{headers:.config.headers, conv:.conversations|{started,completed,failed,order_violations,scenario_not_applied}}'
# {"headers":["x-wwz-scenario"],"conv":{"started":20,"completed":20,"failed":0,"order_violations":0,"scenario_not_applied":0}}
```

O bloco `conversations` traz `started` = `completed` + `failed` + `interrupted`, `failed_by_reason`, `turns`, `tool_calls`, `e2e_ms` da conversa inteira e os contadores de conferência:

| Contador | Conta |
| --- | --- |
| `order_violations` | Respostas com tool calls fora do passo esperado (pelo id `call_<cenário>_<passo>_<n>_…` do wwz ou pela previsão do cliente) |
| `id_mismatches` | `tool_call_id`/`tool_use_id` vazio ou repetido dentro da mesma resposta |
| `scenario_not_applied` | Respostas 2xx sem `x-wwz-scenario` ou com outro valor: o wwz não aplicou o cenário ou o roteador removeu o header |
| `reasoning_missing` | O passo pedia raciocínio e não veio texto nem `reasoning_tokens` > 0 (o driver `openai` com `reasoning_field: hidden` só informa os tokens) |

Uma conversa só é **concluída** quando a resposta final cai num passo terminal (sem `tool_calls` declaradas). Se o passo ainda pedia tool calls, ela falha com o motivo `incomplete`. Sem o header, o cenário não roda e o relatório mostra isso:

```sh
wwz-load --target $B --scenario scenarios/file-agent-flow.yaml --header 'x-wwz-scenario:' \
  --concurrency 2 --requests 4 --duration 0 --seed 42 --progress 0
```

```text
conversations: 4 started  0 completed  4 failed (0 max_turns)  0 interrupted
  order violations 6  id mismatches 0  reasoning missing 4  scenario not applied 7
aviso: 7 respostas sem x-wwz-scenario: file-agent-flow (cenário não aplicado ou header removido pelo roteador)
  failed        4  incomplete
```

`scenario_not_applied` não muda o código de saída, porque um roteador pode remover headers de resposta.

## `--instances`: controlador e workers

Para passar do que uma máquina gera, um `controller` divide o plano entre N `worker`s, que podem rodar em outras máquinas. O controlador lê `--prompt-file` e `--scenario` e envia o conteúdo no plano, então os workers não precisam de nenhum arquivo. O transporte é HTTP/JSON interno entre binários da mesma versão.

```sh
export WWZ_LOAD_TOKEN=segredo          # opcional; com ele, chamada sem bearer recebe 401
wwz-load controller --listen 127.0.0.1:18620 --instances 2 --register-timeout 30s \
  --target $B --rps 20 --duration 3s --seed 7 --output json --out tmp/dist.json &
wwz-load worker --controller http://127.0.0.1:18620 --id w0 &
wwz-load worker --controller http://127.0.0.1:18620 --id w1
jq -c '{req:.total.requests, inst:.instances|{expected,reported,missing}}' tmp/dist.json
# {"req":60,"inst":{"expected":2,"reported":2,"missing":[]}}
```

| Lado | Flags próprias |
| --- | --- |
| `controller` | `--listen` (padrão `:9100`), `--instances N` (padrão 1), `--register-timeout` (padrão `5m`), `--token` (padrão `$WWZ_LOAD_TOKEN`) e todas as flags de `run` |
| `worker` | `--controller URL` (obrigatória), `--id` (padrão `<hostname>-<pid>`), `--token` |

- **Divisão:** `rps`, extremos do `ramp` e taxas do `sweep` divididos igualmente; `concurrency` e `requests` inteiros, com o resto nos menores índices (menores que `--instances` dão código 2).
- **Início:** todos começam em `start_at` = último registro + 2 s. Os relógios precisam de NTP; o atraso de cada worker sai em `late_start_ms`.
- **Sementes ([ADR 0033](../specs/decisions/0033-sementes-e-parada-do-wwz-load-distribuido.md)):** com `--seed N ≠ 0`, o worker `k` envia `x-wwz-seed: N + k + instances · i`, a mesma união de sementes de uma máquina só.
- **Agregação:** contadores somados e histogramas mesclados passo a passo; os percentis agregados são os de um histograma único com todas as amostras.
- **Parada e falhas:** `SIGINT` no controlador depois do registro faz os workers pararem (eles consultam `GET /v1/status` a cada 1 s) e o relatório sai com `"interrupted": true`. Worker sem resultado no prazo entra em `instances.missing` e o controlador sai com código 4.

## `agents`: agentes multi-turno com ferramentas

N agentes concorrentes conduzem sessões de `--turns` turnos de usuário. Cada tool call recebida é executada pelo catálogo do [wwz-tools](wwz-tools.md) (em processo, por padrão, ou num `wwz-tools serve` com `--tools-url`) e o resultado volta como `tool_result`. Cada turno repete requisições enquanto vierem tool calls, até `--max-tool-rounds`.

```sh
wwz-load agents --target $B --agents 4 --profile mixed --turns 3 --tool-prob 40 --seed 42 --progress 0 --output csv
```

```text
agent,profile,sessions_started,sessions_completed,sessions_failed,turns,requests,retries,tool_calls,tool_errors,errors,ttft_p50_ms,ttft_p99_ms,e2e_p50_ms,e2e_p99_ms,error_types
0,coder,1,1,0,3,6,0,8,4,0,0.825,2.055,1.115,5.507,
1,pesquisa,1,1,0,3,5,0,2,1,0,0.933,1.634,1.339,6.435,
2,chat,1,1,0,3,3,0,0,0,0,0.645,0.909,0.977,1.299,
3,coder,1,1,0,3,4,0,4,2,0,0.929,1.155,1.115,2.471,
total,mixed,4,4,0,12,18,0,14,7,0,0.909,2.055,1.115,6.435,
```

Sem cenário, o wwz só gera tool calls com probabilidade: `--tool-prob P` envia `x-wwz-tool-prob: P` (sem efeito atrás de um roteador que remova o header). Com `--tool-prob 100` toda resposta pede ferramentas e as sessões acabam em `max_tool_rounds`.

**Perfis.** `coder` (`read_file`, `write_file`, `edit_file`, `list_dir`, `search_code`, `run_command`), `pesquisa` (`web_search`, `fetch_url`, `read_file`), `chat` (sem ferramentas, só histórico crescente) e `mixed` (rodízio `coder`, `pesquisa`, `chat` pelo índice do agente). Cada perfil tem mensagem de sistema, primeira mensagem e continuações fixas; as ferramentas vão no dialeto do `--protocol`, sem `tool_choice`.

**Ferramentas fora do processo.** Com `--tools-url`, as chamadas vão para `POST /v1/call` com o bearer de `$WWZ_TOOLS_TOKEN`. 504 vira resultado `timeout` e 404 vira `natural_error`; outro status, corpo inválido ou conexão recusada encerram a sessão com o motivo `tools_unavailable`, sem nova tentativa. `--tools-config tools.yaml` aplica latências e taxas de erro ao executor em processo (exclusiva com `--tools-url`).

```sh
wwz-tools serve --listen 127.0.0.1:18612 &
wwz-load agents --target $B --agents 3 --profile coder --turns 2 --tool-prob 100 --seed 42 --progress 0 \
  --output json --tools-url http://127.0.0.1:18612 | jq -c '.total.tools'
# o mesmo resultado em processo e com --tools-url, para a mesma semente:
# {"calls_by_name":{"edit_file":2,"list_dir":9,"read_file":9,"run_command":7,"search_code":6,"write_file":8},"outcomes":{"natural_error":19,"success":22}}
```

**Semente e ids ([ADR 0047](../specs/decisions/0047-regras-do-wwz-load-agents.md)).** Toda requisição da sessão leva `x-wwz-conversation-id: <run>-a<agente>-s<sessão>`. Com `--seed N ≠ 0`, `<run>` deriva de N e cada sessão leva `x-wwz-seed: N + g` (g = índice global da sessão), então a mesma semente reproduz as respostas do wwz e das ferramentas. Com `--seed 0`, `<run>` é aleatório.

**Novas tentativas.** `--retries R` (padrão 2) repete as classes transitórias: `http_408`, `http_429/*`, `http_5xx/*`, `connect`, `reset`, `timeout`, `stream_truncated` e `stream_error/*`. A espera é `max(backoff, retry-after)`: o backoff é `rand(0, min(--backoff-max, --backoff · 2^tentativa))` e o `retry-after-ms`/`retry-after` da resposta é respeitado até `--retry-after-max` (acima dele, desiste). Cada tentativa conta em `requests`; só a final entra nas latências. `--retries 0` mede as falhas sem mascarar.

```sh
wwz-load agents --target $B --agents 1 --profile chat --turns 1 --retries 1 \
  --header 'x-wwz-return-error: rate_limit' --seed 3 --progress 0 --output json \
  | jq -c '.total|{requests, retries, retries_by_type, errors_by_type, retry_wait_ms:.retry_wait_ms.max}'
# {"requests":2,"retries":1,"retries_by_type":{"http_429/requests":1},"errors_by_type":{"http_429/requests":1},"retry_wait_ms":20000}
```

O wwz devolveu `retry-after: 20`, e o cliente esperou 20 s antes da segunda tentativa.

### Flags de `agents`

Herdadas do `run`, com o mesmo significado: `--target`, `--protocol`, `--path`, `--model`, `--key`, `--stream`, `--max-tokens`, `--header`, `--test-id`, `--timeout`, `--seed`, `--output`, `--out`, `--admin`, `--admin-token` e `--progress`. Recusadas com código 2: `--rps`, `--concurrency`, `--ramp`, `--sweep`, `--prompt`, `--prompt-file`, `--scenario`, `--warmup`, `--requests` e `--instances`.

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--agents N` | `10` | Agentes concorrentes |
| `--profile coder\|pesquisa\|chat\|mixed` | `coder` | Perfil |
| `--turns M` | `5` | Turnos de usuário por sessão |
| `--max-tool-rounds K` | `8` | Requisições por turno enquanto vierem tool calls |
| `--sessions S` | `1` | Sessões por agente com `--duration 0`; explícita com `--duration > 0` dá código 2 |
| `--duration D` | `0` | `D > 0`: abre sessões novas até D (a sessão em curso termina) |
| `--think-time D`, `--spawn-interval D` | `0s` | Pausa entre turnos; intervalo entre as partidas dos agentes |
| `--tool-prob P` | — | Envia `x-wwz-tool-prob: P` |
| `--tools-url URL`, `--tools-config ARQ` | em processo | Executor remoto ou configuração do executor em processo |
| `--retries R`, `--backoff D`, `--backoff-max D`, `--retry-after-max D` | `2`, `500ms`, `30s`, `60s` | Novas tentativas |
| `--fail-on-error-pct P` | `0` | Código 3 quando `sessions.failed / sessions.started` passa de P % |

O relatório JSON (`kind: "agents"`) traz uma linha por agente e o `total` com `sessions` (`started`, `completed`, `failed`, `interrupted`, `failed_by_reason`), `retries_by_type`, `retry_wait_ms`, `turn_e2e_ms` (do envio do turno à resposta final, com as ferramentas), `session_e2e_ms` (só as concluídas) e `tools` (`calls_by_name`, `outcomes`). `tool_errors` conta os resultados com `is_error` devolvidos ao modelo.

## `top`: painel ao vivo

Painel estilo htop sobre `GET /admin/stats` de uma ou mais instâncias. Em TTY: tela alternativa, `q`/`Ctrl-C` sai, `s` troca a ordenação e `p` pausa. Fora de TTY, imprime um quadro em texto por intervalo. Com `--once`, faz duas amostras separadas por `--interval`, imprime um quadro e sai.

```sh
WWZ_ADMIN_TOKEN=tok wwz-load top --admin $A --once --interval 1s
```

```text
wwz-load top  13:16:27  instances 1/1  store memory  uptime 2m20s  pod wwz-local
req/s 49.9  in_flight 0  streams_open 0  errors/s 0.0  rate_limited/s 0.0  blocked/s 0.0  requests 6842
status 200=6839 429=3

ERROR KIND  COUNT  /S

DRIVER     MODEL     REQ/S  ERR%  TTFT P50  TTFT P99  E2E P50  E2E P99  (sort: requests)
openai     wwz-fast  49.9   0.0   0.5       1.0       0.6      3.9
anthropic  wwz-fast  0.0    0.0   0.0       0.0       0.7      11.6

KEY      STATUS  REQ/S  REQUESTS  SPEND USD
test-01  active  49.9   6839      1.4172
```

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--admin URL` | `http://127.0.0.1:9000` | Control plane; repetível para somar instâncias |
| `--token T` | `$WWZ_ADMIN_TOKEN` | Bearer |
| `--interval D` | `1s` | Intervalo entre amostras (≥ 100ms) |
| `--once` | `false` | Um quadro e sai |
| `--output text\|json` | `text` | `json` exige `--once` |
| `--sort requests\|errors\|ttft\|e2e` | `requests` | Ordem da tabela por modelo |
| `--rows N` | `10` | Linhas por tabela |
| `--timeout D` | `2s` | Teto de cada `GET` |

- As taxas vêm da diferença entre duas amostras; no primeiro quadro aparecem como `—` (JSON `null`). Um contador que diminuiu (`POST /admin/reset` ou restart) dá taxa 0 naquele quadro.
- Com várias `--admin`, totais e linhas por modelo somam e os percentis são o máximo entre as instâncias. As linhas por chave de instâncias com `store: valkey` entram pelo máximo, porque cada uma já informa os números do cluster; com `store: memory`, somam ([ADR 0045](../specs/decisions/0045-regras-do-wwz-load-top.md)). Instância que falha aparece como indisponível e o quadro continua.
- **Códigos de saída:** `0` normal; `1` nenhuma instância respondeu na primeira amostra ou token recusado (`wwz-load top: token recusado por <admin>`); `2` uso inválido.

```sh
WWZ_ADMIN_TOKEN=tok wwz-load top --admin $A --once --interval 500ms --output json --rows 2 | jq -c '{kind, totals:{rps:.totals.rps}}'
# {"kind":"top","totals":{"rps":49.71}}
```

## `claude-code`: cliente que se comporta como o Claude Code

Contrato em "Contrato do `wwz-load claude-code`" da spec 015 ([ADR 0074](../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md)); o lado do servidor (assinatura OAuth e `client_verification: claude-code`) em [claude-code.md](claude-code.md). As requisições de inferência saem byte a byte iguais às do Claude Code 2.1.308 (perfil `internal/claudecode`, spec 020): HTTP/1.1 escrito à mão sobre a conexão (o `net/http` canoniza e reordena headers), `Content-Length` sempre, uma conexão keep-alive por sessão. Por isso o cliente passa no modo estrito, onde um SDK comum recebe 403 `forged_request_error`.

```sh
cat >/tmp/cc.yaml <<'EOF'
server: {auth: keys}
keys: [{key: wwz-segredo, label: assinante}]
drivers:
  - {name: anthropic, prefix: /claude-code, options: {oauth_key: assinante, client_verification: claude-code}}
models: [{id: claude-sonnet-5-5}, {id: claude-opus-5-5}]
EOF
WWZ_ADMIN_TOKEN=tok go run ./cmd/wwz serve -config /tmp/cc.yaml -port 18610 -admin-listen 127.0.0.1:18611
C=http://127.0.0.1:18610/claude-code   # mount com oauth_key e client_verification: claude-code

wwz-load claude-code login --target $C --state /tmp/cc.json
# account       assinante@wwz.invalid (f186be7f-…)
# organization  acct-assinante (7afa17c0-…)
# plan          max (default_claude_max_5x)
# expires       2026-11-04T04:29:05Z
# state         /tmp/cc.json
wwz-load claude-code ask "say ok" --target $C --state /tmp/cc.json
wwz-load claude-code ask "and the second turn" --resume --target $C --state /tmp/cc.json
wwz-load claude-code ask "say ok" --entrypoint cli --target $C --state /tmp/cc.json
wwz-load claude-code replay testdata/real/claude-code/2.1.308/013-sdk-cli-oauth-count-tokens.raw
# identical sha256=750deb15c0ed09b491c7b235743c5cd5ef33558d5a67b9f91f91b008742e633c
wwz-load claude-code replay testdata/real/claude-code/2.1.308/002-sdk-cli-oauth-turn.raw --send --target $C --token "$(jq -r .access_token /tmp/cc.json)"
# identical sha256=5644b70f…
# sent: HTTP 200
wwz-load claude-code run --target $C --state /tmp/cc.json --concurrency 4 --duration 3s --turns 2 --output json | jq -c '{p: .config.protocol, err: .total.errors}'
# {"p":"claude-code","err":0}
wwz-load claude-code ask "say ok" --auth api-key --key wwz-segredo --target $C --state /tmp/cc.json   # com a chave de API
wwz-load claude-code ask oi --target https://claude.ai
# wwz-load claude-code: refusing real Anthropic host claude.ai: this client only talks to the simulator   (código 2)
```

| Ação | O que faz | Saída e código |
| --- | --- | --- |
| `login` | Fluxo OAuth do Claude Code contra o `--target`: PKCE, `GET /oauth/authorize`, callback local em `127.0.0.1` (ou `--manual`, que lê `code#state` da página), `POST /v1/oauth/token`, `/api/oauth/profile` e `/api/oauth/claude_cli/roles`. `--setup-token` pede só `user:inference` e um token de um ano, como o `claude setup-token` (sem perfil: `plan -`) | Grava o estado; 0 ok, 1 recusa do servidor, 2 uso |
| `refresh` | `POST /v1/oauth/token` com o refresh token e os escopos do estado | Atualiza o estado; 0/1/2 |
| `ask <texto>` | Um turno (`turn`); `--resume` continua a conversa do estado, `--dump F` grava os bytes enviados. Token vencido é renovado antes | Texto em stdout; 0 com 200, 1 com erro HTTP (corpo em stderr) |
| `replay <captura.raw>` | Extrai os insumos de uma captura, reconstrói com o perfil e compara byte a byte; `--send` também envia (com `--token`/`--key`, troca a credencial) | `identical sha256=<h>` ou a primeira divergência; 0 idêntico, 1 diferente, 2 erro |
| `run` | Carga: cada sessão é um `ask` com sessão nova (`--turns N`), com `--concurrency`/`--rps`, `--duration`, `--requests`, `--prompt` | Relatório do `run` (texto, JSON, CSV) com `config.protocol: "claude-code"`; a recusa do modo estrito conta como `http_403/forged_request_error` |

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--target` | (obrigatória, salvo `replay` sem `--send`) | URL base do wwz, com o prefixo do mount (o `ANTHROPIC_BASE_URL` do Claude Code) |
| `--version` | `2.1.308` | Versão do perfil; outra sai com 2 |
| `--entrypoint` | `sdk-cli` | `sdk-cli` (`claude -p`) ou `cli` (interativo: sonda de cota antes do primeiro turno, `turn_safeguards`, `thinking.display: updates`) |
| `--auth` | `oauth` | `oauth` (token do estado ou `--token`) ou `api-key` (`--key`) |
| `--token`, `--key` | — | Access token ou chave de API explícitos, sem estado |
| `--state` | `$XDG_STATE_HOME/wwz-load/claude-code.json` (sem a variável, `~/.local/state/…`) | Tokens, validade, escopos, conta, plano, `device_id` e a última conversa; modo 0600 |
| `--model` | `claude-sonnet-5-5` (`oauth`), `claude-opus-5-5` (`api-key`) | Como o Claude Code 2.1.308 sem `--model` |
| `--system-prompt` | `You are a terse test assistant.` | O `--system-prompt` das capturas |
| `--arch`, `--os` | `arm64`, `MacOS` | `X-Stainless-Arch` e `X-Stainless-OS` |
| `--stream` | `true` | `false` usa o fallback não-stream (`max_tokens` 64000, timeout 300) |
| `--retries`, `--timeout` | `2`, `600s` | Retentativas (com `X-Stainless-Retry-Count` incrementado) e teto por requisição |

- **Retentativa como o Claude Code** (`shouldRetry` do 2.1.308): erros de conexão retentam; o 429 com `overage-disabled-reason` de corte nunca; para o **assinante** (OAuth com `user:inference`, plano do estado diferente de `enterprise`; `--token` explícito conta como assinante), o 429 com header de assinatura (`representative-claim`, `overage-status` ou `overage-disabled-reason`) **nunca retenta, mesmo com `x-should-retry: true`**, e esse header não força retentativa nenhuma; `x-should-retry: false` nunca retenta; 408, 409, 429 e 5xx retentam. Contra o wwz com a cota da assinatura esgotada, um `ask` manda uma requisição só. O 401 não é retentado.
- **Hosts reais recusados**: `--target` em `anthropic.com`, `claude.ai`, `claude.com` ou subdomínio sai com 2 sem abrir conexão; redirect para outro host nunca é seguido; o cliente não lê `~/.claude`, o keychain nem `CLAUDE_CODE_OAUTH_TOKEN`. É um cliente do simulador, não um meio de usar uma assinatura real fora do Claude Code.

## Metodologia

### O que é medido no cliente

`t0` é o instante logo antes de entregar a requisição ao cliente HTTP (fasthttp, conexões keep-alive reutilizadas). Numa conexão nova o estabelecimento entra no tempo, por isso vale usar `--warmup`.

| Medida | Definição |
| --- | --- |
| TTFT | `t0` até o primeiro evento SSE com conteúdo visível, raciocínio ou tool call (OpenAI `delta.content`, `delta.reasoning_content`/`reasoning`, `delta.tool_calls`; Anthropic `content_block_delta` ou `content_block_start` de `tool_use`). Keep-alive, `ping`, `message_start` e chunk só com `role` não contam. Sem stream, TTFT = E2E |
| ITL | Cada intervalo entre dois eventos com conteúdo consecutivos é uma amostra (k eventos dão k−1 amostras). Sem stream, nenhuma |
| E2E | `t0` até o último byte do corpo |
| Tokens | Do `usage` da resposta; sem `usage`, a saída é o número de eventos com conteúdo e o relatório marca `tokens_estimated: true` |
| req/s, tokens/s | Requisições concluídas ÷ duração do passo; tokens de saída das requisições com sucesso ÷ duração |
| `schedule_lag_ms` | Atraso entre a partida agendada e o envio real (malha aberta); valores altos indicam cliente saturado |

Só os sucessos (2xx com corpo completo: `[DONE]` na OpenAI, `message_stop` na Anthropic, JSON válido sem stream) entram nas latências. Os erros são classificados: `http_<status>/<error.type>` (ex.: `http_429/rate_limit_error`, `http_529/overloaded_error`), `http_<status>`, `stream_error/<type>`, `stream_truncated`, `reset`, `connect`, `timeout` e `parse`.

### Histogramas e percentis

O `wwz-load` grava cada amostra num histograma log-linear mesclável, com erro relativo de no máximo 1 % ([ADR 0024](../specs/decisions/0024-percentis-e-cliente-do-wwz-load.md)), e o `max` é exato. É por isso que os workers do modo distribuído mandam histogramas, não percentis: o controlador mescla e calcula os percentis do conjunto inteiro.

Os percentis do `/admin/stats` (o bloco `admin_stats` do relatório e o `top`) são outra coisa: o wwz os estima a partir dos baldes dos histogramas Prometheus `wwz_ttft_seconds`, `wwz_itl_seconds` e `wwz_e2e_seconds`, por interpolação dentro do balde (como o `histogram_quantile`) e limitados por um teto derivado de `_sum`. O erro chega a um balde inteiro (até ~67 % de largura relativa abaixo de 10 s, [ADR 0050](../specs/decisions/0050-baldes-de-latencia-e-percentis-do-admin-stats.md)), e latências abaixo de 1 ms podem aparecer como 0. Para comparar versões de um roteador, use os percentis do `wwz-load`; o `/admin/stats` serve para ver o lado do servidor e os contadores.

### Roteiro de teste de um roteador

1. **Teto do simulador sozinho:** carga direto no `wwz` com latência zero, para garantir que ele nunca é o gargalo (meta ≥ 10 000 req/s por instância, spec 014).
2. **Teto do roteador:** o mesmo teste passando pelo roteador; a diferença é o overhead dele.
3. **Streaming em escala:** `--concurrency 20000` com ITL de 20 ms; observe conexões abertas e memória nos dois lados.
4. **Failover:** dois modelos com chaves diferentes, um com `errors.rate: 30` e `rate_limit`, outro saudável; meça quanto e com que atraso o roteador redireciona.
5. **Cota e backpressure:** chave com `rpm: 300` e `on_limit: queue`; confira se o roteador respeita `retry-after` e os headers `x-ratelimit-*`/`anthropic-ratelimit-*`.
6. **Agentes:** `--scenario scenarios/file-agent-flow.yaml` com 1 000 conversas simultâneas, conferindo `order_violations`, `id_mismatches` e `reasoning_missing`; ou `agents` para sessões com ferramentas executadas.
7. **Caos:** `stream_truncated`, `stream_hang` e `connection_reset` a 5 % (com `--test-id` para isolar o escopo); meça timeouts e vazamento de conexões.
8. **Reprodução:** `--seed` fixo e `--admin` para salvar o `/admin/stats` no relatório, e assim comparar execuções entre versões.

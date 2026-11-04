# wwz-tools

Executor de ferramentas falsas para agentes: fecha o ciclo tool call → execução → resultado sem executar nada de verdade. O sistema de arquivos é virtual e fica em memória, os comandos são simulados e não há acesso à rede. O núcleo (`internal/tools`) roda em processo dentro do [`wwz-load agents`](wwz-load.md#agents-agentes-multi-turno-com-ferramentas) e é servido por HTTP pelo binário `wwz-tools`. A referência normativa é a seção "Contrato do `wwz-tools`" da [spec 017](../specs/017-recursos-importados.md), com a [ADR 0034](../specs/decisions/0034-contrato-do-wwz-tools.md); o contrato HTTP é [cmd/wwz-tools/openapi.yaml](../cmd/wwz-tools/openapi.yaml).

O `wwz-tools` não tem imagem própria: vai na imagem `white-wizard-load` (`docker run --rm white-wizard-load:dev wwz-tools version`), e o compose da raiz o sobe como serviço `wwz-tools` em `http://127.0.0.1:18003`.

## CLI

| Subcomando | O que faz |
| --- | --- |
| `serve [--listen :9200] [--config ARQ] [--seed N]` | Sobe o servidor HTTP; `SIGINT`/`SIGTERM` encerram com graça (5 s) |
| `list [--profile coder\|pesquisa\|chat\|all] [--format raw\|openai\|anthropic]` | Imprime o catálogo em JSON (padrão `all`, `raw`) |
| `call --name N [--args JSON] [--profile P] [--seed N] [--outcome O] [--config ARQ]` | Executa uma chamada em processo numa sessão nova e imprime o resultado |
| `version`, `help` | Versão (com commit e Go) e ajuda |

**Códigos de saída:** `0` sucesso (inclusive `call` com `is_error: true`); `1` falha de execução (porta ocupada, configuração inválida); `2` uso inválido (inclusive ferramenta desconhecida em `call`).

```sh
wwz-tools list --profile pesquisa --format openai | jq -c '.[].function.name'
# "fetch_url"  "read_file"  "web_search"

wwz-tools call --name read_file --args '{"path":"../etc/passwd"}' --seed 7
```

```json
{
  "tool_call_id": "",
  "content": "EACCES: path outside sandbox, open '../etc/passwd'",
  "is_error": true,
  "outcome": "natural_error",
  "latency_ms": 10
}
```

```sh
wwz-tools call --name list_dir --profile coder --seed 7 | jq -r .content
# Makefile  README.md  cmd/  docs/  go.mod  internal/   (uma entrada por linha)

wwz-tools call --name run_command --args '{"command":"go test ./..."}' --seed 7 --outcome invalid_payload
# outcome "invalid_payload": JSON truncado no content, com is_error false
```

## Catálogo

Perfis: `coder`, `pesquisa`, `chat` (sem ferramentas) e `all` (a união). Todos os schemas são `type: object` com `additionalProperties: false`.

| Ferramenta | Perfis | Argumentos (obrigatórios em **negrito**) | Resultado de sucesso |
| --- | --- | --- | --- |
| `read_file` | coder, pesquisa | **`path`**, `offset` ≥ 0, `limit` 1–2000 (linhas) | Texto do arquivo |
| `write_file` | coder | **`path`**, **`content`** | `ok: <n> bytes escritos em <path>` |
| `edit_file` | coder | **`path`**, **`old`**, **`new`**, `replace_all` | `ok: <k> substituições em <path>` |
| `list_dir` | coder | `path` (padrão `.`), `recursive` | Uma entrada por linha, diretórios com `/`, ordem lexicográfica |
| `search_code` | coder | **`pattern`** (substring), `path` | `arquivo:linha: trecho`, até 50 linhas |
| `run_command` | coder | **`command`**, `timeout_ms` | JSON `{"exit_code","stdout","stderr"}`: `go test`/`npm test`/`pytest` dão saída de teste plausível; `ls`, `cat`, `grep`, `pwd` respondem pelo sistema virtual; outro comando dá `exit_code: 127` |
| `web_search` | pesquisa | **`query`**, `max_results` 1–10 (padrão 5) | JSON `{"results":[{"title","url","snippet"}]}` com URLs em `https://example.invalid/…` |
| `fetch_url` | pesquisa | **`url`** (http/https), `max_bytes` | Texto de 200–4000 bytes |

**Sandbox.** Cada `session` é um sistema de arquivos virtual criado na primeira chamada (ou por `POST /v1/sessions`) e semeado por `(seed, profile)` com uma árvore de projeto de 12–30 arquivos. Caminhos que saem da raiz recebem o erro natural `path outside sandbox`. Tetos: arquivo ≤ 64 KiB, sessão ≤ 4 MiB e ≤ 1 000 arquivos (`no space left on device`), `max_sessions` com despejo LRU e `session_ttl` de inatividade.

**Determinismo.** A mesma semente, a mesma sessão e a mesma sequência de chamadas dão conteúdo, latência e resultado injetado idênticos. A semente vem do header `x-wwz-seed`, do campo `seed` da sessão ou de `--seed`; `0` é aleatória.

## Resultados

| `outcome` | Efeito |
| --- | --- |
| `success` | Resultado do catálogo, `is_error: false` |
| `natural_error` | O erro que a ferramenta real daria (argumento fora do schema, arquivo inexistente, `old` não encontrado, caminho fora da sandbox), em estilo POSIX, `is_error: true` |
| `error` (injetado) | `is_error: true`: `permission denied`, `EIO: i/o error`, `resource temporarily unavailable` ou `internal tool error` |
| `timeout` (injetado) | Espera `timeout` (padrão `30s`). Em processo: `is_error: true`, `tool timed out after 30s`; por HTTP: `504` |
| `invalid_payload` (injetado) | `is_error: false`, mas o `content` é inválido: JSON truncado, bytes não UTF-8 ou texto vazio |

O sorteio é por chamada, na ordem `timeout`, `error`, `invalid_payload` e, por fim, a execução normal. O header `x-wwz-tools-outcome: success|error|timeout|invalid_payload` força o resultado de uma chamada HTTP, e `--outcome` faz o mesmo no `call`.

## Configuração

`--config tools.yaml`; precedência flag > ambiente > arquivo > padrão. Campo desconhecido ou valor fora da faixa dá erro com o caminho do campo e código 1.

```yaml
listen: ":9200"
seed: 0
timeout: 30s
max_sessions: 100000
session_ttl: 30m
latency: {min: 5ms, max: 50ms}        # uniforme; por ferramenta em tools.<nome>.latency
outcomes: {error: 0, timeout: 0, invalid_payload: 0}   # percentuais 0–100
tools:
  run_command: {fail_rate: 10, latency: {min: 200ms, max: 2s}}
  web_search: {latency: {min: 100ms, max: 800ms}}
  fetch_url: {latency: {min: 50ms, max: 500ms}, outcomes: {error: 2}}
```

Por ferramenta valem `latency`, `outcomes` (substitui as taxas globais campo a campo) e `fail_rate` (só em `run_command`: probabilidade de o teste falhar). Variáveis de ambiente (a flag de `serve` vence a variável, que vence o arquivo):

| Variável | Equivale a | Efeito |
| --- | --- | --- |
| `WWZ_TOOLS_LISTEN` | `listen`, `--listen` | Endereço HTTP (padrão `:9200`) |
| `WWZ_TOOLS_SEED` | `seed`, `--seed` | Semente inteira; `0` = aleatória; valor não inteiro recusa o boot |
| `WWZ_TOOLS_TOKEN` | — | Bearer exigido nas rotas (exceto `/healthz`); vazio = sem autenticação |

## HTTP (`wwz-tools serve`)

`net/http` com JSON e corpo de até 1 MiB. Com `WWZ_TOOLS_TOKEN`, toda rota exceto `/healthz` exige `Authorization: Bearer <token>` e, sem ele, responde 401. Erros vêm no envelope `{"error": {"type", "message"}}`.

| Rota | Corpo | Resposta |
| --- | --- | --- |
| `GET /v1/tools?profile=&format=` | — | Catálogo (padrão `profile=all`, `format=raw`); valor inválido → 400 |
| `POST /v1/sessions` | `{"profile", "seed"}` | `201 {"session", "profile", "seed", "files"}` |
| `DELETE /v1/sessions/{id}` | — | `204`; inexistente → 404 |
| `POST /v1/call` | `{"session", "name", "arguments", "tool_call_id"}` (`arguments` como objeto ou string JSON, do jeito que chega da OpenAI) | `200 {"tool_call_id", "content", "is_error", "outcome", "latency_ms"}`; ferramenta desconhecida → 404; corpo inválido → 400; `timeout` → 504 |
| `GET /healthz` | — | `200 ok` |
| `GET /metrics` | — | `wwz_tools_calls_total{tool,outcome}`, `wwz_tools_call_duration_seconds{tool}`, `wwz_tools_sessions` |

Uma sessão desconhecida em `POST /v1/call` é criada com o perfil `all`.

```sh
wwz-tools serve --listen 127.0.0.1:18612 --seed 7 &
U=http://127.0.0.1:18612

curl -s "$U/v1/tools?profile=coder&format=anthropic" | jq -c '.[0]|{name, required:.input_schema.required}'
# {"name":"edit_file","required":["path","old","new"]}

curl -s -X POST $U/v1/call -d '{"session":"s1","name":"write_file","arguments":"{\"path\":\"a.txt\",\"content\":\"oi\"}","tool_call_id":"call_1"}'
# {"tool_call_id":"call_1","content":"ok: 2 bytes escritos em a.txt","is_error":false,"outcome":"success","latency_ms":5}

curl -s -X POST $U/v1/call -d '{"session":"s1","name":"read_file","arguments":{"path":"a.txt"},"tool_call_id":"call_2"}'
# {"tool_call_id":"call_2","content":"oi","is_error":false,"outcome":"success","latency_ms":11}

curl -s -X POST $U/v1/call -H 'x-wwz-tools-outcome: error' -d '{"session":"s1","name":"list_dir","arguments":{}}'
# {"tool_call_id":"","content":"permission denied","is_error":true,"outcome":"error","latency_ms":37}

curl -s -o /dev/null -w '%{http_code}\n' -X DELETE $U/v1/sessions/s1    # 204
```

Com token e `timeout` forçado:

```sh
WWZ_TOOLS_TOKEN=segredo wwz-tools serve --config tools.yaml &      # tools.yaml com listen 127.0.0.1:18613 e timeout: 2s
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18613/v1/tools      # 401
curl -si -H 'Authorization: Bearer segredo' -H 'x-wwz-tools-outcome: timeout' \
  -X POST http://127.0.0.1:18613/v1/call -d '{"session":"x","name":"list_dir","arguments":{}}'
# HTTP/1.1 504 Gateway Timeout
# {"error":{"type":"timeout","message":"tool timed out after 2s"}}
```

## Uso com `wwz-load agents`

Por padrão, o `wwz-load agents` executa as ferramentas em processo. `--tools-config tools.yaml` aplica as mesmas latências e taxas ao executor em processo, e `--tools-url` manda as chamadas para um `wwz-tools serve`, usando `$WWZ_TOOLS_TOKEN` como bearer. Para a mesma `--seed`, os dois modos dão o mesmo relatório de ferramentas, porque a sessão de ferramentas de cada sessão do agente (`<run>-a<agente>-s<sessão>`) usa a semente `N + g`.

```sh
WWZ_TOOLS_TOKEN=segredo wwz-load agents --target http://127.0.0.1:18610 --agents 2 --profile pesquisa \
  --turns 2 --tool-prob 60 --seed 42 --tools-url http://127.0.0.1:18613 --progress 0 --output json \
  | jq -c '{s:.total.sessions|{started,completed}, tools:.total.tools}'
# {"s":{"started":2,"completed":2},"tools":{"calls_by_name":{"read_file":4,"web_search":3},"outcomes":{"natural_error":4,"success":3}}}
```

Sem o token, o servidor responde 401 e as sessões falham com `tools_unavailable`. Veja as regras completas em [wwz-load.md](wwz-load.md#agents-agentes-multi-turno-com-ferramentas).

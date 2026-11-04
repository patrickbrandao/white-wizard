# White Wizard (wwz)

Servidor em Go que finge ser um provedor de LLM (OpenAI, Anthropic, OpenRouter, OpenCode Go, Gemini e Azure OpenAI) para testar roteadores de modelos e clientes sob carga de milhares a dezenas de milhões de requisições por minuto, sem GPU e sem custo de API.

O `wwz` recebe requisições reais, devolve respostas inventadas no formato exato de cada provedor (texto, raciocínio, tool calls válidas contra o JSON Schema enviado, streaming SSE) e simula o que um provedor faz de pior: latência por etapa, filas, erros antes e no meio do stream, limites de taxa, cotas, bloqueios e banimentos. Sem nenhuma configuração de falha, cada driver se comporta como o provedor real ([ADR 0042](specs/decisions/0042-fidelidade-total-ao-provedor-real.md)).

## Início rápido

### Com Docker Compose (wwz + Valkey + Prometheus + painel)

```sh
WWZ_ADMIN_TOKEN=troque-me docker compose up -d --build
```

| Serviço | Endereço local | Observação |
| --- | --- | --- |
| `wwz`, data plane (inferência) | `http://127.0.0.1:18000` | Chaves de teste `wwz-key-01` a `wwz-key-10` |
| `wwz`, control plane (admin, `/health`, `/metrics`) | `http://127.0.0.1:18001` | Bearer `WWZ_ADMIN_TOKEN` em `/admin/*` |
| `wwz-panel` | `http://127.0.0.1:18002` | Usuário `admin`; a senha vem de `WWZ_PANEL_ADMIN_PASSWORD` ou é gerada e impressa uma vez em `docker compose logs wwz-panel` |
| Prometheus | `http://127.0.0.1:19090` | Raspa o `/metrics` do `wwz` |
| Valkey | `127.0.0.1:16379` | Estado de limites, bloqueios e eventos |

As portas publicadas mudam por variável (`WWZ_PORT`, `WWZ_ADMIN_PORT`, `WWZ_PANEL_PORT`, `PROMETHEUS_PORT`, `VALKEY_PORT`), e `WWZ_CONFIG_FILE=./meu.yaml` troca o YAML do `wwz` (padrão [deploy/wwz/wwz.yaml](deploy/wwz/wwz.yaml)). Para derrubar: `docker compose down -v`.

```sh
curl -s http://127.0.0.1:18000/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":20}'
```

Benchmark rápido contra o compose (perfil `load`):

```sh
WWZ_ADMIN_TOKEN=troque-me docker compose run --rm wwz-load --rps 50 --duration 10s
```

### Direto com Go (sem Docker, store em memória)

```sh
go run ./cmd/wwz serve -scenarios-dir ./scenarios
# data plane em :8000; control plane em 127.0.0.1:9000 (sem token, só loopback)
```

Sem `-config`, o servidor sobe com os padrões embutidos: drivers `openai`, `anthropic`, `openrouter` e `opencode` na raiz (`gemini` e `azure` entram por `drivers:` no YAML, com ou sem prefixo, e pedem um modelo com `protocols: ["*"]` ou `[gemini]`), modelos `wwz-fast`, `wwz-realistic` e `wwz-flaky` (dialetos `openai` e `anthropic`) e chaves `wwz-key-01` a `wwz-key-10`. `go run ./cmd/wwz validate -config meu.yaml` confere uma configuração antes de subir.

## Drivers

Seis drivers, um por provedor, todos fiéis ao real ([ADR 0042](specs/decisions/0042-fidelidade-total-ao-provedor-real.md)). Rotas completas, limites e erros em [docs/drivers.md](docs/drivers.md); exemplos de `curl` em [docs/uso.md](docs/uso.md#chamar-cada-driver).

| Driver | Rotas principais | Credencial |
| --- | --- | --- |
| `openai` | `/v1/chat/completions`, `/v1/responses`, `/v1/completions`, `/v1/models` | `Authorization: Bearer` |
| `anthropic` | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`; com `oauth_key`, as rotas OAuth da assinatura do Claude Code | `x-api-key` + `anthropic-version`; token de assinatura `sk-ant-oat01-…` com `oauth_key` ([docs/claude-code.md](docs/claude-code.md)) |
| `openrouter` | `/api/v1/chat/completions`, `/api/v1/responses`, `/api/v1/messages`, `/api/v1/models`, `/api/v1/key` | `Authorization: Bearer` |
| `opencode` | Sob `/inference/go` e `/zen/go`: `/v1/chat/completions`, `/v1/responses`, `/anthropic/v1/messages`, `/google/v1beta/models/{m}:generateContent` (só em `/inference`), `/v1/usage`; exige `x-opencode-session` | `Authorization: Bearer` (`x-goog-api-key` na rota Gemini) |
| `gemini` | `/v1beta/models/{m}:generateContent`, `:streamGenerateContent`, `:countTokens`, `/v1beta/models`, `/v1beta/cachedContents` | `x-goog-api-key` ou `?key=` |
| `azure` | `/openai/deployments/{d}/chat/completions?api-version=…`, `/openai/v1/chat/completions`, `/openai/v1/responses`, `/openai/models` | `api-key` ou `Authorization: Bearer` |

A Responses API existe nos drivers `openai`, `azure`, `openrouter` e `opencode` ([ADR 0055](specs/decisions/0055-contrato-da-responses-api.md)); só o `openai` guarda estado (`previous_response_id`), nos demais é 400 como no real.

## Exemplos

[examples/](examples/README.md) tem 18 configurações prontas, cada uma com `config.yaml`, `docker-compose.yml`, `README.md` e um `smoke.sh` (`make example NAME=<nome>`, `make examples`; exige `make image` antes). Porta local = 18100 + posição na tabela da [spec 019](specs/019-exemplos-e-producao.md) (`opencode-completo` é a 16 e `claude-code-assinatura` a 17).

| Exemplo | Demonstra |
| --- | --- |
| `openai-perfeito`, `anthropic-perfeito` | Driver sem erros, latência realista |
| `falhas-20pct` | 20 % de erros variados |
| `latencia-intermitente` | Picos por cauda pesada |
| `indisponibilidade-janelas` | `errors.outages` em segundos, minutos e horas |
| `rate-limit-rpm` | `rpm` por chave com headers `x-ratelimit-*` |
| `cota-5h-semana` | Cota em janelas de 5 h e semana (OpenCode Go) |
| `creditos-openrouter-esgotando` | Crédito decrescendo até 402 |
| `ban-por-abuso` | Detecção de abuso e bloqueio de chave |
| `stream-corta-no-meio` | `stream_truncated` e `stream_reset` |
| `tool-calls-quebradas` | Argumentos de tool call inválidos |
| `agente-de-arquivos` | Cenário `file-agent-flow` |
| `raciocinio-longo` | Raciocínio extenso antes da resposta |
| `multi-provedor-failover` | Os seis drivers por prefixo, um instável |
| `cluster-3-instancias-valkey` | 3 instâncias com limites compartilhados no Valkey |
| `benchmark-vazao-maxima` | Latência zero e sem limites, para o teto de req/s |
| `opencode-completo` | Catálogos reais do opencode.ai, planos Go, gratuitos e budgets |
| `claude-code-assinatura` | Assinatura OAuth do Claude Code (cota 5 h/semana nos `anthropic-ratelimit-unified-*`) e o modo estrito `client_verification: claude-code` |

## Produção

O autor mantém uma instância pública de testes: dados em `https://<host de inferência>/<driver>` (`/openai`, `/anthropic`, `/openrouter`, `/opencode`, `/gemini`, `/azure`), admin, painel e Grafana em hosts próprios, e instâncias por papel nas portas 8000 a 8008 (perfeito, 20 % de falhas, latência intermitente, indisponibilidade, Anthropic, OpenRouter, OpenCode, admin e o modo estrito do Claude Code). Hosts, IP e chaves não ficam no repositório: os dados do servidor estão em `autor/prod.env`, fora do git (modelo em [deploy/prod/autor.env.example](deploy/prod/autor.env.example)); ver [docs/producao.md](docs/producao.md).

## Binários

| Binário | Papel | Referência |
| --- | --- | --- |
| `wwz` | O simulador: `serve`, `validate`, `simulate` | [docs/uso.md](docs/uso.md), [specs/004](specs/004-apis-e-endpoints.md) |
| `wwz-load` | Cliente de benchmark: TTFT, ITL, E2E, relatórios JSON/CSV, conversas por cenário, instâncias distribuídas, `agents`, `top` e `claude-code` (cliente byte a byte do Claude Code) | [docs/wwz-load.md](docs/wwz-load.md), [specs/015](specs/015-wwz-load.md) |
| `wwz-panel` | Painel web (Go + HTMX + SQLite): uso, erros, entidades bloqueadas e banidas, ações de operador com auditoria | [specs/016](specs/016-painel.md), [docs/painel-backup.md](docs/painel-backup.md) |
| `wwz-tools` | Executor de ferramentas falsas para agentes (catálogos `coder`, `pesquisa`, `chat`) | [docs/wwz-tools.md](docs/wwz-tools.md), [specs/017](specs/017-recursos-importados.md) |
| `wwz-drift` | Compara o wwz com o provedor real e acusa divergência de forma | [docs/wwz-drift.md](docs/wwz-drift.md), [specs/017](specs/017-recursos-importados.md) |

Todos aceitam `help` e `version`.

## Documentação

- [docs/uso.md](docs/uso.md): subir, chamar cada driver, headers `x-wwz-*`, cenários, caos e API admin.
- [docs/configuracao.md](docs/configuracao.md): o YAML, variáveis `WWZ_*`, flags e recarga.
- [docs/drivers.md](docs/drivers.md): drivers, rotas, autenticação, limites e erros de cada provedor.
- [docs/claude-code.md](docs/claude-code.md): o Claude Code real com o wwz (token de assinatura emitido pelo wwz), `oauth_key`/`oauth_plan`, o modo estrito `client_verification: claude-code` e o `wwz-load claude-code`.
- [docs/grafana.md](docs/grafana.md): dashboard Grafana pronto.
- [docs/painel-backup.md](docs/painel-backup.md): backup e restauração do painel.
- [docs/wwz-load.md](docs/wwz-load.md): benchmark (`run`, distribuído, `--scenario`, `agents`, `top`) e metodologia de medida.
- [docs/wwz-tools.md](docs/wwz-tools.md): ferramentas falsas para agentes (catálogo, configuração, HTTP).
- [docs/wwz-drift.md](docs/wwz-drift.md): comparação de forma com os provedores reais (chaves, orçamento, relatório).
- [docs/producao.md](docs/producao.md): URLs, portas 8000-8007 e deploy da produção.
- [docs/verificacao.md](docs/verificacao.md): `verify-quick`/`full`/`heavy`, ensaio de release e fixtures reais.

As especificações em [specs/](specs/INDEX.md) são a fonte da verdade; as decisões estão em [specs/decisions/](specs/decisions/HISTORY.md). Quando a documentação e uma spec divergirem, vale a spec.

## Desenvolvimento

```sh
go mod download
make verify-quick      # gofumpt, golangci-lint, vet e testes dos pacotes alterados
```

Fluxo de trabalho, papéis e Definition of Done em [specs/002](specs/002-organizacao-do-trabalho.md); verificação e CI em [specs/018](specs/018-verificacao-ci-release.md) e [docs/verificacao.md](docs/verificacao.md).

## Publicação

Até a `v0.1.0` a publicação é só local ([ADR 0018](specs/decisions/0018-publicacao-local.md)): não há push para GitHub nem Docker Hub, e as imagens são construídas localmente (`docker compose build` ou `docker build`). Remoto, secrets e a tag de release ficam com o mantenedor.

## Licença

[Apache-2.0](LICENSE).

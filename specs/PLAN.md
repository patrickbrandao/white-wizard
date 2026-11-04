# Plano de implementação

O MVP é entregue inteiro (ADR 0001); as fases abaixo são a ordem interna de implementação. Regra transversal ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md)): nenhum driver diverge do provedor real; cada comportamento é conferido no real (teto de US$ 20 por provedor) ou fica marcado "não conferido" na [spec 006](006-fidelidade-provedores.md#não-conferidos-no-real), seção "Não conferidos no real", e nenhuma fase dá como pronto o que não foi conferido. Uma fase só começa quando o critério de saída da anterior passa. Estado de partida: pacotes de `internal/` prontos e testados, exceto o servidor em volta (`internal/server`), o store Valkey e os binários (estado do pacote original, incorporado aqui).

## Fase 1 — Fundação e servidor utilizável

- **Objetivo:** `wwz` sobe, atende OpenAI e Anthropic com e sem streaming e tem control plane autenticado; repositório com verificação rápida.
- **Specs:** 001, 002, 003, 004, 013, 014, 018.
- **Depende de:** nada.
- **Frentes em paralelo:**
  - infra-ci: `go.mod` em Go 1.27 sem `replace`, `go mod tidy`, gofumpt em todo o código, `.golangci.yml`, `scripts/verify.sh` (quick), `Makefile`.
  - implementador (server): data plane fasthttp em `internal/server` (rotas por mount, `Sniff`, `x-wwz-path`, `clientip`, `max_rps`, cancelamento ao cliente sumir).
  - implementador (admin): control plane net/http em `internal/admin` com bearer `WWZ_ADMIN_TOKEN`, rotas da seção 10, reload atômico e SIGHUP.
  - guardião das specs: `cmd/wwz/openapi.yaml`.
  - Em seguida (depende das três frentes de código): implementador (cmd): `cmd/wwz` mínimo (flags, `serve`).
- **Critério de saída:** `make verify-quick` verde; `wwz` local responde a `POST /v1/chat/completions` e `POST /v1/messages` (stream e não-stream); `/admin/config` sem token devolve 401; todas as rotas admin estão no openapi.yaml.

## Fase 2 — Store, binário completo e contrato dos drivers

- **Objetivo:** limites valendo para o cluster e drivers verificados por contrato.
- **Specs:** 005, 006, 010, 012, 017.
- **Depende de:** fase 1.
- **Frentes em paralelo:**
  - implementador (store): `limits.Store` Valkey (`valkey-go`, Lua, hash tags, `store.on_down`, `store.local_first`, `ev:stream`).
  - guardião das specs: `cache/valkey/README.md` (schema de chaves) e `eventbus/valkey/asyncapi.yaml`.
  - testador: `internal/driver/drivertest` e testes de contrato dos drivers `openai`, `anthropic`, `openrouter` e `opencode` (`gemini` e `azure` entram no drivertest nas tarefas 164–165, fase 5).
  - implementador (cmd): `wwz validate` e `wwz simulate`.
  - infra-ci: `Dockerfile` multi-stage, `cache/valkey/docker-compose.yml`, `deploy/prometheus/`, `docker-compose.yml` raiz, smoke.
- **Critério de saída:** `store: memory` e `store: valkey` passam a mesma suíte; drivertest verde para `openai`, `anthropic`, `openrouter` e `opencode`; `docker compose up` sobe wwz + Valkey + Prometheus e o smoke passa.

## Fase 3 — Clientes e painel

- **Objetivo:** benchmark e operação do cluster.
- **Specs:** 012 (políticas, ADR 0025), 013 (rotas `tier`/`reset`), 015, 016; ADRs 0023–0025.
- **Depende de:** fase 2.
- **Ordem:** 040 (guardião: specs 015/016 e ADRs 0023–0025) → {041, 042, 046} → load (043 → 044 → 045) e painel (047 + 048 → 049 → 050 → 051 → 053); 052 depois de 041 e 022; 054 depois de 043 e 049; 055 fecha a fase.
- **Frentes em paralelo:**
  - guardião das specs: `cmd/wwz-panel/openapi.yaml`, acréscimos `tier`/`reset` em `cmd/wwz/openapi.yaml`, `cache/valkey/README.md` com `cfg:policies` completo (041).
  - implementador (load): núcleo `internal/loadgen` (cliente, SSE, medidas, histograma; 042), modos e relatórios + `cmd/wwz-load` (043), `--scenario` (044), `--instances` (045). Sem dependência nova (ADR 0024).
  - implementador (panel): config + SQLite + bootstrap (046, dono do `go.mod`), autenticação (047, dono do `go.mod` em seguida), fontes de dados (048), `/api/v1` + `wwz-panel serve` (049), telas do núcleo (050), backup/restauração (051), telas restantes (053).
  - implementador (store/admin): `cfg:policies`, tier e reset por entidade no wwz (052, ADR 0025).
  - infra-ci: imagens `wwz-load`/`wwz-panel`, `deploy/panel/`, alertas Prometheus, smoke (054).
  - testador: verificação da saída (055).
- **Critério de saída:** `make verify-quick` verde; `wwz-load` mede TTFT/ITL/E2E contra o compose local (JSON no esquema da spec 015); painel mostra uso e bloqueia/desbloqueia uma chave pela API admin com auditoria; tier por chave e políticas publicadas valem no wwz; restauração de backup testada; todas as rotas de `/api/v1` no `cmd/wwz-panel/openapi.yaml`.

## Fase 4 — Ferramentas auxiliares, release e documentação

- **Objetivo:** pronto para publicar `v0.1.0`.
- **Specs:** 015 (contrato de `agents` e `top`), 017 (contratos do `wwz-tools` e do `wwz-drift`), 018 (verify full/heavy, workflows, ensaio de release, SDKs), 019 (só a parte da fase 4); ADRs 0034–0037.
- **Depende de:** fase 3. Só as tarefas que usam o `wwz-load` ou o painel prontos dependem da saída da fase 3 (task 055); as demais podem começar antes, com `arquivos` disjuntos dos da fase 3.
- **Frentes em paralelo** (até 4 tarefas simultâneas; o disco do Mac tem ~2 GiB livres, então builds de imagem, `node_modules` e o ensaio multi-arch nunca rodam ao mesmo tempo):
  - implementador (tools): `internal/tools` → `cmd/wwz-tools` (contrato `cmd/wwz-tools/openapi.yaml` do guardião antes).
  - implementador (drift): `internal/drift` → `cmd/wwz-drift` (`check`; `record` na fase 5).
  - implementador (load): `wwz-load top` → `wwz-load agents` (o mesmo `cmd/wwz-load/main.go`, em sequência).
  - testador: `tests/sdk/` Python → Node; modo `cluster` dos testes de `internal/limits`.
  - infra-ci: workflows (`quick`, `release`, `drift`) + `actionlint` → `verify full` → `verify heavy` + `deploy/heavy/` → Dockerfile com os 5 binários + `make release-dryrun`; `deploy/grafana/`.
  - redação (implementador): `README.md`, `LICENSE`, `docs/` (uso, configuração, `drivers.md`), depois os documentos de `wwz-load agents/top`, `wwz-tools` e `wwz-drift`.
  - guardião: `CHANGELOG.md` pronto para a `v0.1.0` e instruções de remoto, secrets e tag (spec 018, "Passos do humano").
- **Critério de saída:** `VERIFY_COVERAGE_MIN=0 make verify-full` verde, com `fixtures` e `examples` pulados (insumos da fase 5) e o relatório de cobertura anexado ([ADR 0037](decisions/0037-verify-full-e-heavy-concretos.md)); `make verify-heavy` verde no servidor (drift pulado sem chaves); `make release-dryrun` com as duas arquiteturas das três imagens e os 20 binários (sem push, ADR 0018); `wwz-load agents` e `top` contra o compose local; `wwz-tools serve` e `wwz-drift check` (contra provedores falsos) com seus critérios da spec 017; a tag `v0.1.0` fica para o humano (spec 018, "Passos do humano").

## Fase 5 — Fidelidade real, exemplos, produção e cobertura

- **Objetivo:** fidelidade bit a bit, exemplos prontos, produção no ar e cobertura total.
- **Specs:** 003, 004, 005, 006, 007, 008, 009, 015, 016, 019; ADRs 0013–0018, 0042–0044, 0046.
- **Depende de:** fase 4 (a gravação de fixtures e `errors.outages` podem começar antes, assim que `wwz-drift` e o chaos existirem; as tarefas 161–166 começam assim que as dependências delas fecham, mesmo com a fase 4 aberta, com `arquivos` disjuntos).
- **Tarefas da fidelidade total (ADRs 0042–0044, 0046):** 160 (guardião: specs) → 161 validação sempre como o real (depois de 140) · 162 cache de prompt real (depois de 141) · 163 OpenRouter recusa id sem autor (depois de 142) · 164 driver `gemini` · 165 driver `azure` · 166 painel com `modernc.org/sqlite` (sem dependência). 164 e 165 tocam `internal/driver/drivertest/` e `tests/sdk/` e por isso rodam em sequência entre si e com 163.
- **Auditoria de rotas e Responses API (ADRs 0054–0056):** 231 (guardião: specs e fixtures) → schema `resp` no `cache/valkey/README.md` (guardião) → registro de respostas no store → Responses no driver `openai` (+ drivertest, SDK, sonda do drift) → Responses no `azure` (com a 165), no `openrouter` e no `opencode`; rotas Gemini do `opencode` depois da 164.
- **Recursos do opencode.ai (ADR 0072, depois do critério de saída da fase 5):** 415 (guardião: specs) → 416 fixtures sem custo · 417 → 418 config · 419 store · 420 → 421 engine · 422 → 424 driver (cobrança e erros, Console) · 423 listagem · 425 contrato e SDK · 426 exemplo · 427 specs × fixtures · 428 docs; achados da 423: 434 (guardião; ADR 0072 revisada e ADR 0073) → 435 campos por catálogo (depois da 424) → 436 origem das URLs absolutas (depois da 424 e da 435); 425, 426 e 428 esperam a 435 (428 também a 436).
- **Assinatura OAuth e cliente Claude Code ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), pedido do humano em 2026-11-03, depois do critério de saída da fase 5; spec 020):** 440 (guardião: specs) → em paralelo 441 (validação aceita a forma do Claude Code 2.1.308, conferida no real, e `/api/hello`), 442 (`internal/claudecode` + fixtures `testdata/real/claude-code/2.1.308/`) e 444 (cabeça crua no data plane) → 443 assinatura OAuth no `anthropic` (depois da 441) → 445 `client_verification: claude-code` (depois de 442, 443 e 444); 446 `wwz-load claude-code` (depois da 442; o teste do login contra o wwz espera a 443) → 447 aceite ponta a ponta (depois de 445 e 446) → 448 docs e exemplo `claude-code-assinatura` (depois da 447). Pronto quando: o Claude Code 2.1.308 real responde 200 contra o wwz com `ANTHROPIC_BASE_URL` e `CLAUDE_CODE_OAUTH_TOKEN` emitido pelo wwz; `replay` idêntico em toda fixture; o modo estrito aceita o cliente e as fixtures e recusa cada mutação e os SDKs oficiais com o `reason` da spec 020; `make verify-quick` e o passo `examples` verdes; o não conferido está na [spec 006](006-fidelidade-provedores.md#não-conferidos-no-real), seção "Não conferidos no real".
- **Divergências conhecidas do `anthropic`** (conferidas no real, ainda não reproduzidas; lista em "Conferido no real e ainda não reproduzido" da [spec 006](006-fidelidade-provedores.md#não-conferidos-no-real)): 450 recusas por capacidade do modelo e `usage` por modelo · 455 ordem das recusas de `system` no `count_tokens` · 456 `tool_addition`/`tool_removal` com o beta `inline-tools-2026-09-15`. As três tocam `internal/driver/anthropic/validate.go` e rodam em sequência.
- **Dados do autor fora do git ([ADR 0076](decisions/0076-dados-do-autor-fora-do-git.md), pedido do humano em 2026-11-04):** 473 (guardião: specs, notas para `autor/`; infra-ci: scripts, compose e haproxy genérico lendo `autor/prod.env`). Pronto quando nenhum arquivo versionado identifica o servidor do autor e os scripts de produção e do heavy falham com mensagem clara sem `autor/prod.env`.
- **Frentes em paralelo:**
  - implementador (drift) + testador: `wwz-drift record` e fixtures em `testdata/real/<provedor>/`, comparação byte a byte (ADR 0014).
  - implementador (chaos): `errors.outages` (spec 009).
  - implementador + infra-ci + testador: `examples/` com smoke no verify-full (ADR 0016).
  - infra-ci: `deploy/prod/` e deploy em `/opt/white-wizard` (ADR 0017), smoke externo.
  - testador: cobertura 100% em `internal/` e `cmd/` (ADR 0013), `verify-heavy` no servidor.
  - implementador (server/engine/drivers): 161, 162, 163, 164 (`gemini`), 165 (`azure`); implementador (panel): 166.
  - guardião: chaves de prefixo do cache em `cache/valkey/README.md` (com a 162) e spec 017 (I17/I18 com Gemini e Azure).
- **Critério de saída:** todas as fases concluídas; todo critério de aceite de todas as specs (R1–R24, I1–I24, ADRs 0012–0018) com teste automatizado; `make verify-full` e `make verify-heavy` verdes; cobertura 100%; SDKs Python e Node (`openai`, `anthropic`, `google-genai` e `openai` com `AzureOpenAI`) passando contra os 6 drivers (`openai`, `anthropic`, `opencode`, `openrouter`, `gemini`, `azure`); drivertest verde para os 6; validação de requisição, cache de prompt e id de modelo do OpenRouter iguais ao real (fixtures) ou marcados "não conferido" na [spec 006](006-fidelidade-provedores.md#não-conferidos-no-real), seção "Não conferidos no real"; nenhuma chave `server.strict` ou `cache_hit_probability` aceita; painel sobre `modernc.org/sqlite` com backup e restauração testados; fixtures reais idênticas (Gemini quando `GEMINI_API_KEY` existir; Azure só pela documentação, ADR 0043); `wwz-drift` sem divergência; todos os `examples/` passando; produção com smoke externo verde em todas as URLs e portas 8000–8008; `wwz-load agents` e `top` contra a produção; `README.md`, `docs/` e `LICENSE` completos.

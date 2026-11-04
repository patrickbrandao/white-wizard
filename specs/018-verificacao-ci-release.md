# 018 — Verificação, CI e release

Status: rascunho
Depende de: 001, 002

## Contexto
Os três níveis de verificação, o que o CI roda e como imagens e versões são publicadas. Consolida a seção 23 do documento original com as decisões da entrevista.

## Requisitos
- RF-01: `scripts/verify.sh quick|full|heavy`, exposto como `make verify-quick`, `make verify-full`, `make verify-heavy`; `make test-quick`/`test-full` são aliases.
- RF-02: o script imprime no fim um resumo com ✓/✗/pulado e o tempo de cada passo.
- RNF-01: `verify-quick` em menos de 2 minutos numa máquina de desenvolvimento.

## Comportamento

### verify-quick (padrão)
Roda durante o desenvolvimento e ao concluir cada tarefa. Olha só o que mudou desde `git merge-base HEAD main` (commits, alterações e arquivos novos).
- gofumpt e golangci-lint nos arquivos `.go` alterados.
- `go build ./...` e `go vet` dos pacotes afetados e dos que dependem deles.
- `go test` dos pacotes afetados e dependentes (`go list -deps` invertido); aceita alvos explícitos depois de `--`, ex.: `make verify-quick -- ./internal/scenario -run TestFailFirst`.
- Contrato dos drivers com corpos fixos (`drivertest`) quando `internal/driver/` ou `internal/core/` mudou.
- Testes do store contra o Valkey local já no ar (`WWZ_TEST_VALKEY=local`, banco lógico dedicado limpo com `FLUSHDB` por execução); se o Valkey estiver fora do ar, tenta `docker compose up -d valkey` ou diz o comando.
- Não roda build de imagem, SDKs, smoke nem a suíte inteira.
- Roda todos os passos e sai com código diferente de zero se algum falhar.

### verify-full (sob pedido)
Roda quando o usuário pedir ("verificação completa", "rode tudo") e antes de criar tag de release. Executa os passos na ordem abaixo e para na primeira falha, mas sempre imprime o resumo e roda a limpeza ([ADR 0037](decisions/0037-verify-full-e-heavy-concretos.md)). Passo cujo insumo ainda não existe no repositório sai como `pulado (motivo)`; quando o insumo existe, o passo bloqueia.

| # | Passo | O que faz | Pulado quando |
| --- | --- | --- | --- |
| 1 | `preflight` | Confere `go`, `gofumpt`, `golangci-lint`, `docker`, `node`/`npm` e o Python de `$VERIFY_PYTHON` (padrão `tmp/venv/bin/python`). Confere também o disco livre ≥ `VERIFY_MIN_FREE_GB` (padrão 1,5): abaixo disso, roda `go clean -testcache` e `docker builder prune -f`, confere de novo e falha com o número medido | nunca |
| 2 | `gofumpt`, `lint`, `vet`, `build` | `gofumpt -l .` vazio, `golangci-lint run ./...` completo, `go vet ./...`, `go build ./...` | nunca |
| 3 | `valkey` | Sobe `docker run --rm -d --name wwz-verify-valkey-<pid> -p 127.0.0.1::6379 valkey/valkey:<versão do compose>` e exporta `WWZ_TEST_VALKEY=local`, `WWZ_TEST_VALKEY_ADDR` (porta efêmera) e `WWZ_TEST_VALKEY_DB=15`. Substitui o testcontainers-go | nunca |
| 4 | `test` | `go test -race -covermode=atomic -coverprofile=tmp/coverage/cover.out ./...`, incluindo todos os cenários, todos os tipos de erro, resiliência (I15), backup e restauração do painel e `drivertest` | nunca |
| 5 | `coverage` | `go tool cover -func` por pacote de `internal/` e `cmd/`; qualquer pacote abaixo de `VERIFY_COVERAGE_MIN` (padrão **100**, [ADR 0013](decisions/0013-cobertura-total.md)) falha, listando os pacotes e os percentuais. Relatório em `tmp/coverage/summary.txt` e `tmp/coverage/cover.html` | nunca |
| 6 | `fixtures` | `go test -count=1 -run 'TestRealFixtures' ./...`: os testes que leem `testdata/real/` ([ADR 0014](decisions/0014-fidelidade-bit-a-bit.md)), no formato da seção "Fixtures reais"; falha se nenhum teste rodar | `testdata/real/` ausente |
| 7 | `workflows` | `scripts/lint-workflows.sh` (`actionlint` em container) sobre `.github/workflows/*.yml` | `.github/workflows/` ausente |
| 8 | `images` | `make image image-panel image-load` (arquitetura nativa, tag `:verify`) | nunca |
| 9 | `smoke` | `scripts/smoke.sh` com a imagem `:verify`: sobe o compose, faz uma chamada por driver e derruba com `down -v` | nunca |
| 10 | `sdk-python` | Sobe `bin/wwz serve --config tests/sdk/wwz.yaml` numa porta livre (store `memory`), exporta `WWZ_SDK_BASE_URL` e `WWZ_SDK_KEY` e roda `$VERIFY_PYTHON -m pytest -q tests/sdk/python` | nunca |
| 11 | `sdk-node` | Com o mesmo wwz, roda `npm ci --prefer-offline --no-audit --no-fund --prefix tests/sdk/node` só se `node_modules` faltar ou estiver desatualizado em relação ao lock, e depois `node --test tests/sdk/node/` | nunca |
| 12 | `examples` | Cada `examples/<nome>/` (diretórios que começam com `_`, como o `_lib.sh`, não são exemplos) precisa de `config.yaml`, `docker-compose.yml`, `README.md` e `smoke.sh`; o passo falha listando o que falta antes de subir qualquer exemplo. Depois, um exemplo por vez: `wwz validate` no `config.yaml` e o `smoke.sh`, que sobe o compose (projeto `wwz-ex-<nome>`, porta `18100 + índice` da tabela da spec 019), confere e derruba com `down -v` mesmo em falha. `scripts/verify.sh examples [nome...]` roda só este passo, opcionalmente só os exemplos pedidos. Convenções em `examples/README.md` (task 202; [ADR 0016](decisions/0016-diretorio-examples.md)) | `examples/` ausente (fase 5) |
| 13 | `cleanup` | Para o Valkey e o wwz do passo 10, remove as imagens `:verify` e roda `docker builder prune -f --filter until=24h`. Roda sempre, mesmo depois de falha | nunca |

O resumo final traz cada passo com ✓/✗/pulado e o tempo; o código de saída é 0 só se nenhum passo falhou. Na saída da fase 4, o PLAN roda `VERIFY_COVERAGE_MIN=0 make verify-full` (relatório gerado, gate desligado). A saída da fase 5 exige o padrão de 100.

### verify-heavy (sob pedido, servidor pesado)
Benchmarks de vazão e de streams abertos ([014](014-desempenho-e-escala.md)), Valkey Cluster e drift (I24). O orquestrador é o próprio `scripts/verify.sh heavy` no Mac, que chama `scripts/heavy.sh`. Os benchmarks rodam só no servidor designado pelo usuário ([ADR 0017](decisions/0017-deploy-producao.md)), dentro dele, pela rede interna. `deploy/heavy/` tem apenas templates; as credenciais ficam no `.env` do servidor, fora do git. O drift roda no Mac ([ADR 0035](decisions/0035-wwz-drift-comparacao-de-forma.md)).

Variáveis: `WWZ_HEAVY_SSH` (comando SSH completo até o servidor, sem padrão no código: vem do ambiente ou de `autor/prod.env`, e o `heavy.sh` falha sem ele; ver spec [019](019-exemplos-e-producao.md#arquivos-de-implementação-do-autor) e [ADR 0076](decisions/0076-dados-do-autor-fora-do-git.md)), `WWZ_HEAVY_DIR` (padrão `/opt/white-wizard/heavy`), `HEAVY_MIN_RPS` (padrão `10000`), `HEAVY_STREAMS` (padrão `7000`, [ADR 0071](decisions/0071-overhead-em-malha-aberta-e-7000-streams.md), que atualiza a [ADR 0069](decisions/0069-pacing-pelo-fim-planejado-e-gogc.md); streams da rodada `streams`, que cabem nos 12 núcleos a ≤ 80 %), `HEAVY_OVERHEAD_RPS` (padrão `HEAVY_MIN_RPS`, taxa da fase de overhead da `throughput`), `HEAVY_MAX_SCHEDULE_LAG_MS` (padrão `1`, `schedule_lag` p99 máximo do cliente nessa fase), `HEAVY_STREAMS_MAX` (padrão `20000`, streams da rodada informativa `streams-max`), `HEAVY_MAX_ITL_P99_MS` (padrão `22`, ITL de 20 ms + 2 ms de overhead da spec 014), `HEAVY_MAX_MEM_PER_10K_MB` (padrão `2048`, pico de memória do container do wwz na rodada `streams` ÷ `HEAVY_STREAMS` × 10 000; medido ~1 650, [ADR 0070](decisions/0070-lote-da-rodada-8-achados-de-spec.md)), `HEAVY_MAX_WWZ_CPU_PCT` (padrão `80`, CPU média máxima do wwz na rodada `streams`), `HEAVY_STREAMS_WWZ_CPUS` (padrão `12`), `HEAVY_STREAMS_LOAD_CPUS` (padrão `14`), `HEAVY_PROD_PAUSE` (padrão `1`), `HEAVY_PROD_PAUSE_STREAMS` (padrão `wwz-admin`, containers da produção parados só durante as rodadas de streams e religados logo depois), `HEAVY_MAX_ERROR_PCT` (padrão `0.1`, % de erros aceito nas rodadas `throughput` e `streams`), `HEAVY_MAX_LOAD` (padrão `48`), `HEAVY_ROUNDS` (padrão `cluster,throughput,streams,streams-max`; nome desconhecido falha o preflight), `HEAVY_DRIFT` (padrão `1`; `0` pula o passo `drift`, para repetir rodadas sem gastar chamadas reais), `HEAVY_KEEP_IMAGES` (padrão `0`; `1` mantém no servidor as imagens `:heavy-<sha7>` e o `limits.test` desta execução, para repetir rodadas). No servidor, o `run.sh` aceita ainda `HEAVY_ROUND_TIMEOUT` (600), `HEAVY_MAX_CPUS` (32), `HEAVY_MAX_MEMORY_GB` (64), `HEAVY_WWZ_CPUS` (8), `HEAVY_LOAD_CPUS` (8), `HEAVY_STREAMS_WWZ_MEMORY` (`16g`) e `HEAVY_STREAMS_LOAD_MEMORY` (`24g`).

| # | Passo | O que faz |
| --- | --- | --- |
| 1 | `preflight` | Local: ferramentas, Docker de pé, `HEAVY_ROUNDS` válido e disco local como no full (`VERIFY_MIN_FREE_GB`). Remoto, numa única chamada `ssh`: load average (1 min) < `HEAVY_MAX_LOAD`, ≥ 5 GiB livres em `/opt`, `docker`, `jq` e `timeout` presentes; registra o `nproc` |
| 2 | `images` | `docker buildx build --platform linux/amd64 --load` dos alvos `wwz` e `wwz-load`, com tag `:heavy-<sha7>`; `docker save \| gzip \| $WWZ_HEAVY_SSH docker load`; remove as imagens locais em seguida |
| 3 | `store-test` | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o tmp/heavy/limits.test ./internal/limits` e cópia para `$WWZ_HEAVY_DIR/bin/` |
| 4 | `sync` | Copia `deploy/heavy/` para `$WWZ_HEAVY_DIR` (nunca copia nem sobrescreve o `.env` do servidor) |
| 5 | rodadas | `deploy/heavy/run.sh <rodada>` no servidor, uma por vez, cada uma sob `timeout 600` (código 124). O `run.sh` recusa começar com load average ≥ 48 (código 4) ou com a soma de `cpus` > 32 ou de `mem_limit` > 64g no perfil (código 2). Um monitor grava `docker stats` dos containers `wwz-heavy-*`, com o load average e o carimbo, em `stats.jsonl` a cada ~5 s e aborta a rodada (`docker compose kill`, código 3) se o load average passar de 48. Projeto compose `wwz-heavy`, recursos `wwz-heavy-*`. Cada rodada grava `report.json` (ou `limits.log`), `stats.jsonl`, `compose.log` e `meta.json` (início, fim, status, load antes e depois, orçamento, `nproc` e memória do host) e derruba o próprio compose com `down -v` ao fim |
| 5a | `cluster` | Valkey Cluster com 3 mestres + `limits.test -test.run 'Valkey'` num container `distroless/static` com `WWZ_TEST_VALKEY=cluster` e `WWZ_TEST_VALKEY_ADDR=<3 endereços separados por vírgula>`; passa se o teste passa |
| 5b | `throughput` | 1 `wwz` (8 CPUs, latência zero, store `memory`) e o `wwz-load` (8 CPUs) com `--concurrency 512 --stream=false --warmup 10s --duration 60s --timeout 30s --output json`; passa com `total.ok_rps ≥ HEAVY_MIN_RPS` e erros < `HEAVY_MAX_ERROR_PCT`. Depois dela, o mesmo wwz recebe a fase de overhead ([ADR 0071](decisions/0071-overhead-em-malha-aberta-e-7000-streams.md)): `--rps $HEAVY_OVERHEAD_RPS --stream=false --warmup 5s --duration 30s --timeout 30s` (malha aberta, `overhead.json`); overhead p99 = `total.e2e_ms.p99` − latência configurada (zero) < `HEAVY_MAX_OVERHEAD_MS` (2), `ok_rps ≥ 95 %` da taxa pedida, erros < `HEAVY_MAX_ERROR_PCT` e `schedule_lag_ms.p99 < HEAVY_MAX_SCHEDULE_LAG_MS`. Com o lag acima do limite o `run.sh` repete a fase uma vez (a primeira fica em `overhead-1.json` e no `result.json` como `overhead_discarded`); se repetir, reprova. O e2e p99 da malha fechada é só informado |
| 5c | `streams` | Meta de ITL da spec 014 ([ADR 0067](decisions/0067-meta-de-streams-pelo-custo-medido.md)). 1 `wwz` (`HEAVY_STREAMS_WWZ_CPUS`, 12 CPUs; ITL fixo de 20 ms, um token por chunk, `max_tokens` 100) e o `wwz-load` (`HEAVY_STREAMS_LOAD_CPUS`, 14 CPUs e 24g, para o cliente não ser o gargalo) com `--concurrency $HEAVY_STREAMS --stream=true --warmup 15s --duration 120s --timeout 60s --output json`; para cada container de `HEAVY_PROD_PAUSE_STREAMS` antes e o religa depois (orçamento 6 + 12 + 14 = 32 núcleos). Passa com erros < `HEAVY_MAX_ERROR_PCT`, `total.itl_ms.p99 ≤ HEAVY_MAX_ITL_P99_MS`, `config.concurrency = HEAVY_STREAMS` `wwz_cpu_util_mean ≤ HEAVY_MAX_WWZ_CPU_PCT/100` (média só das amostras do wwz com o container do wwz-load de pé, mesmo carimbo `t` no `stats.jsonl`) e memória por 10 000 streams ≤ `HEAVY_MAX_MEM_PER_10K_MB` (acima disso a medida não vale e a rodada reprova; baixe `HEAVY_STREAMS`) |
| 5d | `streams-max` | Capacidade, só informativa. Mesma configuração da 5c com `--concurrency $HEAVY_STREAMS_MAX`; grava no `result.json` erros, TTFT, ITL, tokens/s, `wwz_cpu_util_mean` e `chunks_per_core_s` (tokens/s ÷ `cpu.wwz.mean_cores`), com `informative: true`. Nunca reprova o verify; só entra como falha se não rodar (sem `report.json`) |
| 6 | `collect` | Cada rodada já trouxe para `tmp/heavy/<UTC AAAAMMDDTHHMMSSZ>/<rodada>/` os arquivos do servidor e gerou um `result.json`. O passo grava `summary.json` com `stamp`, `tag`, `commit`, `dirty`, `server`, `dir`; `thresholds` (`min_ok_rps`, `streams`, `streams_max`, `max_itl_p99_ms`, `max_wwz_cpu_pct`, `overhead_rps`, `max_overhead_schedule_lag_p99_ms`, `max_error_pct`, `max_load_average`, `round_timeout_s`, `max_cpus`, `max_memory_gb`, `streams_wwz_cpus`, `streams_load_cpus`); `rounds.<rodada>` com as medidas (requisições, erros, % e tipos, `rps`/`ok_rps`, tokens/s, TTFT/ITL/E2E p50/p90/p99/max/mean), a CPU por container (`cpu.wwz` e `cpu.load` com `max_cores`, `mean_cores` e amostras), a última memória de cada container, `checks`, `pass` e o `meta` da rodada (com o load average); na `cluster`, a contagem de PASS/FAIL/SKIP; na `throughput`, o objeto `overhead` (`target_rps`, `requests`, `ok`, `errors`, `dropped`, `error_pct`, `rps`, `ok_rps`, `e2e_ms`, `schedule_lag_ms`) e os checks `overhead_rate`, `overhead_errors` e `overhead_lag`; nas de streams, `wwz_cpu_util_mean`. `pass` geral só com todas as rodadas pedidas aprovadas (a `streams-max` conta só por ter rodado); rodada sem resultado entra como `skipped` e reprova |
| 7 | `drift` | No Mac: `wwz-drift check --real --output json --out tmp/heavy/<carimbo>/drift.json` (`--real` é o que autoriza chamar o real; sem ele o provedor sai `skipped`/`real_not_confirmed`, task 232). Pulado com `HEAVY_DRIFT=0` ou sem `~/.config/white-wizard/real-keys.env` e sem as variáveis de chave; código 3 do drift falha o passo |
| 8 | `cleanup` | Roda sempre (trap). No servidor, `run.sh cleanup`: `docker compose -p wwz-heavy down -v`, remoção de **todas** as imagens `white-wizard*:heavy-*` (com `HEAVY_KEEP_IMAGES=1`, mantém só as da execução atual e o `limits.test`), das imagens base que o `run.sh` puxou, de `runs/` e de `bin/limits.test`. No Mac, remove as imagens `:heavy-*` locais e os binários temporários. Falha se sobrar container, rede ou imagem `wwz-heavy`/`:heavy-*` fora da exceção (critério "nada no servidor") |

Histórico da rodada `streams`: a primeira execução (2026-10-31, 20 000 streams em 8 CPUs) reprovou com ITL p99 de 186 ms (~20 mil chunks/s por núcleo); depois das tasks 158, 268, 269 e 341 o wwz chegou a ~49 mil chunks/s por núcleo, e 20 000 streams em 12 CPUs ficaram em ITL p50/p90/p99 de 20,0/20,4/28,7 ms com o wwz a 96 % (`tmp/heavy/20261031T181146Z`). A [ADR 0067](decisions/0067-meta-de-streams-pelo-custo-medido.md) passou a medir o ITL com o wwz ≤ 80 % (rodada `streams`, 8 000 streams a partir da [ADR 0069](decisions/0069-pacing-pelo-fim-planejado-e-gogc.md) e 7 000 a partir da [ADR 0071](decisions/0071-overhead-em-malha-aberta-e-7000-streams.md)) e a registrar os 20 000 streams só como capacidade (`streams-max`).

Os testes de `internal/limits` aceitam `WWZ_TEST_VALKEY=cluster`: endereços separados por vírgula, banco 0 e `FLUSHALL` em cada mestre antes de cada teste.

### CI (GitHub Actions)
O remoto é `git@github.com:patrickbrandao/white-wizard.git`, público ([ADR 0075](decisions/0075-github-publico-e-imagens-tmsoftbrasil.md), que encerra a publicação só local da [ADR 0018](decisions/0018-publicacao-local.md)); os workflows também são validados localmente pelo passo `workflows` do `verify-full`.
- `quick.yml`: em todo push e PR, Go 1.27, `gofumpt`, `golangci-lint` e `make verify-quick` com `VERIFY_BASE=origin/main` e um service container `valkey` (ADR 0008, provisória).
- `release.yml`: em push na `main`, build multi-arch (`linux/amd64`, `linux/arm64`) com `docker/build-push-action` e push de `docker.io/tmsoftbrasil/white-wizard:latest` e `:sha-<7>`, mais `white-wizard-load` (que também leva o `wwz-tools` e o `wwz-drift`, [ADR 0034](decisions/0034-contrato-do-wwz-tools.md)) e `white-wizard-panel` como imagens separadas, com as mesmas tags. As três recebem os build-args `VERSION` (versão do metadata action) e `COMMIT` (`github.sha`).
- Em tag `vX.Y.Z`: imagens `:X.Y.Z` e `:X.Y`; binários `wwz`, `wwz-load`, `wwz-panel`, `wwz-tools` e `wwz-drift` para `linux`/`darwin` × `amd64`/`arm64` (`CGO_ENABLED=0`, `-trimpath`, `-ldflags "-X main.version=… -X main.commit=…"`), empacotados como `white-wizard_<versão>_<os>_<arch>.tar.gz` com `SHA256SUMS`, no GitHub Releases; o corpo da release é a seção da versão no `CHANGELOG.md`. O `CHANGELOG.md` com `[Não lançado]` é movido para a versão pelo humano antes da tag.
- `drift.yml`: cron diário às 06:00 UTC e `workflow_dispatch`, `wwz-drift check --real --output json` com as chaves em secrets (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`, `OPENCODE_API_KEY`, `GEMINI_API_KEY`), relatório como artefato e falha do job no código 3 ([ADR 0035](decisions/0035-wwz-drift-comparacao-de-forma.md)).
- Credenciais `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` (conta `tmsoftbrasil` e um token de acesso pessoal dela, ADR 0075) e as chaves reais ficam em secrets do repositório, configurados pelo usuário, nunca pelo agente.

### Passos do humano
Ações irreversíveis ou que exigem segredo ([spec 002](002-organizacao-do-trabalho.md), escalonamento): nenhum agente as executa. Os comandos rodam na raiz do repositório.

1. **Ratificar as ADRs provisórias** do [HISTORY](decisions/HISTORY.md) antes da tag (aceitar ou pedir a substituta ao guardião das specs). Mudam contrato público (YAML incompatível) ou contrariam uma decisão anterior do humano: 0046, 0048, 0051, 0052, 0059, 0062, 0067, 0068 e 0071. A 0067 e a 0071 relaxam a meta de streams da spec 014; recuperar a meta original (20 000 streams com ITL p99 ≤ 22 ms) exige subir o teto de 32 núcleos da [ADR 0017](decisions/0017-deploy-producao.md) e `HEAVY_STREAMS`.
2. **Secrets do repositório** (com o `gh` autenticado):
   ```sh
   gh secret set DOCKERHUB_USERNAME --body tmsoftbrasil
   gh secret set DOCKERHUB_TOKEN            # token de acesso pessoal da conta tmsoftbrasil (Read & Write)
   gh secret set -f ~/.config/white-wizard/real-keys.env   # OPENAI, ANTHROPIC, OPENROUTER, OPENCODE, GEMINI _API_KEY
   gh secret list
   gh run rerun --failed $(gh run list --workflow release.yml -L1 --json databaseId -q '.[0].databaseId')
   ```
   Sem `DOCKERHUB_TOKEN`, o job de imagens do `release.yml` falha sem publicar nada. Com os secrets de provedor, o `drift.yml` diário custa cerca de US$ 0,0002 por execução; para desligar o agendamento, `gh workflow disable drift.yml`. Se o `docker login` reclamar de `docker-credential-desktop`, ponha `/Applications/Docker.app/Contents/Resources/bin` no `PATH`.
3. **Tag `v0.1.0`**: a seção `[0.1.0]` do `CHANGELOG.md` é o corpo da release; se a tag sair noutra data, ajuste a data da seção antes.
   ```sh
   make release-dryrun          # opcional: 3 imagens × 2 arquiteturas e 20 binários, sem push; não rode junto com outros builds de imagem
   git tag -a v0.1.0 -m "White Wizard v0.1.0"
   git push origin v0.1.0
   gh run watch
   gh release view v0.1.0
   ```
   A tag publica as imagens `:0.1.0` e `:0.1` e a GitHub Release com os 20 binários e o `SHA256SUMS`.
4. **Produção**: as portas 8000–8008 encaminhadas do IP público ao host (NAT ou firewall do provedor, quando houver) e o `autor/prod.env` preenchido (spec [019](019-exemplos-e-producao.md#arquivos-de-implementação-do-autor)); credenciais só em `/opt/white-wizard/CREDENTIALS.md` no servidor.
5. **Chaves reais para fechar os "não conferidos"** (opcional; lista na spec [006](006-fidelidade-provedores.md#não-conferidos-no-real)): Azure OpenAI (`AZURE_OPENAI_API_KEY` e o endpoint de um recurso com um deployment), chave de provisionamento do OpenRouter, chave admin ou limite de gasto de workspace da Anthropic, projeto da OpenAI com limite próprio e acesso a gpt-5/o-series. Com as chaves em `~/.config/white-wizard/real-keys.env`: `go run ./cmd/wwz-drift check --real` e `go run ./cmd/wwz-drift record --real --providers <p>`; o gasto fica em `tmp/real-spend.json` (teto de US$ 20 por provedor, [ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md)).

### Imagens
Um único `Dockerfile` multi-stage: o estágio `build` (no `BUILDPLATFORM`, sem emulação) compila os 5 binários e um `healthcheck` mínimo (GET numa URL, sai 0 em 2xx) com `CGO_ENABLED=0`, `-trimpath` e `-ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}"` (build-args `VERSION`, padrão `dev`, e `COMMIT`, padrão vazio). As imagens finais são `gcr.io/distroless/static-debian12:nonroot` (sem shell, usuário 65532), com os binários em `/usr/local/bin`:

| Alvo | Imagem | Binários | Entrada | Portas | Health check |
| --- | --- | --- | --- | --- | --- |
| `wwz` (padrão, último estágio) | `white-wizard` | `wwz`, `healthcheck` | `ENTRYPOINT wwz`, `CMD serve` | 8000, 9000 | `healthcheck` em `:9000/health/ready` |
| `wwz-panel` | `white-wizard-panel` | `wwz-panel`, `healthcheck` | `ENTRYPOINT wwz-panel`, `CMD serve`; volumes `/data` (`WWZ_PANEL_DB=/data/panel.db`) e `/backup` | 8100 | `healthcheck` em `:8100/health/ready` |
| `wwz-load` | `white-wizard-load` | `wwz-load`, `wwz-tools`, `wwz-drift`, `healthcheck` | **sem `ENTRYPOINT`**; `CMD ["wwz-load", "help"]`: o comando escolhe o binário pelo `PATH` (`docker run --rm white-wizard-load:dev wwz-tools version`) | 9200 (`wwz-tools serve`) | nenhum na imagem (o `wwz-load` é processo de vida curta); o compose usa o `healthcheck` em `/healthz` do `wwz-tools` |

- `make image`, `make image-panel` e `make image-load` constroem cada alvo na arquitetura nativa; `make image-multiarch` só no cache do buildx (sem push, [ADR 0018](decisions/0018-publicacao-local.md)).
- `docker-compose.yml` raiz: serviço `wwz-tools` com a imagem do `wwz-load` (`WWZ_LOAD_IMAGE`, padrão `white-wizard-load:dev`), `entrypoint: [/usr/local/bin/wwz-tools]`, `command: [serve]`, `WWZ_TOOLS_LISTEN=:9200` e porta publicada `${WWZ_TOOLS_PORT:-18003}:9200`; raspado pelo Prometheus em `wwz-tools:9200` e conferido no smoke. O `wwz-tools` não tem imagem própria (ADR 0034).
- `wwz version` (como `wwz-load version` e `wwz-panel version`) imprime `wwz <versão> (commit <commit>)` quando o commit foi injetado e `wwz <versão>` sem ele; sem `-X main.version`, a versão vem do build info do Go ou fica `dev`.

### Ensaio local da release (`make release-dryrun`)
`scripts/release-dryrun.sh` reproduz localmente o que o `release.yml` faria, sem push e sem tag:
- Para cada alvo (`wwz`, `wwz-load`, `wwz-panel`), um por vez: `docker buildx build --platform linux/amd64,linux/arm64 --output type=oci,dest=tmp/release/<alvo>.tar` num builder `docker-container` chamado `wwz-builder` (criado se faltar). Em seguida, confere no `index.json` do tar os manifests das duas arquiteturas, registra os digests em `tmp/release/manifests.txt` e apaga o tar.
- Compila os 20 binários (5 × 2 SO × 2 arquiteturas) um por vez, grava o `sha256` em `tmp/release/SHA256SUMS` e apaga cada binário depois de medir o hash. A compilação cruzada usa um `GOCACHE` próprio em `tmp/release/gocache`, para não inchar o cache do host.
- Confere, antes das imagens, que o `CHANGELOG.md` tem `[Não lançado]` não vazio e que `wwz version` imprime a versão e o commit injetados (`VERSION`, padrão `git describe --tags --always --dirty`; `COMMIT`, padrão `git rev-parse HEAD`).
- Recusa começar com menos de `RELEASE_MIN_FREE_GB` (padrão 3) GiB livres. Variáveis: `RELEASE_OUT` (padrão `tmp/release`), `RELEASE_TARGETS` (padrão `wwz wwz-load wwz-panel`), `RELEASE_SKIP_IMAGES=1` e `RELEASE_SKIP_BINARIES=1` (resultado incompleto).
- Ao fim, com sucesso ou falha (trap): `docker buildx prune -af --builder wwz-builder` e remoção de qualquer tar, binário e do `tmp/release/gocache`. Sai ≠ 0 se faltar alguma arquitetura, algum binário ou a seção do `CHANGELOG.md`.

### Fixtures reais (`testdata/real/`)
Insumo do passo `fixtures` e do `verify-quick` quando o driver muda (ADR 0014). O conjunto atual é o de validação (task 161): `testdata/real/<provedor>/validation/<nome>.json` (`openai`, `anthropic`, `openrouter`, `opencode`), um arquivo por requisição gravada contra o provedor real:

```json
{
  "provider": "anthropic",
  "name": "messages_nested_unknown",
  "recorded_at": "2026-10-31",
  "request": {
    "method": "POST",
    "path": "/v1/messages",
    "headers": {"x-api-key": "<key>", "anthropic-version": "2023-06-01"},
    "body": "{\"model\": \"claude-haiku-4-5\", ...}"
  },
  "response": {
    "status": 400,
    "content_type": "application/json",
    "body": "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.0.content.0.text.foo: Extra inputs are not permitted\"},\"request_id\":\"<request_id>\"}"
  }
}
```

- `request`: `method`, `path`, `headers` e `body` (o corpo exato enviado, como string). Credenciais viram marcadores (`Bearer <key>` e `<key>`; id de sessão `<session>`), que o teste troca por uma chave e uma sessão válidas do wwz.
- `response`: `status`, `content_type` exatamente como veio (`""` quando o real não mandou o header; o teste lê então o header cru) e `body` normalizado: `"request_id":"req_…"` vira `"<request_id>"` e `"user_id":"user_…"` vira `"<user_id>"`. Requisição aceita guarda só `status: 200` e `body: null` (o conteúdo gerado não é comparado).
- Comparação: status sempre; com corpo, também o tipo de mídia e o corpo. Nos gateways (`openrouter`, `opencode`), cujas respostas de validação o wwz escreve por inteiro, byte a byte. Em `openai` e `anthropic`, hoje, por campos e valores (JSON decodificado), porque o envelope ainda não reproduz a serialização do real (a OpenAI devolve JSON indentado com 4 espaços e `\n` final). A ADR 0014 continua valendo: a comparação passa a byte a byte quando a serialização desses dois drivers for igual à do real (tarefa própria).
- Forma das respostas (`testdata/real/<provedor>/shape/<sonda>.json`; `opencode-google` em `testdata/real/opencode/google/`), gravadas por `wwz-drift record` (spec 017), uma por sonda (`models`, `chat`, `chat_stream`, `error_auth`, `error_model`, `responses`, `responses_stream`). Mesmo formato, mais `request.headers` com os cabeçalhos enviados e `response.headers` com os relevantes, em minúsculas. Marcadores além de `<key>`, `<session>`, `<request_id>` e `<user_id>`: `<generation_id>` (ids `gen-…`, também quando o real os põe em `request-id`, e `x-generation-id`), `<duration>` (`x-ratelimit-reset-*` da OpenAI, como `6m0s`), `<project_id>` (`openai-project`), `<organization_id>`, `<workspace_id>`, `<log_id>`, `<ms>` (`openai-processing-ms`) e `<rfc3339>` (`anthropic-ratelimit-*-reset`). Lista de modelos acima de 64 KiB guarda só o item da sonda. O conteúdo gerado não é comparado.
- Ordem de validação (`testdata/real/<provedor>/auth-order/`, task 187, ADR 0053): mesmo formato. Nas fixtures `badkey_*` a credencial inválida fica literal (é falsa de propósito; a OpenAI a devolve mascarada no corpo) e o teste a envia como está; nas `nokey_*` o header de credencial não vai; nas `validkey_*` `<key>` vira uma chave válida do wwz. Lidas por `TestRealFixturesAuthOrder`.
- Cliente Claude Code (`testdata/real/claude-code/<versão>/`, [ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), spec 020): requisições cruas do **cliente** real (não respostas do provedor), `*.raw` com a cabeça e o corpo exatamente como o Claude Code os mandou a um servidor de captura, normalizados só nos caminhos e no usuário (com `Content-Length` recalculado), e `MANIFEST.md` com modo, tipo e sha256 de cada uma. Nunca o prompt padrão do Claude Code. Lidas por `TestRealFixturesClaudeCode` (`Build`/`replay` com o mesmo sha256, `Verify` aceita) e pelo teste do driver que as envia cruas por TCP. Gravar uma versão nova é captura local com `tmp/claude-code/capture.py`, sem custo e sem credencial real.
- Nome dos testes: todo teste que lê `testdata/real/` se chama `TestRealFixtures<Assunto>` (ex.: `TestRealFixturesValidation`), para o passo `fixtures` achá-lo pelo `-run`.

### Testes de SDK (`tests/sdk/`)
- `tests/sdk/wwz.yaml` é a configuração do wwz usada pelos dois SDKs: os 6 drivers montados (`openai`, `anthropic`, `openrouter`, `opencode`, `gemini`, `azure`), latência zero, modelos `wwz-fast` (texto), `wwz-tools` (tool calls com probabilidade 100), `wwz-reason` (raciocínio) e um modelo `wwz-rl` com `rpm: 1` para o erro de limite.
- **Python** (`tests/sdk/python/`, pytest, versões fixadas em `requirements.txt` iguais às de `tmp/venv`: `openai` 3.42, `anthropic` 2.3, `google-genai`) e **Node** (`tests/sdk/node/`, `node --test`, `openai`, `@anthropic-ai/sdk` e `@google/genai` do `package-lock.json`). Cada um cobre, para cada driver (`openai`, `openrouter` e `opencode` pelo SDK `openai` com `base_url` no mount do driver; `azure` pelo cliente `AzureOpenAI` do SDK `openai` com `azure_endpoint` no mount `/azure`, `api-key` e `api_version`; `anthropic` pelo SDK `anthropic`; `gemini` pelo SDK `google-genai` com `base_url` no mount `/gemini`, spec 006 seção 16.5):
  - chat sem stream e com stream, com o texto montado igual ao `content` e o `usage` coerente;
  - tool call com argumentos que o SDK desserializa (stream acumulado e não-stream);
  - raciocínio no campo do dialeto (ADR 0022);
  - `401` com chave inválida virando a exceção de autenticação do SDK, e `429` virando a exceção de rate limit com `retry-after` legível (`max_retries=0`);
  - `GET /v1/models` pelo SDK.
- Modo estrito do cliente ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)): `tests/sdk/wwz.yaml` monta também um `anthropic` em `/anthropic-cc` com `client_verification: claude-code`; o SDK `anthropic` (Python e Node) recebe ali 403 `forged_request_error` (a exceção de permissão do SDK, com o `type` no corpo) e 200 no mount sem a opção.
- Base e chave por `WWZ_SDK_BASE_URL`/`WWZ_SDK_KEY`. Sem essas variáveis, os testes saem com "pulado" (para rodar à mão fora do verify).
- Disco: nenhum `pip install` nem `npm install` fora do que já está em `tmp/venv` e em `tests/sdk/node/node_modules`. Atualizar um SDK é tarefa própria; a instalação do `google-genai` (Python) e do `@google/genai` (Node) entra pela tarefa do driver `gemini` (164), sem outra dependência nova (o `AzureOpenAI` já vem no SDK `openai`).

### Regras para os agentes
- Toda mudança vem com o teste que a cobre; o nível só decide quanto da suíte roda.
- Ao terminar, dizer qual nível rodou e o resultado.
- Subagentes seguem a mesma regra: quick por padrão, full só quando quem delegou pedir.

## Interfaces e contratos
- `scripts/verify.sh`, `scripts/heavy.sh`, `scripts/lint-workflows.sh`, `scripts/release-dryrun.sh`, `Makefile`, `.golangci.yml`, `.github/workflows/{quick,release,drift}.yml`, `deploy/heavy/` (templates e `run.sh`), `tests/sdk/` (`wwz.yaml`, `python/`, `node/`).

## Dados
- Relatórios de cobertura (`tmp/coverage/`), benchmark (`tmp/heavy/<carimbo>/`) e ensaio de release (`tmp/release/`) em `./tmp/` (fora do git).

## Critérios de aceite
- [ ] `make verify-quick` sem mudanças termina em segundos com tudo "pulado" ou ✓.
- [ ] Alterar um arquivo de `internal/scenario` roda só os testes de `scenario` e de quem depende dele.
- [ ] `release.yml` publica as duas arquiteturas (validado localmente por `make release-dryrun`; sem push, ADR 0018).
- [ ] `verify-full` falha com qualquer pacote abaixo de 100% de cobertura (com `VERIFY_COVERAGE_MIN` no padrão).
- [ ] `verify-full` sem `examples/` nem `testdata/real/` marca `fixtures` e `examples` como pulados e não falha por isso.
- [ ] `verify-full` deixa o disco como encontrou: nenhuma imagem `:verify`, nenhum container `wwz-verify-*` e nenhum tar em `tmp/release/` depois do fim, com sucesso ou falha.
- [ ] `verify-heavy` grava `tmp/heavy/<carimbo>/summary.json` com `throughput`, `streams` e `cluster` aprovados contra os limiares e não deixa recursos `wwz-heavy-*` nem imagens `:heavy-*` no servidor (salvo `HEAVY_KEEP_IMAGES=1`).
- [ ] `actionlint` sem erros nos três workflows.
- [ ] Com `testdata/real/` presente, o passo `fixtures` roda ao menos um teste `TestRealFixtures*` e falha se nenhum rodar.
- [ ] Com `testdata/real/claude-code/2.1.308/` presente, `TestRealFixturesClaudeCode` roda no passo `fixtures` e no `verify-quick` quando `internal/claudecode` ou `internal/driver/anthropic` muda
- [ ] `docker run --rm white-wizard-load:dev wwz-tools version` e `... wwz-drift version` funcionam; `wwz version` de um binário com `-X main.commit` mostra `(commit <commit>)`.

## Fora de escopo
- Hook de pre-commit.
- Benchmarks em CI hospedado.

## Decisões relacionadas
- [0008](decisions/0008-verificacao-e-ci.md), [0010](decisions/0010-versionamento-releases.md), [0013](decisions/0013-cobertura-total.md), [0014](decisions/0014-fidelidade-bit-a-bit.md), [0016](decisions/0016-diretorio-examples.md), [0017](decisions/0017-deploy-producao.md), [0018](decisions/0018-publicacao-local.md), [0034](decisions/0034-contrato-do-wwz-tools.md), [0035](decisions/0035-wwz-drift-comparacao-de-forma.md), [0037](decisions/0037-verify-full-e-heavy-concretos.md), [0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), [0075](decisions/0075-github-publico-e-imagens-tmsoftbrasil.md), [0076](decisions/0076-dados-do-autor-fora-do-git.md)

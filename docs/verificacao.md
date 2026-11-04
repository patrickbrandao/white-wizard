# Verificação, ensaio de release e fixtures reais

Os três níveis de verificação (`quick`, `full`, `heavy`), o ensaio local da release e o formato das fixtures reais. A referência normativa é a [spec 018](../specs/018-verificacao-ci-release.md), com as ADRs [0008](../specs/decisions/0008-verificacao-e-ci.md), [0013](../specs/decisions/0013-cobertura-total.md), [0014](../specs/decisions/0014-fidelidade-bit-a-bit.md), [0017](../specs/decisions/0017-deploy-producao.md), [0018](../specs/decisions/0018-publicacao-local.md) e [0037](../specs/decisions/0037-verify-full-e-heavy-concretos.md). Os scripts são [scripts/verify.sh](../scripts/verify.sh), [scripts/heavy.sh](../scripts/heavy.sh) e [scripts/release-dryrun.sh](../scripts/release-dryrun.sh), e o cabeçalho de cada um repete os passos.

| Comando | Quando | Duração |
| --- | --- | --- |
| `make verify-quick` (padrão; alias `make test-quick`) | Durante o desenvolvimento e ao concluir cada tarefa | Menos de 2 min |
| `make verify-full` (alias `make test-full`) | Sob pedido e antes de uma tag de release | Dezenas de minutos (imagens, SDKs) |
| `make verify-heavy` | Sob pedido, com o servidor pesado | Até ~10 min por rodada |
| `make release-dryrun` | Antes de uma release | Builds multi-arch |

Todo nível imprime no fim um resumo com ✓, ✗ ou `pulado (motivo)` e o tempo de cada passo. O código de saída é 0 só se nenhum passo falhou.

## verify-quick

Olha só o que mudou desde `git merge-base HEAD main` (commits da branch, alterações no working tree e arquivos novos). A base muda com `VERIFY_BASE` (o CI usa `origin/main`). Roda todos os passos, mesmo depois de uma falha.

| Passo | O que faz |
| --- | --- |
| `gofumpt` | `gofumpt -l` nos `.go` alterados |
| `lint` | `golangci-lint` nos pacotes alterados, só nas linhas novas (`--new-from-rev`) |
| `build` | `go build ./...` |
| `vet`, `test` | `go vet` e `go test` dos pacotes afetados: os alterados, os que os importam (fecho de `go list -deps` invertido) e os cujos *testes* importam um deles (o import de teste afeta só o próprio pacote, não propaga a quem o importa); mudança em `go.mod`/`go.sum` afeta todos |
| `drivertest` | Contrato dos drivers, quando `internal/driver/` ou `internal/core/` mudou |
| `valkey` | Quando algum pacote a testar usa o store: confere o Valkey local em `127.0.0.1:$VALKEY_PORT` (padrão 16379) e, fora do ar, sobe só o compose de `cache/valkey/`. Exporta `WWZ_TEST_VALKEY=local`, `WWZ_TEST_VALKEY_ADDR` e `WWZ_TEST_VALKEY_DB` (padrão 15; os testes fazem `FLUSHDB`) |
| `grafana` | `scripts/check-grafana.sh`, quando mudou `deploy/grafana/`, `internal/metrics/` ou código que cita uma métrica `wwz_*` |
| `selftest` | Os testes do próprio script, quando `scripts/verify.sh` mudou: escopo, passos "pulado" sem insumo, lista dos 16 `examples/`, gate de cobertura, limpeza do disco (com Docker, uma falha forçada em `images` conferida por `docker ps -a`/`docker images`), funções do `release-dryrun.sh`, `heavy_eval_load` e um `verify quick` sem mudanças num checkout limpo do `HEAD` (todo passo ✓ ou pulado, em segundos) |

Sem mudanças, termina em segundos com tudo `pulado` ou ✓. Alvos explícitos substituem o escopo de `vet`/`test`:

```sh
make verify-quick -- ./internal/scenario -run TestFailFirst
make verify-quick ARGS="./internal/scenario -run TestFailFirst"     # equivalente
scripts/verify.sh quick -- ./internal/scenario -run TestFailFirst    # direto
```

## verify-full

Passos em ordem fixa, parando no primeiro ✗ (os seguintes saem `pulado (não rodou)`). O `cleanup` e o resumo rodam sempre, por trap, inclusive com `^C`. Um passo cujo insumo ainda não existe no repositório sai `pulado (motivo)`; quando o insumo existe, o passo bloqueia.

| # | Passo | O que faz | Pulado quando |
| --- | --- | --- | --- |
| 1 | `preflight` | `go`, `gofumpt`, `golangci-lint`, `docker` (com daemon), `node`, `npm`, `curl`, `jq` e `$VERIFY_PYTHON` com pytest; disco livre ≥ `VERIFY_MIN_FREE_GB` (abaixo disso, roda `go clean -testcache` e `docker builder prune -f` e mede de novo) | nunca |
| 2 | `gofumpt`, `lint`, `vet`, `build` | `gofumpt -l .` vazio, `golangci-lint run ./...` completo, `go vet ./...`, `go build ./...` | nunca |
| 3 | `valkey` | `docker run --rm -d --name wwz-verify-valkey-<pid> -p 127.0.0.1::6379 valkey/valkey:<versão do .env>-alpine`; exporta `WWZ_TEST_VALKEY*` com a porta efêmera e o banco 15 | nunca |
| 4 | `test` | `go test -race -covermode=atomic -coverprofile=tmp/coverage/cover.out ./...`; depois `go test -covermode=atomic` só de `internal/driver/drivertest` (sem `-race`, que pula o sweep de mutações) e fusão dos perfis (máximo por bloco) em `cover.out` | nunca |
| 5 | `coverage` | Cobertura por pacote de `internal/` e `cmd/`; falha listando os pacotes abaixo de `VERIFY_COVERAGE_MIN`. Relatórios em `tmp/coverage/` (`summary.txt`, `cover.html`, `func.txt`) | nunca |
| 6 | `fixtures` | `go test -run '^TestRealFixtures' ./...`; falha se nenhum teste rodar | sem `testdata/real/` |
| 7 | `workflows` | [scripts/lint-workflows.sh](../scripts/lint-workflows.sh) (`actionlint` em container) | sem `.github/workflows/` ou sem Docker |
| 8 | `images` | `make image image-panel image-load` com tag `:verify` | nunca |
| 9 | `smoke` | [scripts/smoke.sh](../scripts/smoke.sh) com as imagens `:verify`, projeto `wwz-verify-smoke-<pid>` e portas livres | nunca |
| 10 | `sdk-python` | `bin/wwz serve --config tests/sdk/wwz.yaml` em portas livres, `WWZ_SDK_BASE_URL`/`WWZ_SDK_KEY` e `$VERIFY_PYTHON -m pytest -q tests/sdk/python` | nunca |
| 11 | `sdk-node` | `npm ci --prefer-offline` só se `node_modules` falta ou está mais velho que o lock; `node --test tests/sdk/node/` | nunca |
| 12 | `examples` | `examples/*/smoke.sh`, um por vez | sem `examples/` |
| 13 | `cleanup` | Para o wwz do SDK e o Valkey, derruba o smoke, remove as imagens `:verify` e roda `docker builder prune -f --filter until=24h`; falha se sobrar container `wwz-verify-*` ou imagem | nunca |

| Variável | Padrão | Efeito |
| --- | --- | --- |
| `VERIFY_COVERAGE_MIN` | `100` | Cobertura mínima por pacote. `0` gera o relatório sem o gate (saída da fase 4: `VERIFY_COVERAGE_MIN=0 make verify-full`) |
| `VERIFY_MIN_FREE_GB` | `1.5` | Disco livre mínimo do preflight |
| `VERIFY_PYTHON` | `tmp/venv/bin/python` | Python dos testes de SDK; nada é instalado fora de `tmp/venv` e `tests/sdk/node/node_modules` |
| `VERIFY_BASE` | `main` | Base do `quick` |
| `VERIFY_FAIL_AT=<passo>` | — | Força ✗ naquele passo (para testar o resumo e a limpeza) |
| `VALKEY_PORT`, `VALKEY_VERSION` | do `.env` | Porta do Valkey do `quick`; versão da imagem do `full` |

O `verify-full` deixa o disco como encontrou: nenhuma imagem `:verify`, nenhum container `wwz-verify-*` e nenhum `tmp/release/*.tar`, com sucesso ou falha.

O passo `examples` roda a lista inteira só se `examples/` tem exatamente os 16 diretórios da [ADR 0016](../specs/decisions/0016-diretorio-examples.md) (sem contar os que começam com `_`); com `examples/` ausente ou sem nenhum exemplo continua `pulado`.

**Cobertura do código só-Linux (decisão da task 391, [ADR 0013](../specs/decisions/0013-cobertura-total.md)).** O gate de cobertura roda no macOS, então `listen_linux.go`, `listen_linux_mipsfamily.go` e `watch_unix.go` de `internal/server` não entram nele pelo ramo Linux. Medi-los pede um container Go por execução (imagem `golang` de centenas de MiB, `go test -cover` com cache frio, num Mac de disco curto), custo que não compensa o ganho: o código é pequeno, o servidor de verificação é Linux e o `make smoke` o exercita nas imagens. Exceção registrada em vez de passo: o passo `vet` roda também `GOOS=linux go vet ./internal/server/...`, que garante que os arquivos só-Linux compilam e passam no vet. Gatilho de revisão: um defeito só-Linux que o macOS não pegue.

## verify-heavy

Benchmarks de vazão e de streams abertos ([spec 014](../specs/014-desempenho-e-escala.md)), Valkey Cluster e drift. É orquestrado do Mac por `scripts/verify.sh heavy` (que carrega `scripts/heavy.sh`). Os benchmarks rodam no servidor designado, pela rede interna dele. [deploy/heavy/](../deploy/heavy/) só tem templates, e as credenciais ficam no `.env` do servidor, fora do git. O drift roda no Mac, porque as chaves reais nunca vão ao servidor.

| # | Passo | O que faz |
| --- | --- | --- |
| 1 | `preflight` | Local: ferramentas, Docker, `HEAVY_ROUNDS` válido, disco (`VERIFY_MIN_FREE_GB`). Remoto, numa chamada `ssh`: load average < `HEAVY_MAX_LOAD`, ≥ 5 GiB livres em `/opt`, `docker`, `jq` e `timeout`; registra o `nproc` |
| 2 | `images` | `docker buildx build --platform linux/amd64 --load` de `wwz` e `wwz-load` com tag `:heavy-<sha7>`, `docker save \| gzip \| ssh … docker load` e remoção local |
| 3 | `store-test` | `go test -c` de `./internal/limits` para linux/amd64, copiado para `$WWZ_HEAVY_DIR/bin/` |
| 4 | `sync` | `deploy/heavy/` → `$WWZ_HEAVY_DIR` (nunca o `.env` do servidor) |
| 5a | `cluster` | Valkey Cluster com 3 mestres e `limits.test -test.run Valkey` com `WWZ_TEST_VALKEY=cluster` |
| 5b | `throughput` | 1 `wwz` (8 CPUs, latência zero) e `wwz-load --concurrency 512 --stream=false --warmup 10s --duration 60s`; passa com `total.ok_rps ≥ HEAVY_MIN_RPS`, erros < `HEAVY_MAX_ERROR_PCT` e overhead p99 (TTFT, com latência configurada zero) < `HEAVY_MAX_OVERHEAD_MS` |
| 5c | `streams` | 1 `wwz` (12 CPUs, ITL fixo de 20 ms) e `wwz-load` (14 CPUs) com `--concurrency $HEAVY_STREAMS --stream=true --warmup 15s --duration 120s`; o `wwz-admin` fica parado durante a rodada ([ADR 0067](../specs/decisions/0067-meta-de-streams-pelo-custo-medido.md)); passa com erros < `HEAVY_MAX_ERROR_PCT`, `total.itl_ms.p99 ≤ HEAVY_MAX_ITL_P99_MS`, overhead do ITL (p99 menos `HEAVY_CONFIGURED_ITL_MS`) < `HEAVY_MAX_OVERHEAD_MS` e CPU média do wwz ≤ `HEAVY_MAX_WWZ_CPU_PCT` % da dada; grava também o TTFT p99 (`ttft_overhead_p99_ms`, informativo: na saturação o wwz atrasa o TTFT, spec 014) e a memória do wwz por 10 000 streams (`mem_per_10k_streams_mb`, MiB da última amostra do `docker stats`) |
| 5d | `streams-max` | Informativa: a mesma configuração com `$HEAVY_STREAMS_MAX` streams; grava ITL, TTFT, tokens/s, `chunks_per_core_s` e CPU no `summary.json` e nunca reprova (só falha se não gerar `report.json`) |
| 6 | `collect` | `tmp/heavy/<UTC AAAAMMDDTHHMMSSZ>/summary.json` com limiares, medidas, CPU e memória por container, `checks` e `pass` por rodada |
| 7 | `drift` | No Mac: `wwz-drift check --real --output json --out tmp/heavy/<carimbo>/drift.json` ([wwz-drift.md](wwz-drift.md)); código 3 falha o passo |
| 8 | `cleanup` | Sempre: `run.sh cleanup` no servidor (projeto `wwz-heavy`, imagens `:heavy-*`, imagens base, `runs/`, `limits.test`) e imagens `:heavy-*` locais; falha se sobrar algo |

Cada rodada roda sob `timeout 600` (código 124), num projeto compose `wwz-heavy`. O `run.sh` recusa começar com load average ≥ 48 (código 4) ou com o perfil acima de 32 CPUs / 64 GB (código 2), e um monitor aborta a rodada (código 3) se o load average passar de 48. Cada rodada grava `report.json` (ou `limits.log`), `stats.jsonl`, `compose.log` e `meta.json`.

| Variável | Padrão | Efeito |
| --- | --- | --- |
| `WWZ_HEAVY_SSH` | sem padrão: vem de `autor/prod.env` (fora do git, ADR 0076; modelo `deploy/prod/autor.env.example`, ex.: `ssh -p 22 root@prod.example.com`) | Comando `ssh` até o servidor; sem ele o `preflight` falha |
| `WWZ_HEAVY_DIR` | `/opt/white-wizard/heavy` | Diretório no servidor |
| `HEAVY_ROUNDS` | `cluster,throughput,streams,streams-max` | Rodadas (nome desconhecido falha o preflight) |
| `HEAVY_MIN_RPS` | `10000` | Limiar de `ok_rps` da `throughput` |
| `HEAVY_STREAMS` | `9000` | Streams simultâneos da `streams` |
| `HEAVY_STREAMS_MAX` | `20000` | Streams simultâneos da `streams-max` (informativa) |
| `HEAVY_MAX_WWZ_CPU_PCT` | `80` | CPU média máxima do wwz na `streams`, em % da dada |
| `HEAVY_MAX_ITL_P99_MS` | `22` | ITL p99 máximo (20 ms + 2 ms de overhead) |
| `HEAVY_MAX_OVERHEAD_MS` | `2` | Overhead p99 máximo (observado menos configurado; desigualdade estrita; spec 014) |
| `HEAVY_CONFIGURED_ITL_MS` | `20` | ITL configurado na rodada `streams` (`deploy/heavy/rounds/streams.yaml`) |
| `HEAVY_MAX_MEM_PER_10K_MB` | `2048` | Pico de memória máximo do wwz por 10 000 streams, em MiB (ADR 0070; medido ~1 610). Vazio desliga a checagem |
| `HEAVY_MAX_ERROR_PCT` | `0.1` | % de erros aceito |
| `HEAVY_MAX_LOAD` | `48` | Load average máximo do servidor |
| `HEAVY_DRIFT` | `1` | `0` pula o drift (repetir rodadas sem gastar chamadas reais) |
| `HEAVY_KEEP_IMAGES` | `0` | `1` mantém no servidor as imagens e o `limits.test` desta execução |

No servidor, o `run.sh` aceita ainda `HEAVY_ROUND_TIMEOUT` (600), `HEAVY_MAX_CPUS` (32), `HEAVY_MAX_MEMORY_GB` (64), `HEAVY_WWZ_CPUS` (8), `HEAVY_LOAD_CPUS` (8), `HEAVY_STREAMS_WWZ_CPUS` (12), `HEAVY_STREAMS_LOAD_CPUS` (14) e `HEAVY_STREAMS_LOAD_MEMORY` (`24g`).

```sh
HEAVY_ROUNDS=throughput HEAVY_DRIFT=0 make verify-heavy        # só a vazão, sem drift
HEAVY_ROUNDS=streams HEAVY_STREAMS=5000 HEAVY_KEEP_IMAGES=1 make verify-heavy
```

## Ensaio da release (`make release-dryrun`)

[scripts/release-dryrun.sh](../scripts/release-dryrun.sh) reproduz localmente o que o `release.yml` faria, sem push e sem tag ([ADR 0018](../specs/decisions/0018-publicacao-local.md)):

1. Confere que o `CHANGELOG.md` tem `[Não lançado]` não vazio e que `wwz version` imprime a versão e o commit injetados por `-ldflags`.
2. Para cada alvo (`wwz`, `wwz-load`, `wwz-panel`), um por vez: `docker buildx build --platform linux/amd64,linux/arm64` num builder `docker-container` chamado `wwz-builder`, com saída OCI em `tmp/release/<alvo>.tar`. Confere no `index.json` os manifests das duas arquiteturas, grava os digests em `tmp/release/manifests.txt` e apaga o tar.
3. Compila os 20 binários (`wwz`, `wwz-load`, `wwz-panel`, `wwz-tools`, `wwz-drift` × `linux`/`darwin` × `amd64`/`arm64`) um por vez, com `GOCACHE` próprio em `tmp/release/gocache`, grava o `sha256` em `tmp/release/SHA256SUMS` e apaga cada binário.
4. R1, binário estático: cada um dos 20 binários tem de trazer `CGO_ENABLED=0` no `go version -m`, e o estágio final das três imagens no `Dockerfile` tem de ser `distroless/static` ou `scratch`.
5. Ao fim, com sucesso ou falha: `docker buildx prune -af --builder wwz-builder` e remoção de tars, binários e do `gocache`.

| Variável | Padrão | Efeito |
| --- | --- | --- |
| `VERSION` | `git describe --tags --always --dirty` | Versão injetada |
| `COMMIT` | `git rev-parse HEAD` | Commit injetado |
| `RELEASE_OUT` | `tmp/release` | Diretório de saída |
| `RELEASE_MIN_FREE_GB` | `3` | Recusa começar com menos disco livre |
| `RELEASE_TARGETS` | `wwz wwz-load wwz-panel` | Imagens do ensaio |
| `RELEASE_SKIP_IMAGES=1`, `RELEASE_SKIP_BINARIES=1` | — | Pulam uma das metades (resultado incompleto) |

As plataformas vêm de `env.PLATFORMS` em `.github/workflows/release.yml` (sem a chave o ensaio falha), então o ensaio nunca diverge do workflow. Sai ≠ 0 se faltar uma arquitetura, um binário, um binário dinâmico ou a seção do `CHANGELOG.md`. A tag `vX.Y.Z` e o push ficam com o mantenedor.

```sh
VERSION=v0.1.0 make release-dryrun
RELEASE_SKIP_IMAGES=1 make release-dryrun          # só os 20 binários e o SHA256SUMS
```

## Fixtures reais (`testdata/real/`)

As fixtures são respostas gravadas contra os provedores reais. São insumo do passo `fixtures` do `verify-full` e dos testes que rodam no `verify-quick` quando um driver muda ([ADR 0014](../specs/decisions/0014-fidelidade-bit-a-bit.md)). Hoje há o conjunto de validação, `testdata/real/<provedor>/validation/<nome>.json` (`openai`, `anthropic`, `openrouter`, `opencode`), e o de modelos do OpenRouter (`testdata/real/openrouter/models/`). Cada arquivo guarda uma requisição:

```json
{
  "provider": "anthropic",
  "name": "messages_nested_unknown",
  "recorded_at": "2026-10-31",
  "request": {"method": "POST", "path": "/v1/messages",
              "headers": {"x-api-key": "<key>", "anthropic-version": "2023-06-01"},
              "body": "{\"model\": \"claude-haiku-4-5\", ...}"},
  "response": {"status": 400, "content_type": "application/json",
               "body": "{\"type\":\"error\",\"error\":{...},\"request_id\":\"<request_id>\"}"}
}
```

- **Segredos:** credenciais viram marcadores (`Bearer <key>`, `<key>`, `<session>`), que o teste troca por uma chave e uma sessão válidas do wwz. Nenhuma chave real entra no repositório.
- **Normalização:** `"request_id":"req_…"` vira `"<request_id>"` e `"user_id":"user_…"` vira `"<user_id>"`. Requisição aceita guarda só `status: 200` e `body: null`.
- **Comparação:** sempre o status; com corpo, também o tipo de mídia e o corpo. Nos gateways (`openrouter`, `opencode`) é byte a byte. Em `openai` e `anthropic`, por enquanto, é por campos e valores do JSON decodificado, até a serialização desses drivers igualar a do real.
- **Nome dos testes:** todo teste que lê `testdata/real/` se chama `TestRealFixtures<Assunto>` (hoje `TestRealFixturesValidation`, em `internal/server`), para o passo `fixtures` achá-lo pelo `-run`.

```sh
go test -count=1 -run '^TestRealFixtures' ./internal/server
```

A gravação automática (`wwz-drift record`) é da fase 5. Até lá, as fixtures entram por tarefa, gravadas na máquina do mantenedor, dentro do orçamento de `tmp/real-spend.json` ([wwz-drift.md](wwz-drift.md#orçamento)).

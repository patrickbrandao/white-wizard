# 0071 — Overhead da throughput em malha aberta e meta de 7 000 streams no heavy

Status: provisória (ADR 0012, loop autônomo de 2026-10-31; revisar nos gatilhos abaixo)
Data: 2026-10-31
Atualiza: [0069](0069-pacing-pelo-fim-planejado-e-gogc.md), só no número de streams da rodada `streams` (8 000 passa a 7 000), e a spec 018 (passos 5b e 5c). Origem: task 409 (proposta em `tmp/spec-batch/rodada10-409.md`).

## Contexto
- `tmp/heavy/20261031T222429Z`: a `throughput`, com 51 460 req/s e 0 erros, reprovou no `overhead_p99` (task 391). O overhead era o e2e p99 da malha fechada de 512 conexões (54,9 ms), que na vazão máxima mede a fila, não o wwz. A spec 014 pede "≥ 10 000 req/s ... com latência zero e p99 de overhead próprio abaixo de 2 ms" e define o overhead como "diferença entre latência configurada e latência observada"; não manda medir na saturação.
- A `streams` com 8 000 streams deu o wwz a 0,82 (> 0,80). A média incluía a amostra do `docker stats` de antes de o wwz-load subir (0 %). Contando só as amostras sob carga, 8 000 streams ficam em ~0,84 (`20261031T192604Z` e `222429Z`). A ADR 0069 já previa: "se reprovar só por CPU, baixar `HEAVY_STREAMS` e registrar".

## Decisão
1. **Spec 018, passo 5b (`throughput`):** a malha fechada (`--concurrency 512`) continua cobrando `ok_rps ≥ HEAVY_MIN_RPS` e erros < `HEAVY_MAX_ERROR_PCT`. Depois dela, o mesmo wwz recebe uma segunda fase `--rps $HEAVY_OVERHEAD_RPS --stream=false --warmup 5s --duration 30s --timeout 30s`, gravada em `overhead.json`. `HEAVY_OVERHEAD_RPS` tem padrão `HEAVY_MIN_RPS` (10 000), a meta da spec 014.
   - Overhead p99 = `total.e2e_ms.p99` dessa fase − latência configurada (zero).
   - A fase passa com overhead < `HEAVY_MAX_OVERHEAD_MS` (2), `ok_rps ≥ 95 %` da taxa pedida, erros < `HEAVY_MAX_ERROR_PCT` e `schedule_lag_ms.p99 < HEAVY_MAX_SCHEDULE_LAG_MS` (padrão 1). Com o cliente atrasado, a medida não é do wwz.
   - Se o lag passar do limite, o `run.sh` repete a fase uma vez; a primeira tentativa fica em `overhead-1.json` e entra no `result.json` como `overhead_discarded`. Se o lag passar de novo, a rodada reprova.
   - O e2e p99 da malha fechada fica só informado.
2. **Specs 018 (passo 5c) e 014 (seção 11):** `wwz_cpu_util_mean` é a média das amostras do wwz feitas com o container do wwz-load de pé (mesmo carimbo `t` no `stats.jsonl`). A amostra de antes da carga não entra.
3. **`HEAVY_STREAMS` padrão 7 000** (≈ 350 mil chunks/s). Os limiares de CPU (80 %) e de ITL (22 ms) não mudam.
4. **Novas variáveis (spec 018):** `HEAVY_OVERHEAD_RPS` (padrão `HEAVY_MIN_RPS`) e `HEAVY_MAX_SCHEDULE_LAG_MS` (padrão `1`). `summary.json.thresholds` ganha `overhead_rps` e `max_overhead_schedule_lag_p99_ms`; `rounds.throughput` ganha o objeto `overhead` (`target_rps`, `requests`, `ok`, `errors`, `dropped`, `error_pct`, `rps`, `ok_rps`, `e2e_ms`, `schedule_lag_ms`) e os checks `overhead_rate`, `overhead_errors` e `overhead_lag`.

## Medidas (servidor, amd64)
| Carimbo | throughput ok_rps | e2e p99 na saturação | overhead p99 a 10 000 req/s | lag p99 | streams | ITL p99 | CPU do wwz |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 20261031T232121Z | 51 396 | 53,9 ms | 58,8 ms (cliente travou, lag máx. 277 ms; motivou o check de lag e a repetição) | 24,9 ms | 7 000 | 21,70 ms | 0,75 |
| 20261031T232622Z | 51 527 | 53,4 ms | 0,557 ms | 0,27 ms | — | — | — |
| 20261031T232835Z | 51 373 | 53,9 ms | 1,075 ms | 0,48 ms | — | — | — |
| 20261031T233039Z | 51 686 | 52,4 ms | 0,549 ms | 0,28 ms | — | — | — |
| 20261031T233243Z | 51 790 | 52,1 ms | 0,553 ms | 0,28 ms | — | — | — |
| 20261031T233559Z (final) | 52 002 | 51,1 ms | 1,021 ms | 0,41 ms | 7 000 | 21,70 ms | 0,76 |

## Alternativas consideradas
- Medir pela métrica `wwz_overhead_seconds`: mede só o engine, sem fasthttp nem rede; mais estreito que a spec 014.
- Tirar o check da rodada de vazão máxima: a meta da spec 014 ficaria sem verificação.
- 13 núcleos ao wwz e 13 ao load: 8 000 streams em ~0,78, sem margem, e o cliente já usa ~10 núcleos.
- 7 500 streams: estimativa de ~0,79–0,80 sob carga, sem margem.

## Consequências
- A rodada `throughput` ganha ~45 s; o `result.json` carrega a tentativa descartada, se houver.
- A meta de streams cai de 8 000 para 7 000 (20 000 continua como capacidade em `streams-max`).

## Gatilho de revisão
- O `overhead_lag` reprovar de forma recorrente (cliente sem folga a 10 000 req/s).
- O custo por chunk cair (rede `host`, kernel) a ponto de 8 000 streams voltarem a ≤ 80 %.

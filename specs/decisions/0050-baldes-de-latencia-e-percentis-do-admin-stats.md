# 0050 — Baldes padrão de latência mais finos; percentis do `/admin/stats` sem métrica de máximo

Status: provisória
Data: 2026-10-31
Gatilho de revisão: o erro do p99 do `/admin/stats` contra o máximo ou o p99 medido pelo `wwz-load` passar de um balde (≈ 50 %) numa carga real, ou a memória/cardinalidade dos histogramas com os baldes novos aparecer no perfil do `verify-heavy`.

## Contexto
A task 146 passou a interpolar os percentis de `/admin/stats` dentro do balde (como o `histogram_quantile` do Prometheus) e a limitá-los por um teto do maior valor observado derivado de `_sum` (`maxBound` em `internal/admin/stats.go`). Com os baldes padrão (`metrics.DefaultBuckets`: … 0,5; 1; 2,5; 5 s …) o p99 de E2E saiu ~2 447 ms numa rodada cujo máximo real foi 1 412 ms: a observação cai no balde (1; 2,5] s, a interpolação anda até perto de 2,5 s e o teto de `_sum` é frouxo quando há muitas observações pequenas. O erro é do tamanho do balde, não da fórmula.

## Decisão
- `metrics.DefaultBuckets` (padrão de `metrics.buckets` e dos histogramas de latência do `wwz` e do `wwz-tools`) passa a seguir a série 1–1,5–2–3–5–7,5 por década de 1 ms a 10 s, mais a cauda: `0.001, 0.0015, 0.002, 0.003, 0.005, 0.0075, 0.01, 0.015, 0.02, 0.03, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 1.5, 2, 3, 5, 7.5, 10, 15, 20, 30, 60` (29 limites). Cada balde até 10 s tem no máximo 67 % de largura relativa; no exemplo acima o p99 fica em (1; 1,5] s.
- Não entra métrica de máximo (`wwz_e2e_seconds_max` e afins): gauge de máximo não é agregável entre instâncias nem por janela no Prometheus, dobraria as séries por `driver`/`model` sob `metrics.max_series` e não serve ao Grafana, que usa `histogram_quantile`. O máximo exato fica no `wwz-load` (spec 015).
- `/admin/stats` mantém a interpolação e o teto de `_sum`; a spec 013 documenta que o percentil é estimativa com erro de até um balde.
- `metrics.buckets` continua configurável e lido só no boot; quem fixou baldes no YAML não muda.

## Alternativas consideradas
- Só a métrica de máximo, mantendo os baldes: corrige o p99 só quando ele é o máximo e não melhora p50/p90 nem o Grafana.
- Histogramas nativos (esparsos) do Prometheus: exigem exposição protobuf, fora do registry próprio do `internal/metrics`.
- Baldes ainda mais finos (1–1,25–1,5–2–2,5–3–4–5–7,5): mais séries por histograma sem ganho prático sobre a meta de "um balde".

## Consequências
- Os rótulos `le` das séries de latência mudam (de 15 para 29 limites): consultas `histogram_quantile` e o painel Grafana seguem valendo; regras que citem um `le` específico (ex.: `le="2.5"`) precisam ser revistas. Dos limites antigos continuam 0,001, 0,005, 0,01, 0,05, 0,1, 0,5, 1, 5, 10, 30 e 60; saem 0,0025, 0,025, 0,25 e 2,5.
- Cada série de histograma ocupa ~2× a memória e as linhas do `/metrics` (29 + 3 contra 15 + 3).
- Tarefa de código: `internal/metrics` (novo `DefaultBuckets`), testes de `internal/admin` com o caso do p99 (máximo 1 412 ms → p99 ≤ 1 500 ms), revisão de `deploy/prometheus/` e `deploy/grafana/` por `le` fixo.

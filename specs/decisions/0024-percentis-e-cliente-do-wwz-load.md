# 0024 — Percentis e cliente HTTP do wwz-load sem dependências novas

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31
Substitui: menção a t-digest na [spec 014](../014-desempenho-e-escala.md) ("Gerador de carga")

## Contexto
A spec 014 diz que o controlador do `wwz-load --instances` agrega os percentis por t-digest. O t-digest é aproximado também na **mesclagem**: o p99 agregado depende da ordem e da compressão de cada worker, e exigiria uma dependência nova. O disco de desenvolvimento é curto e o `go.mod` já tem `valyala/fasthttp`, que lê o corpo em streaming (`Client.StreamResponseBody`).

Fato novo: com histograma de baldes fixos, a mesclagem é soma de contadores, logo o percentil agregado entre instâncias é **idêntico** ao de um histograma único com as mesmas amostras — o que permite um critério de aceite exato (spec 015) que o t-digest não permite.

## Decisão
- **Cliente HTTP:** `github.com/valyala/fasthttp` (já no go.mod) com `StreamResponseBody = true` e leitura incremental do `BodyStream`; HTTP/1.1 com keep-alive. Controlador/worker em `net/http`.
- **Percentis:** histograma log-linear próprio em `internal/loadgen/hist`: valores em microssegundos de 1 µs a 1 h; cada potência de 2 dividida em 128 sub-baldes lineares (erro relativo ≤ 1/128 < 1 %); guarda também contagem, soma, mínimo e máximo exatos. `Record`, `Merge`, `Quantile`, `Count`, `Mean`, `Max` e serialização JSON versionada e esparsa (só baldes não vazios). Histogramas por shard — `min(GOMAXPROCS, 16, executores)` shards por execução, cada executor gravando no shard `id mod shards` sob um mutex curto (centenas de ns) — e merge dos shards no fim (revisão de 2026-10-31, abaixo).
- Sem nenhuma dependência nova para o `wwz-load` na fase 3.

## Alternativas consideradas
- t-digest (`influxdata/tdigest` ou similar): dependência nova e mesclagem aproximada.
- HdrHistogram-go: mesma ideia, mas dependência a mais para ~200 linhas de código.
- `net/http` como cliente de carga: mais alocação por requisição; limita a taxa por máquina.

## Consequências
- Spec 014 passa a citar o histograma log-linear; spec 015 fixa o critério de agregação exata.
- Os relatórios de várias instâncias são comparáveis bit a bit com os de uma instância só.

## Revisão de 2026-10-31 (task 043)
Fato novo: o texto original previa um histograma por worker/goroutine, sem lock. Com `--concurrency` de 10 000 executores, cada um com os histogramas de TTFT, ITL e E2E de cada passo (cada histograma aloca ~3 300 baldes de 8 bytes, ~26 KiB, no primeiro `Record`), isso custa gigabytes de memória só em contadores, e o merge final cresce com o número de goroutines. A implementação da task 043 (`internal/loadgen/runner.go`) usa poucos shards (`min(GOMAXPROCS, 16, executores)`) protegidos por `sync.Mutex`; a seção crítica é só o `Record` (centenas de ns), quase nunca disputada com ≤ GOMAXPROCS shards.

Consequências: a memória dos histogramas fica limitada a 16 conjuntos por passo, independente da concorrência; a agregação continua exata (merge = soma de contadores), então o critério de aceite da spec 015 não muda. Gatilho de revisão adicional: se um perfil mostrar disputa relevante nos mutexes dos shards (ex.: `sync.Mutex` acima de 5 % do CPU do `wwz-load` no `--rps` máximo), aumentar o número de shards ou voltar a histogramas por P.

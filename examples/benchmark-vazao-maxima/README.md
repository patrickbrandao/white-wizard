# benchmark-vazao-maxima

Configuração para procurar o teto de req/s do `wwz` (ou de um roteador na frente
dele): latência zero, nenhum limite, nenhum erro, resposta mínima e log quase mudo.
O que sobra de tempo por requisição é o do servidor, não o do modelo simulado.

## O que demonstra

- `profiles.latency.zero: { ttft: 0ms, itl: 0ms }`: nenhuma espera simulada.
- Chave sem `limits` (0 = sem limite de rpm, tpm nem concorrência) e
  `server.max_rps: 0` (sem proteção global).
- `errors: none` (`rate: 0`) e geração mínima: 16 tokens, sem raciocínio, sem tool calls.
- `reuseport: true` (um listener por CPU), `metrics.enabled: false` e
  `logging.level: error`: menos trabalho fora do caminho da resposta.
- O `wwz-load` do perfil de vazão (malha fechada: N conexões, cada uma envia a
  próxima requisição ao terminar a anterior) e o relatório JSON da spec 015.

## Como subir

```sh
make image image-load                             # white-wizard:dev e white-wizard-load:dev
cd examples/benchmark-vazao-maxima
EX_PORT=18115 docker compose up -d --wait         # só o wwz
docker compose run --rm -T wwz-load > tmp.json    # 10 s de carga, JSON no stdout
jq '.total | {requests, errors, rps}' tmp.json
docker compose down -v
```

Ajustes por variável: `WWZ_LOAD_CONCURRENCY` (padrão 64), `WWZ_LOAD_DURATION`
(10s), `WWZ_LOAD_WARMUP` (1s), `WWZ_CPUS` (2) e `WWZ_LOAD_CPUS` (2). Para medir um
roteador, aponte o `wwz-load` para ele com `--target` e deixe este `wwz` como origem.
Porta do host do `wwz`: `EX_PORT` (padrão 18115). Chave: `wwz-key-01`.

## O que esperar

O relatório traz `total.requests`, `total.rps`, `total.errors = 0` e os percentis
de `ttft_ms`/`e2e_ms`. Neste exemplo, no Docker Desktop do Mac com 2 CPUs para o
`wwz` e 2 para o `wwz-load`, a rodada de 10 s passou de 18 mil req/s; esse
número muda muito com a máquina e **não é meta**. Em Linux dedicado, aumente
`WWZ_LOAD_CONCURRENCY` e os CPUs. Se `dropped` ficar acima de zero ou o `wwz-load`
saturar antes do `wwz`, o gargalo é o cliente: use `controller`/`worker`
(`--instances`, spec 015).

## Smoke

`make example NAME=benchmark-vazao-maxima` (ou `./smoke.sh`) sobe o `wwz`, roda o
`wwz-load` por **5 s** (o objetivo é mostrar a configuração, não medir o teto) e
confere:

| Conferência | Faixa | Por quê |
| --- | --- | --- |
| JSON no esquema da spec 015 | `version: 1`, `total` e `ttft_ms` com os campos | o relatório é o contrato |
| erros | exatamente 0 | `rate: 0` e sem limites: nada pode falhar |
| requisições | maior que 0 | houve carga |
| vazão (req/s) | maior que 0 | só é registrada, sem meta numérica |
| `admin_stats` | presente | o `wwz` consultado confirma a carga |

Ao fim derruba o compose com `down -v`, inclusive em falha.

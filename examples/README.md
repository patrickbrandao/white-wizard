# examples/

Configurações prontas para reproduzir situações de teste de roteadores (spec
[019](../specs/019-exemplos-e-producao.md), [ADR 0016](../specs/decisions/0016-diretorio-examples.md)).
Cada exemplo sobe com `docker compose`, tem um smoke automatizado e entra no
passo `examples` do `make verify-full`.

## Estrutura de um exemplo

```
examples/<nome>/
  config.yaml          configuração do wwz (validada com `wwz validate`)
  docker-compose.yml   sobe só o que o exemplo precisa
  README.md            o que demonstra, como rodar à mão, o que o smoke confere
  smoke.sh             sobe, exercita, confere e derruba (`down -v`)
```

Os quatro arquivos são obrigatórios; o passo `examples` falha listando o que
faltar, antes de subir qualquer exemplo. Diretórios que começam com `_` não são
exemplos (`_lib.sh` mora aqui).

## Convenções

- **Projeto compose**: `wwz-ex-<nome>` (`docker compose -p`), para não colidir com
  o `smoke` (`wwz-verify-smoke-*`) nem com os outros exemplos.
- **Porta**: `18100 + índice`, o índice sendo a posição (a partir de 0) do exemplo
  na tabela da spec 019. O smoke lê a porta em `EX_PORT` (exportada por `ex_init`)
  e o `docker-compose.yml` a publica em `127.0.0.1:${EX_PORT}`. Depois dos 16 da
  tabela vem `opencode-completo` (índice 16, porta 18116: catálogos reais do
  opencode.ai, planos Go, gratuitos e budgets; ADR 0072, item 18) e depois
  `claude-code-assinatura` (índice 17, porta 18117: assinatura OAuth do Claude Code e
  modo estrito `client_verification: claude-code`; ADR 0074, item 17). Exemplos com mais de
  uma porta (cluster, multi-provedor) usam `EX_PORT`, `EX_PORT + 1000`, ...
- **Imagem**: `${WWZ_IMAGE}`; a biblioteca escolhe `white-wizard:verify` se existir
  (o `verify-full` a constrói), senão `white-wizard:dev` (`make image`).
- **Container**: `read_only`, `/run` em tmpfs, portas > 1024, limites de CPU/RAM,
  como no `docker-compose.yml` da raiz. Nada de segredo: chaves `wwz-key-*` de teste.
- **Limpeza**: `ex_init` registra `trap EXIT` que roda `down -v --remove-orphans`,
  inclusive quando o smoke falha ou é interrompido.
- **Margem estatística**: smoke que depende de sorteio usa `seed` fixo na
  configuração, pelo menos 200 requisições e uma faixa de aceite de pelo menos
  4 sigma (binomial: `n*p ± 4*sqrt(n*p*(1-p))`). Ex.: 20 % de erro em 400
  requisições é 80 ± 32, ou seja, aceitar 48 a 112. Com `seed` fixo o resultado é
  reprodutível; a margem cobre diferenças de escalonamento.
- **Shell**: bash 3.2 (macOS) e Linux; exige `curl`, `jq` e `docker compose`.

## Escrevendo um smoke.sh

```bash
#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../_lib.sh"

ex_init falhas-20pct 2          # nome (igual ao diretório) e índice da tabela
ex_up                            # compose up -d --wait; derrubado no EXIT
# ex_up --wait já espera o healthcheck do container (o data plane não tem /healthz)

ex_count_codes 400 ex_post_chat "$EX_PORT" wwz-key-demo
ex_assert_between "$(ex_hist 200)" 48 112 "respostas 200"
```

| Função | O que faz |
| --- | --- |
| `ex_init <nome> <índice>` | exige curl/jq/docker; define `EX_NAME`, `EX_PROJECT`, `EX_PORT`, `EX_DIR`, `WWZ_IMAGE`; registra o `trap EXIT` |
| `ex_up` | `docker compose -p wwz-ex-<nome> up -d --wait` (timeout `EX_UP_TIMEOUT`, 90 s); em falha mostra os logs |
| `ex_down` | `down -v --remove-orphans`; chamado pelo trap, não precisa chamar |
| `ex_wait_http <url> [s]` | espera um 2xx; falha no timeout |
| `ex_post_chat <porta> <chave> [corpo] [caminho]` | POST (padrão OpenAI `/v1/chat/completions`); preenche `EX_HTTP_CODE` e `EX_HTTP_BODY` |
| `ex_count_codes <n> <cmd...>` | roda `cmd` n vezes, conta `EX_HTTP_CODE`; `ex_hist <código>` lê a contagem |
| `ex_assert_between <v> <min> <max> <rótulo>` | falha fora da faixa (inclusiva) |
| `ex_compose <args>` | `docker compose` do exemplo, para `logs`, `exec`, `stop`/`start` |
| `ex_fail <msg>` | mensagem em stderr e saída 1 (o trap derruba o compose) |

Para outro dialeto, passe o corpo e o caminho: `ex_post_chat "$EX_PORT" wwz-key-demo
"$body" /v1/messages` (os cabeçalhos `x-api-key`/`anthropic-version` são do smoke,
via `curl` próprio, quando o driver exigir). Para medir latência ou vazão use o
`wwz-load` do compose do exemplo. O `claude-code-assinatura` roda o
`wwz-load claude-code` no host (`WWZ_LOAD_BIN`, ou compilado pelo smoke com Go): o
smoke lê o token do estado e a fixture de `testdata/real/claude-code/`, e o modo
estrito exige a porta publicada direto, sem proxy.

## Rodando

```bash
make image                                # white-wizard:dev (uma vez)
make example NAME=falhas-20pct           # um exemplo: valida o config.yaml e roda o smoke
make examples                            # todos, em sequência
scripts/verify.sh examples falhas-20pct  # equivalente, direto
make verify-full                         # inclui o passo examples
```

À mão, sem o smoke: `EX_PORT=18102 WWZ_IMAGE=white-wizard:dev docker compose -p wwz-ex-falhas-20pct up -d`
dentro do diretório do exemplo; pare com `down -v`.

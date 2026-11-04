# 0037 — `verify-full` e `verify-heavy` concretos

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A spec 018 lista o que o `verify-full` e o `verify-heavy` cobrem, mas sem ordem, sem regra de "pulado" e sem orquestração. Fatos novos:
- O Mac de desenvolvimento tem ~2 GiB livres. Imagens multi-arch, `node_modules` e o módulo `testcontainers-go` (dezenas de dependências) não cabem juntos.
- O PLAN pede `verify-full` verde na saída da fase 4, mas `examples/` e `testdata/real/` só nascem na fase 5. Além disso, a cobertura de 100% ([ADR 0013](0013-cobertura-total.md)) fecha antes da saída da última fase, depois da funcionalidade.
- As chaves reais não podem ir ao servidor ([ADR 0014](0014-fidelidade-bit-a-bit.md)), e por isso o drift sai do servidor ([ADR 0035](0035-wwz-drift-comparacao-de-forma.md)).

## Decisão
- **`verify-full`**: passos em ordem fixa (spec 018), parando na primeira falha. Um passo cujo insumo **ainda não existe no repositório** (`examples/`, `testdata/real/`) sai como "pulado (motivo)". Assim que o diretório existe, o passo roda e bloqueia.
- **Valkey isolado sem `testcontainers-go`**: o script sobe um container `valkey` descartável (`docker run --rm`, porta efêmera em 127.0.0.1) e exporta as variáveis `WWZ_TEST_VALKEY*` que os testes já usam. Isso substitui a menção a testcontainers-go da spec 018.
- **Cobertura**: limite `VERIFY_COVERAGE_MIN`, com padrão 100 (a ADR 0013 fica intacta). A saída da fase 4 roda `VERIFY_COVERAGE_MIN=0 make verify-full`, que gera o relatório e o anexa; a saída da fase 5 exige o padrão.
- **Disco**: pré-checagem `VERIFY_MIN_FREE_GB` (padrão 1,5). Abaixo disso, o script roda `go clean -testcache` e `docker builder prune -f` e confere de novo antes de falhar. Imagens e artefatos de release são apagados ao fim de cada passo.
- **`verify-heavy`** é orquestrado do Mac: constrói as imagens amd64, envia por `docker save | ssh … docker load` e copia `deploy/heavy/` e o binário de teste do store compilado para linux/amd64. Em seguida roda as rodadas no servidor (projeto `wwz-heavy`, ≤ 10 min cada, aborta com load average > 48, teto de 32 CPUs/64 GB) e traz os relatórios para `tmp/heavy/<carimbo>/`. O passo `drift` roda localmente.
- **Workflows** validados por `actionlint` em container (`scripts/lint-workflows.sh`), como passo do `verify-full`.

## Alternativas consideradas
- `testcontainers-go`: o mesmo isolamento, com muito mais módulo e disco.
- Exigir 100% de cobertura já na saída da fase 4: inverte a ordem da ADR 0013 (funcionalidade antes de cobertura).
- Rodar o heavy inteiro pelo `ssh` com o repositório clonado no servidor: exige Go e o código-fonte no servidor, fora do que a ADR 0017 permite.

## Consequências
- Spec 018 e PLAN (critério de saída da fase 4) atualizados.
- Tarefas de infra-ci: `verify full`, `verify heavy` + `deploy/heavy/`, `release-dryrun` e workflows.

# 0008 — Níveis de verificação, ferramentas e CI

Status: provisória (revisar se uma incompatibilidade com SDK oficial chegar à main, ou se verify-full ficar abaixo de 10 min)
Data: 2026-10-30

## Contexto
A seção 23 tinha `make test-quick`/`test-full` e CI só com o quick; o padrão do projeto pede `verify:quick/full` e CI com o full.

## Decisão
- `scripts/verify.sh quick|full|heavy` → `make verify-quick`, `verify-full`, `verify-heavy`; `test-quick`/`test-full` como aliases. Detalhe em [018](../018-verificacao-ci-release.md).
- `verify-heavy` separa do full os benchmarks de vazão, Valkey Cluster e drift, que exigem o servidor pesado (provisório: proposto pelo agente na geração das specs).
- CI roda só `verify-quick` em push/PR, como na seção 23; full e heavy só sob pedido.
- gofumpt + golangci-lint (`govet`, `staticcheck`, `errcheck`, `ineffassign`, `revive`, `gosec`) em `.golangci.yml`; sem hook de pre-commit.
- ~~Cobertura alvo 70% por pacote em `internal/`, relatada no full, não bloqueante.~~ Substituído pela [0013](0013-cobertura-total.md): 100% bloqueante.

## Alternativas consideradas
- CI com o full sem benchmarks (recomendado na entrevista, recusado): pega incompatibilidade de SDK antes da main, CI mais lento.
- gofmt + go vet apenas; 80% bloqueante.

## Consequências
- Risco aceito: incompatibilidade com SDK pode chegar à main; rodar `verify-full` antes de cada tag.

# 0013 — Cobertura de 100% de statements

Status: aceita
Data: 2026-10-30

## Contexto
A [ADR 0008](0008-verificacao-e-ci.md) fixava cobertura alvo de 70% por pacote, não bloqueante.

## Decisão
- Cobertura de 100% de statements em `internal/` e `cmd/`, **bloqueante** no `verify-full` (substitui a meta de 70% da 0008).
- Código inalcançável é refatorado para ficar testável (injeção de dependência, interfaces, relógio e rede injetáveis), nunca excluído artificialmente (sem build tags ou filtros de cobertura para esconder código).
- Ordem de trabalho: funcionalidade antes de cobertura; a cobertura fecha antes do critério de saída da última fase do PLAN.

## Alternativas consideradas
- 70% não bloqueante (0008); 90% bloqueante.

## Consequências
- `verify-full` falha se algum pacote ficar abaixo de 100%; o relatório fica em `tmp/coverage/`.

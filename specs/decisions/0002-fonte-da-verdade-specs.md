# 0002 — Fonte da verdade em ./specs/ e registro de decisões em ADRs

Status: aceita
Data: 2026-10-30

## Contexto
A especificação vivia num documento Claude Docs (https://claude.ai/artifact/HruN1zX52ygBiNCC7CUjbK, rev 75) que agentes nem sempre conseguem ler, e a seção 23 previa `docs/decisions.md` para dúvidas.

## Decisão
- `./specs/` passa a ser a fonte da verdade, migrada do documento seção por seção, sem resumir requisitos.
- O documento Claude Docs fica congelado como histórico, com aviso no topo.
- Decisões ficam em `./specs/decisions/` (ADRs `nnnn-titulo.md` + `HISTORY.md`); `docs/decisions.md` não existe.
- `./docs/` é derivada das specs e nunca é fonte de verdade.

## Alternativas consideradas
- Docs como mestre e `./specs/` como espelho: risco de divergência.
- Arquivo único `docs/decisions.md`: mistura documentação derivada com registro de decisões.

## Consequências
- Mudanças de comportamento começam por PR/commit em `./specs/`.
- Três diagramas do documento foram redesenhados em Mermaid; o do roadmap foi trocado por uma nota (o roadmap foi substituído pelo MVP completo).

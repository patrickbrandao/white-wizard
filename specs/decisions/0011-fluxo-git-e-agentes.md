# 0011 — Fluxo git, papéis e escalonamento dos agentes

Status: aceita (escalonamento substituído pela [0012](0012-execucao-autonoma-noturna.md))
Data: 2026-10-30

## Contexto
A seção 23 previa commits direto na main, mensagens `área: resumo` e um agente que nunca para para perguntar. Com vários agentes em paralelo isso gera conflitos.

## Decisão
- Worktree e branch `task/nnn-slug` por tarefa; `verify-quick`, rebase e merge fast-forward na main, sem PR obrigatório.
- Conventional Commits `tipo(área): resumo (task nnn)`.
- Papéis: guardião das specs, implementador, testador/revisor, infra e CI ([002](../002-organizacao-do-trabalho.md)).
- Fila de trabalho em `./tasks/` (fora do git).
- Escalonar ao humano só em: conflito com spec/ADR aceita, mudança incompatível de contrato público, ação irreversível (apagar dados, reescrever histórico, push forçado, tag ou imagem versionada), segredo necessário, ou "modo entrevista". O resto: ADR provisória e segue.

## Alternativas consideradas
- Direto na main; branch + PR; nunca escalonar.

## Consequências
- Substitui as linhas "Fluxo de trabalho do agente" e "Modo entrevista" da seção 23.

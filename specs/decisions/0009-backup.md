# 0009 — Backup só do SQLite do painel

Status: aceita
Data: 2026-10-30

## Contexto
O Valkey guarda contadores, bloqueios e banimentos de teste; o painel guarda eventos, usuários e auditoria em SQLite.

## Decisão
- Valkey é descartável: sem persistência (AOF/RDB desligados) e sem backup.
- SQLite do painel em volume; `wwz-panel backup` gera snapshot diário com `VACUUM INTO`, retém 7 dias; restauração coberta no verify-full.

## Alternativas consideradas
- Nada; Valkey também com AOF/RDB.

## Consequências
- Reiniciar o cluster zera bloqueios e banimentos; a auditoria de operador sobrevive.

# 0044 — SQLite do painel com modernc.org/sqlite

Status: aceita
Data: 2026-10-31

## Contexto
A [ADR 0023](0023-dependencias-do-painel.md) escolheu `ncruces/go-sqlite3` por causa do disco curto. Com 47 GiB livres, o humano preferiu a biblioteca mais madura.

## Decisão
- Painel usa `modernc.org/sqlite` (sem CGO), mantendo `VACUUM INTO`, WAL, `PRAGMA user_version`, backup e restauração como estão especificados.
- O resto da ADR 0023 (argon2id, go-oidc, HTMX vendorizado, stdlib) continua.

## Alternativas consideradas
- Manter `ncruces/go-sqlite3` (recomendação do agente; recusada).

## Consequências
- Tarefa 166; cobertura e testes de backup/restauração refeitos sobre o novo driver.

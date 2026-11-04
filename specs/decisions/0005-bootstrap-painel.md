# 0005 — Bootstrap do primeiro operador do painel por env

Status: aceita
Data: 2026-10-30

## Contexto
O painel tem login local (argon2) ou OIDC com papéis `viewer`/`operator`, mas não definia como nasce o primeiro operador.

## Decisão
Com a tabela de usuários vazia, `WWZ_PANEL_ADMIN_USER` e `WWZ_PANEL_ADMIN_PASSWORD` criam um `operator`. Sem senha, gera uma aleatória de 24 caracteres e a imprime uma única vez no log. Com usuários existentes, as variáveis são ignoradas.

## Alternativas consideradas
- Comando `wwz-panel user add`; só OIDC.

## Consequências
- `WWZ_PANEL_ADMIN_PASSWORD` sem default no `.env`.

# 0004 — Autenticação do control plane por bearer token

Status: aceita
Data: 2026-10-30

## Contexto
A seção 10 deixava `/admin/*` sem autenticação "em rede confiável"; qualquer um na rede poderia derrubar ou alterar um teste.

## Decisão
- `/admin/*` exige `Authorization: Bearer <WWZ_ADMIN_TOKEN>`; resposta 401 sem token válido; comparação em tempo constante.
- `/metrics`, `/health` e `/health/ready` ficam abertos.
- Sem `WWZ_ADMIN_TOKEN` configurado, o control plane só escuta em `127.0.0.1` e registra um aviso.
- O painel usa o mesmo token para chamar a API admin.

## Alternativas consideradas
- Sem auth, só rede; vários tokens com papéis.

## Consequências
- `WWZ_ADMIN_TOKEN` não tem default no `.env`.

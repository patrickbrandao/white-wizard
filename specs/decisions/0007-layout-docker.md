# 0007 — Layout Docker por sistema

Status: aceita
Data: 2026-10-30

## Contexto
A seção 23 previa `deploy/local/docker-compose.yml`; o padrão do projeto pede um compose por sistema.

## Decisão
- `./cache/valkey/` (schema de chaves + `docker-compose.yml`), `./eventbus/valkey/` (`asyncapi.yaml`), `./deploy/prometheus/`, `./deploy/grafana/`, `./deploy/heavy/` (templates).
- `./docker-compose.yml` raiz inclui todos (`include:`) e define o serviço `wwz`.
- `.env` com defaults, exceto segredos (`${WWZ_ADMIN_TOKEN:?}`).
- Containers com `read_only: true`, `/run` em tmpfs, escrita só em volumes, limites de CPU (1 núcleo) e RAM configuráveis no `.env`, `ulimits.nofile`, portas publicadas > 1024.
- Dockerfile multi-stage com dependências antes do código.

## Alternativas consideradas
- Compose único em `deploy/local/`.

## Consequências
- `deploy/local/` não existe.

# 0006 — Contratos OpenAPI só para APIs próprias

Status: aceita
Data: 2026-10-30

## Contexto
O padrão do projeto exige `openapi.yaml` (OpenAPI 3.1) na raiz de cada aplicação que expõe API HTTP.

## Decisão
- `cmd/wwz/openapi.yaml`: control plane (`/admin/*`, `/metrics`, `/health*`).
- `cmd/wwz-panel/openapi.yaml`: `/api/v1/*` do painel.
- Endpoints de inferência imitam provedores e apontam para os contratos oficiais, sem cópia.
- `eventbus/valkey/asyncapi.yaml` descreve `ev:stream` e `cfg:changed`.

## Alternativas consideradas
- Descrever também as rotas OpenAI/Anthropic/OpenRouter/OpenCode emuladas: volume alto e risco de divergir.

## Consequências
- Mudança de rota admin começa pelo openapi.yaml.

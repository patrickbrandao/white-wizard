# 0015 — `wwz-load agents` e `wwz-load top`

Status: aceita
Data: 2026-10-30

## Contexto
O `wwz-load` ([015](../015-wwz-load.md)) gera carga por taxa ou concorrência, mas não reproduz agentes reais conversando em vários turnos, nem há visão ao vivo do servidor.

## Decisão
- `wwz-load agents`: N agentes concorrentes multi-turno, perfis `coder`, `pesquisa` e `chat`; tool calls recebidas são executadas via `wwz-tools` (sandbox) e o resultado volta como `tool_result`; retries configuráveis (`--retries`, backoff, respeito a `retry-after`); alvo é o wwz ou um roteador (`--target`); relatório por agente e agregado (turnos, tool calls, erros por tipo, TTFT/E2E).
- `wwz-load top`: TUI estilo htop lendo `/admin/stats` (com `WWZ_ADMIN_TOKEN`) em intervalo configurável: req/s, streams abertos, erros por tipo, latência p50/p99, uso por chave e modelo.

## Alternativas consideradas
- Script externo em Python para agentes; painel web só.

## Consequências
- `wwz-load` depende de `internal/` do `wwz-tools`; a TUI usa uma biblioteca de terminal (decisão do implementador, registrar se não for padrão).

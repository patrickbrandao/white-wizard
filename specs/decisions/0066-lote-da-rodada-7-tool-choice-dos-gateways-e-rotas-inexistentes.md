# 0066 — Lote da rodada 7: `tool_choice` dos gateways, escala de saída e rotas inexistentes

Status: provisória (revisar quando a escala de saída for medida no real ou quando as tarefas 369–371 conferirem as rotas)
Data: 2026-10-31

## Contexto
Tarefa 374 do guardião reuniu os achados das tasks 342, 317 e 315 (rodada 7). Complementa a [0065](0065-lote-da-rodada-6-d-tokenizador-claude-tool-choice-e-usage.md) sem substituí-la. Princípio: ADR 0042.

## Decisão
1. **`tool_choice` (342).** Textos reais de tipo ausente e desconhecido na Anthropic; OpenRouter descarta `tool_choice` sem `tools`, valida o tipo ele mesmo (400 `Invalid Anthropic Messages API request`, `metadata.raw`) e repassa 400 do upstream com `provider_name`/`is_byok`; OpenCode recusa só depois de resolver o modelo, com `x-zen-model`. `GET /v1/models/{id}/x` na Anthropic: 404 sem chave, 401 com chave inválida (substitui o "follow-up de código" do item 3 da 0065).
2. **Escala de saída (342).** O fator 1,32 do Claude 4.7+ vale também em `output_tokens`, no corte por `max_tokens`, no stream e nos pontos de cache; gateways sem escala. Escala de saída medida: não conferido.
3. **Rotas inexistentes (317, 315).** OpenRouter nunca 405; OpenCode devolve a página HTML 404; Gemini 404 vazio com headers do GFE. `GET /v1/chat/completions` na OpenAI é lista vazia (200), não 405.

## Consequências
Specs 006 (16.7) e 007 atualizadas; código nas tarefas 369–371 e 342.

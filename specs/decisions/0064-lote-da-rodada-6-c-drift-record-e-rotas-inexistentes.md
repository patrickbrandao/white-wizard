# 0064 — Lote da rodada 6 (C): `wwz-drift record`, sondas da Responses e 404/405 reais

Status: provisória (revisar se a regravação das fixtures `shape/` (task 313) mostrar que o layout ou os marcadores não bastam, ou se uma fixture de `routes/` de OpenRouter, OpenCode ou Gemini contradisser a forma da OpenAI)
Data: 2026-10-31

## Contexto
Tarefa 314 do guardião reuniu os achados das tasks 239, 309 e 235, já mergeadas. Regra da 265: o código mergeado manda. Princípio: ADR 0042.

## Decisão
1. **Sondas da Responses (task 239).** `--probes` aceita `responses` e `responses_stream`; provedor sem rota Responses (`anthropic`, `gemini`, `opencode-google`) sai `skipped`/`not_supported`. Aditivo ao contrato do relatório.
2. **`wwz-drift record` (task 309; completa a ADR 0035).** Deixa de sair com código 2. Grava `<out>/<provedor>/shape/<sonda>.json` (`opencode-google` em `<out>/opencode/google/`), com marcadores novos (`<duration>`, `<project_id>`, `<generation_id>` também como `request-id`, entre outros), lista de modelos > 64 KiB reduzida ao item da sonda e códigos de saída `0`/`1`/`2`. A spec 017 ganha a seção `record`; a 018, o item "Forma das respostas" em "Fixtures reais".
3. **404/405 reais (task 235).** A spec 006 registra as regras conferidas da Anthropic (fora de `/v1` 404 sem corpo; `GET /v1/messages` 405 da borda; rota desconhecida `not_found_error` com `request_id: null`; método errado 405 com `allow`; ids `req_011C`/`msg_011C`) e da OpenAI (404 sem corpo; 405 `Invalid method`; `GET` no chat 405 `Only POST`). Os outros drivers seguem a forma da OpenAI, "não conferido".

## Consequências
- Tarefas de código: 315 (OpenAI `GET /v1/chat/completions` lista as completions guardadas), 316 (Anthropic `/v1/models/{id}/...` como rota desconhecida), 317 (testador: fixtures `routes/` de OpenRouter, OpenCode e Gemini).
- Nenhuma decisão aceita foi mudada: o fato novo da 0035 ("record na fase 5") só foi cumprido.

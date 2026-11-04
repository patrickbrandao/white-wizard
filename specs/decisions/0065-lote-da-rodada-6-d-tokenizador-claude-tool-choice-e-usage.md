# 0065 — Lote da rodada 6 (D): tokenizador do Claude, `tool_choice`, Azure por deployment e cota semanal

Status: provisória (revisar quando a conferência no real contradisser uma constante ou quando houver chave Azure; a meta de streams da task 269 fica fora e vira ADR própria após a task 341)
Data: 2026-10-31

## Contexto
Tarefa 360 do guardião reuniu os achados das tasks 301, 316, 295 e 296, já no código, e conferiu os lotes 6-b, 6-c, 265 (já cobertos pelas ADRs 0063 e 0064 e presentes nas specs). Regra da 265: o código mergeado manda. Princípio: ADR 0042.

## Decisão
1. **Tokenizador simulado do Claude (task 301).** Da 4.7 em diante o `Simulated` é multiplicado por 1,32 no texto e 1,26 no texto das tools; conversa iniciada por `assistant` custa +12/+24/+21 (4.5-4.6, 4.7, 4.8+); blocos de texto seguidos unem-se sem `\n`. Spec 007.
2. **`tool_choice` (task 301).** Textos reais e ordem das recusas da Anthropic na spec 006 16.7; OpenCode repassa com `Upstream request failed: [invalid_request_error] `; OpenRouter recusa `any`/`tool` pelo id dele e ignora chaves extras.
3. **Anthropic `GET /v1/models/{id}/...` (task 316).** Rota desconhecida: 404 `not_found_error` com `request_id` preenchido; sem chave o real dá 404 e o wwz 401 (follow-up de código).
4. **Azure por deployment (task 295).** Entidade `deployment` (`<conta>/<modelo>`), RPM derivado do TPM, janelas de 1 s/10 s, Responses com `content_filters`, `/openai/models` com `api-version`, `oai/structured` compartilhado. Specs 001, 004, 006 16.6, 010, 012 e README do Valkey; `deployment` entra nos enums de entidade do asyncapi e dos openapi (aditivo). Não conferido (ADR 0043).
5. **Cota semanal UTC (task 296).** `quota_weekly` vira na segunda 00:00 UTC; `GET /v1/usage` do OpenCode preenche `percent` e `resetsAt`. Formato das chaves inalterado. Specs 006 e 010 e README do Valkey.

## Consequências
- Tarefas de código: admin (`entityKinds`) e painel aceitarem `deployment`; `freeModelsSubject` com `driver.As`; engine autenticar a rota de modelo da Anthropic só depois de decidir que é rota conhecida.
- Nenhuma decisão aceita foi mudada.

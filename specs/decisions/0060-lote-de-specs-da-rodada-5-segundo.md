# 0060 — Segundo lote da rodada 5: conta e escada do OpenRouter, variantes, tokens de tools e drift do OpenCode

Status: provisória; o trecho "CORS" foi substituído pela [0061](0061-lote-da-rodada-6-achados-de-spec.md) (CORS em toda rota do OpenRouter) (revisar se uma fixture do real contradisser o cálculo de `total_usage`, o CORS das rotas de inferência do OpenRouter ou os limites de `:free`)
Data: 2026-10-31

## Contexto
O código das tasks 186, 189, 234, 250, 252 e 257 foi além das specs. O guardião conferiu cada ponto no código e nas fixtures e fixou o texto. Princípio: ADR 0042 (igual ao real; senão "não conferido").

## Decisão
- **`GET /api/v1/credits` (task 250).** `total_credits` = `credits_usd` da conta + `purchased` (`add_usd`); `total_usage` = `total_credits` − saldo atual; sem `credits_usd` nem saldo, `total_credits` = `purchased` e `total_usage` = gasto da conta. Na `key`, `usage`, `usage_daily`, `usage_weekly` e `usage_monthly` valem o gasto do mês da chave até o store guardar total, dia e semana (aproximação, não conferida).
- **CORS.** `access-control-allow-origin: *` só nas rotas de conta (`key`, `auth/key`, `credits`, `keys`): é o que as fixtures trazem; nenhuma fixture de inferência ou de modelos o traz, então o driver não o envia ali. O código ainda não o emite nas rotas de conta (tarefa de código).
- **Escada de abuso.** Degrau = `strikes − strike_429.count` limitado ao último; `unblock` mantém os strikes; `reset` com `scope: strikes` perdoa; o motivo `abuse ladder: …` fica no store e nunca vai ao cliente (spec 012).
- **Variantes do OpenRouter (task 186).** Regras conferidas nas fixtures de `testdata/real/openrouter/models/` (spec 006, 16.4): `:nitro`/`:floor`/vazio/duplo/desconhecido → base; `:free` é modelo próprio (`models[].id` pode terminar em `:free`); 404 `This model is unavailable for free…`, 404 `No endpoints found for <nome>.` e `Model not found: …:free` no GET; base desconhecida → 400 da regra 3. A pendência da 0052 fica resolvida.
- **Limites de `:free`.** 20 req/min e 50/1 000 req/dia não são aplicados pelo código; ficam "não conferido" e viram tarefa de código (conta, unidade própria para `:free`, `free_model_daily_requests`).
- **Tokens de tools e `json_schema` (task 252).** Regra derivada de 110 chamadas reais ao `gpt-4o-mini`, escrita na spec 007 com as constantes de `internal/generate/tools.go`; fora da OpenAI, "não conferido".
- **Drift do OpenCode (task 257).** A retentativa de sessão vale para qualquer `id` que não seja `gw-<12 hex>-<6 hex>` (32 hex, `chatcmpl-` e ids longos de 53 caracteres foram vistos no real); sondas do `opencode` com `max_tokens: 256`. Refina a 0059.
- **README do Valkey.** Itens 30–32 da resposta de `check`/`check_entity` (tokens do projeto) e menor `remaining` no lugar de "última entidade vence" (task 189).

## Consequências
- Tarefas de código: (1) `access-control-allow-origin: *` nas rotas de conta do OpenRouter; (2) limites de `:free` na conta; (3) `usage_daily/weekly/total` reais no store (quando houver como distingui-los).
- Specs 003, 006, 007, 012, 017 e `cache/valkey/README.md` atualizados; 0052 anotada; HISTORY.md atualizado.

## Alternativas descartadas
- Enviar CORS em toda rota do OpenRouter: sem fixture que o mostre.
- Aplicar os limites de `:free` com números da documentação sem marcar "não conferido": viola a 0042.

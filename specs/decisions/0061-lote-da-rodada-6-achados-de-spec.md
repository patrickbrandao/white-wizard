# 0061 — Lote da rodada 6: CORS em toda rota do OpenRouter, formas reais do 194/241/245 e `cachedContents` no store

Status: provisória (revisar se uma fixture mostrar rota do OpenRouter sem `access-control-allow-origin`, se `GET /api/v1/models` sem chave não for público, ou se o `cachedContents` real exigir texto guardado)
Data: 2026-10-31

## Contexto
Tarefa 280 do guardião reuniu os achados das tasks 262, 194, 241, 245 e 265. Princípio: ADR 0042 (igual ao real; senão "não conferido").

## Decisão
1. **CORS (fato novo da task 262; substitui o trecho "CORS" da [0060](0060-lote-de-specs-da-rodada-5-segundo.md)).** O OpenRouter real manda `access-control-allow-origin: *` também no chat, nas demais rotas de inferência (erros incluídos) e em `/models`. O driver o envia em **toda** rota. Tarefa de código: estender o que a 262 fez nas rotas de conta.
2. **Spec 006, 16.7 (task 194).**
   - `GET /api/v1/models` do OpenRouter é público: 200 com chave inválida; sem chave, "não conferido" (o wwz responde 200). O engine reconhece rota pública por **interface opcional do driver** (`PublicRoute`), sem caso do `openrouter` dentro do engine. Isso também serve ao `GET /v1/models` do `opencode` sem chave (task 254).
   - OpenAI sem chave: texto longo `You didn't provide an API key…`, `code: null`. Máscara da chave inválida: um `*` por caractere escondido (comprimento − 12); chave com 12 caracteres ou menos "não conferido".
   - Anthropic: `anthropic-version: header is required` vem depois do JSON válido e antes do resto do corpo (a chave continua primeiro).
3. **Spec 006, 16.3 (task 241).** A rota Gemini do OpenCode existe só em `/inference` e `/inference/go`, não em `/zen` nem `/zen/go`. Modelos `gemini-*` respondem na forma Vertex AI (`modelVersion` primeiro, `createTime`, `responseId`, sem `index`, `cost`/`opencode` no usage); os outros, numa forma traduzida (raciocínio como partes `thought`); `allowance` 2× o preço de lista nos `gemini-*`. O real serve qualquer modelo nessa rota: ver a [0062](0062-opencode-serve-todo-modelo-em-toda-rota.md).
4. **Spec 011 (task 245).** `system_matches` casa a primeira mensagem `system`/`developer` nas rotas OpenAI (o `req.System` é só o system de fora das mensagens; `core.Request.SystemPrompt()`).
5. **README do Valkey, seção 2.6 (task 265).** Chaves `P:cc:{cc:<id>}` (hash) e `P:cc:idx:{acct:<conta>}` (zset) para `cachedContents` compartilhados entre instâncias; sem script, sem texto guardado; operações opcionais `CachedContent{Put,Get,Update,Delete,List}`. Escolhas minhas, sem conferência no real: tamanho padrão de página e ordem da listagem ("não conferido").
6. Spec 006 também registra que os limites de `:free` já valem no código (task 262).

## Consequências
- Tarefas de código 281 a 285 (CORS 282, rota pública 284, `cachedContents` 285; a 281 e a drivertest 283 vêm da 0062) (ver `tasks/`). Specs 003, 006, 011, README do Valkey, 0048 e 0060 anotadas; HISTORY.md atualizado.

## Alternativas descartadas
- Guardar o conteúdo do `cachedContents` no store: custo de memória sem uso (o motor só precisa de tokens e prefixo).
- Script Lua para `cachedContents`: as duas chaves ficam em slots diferentes; o pipeline basta e a inconsistência é corrigida na listagem.

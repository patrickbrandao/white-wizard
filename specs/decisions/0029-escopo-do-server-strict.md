# 0029 — Escopo do `server.strict`

Status: aceita com a mudança da 0042 (validação sempre ligada) (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A spec 004 dizia que `strict: true` valida o corpo "contra o schema de cada provedor"; a spec 003 o dava como reservado e sem efeito; a spec 007 previa 400 para schemas de tools fora do subconjunto que o gerador entende. A task 073 implementou em `internal/server/strict.go` a recusa de campos de topo desconhecidos nos dialetos `openai` e `anthropic`. Fato novo: o subconjunto do gerador é uma limitação do wwz, não do provedor; a API real aceita `not`, `if/then`, `patternProperties` etc. Recusá-los inventaria um erro que o provedor nunca dá (contra a ADR 0014).

## Decisão
- `server.strict` (padrão `false`) significa "recusar o que o provedor real recusa, com o texto real", ao custo de uma varredura extra do JSON.
- Campos de topo: drivers `openai` (`/v1/chat/completions`, `/v1/completions`) e `anthropic` (`/v1/messages`, `/v1/messages/count_tokens`) devolvem 400 `invalid_request_error`. OpenAI: `Unrecognized request argument supplied: <campo>` (com vários: `Unrecognized request arguments supplied: a, b`). Anthropic: `<campo>: Extra inputs are not permitted` (o primeiro desconhecido). OpenRouter e OpenCode Go nunca são afetados (gateways que repassam ou ignoram). Rotas não listadas não são verificadas.
- Schemas fora do subconjunto do gerador: sempre melhor esforço, com ou sem `strict`; o 400 da spec 007 é retirado.
- Novo (implementar): com `strict`, o driver `openai` valida `tools[].function` com `strict: true` e `response_format.json_schema` com `strict: true` pelas regras de Structured Outputs: todo `object` declara `additionalProperties: false` e `required` lista todas as chaves de `properties`. Violação → 400 `invalid_request_error`, `param` `tools[<i>].function.parameters` (ou `response_format`), `code` `invalid_function_parameters` (ou `invalid_json_schema`), mensagem `Invalid schema for function '<nome>': In context=(<caminho>), 'additionalProperties' is required to be supplied and to be false.` ou `Invalid schema for function '<nome>': In context=(<caminho>), 'required' is required to be supplied and to be an array including every key in properties. Missing '<chave>'.` (`response_format '<nome>'` no lugar de `function '<nome>'`). Os textos serão confirmados pelas fixtures reais da fase 5.

## Alternativas consideradas
- Manter o 400 para schemas fora do subconjunto: erro que o provedor não dá.
- Validar o corpo inteiro contra o OpenAPI oficial de cada provedor: custo alto no caminho quente e mensagens diferentes das reais.

## Consequências
- Specs 003, 004 e 007 atualizadas.
- Task 086 implementa a validação de `strict: true` em `internal/server/strict.go`.

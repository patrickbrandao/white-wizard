# 0055 — Contrato da Responses API: forma conferida, estado de `previous_response_id` e lacunas

Status: provisória (revisar quando uma fixture real de modelo de raciocínio, de `previous_response_id` com cache ou de `background`/`conversation` mostrar outra regra; ou se o registro de respostas pesar no Valkey sob carga)
Data: 2026-10-31

## Contexto
A [ADR 0054](0054-escopo-de-rotas-do-mvp.md) põe `POST /v1/responses` no MVP. A ADR 0042 exige a forma real. Fato novo: 16 fixtures gravadas em 31/10/2026 (task 231, `testdata/real/openai/responses/`, `gpt-4o-mini`; gasto abaixo de US$ 0,01) mais a fixture de cache da task 201 mostram:

- Corpo não-stream com 40 campos de topo em ordem fixa, `billing` presente só no não-stream; `store` padrão `true`; `model` = snapshot (`gpt-4o-mini-2024-07-18`); `usage.input_tokens_details` com `cache_write_tokens` **antes** de `cached_tokens`.
- Stream: `response.created` → `response.in_progress` → `response.output_item.added` → (`response.content_part.added` → `response.output_text.delta`… → `response.output_text.done` → `response.content_part.done` | `response.function_call_arguments.delta`… → `response.function_call_arguments.done`) → `response.output_item.done` → `response.completed` ou `response.incomplete`; `sequence_number` de 0 em diante, sem `data: [DONE]`; deltas com `logprobs: []` e `obfuscation`; `service_tier: "auto"` nos eventos iniciais e `"default"` no final.
- `previous_response_id` de resposta gravada soma a conversa anterior à entrada (27 = 10 da mensagem anterior + 7 da saída como mensagem `assistant` + 7 da nova + 3 de priming, a mesma regra de overhead do Chat, ADR 0049); id inexistente **ou de resposta com `store: false`** → 400 `previous_response_not_found`.
- Tool `function` sem `strict` sai como `strict: true` com o schema normalizado (`additionalProperties: false` e `required` com todas as chaves); com `strict: true` explícito e schema fora da regra → 400 `invalid_function_parameters`.
- Erros de validação com `param` e `code` preenchidos (`unknown_parameter`, `missing_required_parameter`, `unsupported_parameter` para `messages`, `integer_below_min_value` para `max_output_tokens` < 16, `invalid_json`), diferentes do Chat Completions (que usa `param: null`, `code: null` no campo desconhecido).
- Não conferido: modelos de raciocínio (a chave do projeto não tem acesso a `gpt-5-nano`, `o4-mini`, `o3-mini`), `event: error`/`response.failed` no meio do stream, `include_obfuscation: false`, ordem de validação.

## Decisão
- **Forma:** a da spec 004, seção "Responses API", que transcreve as fixtures; o que não foi conferido fica marcado ali e na spec 006, seção "Não conferidos no real".
- **Estado de `previous_response_id`:** o wwz grava um registro compacto de cada resposta com `store` diferente de `false`: conta dona, `models[].id`, número de mensagens e tokens da conversa acumulada (com o overhead por mensagem) e o identificador do prefixo para o cache de prompt. Não grava texto. TTL de 30 dias, como a retenção do real. Chave por resposta com hash tag no próprio id (não na conta, para não concentrar um slot), no store configurado (`memory` ou `valkey`); o schema literal vai ao `cache/valkey/README.md` (guardião, antes do código do store). Com `store: memory`, o registro vale só na instância e é limitado por LRU (padrão 1 000 000 registros); com `valkey`, a memória é a do operador (`maxmemory` com política `volatile-*` descarta registros antes de contadores).
- **Uso com `previous_response_id`:** `input_tokens` = tokens da conversa gravada + entrada nova + 3; a conversa gravada conta como prefixo já escrito no cache de prompt da conta (o acerto segue as regras da spec 007; "não conferido" até a fixture com prompt ≥ 1 024 tokens). Resposta de outra conta, vencida, descartada ou gravada com `store: false` → `previous_response_not_found`.
- **`instructions`** não é herdada da resposta anterior (documentação oficial; não conferido).
- **Tools:** `function` (formato plano: `type`, `name`, `description`, `parameters`, `strict`) é gerada como no Chat; ferramentas nativas (`web_search`, `file_search`, `code_interpreter`, `image_generation`, `computer_use_preview`, `mcp`, `custom`…) são aceitas, ecoadas em `tools` e nunca chamadas (o modelo responde em texto), o que o real também pode fazer. A normalização de `strict` omitido segue o real conferido; quando o schema não cabe no modo estrito, o comportamento do real não foi conferido e o wwz mantém `strict: false` (marcado).
- **Lacunas do MVP (desvio registrado, cada uma com o gatilho desta ADR):** `background: true` é atendido de forma síncrona (o corpo diz `background: true`); `conversation` e `prompt` (prompt templates) recebem 400 `invalid_request_error` com `param` do campo e o texto "não conferido"; `include` aceita os valores documentados e só `reasoning.encrypted_content` muda a saída.
- **Raciocínio:** item `reasoning` (`rs_…`, `summary: []`) antes da mensagem nos modelos com `capabilities.reasoning`; com `reasoning.summary` (`auto`, `concise`, `detailed`) os eventos `response.reasoning_summary_part.added`, `response.reasoning_summary_text.delta`/`.done` e `response.reasoning_summary_part.done`; `encrypted_content` com `include: ["reasoning.encrypted_content"]`; `output_tokens_details.reasoning_tokens` > 0. Tudo "não conferido" até uma chave com acesso a modelo de raciocínio.

## Alternativas consideradas
- Responses sem estado (sempre `previous_response_not_found`): diverge do real na primeira chamada encadeada, que é o uso principal da API pelos agentes.
- Guardar o texto completo para servir `GET /v1/responses/{id}`: memória proporcional ao tráfego e rota fora do MVP (ADR 0054).
- TTL curto (1 h) para poupar o Valkey: diverge da retenção real; a mitigação é o operador mandar `store: false`, como os roteadores sob carga fazem, ou limitar `maxmemory`.

## Consequências
- Spec 004 (seção "Responses API" e tabela de validação), 006 (16.1 e 16.7), 007 (uso e cache na Responses) e 012 (registro de respostas) atualizadas.
- Tarefas: guardião (schema `resp` no `cache/valkey/README.md`), store (registro de respostas em `limits.Store` ou interface irmã), driver `openai` (rota, forma, stream, validação, estado), drivertest, SDK (`client.responses.create`, stream e encadeamento), sonda `responses` do `wwz-drift`.

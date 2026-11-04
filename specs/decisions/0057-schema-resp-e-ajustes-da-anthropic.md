# 0057 — Schema `resp` no Valkey, formas finas da Anthropic e `reasoning_field` por modelo

Status: provisória (revisar se uma gravação sem anonimização mostrar outro prefixo de id da Anthropic, se o registro de respostas pesar no Valkey sob carga, ou se um modelo `opencode` exigir `reasoning_field` por driver em vez de por modelo)
Data: 2026-10-31

## Contexto
Task 236, lote do guardião. A [ADR 0055](0055-contrato-da-responses-api.md) deixou o schema literal do registro de `previous_response_id` para o `cache/valkey/README.md`. A task 173 (forma real da Anthropic) e a task 154 (`reasoning_field` por driver) trouxeram achados que as specs ainda não diziam. Fixtures conferidas em `testdata/real/anthropic/shape/` (`tool_use`, `tool_use_stream`, `thinking`, `thinking_stream`, `error_auth`, `models`).

## Decisão
- **Schema `resp`** (README 2.5): `P:resp:{resp:<id-escapado>}`, hash com `acct`, `drv`, `model`, `msgs`, `toks`, `pfx`, `at`; TTL fixo de 30 d (`PEXPIRE`, sem renovar na leitura); gravação com `MULTI`/`HSET`/`PEXPIRE`/`EXEC` no primeiro byte; leitura com `HGETALL`; conta conferida em Go; sem índice global nem script. Operações `ResponsePut` e `ResponseGet`, interfaces opcionais do store (a ADR 0055 só dizia "em `limits.Store` ou interface irmã"; escolhida a opcional, como `PromptCacheLookup`). `memory`: LRU de 1 000 000 registros.
- **Anthropic, spec 006 16.7** (conferido): `tool_use` com `caller{type: direct}`; primeiro `input_json_delta` com `partial_json: ""`; `thinking` com `signature: ""` no `content_block_start` do stream; `usage.output_tokens_details.thinking_tokens` (no `usage` completo e no do `message_delta`) só quando há raciocínio; 401 com `cache-control: no-store`; `lifecycle: "deprecated"` com `deprecated_at` e `retires_at` em RFC 3339 UTC.
- **Ids `msg_011C…`/`req_011C…`**: relato da task 173, **não conferido** (as fixtures anonimizam os ids). A spec mantém `msg_` + 24 base58 e registra o prefixo `011C` como não conferido; a task de código só o adota se houver gravação com id real.
- **`internal/drift/wwz.yaml` com `models[].generation.reasoning_field: reasoning_content`** no modelo `opencode` **segue a ADR 0046**: o valor é permitido a `opencode` e `openrouter` (que imitam o upstream) e proibido só em `openai`/`azure`. Nada a mudar. A spec 007 passa a dizer que `reasoning_field` pode vir do perfil `generation` do modelo (padrão do código: `reasoning_content`) ou de `drivers[].options`, e que a validação de `hidden`-só vale para os dois lugares nos drivers `openai` e `azure`.

## Alternativas consideradas
- Script Lua para `ResponsePut`: uma chave só, `MULTI` basta; script adicionaria manutenção sem ganho.
- Renovar o TTL na leitura: diverge da retenção do real (30 d desde a criação).
- Gravar o prefixo `011C` já: sem fixture com id real, violaria a ADR 0042.

## Consequências
- README do Valkey (2.5, tabela de operações), specs 006, 007 e 012 e `CHANGELOG.md` atualizados.
- Tarefas de código derivadas: (1) store: `ResponsePut`/`ResponseGet` em `memory` e `valkey`, com `storetest`; (2) driver `anthropic`/`drivertest`: validar os itens de 16.7 acima nas fixtures (parte pode já estar na task 173); (3) validação de configuração: `generation.reasoning_field: hidden` é o único valor aceito em `openai`/`azure`, também no perfil do modelo.

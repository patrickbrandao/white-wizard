# agente-de-arquivos

Um agente de arquivos conduzido por cenário: o servidor responde como se um LLM
decidisse ler, gravar, editar e apagar um arquivo, e depois dá a resposta final.
Serve para testar se o roteador preserva a ordem das tool calls, os `tool_call_id`
e o histórico de uma conversa multi-turno.

## O que demonstra

- O cenário embutido `file-agent-flow` (specs [011](../../specs/011-cenarios.md),
  [ADR 0048](../../specs/decisions/0048-dialetos-catalogos-e-templates.md)):
  `read_file` -> `write_file` -> `edit_file` -> `delete_file` -> resposta final
  (`finish_reason: stop`). Os seis cenários embutidos vêm no binário
  (`scenarios.builtin`, padrão `true`): nada para montar no container.
- O servidor **não guarda estado**: o passo sai do histórico da conversa
  (resultados de tool e ids de tool call `call_<cenário>_<passo>_<n>_<nonce>`). Por isso
  qualquer instância atende qualquer turno.
- O cenário só casa se o pedido oferece as quatro tools (`match.tools_include`) e
  o modelo declara `scenarios: [file-agent-flow]`.
- O header `x-wwz-step` informa o passo (`read`, `write`, `edit`, `delete`, `done`)
  e `x-wwz-scenario` o cenário. O resultado da gravação precisa conter `ok`,
  `written`, `success`, `saved` ou `done`; senão o cenário cai no passo
  `edit-failed` e tenta gravar de novo.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/agente-de-arquivos
EX_PORT=18111 docker compose up -d --wait    # porta 18111 no host
# primeiro turno (precisa das 4 tools, veja smoke.sh):
./smoke.sh                                   # percorre o fluxo inteiro
docker compose down -v
```

Chave: `wwz-key-demo`. Modelos: `gpt-4o-mini` (OpenAI) e `claude-sonnet-4` (Anthropic).
Para ver o fluxo sem subir servidor: `wwz simulate --scenario file-agent-flow --turns 6`.

## O que esperar

5 respostas por conversa: 4 com uma tool call cada (`finish_reason: tool_calls`
ou `stop_reason: tool_use`) e a última com texto.

## Smoke

`./smoke.sh` (ou `make example NAME=agente-de-arquivos`, ~10 s):

- Dialeto OpenAI: executa cada tool pedida (resultado `ok`), devolve o `role: tool`
  e confere a ordem `read_file write_file edit_file delete_file`, os passos
  `read write edit delete done` em `x-wwz-step`, argumentos que parseiam e o
  `finish_reason: stop` com texto no fim.
- Dialeto Anthropic: o mesmo com `tool_use`/`tool_result` e `stop_reason: end_turn`.
- Derruba o compose com `down -v`, inclusive em falha.

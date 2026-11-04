# tool-calls-quebradas

Parte das tool calls sai com `arguments` que não são JSON válido. Serve para ver
se o agente ou o roteador valida os argumentos antes de executar a tool, em vez de
quebrar com `JSONDecodeError`.

## O que demonstra

- `errors.tool_misbehavior` num perfil de erros (specs [009](../../specs/009-erros-e-caos.md)
  e [017](../../specs/017-recursos-importados.md), I1): `rate: 40` e
  `kinds: [truncated, trailing_comma, single_quotes]`. Em 40% das respostas que
  trazem tool call, os argumentos saem truncados (`{"path":"alph`), com vírgula
  sobrando (`{"path":"x",}`) ou com aspas simples (`{'path':'x'}`). As demais saem
  normais. Pedidos sem `tools` nunca são afetados.
- No dialeto OpenAI (`/v1/chat/completions`), `function.arguments` é uma string e
  o defeito aparece no `JSON.parse` do cliente.
- No dialeto Anthropic, como no real, o `input` do não-stream é sempre um objeto;
  o JSON quebrado só existe no **stream** (`input_json_delta.partial_json`, que o
  cliente concatena e parseia no fim). Por isso o smoke usa `"stream": true` ali.
- `server.seed` fixo. **Atenção:** com `seed`, o sorteio depende do primeiro prompt
  do usuário; varie o prompt (ou mande `x-wwz-conversation-id`), senão toda
  requisição sai igual.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/tool-calls-quebradas
EX_PORT=18110 docker compose up -d --wait    # porta 18110 no host
curl -s -H 'Authorization: Bearer wwz-key-demo' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","tool_choice":"required","messages":[{"role":"user","content":"leia 1"}],
       "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]}' \
  http://127.0.0.1:18110/v1/chat/completions | jq '.choices[0].message.tool_calls[0].function.arguments'
docker compose down -v
```

Chave: `wwz-key-demo`. Modelos: `gpt-4o-mini` (OpenAI) e `claude-sonnet-4` (Anthropic).

## O que esperar

Mudando o prompt a cada chamada: cerca de 40% dos `arguments` não parseiam.

## Smoke

`./smoke.sh` (ou `make example NAME=tool-calls-quebradas`, ~30 s):

- 300 chamadas OpenAI com `tools`: de 86 a 154 `arguments` não parseiam (40% +- 4
  sigma, sigma = 8,5) e o resto parseia como objeto; nenhuma resposta sem tool call.
- 200 chamadas Anthropic em stream: de 52 a 108 com `partial_json` concatenado
  inválido (sigma = 6,9).
- 5 chamadas sem `tools`: sempre 200 com texto.
- Derruba o compose com `down -v`, inclusive em falha.

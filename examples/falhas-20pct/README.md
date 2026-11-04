# falhas-20pct

20% das requisições falham, com tipos de erro variados e no envelope exato de
cada dialeto. Serve para ver se o roteador faz retry, troca de provedor e
backoff sem exagerar quando um provedor "só" está ruim.

## O que demonstra

- `errors.rate: 20` num perfil de erros (spec [009](../../specs/009-erros-e-caos.md)),
  com `types: [rate_limit, server_error, bad_gateway, overloaded]` e pesos 3/3/2/2.
- OpenAI (`/v1/chat/completions`): 429 `rate_limit_exceeded` (com `retry-after`),
  500 `server_error`, 502 (corpo HTML, como um proxy real) e 503
  `service_unavailable_error`. Anthropic (`/v1/messages`): 429, 500, 502 e **529**
  `overloaded_error`, no envelope `{"type":"error","error":{...}}`.
- `server.seed` fixo. **Atenção:** com `seed`, o sorteio depende da conversa (hash
  do primeiro prompt do usuário, mais o modelo); duas requisições com o mesmo prompt
  têm o mesmo destino. Quem testa um roteador deve variar o prompt (ou mandar
  `x-wwz-conversation-id`), senão vê 0% ou 100% de erros. Sem `seed`, cada requisição
  sorteia de forma independente.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/falhas-20pct
EX_PORT=18102 docker compose up -d --wait    # porta 18102 no host
curl -s -H 'Authorization: Bearer wwz-key-demo' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"oi 1"}]}' \
  http://127.0.0.1:18102/v1/chat/completions
docker compose down -v
```

Chave: `wwz-key-demo`. Modelos: `gpt-4o-mini` (OpenAI) e `claude-sonnet-4` (Anthropic).

## O que esperar

Em 500 chamadas com prompts diferentes: cerca de 100 erros (20%), distribuídos
entre os quatro códigos. O resto é 200.

## Smoke

`./smoke.sh` (ou `make example NAME=falhas-20pct`, ~30 s):

- 500 chamadas ao dialeto OpenAI: erros dentro de 20% +- 4 sigma (n = 500, p = 0,2,
  sigma = 8,94: aceita 64 a 136), pelo menos 3 dos 4 códigos observados, nenhuma
  resposta fora de 200/429/500/502/503 e todo envelope no formato do dialeto.
- 200 chamadas ao dialeto Anthropic: erros em 17 a 63 e pelo menos um 529.
- Derruba o compose com `down -v`, inclusive em falha.

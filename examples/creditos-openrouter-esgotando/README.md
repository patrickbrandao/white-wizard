# creditos-openrouter-esgotando

Saldo pré-pago do OpenRouter decrescendo a cada chamada até acabar. Serve para
ver o que o roteador faz quando um provedor passa a responder 402: o certo é
tirar a conta de rotação (não adianta retentar) e cair no próximo provedor.

## O que demonstra

- `credits_usd` na **conta** (`accounts[].limits`, ADR
  [0051](../../specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md)): o
  OpenRouter cobra o saldo da conta, e chaves extras não mudam isso. As duas
  chaves da conta `pobre` (saldo inicial US$ 3,00) gastam do mesmo saldo; a conta
  `rica` (US$ 1 000) é independente.
- Cada resposta debita o custo do modelo (`models[].pricing`). O preço do exemplo
  é inflado (US$ 5 000 por milhão de tokens de saída; cada resposta tem 100 tokens
  fixos, ou seja ~US$ 0,50) para o saldo acabar em 6 chamadas; com o preço real
  do `gpt-4o-mini` seriam milhares.
- Depois que o saldo cruza zero a conta fica `limited` (spec
  [012](../../specs/012-estado-valkey.md): "saldo negativo põe a entidade no
  índice `limited`") e toda chamada devolve **402** no envelope do OpenRouter
  (spec [006](../../specs/006-fidelidade-provedores.md), 16.4, "Saldo negativo"):

  ```json
  {"error":{"message":"Insufficient credits. Add more using https://openrouter.ai/settings/credits",
   "code":402,"metadata":{"limit_source":"openrouter_credits","reason":"insufficient_credits",
   "remedy_hint":"Add credits or raise the key limit, then retry.","error_type":"payment_required"}},
   "user_id":"user_wwz"}
  ```

- Como no real, a requisição que cruza o zero ainda é atendida: o saldo fica
  levemente negativo (alguns microdólares) e é a chamada seguinte que recebe 402.
- O saldo atual se lê na API admin (`GET /admin/entities/account/pobre`, campo
  `credits_usd`) e se repõe com `POST /admin/entities/account/pobre/credits`
  (`{"usd": 2}` define o saldo; `{"add_usd": 2}` soma como compra).

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/creditos-openrouter-esgotando
EX_PORT=18107 EX_ADMIN_PORT=19107 docker compose up -d --wait
for i in $(seq 1 8); do
  curl -s -o /dev/null -w '%{http_code} ' -H 'Authorization: Bearer wwz-key-or-a' \
    -H 'Content-Type: application/json' \
    -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}' \
    http://127.0.0.1:18107/api/v1/chat/completions
  curl -s -H 'Authorization: Bearer wwz-ex-admin-token' \
    http://127.0.0.1:19107/admin/entities/account/pobre | jq .credits_usd
done
docker compose down -v
```

Chaves: `wwz-key-or-a` e `wwz-key-or-b` (conta `pobre`) e `wwz-key-or-rica` (conta
`rica`). Modelo: `openai/gpt-4o-mini` (id com autor, como no OpenRouter).

## O que esperar

Seis respostas 200 com o saldo caindo de 3,00 para 2,50, 2,00, 1,50, 1,00, 0,50 e
~0; da sétima em diante, 402. A conta `rica` segue em 200.

## Smoke

`./smoke.sh` (ou `make example NAME=creditos-openrouter-esgotando`, ~10 s):

- Alterna as duas chaves da `pobre` até o 402; confere que o saldo (admin)
  decresce estritamente a cada 200, que há 5 a 7 respostas 200 e que o saldo
  termina em ~0.
- As duas chaves recebem 402 `openrouter_credits` / `insufficient_credits`.
- `wwz-key-or-rica` recebe 200 e o saldo da `rica` cai abaixo de 1 000.
- Uma recarga de US$ 2 pela admin devolve o 200 à conta `pobre`.
- Derruba o compose com `down -v`, inclusive em falha.

## Notas

- `protocols: [openai, openrouter]` no modelo: o driver `openrouter` ainda só
  serve modelos com `openrouter` na lista (task 177, dialeto por modelo). Quando
  ela chegar, basta `protocols: [openai]`.
- O `GET /api/v1/key` do driver hoje devolve sempre `limit` e `limit_remaining`
  iguais ao saldo inicial e `usage: 0`, mesmo depois das chamadas; por isso o
  smoke lê o saldo da API admin e não desse endpoint.

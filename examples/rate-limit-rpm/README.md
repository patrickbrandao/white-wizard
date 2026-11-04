# rate-limit-rpm

Limite de requisições por minuto com os headers `x-ratelimit-*` e o 429 da
OpenAI. Serve para ver se o roteador respeita `retry-after`, para de insistir
numa conta esgotada e desvia para outra.

## O que demonstra

- `rpm` declarado na **conta** (`accounts[].limits`, ADR
  [0051](../../specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md)), como
  no provedor real: o limite é da organização, nunca da chave. As duas chaves da
  conta `acme` (rpm 30) dividem o mesmo limite; a conta `globex` (rpm 100) tem o
  seu e não é afetada pela `acme`.
- Em toda resposta, `x-ratelimit-limit-requests`, `x-ratelimit-remaining-requests`
  e `x-ratelimit-reset-requests` (e os três de `tokens`), no formato da OpenAI
  (spec [010](../../specs/010-controle-de-taxa.md) e
  [006](../../specs/006-fidelidade-provedores.md)). `remaining` decresce a cada
  chamada.
- Ao estourar: 429 `rate_limit_exceeded` com `error.type: requests`, a mensagem
  "Rate limit reached for gpt-4o-mini in organization ... on requests per min
  (RPM): Limit 30, Used 30, Requested 1", `retry-after` (segundos) e
  `retry-after-ms`.
- O token bucket repõe 0,5 ficha por segundo (30/min), então depois de esgotar a
  conta volta a aceitar uma chamada a cada ~2 s. Com `algorithm: fixed_window` na
  conta a contagem zera de uma vez no fim da janela de 60 s.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/rate-limit-rpm
EX_PORT=18105 docker compose up -d --wait    # porta 18105 no host
for i in $(seq 1 40); do
  curl -s -o /dev/null -w '%{http_code} ' -H 'Authorization: Bearer wwz-key-acme-a' \
    -H 'Content-Type: application/json' \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}' \
    http://127.0.0.1:18105/v1/chat/completions
done
docker compose down -v
```

Chaves: `wwz-key-acme-a` e `wwz-key-acme-b` (conta `acme`, rpm 30) e
`wwz-key-globex` (conta `globex`, rpm 100). Modelo: `gpt-4o-mini`.

## O que esperar

Uma rajada de 40 chamadas da `acme` (qualquer mistura das duas chaves) dá cerca
de 30 respostas 200 e as demais 429. Logo depois, `wwz-key-globex` ainda recebe 200
com `x-ratelimit-limit-requests: 100`.

## Smoke

`./smoke.sh` (ou `make example NAME=rate-limit-rpm`, ~15 s):

- Primeira chamada: `limit-requests` 30 e `remaining` 28 ou 29.
- Rajada de 45 chamadas alternando as duas chaves da `acme`: 30 a 33 respostas
  200 (30 fichas mais a reposição de ~1 s de rajada) e 12 a 15 respostas 429; o
  `remaining` nunca sobe nos 200 e termina em 0 ou 1.
- Cada chave recebeu 429 na rajada, com `retry-after` de 1 a 60 s,
  `x-ratelimit-limit-requests: 30`, `remaining: 0` e o envelope `rate_limit_exceeded`.
- `wwz-key-globex` recebe 200 com `limit-requests: 100` e `remaining` 98 ou 99.
- Derruba o compose com `down -v`, inclusive em falha.

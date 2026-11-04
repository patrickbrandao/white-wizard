# ban-por-abuso

Detecção de abuso e bloqueio de chave. Serve para ver o roteador reagir quando
um provedor deixa de dar 429 (com `retry-after`) e passa a dar 403 de conta
suspensa: o certo é parar de usar a chave, não retentar.

## O que demonstra

- A política de abuso (`abuse`, spec [012](../../specs/012-estado-valkey.md)):
  `strike_429: {count: 5, window: 1m}` e uma escada de um degrau,
  `suspend` por 10 minutos. Cada 429 que a chave leva é um strike; no quinto em
  um minuto o wwz **bloqueia a chave**.
- A chave `wwz-key-abusiva` tem `limits: {rpm: 6}`. Insistir depois do limite dá
  6 respostas 200, 5 respostas 429 (strikes) e, a partir daí, **403
  `account_suspended`** no envelope da OpenAI, **sem `retry-after`**: provedores
  reais não mandam esse header em 403 e os SDKs não retentam 403. O fim do
  bloqueio fica visível em `until` na API admin.
- `GET /admin/entities/key/abusiva` mostra `status: "suspended"`, `reason`,
  `until`, `strikes` e `by: "abuse-ladder"`; `GET /admin/entities?status=suspended`
  lista a chave. `POST /admin/entities/key/abusiva/unblock` a libera; os strikes
  continuam contados (um novo 429 reabriria o bloqueio), então o smoke também usa
  `POST .../reset` com `{"scope":["counters","strikes"]}`.
- O estado (contadores, strikes e bloqueios) vive no **Valkey**, o store
  compartilhado do cluster (`store.kind: valkey`): uma segunda instância do wwz
  apontando para o mesmo Valkey também negaria a chave bloqueada.
- A chave `wwz-key-ok` (outra conta) nunca é afetada.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/ban-por-abuso
EX_PORT=18108 EX_ADMIN_PORT=19108 docker compose up -d --wait
for i in $(seq 1 14); do
  curl -s -o /dev/null -w '%{http_code} ' -H 'Authorization: Bearer wwz-key-abusiva' \
    -H 'Content-Type: application/json' \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}' \
    http://127.0.0.1:18108/v1/chat/completions
done
curl -s -H 'Authorization: Bearer wwz-ex-admin-token' http://127.0.0.1:19108/admin/entities/key/abusiva
curl -s -X POST -H 'Authorization: Bearer wwz-ex-admin-token' http://127.0.0.1:19108/admin/entities/key/abusiva/unblock
docker compose down -v
```

Chaves: `wwz-key-abusiva` (conta `acme-abusiva`, `rpm: 6`) e `wwz-key-ok` (conta
`acme-ok`). Modelo: `gpt-4o-mini`. Token da admin: `wwz-ex-admin-token`.

## O que esperar

`200 200 200 200 200 200 429 429 429 429 429 403 403 403 ...`; depois do
`unblock` (e do perdão dos strikes) a chave volta a receber 200.

## Smoke

`./smoke.sh` (ou `make example NAME=ban-por-abuso`, ~10 s):

- Chama a chave abusiva até 5 respostas 403: confere 6 a 7 respostas 200, 5 a 6
  respostas 429, exatamente 5 respostas 403 com `error.code: account_suspended`
  e sem `retry-after`.
- Admin: a chave aparece `suspended` (`by: abuse-ladder`, `until`, `strikes >= 5`)
  e é a única na lista `?status=suspended`.
- `wwz-key-ok` recebe 200 durante o bloqueio da outra.
- `unblock` e `reset` (`counters` e `strikes`) pela admin: a abusiva volta a
  `active` com 0 strikes e recebe 200.
- Derruba o compose (wwz e Valkey) com `down -v`, inclusive em falha.

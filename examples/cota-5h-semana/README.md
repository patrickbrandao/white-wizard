# cota-5h-semana

Cota em dólares em janelas de 5 horas, semana e mês, no estilo de assinatura (o
plano OpenCode Go). Serve para ver o roteador reagir a um 429 que não passa em
segundos, mas em horas ou dias: quem retenta com backoff curto só desperdiça
chamadas; o certo é tirar a conta de rotação até o `retry-after`.

## O que demonstra

- `quota_usd: {monthly_usd, rolling_5h_pct, weekly_pct}` na **conta**
  (`accounts[].limits`, ADR [0051](../../specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md)).
  A configuração real do plano é a que está no `config.yaml` da conta `ws-5h`:
  **US$ 60 por mês, 5 h = 20 % (US$ 12), semana = 50 % (US$ 30)**. A janela de
  5 h é deslizante; a semanal vira na segunda-feira 00:00 UTC e a mensal no dia 1º
  (UTC), como o real reporta em `GET /v1/usage`. Nenhuma se encurta por configuração.
- Estourou uma janela: 429 `GoUsageLimitError` no envelope do gateway
  (`{"type":"error","error":{"type":"GoUsageLimitError",...},"metadata":{"limitName":"5 hour"}}`),
  com `retry-after` em segundos até a janela liberar: ~5 h (17 995 s) para a de 5
  horas e até a próxima segunda 00:00 UTC (de segundos a 7 dias) para a semanal. O cliente oficial não retenta esse
  erro (spec [006](../../specs/006-fidelidade-provedores.md), 16.3).
- A conta `ws-semana` tem 5 h = 100 % e semana = 25 % (US$ 15): quem barra é a
  semana (`limitName: "weekly"`), mostrando que o simulador escolhe a janela que
  estourou. `ws-livre` não tem cota e continua em 200.
- Cada resposta 200 traz `usage.opencode.allowance` (quanto a chamada consumiu da
  cota) e `usage.opencode.billing_source: "go"`. O OpenCode não manda headers de limite.

## Como o smoke cabe em segundos

Esperar 5 h não é opção, então o smoke não mexe nas janelas (a config é a real, em
dólares): o **preço do modelo é inflado** (`output_per_m: 15000`, cada resposta tem
exatamente 100 tokens de saída, ~US$ 1,50 por chamada), de modo que 8 chamadas
gastam os US$ 12 da janela de 5 h e 10 gastam os US$ 15 da semanal. Para repetir o
teste sem esperar, o smoke usa o **reset de contadores da API admin**
(`POST /admin/entities/account/<id>/reset`, `{"scope":["counters"]}`), que zera
janelas e gasto e preserva bloqueios e créditos. No dia a dia, o preço de lista
do modelo (`pricing`) e as cotas reais valem sem inflar nada.

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/cota-5h-semana
EX_PORT=18106 EX_ADMIN_PORT=19106 docker compose up -d --wait
for i in $(seq 1 10); do
  curl -s -o /dev/null -w '%{http_code} ' -H 'Authorization: Bearer wwz-key-go-5h' -H 'x-opencode-session: ses-demo' \
    -H 'Content-Type: application/json' \
    -d '{"model":"glm-5.2","messages":[{"role":"user","content":"oi"}]}' \
    http://127.0.0.1:18106/zen/go/v1/chat/completions
done
curl -s -H 'Authorization: Bearer wwz-ex-admin-token' http://127.0.0.1:19106/admin/entities/account/ws-5h
curl -s -X POST -H 'Authorization: Bearer wwz-ex-admin-token' http://127.0.0.1:19106/admin/entities/account/ws-5h/reset
docker compose down -v
```

Chaves: `wwz-key-go-5h` (conta `ws-5h`, 5 h = US$ 12), `wwz-key-go-semana` (conta
`ws-semana`, semana = US$ 15) e `wwz-key-go-livre` (conta `ws-livre`, sem cota).
Modelo: `glm-5.2`. Rota: `/zen/go/v1/chat/completions` (também `/inference/...`).

## O que esperar

`ws-5h`: 8 respostas 200 e depois 429 com `limitName: "5 hour"` e `retry-after`
de ~18 000 s. `ws-semana`: 10 respostas 200 e depois 429 `weekly` com
`retry-after` até a próxima segunda 00:00 UTC. `ws-livre` sempre 200.

## Smoke

`./smoke.sh` (ou `make example NAME=cota-5h-semana`, ~15 s):

- `ws-5h`: 7 a 9 respostas 200 até o 429 `GoUsageLimitError` / `5 hour`, com
  `retry-after` de 14 400 a 18 010 s; `usage.opencode.list_cost` de ~US$ 1,50 por chamada (o `allowance` é o valor com o desconto do plano)
  e gasto da conta (`GET /admin/entities/account/ws-5h`) em US$ 12;
  `GET /zen/go/v1/usage` com 5 h em 100 %, semana em 40 %, mês em 20 % e
  `rolling.resetsAt` 5 h depois do primeiro gasto (17 000 a 18 000 s à frente).
- `ws-semana`: 9 a 11 respostas 200 até o 429 `weekly`, com `retry-after` igual
  aos segundos até a próxima segunda 00:00 UTC; `GET /zen/go/v1/usage` com os
  percentuais do gasto (5 h sobre US$ 60, semana 100 %).
- Sem chave: `GET /zen/go/v1/models` responde 200 (público, como no real) e
  `GET /zen/go/v1/usage` responde 401 `AuthError`/`Unauthorized`.
- `ws-livre` ainda recebe 200; `ws-5h` segue em 429 até o reset de contadores pela
  admin, e depois dele recebe 200.
- Derruba o compose com `down -v`, inclusive em falha.

## Notas

- `GET /zen/go/v1/usage` mostra o uso da cota da conta (tasks 254 e 296):
  `rolling` é o gasto das últimas 5 h sobre a cota de 5 h, com `resetsAt` = gasto
  mais antigo da janela + 5 h (5 h à frente sem gasto); `weekly` é o gasto desde
  segunda 00:00 UTC sobre a cota semanal (a mesma janela que barra com
  `quota_weekly`) e `monthly` o gasto do mês UTC sobre a mensal, arredondados e
  limitados a 100 %. O `resetsAt` da janela de 5 h pelo primeiro gasto é o que o
  real sugere; não conferido.

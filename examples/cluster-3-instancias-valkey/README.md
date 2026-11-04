# cluster-3-instancias-valkey

Três instâncias do wwz com o mesmo Valkey: os limites valem para o cluster inteiro,
não por instância. Serve para ver como um roteador se comporta atrás de um
provedor que tem várias réplicas mas uma cota só.

## O que demonstra

- `store.kind: valkey` (specs [010](../../specs/010-controle-de-taxa.md) e
  [012](../../specs/012-estado-valkey.md)): contadores em scripts Lua atômicos por
  entidade, compartilhados pelas três instâncias.
- A conta `acme` tem `rpm: 60` (limite da conta, [ADR 0051](../../specs/decisions/0051-limites-na-conta-e-promocao-de-tier.md));
  as duas chaves dela dividem o teto, em qualquer instância. A conta `globex`
  (`rpm: 600`) não é afetada.
- Sem balanceador no compose: as instâncias aparecem em três portas do host
  (`EX_PORT`, +1000 e +2000), e o smoke faz o rodízio. Para um LB de verdade,
  coloque nginx ou o seu roteador na frente das três.
- Configuração idêntica nas três instâncias (`config.yaml` montado igual).

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/cluster-3-instancias-valkey
EX_PORT=18114 EX_PORT2=19114 EX_PORT3=20114 docker compose up -d --wait
for p in 18114 19114 20114; do
  curl -s -o /dev/null -w "$p %{http_code}\n" -H 'Authorization: Bearer wwz-key-acme-a' \
    -H 'Content-Type: application/json' \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}' \
    http://127.0.0.1:$p/v1/chat/completions
done
docker compose down -v
```

Chaves: `wwz-key-acme-a` e `wwz-key-acme-b` (conta `acme`), `wwz-key-globex`.
Modelo: `gpt-4o-mini`.

## O que esperar

Uma rajada de 120 requisições em rodízio pelas três instâncias recebe 60 respostas
200 mais cerca de uma por segundo de rajada (o bucket repõe 1 ficha/s), e o resto
429 `rate_limit_exceeded`, igualmente distribuído entre as instâncias.

## Smoke

`./smoke.sh` (ou `make example NAME=cluster-3-instancias-valkey`, ~15 s):

- Rajada de 120 requisições da conta `acme` em rodízio por instâncias e chaves:
  200 entre 60 e `62 + segundos da rajada`, e pelo menos um 429 em cada instância.
- Com a conta esgotada, cada uma das 3 instâncias nega com 429 `rate_limit_exceeded`.
- A conta `globex` recebe 200 nas 3 instâncias.
- Derruba o compose (3 wwz e Valkey) com `down -v`, inclusive em falha.

# indisponibilidade-janelas

Janelas programadas de indisponibilidade (`errors.outages`, spec
[009](../../specs/009-erros-e-caos.md), [ADR 0016](../../specs/decisions/0016-diretorio-examples.md)):
o alvo falha só dentro das janelas, em segundos, minutos e horas. Serve para testar
failover, circuit breaker e o respeito do roteador ao `retry-after`.

## As três janelas

Cada janela mora no `errors` inline de um modelo próprio, para o smoke poder provar a
de segundos a qualquer hora. `schedule` é um cron de seis campos (com segundos), em UTC.

| Modelo | `schedule` | `duration` | Quando está ativa | Resposta na janela |
| --- | --- | --- | --- | --- |
| `janela-segundos` | `*/20 * * * * *` | 5 s | segundos :00-:04, :20-:24 e :40-:44 de todo minuto | 503 `service_unavailable_error`, `retry-after` até o fim da janela |
| `janela-minutos` | `0 */10 * * * *` | 1 min | minutos hh:00, hh:10, ... hh:50 | 500 `server_error` |
| `janela-horas` | `0 0 */6 * * *` | 1 h | 00:00-01:00, 06:00-07:00, 12:00-13:00 e 18:00-19:00 UTC | 502 `bad_gateway` (corpo HTML) |

Fora das janelas todas as chamadas dão 200. Em produção os modelos reais teriam as
janelas no perfil ou na raiz (`errors.outages`); aqui estão separados só para teste.
Outros campos: `rate` (probabilidade de falhar dentro da janela, padrão 100),
`timezone` (ex.: `America/Sao_Paulo`) e `drivers`. As janelas dos níveis raiz,
modelo, chave e perfil de `x-wwz-chaos-profile` se somam.

Só o 503/529 (`overloaded`) e o 429 levam `retry-after`; o 500 e o 502 não o enviam,
como o provedor real (ADR 0042).

## Como subir

```bash
make image
cd examples/indisponibilidade-janelas
EX_PORT=18104 docker compose up -d --wait
for i in $(seq 1 45); do
  curl -s -o /dev/null -w "$(date -u +%T) %{http_code}\n" -H 'Authorization: Bearer wwz-key-demo' \
    -H 'Content-Type: application/json' \
    -d '{"model":"janela-segundos","max_tokens":8,"messages":[{"role":"user","content":"oi"}]}' \
    http://127.0.0.1:18104/v1/chat/completions; sleep 1
done
docker compose down -v
```

Chave: `wwz-key-demo`.

## Smoke

`./smoke.sh` (ou `make example NAME=indisponibilidade-janelas`, ~50 s). O relógio é o
do cabeçalho `Date` do próprio wwz.

- **Segundos:** sonda 1/s por 45 s (pelo menos 2 janelas). Nos segundos 1 a 3 de cada
  20, todas as chamadas dão 503 com envelope de erro e `retry-after` entre 1 e o que
  falta para a janela fechar; nos segundos 6 a 18, todas dão 200. Os segundos das
  bordas não contam (resolução de 1 s do `Date`).
- **Minutos e horas:** em vez de esperar até 6 h, é um teste de relógio. O smoke espera
  o segundo 5 a 54 do minuto e chama `janela-minutos` e `janela-horas` uma vez cada. Se
  o minuto atual for múltiplo de 10, exige 500; senão, 200. Se a hora UTC for múltipla
  de 6, exige 502; senão, 200. Uma execução cai dentro da janela de minutos em 10% das
  vezes e na de horas em 1/6 das vezes (o `retry-after`, se vier, é conferido). A
  validação do cron também passa pelo `wwz validate` do passo `examples`.

# anthropic-perfeito

Driver Anthropic que nunca falha e responde com latência realista. É a linha de
base para comparar com os outros exemplos: o roteador sob teste deve ver 100 % de
respostas 200 em `/v1/messages`, no formato exato da Messages API real.

## O que demonstra

- `errors: none` (`rate: 0`): nenhum erro HTTP, de rede ou de stream.
- Latência de um modelo hospedado de porte médio: TTFT normal com média de 400 ms
  (desvio 90 ms), fila lognormal (mediana 20 ms) e ITL de 15 ms por token.
- Respostas curtas (20 a 60 tokens) com `id` `msg_…`, `usage` completo e `model`
  igual ao snapshot `claude-sonnet-4-5-20250929`.
- `seed` fixo no YAML: as latências sorteadas se repetem de uma subida para outra.

## Como subir

```sh
make image                                         # white-wizard:dev (uma vez)
cd examples/anthropic-perfeito
EX_PORT=18101 WWZ_IMAGE=white-wizard:dev docker compose up -d --wait
curl -s http://127.0.0.1:18101/v1/messages \
  -H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \
  -d '{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"oi"}]}'
docker compose down -v
```

Porta do host: `EX_PORT` (padrão 18101). Chave: `wwz-key-01`. Modelo:
`claude-sonnet-4-5`. O control plane (`:9000`) não é publicado; o token
`wwz-admin-example` do compose só vale dentro do exemplo.

## O que esperar

- Sem stream: um JSON `type: message` com `id` `msg_…`, bloco `text`,
  `stop_reason: end_turn` e `usage` com `input_tokens`/`output_tokens`.
- Com stream: eventos `message_start`, `content_block_start`, `ping`, vários
  `content_block_delta`, `content_block_stop`, `message_delta` (com o `usage`
  final) e `message_stop`, nesta ordem.
- O primeiro byte chega ~400 ms depois do pedido (TTFT); o resto, a cada ~15 ms.

## Smoke

`make example NAME=anthropic-perfeito` (ou `./smoke.sh`) sobe o compose, envia 50
chamadas (25 sem stream, 25 com stream, 10 em paralelo) e confere:

| Conferência | Faixa | Por quê |
| --- | --- | --- |
| respostas 200 | exatamente 50 | `rate: 0` não admite exceção, não há sorteio de erro |
| formato Anthropic (`msg_…`, sequência de eventos, `usage`) | 0 falhas | fidelidade ao real |
| chamadas com TTFT 0 | 0 | o tempo até o primeiro byte vem do `curl` (`time_starttransfer`) |
| TTFT médio do stream | 200 a 800 ms | esperado ~420 ms; a média de 25 amostras tem desvio de ~22 ms, então os limites ficam a mais de 4 sigma. Sem stream o primeiro byte é a resposta inteira e não entra na média |

Ao fim derruba o compose com `down -v`, inclusive em falha.

# openai-perfeito

Driver OpenAI que nunca falha e responde com latência realista. É a linha de base
para comparar com os outros exemplos: o roteador sob teste deve ver 100 % de
respostas 200, no formato exato da Chat Completions real.

## O que demonstra

- `errors: none` (`rate: 0`): nenhum erro HTTP, de rede ou de stream.
- Latência de um modelo hospedado de porte médio: TTFT normal com média de 350 ms
  (desvio 80 ms), fila lognormal (mediana 20 ms) e ITL de 12 ms por token.
- Respostas curtas (20 a 60 tokens) com `id` `chatcmpl-…`, `usage` completo e
  `model` igual ao snapshot `gpt-4o-mini-2024-07-18`.
- `seed` fixo no YAML: as latências sorteadas se repetem de uma subida para outra.

## Como subir

```sh
make image                                         # white-wizard:dev (uma vez)
cd examples/openai-perfeito
EX_PORT=18100 WWZ_IMAGE=white-wizard:dev docker compose up -d --wait
curl -s http://127.0.0.1:18100/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"oi"}]}'
docker compose down -v
```

Porta do host: `EX_PORT` (padrão 18100). Chave: `wwz-key-01`. Modelo: `gpt-4o-mini`.
O control plane (`:9000`) não é publicado; o token `wwz-admin-example` do compose
só vale dentro do exemplo.

## O que esperar

- Chat sem stream: um JSON com `id` `chatcmpl-…`, `object: chat.completion`,
  `usage.total_tokens = prompt_tokens + completion_tokens`.
- Chat com stream (`stream_options.include_usage`): chunks `chat.completion.chunk`
  com o mesmo `id`, o último com `usage`, e `data: [DONE]`.
- O primeiro byte chega ~350 ms depois do pedido (TTFT); o resto, a cada ~12 ms.

## Smoke

`make example NAME=openai-perfeito` (ou `./smoke.sh`) sobe o compose, envia 50
chamadas (25 sem stream, 25 com stream, 10 em paralelo) e confere:

| Conferência | Faixa | Por quê |
| --- | --- | --- |
| respostas 200 | exatamente 50 | `rate: 0` não admite exceção, não há sorteio de erro |
| formato OpenAI (`id`, `object`, `usage`, `[DONE]`) | 0 falhas | fidelidade ao real |
| chamadas com TTFT 0 | 0 | o tempo até o primeiro byte vem do `curl` (`time_starttransfer`) |
| TTFT médio do stream | 200 a 700 ms | esperado ~370 ms; a média de 25 amostras tem desvio de ~20 ms, então os limites ficam a mais de 4 sigma. Sem stream o primeiro byte é a resposta inteira e não entra na média |

Ao fim derruba o compose com `down -v`, inclusive em falha.

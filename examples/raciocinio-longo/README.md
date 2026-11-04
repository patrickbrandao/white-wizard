# raciocinio-longo

Modelo (`wwz-reasoner`) que raciocina por vários segundos antes de responder. Serve
para testar timeouts, TTFT e contagem de tokens de um roteador diante de modelos de
raciocínio.

## O que demonstra

- Raciocínio de 300 a 400 tokens (`reasoning_tokens` com `probability: 100`),
  gasto a 8 ms por token (`reasoning_itl`): 2,4 a 3,2 s a mais antes do texto.
  `reasoning_effort` `low`/`medium`/`high` multiplica por 0,5 / 1 / 2 (spec 007).
- **OpenAI** (`reasoning_field: hidden`, como a Chat Completions real): o texto do
  raciocínio não é serializado; aparece só `usage.completion_tokens_details.reasoning_tokens`
  e o atraso até o primeiro byte do stream.
- **Anthropic** (formato fixo): com `thinking: {type: enabled, budget_tokens}` a
  resposta traz um bloco `thinking` antes do `text`, `thinking_delta` no stream e
  `usage.output_tokens_details.thinking_tokens`.
- Sem `reasoning_effort` / `thinking` o mesmo modelo responde na latência normal
  (TTFT ~300 ms), sem raciocínio.

## Como subir

```sh
make image                                         # white-wizard:dev (uma vez)
cd examples/raciocinio-longo
EX_PORT=18112 WWZ_IMAGE=white-wizard:dev docker compose up -d --wait
curl -s http://127.0.0.1:18112/v1/chat/completions \
  -H 'Authorization: Bearer wwz-key-01' -H 'Content-Type: application/json' \
  -d '{"model":"wwz-reasoner","reasoning_effort":"high","messages":[{"role":"user","content":"oi"}]}' \
  | jq .usage.completion_tokens_details
curl -s http://127.0.0.1:18112/v1/messages \
  -H 'x-api-key: wwz-key-01' -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \
  -d '{"model":"wwz-reasoner","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":2000},"messages":[{"role":"user","content":"oi"}]}' \
  | jq '[.content[].type], .usage.output_tokens_details'
docker compose down -v
```

Porta do host: `EX_PORT` (padrão 18112). Chave: `wwz-key-01`.

## O que esperar

| Pedido | Primeiro byte / resposta | Tokens |
| --- | --- | --- |
| OpenAI sem `reasoning_effort` | ~0,4 s | `reasoning_tokens` 0 |
| OpenAI `medium` | ~2,7 a 3,5 s | `reasoning_tokens` 300 a 400 |
| OpenAI `high` | ~6 a 9 s | 600 a 800 |
| Anthropic sem `thinking` | ~0,5 s | só bloco `text` |
| Anthropic com `thinking` | stream abre em ~0,3 s e o texto vem após ~3 s | `thinking_tokens` 300 a 400 |

## Smoke

`make example NAME=raciocinio-longo` (ou `./smoke.sh`) envia 6 pares de pedidos em
stream por dialeto (com e sem raciocínio) e um pedido sem stream de cada tipo, e confere:

| Conferência | Faixa | Por quê |
| --- | --- | --- |
| `reasoning_tokens` (OpenAI, `medium`) | 250 a 1000 | o YAML sorteia 300 a 400; o piso 250 deixa folga |
| `reasoning_tokens` sem `reasoning_effort` | 0 | sem pedido, sem raciocínio |
| texto do raciocínio na OpenAI | ausente | `hidden` |
| blocos Anthropic | `thinking,text` com `thinking`, `text` sem | formato real |
| `thinking_tokens` e `thinking_delta` | 250 a 1000 | idem |
| atraso médio com raciocínio, em ambos | pelo menos 1500 ms | o raciocínio custa no mínimo 300 x 8 ms = 2,4 s; ruído da média de 6 amostras é da ordem de 100 ms |

O atraso na OpenAI é o do primeiro byte do stream; na Anthropic o `message_start`
sai logo, então o smoke compara o tempo total (a resposta tem o mesmo tamanho
nos dois casos). Ao fim derruba o compose com `down -v`, inclusive em falha.

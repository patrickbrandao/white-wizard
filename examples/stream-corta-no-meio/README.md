# stream-corta-no-meio

30% dos streams são interrompidos no meio, sem o evento final. Serve para testar se o
roteador detecta stream incompleto (sem `[DONE]` / `message_stop`), descarta ou
retoma a resposta parcial e faz retry em outro provedor.

## O que demonstra

Erros no meio do stream têm sorteio próprio (spec
[009](../../specs/009-erros-e-caos.md)): `mid_stream_rate: 30` e
`mid_stream_types: [stream_truncated, stream_reset]`, cortando na metade dos chunks
(`after_ratio: 0.5`):

- `stream_truncated`: o servidor fecha a conexão sem `data: [DONE]` (OpenAI) nem
  `event: message_stop` (Anthropic). O cliente vê o fim da resposta, mas sem o final.
- `stream_reset`: TCP RST no meio do stream; o cliente recebe erro de transporte
  (no `curl`, "recv failure"/código 56) depois dos bytes parciais.

Os bytes recebidos antes do corte são válidos: cada frame `data:` completo é JSON. Uma
resposta sem `stream: true` nunca é cortada. `server.seed` fixo, com o mesmo aviso
do exemplo `falhas-20pct`: o destino depende do hash do primeiro prompt, então varie o
prompt (ou `x-wwz-conversation-id`) a cada chamada.

## Como subir

```bash
make image
cd examples/stream-corta-no-meio
EX_PORT=18109 docker compose up -d --wait
for i in $(seq 1 20); do
  curl -sN -o /tmp/s.txt -H 'Authorization: Bearer wwz-key-demo' -H 'Content-Type: application/json' \
    -d "{\"model\":\"gpt-4o-mini\",\"stream\":true,\"max_tokens\":40,\"messages\":[{\"role\":\"user\",\"content\":\"oi $i\"}]}" \
    http://127.0.0.1:18109/v1/chat/completions; echo "rc=$? DONE=$(grep -c '^data: \[DONE\]' /tmp/s.txt)"
done
docker compose down -v
```

Chave: `wwz-key-demo`. Modelos: `gpt-4o-mini` (OpenAI) e `claude-sonnet-4` (Anthropic,
`/v1/messages` com `x-api-key` e `anthropic-version`).

## Smoke

`./smoke.sh` (ou `make example NAME=stream-corta-no-meio`, ~40 s):

- 200 streams OpenAI: os sem `[DONE]` ficam em 30% +- 4 sigma (n = 200, sigma = 6,48:
  aceita 34 a 86);
- 100 streams Anthropic: os sem `message_stop` ficam em 12 a 48 (sigma = 4,58);
- nos cortados, todo frame `data:` completo é JSON válido e há pelo menos um frame
  (bytes parciais válidos);
- OpenAI: ocorrem cortes limpos (`stream_truncated`) e cortes com erro de transporte
  (`stream_reset`).

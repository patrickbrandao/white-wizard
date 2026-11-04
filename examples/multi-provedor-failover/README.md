# multi-provedor-failover

Um wwz fingindo ser vários provedores ao mesmo tempo, um deles instável. Serve
para medir se o roteador troca de provedor quando um falha e quanto tráfego perde.

## O que demonstra

- Os seis drivers montados por prefixo na mesma porta (spec
  [005](../../specs/005-drivers.md)), cada um no dialeto dele:

  | Prefixo | Base do SDK | Rota | Modelo |
  | --- | --- | --- | --- |
  | `/openai` | `<base>/openai/v1` | `/v1/chat/completions` | `gpt-4o-mini` |
  | `/anthropic` | `<base>/anthropic` | `/v1/messages` | `claude-sonnet-4` |
  | `/openrouter` | `<base>/openrouter/api/v1` | `/api/v1/chat/completions` | `openai/gpt-4o-mini` |
  | `/opencode` | `<base>/opencode/zen/go/v1` | `/zen/go/v1/chat/completions` | `kimi-k2` |
  | `/gemini` | `<base>/gemini` (google-genai, `x-goog-api-key`) | `/v1beta/models/gemini-2.5-flash:generateContent` | `gemini-2.5-flash` |
  | `/azure` | `<base>/azure` (`AzureOpenAI`, `api-key`, `api-version=2024-10-21`) | `/openai/deployments/gpt-4o-mini/chat/completions` | deployment `gpt-4o-mini` |

- **O OpenRouter é o instável**: o modelo que só ele serve tem `errors.rate: 30`
  (429/500/503 em pesos 1/2/2). Cada modelo tem `models[].drivers`
  (catálogo, spec [003](../../specs/003-configuracao.md)), então a instabilidade
  não vaza para os outros prefixos.
- `server.seed` fixo: o sorteio depende do primeiro prompt; varie o prompt.
- No Azure o deployment é o `id` do modelo (spec [005](../../specs/005-drivers.md)): o
  `gpt-4o-mini` está nos catálogos do `openai` e do `azure`. O driver Azure segue
  a documentação, sem chave real ([ADR 0043](../../specs/decisions/0043-drivers-gemini-e-azure.md)).

## Como subir

```bash
make image                                   # uma vez: white-wizard:dev
cd examples/multi-provedor-failover
EX_PORT=18113 docker compose up -d --wait    # porta 18113 no host
curl -s -H 'Authorization: Bearer wwz-key-demo' -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"oi 1"}]}' \
  http://127.0.0.1:18113/openrouter/api/v1/chat/completions
docker compose down -v
```

Chave: `wwz-key-demo`.

## O que esperar

Cerca de 30% de erro em `/openrouter` (com prompts diferentes) e 0% nos outros
cinco prefixos.
Um cliente que tenta o OpenRouter e cai para o OpenAI perde 0 requisições, e cerca
de 30% passam pelo fallback.

## Smoke

`./smoke.sh` (ou `make example NAME=multi-provedor-failover`, ~40 s):

- Uma chamada em cada prefixo, conferindo o formato do provedor (`chatcmpl-`,
  `msg_`, `provider` do OpenRouter, `candidates` e `usageMetadata` do Gemini,
  `prompt_filter_results` e `content_filter_results` do Azure).
- 300 chamadas ao OpenRouter: de 58 a 122 erros (30% +- 4 sigma, sigma = 7,9),
  só 200/429/500/503. 100 chamadas a cada um dos outros cinco: todas 200.
- Failover OpenRouter -> OpenAI em 300 requisições: 0 perdidas e de 58 a 122
  fallbacks.
- Derruba o compose com `down -v`, inclusive em falha.

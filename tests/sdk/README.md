# Testes de SDK

Verificam que os SDKs oficiais conversam com o wwz sem ajuste (spec 018, "Testes de SDK"). Ambos usam a mesma configuração, `tests/sdk/wwz.yaml`.

## Configuração (`wwz.yaml`)

| Driver | Prefixo | `base_url` do SDK |
| --- | --- | --- |
| `openai` | `/openai` | `<base>/openai/v1` |
| `anthropic` | `/anthropic` | `<base>/anthropic` |
| `openrouter` | `/openrouter` | `<base>/openrouter/api/v1` |
| `opencode` | `/opencode` | `<base>/opencode/zen/go/v1` |
| `gemini` | `/gemini` | `<base>/gemini` (o SDK monta `<base>/gemini/v1beta/...`) |
| `azure` | `/azure` | `AzureOpenAI(azure_endpoint=<base>/azure, api_version=2024-10-21)` (rotas por deployment) ou `<base>/azure/openai/v1` (API v1) |
| `anthropic` com `client_verification: claude-code` | `/anthropic-cc` | `<base>/anthropic-cc` (só o Claude Code passa; spec 020) |

- Modelos com latência zero: `wwz-fast` (texto), `wwz-tools` (tool call com probabilidade 100), `wwz-reason` (raciocínio sempre) e `wwz-rl` (alvo do 429).
- `opencode` (task 425, ADR 0072): `wwz-free` (gratuito, só `opencode`), `free_requests_per_day: 2`, `trusted_proxies` de loopback (cada teste manda um `X-Forwarded-For` próprio, para a cota diária por IP) e a chave `sk-wwz-oc-extra` (cota Go esgotada na 1ª requisição, `extra_usage`).
- Chaves: `sk-wwz-sdk` (sem limites, `WWZ_SDK_KEY`) e `sk-wwz-sdk-rl` (só `wwz-rl`, `rpm: 1`; trocável por `WWZ_SDK_RL_KEY`). O `rpm` é da chave, então os drivers dividem a mesma janela: o teste de 429 faz até duas chamadas.
- `server.seed: 42`: a mesma requisição devolve o mesmo texto, o que permite comparar o stream montado com o `content` da resposta sem stream.

## Python (`python/`)

Só o `tmp/venv` do repositório, sem `pip install` (versões em `python/requirements.txt`).

```sh
go run ./cmd/wwz serve --config tests/sdk/wwz.yaml   # data plane em 127.0.0.1:8000
WWZ_SDK_BASE_URL=http://127.0.0.1:8000 WWZ_SDK_KEY=sk-wwz-sdk \
  PYTHONDONTWRITEBYTECODE=1 tmp/venv/bin/python -m pytest -q -p no:cacheprovider tests/sdk/python
```

Use `--listen`/`--admin-listen` em `wwz serve` para outra porta e ajuste `WWZ_SDK_BASE_URL`. Sem `WWZ_SDK_BASE_URL` e `WWZ_SDK_KEY` todos os testes saem como "pulado".

Cobertura por driver (`openai`, `openrouter` e `opencode` pelo SDK `openai`; `anthropic` pelo SDK `anthropic`; `gemini` pelo `google-genai`, em `test_gemini.py`; `azure` pelo `openai` com `AzureOpenAI`, em `test_azure.py`):

- chat sem stream e com stream (texto montado igual ao `content`, `usage` coerente; helpers `chat.completions.stream` e `messages.stream`);
- tool call com argumentos desserializados, com e sem stream;
- raciocínio no campo do dialeto (ADR 0022): oculto no `openai` (só `reasoning_tokens`), `reasoning_content` no `opencode`, `reasoning` no `openrouter`, bloco `thinking` com `signature` no `anthropic`;
- `401` com chave inválida como `AuthenticationError` e `429` como `RateLimitError` com `retry-after` legível (`max_retries=0`);
- `GET /v1/models` pelo SDK.
- `opencode` sem conta (`test_opencode_free.py`, `opencode_free.test.mjs`): `api_key="public"` com `User-Agent: opencode/...` (só contra o wwz; o teste recusa `opencode.ai`) → 200 `billing_source: free`; sem esse header → `RateLimitError` `FreeUsageLimitError` com `retry-after` até 00:00 UTC; modelo pago → `AuthenticationError` `AuthError`; base Go → `AuthenticationError`; a 3ª chamada do dia → `RateLimitError`; cota Go esgotada com Extra Usage → `billing_source: balance`.
- Responses API (`test_responses.py`, task 239) em `openai`, `openrouter` e `opencode`: `responses.create` com e sem stream (texto montado igual, `sequence_number` contínuo), helper `responses.stream`, `incomplete` por `max_output_tokens`, tool com e sem stream, conversa encadeada mandando os itens no `input`, `previous_response_id` (no `openai` a cadeia entra no prompt; nos gateways 400, como o real), `store: true` (recusado nos gateways) e 401.
- `gemini`: `generate_content` com e sem stream (texto montado igual), `function_calls`, partes `thought` com `include_thoughts`, `count_tokens`, `models.list`/`models.get`, 400 `INVALID_ARGUMENT` de chave inválida, 404 de modelo, 429 `RESOURCE_EXHAUSTED` e o ciclo de `caches` (criar, ler, gerar com `cached_content`, atualizar, apagar).
- Modo estrito do cliente (`test_claude_code_strict.py`, `claude_code_strict.test.mjs`; task 447, spec 020): `messages.create` e `beta.messages.create` no `/anthropic-cc`, com `x-api-key` e com `Authorization: Bearer` → `PermissionDeniedError` com `forged_request_error` no corpo e `x-wwz-client-verification: failed; reason=query` (os SDKs não acrescentam `?beta=true`); no `/anthropic` → 200. O `/anthropic-cc` tem `oauth_key: sdk-oauth` (chave `sk-wwz-sdk-oauth-secret`).
- `azure`: chat com e sem stream (primeiro evento só com `prompt_filter_results`, texto montado igual), `prompt_filter_results` e `content_filter_results`, helper de stream, tool call, raciocínio oculto, `completions` legado, 401, 404 `DeploymentNotFound`, 400 de argumento desconhecido, 429 com `retry-after-ms`, `models.list` e a API v1 (`chat` e `responses` com e sem stream).

Divergências entre SDK e wwz ficam como `xfail` com `reason="bug: ..."` até a correção no código Go.

## Node (`node/`)

Mesma matriz com `node --test` (Node 22+) e os SDKs fixados em `node/package-lock.json` (`openai` 7.46.0, `@anthropic-ai/sdk` 1.3.0, `@google/genai` 2.32.0). `node_modules/` fica fora do git; num worktree sem `node_modules`, o `scripts/verify.sh full` liga temporariamente o do checkout principal quando o `package-lock.json` é igual (um symlink manual também serve). A comparação é pelo conteúdo do lock, não pelo mtime, e o verify nunca roda `npm ci` através de um `node_modules` em symlink: desatualizado, o passo falha pedindo o `npm ci` no checkout principal. Se faltar pacote: `npm ci --prefer-offline` em `tests/sdk/node` e depois `npm cache clean --force`.

```sh
go run ./cmd/wwz serve --config tests/sdk/wwz.yaml   # data plane em 127.0.0.1:8000
WWZ_SDK_BASE_URL=http://127.0.0.1:8000 WWZ_SDK_KEY=sk-wwz-sdk node --test tests/sdk/node/
```

Os mesmos `WWZ_SDK_BASE_URL`, `WWZ_SDK_KEY` e `WWZ_SDK_RL_KEY` da suíte Python; sem os dois primeiros as suítes saem como "puladas".

- `helpers.mjs`: clientes (`maxRetries: 0`), mounts por driver e leitura de `retry-after`.
- `openai_compat.test.mjs`: SDK `openai` contra `openai`, `openrouter` e `opencode` (uma suíte por driver), mais `reasoning_details[]` do `openrouter`. O SDK Node preserva campos fora do tipo, então os dialetos de raciocínio são lidos direto do objeto.
- `anthropic.test.mjs`: SDK `@anthropic-ai/sdk` contra `anthropic`, inclusive `stream: true` com eventos brutos e `models.retrieve`.
- `gemini.test.mjs`: SDK `@google/genai` contra `gemini`, a mesma matriz da suíte Python.
- `azure.test.mjs`: `AzureOpenAI` do SDK `openai` contra `azure`, a mesma matriz da suíte Python.
- `responses.test.mjs`: Responses API do SDK `openai` contra `openai`, `openrouter` e `opencode`, a mesma matriz de `test_responses.py`.

Divergências entre SDK e wwz ficam como `test(..., { todo: 'bug: ...' })` até a correção no código Go.

# wwz-drift

Compara a **forma** das respostas dos drivers do wwz com a das APIs reais (I24) e acusa a divergência. A comparação cobre caminhos JSON com tipo e ordem, a sequência de eventos SSE, os nomes de headers relevantes e o status, nunca os valores, porque o simulador inventa o conteúdo. A referência normativa é a seção "Contrato do `wwz-drift`" da [spec 017](../specs/017-recursos-importados.md), com as ADRs [0035](../specs/decisions/0035-wwz-drift-comparacao-de-forma.md), [0042](../specs/decisions/0042-fidelidade-total-ao-provedor-real.md) e [0049](../specs/decisions/0049-formas-reais-do-primeiro-drift.md). A comparação byte a byte contra fixtures gravadas (`wwz-drift record`, `testdata/real/`) é da fase 5 ([ADR 0014](../specs/decisions/0014-fidelidade-bit-a-bit.md)).

Cada execução de `check` faz chamadas reais e pagas aos provedores. Rode-a só na sua máquina ou no CI com secrets, nunca no servidor pesado.

## Subcomandos

| Subcomando | O que faz |
| --- | --- |
| `check` (padrão) | Sondas ao vivo, real × wwz, e relatório de divergências |
| `record` | As mesmas sondas, só no real, gravadas como fixtures normalizadas em `testdata/real/` (ver [Gravação de fixtures](#gravação-de-fixtures-record)) |
| `version`, `help` | Versão (com commit e Go) e ajuda |

## Chaves: só no ambiente

As chaves reais nunca entram no repositório nem em arquivo do projeto. O `check` lê, nesta ordem de precedência:

1. a variável de ambiente (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`, `OPENCODE_API_KEY` (também do `opencode-google`), `GEMINI_API_KEY` (ou `GEMENI_API_KEY`) e `AZURE_OPENAI_API_KEY`);
2. o arquivo `CHAVE=valor` de `--keys-file`, por padrão `~/.config/white-wizard/real-keys.env`, fora do repositório.

O arquivo padrão ausente não é erro. Ausente ou ilegível quando passado explicitamente com `--keys-file`, dá código 1. Provedor sem chave sai `skipped` com o motivo `no_key`.

A chave nunca aparece em stdout, stderr, relatório, `--save-raw` nem em mensagens de erro: é trocada por `***`. No CI (`.github/workflows/drift.yml`), as chaves vêm dos secrets do repositório, configurados pelo mantenedor, nunca por um agente.

## Orçamento

- Teto de **US$ 20 por provedor** (`--budget-usd`, [ADR 0042](../specs/decisions/0042-fidelidade-total-ao-provedor-real.md)), acumulado em `tmp/real-spend.json` (`--budget-file`), o mesmo arquivo da gravação de fixtures. O formato é `{"<provedor>": {"usd", "calls", "updated"}}`; arquivo ausente = gasto zero.
- Antes de cada provedor, o `check` lê o arquivo. Com `usd ≥ --budget-usd`, o provedor sai `skipped` com o motivo `budget_exhausted`.
- Depois de cada sonda paga (`chat`, `chat_stream`, `responses`, `responses_stream`), soma o custo estimado (usage × tabela de preços de `internal/drift`; modelo sem preço = US$ 0,01 por chamada) e regrava o arquivo de forma atômica. As sondas usam `max_tokens: 16` (`opencode`: 256, para o modelo terminar o raciocínio e responder, senão o `finish_reason` do real varia entre `length` e `stop`), cerca de US$ 0,0001 cada.

## Sondas fixas

Cada sonda vai ao real e ao wwz com o mesmo corpo e o mesmo modelo.

| Sonda | Requisição | Custo |
| --- | --- | --- |
| `models` | `GET` da lista de modelos (compara o topo e só o item do modelo da sonda) | grátis |
| `chat` | Chat sem stream, `max_tokens: 16`, mensagem `Responda só: ok` | ~US$ 0,0001 |
| `chat_stream` | O mesmo com `stream: true` (OpenAI com `stream_options.include_usage`) | ~US$ 0,0001 |
| `error_auth` | Chat com a chave `sk-invalid-wwz-drift` | grátis |
| `error_model` | Chat com o modelo `wwz-drift-modelo-inexistente` | grátis |
| `responses` | `POST .../responses` sem stream, `input: "Responda só: ok"`, `max_output_tokens: 16` (`opencode`: 256) | ~US$ 0,0001 |
| `responses_stream` | O mesmo com `stream: true` | ~US$ 0,0001 |

As sondas `responses` e `responses_stream` (task 239, [ADR 0055](../specs/decisions/0055-contrato-da-responses-api.md)) vão à Responses API de quem a tem: `openai` (`/v1/responses`), `openrouter` (`/api/v1/responses`), `opencode` (`<base>/openai/v1/responses`) e `azure` (API v1, `/openai/v1/responses`, sem `api-version`). Nos demais (`anthropic`, `gemini`, `opencode-google`) saem `skipped` com o motivo `not_supported`. O wwz embutido responde curto (1 a 3 tokens) nos modelos de `openai` e `openrouter`, como o real responde à sonda: com mais de 16 tokens o stream do wwz terminaria em `response.incomplete` e o do real em `response.completed`.

| Provedor | Modelo padrão da sonda |
| --- | --- |
| `openai` | `gpt-4o-mini` |
| `anthropic` | `claude-haiku-4-5-20251001` |
| `openrouter` | `openai/gpt-4.1-nano` |
| `opencode` | `deepseek-v4.1-flash` |
| `gemini` | `gemini-3.5-flash-lite` |
| `opencode-google` | `deepseek-v4.1-flash` (rota Gemini do OpenCode Go, traduzida) |
| `azure` | `gpt-4o-mini` (deployment = id do modelo) |

**Gemini** (`gemini`): `models/{m}:generateContent` e `:streamGenerateContent?alt=sse`, chave em `x-goog-api-key`, lista com `pageSize=1000`.

**OpenCode Go, rota Gemini** (`opencode-google`): `<base>/google/v1beta/models/{m}:generateContent` em `https://opencode.ai/inference/go/google`, com a chave, o `User-Agent` e a sessão do `opencode` (a mesma sessão da execução) e `maxOutputTokens: 256`. O gateway não tem lista de modelos nessa rota: a sonda `models` compara o 404 sem corpo do real.

**Azure** (`azure`): `/openai/deployments/{m}/chat/completions?api-version=2024-10-21` e `/openai/models?api-version=2024-10-21`, chave em `api-key`. Não há chave real nem base pública ([ADR 0043](../specs/decisions/0043-drivers-gemini-e-azure.md)): o `azure` fica fora do padrão de `--providers` e só é sondado com `--base-url azure=URL` (um provedor falso) e `AZURE_OPENAI_API_KEY`; sem base sai `skipped`/`no_key`, mesmo com `--real`.

O mesmo id vai aos dois lados, e `--model P=M` troca o modelo nos dois. Sem `--wwz`, o `check` sobe o wwz em processo em `127.0.0.1:0`, com a configuração embutida [internal/drift/wwz.yaml](../internal/drift/wwz.yaml), que monta os drivers sondados com esses ids.

## Flags de `check`

As flags conferem com `wwz-drift check -h`.

| Flag | Padrão | Significado |
| --- | --- | --- |
| `--providers LISTA` | `openai,anthropic,openrouter,opencode,gemini,opencode-google` | Provedores a sondar (`azure` só com `--base-url`) |
| `--keys-file ARQ` | `~/.config/white-wizard/real-keys.env` | Arquivo de chaves (o ambiente vence) |
| `--wwz URL` | em processo | Data plane do wwz a comparar |
| `--wwz-key K` | `wwz-key-drift` | Chave enviada ao wwz |
| `--model P=M` | tabela acima | Modelo da sonda por provedor (repetível) |
| `--base-url P=URL` | URLs reais | Troca a base do provedor real (os testes usam `httptest`) |
| `--real` | desligado | Confirma sondar a base real dos provedores que têm chave e não têm `--base-url`; sem ele o provedor sai `skipped` (`real_not_confirmed`) e nenhuma chamada vai ao real |
| `--probes LISTA` | `models,chat,chat_stream,error_auth,error_model,responses,responses_stream` | Subconjunto das sondas |
| `--ignore ARQ` | — | YAML de divergências aceitas |
| `--budget-file ARQ` | `tmp/real-spend.json` | Gasto acumulado por provedor |
| `--budget-usd N` | `20` | Teto por provedor (US$) |
| `--timeout D` | `60s` | Teto por sonda |
| `--save-raw DIR` | — | Grava as respostas brutas, sem headers de autenticação; `DIR` precisa estar sob `tmp/` |
| `--output text\|json` | `text` | Formato do relatório |
| `--out ARQ` | stdout | Arquivo do relatório |

```sh
wwz-drift check -h                  # lista as flags; não faz chamadas
wwz-drift check --providers nenhum  # uso inválido, código 2, antes de qualquer chamada
# wwz-drift check: uso inválido: provedor "nenhum" desconhecido (use openai,anthropic,openrouter,opencode,gemini,opencode-google,azure)

# Chamadas reais (não executado ao escrever este guia): só as sondas grátis de um provedor
wwz-drift check --real --providers anthropic --probes models,error_auth,error_model \
  --output json --out tmp/drift.json
```

### Divergências aceitas (`--ignore`)

```yaml
- provider: openai        # vazio ou "*" = qualquer; "pre*" = prefixo
  probe: chat_stream
  kind: missing_field     # vazio = qualquer kind
  path: "events[*"        # o mesmo curinga de sufixo
  reason: "campo experimental, fora do contrato"   # obrigatório
```

As entradas se somam à lista embutida (hoje vazia). Divergências que casam aparecem no relatório com `ignored: true` e não contam.

## Relatório e códigos de saída

| `kind` | Quando |
| --- | --- |
| `status_mismatch` | Status HTTP diferente |
| `missing_field` / `extra_field` | Caminho só no real / só no wwz |
| `type_mismatch` | Mesmo caminho com tipos incompatíveis (`null` é compatível com qualquer tipo) |
| `order_mismatch` | Mesmos campos irmãos em outra ordem |
| `event_sequence` | Evento SSE sem par no outro lado, depois do alinhamento (um por evento, todos listados) |
| `missing_header` / `extra_header` | Header relevante (`x-ratelimit-*`, `anthropic-ratelimit-*`, `retry-after*`, `request-id`, `openai-*`…) só num dos lados |

O JSON (`--output json`, `kind: "drift"`, `version: 1`) traz `wwz` (`url`, `version`), `providers[]` com `status`, `reason`, `base` (a base sondada do real; vazia se não selecionado), `model`, `session` (só no `opencode`), `spend_usd` e `probes[]` (`real_status`, `wwz_status`, `divergences[]` com `kind`, `path`, `real`, `wwz`, `ignored`), e o `summary` com a contagem por status. O status do provedor e da sonda é `ok`, `drift`, `skipped` (`no_key`, `real_not_confirmed`, `budget_exhausted`, `not_selected`, `not_supported` para a sonda sem rota no provedor e, no `opencode`, `upstream_form_unreproduced`) ou `error` (rede, timeout ou resposta ilegível, sem segredos no `reason`). O texto lista um bloco por provedor e uma divergência por linha; não é contrato.

**OpenCode e o upstream da sessão (ADR 0059).** O gateway encaminha cada `x-opencode-session` a um upstream, e o `id` do Chat muda de forma (e de corpo) com ele: `gw-<12 hex>-<6 hex>` (DeepSeek, o que o wwz reproduz), 32 hex, `chatcmpl-<16 hex>` e ids longos de outros upstreams, observados no real. A sonda usa uma sessão por execução (`ses_<64 hex>`, a mesma em todas as sondas, no relatório em `providers[].session`); o `id` nunca é divergência. Se o `chat` real (2xx) não vier na forma `gw-`, a sonda tenta até 3 sessões novas e, se nenhuma devolver `gw-`, o provedor sai `skipped`/`upstream_form_unreproduced` com código 0, em vez de acusar drift que o wwz não pode eliminar. Cada tentativa é uma chamada paga (centavos) e entra no gasto.

| Código | Significado |
| --- | --- |
| `0` | Nenhuma divergência (só `ok`/`skipped`) |
| `3` | Alguma divergência não ignorada (vence o `1`) |
| `1` | Sem divergência, mas alguma sonda em `error`, ou falha de execução (wwz inalcançável, `--keys-file` explícito ilegível, `--budget-file` não gravável) |
| `2` | Uso inválido (inclusive `--save-raw` fora de `tmp/`) |

## Gravação de fixtures (`record`)

`wwz-drift record` manda as sondas do `check` só ao provedor real e grava cada resposta, normalizada, como fixture no formato que os testes `TestRealFixtures*` leem ([ADR 0014](../specs/decisions/0014-fidelidade-bit-a-bit.md), seção "Fixtures reais" da [spec 018](../specs/018-verificacao-ci-release.md)): `<out>/<provedor>/shape/<sonda>.json`, e `<out>/opencode/google/<sonda>.json` para o `opencode-google`. Uma gravação nova sobrescreve a fixture de mesmo nome; confira o `git diff` antes de commitar.

Valem as travas do `check`: chave no ambiente ou no arquivo, `--real` para a base real (ou `--base-url`), orçamento lido antes de cada provedor e custo somado depois de cada sonda paga. As flags são as do `check` que dizem respeito ao real (`--providers`, `--keys-file`, `--model`, `--base-url`, `--real`, `--probes`, `--budget-file`, `--budget-usd`, `--timeout`) mais `--out DIR` (padrão `testdata/real`).

Cada arquivo traz `provider`, `name` (a sonda), `recorded_at` (data UTC), `note`, `request` (`method`, `path` com o caminho da base, `headers` sem `Accept`/`User-Agent`, `body` exato ou `null` no `GET`) e `response` (`status`, `headers`, `body`). A normalização:

- credenciais: `Bearer <key>`, `<key>` e `<session>` (o `error_auth` guarda a chave inválida literal); a chave real nunca chega ao arquivo;
- headers: só os do dialeto (os relevantes do `check`, `content-type`, `cache-control`, `access-control-*`, os do front-end do Google no dialeto Gemini e `content-length` de corpo vazio); `request-id`/`x-request-id` → `<request_id>` (no OpenRouter, `<generation_id>`), `x-generation-id`, `x-opencode-log-id`, contas (`<organization_id>`, `<workspace_id>`, `<project_id>`), `anthropic-ratelimit-*-reset` → `<rfc3339>`, `x-ratelimit-reset-*` → `<duration>`, `openai-processing-ms` e `server-timing` → `<ms>`;
- corpo: ids de mensagem, geração, chamada, item e resposta (`<msg_id>`, `<generation_id>`, `<gw_id>`, `<id>`, `<item_id>`, `<call_id>`, `<tool_id>`), `request_id`, `user_id`, `system_fingerprint`, assinaturas de raciocínio, `responseId` e tempos do Gemini; o conteúdo gerado e as contagens de tokens ficam, porque os testes comparam o wwz configurado com eles. O preenchimento de espaços do SSE da Anthropic sai. A lista de modelos acima de 64 KiB (OpenRouter) fica só com o item da sonda e `total_count` ajustado, e o `note` registra o tamanho real.

```sh
# Uma gravação mínima (5 sondas da Anthropic, ~US$ 0,0001), fora do testdata para revisar antes
wwz-drift record --real --providers anthropic --out tmp/record
```

Códigos de saída: `0` tudo gravado (ou `skipped`), `1` sonda em erro ou falha de execução (orçamento ilegível ou não gravável), `2` uso inválido.

## Agendamento

- **CI:** [.github/workflows/drift.yml](../.github/workflows/drift.yml) roda todo dia às 06:00 UTC e por `workflow_dispatch`, compila o `wwz-drift`, roda `check --output json --out tmp/drift.json` com as chaves dos secrets, publica o relatório como artefato e falha o job no código 3. Fica inativo até existir o remoto ([ADR 0018](../specs/decisions/0018-publicacao-local.md)).
- **Local:** o passo `drift` do `make verify-heavy` roda o `check` no Mac (nunca no servidor) e grava `tmp/heavy/<carimbo>/drift.json`. Ele é pulado com `HEAVY_DRIFT=0` ou quando não há arquivo de chaves nem variáveis. Ver [verificacao.md](verificacao.md#verify-heavy).

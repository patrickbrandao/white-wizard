# 005 — Drivers de provedor

Status: rascunho
Depende de: 001, 004
Origem: seções 17 do documento Claude Docs congelado em 2026-10-30

## Contexto
Interface comum de driver, divisão driver × núcleo e registro de novos drivers (seção 17).

## Requisitos
- R15

## Interfaces e contratos
- `internal/driver` (`driver.Register`)
- `docs/drivers.md` (derivado)

## Dados
- Política de entidades por driver (níveis IP/chave/usuário/conta)

## Critérios de aceite
- [ ] Suíte `drivertest` passa para openai, anthropic, opencode, openrouter, gemini e azure
- [ ] Driver novo não altera o núcleo (validação e opções por interface opcional; teste de fronteira de imports)
- [ ] Na raiz sem prefixo, a detecção por credencial e caminho escolhe o driver certo para cada SDK oficial (`openai`, `AzureOpenAI`, `anthropic`, `google-genai`)
- [ ] Nenhum driver tem opção que o faça divergir do provedor real (ADR 0042), exceto `client_verification` do `anthropic` (exceção pedida pelo humano, [ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md))

## Fora de escopo
- Bedrock, Ollama, LiteLLM, Vertex AI (candidatos futuros)

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0042-fidelidade-total-ao-provedor-real.md](decisions/0042-fidelidade-total-ao-provedor-real.md)
- [specs/decisions/0043-drivers-gemini-e-azure.md](decisions/0043-drivers-gemini-e-azure.md)
- [specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)
- [specs/decisions/0068-lote-de-specs-da-auditoria-do-done.md](decisions/0068-lote-de-specs-da-auditoria-do-done.md)
- [specs/decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)
- [specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### 17. Drivers de provedor

Cada provedor simulado é um driver Go que implementa uma interface única; o núcleo (gerador, latência, erros, limites em Valkey, métricas) não sabe qual provedor está atendendo. Os drivers do MVP são `openai`, `anthropic`, `opencode`, `openrouter`, `gemini` e `azure` ([ADR 0043](decisions/0043-drivers-gemini-e-azure.md)); um driver novo é um pacote novo registrado em uma linha. Todo driver é o provedor real: nenhuma opção o faz divergir dele ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md)); as condições que o operador configura (latência, erros, limites, cenários) saem na forma exata do provedor.

**O que o driver decide e o que o núcleo decide**

| Responsabilidade | Driver | Núcleo |
| --- | --- | --- |
| Rotas e versão da API (`/v1/chat/completions`, `/v1/messages`, `/api/v1/...`, `/v1beta/models/{m}:generateContent`, `/openai/deployments/{d}/...?api-version=`) | Sim |  |
| Parsing do corpo para o modelo interno `Request` (mensagens, tools, tool\_choice, max\_tokens, stream, reasoning) | Sim |  |
| Autenticação: de onde vem a credencial (`Authorization: Bearer`, `x-api-key`, `x-goog-api-key`/`?key=`, `api-key`), headers e parâmetros obrigatórios (`anthropic-version`, `api-version`) | Sim |  |
| Regras de cache de prompt do provedor (automático por prefixo, `cache_control`, `cachedContents`) e onde o `usage` as informa | Sim (declara) | Sim (prefixos no store) |
| Identidade para limites: que entidade conta (chave, projeto, organização, conta, IP) e quais unidades (RPM, TPM, ITPM/OTPM, créditos) | Sim (declara) | Sim (aplica em Valkey) |
| Serialização de sucesso: JSON não-streaming, eventos SSE, `usage`, `finish_reason`/`stop_reason` | Sim |  |
| Serialização de erro: código HTTP, corpo, headers de rate limit e `retry-after` no dialeto do provedor | Sim |  |
| `/v1/models` no formato do provedor | Sim |  |
| Sortear conteúdo, tool calls, raciocínio, tamanho |  | Sim |
| Agendar latência, injetar erros, escolher passo de cenário |  | Sim |
| Contar, bloquear, banir, expirar (Valkey) |  | Sim |
| Métricas e painel |  | Sim |

**Interface** (esboço do pacote `internal/driver`)

```go
type Driver interface {
    Name() string                              // "openai", "anthropic", ...
    Routes() []Route                           // método, caminho, handler lógico
    Authenticate(r *http.Request) (Principal, *ErrorOut)
    ParseRequest(r *http.Request, body []byte) (*core.Request, *ErrorOut)
    LimitPolicy() LimitPolicy                  // entidades e unidades que este provedor limita
    WriteResponse(w core.ResponseWriter, resp *core.Response) error    // não-streaming
    StreamWriter(w core.ResponseWriter, req *core.Request) core.StreamWriter // SSE
    WriteError(w core.ResponseWriter, e core.Error, rl *core.RateLimitState)
    ListModels(models []config.Model) any
}

type Principal struct {
    Key, Account, User, Org, Project string   // o que o driver conseguiu identificar
    IP      string                             // sempre preenchido pelo núcleo (X-Forwarded-For confiável ou TCP)
    Tier    string
}
```

**Validação e opções sem tocar o núcleo ([ADR 0068](decisions/0068-lote-de-specs-da-auditoria-do-done.md)).** Além da interface `Driver`, o pacote `internal/driver` define interfaces opcionais, que o núcleo descobre por asserção de tipo (`driver.As`, que atravessa wrappers) e nunca por nome de driver: (a) um validador de requisição, com que o driver recusa o corpo com o texto real (spec 004) antes do gerador; (b) um declarador de opções, com que o driver lista as opções de `drivers[].options` que aceita, o valor padrão e a regra de validação de cada uma (`reasoning_field`, `require_session`, `require_version`). O driver as publica no registro (`driver.Register`) junto da fábrica; `internal/config` e `internal/server` consultam o registro e não importam pacotes de driver nem têm mapa por nome. Driver sem a interface opcional não valida nem aceita opções (opção desconhecida torna a configuração inválida). Teste de fronteira: o núcleo não importa `internal/driver/<nome>`.

**Assinatura e verificação do cliente ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)).** Mais três interfaces opcionais, descobertas da mesma forma, nenhuma ligada ao nome `anthropic`: (a) **cabeça crua**: o driver diz, por montagem, se precisa da linha e dos headers crus da requisição (spec 004, "Cabeça crua"); o servidor só os copia quando ele diz que sim; (b) **verificador de cliente**: recebe a cabeça crua e o corpo cru de uma rota de inferência e devolve aceite ou um erro de cliente falsificado (`core` ganha o tipo de erro `forged_request`, que o driver escreve como 403 `forged_request_error`, spec 020); o engine chama antes da credencial e não conta nada no store; (c) **cota de assinatura**: o driver pede, para uma requisição autenticada por token, a visão das janelas de 5 h e semanal da `quota_usd` da conta (gasto e reset), que o engine lê do store e entrega na escrita da resposta, para os headers `anthropic-ratelimit-unified-*` (spec 006, 16.2.1). O token OAuth usa o `KeyResolver` que já existe (Console do `opencode`).

Nomes da interface (c) em `internal/driver` (task 443):
- `driver.TokenChecker` (`CheckToken(kind, cred, h, keys, now) *core.Error`): as recusas de um token além de "inválido" (vencido, sem o beta, sem o escopo da rota, no header errado). O engine pergunta com `server.auth` ligado, depois do `KeyResolver`, e só para credencial que não é uma entrada de `keys[]` pelo valor; `nil` mantém o veredito do engine.
- `driver.SubscriptionReporter` (`Subscription(kind, cred) bool`): se a resposta a essa credencial leva as janelas da assinatura.
- Quando o driver diz que sim, o engine lê o estado da conta no store antes do `Check` dos limites (uma leitura, só para essas requisições), monta a visão com `driver.NewSubscriptionView(quota_usd, estado, agora)` e a guarda na requisição com `driver.SetSubscription`; o driver lê com `driver.SubscriptionOf` na escrita do 200 e do 429.
- `driver.SubscriptionView` tem as janelas de 5 h e semanal (gasto, cota, reset); `driver.NextWeek` é a virada da semana (segunda 00:00 UTC).

**Catálogo e cobrança por rota ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md)).** Mais duas interfaces opcionais, descobertas da mesma forma:
- **(c) Catálogo da rota.** O driver diz qual catálogo de `models[].catalogs` (spec 003) uma rota serve. O engine filtra a listagem e a aceitação de `model` por ele e, numa rota com catálogo, usa o modelo efetivo do catálogo (`pricing`, `context_window`, `max_output`, `capabilities` e `extra` do catálogo sobre os do modelo; spec 003) no custo, nas verificações e no `driver.ModelInfo` que o driver lista.
- **(d) Origem da cobrança.** Dados a requisição, a credencial, a chave, o modelo efetivo e o tier da conta, o driver devolve:
  - a origem (`free`, `balance`, `go` ou `go-plus`);
  - o fator de cota (`allowance_factor`);
  - se a credencial é o modo sem conta (`public`);
  - a cota diária de gratuitos;
  - a recusa que vem antes do `Check` (plano `none` na base Go, gratuito fora do cliente OpenCode, `public` com modelo pago ou base Go).

  O engine monta os sujeitos e o débito a partir disso (spec 010). Hoje só o `opencode` as implementa; driver sem elas segue cobrando como antes.

`core.StreamWriter` recebe eventos abstratos (`Start`, `ReasoningDelta`, `TextDelta`, `ToolCallStart`, `ToolArgsDelta`, `ToolCallEnd`, `Usage`, `Finish`, `Error`, `Ping`) e cada driver traduz para seu formato: o driver OpenAI emite `chat.completion.chunk`; o Anthropic emite `content_block_start` e companhia; o OpenRouter emite o chunk OpenAI com `provider` e `usage.cost`; o OpenCode emite o dialeto que a seção 16.3 descrever; o Gemini emite um `GenerateContentResponse` completo por evento (`data:` com `?alt=sse`, elementos de array JSON sem ele) com partes `thought: true` para o raciocínio; o Azure emite o chunk OpenAI precedido do chunk de `prompt_filter_results` e com `content_filter_results` por choice.

**Registro.** `driver.Register("openai", openai.New)` em `init()`; a configuração ativa drivers por nome e prefixo de rota:

```yaml
drivers:
  - name: openai
    prefix: /openai          # vazio = raiz; permite vários drivers na mesma porta
  - name: anthropic
    prefix: /anthropic
  - name: openrouter
    prefix: /openrouter
  - name: opencode
    prefix: /opencode
  - name: gemini
    prefix: /gemini          # SDK: http_options.base_url = http://host:8000/gemini
  - name: azure
    prefix: /azure           # SDK: AzureOpenAI(azure_endpoint="http://host:8000/azure")
```

Sem `prefix`, cada driver atende seus caminhos canônicos na raiz; colisões (`/v1/models` existe em vários) são resolvidas pela credencial: `x-api-key` sem `Authorization` é Anthropic, `Authorization: Bearer sk-or-` é OpenRouter, `x-goog-api-key` ou `?key=` é Gemini, `api-key` é Azure, e assim por diante, com `driver_hint` por chave no YAML como desempate. Caminhos exclusivos não precisam de credencial para escolher o driver: `/v1beta/...` e `/v1/models/{m}:<método>` são Gemini; `/openai/deployments/...` e `/openai/v1/...` são Azure (com `api-key` ou Bearer). O SDK `AzureOpenAI` com `azure_endpoint=<base>` chama `<base>/openai/deployments/{d}/...` e o `google-genai` com `base_url=<base>` chama `<base>/v1beta/...`, então o mesmo `prefix` serve aos dois modos.

**Origem das URLs do próprio wwz** ([ADR 0073](decisions/0073-origem-das-urls-absolutas-do-wwz.md)). O servidor calcula, por requisição e depois do roteamento, a origem do wwz (esquema, host, prefixo externo e o `prefix` do driver; regra na spec 004, "URLs do próprio wwz") e a entrega ao driver num header interno que o cliente não forja (prefixo `\x00`, como `driver.QueryHeader`). O driver lê a origem com `driver.Origin(req)` e nunca lê `Host` nem `X-Forwarded-*` para montar URLs. Sem o header (driver chamado fora do servidor), a origem é vazia e a URL sai relativa.

**Deployments do Azure** ([ADR 0046](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)): o segmento `{deployment}` (ou o `model` da API v1) é o `id` ou um alias de `models[]`; a resposta traz em `model` o `id` do modelo (como o real, que devolve o nome do modelo e não o do deployment). Deployment inexistente → 404 `DeploymentNotFound` (spec 006, 16.6).

**Modelos do Gemini:** o caminho usa `models/<id>` (ou alias); `name` da listagem é `models/<id>` e `modelVersion` da resposta é o `id`.

**Criar um driver novo.** Copiar `internal/driver/openai`, implementar os oito métodos, registrar, adicionar o dialeto de erro em `errors.go` e um teste de contrato (`drivertest.Run(t, New())`) que verifica streaming, tool calls, raciocínio, cada código de erro e os headers de limite. O teste de contrato é o mesmo para todos os drivers, então um driver novo nasce com a cobertura dos existentes.

**Drivers previstos depois do MVP**: `bedrock` (Converse API), `vertex` (Gemini no Vertex AI), `ollama` (`/api/chat`), `litellm` (dialeto OpenAI com erros próprios).

**Entidades e unidades por driver (`LimitPolicy`)**

| Driver | Entidades | Unidades |
| --- | --- | --- |
| `openai` | conta (org), projeto | RPM, TPM (reserva `max_tokens`), RPD, TPD |
| `anthropic` | conta (org), workspace | RPM, ITPM (sem cache read), OTPM |
| `opencode` | workspace, membro | US$ em janelas de 5 h, semana e mês |
| `openrouter` | conta | crédito por chave, orçamento em voo, RPM/RPD dos `:free` |
| `gemini` | projeto (a chave resolve para o projeto, nunca limita sozinha) | RPM, TPM de entrada, RPD (reset à meia-noite do Pacífico) por modelo e tier |
| `azure` | deployment (quota TPM por subscrição, região, modelo e tipo de deployment) | TPM do deployment e RPM derivado dele, avaliados em janelas de 1 s ou 10 s |

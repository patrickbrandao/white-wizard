# Claude Code com o wwz

O driver `anthropic` atende o Claude Code em três frentes ([ADR 0074](../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md)):

| Frente | O que é | Liga com | Referência |
| --- | --- | --- | --- |
| Assinatura OAuth | Tokens `sk-ant-oat01-…` das contas Pro, Max, Team e Enterprise, rotas OAuth e de conta, cota de 5 h e da semana nos headers `anthropic-ratelimit-unified-*` | `drivers[].options.oauth_key` e `oauth_plan` | [spec 006](../specs/006-fidelidade-provedores.md), 16.2.1 |
| Modo estrito do cliente | O mount só aceita requisições byte a byte iguais às do Claude Code 2.1.308; o resto recebe 403 `forged_request_error` | `drivers[].options.client_verification: claude-code` | [spec 020](../specs/020-cliente-claude-code.md) |
| Cliente `wwz-load claude-code` | Cliente de testes que manda os mesmos bytes do Claude Code e faz o login OAuth contra o wwz | — | [spec 015](../specs/015-wwz-load.md), [wwz-load.md](wwz-load.md#claude-code-cliente-que-se-comporta-como-o-claude-code) |

O [`examples/claude-code-assinatura`](../examples/claude-code-assinatura/README.md) monta as três com um smoke. Quando este guia e as specs divergirem, valem as specs.

## Configuração

```yaml
keys:
  # O valor é o segredo dos tokens OAuth; a chave dá a conta, os limites e a cota.
  - { key: wwz-segredo-da-assinatura, label: assinante, account: assinatura }

accounts:
  - id: assinatura
    limits:
      quota_usd: { monthly_usd: 60, rolling_5h_pct: 20, weekly_pct: 50 } # cota de 5 h e da semana

drivers:
  - name: anthropic
    prefix: /anthropic                      # a API real: qualquer cliente
    options: { oauth_key: assinante, oauth_plan: max_20x }
  - name: anthropic
    prefix: /claude-code                    # só o Claude Code
    options: { oauth_key: assinante, oauth_plan: max_20x, client_verification: claude-code }

models:                                     # os modelos padrão do Claude Code 2.1.308
  - { id: claude-sonnet-5-5 }               # com OAuth
  - { id: claude-opus-5-5 }                 # com API key
  - { id: claude-haiku-5-5 }                # consultas laterais
```

As três opções são só do `anthropic` (em outro driver são opção desconhecida) e são validadas no boot, na recarga e em `wwz validate` ([configuracao.md](configuracao.md#drivers)). Sem elas o driver se comporta como antes.

## Assinatura OAuth

Com `oauth_key`, o driver emite e aceita tokens de assinatura e serve as rotas OAuth e de conta no próprio mount. Sem a opção, nenhum token é do wwz: todo Bearer `sk-ant-oat…` recebe o 401 `OAuth access token is invalid.` (conferido no real) e as rotas OAuth respondem como rota inexistente.

**Tokens sem estado.** Access token (`sk-ant-oat01-…`), refresh token (`sk-ant-ort01-…`) e authorization code são assinados com HMAC-SHA256, tendo como segredo o valor da chave `oauth_key`, e levam escopos, emissão e expiração. O token autentica como essa chave: conta, projeto, tier, limites, cota, bloqueio e banimento. Nada vai ao store, então nada é revogado e o code não é de uso único. Tempos do wwz (não conferidos): code 600 s, access 8 h (ou o `expires_in` pedido; o `claude setup-token` pede um ano), refresh 30 dias.

**Inferência com token** (`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`): `Authorization: Bearer sk-ant-oat01-…` com o beta `oauth-2025-04-20` no `anthropic-beta`.

| Situação | Resposta |
| --- | --- |
| Token que não é do wwz, adulterado, de outro segredo, ou um refresh token | 401 `authentication_error` `OAuth access token is invalid.` (conferido) |
| Token vencido | 401 `OAuth token has expired. Please obtain a new token or refresh your existing token.` |
| Sem o beta `oauth-2025-04-20` | 401 `OAuth authentication is currently not supported.` |
| Sem o escopo `user:inference` | 403 `permission_error` `OAuth token does not meet scope requirement user:inference` |
| Token em `x-api-key` | o 401 de chave inválida da API |
| Válido | a resposta normal, com os headers de assinatura abaixo |

Uma entrada de `keys[]` cujo valor começa com `sk-ant-oat` é uma chave como outra (as fixtures do Claude Code usam assim): autentica pelo valor e recebe os headers de assinatura quando vem em `Authorization: Bearer`. Com `server.auth: none`, qualquer Bearer passa.

**Rotas OAuth e de conta**, no mount do driver: `/oauth/authorize` e `/cai/oauth/authorize` (consentimento automático com PKCE S256; `302` para o callback local, ou `code#state` em texto no redirect manual), `/v1/oauth/token` (`authorization_code` e `refresh_token`, erros `invalid_grant`, `invalid_scope` e `invalid_client`), `/v1/oauth/token/revoke`, `/api/oauth/profile`, `/api/claude_cli_profile`, `/api/oauth/validate`, `/api/oauth/claude_cli/roles`, `/api/oauth/claude_cli/create_api_key`, `/api/claude_cli/bootstrap`, `/api/oauth/usage` e `/api/organization/claude_code_first_token_date`. `/api/hello` e `/v1/oauth/hello` respondem com ou sem `oauth_key`, iguais ao real. Formas completas na spec 006, 16.2.1.

**Plano** (`oauth_plan`, padrão `max_5x`). Só muda o que o perfil e o bootstrap dizem; os limites são os da conta.

| `oauth_plan` | `organization_type` | `rate_limit_tier` |
| --- | --- | --- |
| `pro` | `claude_pro` | `default_claude_ai` (não conferido) |
| `max_5x` | `claude_max` | `default_claude_max_5x` |
| `max_20x` | `claude_max` | `default_claude_max_20x` |
| `team` | `claude_team` | `default_claude_ai` (não conferido) |
| `enterprise` | `claude_enterprise` | `default_claude_ai` (não conferido) |

### Cota e headers `anthropic-ratelimit-unified-*`

A cota da assinatura é a `quota_usd` da conta da chave (`monthly_usd`, `rolling_5h_pct`, `weekly_pct`; o mesmo mecanismo do [`cota-5h-semana`](../examples/cota-5h-semana/README.md)). Com token OAuth, a Messages (corpo e stream) troca os `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-*` por:

| Header | Valor |
| --- | --- |
| `anthropic-ratelimit-unified-status` | `allowed`; `allowed_warning` com a janela de 5 h acima de 0,9 ou a semanal acima de 0,75; `rejected` quando a cota recusou |
| `anthropic-ratelimit-unified-5h-utilization`, `-7d-utilization` | gasto da janela ÷ cota da janela, até duas casas (`0.25`, `0.1`); 0 sem `quota_usd` |
| `anthropic-ratelimit-unified-5h-reset`, `-7d-reset` | segundos Unix (5 h depois do débito mais antigo da janela; a semana vira na segunda 00:00 UTC) |
| `anthropic-ratelimit-unified-representative-claim`, `-reset` | `five_hour` ou `seven_day`, a janela de maior utilização, e o reset dela |
| `anthropic-ratelimit-unified-overage-status`, `-overage-disabled-reason` | `rejected` e `org_level_disabled` (sem Extra Usage no wwz) |

Cota estourada: 429 `rate_limit_error` `This request would exceed your account's rate limit. Please try again later.`, com `retry-after` até o reset da janela, `status: rejected`, `representative-claim` da janela que recusou (`five_hour`; `seven_day` também para a mensal) e `x-should-retry: true`. Com um dos headers de assinatura no 429, o Claude Code assinante (plano que não é `enterprise`) não retenta, mesmo com `x-should-retry: true`, e mostra o limite da sessão; o `wwz-load claude-code` faz o mesmo. `count_tokens` e `/v1/models` não mandam headers de limite. Nomes vêm do binário do Claude Code; valores, ordem e presença são "não conferidos".

## Claude Code real contra o wwz

O `/login` do Claude Code **nunca chega ao wwz**: `CLAUDE_CODE_CUSTOM_OAUTH_URL` só aceita uma allowlist fixa e as rotas de conta e OAuth usam os hosts fixos `api.anthropic.com` e `platform.claude.com`. Só `/v1/messages`, `/v1/messages/count_tokens` e a sonda `HEAD /api/hello` seguem `ANTHROPIC_BASE_URL`. Por isso o caminho é emitir um token no wwz e entregá-lo ao Claude Code em `CLAUDE_CODE_OAUTH_TOKEN`:

```sh
B=http://127.0.0.1:18117/claude-code        # mount com oauth_key (com ou sem modo estrito)

# Token de um ano só com user:inference, como o `claude setup-token`, emitido pelo wwz.
go run ./cmd/wwz-load claude-code login --setup-token --target "$B" --state /tmp/cc.json
T=$(jq -r .access_token /tmp/cc.json)       # sk-ant-oat01-…, vale só neste wwz

# Claude Code com ambiente limpo: sem a sua conta, sem o keychain, sem ~/.claude.
mkdir -p /tmp/cc-home /tmp/cc-config
env -i HOME=/tmp/cc-home PATH="$(dirname "$(command -v claude)"):/usr/bin:/bin" \
  CLAUDE_CONFIG_DIR=/tmp/cc-config CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  ANTHROPIC_BASE_URL="$B" CLAUDE_CODE_OAUTH_TOKEN="$T" \
  claude -p "say ok"
```

- `env -i` e um `CLAUDE_CONFIG_DIR` vazio garantem que o Claude Code não use a sua credencial real; o token do wwz não vale na Anthropic. As rotas de conta ainda vão a `api.anthropic.com` com esse token (o real o recusa com 401, aceito na ADR 0074); `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` desliga telemetria e atualização.
- Com API key: `ANTHROPIC_API_KEY=<valor de keys[]>` no lugar de `CLAUDE_CODE_OAUTH_TOKEN`; o modelo padrão passa a ser `claude-opus-5-5` e não há headers de assinatura.
- Os modelos que o Claude Code pede têm de existir em `models[]`. Para o Claude Code não receber tool calls que ninguém pediu, use uma geração com `tool_calls: {probability: 0}`; para exercitar uma tool, um cenário casado por uma palavra do prompt, como em `tests/claudecode/default-mode.sh`.

Dois roteiros manuais (fora do `verify`; pulam sem `claude` no PATH) conferem o Claude Code instalado contra um wwz local, com token emitido pelo próprio wwz e `env -i`:

- `tests/claudecode/real-claude.sh`: os mounts `/strict` e `/off`, nos modos mínimo (a forma das fixtures), padrão (prompt e tools padrão) e num diretório com `CLAUDE.md` ([tests/claudecode/README.md](../tests/claudecode/README.md)).
- `tests/claudecode/default-mode.sh`: 15 modos do Claude Code 2.1.308 em uso normal contra um wwz estrito: `-p` com OAuth e com API key, `--resume`, tool `Read` de um arquivo grande (com `count_tokens`), subagente, `CLAUDE.md`, interativo com OAuth e com API key, interativo com tool, com o classificador do modo auto, com `/context` e com `!comando` antes do prompt. Falha em qualquer 403 `forged_request_error`; o resultado de 2026-11-03 foi nenhuma recusa, e o que o roteiro não alcança está na [spec 006, "Não conferidos no real"](../specs/006-fidelidade-provedores.md#não-conferidos-no-real).

## Modo estrito (`client_verification: claude-code`)

Pedido pelo humano e opt-in por mount, é a única opção de driver que diverge do real (exceção explícita à [ADR 0042](../specs/decisions/0042-fidelidade-total-ao-provedor-real.md)): o mount recusa todo cliente que não se comporta exatamente como o Claude Code 2.1.308. Com `off` (padrão) o driver é o real e a verificação não custa nada no caminho quente.

**Onde vale.** Só em `POST /v1/messages` e `POST /v1/messages/count_tokens` do mount com a opção, como **primeiro passo**: antes da credencial, do corpo e dos limites. `/v1/models`, `/api/hello` e as rotas OAuth e de conta não são verificadas. A recusa não debita cota nem crédito, não conta em limite, concorrência nem abuso e sai sem latência.

**O que confere** (o perfil da spec 020, sobre a cabeça crua, na ordem e grafia em que chegou): a linha `POST <prefixo>/v1/messages?beta=true HTTP/1.1`; os headers do Claude Code, na ordem e com a caixa exatas, sem nenhum a mais; `User-Agent: claude-cli/2.1.308 (external, sdk-cli|cli)`; os valores fixos (`X-Stainless-*`, `anthropic-version`, `x-app`, `Accept-Encoding`); a lista de betas do tipo de requisição, na ordem canônica e coerente com a credencial; o corpo como o `JSON.stringify` o serializa (chaves na ordem de inserção, sem espaços, `<>&` crus); as chaves de topo de um tipo do perfil; `metadata.user_id` e a sessão igual a `X-Claude-Code-Session-Id`; o billing header em `system[0]` com a versão, o entrypoint e o fingerprint da primeira mensagem do usuário; a identidade em `system[1]`. O conteúdo das mensagens, o modelo e os valores de `thinking`/`output_config` não são conferidos além da forma: a validação real do driver vem depois.

**Recusa.** HTTP 403, `content-type: application/json`, `request-id`, `x-should-retry: false` e `x-wwz-client-verification: failed; reason=<reason>`:

```json
{"type":"error","error":{"type":"forged_request_error","message":"Client verification failed: this request does not match Claude Code 2.1.308; the client is forging Claude Code requests (query: expected \"POST <path>?beta=true HTTP/1.1\", got \"POST /claude-code/v1/messages HTTP/1.1\")."},"request_id":"req_…"}
```

O tipo `forged_request_error` não existe no real (lá um 403 é `permission_error`): é do wwz, para o roteador sob teste distinguir a recusa. Conta em `wwz_requests_total{status="403"}` e em `wwz_client_verification_failures_total{driver,reason}`, vai ao log (`error=forged_request`, `client_verification_reason`, `client_verification_detail`) e ao `ev:stream` como `error`.

**Códigos `reason`** (lista fechada, na ordem das checagens; vale a primeira que falha):

| `reason` | Recusa quando |
| --- | --- |
| `query` | a linha não é `POST <caminho>?beta=true HTTP/1.1` |
| `transfer_encoding` | há `Transfer-Encoding` (corpo chunked) |
| `header_missing` | falta `Authorization`/`x-api-key` ou algum header do perfil |
| `header_order` | header a mais (inclusive de proxy), fora de ordem, repetido ou com outra caixa |
| `user_agent` | `User-Agent` fora de `claude-cli/<VER> (external, cli\|sdk-cli)` |
| `unknown_version` | versão sem perfil (só `2.1.308`) |
| `header_value` | valor fora do perfil (`X-Stainless-*`, `Content-Length`, sessão que não é UUID v4 etc.) |
| `beta` | beta desconhecido, fora da ordem canônica, lista que não é a do tipo, ou `oauth-2025-04-20` sem token OAuth (ou o contrário) |
| `body_encoding` | corpo que não é JSON UTF-8 válido ou difere da re-serialização `JSON.stringify` dele |
| `body_key_order` | chaves de topo que não são as de um tipo do perfil, na ordem |
| `metadata` | `metadata.user_id` ausente ou fora da forma |
| `session_mismatch` | `session_id` do `metadata` diferente de `X-Claude-Code-Session-Id` |
| `billing_header` | `system[0]` ausente ou fora da forma, versão ou entrypoint diferentes, `cch=` ou campo de first-party |
| `fingerprint` | o fingerprint do billing header não é o de nenhum candidato da primeira mensagem do usuário |
| `identity` | `system[1]` não é a identidade do entrypoint com o `cache_control` esperado |

**Uso normal do Claude Code é aceito** (task 457, conferido com o Claude Code 2.1.308 real nos 15 modos do `default-mode.sh`): o fingerprint vale para qualquer bloco `text` da primeira mensagem `user` (os lembretes, o `CLAUDE.md` e a saída de `/context` vêm antes do prompt, nem sempre com tag); `inline-tools` e `advisor-tool` podem vir ou não (flags do Claude Code); `extended-cache-ttl` pode faltar com OAuth quando o `cache_control` não tem `ttl` (subagente); o subagente (`x-claude-code-agent-id`/`x-claude-code-parent-agent-id` depois de `x-app` e `cc_is_subagent=true;` no billing header, sempre juntos); o turno depois de um resultado de tool com os betas de `turn_safeguards`; o classificador do modo auto (tipo `classifier`, não-stream, `X-Stainless-Timeout: 60`). Seguem recusados os betas atrás de outras flags ou configurações (`context-1m` num turno, `fast-mode`, `prompt-caching-scope`, `extended-cache-ttl` com API key), sem conferência no real.

**SDKs oficiais e outros clientes.** Os SDKs `anthropic` (Python e Node) saem com `reason=query`: nem `messages.create` nem `beta.messages.create` acrescentam `?beta=true`. Com `?beta=true` forçado falta `X-Claude-Code-Session-Id` (`header_missing`); com os headers acrescentados, a ordem e a caixa não batem (`header_order`). Só o Claude Code 2.1.308 e o `wwz-load claude-code` passam.

**Atrás de proxy não funciona.** A verificação precisa dos headers como o cliente os mandou. Um proxy que canoniza nomes ou acrescenta headers (Traefik, Go `net/http`, `X-Forwarded-For`) faz toda requisição sair com `header_order`. Publique a porta do wwz direto (o compose do exemplo faz isso). Em produção o modo estrito fica na porta 8008 (`http://<IP público>:8008/claude-code`, repassada em TCP puro). A 8004 é a mesma instância, mas no servidor do autor o haproxy a atende em `mode http` (outro serviço do host divide a porta pelo Host), que tira `Connection` e reescreve a cabeça: lá o `/claude-code` recusa até o Claude Code real. A raiz da 8004 (assinatura sem modo estrito) segue valendo, e o `/anthropic` do HTTPS, atrás do Traefik, tem só a assinatura ([deploy/prod/README.md](../deploy/prod/README.md#claude-code-adr-0074-specs-006-1621-e-020)).

**O que não foi conferido.** Algumas listas de betas não têm captura e foram inferidas da regra das capturas: as do entrypoint `cli` (interativo) no tipo `turn`, as de API key fora do `-p` (`turn_safeguards`, `quota`, `title`, `classifier`) e o `count_tokens` do `cli`. Existem para o `wwz-load claude-code` dar 200 em todos os modos e ficam "não conferido" na [spec 006, "Não conferidos no real"](../specs/006-fidelidade-provedores.md#não-conferidos-no-real) (spec 020, revisão da task 452). Também sem captura: o fallback não-stream do turno e o `thinking` no fim do `count_tokens`.

**Versões.** Só a 2.1.308 tem perfil; outra versão do Claude Code sai com `unknown_version`. Uma versão nova entra por tarefa, com captura nova (`testdata/real/claude-code/<versão>/`, [verificacao.md](verificacao.md)).

## `wwz-load claude-code`

Cliente de testes que faz o papel do Claude Code contra o wwz: as requisições de inferência saem byte a byte iguais às do Claude Code 2.1.308, e o login segue o fluxo OAuth do Claude Code contra as rotas do driver. Ações:

| Ação | O que faz |
| --- | --- |
| `login [--manual] [--setup-token]` | PKCE, `/oauth/authorize`, callback local (ou redirect manual), `/v1/oauth/token`, perfil e papéis; grava o estado (`--state`) |
| `refresh` | Renova o access token do estado |
| `ask <texto> [--resume]` | Um turno; imprime a resposta |
| `replay <captura.raw> [--send]` | Reconstrói uma fixture e compara byte a byte; com `--send`, também a envia |
| `run` | Carga: uma sessão por unidade, relatório do `run` com `config.protocol: "claude-code"` |

```sh
B=http://127.0.0.1:18117/claude-code
wwz-load claude-code login --target "$B" --state /tmp/cc.json
wwz-load claude-code ask "say ok" --target "$B" --state /tmp/cc.json
wwz-load claude-code ask "say ok" --target "$B" --state /tmp/cc.json --auth api-key --key wwz-segredo-da-assinatura
wwz-load claude-code replay testdata/real/claude-code/2.1.308/002-sdk-cli-oauth-turn.raw
# identical sha256=5644b70ff35f1ac004b1a44597fef60c5a4fae949c14026b92ab3b749a42014d
wwz-load claude-code run --target "$B" --state /tmp/cc.json --concurrency 4 --duration 10s --output json
```

Retenta como o `shouldRetry` do Claude Code: em particular, o assinante **não retenta o 429 de assinatura** (com `representative-claim`, `overage-status` ou `overage-disabled-reason`), mesmo com `x-should-retry: true`; um login `enterprise` e a API key retentam como num 429 comum. Flags, estado e códigos de saída em [wwz-load.md](wwz-load.md#claude-code-cliente-que-se-comporta-como-o-claude-code).

### Nunca contra hosts reais

`wwz-load claude-code` é um cliente do simulador, não um meio de usar credencial de assinatura fora do Claude Code. `--target` em `anthropic.com`, `claude.ai`, `claude.com` ou subdomínio deles sai com código 2 sem abrir conexão (`refusing real Anthropic host <host>: this client only talks to the simulator`); redirect para outro host nunca é seguido; as URLs de authorize e token saem do `--target`; o cliente não lê `~/.claude`, o keychain nem `CLAUDE_CODE_OAUTH_TOKEN`. Não aponte o cliente nem o wwz para um token de assinatura real: o do humano não é do projeto e não é usado em nenhum teste.

## Não conferido

Vem do binário do Claude Code, de capturas locais e de sondas do real sem credencial; nenhum token de assinatura real foi usado. Ficam "não conferido" (lista completa na [spec 006, "Não conferidos no real"](../specs/006-fidelidade-provedores.md#não-conferidos-no-real)): os tempos dos tokens, os textos de token vencido, sem beta e sem escopo, o formato exato e a presença dos `anthropic-ratelimit-unified-*` e do 429 de cota (o wwz manda `x-should-retry: true`), os `rate_limit_tier` de `pro`, `team` e `enterprise`, as escolhas do wwz nas rotas OAuth (textos de erro, `unsupported_grant_type`) e a recusa real "This credential is only authorized for use with Claude Code…", que o wwz não reproduz (a regra que a dispara é desconhecida; para recusar quem não é o Claude Code existe o modo estrito).

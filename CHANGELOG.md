# Changelog

Formato [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/); versões [SemVer](https://semver.org/lang/pt-BR/). Detalhe das decisões em `specs/decisions/` (índice em `HISTORY.md`); o detalhe de cada entrega está no histórico do Git.

## [Não lançado]

Recursos do opencode.ai no driver `opencode` (ADRs 0072 e 0073, tasks 415–438). Guia em `docs/drivers.md` (seção OpenCode), campos em `docs/configuracao.md` e uso do cliente em `docs/uso.md`.

### Adicionado
- **Porta 8008 do modo estrito do Claude Code** (task 468, ADRs 0017 e 0074 item 19, specs 019 e 020). Uma porta que o haproxy atende em `mode http` (tira `Connection`, acrescenta `X-Forwarded-For`), como a 8004 quando divide a porta com outro serviço do host, não serve ao `client_verification: claude-code`; a 8008 do IP público, em TCP puro para a instância `anthropic`, é o caminho do modo estrito em produção. Portas diretas 8000–8008; smoke externo cobre a 8008; spec 020 exige caminho L4 ou direto para o modo estrito.
- **Docs e exemplo do Claude Code** (task 448, ADRs 0016 e 0074, specs 015, 019 e 020). `docs/claude-code.md`: o Claude Code real com o wwz (`ANTHROPIC_BASE_URL` + `CLAUDE_CODE_OAUTH_TOKEN` emitido por `wwz-load claude-code login --setup-token`, com `env -i`; o `/login` do Claude Code nunca chega ao wwz), `oauth_key`/`oauth_plan`, a cota 5 h/semana nos `anthropic-ratelimit-unified-*`, o modo estrito `client_verification: claude-code` (os 15 `reason`, o uso normal aceito, SDKs oficiais com `reason=query`, sem proxy que reescreva headers, listas de betas inferidas), `wwz-load claude-code` e a recusa de hosts reais; opções em `docs/configuracao.md`, seção Anthropic em `docs/drivers.md`, subcomando em `docs/wwz-load.md`, README. Exemplo `examples/claude-code-assinatura` (índice 17, porta 18117): dois mounts com a mesma assinatura (`max_20x`, `quota_usd` na conta), um estrito; o smoke faz `login`, `ask` e `replay --send` da fixture 002 (200), `curl` comum no estrito (403 `forged_request_error`, `reason=query`/`header_missing`), confere `anthropic-ratelimit-unified-status` e o 429 de cota `rejected` que o `ask` não retenta. `scripts/verify.sh`: `EXAMPLES_ADR0016` com 18 nomes e o selftest de contagem.
- **Produção com o Claude Code** (task 465, ADRs 0017 e 0074, specs 019 e 020). `deploy/prod/config/anthropic.yaml` (8004): `anthropic` na raiz com `oauth_key: prod-3` e `oauth_plan: max_20x` e em `/claude-code` também com `client_verification: claude-code`; modelos `claude-sonnet-5-5`, `claude-opus-5-5` e `claude-haiku-5-5` (texto sem tool calls); conta `acct-claude-max` com `quota_usd` (US$ 50 em 5 h, US$ 250 na semana) para os `anthropic-ratelimit-unified-*`. `config/todos.yaml`: os mesmos modelos e a assinatura no `/anthropic`, sem modo estrito (atrás do Traefik). `scripts/prod-smoke.sh`: login `--setup-token` com o `wwz-load claude-code`, `ask` 200 na raiz e em `/claude-code` (`sdk-cli` e `cli`), `/api/hello`, o `unified-status` do assinante (8004 e HTTPS) e o 403 `x-wwz-client-verification: failed` de um curl comum; `--only-claude-code` e `SMOKE_CC_URL`. `deploy/prod/test.sh` confere os mounts e roda esse bloco do smoke contra um wwz local. README com a seção "Claude Code".
- **Alocações com `client_verification: off`** (task 460, spec 020 RNF-01, ADR 0074). `internal/server/client_verification_alloc_test.go`: o mesmo `POST /v1/messages` por `Server.Handle` (sem e com stream, o corpo drenado) aloca o mesmo por requisição no mount `anthropic` sem opções, com `client_verification: off` explícito e com `oauth_key` usado com chave de API (167 e 276 allocs/op; média arredondada, sem `-race`).
- **Aceite ponta a ponta do Claude Code** (task 447, ADR 0074, specs 015 e 020). `cmd/wwz-load/claudecode_e2e_test.go`: `wwz-load claude-code` contra um wwz em processo no modo estrito (`login` com callback e `--manual`, `refresh`, `--setup-token`, `ask` em `sdk-cli`/`cli` com OAuth e chave, com e sem stream e `--resume`, `run --requests 50` sem erro, `replay --send` de cada fixture com 200). `internal/server/claude_code_oauth_e2e_test.go`: cada mutação da spec 020 com token da assinatura dá o `reason` certo sem mexer na conta nem na chave (`/admin/entities`), conta em `wwz_client_verification_failures_total`; a captura sai com `anthropic-ratelimit-unified-*` e a cota esgotada dá 429 `status: rejected`, com a recusa estrita antes dela. SDKs Python e Node no mount novo `/anthropic-cc` de `tests/sdk/wwz.yaml` (403 `forged_request_error`, `reason=query`). `tests/claudecode/real-claude.sh` roda o Claude Code instalado com token do wwz (resultado de 2026-11-03: mínimo 200 nos dois mounts; prompt padrão e `CLAUDE.md` recusados no estrito por `fingerprint`, task 457).
- **Verificação estrita do cliente no driver `anthropic`** (task 445, ADR 0074, spec 020). `drivers[].options.client_verification` (`off` padrão, `claude-code`; outro valor ou outro driver tornam a configuração inválida). Com `claude-code`, `POST /v1/messages` e `/v1/messages/count_tokens` passam por `claudecode.Verify` sobre a cabeça crua antes da credencial, do corpo e dos limites: quem não é o Claude Code 2.1.308 recebe 403 `forged_request_error` com `x-wwz-client-verification: failed; reason=<reason>`, sem latência e sem tocar o store (nenhum débito, contagem, concorrência ou abuso), contado em `wwz_client_verification_failures_total{driver,reason}` e `wwz_requests_total{status="403"}`, no log (`client_verification_reason`/`_detail`) e no `ev:stream` (`error: forged_request`, `fields.reason`). Interface opcional `driver.ClientVerifier`, `core.ErrForgedRequest` e `limits.FinishRequest.Reason`. Com `off` nada muda (mesmas alocações no caminho quente).
- **Assinatura OAuth do Claude Code no driver `anthropic`** (task 443, ADR 0074, spec 006 16.2.1; formas do cliente, não conferidas salvo o 401 de token inválido). Opções `drivers[].options.oauth_key` (label de `keys[]`: a conta da assinatura e o segredo dos tokens) e `oauth_plan` (`max_5x` padrão, `max_20x`, `pro`, `team`, `enterprise`). Tokens sem estado assinados com HMAC-SHA256 (access `sk-ant-oat01-`, 8 h ou o `expires_in` pedido; refresh `sk-ant-ort01-`, 30 dias; code de 600 s ligado ao PKCE S256), válidos em toda instância; o access token vale como a chave `oauth_key`. Rotas no mount do driver: `/oauth/authorize` e `/cai/oauth/authorize` (consentimento automático: 302 ao callback local ou `code#state` no redirect manual), `/v1/oauth/token` (`authorization_code`, `refresh_token` e os `invalid_*`), `/v1/oauth/token/revoke`, `/api/oauth/profile` (com `request-id` que o cliente aceita), `/api/claude_cli_profile`, `/api/oauth/validate`, `/api/oauth/claude_cli/roles`, `/api/oauth/claude_cli/create_api_key`, `/api/claude_cli/bootstrap` e `/api/oauth/usage`; `/v1/oauth/hello` e `/api/organization/claude_code_first_token_date` respondem sempre. Bearer `sk-ant-oat…` fora de `keys[]` que não verifica → 401 `OAuth access token is invalid.` (com ou sem `oauth_key`); vencido, sem o beta `oauth-2025-04-20` e sem `user:inference` com os 401/403 da spec. As respostas da Messages a um Bearer `sk-ant-oat…` levam `anthropic-ratelimit-unified-*` da `quota_usd` da conta (5 h e semana) no lugar dos `anthropic-ratelimit-*`, e a cota estourada é 429 `rate_limit_error` com `retry-after` e `status: rejected`. Interfaces opcionais `driver.TokenChecker` e `driver.SubscriptionReporter` (`driver.SubscriptionView`, `NewSubscriptionView`, `NextWeek`); o engine lê a visão do store só para essas requisições. Sem as opções, o `anthropic` se comporta como antes, fora o texto do 401 dos tokens OAuth. Fixtures sem custo `auth-order/badkey_oauth_messages`, `routes/oauth_profile_badkey` e `routes/oauth_hello_{get,head}`.
- **Forma do Claude Code 2.1.308 no driver `anthropic`** (task 441, ADR 0074). O wwz aceita e recusa como o real (76 fixtures `validation/cc_*.json`, conferidas em 2026-11-03): `role: "system"` em `messages` (na conversa como mensagem de sistema, com as recusas de posição, conteúdo vazio e blocos), `output_config` por mensagem com o beta `per-turn-control-2026-07-01`, `thinking` `adaptive` com `display` (`updates` só com `thinking-display-updates-2026-08-18`), `safeguards` com `dangerous-tool-use-2026-09-03`, `tools[].defer_loading` (todas deferidas é 400) e `?beta=true`. `adaptive` ativa o raciocínio no tamanho de `output_config.effort`; `display: omitted`/`updates` tiram o texto do bloco `thinking` e `context_management` vai na resposta como o real (fixtures `cc-shape/`). `GET`, `HEAD` e os demais métodos de `/api/hello` respondem `{"message": "hello"}` sem credencial. As capturas do Claude Code 2.1.308 recebem 200; `openrouter` e `opencode` não mudam.
- **Modo `public` e gratuitos** (tasks 421, 422). A credencial `public` (bearer, `x-api-key` na Messages, `x-goog-api-key` na Google) é o uso sem conta: nunca é procurada em `keys[]`, vale com `auth: none` e só atende gratuitos (`pricing` declarado com `input_per_m: 0`) nas bases pay-as-you-go. Com modelo pago, ou sem credencial, dá 401 `AuthError` `Missing API key.`; na base Go, 401 `Go requires a credential`; no `/v1/usage`, 401 `Unauthorized`. Fora de `User-Agent: opencode/...`, a cota de gratuitos vale zero: 429 `FreeUsageLimitError` com `retry-after` até 00:00 UTC, também na Messages, nunca 403. Cota diária `drivers[].options.free_requests_per_day` (padrão 200), por IP sem chave ou por conta, na entidade `free_models`; o evento leva `key: "public"`. `x-api-key` vale na rota Messages para qualquer chave.
- **Cobrança por base** (tasks 419, 421, 422, 432). O driver decide a origem por requisição (`driver.BillingDecider`): `free` não debita nada; `balance` debita saldo, `spend_*` e budgets; `go`/`go-plus` debitam só a cota Go, pelo `allowance` (lista × `allowance_factor`, padrão 2 nos `gemini-*` e 0,6 nos demais), com `cost` 0. Sai em `usage.opencode.billing_source` e no evento `request` (`billing_source`, `allowance_usd`; asyncapi 0.5.0). O store debita a cota à parte do custo (`finish` com `quota_micro`, também com `local_first`) e expõe `QuotaWeeklyUSD` e `QuotaMonthlyUSD`.
- **Planos e contas** (tasks 418, 421, 422). `tiers.<n>.plan` (`go`, `go-plus`, `go-teams`, `none`; ausente = `go`), `accounts[].extra_usage` (cota estourada vai para o saldo; padrão ligado no `go-teams`), `keys[].service_account` (fora da cota Go, cobrada no saldo) e o membro do workspace como entidade `project` (a cota Go do Go Teams conta nele). Plano `none` dá 403 `ForbiddenError` nas bases Go e no `/v1/usage`.
- **Cota e budget por modelo** (tasks 418, 419, 421). `quota_usd.models` (entidade `model_quota`) e `spend_monthly_models_usd` (entidade `model_spend`, com `opencode/<id>` e `opencode-go/<id>`), em todo nível de `limits` e em `cfg:policies`. Nos outros drivers, o budget por modelo vale com o `spend_limit` do dialeto.
- **Preço por horário e por contexto** (tasks 417, 420). `pricing.multipliers` e `pricing.overrides` com a gramática `when` do real (`utc_days`, `utc_hours`, `date`, `context`, `any`, `all`). O preço efetivo vale para o custo de todo driver; configurações sem os campos não mudam.
- **Catálogos** (tasks 417, 420, 423, 435, 438). `models[].catalogs.{opencode,opencode-go}` com `pricing`, `allowance_factor`, `context_window`, `max_output`, `capabilities` e `extra` (`null` tira o campo). O modelo efetivo do catálogo vale para listagem, `context_length_exceeded`, teto de saída, recusas por capacidade e preço; fora do catálogo da base, o modelo some da listagem e dá 400 `Model is unavailable.`. A listagem sai como os catálogos públicos do real, byte a byte fora `created`: `cost.multipliers`, `cost.text.*.overrides`, um único protocolo nativo em `api` (ou as chaves de `extra.api`) e `base_url` absoluto na família `/inference`.
- **Console** em `<prefixo>/console` (task 424; formas e tempos não conferidos). Device flow sem estado (`slow_down` antes de 5 s, `authorization_pending` até 10 s, `expired_token` após 900 s), refresh com `invalid_grant`, `api/user`, `api/orgs`, `api/v2/config` e `api/policies/current-budgets`. Tokens HMAC-SHA256 com o valor da chave `drivers[].options.console_key` (access 1 h, refresh 30 dias), válidos em toda instância; o access token vale como essa chave na inferência.
- **Núcleo dos drivers** (tasks 418, 420, 421, 424, 436). Interfaces opcionais `driver.CatalogRouter`, `driver.BillingDecider`, `driver.KeyResolver` e `driver.PublicRouter`; `driver.Origin`; `DriverDecl.Catalogs`, `Plans` e `ReservedKeys`; `DriverOption.Check` (`OptionPositiveInt`, `OptionKeyLabel`). Drivers sem essas interfaces se comportam como antes.
- **Fixtures sem custo** (task 416): `testdata/real/opencode/catalog/` (os catálogos públicos, 79 e 28 modelos) e `free/` (modo `public` com cliente genérico). Gasto: US$ 0.
- **Exemplo `examples/opencode-completo`** (tasks 426, 437; porta 18116): 91 modelos gerados das fixtures, os quatro planos, Extra Usage, membro, service account, budgets e Console. O smoke confere as listagens campo a campo, `public`, cada origem de cobrança, o 403 sem Go, o `BudgetLimitError` e o Console. Entra no `verify-full` (17 exemplos).
- **Testes** (task 425): drivertest com `Profile.Billing` e `Profile.PublicRoutes`, os erros novos com corpo exato, as fixtures `free/` e `catalog/` pelo engine, o contrato do Console e os SDKs `openai` Python e Node com `public`, cota zero, `free_requests_per_day`, base Go e Extra Usage.
- **Cabeça crua da requisição** (task 444, ADR 0074). Interface opcional `driver.RawHeadWanter` (`WantsRawHead() bool`, por montagem): só quando o driver pede, o data plane guarda a linha da requisição e os headers exatamente como chegaram (ordem, grafia, repetições, `Host`, `Content-Length`, `Transfer-Encoding`), lidos com `driver.RawHeadOf(h)`/`RawHeadOfRequest(req)` e `RawHead.Headers()`. Sem o pedido, nada é copiado (mesmas `allocs/op` no `Handle`). Base do modo `client_verification: claude-code` (spec 020).
- **Perfil do Claude Code 2.1.308** (task 442, spec 020, ADR 0074). Pacote `internal/claudecode`: `Profile` por versão (`Lookup`, headers, betas em ordem canônica, listas por tipo × credencial × entrypoint, ordens de chaves), `Fingerprint`, `BillingHeader`, `UserID`, codec compatível com `JSON.stringify` sobre valores ordenados (`Value`, `Parse` que recusa chave repetida, `Marshal`), `Build` (bytes idênticos aos do Claude Code nos tipos `turn`, `turn_safeguards`, `quota`, `title`, `count_tokens` e a sonda `HEAD /api/hello`), `Extract`, `ParseHead` e `Verify` (as 15 checagens e os `reason` da spec 020). Fixtures `testdata/real/claude-code/2.1.308/` (9 requisições cruas normalizadas, com `count_tokens` capturado sem custo, e `MANIFEST.md`); `TestRealFixturesClaudeCode` reconstrói cada uma com o mesmo sha256. Listas de betas sem captura (modo `cli` com API key, entre outras) são inferidas; achados para a spec na task 452.
- **`wwz-load claude-code`** (task 446, spec 015, ADR 0074). Cliente que se comporta como o Claude Code 2.1.308 (`internal/claudecode/client`): HTTP/1.1 escrito à mão sobre `net.Conn`/`crypto/tls` com os bytes de `claudecode.Build` (keep-alive por sessão; resposta com `Content-Length` ou chunked, `gzip`/`deflate`; `br`/`zstd` dão `parse`), retentativas como o SDK (`connect`, `reset`, 408, 409, 429 sem os headers de assinatura, 5xx, `x-should-retry`) com `X-Stainless-Retry-Count`. Ações `login` (PKCE, callback em `127.0.0.1` ou `--manual`, `--setup-token`, profile e roles), `refresh`, `ask <texto>` (`--resume`, `--dump`), `replay <captura>` (`identical sha256=…` ou a primeira divergência com linha da cabeça ou caminho JSON; `--send`) e `run` (`--turns`, `--rps`/`--concurrency`, relatório do `run` com `config.protocol: "claude-code"`). Recusa `anthropic.com`, `claude.ai`, `claude.com` e subdomínios antes de qualquer conexão (código 2), sem opção para desligar; estado (`device_id`, tokens, última conversa) em `$XDG_STATE_HOME/wwz-load/claude-code.json`, modo 0600. `ask` reproduz byte a byte as fixtures 002–012 e `replay` dá `identical` nas 9; o login é testado contra um OAuth falso pela tabela de 16.2.1 (o driver é a task 443).

### Alterado
- **Dados do autor fora do git** (task 473, ADR 0076, specs 001, 002, 006, 018 e 019). Tudo que identifica o servidor de produção do autor (host, IPs, porta SSH, serviços de terceiros do host, o haproxy real) sai do repositório para `autor/` (inteiro no `.gitignore`): `autor/prod.env` e `autor/haproxy.cfg`, com o template `deploy/prod/autor.env.example`. `scripts/prod-deploy.sh`, `prod-smoke.sh` e `heavy.sh` leem `${WWZ_AUTHOR_ENV:-autor/prod.env}` (o ambiente vale mais) e falham sem os valores; o compose de produção não tem host padrão; o `deploy/prod/haproxy/haproxy.cfg` versionado é o genérico (só o wwz, 8000–8008 em `mode tcp`). As notas do loop (`MORNING.md`, `DONE.md`, `PROGRESS.md`, `STATUS.md`, `PROMPT-LOOP.md`) e `ops/loop/` saem do git para `autor/notas/` e `autor/loop/`. O não conferido no real passa a viver na spec 006 ("Não conferidos no real", por provedor, com as divergências conferidas e ainda não reproduzidas), os passos do humano para secrets e a tag `v0.1.0` na spec 018 ("Passos do humano"), e os arquivos do autor na spec 019. Specs, ADRs e docs não citam mais hashes de commit.
- **GitHub e Docker Hub** (task 471, ADR 0075, spec 018). Repositório público `patrickbrandao/white-wizard` (SSH); o `release.yml` publica em `docker.io/tmsoftbrasil/white-wizard`, `-load` e `-panel`, com login pelos secrets `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` da conta `tmsoftbrasil`; as primeiras imagens (`:latest` e um `:sha-<7>`) saíram do Mac do humano. Os binários `wwz` e `wwz-load` commitados por engano não vão ao GitHub.
- **Produção: modo estrito do Claude Code na 8008 e `haproxy.cfg` igual ao do servidor** (task 469, ADRs 0017 e 0074, specs 019 e 020). O `haproxy.cfg` de produção ganha `listen p8008` em `mode tcp` para a instância anthropic: o modo estrito em produção é `http://<IP público>:8008/claude-code`, porque uma porta em `mode http` tira `Connection` e reescreve a cabeça. `prod-deploy.sh sync` não manda mais o `haproxy.cfg` no tar (o inode novo deixava o bind de arquivo do `wwz-ports` com o conteúdo antigo): confere com `haproxy -c` num container avulso, grava no lugar (mesmo inode), guarda o sha256 em `run/haproxy.cfg.sha256`, recusa com `diff` um arquivo editado à mão (salvo `WWZ_PROD_HAPROXY_ACCEPT=<sha256>`) e, quando mudou, reinicia o `wwz-ports` (~1-2 s fora) e confere o sha256 e as portas por dentro do container. `prod-smoke.sh`: as checagens do modo estrito vão para a 8008 (`SMOKE_CC_STRICT_URL`), a 8004 fica com hello, login, `ask` e `unified-status`, e a 8008 entra nas portas. `deploy/prod/test.sh` confere as portas 8000-8008, o `p8008` TCP e o `sync` num diretório temporário.
- **Spec 020 com o uso normal do Claude Code no modo estrito** (task 464, ADR 0074 item 18). A spec descreve o que o `Verify` da task 457 faz: tipo `classifier`, headers opcionais `x-claude-code-agent-id`/`x-claude-code-parent-agent-id`, `X-Stainless-Timeout: 60` só no `classifier`, betas de flag (`inline-tools`, `advisor-tool`, `extended-cache-ttl`), `context-1m` na ordem canônica, ` cc_is_subagent=true;` no billing header, candidatos do fingerprint e `cache_control` de `system[1]` pelo `extended-cache-ttl`; critério de aceite com `tests/claudecode/default-mode.sh` (15 modos sem 403).
- **Spec 015, retentativa do `wwz-load claude-code` igual ao Claude Code 2.1.308** (task 458): para o assinante (OAuth com `user:inference`, plano não `enterprise`), 429 com `representative-claim`, `overage-status` ou `overage-disabled-reason` nunca retenta, mesmo com `x-should-retry: true`, que não força nada para ele; `overage-disabled-reason` de corte (`org_spend_cap_reached`, `org_level_disabled_until`) para qualquer credencial. Spec 006 16.2.1 registra o `x-should-retry: true` do 429 de cota do wwz como não conferido.
- **Spec 006 16.2.1 e spec 005 com o que a task 443 entregou** (task 454): fixture `badkey_oauth_messages`, `/v1/oauth/hello` conferido (19 bytes, `request-id`, 405 de 132 bytes), rotas de conta sem `oauth_key`, `keys[].key` `sk-ant-oat…`, onde e em que formato saem os `anthropic-ratelimit-unified-*`, claim `seven_day` para `quota_monthly`, as escolhas não conferidas das rotas OAuth e os nomes `driver.TokenChecker`/`SubscriptionReporter`.
- Contagem Claude: mensagem `role: "system"` em `messages` soma em `system` (tokenizador e `x-wwz-input-breakdown`) com o cabeçalho do system e sem abrir turno de `user`; uma sequência delas custa +1 no 4.8/5.x e, nos 5.5, +1 antes de um turno `assistant` com cabeçalho; a forma só-diretiva custa 0 (conferido com `count_tokens` em 2026-11-03, task 451).
- **Mudança de comportamento: cobrança por base** (tasks 421, 422). O formato do YAML não muda, mas os números mudam. Na base Go, a cota cai pelo `allowance` (0,6× a lista, 2× nos `gemini-*`), e o saldo e o `spend` deixam de ser debitados. Na base pay-as-you-go, `quota_usd` deixa de valer. Para manter os números antigos da cota, use `allowance_factor: 1`, como o `examples/cota-5h-semana`. Lembre de declarar também `opencode: {}`, porque com `catalogs` o modelo só existe nos catálogos listados:
  ```yaml
  catalogs: {opencode: {}, opencode-go: {allowance_factor: 1}}
  ```
- **Mudança de comportamento: `X-Forwarded-*`** (task 436, ADR 0073). `X-Forwarded-Proto`, `X-Forwarded-Host` e `X-Forwarded-Prefix` só valem com o par TCP em `server.trusted_proxies` (ou `TRUSTED_PROXIES`), como o `X-Forwarded-For`. Vindos de outro cliente, deixam de mudar as URLs que o wwz devolve.
- Config: `keys[].key: public` é recusada com o `opencode` montado, e os campos novos têm as recusas da spec 003 no boot, na recarga e em `wwz validate` (task 418).
- Specs 003, 004, 005, 006, 010, 012 e 019, ADRs 0072 (com as revisões das tasks 427 e 434), 0073 e 0016 (revisão), `cache/valkey/README.md` (`finish` com `quota_micro`, ARGV 14–16, janelas `sday`/`swk`/`free`, `qwk` e `qmo` no `Get`) e asyncapi 0.5.0 (tasks 415, 427, 429, 431, 433, 434, 437).
- Docs: seção OpenCode de `docs/drivers.md`, campos e erros do OpenCode em `docs/configuracao.md` e o cliente OpenCode apontado para o wwz em `docs/uso.md` (task 428).

### Corrigido
- **Comentários de produção com a porta 8008** (task 470). `deploy/prod/config/anthropic.yaml`, `todos.yaml` e `docker-compose.yml` diziam que o modo estrito ficava na 8004 e que as portas eram 8000-8007; passam a citar a 8008 (TCP) e o motivo (a 8004 divide a porta com outro serviço do host em `mode http`). Só comentários.
- **Teste dos exemplos com as opções do `anthropic`** (task 467). `TestNoNewFieldsSameMeaning` carrega todos os `examples/*/config.yaml`, mas só registrava a declaração do `opencode`; com `examples/claude-code-assinatura` (`oauth_key`, `oauth_plan`, `client_verification`) a suíte de `internal/config` falhava na main. O teste passa a registrar também a declaração do `anthropic` do golden `driver_decls.json`.
- **Segredo próprio da assinatura OAuth em produção** (task 466, ADRs 0017 e 0074, spec 006 16.2.1). O `create_api_key` (consentimento automático) devolvia a `WWZ_KEY_3`, válida em todas as instâncias. `scripts/prod-deploy.sh credentials` gera `WWZ_OAUTH_KEY` (idempotente, acrescentada a um `.env` existente sem mexer nos outros valores, sem imprimir) e a lista no `CREDENTIALS.md`; `render` troca `${WWZ_OAUTH_KEY}` e só exige cada placeholder das configs que o usam. `config/anthropic.yaml` e `config/todos.yaml`: entrada `claude-code-oauth` (conta `acct-claude-max` com a `quota_usd`, só os `claude-*`) como `oauth_key`; `prod-3` volta à `acct-prod`. `deploy/prod/test.sh` confere onde a variável aparece, o `create_api_key` contra um wwz local (devolve a `WWZ_OAUTH_KEY`, recusada em `wwz-fast`) e o `.env` antigo.
- **`tool_calls.probability: 0` explícito vale 0** (task 463, specs 003 e 007). Antes, um bloco `tool_calls` com as três probabilidades em 0 (ou ausentes) voltava a 35/45/50, e `tool_calls: {probability: 0}` chamava tools com 35% de chance. Agora só as probabilidades ausentes (`probability`, `extra_call_probability`, `optional_param_probability`) recebem 35, 45 e 50; `tool_calls: {}` ou ausente seguem os padrões de antes.
- **Verificação estrita aceita o Claude Code real em uso normal** (task 457, spec 020, ADR 0074). Com o prompt e as tools padrão o `client_verification: claude-code` recusava o Claude Code 2.1.308 real (`fingerprint: expected 031, got 7f2`). O `Verify` passa a aceitar o fingerprint de algum bloco `text` da primeira mensagem `user` (os blocos meta vêm antes do prompt e nem sempre têm tag), `inline-tools` e `advisor-tool` presentes ou ausentes (flags do Claude Code), `extended-cache-ttl` ausente com OAuth quando o `cache_control` não tem `ttl` (subagente), o subagente (`x-claude-code-agent-id`/`x-claude-code-parent-agent-id` depois de `x-app`, `cc_is_subagent=true;` no billing header, um sem o outro é `billing_header`), o turno depois de um resultado de tool com os betas de `turn_safeguards` e o tipo novo `classifier` (classificador do modo auto: não-stream, `X-Stainless-Timeout: 60`, beta `context-1m-2025-08-07`). As mutações da spec seguem recusadas com o mesmo `reason`. `tests/claudecode/default-mode.sh` (fora do verify) roda 15 modos do Claude Code real contra um wwz estrito.
- **`wwz-load claude-code` não retenta o 429 da assinatura** (task 459, spec 015 "Transporte"). `shouldRetry` segue a ordem do Claude Code 2.1.308: `connect`/`reset`; 429 com `overage-disabled-reason` de corte nunca retenta; para o assinante (OAuth com `user:inference`, plano não `enterprise`; `Options.Scopes`/`Options.Plan`, sem eles conta como assinante) 429 com header de assinatura nunca retenta e `x-should-retry: true` não força nada; `x-should-retry: false` nunca retenta; 408, 409, 429 e 5xx retentam. Contra o wwz com a cota esgotada, um `ask` manda uma requisição só (eram 3).
- **`wwz-load claude-code` passa os escopos e o plano do estado ao cliente** (task 462, spec 015 "Transporte"). `ask` e `run` com o token do estado informam `scopes` e `plan` do login, então um login `enterprise` volta a retentar o 429 com headers de assinatura, como o Claude Code; `--token` explícito segue como assinante.
- **`verify full` (sdk-node) não esvazia mais o `node_modules` do checkout principal** (task 461). `node_modules_stale` compara o conteúdo do `package-lock.json` com o `node_modules/.package-lock.json` (sem a entrada raiz `""`) em vez do mtime, que o checkout do worktree renova; `node_modules_install` recusa o `npm ci` quando `node_modules` é symlink (falha pedindo o `npm ci` no destino). Selftests dos dois casos.
- **`/api/hello` do driver `anthropic` como rota pública declarada** (task 449). Os 7 métodos que o real responde (GET, HEAD, POST, PUT, DELETE, PATCH, OPTIONS) viram rotas `KindResource` públicas (`PublicRouter`) servidas por `Extra`, fora da verificação estrita do cliente; `HEAD /api/hello` conta como 200 em `wwz_requests_total{endpoint="resource"}` em vez de `route_miss` 404. O drivertest aceita esses métodos em rotas `resource`, o perfil `anthropic` lista `/api/hello` em `Routes` e `PublicRoutes`, e o perfil `openai` passa a exigir `GET /v1/chat/completions`. Fixtures `api_hello_*` inalteradas.
- **`role: "system"` em `messages` como o real** (task 453; 42 fixtures `validation/cc_sys_*.json` e 7 `ct_sys_*.json`, conferidas em 2026-11-03). Recusa `must precede an 'assistant' message or end the array` no fim da sequência de mensagens `system` seguida de algo que não seja `assistant` (uma diretiva no fim conta; só diretivas não são recusadas) e `system text blocks must contain non-whitespace text`, na ordem do real entre as recusas de `system`. Cada bloco de texto de uma mensagem `system` conta como uma mensagem `system` própria (`[s, t]` = 13 no `claude-haiku-5-5`, como `system s` + `system t`; `"st"` = 11; fixtures `template/claude-haiku-5-5/msys_*.json`).
- **Fidelidade do `opencode`** (task 422). `GoUsageLimitError` com `metadata.workspace` = `wrk_` + 26 base32 do sha256 da conta (antes `wrk_wwz`). `BudgetLimitError` com `coverage` `all` ou `models` e `resetsAt` no dia 1º 00:00 UTC (antes `models` e um mês à frente). O bloqueio sai com `This workspace has been suspended` (antes, o texto da falta de Go). `GET <base>/v1/usage` mostra as janelas do dono da cota, a maior entre a cota inteira e as por modelo, e `status: "rate-limited"` quando a janela nega (antes, sempre `ok`).
- **Origem das URLs absolutas** (task 436, ADR 0073). O servidor calcula `<esquema>://<host><prefixo externo><prefixo de montagem>` e a entrega ao driver por um header interno que o cliente não forja. O `base_url` da listagem e o `verification_uri_complete` do Console passam a levar o `drivers[].prefix` e `https` quando a conexão chega por TLS, e saem relativos sem host (antes, o Console saía com `localhost`).
- **Validador antecipado** (task 430). Um modelo fora do catálogo da rota recebe `Model is unavailable.` no mesmo passo de um id desconhecido, também na rota Responses do `opencode`.
- **Ordem real da listagem** (task 438) em `capabilities.cache`, `cache.explicit`, `reasoning.default` e `cost.text.input.overrides[].cache`.
- **Cobertura de `internal/config` de volta a 100 %** (task 439): o operador `lte` das condições `when` de `pricing` passa a ser avaliado num teste (só era validado), o que o `verify-full` exige.

## [0.1.0] - 2026-11-01

Primeira versão. O White Wizard (WWZ) é um servidor em Go que finge ser um provedor de LLM para testar roteadores de modelos sob carga, sem GPU e sem custo de API. Seis drivers (`openai`, `anthropic`, `opencode`, `openrouter`, `gemini`, `azure`) e cinco binários (`wwz`, `wwz-panel`, `wwz-load`, `wwz-tools`, `wwz-drift`). Princípio: nenhum driver diverge do provedor real; o que não pôde ser conferido no real está em "Limitações conhecidas" (ADR 0042).

Verificação desta versão:
- `make verify-full`: todos os passos verdes, cobertura de 100,00% em `internal/` e `cmd/`, 50 testes `TestRealFixtures*` sobre as fixtures reais, SDKs Python e Node e os 16 `examples/`.
- `make verify-heavy` no servidor (amd64):
  - store em cluster verde;
  - vazão de 51 208 req/s; overhead p99 de 0,541 ms a 10 000 req/s;
  - 7 000 streams com ITL p99 de 21,7 ms e TTFT p99 de 26,2 ms;
  - 46 657 chunks/s por núcleo com 20 000 streams;
  - `wwz-drift check --real` sem divergência.
- Produção: smoke 20/20 em HTTPS e nas portas 8000–8007. `wwz-load agents` com 200 agentes por 240 s: 947 250 requisições a 3 778 req/s, sem erros, TTFT p99 de 38,8 ms.
- Gasto total com provedores reais nas conferências: cerca de US$ 0,12 em 1 766 chamadas (teto de US$ 20 por provedor).

### Adicionado

#### Servidor `wwz`
- Data plane fasthttp: `SO_REUSEPORT` (um listener por CPU), rotas por mount (`drivers[].prefix`) e detecção do provedor na raiz (`Sniff`, `keys[].driver`). Também `x-wwz-path`, `clientip` com proxies confiáveis, `server.max_rps` (503 `overloaded` no formato do driver), cancelamento quando o cliente some e TLS por arquivo ou autoassinado em memória (`server.tls.self_signed`).
- Control plane net/http (`cmd/wwz/openapi.yaml`): bearer `WWZ_ADMIN_TOKEN` (sem token, só loopback); `/admin/config` com merge patch e segredos mascarados; recarga atômica por API e por SIGHUP. Rotas de entidades (ver, bloquear, desbloquear, tier, créditos e reset, inclusive `deployment`), caos, cenários, resiliência e `/admin/stats` com percentis.
- Subcomandos `serve`, `validate` (config e cenários, com o caminho do campo no erro), `simulate`, `version`; `-validate-on-load`/`WWZ_VALIDATE_ON_LOAD`.
- Configuração YAML estrita, com padrões embutidos e precedência flag > env > YAML (spec 003): `accounts[]`, `projects[]`, `keys[]`, `tiers` com `promote_when`, `models[]` com `protocols` (dialeto), `drivers` (catálogo), `version`, `deprecated_at`/`retires_at`, `pricing`, `max_output`, `tokenizer` e `prompt_cache`.

#### Drivers e fidelidade
- `openai`, `anthropic`, `openrouter` e `opencode` na forma real, conferidos contra fixtures gravadas dos provedores (`testdata/real/`, mais de 2 000 arquivos). O que foi conferido:
  - ordem dos campos e bytes do JSON;
  - headers de conta, de id e de limite (`x-ratelimit-*`, `anthropic-ratelimit-*`, `x-generation-id`, `x-opencode-log-id`…);
  - envelopes de erro e textos reais;
  - ordem de validação (chave × JSON × campos × modelo);
  - rotas inexistentes e método errado (404/405 e páginas HTML como o real);
  - `/models` com ciclo de vida (`shutdown_date`, `deprecated_at`/`retires_at`) e snapshot em `model`.
- `gemini` (Gemini API `v1beta`/`v1`): `generateContent`, `streamGenerateContent` (SSE com `alt=sse`, array JSON sem ele), `countTokens`, `models` paginado e `cachedContents` guardado no store. Também `promptFeedback` para prompt bloqueado.
- `azure` (Azure OpenAI, pela documentação; ADR 0043): rotas por deployment com `api-version` obrigatório, `/openai/v1/...`, `api-key`, `content_filters` e limites por deployment (TPM do tier, RPM por família).
- OpenRouter:
  - id de modelo com e sem autor e variantes (`:nitro`, `:floor`, `:free` como modelo próprio);
  - limites de `:free` (20/min, 50 ou 1 000/dia) e CORS `*` em toda rota;
  - rotas de conta `key`, `auth/key` e `credits`, com gasto por dia, semana e mês;
  - `cost`/`cost_details` em picodólares.
- OpenCode:
  - `x-opencode-session` obrigatório e todo modelo em toda rota (Chat, Responses, Messages e a rota Gemini em `/inference*`);
  - cotas de 5 h, semanal (de segunda, UTC) e mensal, visíveis em `GET <base>/v1/usage`.
- Raciocínio fiel ao provedor (ADR 0022): oculto na OpenAI (só `reasoning_tokens`), `thinking` com assinatura na Anthropic, `reasoning_details` no OpenRouter, partes `thought` no Gemini.
- Suíte de contrato `internal/driver/drivertest`, verde nos 6 drivers, com um catálogo de drivers quebrados que ela precisa reprovar.
- Testes com os SDKs oficiais nos 6 drivers. Python: `openai` 3.42, `anthropic` 2.3, `google-genai` 2.34 e `AzureOpenAI`. Node: `openai` 7.46, `@anthropic-ai/sdk` 1.3, `@google/genai` 2.32.

#### Responses API
- `POST /v1/responses` no `openai` e no `azure`: forma real, com os 40 campos em ordem, eventos SSE com `sequence_number` e `response.incomplete`. `previous_response_id` usa um registro de respostas no store (TTL de 30 dias; `P:resp:*` no Valkey).
- Na forma própria de cada gateway, no `openrouter` (`/api/v1/responses`) e no `opencode`. Os dois recusam `store: true` e `previous_response_id`, como o real.

#### Geração, tokens e custo
- Modos `random`, `echo`, `lorem`, `template`, `json` e `dataset` (JSONL casado pelo histórico); tool calls, raciocínio e corte em `max_tokens` com `length`/`max_tokens`/`MAX_TOKENS`.
- Tokenizador BPE real para a OpenAI (`o200k_base`, `cl100k_base`, embutido; `models[].tokenizer: auto`). Para Claude e Gemini, tokenizador simulado calibrado por geração. Tools e `json_schema` contados como o real: namespace TypeScript da OpenAI, prompt de tools do Claude 4.x e família GPT-4.1.
- Custo em picodólares, separado em prompt (entrada e cache) e resposta.

#### Cache de prompt
- Igual ao real por provedor (ADRs 0042, 0046 e 0063): prefixos por conta no store (`P:pc:*`), com mínimo, granularidade e TTL de cada provedor.
  - OpenAI: automático 1 024/128, `prompt_cache_key`, `24h`.
  - Anthropic: `cache_control` com 5 min/1 h e breakpoints.
  - Gemini: implícito e `cachedContents`.
  - Gateways: pelo autor ou pelo endpoint.
- Tools no início do hash; `cache_ttft_reduction` afeta só a latência.

#### Latência e streaming
- Distribuições de TTFT e ITL por perfil, efeito do cache no TTFT e pacing contado do fim planejado do chunk anterior.
- Caminho do chunk sem alocação: frame SSE pré-serializado, escrito direto na conexão. A imagem do `wwz` roda com `GOGC=200`.

#### Erros e caos
- Perfis de erro com `types`, `mid_stream_types` e `weights`, além de `errors.burst`. Também `errors.tool_misbehavior` (JSON truncado, vírgula sobrando, aspas simples) e `slow_headers` com `slow_headers_delay`.
- `content_filter`: 200 filtrado na OpenAI e na Anthropic; `promptFeedback` no Gemini; erro próprio no Azure e no OpenRouter.
- `finish_length` no meio do stream, `duplicate_frames` e `errors.outages` (janelas por cron, fuso e driver).
- Headers `x-wwz-chaos-*` e `x-wwz-return-error`. `POST /admin/chaos` cria eventos com escopo por `X-Test-Id` e one-shot com corpo cru.

#### Limites, contas e abuso
- rps, rpm, tpm, itpm, otpm, rpd, tpd, concorrência, rampa, `mode: window`, gasto e créditos.
- Os limites do tier contam na conta, como no real (ADR 0051). `keys[].limits` vira teto da própria chave; `projects[].limits` gera os headers `-project-tokens`.
- Promoção automática de tier por gasto e compras (`add_usd`).
- Escada de abuso persistente (strikes, bloqueio crescente, ban) com texto do provedor; bloqueio e banimento por entidade.
- Políticas do cluster em `cfg:policies` e tier por entidade.

#### Store e eventos
- `store: memory` e `store: valkey` (`valkey-go`): scripts Lua, hash tags, cluster, `store.on_down` e `store.local_first`. Ambos passam a mesma suíte (`internal/limits/storetest`).
- Schema de chaves em `cache/valkey/README.md`.
- Eventos `request`/`error`/limites em `ev:stream` (`eventbus/valkey/asyncapi.yaml`): conta, chave, IP, uso, custo, `endpoint` e `input_breakdown`.

#### Cenários e recursos importados
- Motor stateless de cenários (spec 011) com seis embutidos: `file-agent-flow`, `search-then-answer`, `multi-tool-parallel`, `retry-on-error`, `long-reasoning` e `refusal`.
- Recursos I1–I24 (spec 017): misbehavior por passo, precedência header > passo > chave > modelo > global, `tool_call_id_format`, marcador `[[wwz:`, `usage.extended`/`x-wwz-input-breakdown`, relatório de resiliência e outros.

#### Observabilidade
- Métricas Prometheus `wwz_*` com 29 baldes de latência, espelho `vllm:*`, `wwz_queue_depth` e `wwz_connections_open`. Log estruturado por requisição. Dashboards em `deploy/grafana/`.

#### Painel `wwz-panel`
- SQLite puro em Go (`modernc.org/sqlite`) com schema criado no boot e operador inicial por env. Login local (argon2id) e OIDC, sessão com CSRF e bearer `WWZ_PANEL_API_TOKEN`.
- Fontes de dados: `ev:stream`, Prometheus e API admin. `/api/v1` com 36 operações (`cmd/wwz-panel/openapi.yaml`). Telas HTMX: uso, entidades, limites por conta, erros, eventos, instâncias e políticas.
- Ações via admin com auditoria (bloquear, desbloquear, tier, créditos, reset).
- Backup diário por `VACUUM INTO` com retenção, e restauração.

#### Clientes e ferramentas
- `wwz-load`:
  - modos `run` rps, concurrency, ramp e sweep, com TTFT, ITL e E2E medidos no cliente;
  - relatórios texto, JSON (`version: 1`) e CSV;
  - `--scenario` (conversa por cenário) e `controller`/`worker` distribuídos;
  - `agents` (perfis `coder`/`pesquisa`/`chat`/`mixed`, sessões multi-turno, retry com `retry-after`, semente reproduzível) e `top` (TUI ou `--once` em JSON).
- `wwz-tools` (`cmd/wwz-tools/openapi.yaml`): catálogo de ferramentas por perfil e sandbox virtual em memória, sem rede nem disco real. HTTP em `:9200`.
- `wwz-drift`:
  - `check` compara a assinatura de forma (JSON, SSE, headers e status) entre o real e o wwz, com as sondas de chat, stream, erros, `models` e Responses;
  - `record` grava fixtures normalizadas;
  - o real só é chamado com `--real` e há orçamento em `tmp/real-spend.json`.

#### Verificação, CI e release
- `make verify-quick` (escopo pelo diff), `verify-full` (13 passos, gate de 100% de cobertura, fixtures, workflows, imagens, smoke, SDKs, examples) e `verify-heavy` (cluster, vazão, overhead, streams e drift no servidor pesado).
- Workflows do GitHub Actions (`quick`, `release`, `drift`) prontos, sem remoto (ADR 0018).
- Dockerfile multi-stage com os 5 binários, `CGO_ENABLED=0`. Três imagens distroless nonroot (`white-wizard`, `white-wizard-panel`, `white-wizard-load`) com `LICENSE` e `THIRD_PARTY_NOTICES`.
- `make release-dryrun`: 3 imagens × 2 arquiteturas e 20 binários.

#### Exemplos, produção e documentação
- 16 `examples/` com smoke testado no `verify-full`:
  - `openai-perfeito`, `anthropic-perfeito`, `benchmark-vazao-maxima`;
  - `falhas-20pct`, `latencia-intermitente`, `indisponibilidade-janelas`, `stream-corta-no-meio`;
  - `rate-limit-rpm`, `cota-5h-semana`, `creditos-openrouter-esgotando`, `ban-por-abuso`;
  - `tool-calls-quebradas`, `agente-de-arquivos`, `raciocinio-longo`;
  - `cluster-3-instancias-valkey`, `multi-provedor-failover`.
- `deploy/prod/` (ADR 0017): oito instâncias por papel nas portas 8000–8007, Traefik com HTTPS, Valkey, Prometheus, Grafana e painel internos, teto de 32 núcleos e 64 GB. Também `scripts/prod-deploy.sh` e `scripts/prod-smoke.sh`.
- `deploy/heavy/` para o `verify-heavy`; `docker-compose.yml` local com wwz, Valkey e Prometheus.
- `README.md`, `LICENSE` (Apache-2.0) e `docs/`: uso, configuração, drivers, painel e backup, `wwz-load`, `wwz-tools`, `wwz-drift`, verificação, Grafana e produção.

### Alterado
Mudanças em relação ao documento de design original. Quem escreveu YAML por ele precisa ajustar:
- `server.strict` e `cache_hit_probability` não existem mais (ADR 0042): a validação de requisição vale sempre, com o texto real, e o cache de prompt segue as regras de cada provedor.
- `drivers[].opts` passou a `drivers[].options`, sem alias (ADR 0048).
- `models[].protocols` é só o dialeto de wire; o catálogo de cada driver vem de `models[].drivers` (ADR 0048).
- `reasoning_field` diferente de `hidden` é recusado em `openai`/`azure` (ADR 0046).
- `opencode.require_session` e `anthropic.require_version` só aceitam `true` (ADRs 0059 e 0068).
- Limites do tier na conta (ADR 0051): as chaves de uma conta dividem os limites, e chaves da mesma conta com tiers diferentes tornam a configuração inválida.
- `models[].tokenizer` tem padrão `auto` (BPE real nos modelos OpenAI) e só aceita `auto`, `simulated`, `tiktoken:o200k_base` e `tiktoken:cl100k_base` (ADR 0053).
- A configuração passa a ser recusada com o caminho do campo:
  - nomes de tier fora de `[A-Za-z0-9._-]{1,64}` (ADR 0031);
  - tipos de erro desconhecidos e `weights` inválidos;
  - chave de `models[].extra` que nenhum dialeto conhece;
  - `generation.mode` desconhecido.
- A rota de compatibilidade `GET /api/v1/models/{slug}` do OpenRouter saiu; vale a real `GET /api/v1/model/{author}/{slug}` (ADRs 0040 e 0046).
- Meta de streams pelo custo medido (ADRs 0067, 0069 e 0071): a vazão por núcleo deixou de ser meta. ITL p99 ≤ 22 ms vale com o wwz a ≤ 80% de CPU (7 000 streams em 12 núcleos no `verify-heavy`). Os 20 000 streams ficam só como medida de capacidade.

### Corrigido
Defeitos achados na verificação final e já corrigidos nesta versão:
- O system message não sai mais duplicado no Chat Completions nem na Responses.
- A re-subscrição de `cfg:policies` em cluster não tem mais corrida.
- O sorteio de `x-wwz-return-error: content_filter` não cai mais por cima do erro pedido.
- O registro da Responses de um stream cortado guarda só a saída entregue.
- Testes de janelas de gasto independem do calendário.
- O `verify-quick` é estável com várias execuções simultâneas.
- O smoke de produção escolhe o modelo do OpenRouter pelo id.

### Decidido
ADRs 0001–0071 em `specs/decisions/`. Marcos:
- `./specs/` é a fonte da verdade (0002).
- fasthttp no data plane e net/http no admin (0003).
- Go 1.27 (0001).
- Cobertura de 100% (0013).
- Fidelidade bit a bit com fixtures reais (0014) e fidelidade total ao real, com teto de US$ 20 por provedor (0042).
- Gemini e Azure no MVP (0043).
- Painel com `modernc.org/sqlite` (0044).
- Deploy em `/opt/white-wizard` (0017); publicação só local até a tag (0018).

As ADRs 0019–0027, 0031–0039 e 0042–0044 foram aceitas pelo humano. As ADRs 0008, 0040, 0041 e 0045–0071 seguem provisórias, decididas pelo loop autônomo (ADR 0012), até a ratificação.

### Limitações conhecidas
Comportamentos implementados sem conferência no real (marcados "não conferido" nas specs; lista única na spec 006, seção "Não conferidos no real"):
- **Azure:** todo o driver segue a documentação (sem chave real), e o `wwz-drift` o pula.
- **OpenAI:**
  - itens de raciocínio (`summary`, `encrypted_content`) e modelos gpt-5/o-series, porque a chave não tinha acesso;
  - headers `x-ratelimit-*-project-tokens`, só pela documentação;
  - na Responses: `event: error`/`response.failed` no meio do stream, `refusal`, `content_filter` → `incomplete`, tools embutidas e o registro de um stream cortado.
- **Anthropic:**
  - 400 de limite de uso do workspace e limite mais restritivo workspace × org (falta chave admin);
  - `stop_details` da recusa, pela documentação;
  - constantes de tools do Claude 5.x não aplicadas.
- **OpenRouter:**
  - 402 do teto por chave e `limit_reset` (falta chave de provisionamento);
  - forma da 429 diária de `:free`;
  - ordem 404 × 401 nas variantes e `:batch`;
  - raciocínio na Responses.
- **OpenCode:**
  - razão `allowance`/`list_cost` fora dos modelos medidos;
  - erros de cota e crédito (`GoUsageLimitError`, `BudgetLimitError`, `CreditLimitExceeded`), 402/403/413/5xx e erro no meio do stream;
  - `/v1/usage` com a cota esgotada.
  - O `wwz-drift check` sai `upstream_form_unreproduced` quando o real não devolve a forma `gw-`.
- **Gemini:**
  - 429 `RESOURCE_EXHAUSTED` com detalhes, 5xx, chave expirada, conta suspensa e bloqueio geográfico;
  - acerto do cache implícito e contagem exata do tokenizador;
  - `promptFeedback` de prompt bloqueado, pela documentação.
- **Todos os drivers:** o balde de tokens ao mudar de tier (ADR 0058).

Fora do escopo desta versão:
- Embeddings, batches, arquivos e rotas de recurso da Responses (ADR 0054).
- Picos periódicos de latência (`latency.burst`), que podem ser simulados com `errors.burst` ou cenário (ADR 0056).
- YAML efetivo por instância no painel.
- ITL p99 ≤ 22 ms com 20 000 streams simultâneos, que exigiria mais de 32 núcleos no servidor.

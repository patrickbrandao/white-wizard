# 016 — Painel e dashboards

Status: rascunho
Depende de: 012, 013
Origem: seções 19 do documento Claude Docs congelado em 2026-10-30; detalhada para a fase 3 em 2026-10-31 (task 040)

## Contexto
Serviço `wwz-panel`: telas, fontes de dados, acesso, API e alertas (seção 19), mais o bootstrap do operador e o backup decididos depois. A seção "Contrato do `wwz-panel` (fase 3)", no fim deste arquivo, é normativa: configuração, schema SQLite, fontes de dados, papéis, sessão, backup e divisão das telas.

## Requisitos
- R18
- RF: com a base de usuários vazia, `WWZ_PANEL_ADMIN_USER` e `WWZ_PANEL_ADMIN_PASSWORD` criam o primeiro operador; sem senha, gera uma aleatória e a imprime uma única vez no log; ignorado quando já há usuários (ADR 0005)
- RF: `wwz-panel backup` gera snapshot diário do SQLite (`VACUUM INTO`) retido 7 dias; restauração coberta no verify-full (ADR 0009)
- RF: toda ação sobre entidades passa pela API admin do wwz; o painel nunca escreve `ent:` no Valkey. A única escrita do painel no Valkey é `cfg:policies` + `PUBLISH cfg:changed` (ADR 0025).
- RF: toda ação de operador vira uma linha de `audit` com usuário, data, ação, alvo e diff.
- RF: funciona com `store: memory` (sem Valkey) e sem Prometheus, com as degradações descritas em "Fontes de dados".
- RNF: binário único `CGO_ENABLED=0`, dependências da ADR 0023, sem npm e sem CDN.

## Interfaces e contratos
- Contrato `/api/v1`: `cmd/wwz-panel/openapi.yaml` (task 041). Divergências encontradas entre contrato e código são resolvidas no contrato ou por ADR ([0039](decisions/0039-remocao-de-tier-atribuido-no-painel.md)); a composição da entrada (`input_breakdown`, I16) ainda está em implementação (task 107).
- Eventos: [eventbus/valkey/asyncapi.yaml](../eventbus/valkey/asyncapi.yaml) (grupo `panel`).
- Chaves lidas e `cfg:policies`: [cache/valkey/README.md](../cache/valkey/README.md).
- Ações: rotas `/admin/*` de [cmd/wwz/openapi.yaml](../cmd/wwz/openapi.yaml), inclusive `tier` e `reset` por entidade (ADR 0025).

## Dados
- SQLite local do painel (`users`, `sessions`, `audit`, `events` por `retention_days` = 7, `settings`) em volume; schema abaixo.

## Critérios de aceite
- [ ] Operador inicial criado só com base vazia
- [ ] Restauração de backup testada
- [ ] Schema criado de forma idempotente com `PRAGMA user_version = 1`
- [ ] `viewer` recebe 403 em qualquer ação; toda ação de `operator` gera linha em `audit`
- [ ] Bloquear e desbloquear uma chave pelo painel muda o estado em `/admin/entities` do wwz
- [ ] Sem Prometheus, a visão geral cai para `/admin/stats`; sem Valkey, entidades vêm de `/admin/entities`
- [ ] As 8 telas existem e cada rota de `/api/v1` está no `openapi.yaml`
- [ ] Limites por conta: abrir uma chave mostra o tier e a conta dela (`account_id`); atribuir tier e comprar créditos (`add_usd`) numa chave agem na conta e ficam em `audit`; `purchased_usd` aparece no estado; projeto, IP e usuário não mostram tier
- [ ] Uso da inferência: taxa de cache hit correta com eventos de `cached_tokens: 0`; grupo só com modelo sem preço mostra US$ 0; decomposição da entrada somada por grupo a partir de `fields.input_breakdown` e "—" sem ela (ADR 0032)

## Fora de escopo
- Edição de cenários em editor visual
- Gestão de usuários pela interface (usuários vêm do bootstrap, do `panel.yaml` e do OIDC)
- Edição de limite por entidade fora de tiers (ADR 0025: crie um tier em políticas e atribua à entidade)
- YAML efetivo por instância quando o heartbeat não traz endereço admin (fase 3 mostra a instância de `WWZ_PANEL_WWZ_ADMIN_URL`)

## Decisões relacionadas
- [specs/decisions/0005-bootstrap-painel.md](decisions/0005-bootstrap-painel.md)
- [specs/decisions/0006-contratos-openapi.md](decisions/0006-contratos-openapi.md)
- [specs/decisions/0009-backup.md](decisions/0009-backup.md)
- [specs/decisions/0019-formato-ev-stream.md](decisions/0019-formato-ev-stream.md)
- [specs/decisions/0023-dependencias-do-painel.md](decisions/0023-dependencias-do-painel.md)
- [specs/decisions/0025-politicas-e-tier-por-entidade.md](decisions/0025-politicas-e-tier-por-entidade.md)
- [specs/decisions/0027-formas-finais-dos-recursos-importados.md](decisions/0027-formas-finais-dos-recursos-importados.md)
- [specs/decisions/0032-campos-de-uso-no-ev-stream.md](decisions/0032-campos-de-uso-no-ev-stream.md)

## Comportamento

### 19. Painel e dashboards

O painel é um serviço separado (`wwz-panel`, Go + HTML/HTMX, binário único) que lê o Valkey e o Prometheus; ele nunca fica no caminho da requisição, então não custa vazão. Qualquer instância do simulador pode ser derrubada sem o painel perder dados, e o painel pode cair sem afetar o teste.

**Fontes de dados.** Métricas agregadas vêm do Prometheus (consultas PromQL prontas). Eventos individuais (quem foi bloqueado, qual chave estourou qual limite) vêm do stream `ev:stream` do Valkey, consumidos por um grupo de consumidores e mantidos em SQLite local do painel por `retention_days` (padrão 7). Estado atual das entidades vem direto das chaves `ent:`, `block:`, `ban:`, `rl:`.

**Telas**

| Tela | Mostra | Ações |
| --- | --- | --- |
| Visão geral | req/s e tokens/s do cluster (1 min, 5 min, 1 h), streams abertos, p50/p99 de TTFT e E2E, erros/s por tipo, instâncias vivas com versão e carga | Selecionar janela, driver, modelo |
| Uso da inferência falsa | Por driver, modelo, chave, conta e IP: requisições, tokens de entrada/saída/raciocínio, tool calls por nome, gasto simulado em dólares, taxa de cache hit, decomposição da entrada por origem (I16) | Exportar CSV, ver série temporal |
| Limites | Por conta ([ADR 0051](decisions/0051-limites-na-conta-e-promocao-de-tier.md)): cada limite com `limit/remaining/reset` ao vivo, barras de consumo por janela (minuto, 5 h, dia, semana, mês), tier da conta, saldo e total comprado (`purchased_usd`). Aberta numa chave, mostra o tier e a conta da chave (`account_id`) e o teto próprio de `keys[].limits`; projeto mostra só `projects[].limits`; IP e usuário não têm tier | Alterar o tier da conta, comprar créditos (`add_usd`), zerar contador |
| Erros | Erros injetados por tipo, fase (pré/mid-stream) e driver; erros reais do simulador (parse, Valkey indisponível); amostra das últimas N requisições com o motivo | Ligar/desligar injeção por driver ou modelo, disparar evento de caos |
| Bloqueios e banimentos | Lista viva de IPs, chaves, usuários e contas bloqueados ou banidos: motivo, desde quando, até quando, strikes, quem aplicou (regra ou operador) | Bloquear, desbloquear, banir, perdoar strikes, marcar chave como vazada |
| Cenários | Cenários carregados, quantas conversas estão em cada passo, taxa de `on_no_match` | Carregar YAML, desativar, simular um cenário |
| Configuração | YAML efetivo por instância, políticas em `cfg:policies` | Editar políticas e publicar para o cluster (pub/sub) |
| Auditoria | Toda ação de operador com usuário, data e diff | Filtrar, exportar |

**Dashboards Grafana** em `deploy/grafana/`, com as mesmas visões em PromQL, para quem já tem Grafana; o painel próprio existe para as ações (bloquear, banir, editar limites), que o Grafana não faz.

**Alertas** (regras Prometheus prontas): instância sem heartbeat, Valkey em circuito aberto, taxa de 429 acima de X %, p99 de overhead do simulador acima de 2 ms (o simulador está virando gargalo), fila `ev:stream` acima de 500 000 (painel não acompanha).

**Acesso.** Login local (usuário/senha em YAML com hash argon2) ou OIDC; papéis `viewer` e `operator`. O painel fala com o simulador só pelo Valkey e pela API admin (seção 10), nunca pelos endpoints de inferência.

**API do painel.** Tudo que a tela faz está em `/api/v1/*` (JSON), para automação em CI: `GET /api/v1/entities?status=banned`, `POST /api/v1/entities/key/{id}/block`, `GET /api/v1/usage?group=model&window=1h`.

## Contrato do `wwz-panel` (fase 3)

### Subcomandos

| Subcomando | O que faz |
| --- | --- |
| `serve` (padrão) | Sobe o painel, o consumidor do `ev:stream`, a purga diária e o backup agendado |
| `backup [--dir D]` | Snapshot sob demanda (`D` padrão `WWZ_PANEL_BACKUP_DIR`; sem nenhum dos dois, código 2) |
| `restore <arquivo>` | Restaura um snapshot com o painel parado |
| `hash-password` | Lê a senha do stdin e imprime o hash argon2id (formato PHC) para o `panel.yaml` |
| `version` | Versão, commit e Go |

Códigos de saída: `0` ok, `1` falha, `2` uso inválido. Flags `-config` e `-listen` sobrepõem as variáveis.

### Configuração

Precedência: flag > variável de ambiente > `panel.yaml` > padrão (como a spec 003). Segredos só por variável de ambiente; o `panel.yaml` com campo de segredo em texto (`password`, `client_secret`, `token`) é recusado na validação. Erros de validação citam o campo e encerram com código 1.

| Variável | Campo do `panel.yaml` | Padrão | Significado |
| --- | --- | --- | --- |
| `WWZ_PANEL_CONFIG` | — | vazio | Caminho do `panel.yaml` (opcional) |
| `WWZ_PANEL_LISTEN` | `listen` | `:8100` | Endereço HTTP do painel (net/http, ADR 0003) |
| `WWZ_PANEL_PUBLIC_URL` | `public_url` | vazio | URL externa; base do redirect OIDC. Com `https://`, o cookie leva `Secure` |
| `WWZ_PANEL_DB` | `db` | `panel.db` | Arquivo SQLite (na imagem, `/data/panel.db`) |
| `WWZ_PANEL_BACKUP_DIR` | `backup.dir` | vazio | Diretório dos snapshots; vazio desliga o backup agendado |
| `WWZ_PANEL_BACKUP_AT` | `backup.at` | `03:00` | Horário UTC (HH:MM) do snapshot diário |
| `WWZ_PANEL_BACKUP_KEEP` | `backup.keep` | `7` | Snapshots retidos |
| `WWZ_PANEL_RETENTION_DAYS` | `retention_days` | `7` | Retenção da tabela `events` |
| `WWZ_PANEL_VALKEY_ADDR` (senão `WWZ_VALKEY_ADDR`) | `valkey.addr` | vazio | Lista `host:porta` separada por vírgula; vazio = modo sem Valkey |
| `WWZ_PANEL_VALKEY_PASSWORD` (senão `WWZ_VALKEY_PASSWORD`) | — | vazio | Senha do Valkey |
| `WWZ_PANEL_VALKEY_PREFIX` | `valkey.prefix` | `wwz` | Mesmo valor de `store.prefix` do wwz |
| `WWZ_PANEL_VALKEY_CLUSTER` | `valkey.cluster` | `false` | Valkey Cluster |
| `WWZ_PANEL_WWZ_ADMIN_URL` | `wwz.admin_url` | `http://127.0.0.1:9000` | Control plane do wwz (uma instância ou o balanceador do admin) |
| `WWZ_ADMIN_TOKEN` | — | vazio | Bearer enviado ao admin do wwz (mesmo nome do wwz, ADR 0004) |
| `WWZ_PANEL_PROMETHEUS_URL` | `prometheus.url` | vazio | Base da HTTP API do Prometheus; vazio = gráficos por `/admin/stats` |
| `WWZ_PANEL_SESSION_KEY` | — | vazio | Chave HMAC do cookie (≥ 32 bytes); vazia = gerada no primeiro boot e guardada em `settings` |
| `WWZ_PANEL_SESSION_TTL` | `session.ttl` | `12h` | Expiração deslizante da sessão |
| `WWZ_PANEL_ADMIN_USER` | — | `admin` | Bootstrap (ADR 0005) |
| `WWZ_PANEL_ADMIN_PASSWORD` | — | vazio | Bootstrap; vazio = gerada e logada uma vez |
| `WWZ_PANEL_API_TOKEN` | — | vazio | Token de automação: `Authorization: Bearer <token>` em `/api/v1` age como `operator` com usuário de auditoria `api-token`; vazio desliga |
| `WWZ_PANEL_OIDC_CLIENT_SECRET` | — | vazio | Segredo do cliente OIDC |
| `WWZ_PANEL_LOG_LEVEL` | `log_level` | `info` | `debug`, `info`, `warn`, `error` (slog JSON) |

`panel.yaml` completo:

```yaml
listen: ":8100"
public_url: https://painel.exemplo
db: /data/panel.db
retention_days: 7
log_level: info
backup: {dir: /backup, at: "03:00", keep: 7}
wwz: {admin_url: "http://wwz:9000"}        # token só por WWZ_ADMIN_TOKEN
valkey: {addr: ["valkey:6379"], prefix: wwz, cluster: false}
prometheus: {url: "http://prometheus:9090"}
session: {ttl: 12h}
users:                                      # login local; hash de `wwz-panel hash-password`
  - {name: ana, role: operator, password_hash: "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"}
oidc:                                       # opcional; client_secret só por env
  issuer: https://login.exemplo
  client_id: wwz-panel
  scopes: [openid, email, profile]
  username_claim: email                     # vira users.name
  role_claim: groups                        # string ou lista de strings
  operator_values: [wwz-operators]
  viewer_values: [wwz-viewers]              # vazio: todo autenticado que não é operator vira viewer
  default_role: viewer                      # viewer | none (none recusa o login sem papel)
```

Usuários do `panel.yaml` entram em `users` no boot com `source = 'yaml'` (atualizando hash e papel dos que já vieram do YAML); não sobrescrevem usuários de outra origem com o mesmo nome (conflito é logado). Usuários do YAML contam como "base não vazia" para o bootstrap só depois de inseridos; o bootstrap roda antes da sincronização do YAML.

### Autenticação, papéis e sessão

- Senhas: argon2id com `m=65536` (64 MiB), `t=3`, `p=2`, sal de 16 bytes, chave de 32 bytes, formato PHC; hashes com outros parâmetros são verificados pelos parâmetros do próprio hash. Comparação em tempo constante. Limite de 10 tentativas de login por minuto por IP (429).
- OIDC: authorization code com PKCE, `state` e `nonce`; rotas `GET /auth/oidc/login` e `GET /auth/oidc/callback` (redirect = `public_url` + `/auth/oidc/callback`); o papel é recalculado a cada login; usuário criado/atualizado com `source = 'oidc'` e `oidc_subject = iss + "|" + sub`.
- Sessão: id aleatório de 32 bytes; cookie `wwz_panel_session` = `base64url(id) + "." + base64url(HMAC-SHA256(chave, id))`, `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` com `public_url` https ou `X-Forwarded-Proto: https`. No banco fica `sha256(id)`. Logout e troca de papel revogam.
- CSRF: token aleatório por sessão (`sessions.csrf`), exigido em todo método não-GET autenticado por cookie, no header `X-CSRF-Token` (HTMX via `hx-headers`) ou no campo de formulário `csrf`. `POST /api/v1/login` não tem sessão: exige `Origin` igual ao host do painel (ou ausente em cliente não-navegador). Requisições com `Authorization: Bearer` (token de API) dispensam CSRF.
- Papéis: `viewer` lê tudo; `operator` lê e executa ações. Ação de `viewer` → 403; sem sessão → 401 (API) ou redirecionamento ao login (HTML).

### Schema SQLite

Banco aberto com `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON` e um único escritor. No boot, numa transação: as instruções abaixo e `PRAGMA user_version = 1`. Banco com `user_version` maior que o suportado: o painel recusa subir (código 1). Mudança futura de schema acrescenta instruções idempotentes e incrementa a versão (sem migrations versionadas, specs/002). Tempos em milissegundos Unix.

```sql
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  role          TEXT NOT NULL CHECK (role IN ('viewer','operator')),
  source        TEXT NOT NULL CHECK (source IN ('bootstrap','yaml','oidc')),
  password_hash TEXT,                       -- PHC argon2id; NULL para oidc
  oidc_subject  TEXT UNIQUE,                -- iss|sub
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_login_at INTEGER
);
CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,            -- sha256 hex do id do cookie
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  revoked      INTEGER NOT NULL DEFAULT 0,
  ip           TEXT,
  user_agent   TEXT
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE TABLE IF NOT EXISTS audit (
  id     INTEGER PRIMARY KEY,
  at     INTEGER NOT NULL,
  user   TEXT NOT NULL,                     -- users.name ou 'api-token'
  role   TEXT NOT NULL,
  action TEXT NOT NULL,                     -- entity.block, entity.tier, chaos.start, policies.publish, ...
  target TEXT NOT NULL,                     -- key/test-01, policies, chaos, scenario/nome
  diff   TEXT NOT NULL,                     -- JSON {"request": ..., "before": ..., "after": ...}
  result TEXT NOT NULL CHECK (result IN ('ok','error')),
  error  TEXT,
  ip     TEXT
);
CREATE INDEX IF NOT EXISTS audit_at ON audit(at);
CREATE TABLE IF NOT EXISTS events (
  stream_id   TEXT PRIMARY KEY,             -- id do XADD; 'req:<request_id>' no modo sem Valkey
  at          INTEGER NOT NULL,             -- campo t do evento
  type        TEXT NOT NULL,                -- request | error | block | unblock | ban | unban | strike
  instance    TEXT,
  request_id  TEXT,
  test_id     TEXT,
  driver      TEXT,
  model       TEXT,
  entity_kind TEXT,
  entity_id   TEXT,
  status      INTEGER,
  error       TEXT,
  sample      REAL NOT NULL DEFAULT 1,      -- peso na agregação = 1/sample
  data        TEXT NOT NULL                 -- JSON completo do evento
);
CREATE INDEX IF NOT EXISTS events_at ON events(at);
CREATE INDEX IF NOT EXISTS events_type_at ON events(type, at);
CREATE INDEX IF NOT EXISTS events_entity ON events(entity_kind, entity_id, at);
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,              -- session_key, last_backup_at, ...
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
```

Inserção de eventos com `INSERT OR IGNORE` (idempotente pelo `stream_id`). Purga de `events` com `at < agora − retention_days` no boot e a cada 24 h; sessões expiradas ou revogadas há mais de 7 dias são apagadas na mesma purga. `audit` nunca é purgada.

### Fontes de dados

| Dado | Com Valkey | Sem Valkey (`store: memory`) |
| --- | --- | --- |
| Eventos | `ev:stream` pelo grupo `panel` (asyncapi): `XGROUP CREATE ... panel $ MKSTREAM` (ignora `BUSYGROUP`), `XREADGROUP GROUP panel <hostname> COUNT 500 BLOCK 1000`, grava em lote em `events` e só então `XACK`; `XAUTOCLAIM` de pendentes há mais de 60 s; reconexão com backoff exponencial até 30 s | A cada 5 s, `GET /admin/requests?last=1000` (exige `admin.debug: true` no wwz; sem ele a tela avisa); cada registro vira evento `request` (ou `error` se `status ≥ 400`) com `stream_id = 'req:' + request_id` |
| Entidades | `idx:ent`/`idx:status:*` + hash `ent:` (status, motivo, until, strikes, tier, créditos) | `GET /admin/entities?status=` e `GET /admin/entities/{tipo}/{id}` |
| Limites ao vivo | `tb:`, `win:`, `q5h:`, `qwk:` conforme o README (`limit/remaining/reset` calculados no painel com os limites efetivos do tier) | Só o estado de `GET /admin/entities/{tipo}/{id}` |
| Instâncias | `idx:inst` + `inst:` (versão, endereço, rps, conexões, em voo, estado do store) | A instância de `WWZ_PANEL_WWZ_ADMIN_URL` via `/admin/stats` |
| Métricas agregadas | Prometheus (consultas abaixo); sem Prometheus, `/admin/stats` | Idem |
| Ações | Sempre a API admin do wwz | Idem |
| Políticas | Lê e escreve `cfg:policies` (ADR 0025) | Indisponível: a tela mostra os `tiers` de `/admin/config` só para leitura e a API responde 409 |

Falha de uma fonte não derruba o painel: a API responde 502 com o envelope de erro para a parte que dependia dela e `/health/ready` lista o estado de cada fonte.

**Consultas PromQL** (`$w` = janela `1m`, `5m` ou `1h`; filtros de driver e modelo viram matchers `{driver="...", model="..."}`; séries por `query_range` com passo `janela/60`):

| Painel | Consulta |
| --- | --- |
| req/s | `sum(rate(wwz_requests_total[$w]))` |
| tokens/s | `sum by (kind) (rate(wwz_tokens_total[$w]))` |
| Streams abertos | `sum(wwz_requests_in_flight)` |
| TTFT p50/p99 | `histogram_quantile(0.5\|0.99, sum by (le) (rate(wwz_ttft_seconds_bucket[$w])))` |
| E2E p50/p99 | Idem com `wwz_e2e_seconds_bucket` |
| Erros/s por tipo | `sum by (kind) (rate(wwz_errors_injected_total[$w]))` e `sum by (status) (rate(wwz_requests_total{status=~"4..\|5.."}[$w]))` |
| Rate limit/s | `sum by (limit) (rate(wwz_rate_limited_total[$w]))` |

**Uso da inferência falsa:** por driver e modelo vem do Prometheus quando disponível (exato, sem amostragem); por chave, conta e IP, tool calls por nome, tokens de raciocínio, gasto e cache hit vêm da agregação de `events` do tipo `request` com peso `1/sample`, agrupando pelos campos de topo do evento `key`, `account` e `ip` (ids crus; `account` quando a chave tem conta) e somando os campos de `fields` (`prompt_tokens`, `output_tokens` e, quando o wwz os publicar, `reasoning_tokens`, `cached_tokens`, `cost_usd`, `tool_calls`), conforme o [asyncapi](../eventbus/valkey/asyncapi.yaml) 0.4.0 ([ADR 0032](decisions/0032-campos-de-uso-no-ev-stream.md)). A conta padrão `acct-<label>` (chave sem `account` no YAML) aparece no agrupamento "por conta" como qualquer outra. A taxa de cache hit é a fração (ponderada) dos eventos atendidos com `cached_tokens > 0` entre os que trazem `cached_tokens`; negações não entram. O gasto soma `cost_usd`, que o wwz publica também quando é 0 (modelo gratuito ou sem preço), então o grupo mostra US$ 0, não "—". `tool_calls` ausente num evento que traz `cost_usd` conta 0 tool calls. Coluna sem dado em nenhum evento do grupo (eventos de wwz anterior ao asyncapi 0.4.0) aparece como "—", nunca como zero.

**Decomposição da entrada (I16, [ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)).** Na mesma tela, com o mesmo agrupamento e filtros (driver, modelo, chave, conta, IP, teste), um bloco "Composição da entrada" soma com peso `1/sample` cada chave de `fields.input_breakdown` (`system`, `tool_schemas`, `user`, `assistant`, `tool_calls`, `tool_results`, `images`, `cached`, `total`) só sobre os eventos que trazem o campo, e mostra cada origem em tokens e em % de `total` (barra empilhada por grupo), mais a contagem de requisições com decomposição. O campo só existe com `usage.extended: true` no wwz; grupo sem nenhum evento com decomposição mostra "—" e a dica "ligue `usage.extended`". O CSV exportado leva as nove colunas `input_<origem>`.

### Métricas e saúde do painel

- `GET /health`: 200 enquanto o processo vive. `GET /health/ready`: 200 quando o SQLite abriu; corpo `{"status": "ready", "sources": {"valkey": "ok|down|off", "admin": "ok|down", "prometheus": "ok|down|off"}}`. Ambas sem autenticação.
- `GET /metrics` (sem autenticação, formato texto do Prometheus escrito com a stdlib): `wwz_panel_stream_lag` (campo `lag` de `XINFO GROUPS` do grupo `panel`), `wwz_panel_events_ingested_total`, `wwz_panel_last_backup_timestamp_seconds`, `wwz_panel_source_up{source}`. Base do alerta "fila `ev:stream` acima de 500 000".

### Alertas (regras Prometheus em `deploy/prometheus/`)

| Alerta | Expressão | Por |
| --- | --- | --- |
| Instância sem heartbeat | `up{job="wwz"} == 0` | 1m |
| Valkey em circuito aberto | `max(wwz_store_circuit_open) == 1` | 1m |
| 429 acima de 5 % | `sum(rate(wwz_requests_total{status="429"}[5m])) / sum(rate(wwz_requests_total[5m])) > 0.05` | 5m |
| Overhead do simulador | `histogram_quantile(0.99, sum by (le) (rate(wwz_overhead_seconds_bucket[5m]))) > 0.002` | 5m |
| Painel atrasado no `ev:stream` | `wwz_panel_stream_lag > 500000` | 5m |

### Ações e mapeamento para a API admin

Cada ação do painel chama uma rota de `cmd/wwz/openapi.yaml` e grava `audit` com `diff.before` (estado da entidade antes, via `GET`), `diff.request` e `diff.after` (resposta). Erro do admin vira `result = 'error'` e a API do painel responde 502 (ou repassa 400/404).

| Ação do painel | Rota do wwz |
| --- | --- |
| block, unblock, ban, unban | `POST /admin/entities/{tipo}/{id}/<ação>` |
| credits (definir saldo `usd` ou comprar `add_usd`, exclusivos; compra `> 0`) | `POST /admin/entities/{tipo}/{id}/credits`; `add_usd` numa chave vai para a conta dela (ADR 0058) e só a compra na conta dispara `promote_when` |
| forgive (perdoar strikes) | `POST /admin/entities/{tipo}/{id}/reset` com `{"scope": ["strikes"]}` (ADR 0025) |
| leak (marcar chave vazada) | `POST /admin/leak` |
| tier | `POST /admin/entities/{tipo}/{id}/tier` (ADR 0025); `tipo = key` é resolvido para a conta da chave e a resposta é o estado da conta (ADR 0051) |
| reset (zerar contadores da entidade) | `POST /admin/entities/{tipo}/{id}/reset` (corpo vazio = `{"scope": ["counters"]}`, ADR 0025) |
| Ligar/desligar injeção por driver ou modelo | `POST /admin/config` (merge patch em `models.<id>.errors` ou no perfil) |
| Disparar/parar caos | `POST /admin/chaos`, `DELETE /admin/chaos` |
| Carregar/remover cenário | `POST /admin/scenarios` (YAML), `DELETE /admin/scenarios?name=` |
| Publicar políticas | Escrita direta em `cfg:policies` (única escrita no Valkey, ADR 0025) |

### Backup e restauração (ADR 0009)

- `serve` com `WWZ_PANEL_BACKUP_DIR` faz um snapshot por dia em `WWZ_PANEL_BACKUP_AT` (UTC): `VACUUM INTO '<dir>/.panel-AAAAMMDD.db.tmp'` seguido de rename para `<dir>/panel-AAAAMMDD.db` (data UTC; o do mesmo dia é substituído), depois apaga os mais antigos pelo nome além de `WWZ_PANEL_BACKUP_KEEP`. Grava `last_backup_at` em `settings` e atualiza a métrica.
- `wwz-panel backup` faz o mesmo sob demanda (pode rodar com o `serve` no ar: `VACUUM INTO` lê um snapshot consistente).
- `serve` mantém `flock` exclusivo em `WWZ_PANEL_DB + ".lock"` enquanto roda. `wwz-panel restore <arquivo>`: tenta o lock sem bloquear (preso → código 1, "painel em execução"); confere `PRAGMA integrity_check` = `ok` no arquivo e `user_version` ≤ suportado; copia para `<WWZ_PANEL_DB>.restore.tmp` no mesmo diretório, `fsync`, apaga `-wal`/`-shm` do banco atual e faz rename atômico sobre `WWZ_PANEL_DB`. Arquivo inválido não toca o banco atual. O bootstrap não recria o operador depois de restaurar (a base já tem usuários).

### Telas da fase 3

Todas as 8 telas entram na fase 3, em HTML server-side (`html/template` + HTMX vendorizado, ADR 0023), consumindo a mesma camada de serviço da API (sem duplicar lógica nem chamar a própria API por HTTP). Atualização ao vivo por `hx-trigger="every 2s"` nas visões de leitura. `Content-Security-Policy` sem script inline e `X-Frame-Options: DENY`. Botões de ação só aparecem para `operator`, sempre com confirmação e CSRF.

| Bloco | Telas | Tarefa |
| --- | --- | --- |
| Núcleo | Login (local e OIDC), Visão geral, Uso da inferência falsa, Bloqueios e banimentos, Auditoria | 050 |
| Restantes | Limites, Erros, Cenários, Configuração | 053 |

Na tela "Cenários", "simular" significa carregar o YAML por `POST /admin/scenarios` e mostrar o resultado da validação (o painel não roda `wwz simulate`). Na tela "Configuração", o YAML efetivo é o da instância de `WWZ_PANEL_WWZ_ADMIN_URL`; as demais instâncias vivas aparecem listadas pelo heartbeat. Na tela "Limites", editar limite é editar o tier em políticas e atribuí-lo à conta (ADR 0025/0051); não há edição de limite por entidade fora de tiers. `GET /api/v1/entities/{kind}/{id}` devolve `effective_tier` (tier da conta, também para uma chave; vazio para projeto, IP e usuário) e `account_id` (só para chave); o estado traz `purchased_usd`.

# Painel (wwz-panel)

O `wwz-panel` é o painel de operação do cluster: usuários, auditoria, uso, bloqueios e políticas, por cima do Valkey e da API admin do `wwz`. Contrato completo na [spec 016](../specs/016-painel.md); backup e restauração em [painel-backup.md](painel-backup.md).

```sh
wwz-panel serve                       # lê WWZ_PANEL_* e, se existir, o panel.yaml
wwz-panel serve --config panel.yaml --listen :8100
```

## Configuração por ambiente

Precedência: **flag > `WWZ_PANEL_*` > `panel.yaml` > padrão**. Todas as variáveis são opcionais; as que guardam segredo não têm campo no YAML.

| Variável | Campo do YAML | Padrão | Efeito |
| --- | --- | --- | --- |
| `WWZ_PANEL_CONFIG` | — | vazio | Caminho do `panel.yaml` (como `--config`) |
| `WWZ_PANEL_LISTEN` | `listen` | `:8100` | Endereço HTTP (como `--listen`) |
| `WWZ_PANEL_PUBLIC_URL` | `public_url` | vazio | URL externa; base do redirect OIDC. Com `https://`, o cookie leva `Secure` |
| `WWZ_PANEL_DB` | `db` | `panel.db` | Arquivo SQLite (na imagem, `/data/panel.db`) |
| `WWZ_PANEL_BACKUP_DIR` | `backup.dir` | vazio | Diretório dos snapshots; vazio desliga o backup agendado |
| `WWZ_PANEL_BACKUP_AT` | `backup.at` | `03:00` | Horário UTC (HH:MM) do snapshot diário |
| `WWZ_PANEL_BACKUP_KEEP` | `backup.keep` | `7` | Snapshots retidos |
| `WWZ_PANEL_RETENTION_DAYS` | `retention_days` | `7` | Retenção da tabela `events` |
| `WWZ_PANEL_VALKEY_ADDR` (senão `WWZ_VALKEY_ADDR`) | `valkey.addr` | vazio | Lista `host:porta` separada por vírgula; vazio = modo sem Valkey |
| `WWZ_PANEL_VALKEY_PASSWORD` (senão `WWZ_VALKEY_PASSWORD`) | — | vazio | Senha do Valkey |
| `WWZ_PANEL_VALKEY_PREFIX` | `valkey.prefix` | `wwz` | Mesmo valor de `store.prefix` do wwz |
| `WWZ_PANEL_VALKEY_CLUSTER` | `valkey.cluster` | `false` | Valkey Cluster (booleano) |
| `WWZ_PANEL_WWZ_ADMIN_URL` | `wwz.admin_url` | `http://127.0.0.1:9000` | Control plane do wwz (uma instância ou o balanceador do admin) |
| `WWZ_ADMIN_TOKEN` | — | vazio | Bearer do control plane do wwz (o mesmo do `wwz`) |
| `WWZ_PANEL_PROMETHEUS_URL` | `prometheus.url` | vazio | Base da HTTP API do Prometheus; vazio = gráficos por `/admin/stats` |
| `WWZ_PANEL_SESSION_KEY` | — | gerada | Chave HMAC do cookie (>= 32 bytes); vazia = gerada no primeiro boot e guardada em `settings` |
| `WWZ_PANEL_SESSION_TTL` | `session.ttl` | `12h` | Expiração deslizante da sessão |
| `WWZ_PANEL_ADMIN_USER` | — | `admin` | Usuário do bootstrap (ADR 0005) |
| `WWZ_PANEL_ADMIN_PASSWORD` | — | gerada | Senha do bootstrap; vazia = gerada e logada uma única vez. Ignorada quando já há usuários |
| `WWZ_PANEL_API_TOKEN` | — | vazio | Bearer de automação em `/api/v1` (age como `operator`, auditado como `api-token`); vazio desliga |
| `WWZ_PANEL_OIDC_CLIENT_SECRET` | — | vazio | Segredo do cliente OIDC (issuer e client id ficam no YAML) |
| `WWZ_PANEL_LOG_LEVEL` | `log_level` | `info` | `debug`, `info`, `warn`, `error` (slog JSON) |

Valor inválido (inteiro, booleano ou duração) aborta o boot com o nome da variável. Segredos (senhas, chave de sessão, tokens, segredo OIDC) nunca vão para o YAML nem para o log.

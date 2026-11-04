# Grafana

O `deploy/grafana/` traz um Grafana com o datasource Prometheus e o dashboard **White Wizard** já provisionados (spec 013, ADR 0007). A produção (host em `WWZ_HOST_GRAFANA` de `autor/prod.env`, fora do git, ADR 0076) usa o mesmo dashboard.

## Subir localmente

```sh
WWZ_ADMIN_TOKEN=x docker compose up -d          # wwz + Valkey + Prometheus + painel (cria a rede)
GRAFANA_ADMIN_PASSWORD=x docker compose -f deploy/grafana/docker-compose.yml up -d
# http://127.0.0.1:13000  (admin / GRAFANA_ADMIN_PASSWORD)
```

O Grafana entra na rede padrão do projeto raiz, `<pasta>_default`. Fora do checkout `white-wizard`, informe `GRAFANA_NETWORK=<projeto>_default`.

## Variáveis

| Variável | Padrão | Uso |
| --- | --- | --- |
| `GRAFANA_ADMIN_PASSWORD` | obrigatória | Senha do usuário admin |
| `GRAFANA_ADMIN_USER` | `admin` | Usuário admin |
| `GRAFANA_NETWORK` | `white-wizard_default` | Rede externa onde está o Prometheus |
| `GRAFANA_PROMETHEUS_URL` | `http://prometheus:9090` | URL do datasource |
| `GRAFANA_SCRAPE_INTERVAL` | `5s` | Intervalo mínimo das consultas (o mesmo do Prometheus) |
| `GRAFANA_BIND` / `GRAFANA_PORT` | `127.0.0.1` / `13000` | Porta publicada no host |
| `GRAFANA_ROOT_URL` | `http://127.0.0.1:13000` | URL pública (produção: `https://<WWZ_HOST_GRAFANA>`, por exemplo `https://grafana-wwz.example.com`) |
| `GRAFANA_COOKIE_SECURE` | `false` | `true` atrás de HTTPS |
| `GRAFANA_CPUS` / `GRAFANA_MEMORY` | `1` / `256m` | Limites do container |
| `GRAFANA_VERSION` | `12.2.0` | Tag da imagem `grafana/grafana` |

## Dashboard

`deploy/grafana/dashboards/wwz.json` (uid `wwz-overview`), filtrável por driver e modelo:

- visão geral: req/s, taxa de erro, streams abertos, requisições em voo, TTFT p99 e estado do Valkey;
- tráfego: req/s por driver, modelo, status e endpoint; tokens/s por tipo;
- latência simulada: TTFT, ITL e E2E p50/p99, TTFT p99 por modelo e overhead do engine;
- erros injetados por kind e fase, rate limited por limite, bloqueios e tool calls;
- Valkey e políticas: circuito do store, versão de `cfg:policies`, políticas recusadas, séries descartadas e passos de cenário;
- painel: fontes (`wwz_panel_source_up`), lag do `ev:stream`, eventos ingeridos e idade do último backup.

O dashboard é só leitura na UI: para alterá-lo, edite o JSON e rode `scripts/check-grafana.sh`, que valida o JSON, confere que cada métrica usada existe no código e roda `docker compose config` sem baixar a imagem.

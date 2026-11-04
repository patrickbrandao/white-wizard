# 019 — Exemplos, produção e execução autônoma

Status: rascunho
Depende de: 009, 015, 018

## Contexto
Configurações prontas para reproduzir situações de teste de roteadores, o deploy de produção no servidor cedido pelo humano, o loop autônomo que implementa o MVP e os arquivos de implementação do autor, que ficam fora do git.

## Fases
- **Fase 4** entrega só a infraestrutura de que esta spec depende: o passo `examples` do `verify-full` (pulado enquanto `examples/` não existir; [ADR 0037](decisions/0037-verify-full-e-heavy-concretos.md)), o `deploy/heavy/` com as rodadas de benchmark e o `deploy/grafana/` (painel e provisionamento) que a produção reaproveita, e o `wwz-load agents`/`top` (spec 015).
- **Fase 5** entrega os 16 exemplos, `deploy/prod/`, o deploy em `/opt/white-wizard` e o smoke externo.

## Requisitos
- RF-01: cada `examples/<nome>/` tem `config.yaml` (aceito por `wwz validate`), `docker-compose.yml`, `README.md` e `smoke.sh` e passa no passo `examples` do `verify-full` (spec 018, passo 12; [ADR 0016](decisions/0016-diretorio-examples.md)).
- RF-02: produção no ar em `/opt/white-wizard` conforme [ADR 0017](decisions/0017-deploy-producao.md), com smoke externo.
- RF-03: o loop autônomo roda até o critério de saída da última fase do [PLAN](PLAN.md) sem intervenção ([ADR 0012](decisions/0012-execucao-autonoma-noturna.md)); os arquivos dele são do autor, fora do git (seção "Arquivos de implementação do autor").
- RF-04: nenhum arquivo versionado identifica o servidor de produção do autor; os valores concretos ficam nos arquivos de implementação do autor ([ADR 0076](decisions/0076-dados-do-autor-fora-do-git.md)).

## Comportamento

### Exemplos
| Exemplo | Demonstra |
| --- | --- |
| `openai-perfeito` | Driver OpenAI sem erros, latência realista |
| `anthropic-perfeito` | Driver Anthropic sem erros, latência realista |
| `falhas-20pct` | `errors.rate: 20` com tipos variados |
| `latencia-intermitente` | Picos intermitentes por cauda pesada: `ttft`/`itl` lognormal com p99 muito acima da mediana (ex.: `ttft` mediana 300 ms, p99 8 s), sem período fixo ([ADR 0056](decisions/0056-latencia-intermitente-como-cauda-pesada.md)); o smoke confere mediana baixa e p99 alto numa amostra com `seed` fixo |
| `indisponibilidade-janelas` | `errors.outages` com janelas em segundos, minutos e horas ([009](009-erros-e-caos.md)) |
| `rate-limit-rpm` | `rpm` por chave com headers `x-ratelimit-*` |
| `cota-5h-semana` | Cota em janela de 5 h e semanal (estilo assinatura) |
| `creditos-openrouter-esgotando` | Créditos OpenRouter decrescendo até 402 |
| `ban-por-abuso` | Detecção de abuso e bloqueio de chave |
| `stream-corta-no-meio` | `stream_truncated`/`stream_reset` |
| `tool-calls-quebradas` | Argumentos de tool call inválidos (`toolargs`) |
| `agente-de-arquivos` | Cenário `file-agent-flow` |
| `raciocinio-longo` | Raciocínio extenso antes da resposta |
| `multi-provedor-failover` | Dois provedores, um instável, para medir failover; o compose monta os 6 drivers por prefixo (`/openai`, `/anthropic`, `/openrouter`, `/opencode`, `/gemini`, `/azure`, [ADR 0043](decisions/0043-drivers-gemini-e-azure.md)) e o smoke chama cada um pelo dialeto dele |
| `cluster-3-instancias-valkey` | 3 instâncias `wwz` com limites compartilhados no Valkey |
| `benchmark-vazao-maxima` | Latência zero, sem limites, para o teto de req/s |
| `opencode-completo` | Driver `opencode` com os catálogos públicos reais do opencode.ai (pay-as-you-go e Go, gratuitos), planos Go e budgets ([ADR 0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md), item 18); índice 16, porta 18116 |
| `claude-code-assinatura` | Driver `anthropic` com assinatura OAuth do Claude Code (`oauth_key`, `oauth_plan`, cota 5 h/semana nos `anthropic-ratelimit-unified-*`) e `client_verification: claude-code`: o smoke faz `wwz-load claude-code login` e `ask` (200), um `replay --send` de fixture (200) e uma chamada do SDK/curl comum no mount estrito (403 `forged_request_error`) ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), item 17; task 448); índice 17, porta 18117 |

O smoke de cada exemplo sobe o compose, envia requisições que mostram o comportamento (ex.: 20% ± margem de erros), verifica e derruba com `down -v`.

### Produção
- Compose em `deploy/prod/` (sem segredos e sem host padrão), instalado em `/opt/white-wizard` como projeto `wwz`. Os quatro nomes HTTPS (`WWZ_HOST_INFERENCIA`, `WWZ_HOST_ADMIN`, `WWZ_HOST_PANEL`, `WWZ_HOST_GRAFANA`), o acesso SSH e os IPs vêm de `autor/prod.env` (seção "Arquivos de implementação do autor").
- Roteamento HTTPS e portas diretas conforme a ADR 0017; smoke externo testa cada URL HTTPS e cada porta 8000–8008 do IP público (`WWZ_PROD_PUBLIC_IP`) com uma chave `wwz-key-*`.
- Porta 8008 ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), item 19): a instância `anthropic` (a mesma da 8004) em TCP puro (`mode tcp` no haproxy `wwz-ports`), só para `client_verification: claude-code` (mount `/claude-code`). No servidor do autor, portas do wwz podem dividir a porta com outro serviço do host em `mode http` (é o caso da 8004, no `autor/haproxy.cfg`), que tira o `Connection` e acrescenta `X-Forwarded-For`; uma porta assim não serve ao modo estrito (recusa `header_missing`/`header_order`) e continua válida para o modo `off`. O smoke externo do modo estrito usa a 8008. O `deploy/prod/haproxy/haproxy.cfg` versionado é o genérico do projeto: só as instâncias do wwz, 8000–8008 em `mode tcp`. Em servidor com NAT, as nove portas (8000–8008) precisam estar encaminhadas do IP público para o host (passo do humano no provedor).
- Gemini e Azure ([ADR 0043](decisions/0043-drivers-gemini-e-azure.md)) não ganham porta própria (as portas diretas continuam as da ADR 0017, mais a 8008 da ADR 0074): o nome HTTPS de inferência (`WWZ_HOST_INFERENCIA`) monta também `/gemini` e `/azure`, e o smoke externo chama os dois prefixos (`generateContent` com `x-goog-api-key`; chat por deployment com `api-key`).
- HTTPS pelo Traefik do servidor: labels `traefik.enable=true`, `traefik.docker.network=network_public`, um router `wwz-<papel>` por nome (entrypoints `web,websecure`, `Host(...)` do `WWZ_HOST_*`, `tls=true`, `tls.certresolver=letsencrypt`) e o serviço com a porta interna; o Traefik usa `exposedbydefault=false`, então só os containers com `traefik.enable` são publicados.
- Credenciais geradas no servidor e gravadas só em `/opt/white-wizard/CREDENTIALS.md` (chmod 600).
- Operação: `scripts/prod-deploy.sh` (build amd64, envio das imagens e `up`), `scripts/prod-deploy.sh down` (derruba sem apagar volumes; `down --purge` apaga), `scripts/prod-smoke.sh --ssh` (lê as chaves do `CREDENTIALS.md` pelo SSH e confere os quatro nomes HTTPS e as portas 8000–8008). Imagens `white-wizard{,-panel,-load}:prod-<sha>` carregadas direto no servidor; as antigas podem ser removidas com `docker rmi`, menos a em uso. Durante a rodada `streams`, o `make verify-heavy` para as instâncias de papel da produção e as religa no fim (`HEAVY_PROD_PAUSE=0` desliga, spec 018).

### Loop autônomo
- O watchdog (`autor/loop/watchdog.sh`) roda `claude -p` com o prompt da rodada (`autor/loop/loop-prompt.md`) em rodadas de até 3 h, dentro de `caffeinate`, com trava de instância única, espera até o reset quando o limite de uso é atingido, backoff 2/5/10/30 min em outros erros e no máximo 200 rodadas.
- Um agente do launchd (`autor/loop/com.whitewizard.loop.plist`, `KeepAlive`) reinicia o watchdog; logs em `tmp/loop/`.
- Estado entre rodadas em `autor/notas/PROGRESS.md`; pendências do humano em `autor/notas/MORNING.md`; término em `autor/notas/DONE.md`. As notas não são fonte da verdade: o que for vital (o não conferido no real, passos do humano, decisões) entra nas specs e ADRs na mesma rodada.

### Arquivos de implementação do autor
Arquivos que só valem para a instalação do autor (o servidor de produção dele e o loop que roda no Mac dele). Ficam em `autor/` na raiz do repositório, inteiro no `.gitignore` (`/autor/`), e **nada disso vai ao GitHub** ([ADR 0076](decisions/0076-dados-do-autor-fora-do-git.md)). Quem implanta em outro servidor cria os seus a partir do template.

| Arquivo | Para que serve |
| --- | --- |
| `autor/README.md` | Índice curto destes arquivos |
| `autor/prod.env` | Valores concretos do servidor, `KEY=VALUE` em sintaxe de shell (`source`): `WWZ_PROD_SSH` (`usuário@host` do SSH), `WWZ_PROD_SSH_PORT`, `WWZ_PROD_PUBLIC_IP` (IP público, onde ficam as portas 8000–8008), `WWZ_PROD_PRIVATE_IP` (IP do host atrás do NAT), `WWZ_HOST_INFERENCIA`, `WWZ_HOST_ADMIN`, `WWZ_HOST_PANEL`, `WWZ_HOST_GRAFANA` (os quatro nomes HTTPS do Traefik), `WWZ_HEAVY_SSH` (comando SSH completo do `verify-heavy`) e `WWZ_PROD_HAPROXY` (caminho do haproxy do servidor, relativo à raiz: `autor/haproxy.cfg`) |
| `autor/haproxy.cfg` | O haproxy real do container `wwz-ports` no servidor do autor: o genérico mais os serviços de terceiros que dividem portas com o wwz em `mode http` |
| `autor/notas/` | Notas do loop: `PROGRESS.md` (estado das rodadas), `MORNING.md` (pendências do humano), `DONE.md` (relatório de término), `STATUS.md` (estado do pacote original) e `PROMPT-LOOP.md` (prompt de montagem do loop) |
| `autor/loop/` | Watchdog, prompt de cada rodada, agente do launchd e README do loop |

- **Template versionado:** `deploy/prod/autor.env.example`, com as mesmas chaves de `autor/prod.env` e só valores de exemplo (`example.com`, IPs de documentação, porta SSH 22). Para outro servidor: `cp deploy/prod/autor.env.example autor/prod.env` e preencher.
- **Quem lê:** `scripts/prod-deploy.sh`, `scripts/prod-smoke.sh` e `scripts/heavy.sh` (e o `deploy/prod/test.sh`, quando precisa do host) carregam `${WWZ_AUTHOR_ENV:-<raiz>/autor/prod.env}` se o arquivo existir; uma variável já definida no ambiente vale mais que o arquivo. Faltando um valor (nem arquivo nem variável), o script falha com uma mensagem que cita `autor/prod.env` e `deploy/prod/autor.env.example`. Nenhum script tem host, IP ou porta SSH do autor como padrão.
- **Deploy:** o `prod-deploy.sh` leva os `WWZ_HOST_*` ao ambiente do compose no servidor (o compose de produção não tem host padrão) e sincroniza o haproxy de `WWZ_PROD_HAPROXY` quando o arquivo existe, senão o genérico `deploy/prod/haproxy/haproxy.cfg`, mantendo a proteção de inode e de edição manual.
- **Fora do git, também:** as credenciais da produção (`/opt/white-wizard/CREDENTIALS.md`, só no servidor) e as chaves reais de provedor (`~/.config/white-wizard/real-keys.env`, só no Mac).

## Interfaces e contratos
- `examples/*/config.yaml` segue o schema de [003](003-configuracao.md).

## Dados
- `tmp/loop/` (logs do loop), `tmp/real-spend.json` (gasto com provedores reais), `autor/` (arquivos de implementação do autor); todos fora do git.

## Critérios de aceite
- [ ] Os 18 exemplos sobem no Docker Desktop e passam no smoke dentro do `verify-full`.
- [ ] Smoke externo verde nos quatro nomes HTTPS de `autor/prod.env` (inferência inclusive com `/gemini` e `/azure`, admin, painel e Grafana) e nas portas 8000–8008 do IP público (o modo estrito `/claude-code` aceita o `wwz-load claude-code` na 8008).
- [ ] `git grep` do repositório não acha host, IP, porta SSH nem serviço de terceiros do servidor do autor; os scripts de produção e do heavy falham com mensagem clara sem `autor/prod.env`.
- [ ] `wwz-load agents` e `wwz-load top` rodam contra a produção.

## Fora de escopo
- Publicar os arquivos de implementação do autor ([ADR 0076](decisions/0076-dados-do-autor-fora-do-git.md)).

## Decisões relacionadas
- [0012](decisions/0012-execucao-autonoma-noturna.md), [0016](decisions/0016-diretorio-examples.md), [0017](decisions/0017-deploy-producao.md), [0018](decisions/0018-publicacao-local.md), [0056](decisions/0056-latencia-intermitente-como-cauda-pesada.md), [0072](decisions/0072-recursos-do-opencode-ai-no-driver-opencode.md), [0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md), [0075](decisions/0075-github-publico-e-imagens-tmsoftbrasil.md), [0076](decisions/0076-dados-do-autor-fora-do-git.md)

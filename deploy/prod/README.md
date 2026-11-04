# Produção (`deploy/prod/`)

Compose e templates da produção (ADR 0017, spec 019). Nada aqui tem segredo: chaves, token admin e senhas nascem no servidor e ficam só em `/opt/white-wizard/.env` e `/opt/white-wizard/CREDENTIALS.md` (chmod 600).

## Dados do servidor (ADR 0076)

Nada que identifique o servidor fica no repositório. Destino e porta ssh, IP público e privado, os quatro hosts HTTPS, o ssh do `verify-heavy` e o `haproxy.cfg` próprio do servidor são arquivos de implementação do autor, em `autor/` na raiz do repositório (pasta inteira no `.gitignore`):

| Arquivo | Conteúdo |
| --- | --- |
| `autor/prod.env` | `KEY=VALUE` em sintaxe de shell: `WWZ_PROD_SSH`, `WWZ_PROD_SSH_PORT`, `WWZ_PROD_PUBLIC_IP`, `WWZ_PROD_PRIVATE_IP`, `WWZ_HOST_INFERENCIA`, `WWZ_HOST_ADMIN`, `WWZ_HOST_PANEL`, `WWZ_HOST_GRAFANA`, `WWZ_HEAVY_SSH` e, opcional, `WWZ_PROD_HAPROXY` |
| `autor/haproxy.cfg` | O `haproxy.cfg` do servidor, quando o host tem outros serviços nas portas do wwz (apontado por `WWZ_PROD_HAPROXY=autor/haproxy.cfg`) |

O modelo versionado é [`autor.env.example`](autor.env.example) (mesmas chaves, valores de exemplo: `root@prod.example.com`, porta 22, `203.0.113.10`, `wwz.example.com`, `admin-wwz.example.com`, `panel-wwz.example.com`, `grafana-wwz.example.com`). `scripts/prod-deploy.sh`, `scripts/prod-smoke.sh`, `scripts/heavy.sh` e `deploy/prod/test.sh` carregam `${WWZ_AUTHOR_ENV:-<raiz>/autor/prod.env}` se o arquivo existir; uma variável já definida no ambiente vence o arquivo. Não há padrão no código: sem o valor, o script falha citando `autor/prod.env` e o modelo antes de qualquer ssh. `scripts/prod-deploy.sh show-env` mostra o que foi lido e o `haproxy.cfg` que o `sync` enviaria, sem conectar. Os exemplos abaixo usam os valores do modelo.

O compose não tem host padrão (`${WWZ_HOST_INFERENCIA:?…}` e os outros três): `credentials` e `up` gravam os `WWZ_HOST_*` de `autor/prod.env` no `/opt/white-wizard/.env` (cada um na linha que já tinha; só reescreve quando mudou), e os comandos de compose do `prod-deploy.sh` e do `heavy.sh` também os levam no ambiente, então `status` e `down` funcionam com um `.env` anterior a eles.

## O que sobe (projeto compose `wwz`)

| Serviço (container) | Papel | Exposição | CPUs | Memória |
| --- | --- | --- | --- | --- |
| `wwz-openai-perfeito` | OpenAI sem erros, latência realista | IP público `:8000` | 5 | 12 GiB |
| `wwz-falhas-20pct` | 20% de erros variados | `:8001` | 3 | 6 GiB |
| `wwz-latencia-intermitente` | Latência de cauda pesada | `:8002` | 3 | 6 GiB |
| `wwz-indisponibilidade` | Janela de 2 min a cada 10 min (UTC, minutos 0-1 de cada dezena) | `:8003` | 2 | 4 GiB |
| `wwz-anthropic` | Driver Anthropic com a assinatura OAuth do Claude Code na raiz e o modo estrito em `/claude-code` (ver "Claude Code") | `:8004` e `:8008` (modo estrito) | 3 | 6 GiB |
| `wwz-openrouter` | Driver OpenRouter | `:8005` | 2 | 4 GiB |
| `wwz-opencode` | Driver OpenCode Go | `:8006` | 2 | 4 GiB |
| `wwz-admin` | Principal: todos os drivers por prefixo (`/openai`, `/anthropic` com a assinatura OAuth, `/openrouter`, `/opencode`, `/gemini`, `/azure`) e control plane | HTTPS `WWZ_HOST_INFERENCIA` (dados), HTTPS `WWZ_HOST_ADMIN` e `:8007` (admin) | 6 | 12 GiB |
| `wwz-valkey` | Store e `ev:stream` do principal | só `wwz-internal` | 2 | 4 GiB |
| `wwz-prometheus` | Métricas (raspa as 8 instâncias e o painel) | só `wwz-internal` | 1 | 2 GiB |
| `wwz-grafana` | Dashboards | HTTPS `WWZ_HOST_GRAFANA` | 1 | 1 GiB |
| `wwz-panel` | Painel (`wwz-panel`) | HTTPS `WWZ_HOST_PANEL` | 1 | 2 GiB |
| **Total** | | | **32** | **63 GiB** |

Hosts HTTPS (Traefik, `entrypoints=web,websecure`, `certresolver=letsencrypt`, rede `network_public`): os quatro `WWZ_HOST_*` de `autor/prod.env` (no modelo, `wwz.example.com`, `admin-wwz.example.com`, `panel-wwz.example.com`, `grafana-wwz.example.com`). Routers `wwz-inferencia`, `wwz-admin`, `wwz-panel`, `wwz-grafana`. As instâncias por papel usam store `memory` (contadores isolados); só a principal usa Valkey e grava o `ev:stream` para o painel. O `deploy/prod/test.sh` confere a soma (≤ 32 núcleos e ≤ 64 GiB), as portas, os nomes `wwz*` e a ausência de `ports:` em Valkey e Prometheus.

`/gemini` (`generateContent` com `x-goog-api-key`) e `/azure` (deployment = id do modelo, `api-key`, `api-version`) estão montados em `config/todos.yaml` e no ar desde a tarefa 156. O `prod-deploy.sh render` (chamado por `up`) ainda pula do `run/config/todos.yaml` qualquer driver sem `internal/driver/<nome>` no commit em uso (`WWZ_PROD_SKIP_DRIVERS=a,b` força a lista; `none` não pula nada), e o smoke marca como PENDENTE uma rota que responda 404/405 (driver não montado).

## Rede e portas 8000-8008

O host só tem o IP privado (`WWZ_PROD_PRIVATE_IP`; o público, `WWZ_PROD_PUBLIC_IP`, é NAT externo); o Docker roda com `iptables:false` e `userland-proxy:false`, então `ports:` não publica nada e o compose não usa. O container `wwz-ports` (haproxy, `network_mode: host`, `haproxy/haproxy.cfg`) escuta 8000-8008 no host e repassa aos IPs fixos da rede `wwz-internal` (`198.18.128.0/25`: `.11` perfeito, `.12` falhas, `.13` latência, `.14` indisponibilidade, `.15` anthropic na 8004 e na 8008, `.16` openrouter, `.17` opencode, `.18` admin em `:9000`). Sem alterar firewall ou Docker. Se `prod-smoke.sh` não conectar em 8000, confira `wwz-ports` (`prod-deploy.sh status`).

O `haproxy/haproxy.cfg` do repositório é o genérico do projeto: só as instâncias do wwz, as portas 8000-8008 em `mode tcp`. Um servidor com outros serviços nas mesmas portas (por exemplo, backends que não são do wwz atendidos pelo Host em `mode http` na 8004 e na 8005) mantém o próprio cfg em `autor/haproxy.cfg`, fora do git, e o aponta em `WWZ_PROD_HAPROXY`; o `sync` envia esse no lugar do genérico. Esse cfg precisa manter os `listen` das portas do wwz nos mesmos IPs e a 8008 em TCP puro (o `test.sh` confere quando o arquivo existe). `WWZ_PROD_HAPROXY` definido com arquivo ausente é erro: o genérico sobrescreveria o cfg do servidor.

`sync` não manda o `haproxy.cfg` no tar. O `wwz-ports` monta esse **arquivo** (bind de arquivo único), e o tar troca o inode: o container seguiria com o conteúdo antigo. O arquivo vai para `run/haproxy.cfg.next` e o `_haproxy_apply` (no servidor) segue esta ordem: (1) igual ao do servidor: nada muda; (2) o do servidor difere do último sincronizado (sha256 em `run/haproxy.cfg.sha256`) e do repositório, ou seja, foi editado à mão: recusa, mostra o `diff -u` e para antes do tar, sem tocar nada; (3) `haproxy -c` num container avulso do serviço (`docker compose -p wwz run --rm --no-deps wwz-ports-check`), e uma recusa não grava; (4) grava no lugar (`cat >`, mesmo inode), guarda o sha256 e faz `docker compose -p wwz restart -t 5 wwz-ports`. O restart refaz o bind mount; depois o sync confere o sha256 lido de dentro do container e espera cada porta do cfg aceitar conexão. As portas 8000-8008 ficam fora por ~1-2 s e as conexões abertas (streams) caem, por isso o restart só acontece quando o arquivo mudou. Um restart que não terminou (`run/haproxy.cfg.reload-pending`) é refeito no sync seguinte. Edição à mão que deve ficar: leve-a ao cfg que o `sync` envia (o de `WWZ_PROD_HAPROXY` ou, sem ele, o `deploy/prod/haproxy/haproxy.cfg`), confira o arquivo do servidor e rode `WWZ_PROD_HAPROXY_ACCEPT=<sha256 do arquivo do servidor> scripts/prod-deploy.sh sync`, que só sobrescreve um arquivo com esse sha256.

Chaves de inferência: `wwz-key-<24 bytes base64url>` (3 chaves, `WWZ_KEY_1..3`, todas na conta `acct-prod`, válidas em todas as instâncias). A assinatura OAuth do Claude Code tem o segredo próprio `WWZ_OAUTH_KEY` (mesmo formato), só em `config/anthropic.yaml` e `config/todos.yaml` (ver "Claude Code"). Os `config/*.yaml` trazem `${WWZ_KEY_n}` e `${WWZ_OAUTH_KEY}`; `prod-deploy.sh render` grava as versões com os valores em `/opt/white-wizard/run/config/` (chmod 400, dono 65532) e o compose monta essas. O wwz não expande variáveis dentro do YAML, por isso o passo de render; cada placeholder só é exigido no `.env` pelas configs que o usam. `credentials` só acrescenta ao `.env` as variáveis que faltam (um `.env` anterior à `WWZ_OAUTH_KEY` ganha só ela; os outros valores ficam como estão) e regrava o `CREDENTIALS.md`.

Sem `admin.debug`: a produção não herda o `deploy/wwz/wwz.yaml` do compose de dev.

## Claude Code (ADR 0074, specs 006 16.2.1 e 020)

A instância anthropic (`config/anthropic.yaml`, portas 8004 e 8008) tem dois mounts do `anthropic`, os dois com `oauth_key: claude-code-oauth` e `oauth_plan: max_20x`:

| Base | Quem passa |
| --- | --- |
| `http://203.0.113.10:8004` | Chave (`x-api-key`) ou token OAuth `sk-ant-oat01-…` emitido pelo wwz |
| `http://203.0.113.10:8008/claude-code` | Só o Claude Code 2.1.308 (`client_verification: claude-code`); curl e SDKs recebem 403 `forged_request_error` com `x-wwz-client-verification: failed; reason=…` |

A 8008 é a mesma instância (`.15`) num `listen` TCP puro do `wwz-ports`: a linha e os headers chegam crus, como o modo estrito exige. A 8004 não serve para ele: num cfg próprio do servidor ela pode estar em `mode http` (outro serviço divide a porta pelo Host), e o haproxy em modo http tira `Connection`, acrescenta `X-Forwarded-For` e reescreve a cabeça, então `http://203.0.113.10:8004/claude-code` recusa até o Claude Code real (`header_order`/`header_missing`). Pelo mesmo motivo, atrás do Traefik ele recusaria tudo, e o `/anthropic` do HTTPS (`config/todos.yaml`) tem só a assinatura. Modelos que o Claude Code pede: `claude-sonnet-5-5`, `claude-opus-5-5` e `claude-haiku-5-5` (latência `gpt-4o-like`, texto sem tool calls, para o Claude Code real não executar ferramentas inventadas). A entrada `claude-code-oauth` de `keys[]` (`${WWZ_OAUTH_KEY}`, conta `acct-claude-max`, `models` restrito aos três `claude-*`) é o `oauth_key`; a conta dela tem `quota_usd` de US$ 50 em 5 h e US$ 250 na semana (escolha do wwz), base dos `anthropic-ratelimit-unified-*` e do 429 de cota. As `WWZ_KEY_1..3` ficam na `acct-prod`, sem essa cota. A cota zera com o reinício do container na 8004 (store `memory`) e com o do `wwz-valkey` na principal (Valkey sem persistência).

O valor da `WWZ_OAUTH_KEY` é o segredo dos tokens e fica só no servidor. O token vem do próprio wwz, nunca de uma conta real da Anthropic:

```sh
wwz-load claude-code login --manual --setup-token --target http://203.0.113.10:8004   # grava o token no --state (0600)
ANTHROPIC_BASE_URL=http://203.0.113.10:8008/claude-code \
CLAUDE_CODE_OAUTH_TOKEN="<access_token do --state>" claude -p "oi"
```

Não use credencial real (`~/.claude`, keychain, `sk-ant-…` da Anthropic) contra o wwz. O login tem consentimento automático: qualquer um que alcance a 8004, a 8008 ou o `/anthropic` consegue um token, e um token com `org:create_api_key` recebe em `POST /api/oauth/claude_cli/create_api_key` o `raw_key`, que é a própria `WWZ_OAUTH_KEY`. Por isso ela é a chave da assinatura e nada mais: vale só na instância anthropic (8004 e 8008) e na principal (as únicas configs que a declaram), só nos modelos `claude-*` (`wwz-fast` e os demais respondem 404) e debita a cota da `acct-claude-max`; nunca é uma `WWZ_KEY_n`, que continuam secretas. O token emitido vale nos dois mounts, nas duas portas e no `/anthropic` do HTTPS (o mesmo segredo).

## Como o humano roda (tarefa 208)

Pré-requisitos no Mac: `autor/prod.env` preenchido (copie de `deploy/prod/autor.env.example`), Docker Desktop com buildx (PATH com `/Applications/Docker.app/Contents/Resources/bin`), chave ssh para `WWZ_PROD_SSH` na porta `WWZ_PROD_SSH_PORT`, ≥ 4 GiB livres em disco. Nenhuma credencial do Docker Hub é configurada: as bases (`valkey`, `prometheus`, `grafana`) são puxadas anonimamente pelo servidor; se o pull falhar, use `push-bases`.

```sh
scripts/prod-deploy.sh show-env       # dados do autor lidos e haproxy.cfg do sync (não conecta)
scripts/prod-deploy.sh build          # imagens amd64 :prod-<commit> (wwz, wwz-panel, wwz-load)
scripts/prod-deploy.sh push-images    # docker save | gzip | ssh docker load + images.env
# scripts/prod-deploy.sh push-bases   # só se o servidor não puder puxar valkey/prometheus/grafana
scripts/prod-deploy.sh sync           # deploy/prod/ -> /opt/white-wizard (sem .env, images.env, run/); haproxy.cfg no lugar
scripts/prod-deploy.sh credentials    # WWZ_HOST_* no .env e segredos no servidor (idempotente; não imprime valores)
scripts/prod-deploy.sh up             # WWZ_HOST_* no .env, render + docker compose -p wwz up -d --wait
scripts/prod-deploy.sh status
scripts/prod-smoke.sh --ssh           # lê as chaves por ssh, só em memória
```

`scripts/prod-deploy.sh deploy` encadeia `build`, `push-images`, `sync`, `credentials` e `up`. Para ver as credenciais: `ssh -p "$WWZ_PROD_SSH_PORT" "$WWZ_PROD_SSH" cat /opt/white-wizard/CREDENTIALS.md` (valores de `autor/prod.env`) (o agente nunca as imprime nem as grava no repositório).

O smoke (`scripts/prod-smoke.sh`) chama `/openai`, `/anthropic`, `/openrouter`, `/opencode`, `/gemini` e `/azure` no `WWZ_HOST_INFERENCIA`, `/admin/config` (401 sem token, 200 com), o painel e o Grafana, e as portas 8000-8008 (8001 com 200 requisições, 10%-30% de erros; 8003 coerente com a janela UTC). Claude Code: login `--setup-token` e o `anthropic-ratelimit-unified-status` do assinante em `/anthropic` no HTTPS; na 8004, `GET /api/hello` 200, login, `ask` 200 na raiz e o assinante com `anthropic-ratelimit-unified-status`; na 8008, `HEAD /claude-code/api/hello` 200, `ask` 200 em `/claude-code` (`sdk-cli` e `cli`) e um curl comum em `/claude-code/v1/messages` com 403 e `x-wwz-client-verification: failed` (`--only-claude-code` roda só esse bloco; `SMOKE_CC_URL` troca a base da raiz e `SMOKE_CC_STRICT_URL` a do modo estrito). Usa o `wwz-load` de `WWZ_LOAD_BIN`, do `PATH` ou um `go build ./cmd/wwz-load` temporário; o token só passa pelo `--state` num diretório temporário 0700, apagado na saída. Falha com a lista de URLs que quebraram e imprime só nome, código e tempo.

## O que NÃO fazer

- Tocar o Traefik, `/opt/opencode`, outros containers, a configuração do Docker, firewall ou pacotes do sistema. Os comandos remotos de `prod-deploy.sh` passam por `guard_cmd`, que recusa caminhos fora de `/opt/white-wizard`, subcomandos docker fora de `load/compose/ps/images/inspect` e `docker compose` sem `-p wwz`.
- Versionar `.env`, `images.env`, `run/` ou `CREDENTIALS.md` (o `.gitignore` desta pasta os ignora; o teste procura segredos).
- `down --purge` sem querer: apaga os volumes `wwz-*` (Prometheus, Grafana, painel).
- Rodar `verify-heavy` com a produção sob carga: as rodadas do `deploy/heavy/` têm o próprio teto de 32 núcleos e 64 GiB (projeto `wwz-heavy`). O `scripts/heavy.sh` coordena isso sozinho: o passo `prod-pause` para as 7 instâncias de papel (8000-8006; mantém `wwz-admin`, Valkey, Prometheus, Grafana e painel) antes das rodadas e o cleanup (trap) as religa; `HEAVY_PROD_PAUSE=0` desliga e aceita medições contaminadas.

## Rollback

- Imagens ficam como `:prod-<commit>` no servidor. Para voltar: edite `/opt/white-wizard/images.env` com a tag anterior (ou rode `push-images` de um commit antigo depois de `build` nele) e `scripts/prod-deploy.sh up`.
- Configs: `git revert` + `sync` + `up` (o `render` regrava `run/config/`; o `sync` regrava o `haproxy.cfg` no lugar e reinicia o `wwz-ports`).
- Parar tudo sem apagar dados: `scripts/prod-deploy.sh down`. Apagar também os dados: `down --purge`.

## Make

`make prod-build`, `make prod-deploy` (build, envio, sync, credenciais e up), `make prod-smoke` (`--ssh`) e `make prod-test`; o último roda no `verify-quick` quando `deploy/prod/` ou `scripts/prod-*` mudam.

## Testes

`deploy/prod/test.sh` (sem acessar o servidor): `docker compose config` com `.env.example`, portas, nomes, soma de recursos, `wwz validate` de todas as configs (os seis drivers), os mounts do Claude Code (`oauth_key: claude-code-oauth`, nunca `prod-n`; `${WWZ_OAUTH_KEY}` só em `anthropic.yaml` e `todos.yaml`, com conta própria e só os `claude-*`; modo estrito só em `/claude-code`, os três modelos), os dados do autor (`autor/prod.env` com as chaves do modelo, quando existe; `autor.env.example` com valores de exemplo; `/autor/` no `.gitignore`; nenhum host, IP ou destino ssh de `autor/prod.env` em `deploy/prod/` e `scripts/`), o `haproxy.cfg` genérico (8000-8008 nos IPs dos papéis, só `listen` do wwz, tudo em TCP, `p8008` em `mode tcp`) e, se `WWZ_PROD_HAPROXY` existir, o do autor (os mesmos `listen` das portas do wwz e a 8008 em TCP), `haproxy -c` se houver `haproxy`, os hosts do compose (os do `.env.example`; sem `WWZ_HOST_*` o `config` falha), o `show-env`, o ambiente vencendo o arquivo, a falha sem dados do autor antes de qualquer ssh, o `sync` enviando o cfg de `WWZ_PROD_HAPROXY` ou o genérico,, o `_haproxy_apply` num diretório temporário (mesmo inode, restart só quando muda, recusa de edição à mão e de `haproxy -c`, `WWZ_PROD_HAPROXY_ACCEPT`, restart pendente refeito, tar sem o `haproxy.cfg`), o `prod-smoke.sh --only-claude-code` contra um wwz local com o `anthropic.yaml` renderizado (sem chave nem token na saída) e, no mesmo wwz, o `create_api_key` devolvendo a `WWZ_OAUTH_KEY` (nenhuma `WWZ_KEY_n`; 200 em `claude-sonnet-5-5`, recusada em `wwz-fast`), `credentials` num diretório temporário (arquivos 600, idempotente, sem imprimir valores, `WWZ_HOST_*` no `.env` e no `CREDENTIALS.md`, host novo trocando só a própria linha, e um `.env` antigo ganhando só a `WWZ_OAUTH_KEY`), a guarda dos comandos remotos e a busca por segredos. Usa `shellcheck` se existir.

# 0076 — Dados do autor fora do git

Status: aceita (decisão do humano em 2026-11-04)
Data: 2026-11-04
Atualiza: [0012](0012-execucao-autonoma-noturna.md) (arquivos do loop e notas), [0017](0017-deploy-producao.md) (dados do servidor), [0074](0074-assinatura-oauth-e-cliente-claude-code.md) item 19 (haproxy do servidor) e [0075](0075-github-publico-e-imagens-tmsoftbrasil.md) item 4 (histórico)

## Contexto
O repositório é público ([ADR 0075](0075-github-publico-e-imagens-tmsoftbrasil.md)) e citava o servidor de produção do autor: nome do host e subdomínios, IP público (NAT) e privado, porta SSH, aplicações de terceiros no mesmo host e as rotas do haproxy para elas. As notas do loop autônomo (estado das rodadas, pendências do humano, relatório de término) e os arquivos do loop também estavam no git, com hashes de commit, logs e dados do servidor. Em 2026-11-04 o humano pediu que tudo que identifica o servidor dele saia do repositório e que o GitHub seja recriado com um único commit: o passado é esquecido, e o que for vital precisa estar nas specs, a única fonte da verdade.

## Decisão
1. **Diretório `autor/`** na raiz do repositório, inteiro no `.gitignore` (`/autor/`). Guarda os **arquivos de implementação do autor**: o que só vale para a instalação do autor e não vai ao GitHub. O conteúdo, o papel de cada arquivo e o template estão na spec [019](../019-exemplos-e-producao.md#arquivos-de-implementação-do-autor):
   - `autor/prod.env` — host SSH, porta SSH, IP público e privado, os quatro nomes HTTPS e o comando SSH do heavy, em `KEY=VALUE` de shell;
   - `autor/haproxy.cfg` — o haproxy real do servidor (com os serviços de terceiros que dividem portas com o wwz);
   - `autor/notas/` — as notas do loop (`MORNING.md`, `DONE.md`, `PROGRESS.md`, `STATUS.md`, `PROMPT-LOOP.md`);
   - `autor/loop/` — o watchdog, o prompt de cada rodada e o agente do launchd (antes em `ops/loop/`);
   - `autor/README.md` — índice desses arquivos.
2. **Template versionado** `deploy/prod/autor.env.example` com as mesmas chaves e valores de exemplo (`example.com`, IPs de documentação). Quem implanta em outro servidor copia para `autor/prod.env` e preenche.
3. **Scripts sem padrão do autor.** `scripts/prod-deploy.sh`, `scripts/prod-smoke.sh` e `scripts/heavy.sh` carregam `${WWZ_AUTHOR_ENV:-autor/prod.env}` quando o arquivo existe; variáveis já definidas no ambiente têm precedência. Sem o valor (nem arquivo nem variável), o script falha citando `autor/prod.env` e o template. O compose de produção não tem host padrão. O `deploy/prod/haproxy/haproxy.cfg` versionado é o genérico do projeto (só o wwz, portas 8000–8008 em `mode tcp`); o deploy usa `WWZ_PROD_HAPROXY` (o `autor/haproxy.cfg`) quando existe.
4. **Notas não são fonte da verdade.** O que era vital nas notas foi levado às specs: o não conferido no real vive na spec [006](../006-fidelidade-provedores.md#não-conferidos-no-real) (seção "Não conferidos no real"), os passos do humano para a release na spec [018](../018-verificacao-ci-release.md#passos-do-humano), a produção na spec 019. Nenhum arquivo versionado cita `PROGRESS.md`, `MORNING.md`, `DONE.md`, `STATUS.md`, `PROMPT-LOOP.md` nem `ops/loop/` como fonte.
5. **Histórico recomeçado.** O GitHub é recriado com um único commit; o histórico antigo não é publicado. Specs, ADRs, docs e `CHANGELOG.md` não citam hashes de commit (nem para rastrear uma conferência ou uma imagem): citam a task, a data ou o conteúdo.
6. **Proibido no repositório** (arquivo versionado de qualquer tipo): o nome do host do autor e seus subdomínios, o IP público e o privado do servidor, a porta SSH, `root@<host>` e os serviços de terceiros do host (nomes, IPs e rotas). Continuam versionados, por serem do desenho do projeto: o namespace `docker.io/tmsoftbrasil` das imagens, a rede Docker `198.18.0.0/15` e os IPs `198.18.128.x` das instâncias, `/opt/white-wizard` e as portas 8000–8008.

## Alternativas consideradas
- Repositório privado: o humano manteve o público ([ADR 0075](0075-github-publico-e-imagens-tmsoftbrasil.md)).
- Valores do autor como padrão dos scripts: quem clona o repositório implantaria (ou rodaria o heavy) contra o servidor do autor por engano.
- Manter as notas do loop no git: misturam estado de execução, logs e dados do servidor com a especificação, e citam hashes de um histórico que não existe mais.

## Consequências
- Sem `autor/prod.env`, `make verify-heavy` (servidor remoto) e os scripts de produção falham com mensagem clara; o `verify-quick` e o `verify-full` não dependem dele.
- O loop autônomo, se religado, roda de `autor/loop/` e lê e escreve as notas em `autor/notas/`. As cópias foram movidas sem mudança e ainda citam os caminhos antigos (`ops/loop/`, notas na raiz): apontá-las para `autor/` é ajuste do autor antes de religar.
- Comportamentos novos não conferidos no real entram na seção "Não conferidos no real" da spec 006 na mesma tarefa que os implementa.
- Gatilho de revisão: o repositório voltar a ser privado ou o projeto ganhar um servidor de produção que não é do autor.

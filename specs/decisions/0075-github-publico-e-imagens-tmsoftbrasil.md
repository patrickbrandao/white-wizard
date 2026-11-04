# 0075 — Repositório público no GitHub e imagens no Docker Hub `tmsoftbrasil`

Status: aceita (decisão do humano em 2026-11-03)
Data: 2026-11-03
Atualiza: [0001](0001-decisoes-iniciais.md) (namespace das imagens) e [0018](0018-publicacao-local.md) (fim da publicação só local)
Atualizada por: [0076](0076-dados-do-autor-fora-do-git.md) (histórico recomeçado com um commit único)

## Contexto
O humano pediu, em 2026-11-03, para iniciar o projeto no GitHub em `patrickbrandao/white-wizard`. Na mesma conversa decidiu:
- repositório **público**;
- não levar ao GitHub os dois binários commitados por engano no histórico local (`wwz`, 35 MB, e `cmd/wwz-load/wwz-load`, 10 MB);
- imagens no Docker Hub na conta **`tmsoftbrasil`**, publicadas com o login dessa própria conta (o humano fez `docker login` como `tmsoftbrasil` em 2026-11-04; a conta `patrickbrandao` não tem escrita na organização: `insufficient_scope`);
- acesso ao GitHub por chave pública RSA (SSH).

## Decisão
1. Remoto `origin` = `git@github.com:patrickbrandao/white-wizard.git` (SSH), repositório público. O caminho dos módulos Go (`github.com/patrickbrandao/white-wizard`) e o label `org.opencontainers.image.source` não mudam.
2. O `release.yml` publica `docker.io/tmsoftbrasil/white-wizard`, `white-wizard-load` e `white-wizard-panel` (`NAMESPACE: tmsoftbrasil`). O login usa os secrets `DOCKERHUB_USERNAME` (`tmsoftbrasil`) e `DOCKERHUB_TOKEN` (token de acesso pessoal da conta `tmsoftbrasil`, Read & Write). Os secrets continuam cadastrados pelo humano, nunca pelo agente (spec 018). As primeiras imagens (`:latest` e um `:sha-<7>`, amd64 e arm64) foram publicadas em 2026-11-04 do Mac do humano com `docker buildx build --push`, com os mesmos target, build-args e labels do workflow, porque os secrets ainda não existiam; os repositórios no Docker Hub são públicos.
3. A partir do push, os workflows `quick.yml`, `release.yml` e `drift.yml` valem como descritos na spec 018. A tag `v0.1.0` continua sendo do humano.
4. O histórico do GitHub recomeça com um commit único ([ADR 0076](0076-dados-do-autor-fora-do-git.md)); o histórico antigo não é publicado nem citado. Specs, ADRs e docs não citam hashes de commit; as tags `prod-<sha>` das imagens de produção apontam para commits que não existem no repositório publicado e só servem para distinguir imagens no servidor.

## Alternativas consideradas
- Repositório privado (sugerido nas notas do loop): o humano escolheu público; os dados que identificam o servidor de produção saem do repositório pela [ADR 0076](0076-dados-do-autor-fora-do-git.md).
- Imagens em `docker.io/patrickbrandao`: o humano escolheu a organização `tmsoftbrasil`.

## Consequências
- O push na `main` sem `DOCKERHUB_TOKEN` faz o job de imagens falhar sem publicar nada; o `quick.yml` roda normalmente.
- Imagens já carregadas no servidor de produção (`white-wizard:prod-<sha>`) não mudam: o deploy de produção continua carregando as imagens direto no servidor.

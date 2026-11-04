# 0018 — Publicação só local

Status: aceita; substituída pela [ADR 0075](0075-github-publico-e-imagens-tmsoftbrasil.md) em 2026-11-03 (remoto público e push)
Data: 2026-10-30

## Contexto
[018](../018-verificacao-ci-release.md) e a [ADR 0010](0010-versionamento-releases.md) previam push para GitHub e Docker Hub.

## Decisão
- Durante o MVP: só git local e imagens carregadas direto no servidor de produção; sem push para GitHub ou Docker Hub.
- Os workflows `.github/workflows/quick.yml` e `release.yml` ficam prontos e testados localmente (sintaxe), para o humano ligar quando criar o remoto e os secrets.
- Tag `v0.1.0` continua sendo do humano (passos na spec [018](../018-verificacao-ci-release.md)).

## Alternativas consideradas
- Push automático (exige credenciais e é irreversível).

## Consequências
- A spec 018 instrui o humano sobre remoto, secrets e tag.

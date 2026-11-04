# 0010 — SemVer por tag

Status: aceita
Data: 2026-10-30

## Contexto
A seção 23 só publicava `:latest` e `:sha-<7>` a cada push na main.

## Decisão
Durante o MVP a publicação é só local ([0018](0018-publicacao-local.md)).

- Tag `vX.Y.Z` (primeira `v0.1.0`) publica `:X.Y.Z`, `:X.Y` e binários no GitHub Releases.
- Push na main continua publicando `:latest` e `:sha-<7>`.
- `CHANGELOG.md` (Keep a Changelog) move `[Não lançado]` para a versão na tag.
- Criar tag é ação do humano ou com aprovação dele.

## Alternativas consideradas
- Só latest e sha.

## Consequências
- Compatibilidade do YAML de configuração e da API admin segue SemVer: quebra exige versão maior (ou menor enquanto 0.x, registrada no CHANGELOG).

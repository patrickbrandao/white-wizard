# 0020 — Nomes de configuração do store

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-30

## Contexto
As specs herdadas do documento original falam em `valkey.addr`, `on_valkey_down` e `limits.local_first`. O YAML implementado em `internal/config` (fase 1) agrupa tudo em `store:` (`store.kind`, `store.addr`, `store.on_down`, `store.local_first`, `store.event_*`). Ainda não há release, então nenhum usuário depende dos nomes antigos.

## Decisão
- Os nomes do código são os oficiais: `store.kind` (`memory` | `valkey`, padrão `memory`), `store.addr` (lista), `store.password`, `store.cluster`, `store.read_timeout` (padrão 5 ms), `store.on_down` (`allow` | `deny` | `local`, padrão `local`), `store.prefix` (padrão `wwz`), `store.local_first`, `store.event_stream`, `store.event_max_len` (padrão 1 000 000), `store.event_sample` (padrão 1).
- Env: `WWZ_STORE` → `store.kind`; `WWZ_VALKEY_ADDR` (lista separada por vírgula) → `store.addr` e força `store.kind: valkey`; `WWZ_VALKEY_PASSWORD` → `store.password`.
- Specs 001, 003, 012 e 014 passam a usar esses nomes. Nesta versão `local_first` vale para o store inteiro, não por limite.

## Alternativas consideradas
- Renomear o código para os nomes do documento original: mudança sem ganho e com retrabalho em testes já verdes.

## Consequências
- O PLAN usa `store.on_down` e `store.local_first` na frente do store da fase 2 (corrigido na task 021).
- `local_first` por limite fica para quando houver demanda (ADR nova).

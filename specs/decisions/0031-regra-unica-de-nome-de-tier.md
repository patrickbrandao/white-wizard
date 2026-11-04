# 0031 — Regra única de nome de tier

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
O painel grava os tiers em `cfg:policies` (campo `tier:<nome>`, ADR 0025) e `internal/panel/source` (task 048) só aceita nomes `[A-Za-z0-9._-]{1,64}` em `ReplaceTiers`. O `wwz` não valida o nome dos tiers do YAML (`tiers.<nome>`) nem de `keys[].tier`. Fato novo: um tier do YAML com espaço, `:` ou acima de 64 bytes é aceito pelo `wwz` mas não pode ser gravado nem editado pelo painel, e `:` colide com o separador do campo `tier:<nome>` do hash.

## Decisão
- Um nome de tier é `[A-Za-z0-9._-]{1,64}` em todo o sistema: chaves de `tiers` no YAML, `keys[].tier`, `POST /admin/entities/{tipo}/{id}/tier` (que já exige tier existente) e `ReplaceTiers` do painel.
- A configuração com um nome fora da regra é inválida (boot abortado, recarga recusada), com a mensagem `config: tiers.<nome>: invalid tier name …` (ou `keys[i].tier`).
- A função de validação mora em `internal/config` e o painel a reutiliza, para as duas pontas não divergirem.

## Alternativas consideradas
- Aceitar qualquer nome no YAML e escapar no Valkey: complica o schema de chaves e o painel por um caso sem uso real.
- Deixar como está: tier do YAML que o painel não consegue gerenciar.

## Consequências
- Spec 003 atualizada. Mudança restritiva no formato do YAML (contrato público): nomes fora da regra, que nenhum exemplo nem perfil pronto usa, passam a ser recusados; registrado nas notas do autor (fora do git) para o humano confirmar.
- Nova tarefa: validar o nome em `internal/config` e trocar `validTierName` do painel pela função compartilhada.

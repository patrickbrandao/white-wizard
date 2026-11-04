# 0068 — Lote de specs da auditoria do DONE

Status: provisória (ADR 0012, loop autônomo de 2026-10-31; revisar se a task 381 ou a 388 pedir outro formato)
Data: 2026-10-31

## Contexto
A auditoria da task 362 (`tmp/done-audit/lacunas.md`) achou texto de spec defasado ou ausente. Esta ADR registra as decisões da task 392.

## Decisão
1. **`drivers[].options.require_version` do `anthropic`: só `true`.** O real sempre exige `anthropic-version`; `false` desligava a exigência, desvio que a ADR 0042 proíbe. Mesma regra da `require_session` (ADR 0059): padrão `true`, `false` torna a configuração inválida. Mudança incompatível do YAML (quem tinha `false` passa a ter erro de configuração), a registrar no CHANGELOG. Código na task 380.
2. **`generation.mode`** aceita `random`, `echo`, `lorem`, `dataset`, `template`, `json`; outro valor invalida a configuração. `mode: dataset` exige `dataset_path`. Código na task 381.
3. **`dataset` só com JSONL** (uma linha `{"prompt","response"}` por entrada). SQLite sai do MVP: nenhum critério o exige e o JSONL cobre o caso de uso (ShareGPT convertido). Reabrir se houver dataset grande demais para memória.
4. **Spec 004:** as recusas de campo desconhecido de `openai` e `gemini` passam a "conferido" (fixtures em `testdata/real/`); `azure` segue pela documentação (ADR 0043).
5. **Spec 005:** validação e opções de driver por interfaces opcionais descobertas no registro, sem mapa por nome no núcleo. Código na task 388.

## Alternativas
- Manter `require_version: false` com aviso: diverge do real (0042).
- Suportar SQLite no `dataset`: dependência e esforço sem critério que o peça.

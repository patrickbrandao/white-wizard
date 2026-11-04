# 0045 — Regras de agregação, ordenação e pausa do `wwz-load top`

Status: provisória
Data: 2026-10-31
Gatilho de revisão: o `/admin/stats` passar a informar números por chave só da instância (ou só do cluster) independentemente do store, ou um operador relatar que a soma/máximo engana na prática.

## Contexto
A [ADR 0036](0036-agents-e-top-do-wwz-load.md) diz que várias `--admin` "somam os contadores". Com `store: valkey`, o `by_key` de cada instância já traz os números do cluster (estado compartilhado), e somar contaria a mesma chave N vezes. A implementação (task 115) também precisou fixar o efeito de `--rows`, os critérios de `--sort` e a pausa.

## Decisão
- Linhas por chave de instâncias com `store: valkey` (campo `store` do `/admin/stats`) entram pelo máximo; com `store: memory`, somam. Totais e linhas por modelo continuam somados (contadores do processo). Ajusta a ADR 0036 só nesse ponto.
- `--rows` no JSON limita só `by_model` e `by_key`; no texto limita também os tipos de erro.
- `--sort`: `requests` = req/s (acumulado no 1º quadro), `errors` = % de erro, `ttft`/`e2e` = p99; empate por `driver` e `model`.
- Pausa não amostra; ao continuar, a taxa cobre todo o tempo desde a última amostra boa.

## Alternativas consideradas
- Somar sempre (texto literal da 0036): superconta chaves com store compartilhado.
- Pedir as chaves a uma só instância com valkey: perde a visão se essa instância cair.

## Consequências
- Spec 015, seção `wwz-load top`, ganha "Regras de agregação, ordenação e pausa".
- Código já conforme (`internal/loadgen/top`, task 115).

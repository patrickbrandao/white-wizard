# 0056 — Exemplo `latencia-intermitente` como cauda pesada, sem "burst de latência"

Status: provisória (revisar se um roteador-alvo precisar de picos com período fixo para testar circuit breaker por janela)
Data: 2026-10-31

## Contexto
A spec 019 descrevia o exemplo `latencia-intermitente` como "picos de latência periódicos (`burst` de latência)". Não existe `burst` em `profiles.latency` (spec 008): `burst: {every, duration, rate}` é de `errors` (spec 009). A task 207 já montou a porta 8002 da produção com uma lognormal de cauda longa (`ttft` mediana 300 ms, p99 8 s; `itl` mediana 15 ms, p99 400 ms).

## Decisão
- O exemplo e a porta de produção demonstram **cauda pesada**: latência sorteada de uma distribuição com p99 muito acima da mediana, que produz picos intermitentes sem período fixo. Nenhuma chave nova de configuração.
- Pico periódico de latência fica fora do MVP. Quem precisar de uma janela periódica de degradação usa `errors.burst` (falhas) ou um cenário com passos de `latency` (spec 011).

## Alternativas consideradas
- Criar `latency.burst: {every, duration, multiplier}`: feature nova (config, motor, validação, testes) para um caso que a cauda pesada e os cenários já cobrem.

## Consequências
- Spec 019: linha do exemplo reescrita. Nenhuma tarefa de código.

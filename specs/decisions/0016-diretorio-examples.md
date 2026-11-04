# 0016 — Diretório `examples/`

Status: aceita
Data: 2026-10-30

## Contexto
Usuários precisam de configurações prontas para reproduzir situações comuns de teste de roteadores.

## Decisão
- `examples/<nome>/` com `config.yaml`, `docker-compose.yml` e `README.md`, cada um testado no `verify-full` (sobe no Docker, roda um smoke que demonstra o comportamento, derruba com `down -v`).
- Exemplos: `openai-perfeito`, `anthropic-perfeito`, `falhas-20pct`, `latencia-intermitente`, `indisponibilidade-janelas`, `rate-limit-rpm`, `cota-5h-semana`, `creditos-openrouter-esgotando`, `ban-por-abuso`, `stream-corta-no-meio`, `tool-calls-quebradas`, `agente-de-arquivos`, `raciocinio-longo`, `multi-provedor-failover`, `cluster-3-instancias-valkey`, `benchmark-vazao-maxima`, `opencode-completo`, `claude-code-assinatura` (revisões abaixo).
- `indisponibilidade-janelas` exige janelas de indisponibilidade em segundos, minutos e horas, estilo cron; o schema `errors.outages` é especificado em [009](../009-erros-e-caos.md).
- Detalhes em [019](../019-exemplos-e-producao.md).

## Alternativas consideradas
- Só `scenarios/` (não cobre compose nem limites); exemplos só na documentação.

## Consequências
- `examples/` é área do implementador (config) e de infra-ci (compose); o testador escreve o smoke.

## Revisão (03/11/2026, task 437)
Fato novo: a [ADR 0072](0072-recursos-do-opencode-ai-no-driver-opencode.md), item 18, criou o `examples/opencode-completo/` (task 426: catálogos públicos reais do opencode.ai, planos Go, gratuitos e budgets). A lista passa de 16 para 17 exemplos; `opencode-completo` entra depois dos 16 originais (índice 16, porta 18116, como no `examples/README.md`) e também roda na rodada completa do passo `examples` do `verify-full`. O resto da decisão não muda.

## Revisão (03/11/2026, task 440)
Fato novo: a [ADR 0074](0074-assinatura-oauth-e-cliente-claude-code.md), item 17, cria o `examples/claude-code-assinatura/` (task 448: assinatura OAuth do Claude Code no driver `anthropic` e `client_verification: claude-code`). A lista passa de 17 para 18 exemplos; `claude-code-assinatura` entra depois do `opencode-completo` (índice 17, porta 18117) e roda na rodada completa do passo `examples` do `verify-full`. Até a task 448 fechar, o diretório não existe e o `EXAMPLES_ADR0016` do `scripts/verify.sh` continua com 17 nomes. O resto da decisão não muda.

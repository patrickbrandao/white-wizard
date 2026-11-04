# 0042 — Fidelidade total ao provedor real

Status: aceita
Data: 2026-10-31

## Contexto
Na revisão das ADRs provisórias (entrevista com o humano em 2026-10-31), várias decisões mantinham desvios deliberados do provedor real por conveniência: id curto aceito no OpenRouter ([0038](0038-slug-do-modelo-no-openrouter.md)), `server.strict` desligado por padrão ([0029](0029-escopo-do-server-strict.md)) e cache de prompt só sorteado ([0030](0030-cache-de-prompt-simulado.md)). O humano decidiu: nenhum provedor simulado pode se comportar diferente do real.

## Decisão
- **Princípio:** todo comportamento observável de um driver (aceitação e recusa de requisições, textos e envelopes de erro, headers, limites, cache, formas de resposta e de stream, ids de modelo) é idêntico ao do provedor real. Não há ajuste, atalho ou opção que faça o simulador divergir do real.
- **Conferência no real:** cada comportamento, inclusive os limites (429, cotas, `x-ratelimit-*`/`anthropic-ratelimit-*`, comportamento sob rajada), é conferido contra o provedor real com as chaves de `~/.config/white-wizard/real-keys.env` e vira fixture ou teste (ADR 0014). O que não puder ser conferido fica marcado como "não conferido" na spec 006, seção "Não conferidos no real", nunca é dado como pronto.
- **Orçamento:** o teto da ADR 0014 sobe de US$ 5 para **US$ 20 por provedor**, no mesmo `tmp/real-spend.json`.
- **Consequências nas ADRs anteriores:**
  - 0038: o driver `openrouter` recusa o id sem autor exatamente como o real (status, envelope e texto); testes, cenários, drivertest, SDK e `wwz-load` passam a usar o slug. *Corrigida pela [0052](0052-openrouter-aceita-id-sem-autor.md): conferido no real (task 163), o OpenRouter **aceita** o id sem autor; pelo mesmo princípio desta ADR, o driver também aceita.*
  - 0029: a validação de campos e schemas vale sempre, com o texto real; a opção `server.strict` é removida (a chave no YAML passa a ser recusada como desconhecida, mudança registrada no CHANGELOG).
  - 0030: o cache de prompt segue o real. Anthropic: `cache_control` cria entradas (TTL 5 min ou 1 h), cobra `cache_creation_input_tokens` e a leitura seguinte gera `cache_read_input_tokens`; OpenAI: cache automático de prefixo com o mínimo e a granularidade do real (`prompt_cache_key` respeitado); OpenRouter e OpenCode como os respectivos reais. Prefixos ficam no store com o TTL do provedor. `cache_hit_probability` deixa de existir.
- **O que continua permitido:** falhas, latência e limites configurados pelo operador (`errors`, `latency`, `limits`, cenários, headers `x-wwz-*` de opt-in) são o propósito do simulador; quando ativados, saem exatamente na forma em que o provedor real os produziria. Sem configuração, o driver é o provedor real.

## Alternativas consideradas
- Manter os desvios por conveniência de teste (recusado pelo humano).

## Consequências
- Tarefas 160–163 em `./tasks/`. Supera as partes divergentes de 0029, 0030 e 0038 e o orçamento de 0014 e 0035.

## Nota (03/11/2026, task 440)
A [ADR 0074](0074-assinatura-oauth-e-cliente-claude-code.md), item 4, abre uma exceção pedida pelo humano: `drivers[].options.client_verification: claude-code` no driver `anthropic` recusa (403 `forged_request_error`) clientes que não se comportam exatamente como o Claude Code. É opt-in por montagem; com o padrão `off` o driver continua igual ao real, e nenhuma outra opção pode divergir. O princípio desta ADR não muda.

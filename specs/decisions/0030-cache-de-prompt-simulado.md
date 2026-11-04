# 0030 — Cache de prompt simulado

Status: substituída pela 0042 (cache igual ao real) (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A spec 007 lista `cached_tokens` "com `cache_hit_probability`" no `usage`, e a spec 008 só o descrevia no prefill por token. A task 080 passou a informar os tokens em cache quando o acerto simulado acontece em `latency.model: per_token | polynomial` (o TTFT é recalculado só sobre os tokens fora do cache). Fatos novos levantados na 080:
- com TTFT fixo (`model: constant`, o padrão), um acerto reduz o TTFT em `cache_ttft_reduction` mas o `usage` informa 0 tokens em cache, então TTFT, `usage` e custo se contradizem;
- não há fonte de acerto vinda da requisição (`cache_control` da Anthropic, `prompt_cache_key`/`prompt_cache_retention` da OpenAI): só o sorteio gera tokens em cache.

## Decisão
- `cache_hit_probability` é um percentual 0–100, como as demais probabilidades do YAML; `cache_ttft_reduction` é a fração 0–1 do prompt em cache (padrão 0,7).
- Todo acerto informa `cached_tokens` = ⌊tokens de entrada × `cache_ttft_reduction`⌋, qualquer que seja o `latency.model`. Em `per_token`/`polynomial` o TTFT é `prefill_overhead` + (1 − redução) × tokens × `prefill_per_token`; em `constant` continua TTFT × (1 − redução). Os tokens em cache entram no custo com `cache_read_per_m` e saem no campo de cada dialeto (`prompt_tokens_details.cached_tokens`, `cache_read_input_tokens`, `opencode.tokens.cache_read`).
- O acerto é só simulado. `cache_control`, `prompt_cache_key` e afins são aceitos e não alteram o sorteio nem geram `cache_creation_input_tokens`/`cache_write_tokens` (ficam 0). Um cache dirigido pela requisição exigiria estado entre requisições (prefixos já vistos), contra o desenho stateless do data plane (spec 011, spec 014), e não muda o que o roteador sob teste exercita: ele recebe `usage` com cache e TTFT menor do mesmo jeito.
- Sem mínimo de 1 024 tokens nem múltiplos de 128 (regras da OpenAI): o operador controla a taxa de acerto pelo YAML.

## Alternativas consideradas
- Manter 0 em `constant`: `usage` e custo incoerentes com o TTFT menor.
- Derivar o acerto de `cache_control`/`prompt_cache_key` com um registro de prefixos no store: custo de estado e de Valkey no caminho quente sem ganho para o teste de roteadores; fica para depois do MVP, se o gatilho de revisão disparar.

## Consequências
- Spec 008 atualizada (seção "Cache de prompt simulado"); a spec 007 já pedia `cached_tokens` com `cache_hit_probability`.
- Nova tarefa: `internal/latency` informa `CachedTokens` também no acerto com TTFT fixo, com teste.

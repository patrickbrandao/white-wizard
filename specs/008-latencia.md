# 008 — Modelo de latência por etapa

Status: rascunho
Depende de: 003
Origem: seções 6 do documento Claude Docs congelado em 2026-10-30

## Contexto
Distribuições por etapa, fator de carga, chunking, pacing e relógio lógico (seção 6).

## Requisitos
- R8
- I5, I20, I21

## Interfaces e contratos
- `internal/latency`

## Dados
- Nenhum

## Critérios de aceite
- [ ] Overhead p99 próprio < 2 ms entre latência configurada e observada
- [ ] Um acerto de cache de prompt (spec 007) reduz o TTFT na proporção dos tokens lidos do cache, em todos os `latency.model`, e o `usage` informa os mesmos tokens
- [ ] `cache_hit_probability` não existe: o perfil que o traz é recusado como campo desconhecido

## Fora de escopo
- Simulação de GPU real

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0026-precedencia-chave-sobre-modelo.md](decisions/0026-precedencia-chave-sobre-modelo.md)
- [specs/decisions/0027-formas-finais-dos-recursos-importados.md](decisions/0027-formas-finais-dos-recursos-importados.md)
- [specs/decisions/0030-cache-de-prompt-simulado.md](decisions/0030-cache-de-prompt-simulado.md) (substituída pela 0042)
- [specs/decisions/0042-fidelidade-total-ao-provedor-real.md](decisions/0042-fidelidade-total-ao-provedor-real.md)
- [specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)

## Comportamento

### 6. Modelo de latência por etapa (R8)

A latência é composta por etapas independentes, cada uma com sua distribuição; o total de uma resposta é a soma delas, multiplicada por um fator de carga que cresce com a concorrência. Nada é um `sleep` fixo.

| Etapa | Chave | Quando acontece | Padrão |
| --- | --- | --- | --- |
| Fila de admissão | `queue` | Antes de qualquer byte; simula espera por slot de inferência quando `concurrency` do modelo está cheia | 0 ms |
| Prefill / TTFT | `ttft` | Entre o fim da fila e o primeiro chunk útil (`role: assistant` ou `message_start`) | 0 ms |
| Prefill por token | `prefill_per_token` | Alternativa a `ttft`: `prefill_overhead + tokens_de_entrada × prefill_per_token`; respeita `cached_tokens` | desligado |
| Inter-token do raciocínio | `reasoning_itl` | Entre chunks de `reasoning_content`/`thinking_delta` | = `itl` |
| Inter-token do conteúdo | `itl` | Entre chunks de texto | 0 ms |
| Emissão de tool call | `tool_call_delay` | Pausa antes do primeiro chunk de cada tool call | 0 ms |
| Inter-chunk de argumentos | `tool_args_itl` | Entre pedaços de `function.arguments`/`partial_json` | = `itl` |
| Fechamento | `finish_delay` | Entre o último token e o chunk final com `finish_reason`/`message_stop` + `[DONE]` | 0 ms |
| Keep-alive | `ping_interval` | Intervalo de `: ping` (OpenAI) ou evento `ping` (Anthropic) durante pausas longas | 15 s |
| Não-streaming | soma de tudo | A resposta inteira é retida pelo tempo total que o stream levaria | — |

**Distribuições.** Cada etapa aceita `fixed`, `uniform {min, max}`, `normal {mean, stddev}`, `lognormal {median, p99}`, `exponential {mean}` ou `pareto {scale, shape}` para caudas longas. Um valor escalar (`ttft: 300ms`) equivale a `fixed`. Valores negativos são truncados em 0; `stddev` limitado a 30 % da média para evitar absurdos.

**Fator de carga.** `load_factor: x` multiplica todas as etapas por `1 + (x − 1) × (ativos / concurrency)`; com `concurrency` cheio o tempo é `x` vezes maior. `concurrency` por modelo define também quando a fila começa e quando `max_queue` devolve 503 ou 429.

**Pacing pelo fim planejado ([ADR 0069](decisions/0069-pacing-pelo-fim-planejado-e-gogc.md)).** Num stream, a espera entre chunks termina em `fim planejado da espera anterior + d`, não em `fim da escrita + d`. Se o chunk atrasou menos que `d`, a espera seguinte é só o resto; se atrasou mais que `d`, o chunk sai já e o relógio recomeça do instante atual (sem rajada de recuperação na saturação). Espera cancelada descarta a marca. O ITL médio observado é exatamente o configurado; o atraso de agendamento de um chunk não se acumula nos seguintes nem sincroniza streams. Não há jitter artificial: `fixed` continua fixa.

**Chunking.** `chunk_tokens` (padrão 1) agrupa N tokens por evento SSE; `chunk_bytes` para pedaços de argumentos JSON. Permite simular provedores que enviam palavras inteiras ou blocos de 20 tokens.

**Por chave e por modelo.** Perfis de latência são resolvidos nesta ordem ([ADR 0026](decisions/0026-precedencia-chave-sobre-modelo.md), I3): header por requisição (`x-wwz-delay-ms`) > `latency` do passo de cenário > `keys[].latency` > `latency` do modelo > perfil `default`. Chave e modelo nomeiam perfis inteiros (sem mescla campo a campo); header e passo sobrescrevem campos do perfil resolvido. Uma chave com `latency` vê o mesmo perfil em todos os modelos (cada chave simula um provedor); sem `latency` na chave, cada modelo usa o seu.

**Pacing explícito (I5).** `pacing` no perfil substitui `itl`/`ttft` da parte de conteúdo, com durações Go como no resto do YAML ([ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)):

| Campo | Efeito |
| --- | --- |
| `tokens_per_second` | ITL = 1 s / valor, aplicado a conteúdo, raciocínio e argumentos |
| `jitter` | 0–1; ITL sorteado em ± `jitter` × ITL, uma vez por requisição |
| `inter_chunk_delays` | Lista `[10ms, 20ms, …]`: atraso antes do chunk *i*; além do fim da lista, repete o último |
| `total_duration` | Duração total do conteúdo, dividida igualmente entre os chunks; não-streaming retém `ttft` + `total_duration` + `finish_delay` |
| `chunk_runes` | Chunks de N caracteres em vez de N tokens |
| `subword_streaming` | Quebra cada chunk em pedaços de até 3 letras, com espaços e pontuação presos ao pedaço seguinte, como provedores com tokenização BPE; desligado quando `chunk_runes` está ativo |
| `dribble_chunks` + `dribble_total` | O conteúdo sai em exatamente `dribble_chunks` chunks (reagrupados depois do `subword_streaming`; com menos chunks que isso, sai como está), espaçados igualmente em `dribble_total`; os dois precisam ser > 0 |

**Relógio e modelo de carga.** `clock: real | logical` (I21: `logical` contabiliza a latência sem dormir) e `model: constant | per_token | polynomial` com `speedup_ratio` global (I20), conforme a spec 017. Com *ativas* = requisições em andamento na instância além da própria:
- `constant` (padrão): TTFT sorteado de `ttft`.
- `per_token`: TTFT = `prefill_overhead` + (tokens de entrada − tokens em cache) × `prefill_per_token`; sem nenhum dos dois campos, volta ao `ttft`.
- `polynomial`: o prefill do `per_token` × (1 + 0,05 × ativas) e, no decode, `itl`, `reasoning_itl` e `tool_args_itl` × (1 + 0,05 × ativas).
- `speedup_ratio` > 0 divide todas as etapas, junto com o `load_factor` e o `speed` do passo.

**Efeito do cache de prompt no TTFT ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md), [ADR 0046](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)).** Os tokens em cache vêm só das regras reais de cada provedor (spec 007); não há sorteio. Com *c* = tokens lidos do cache e *n* = tokens de entrada:
- `per_token`/`polynomial`: TTFT = `prefill_overhead` + (*n* − *c*) × `prefill_per_token` (o overhead não encolhe).
- `constant` (ou `per_token` sem campos de prefill): TTFT × (1 − `cache_ttft_reduction` × *c* / *n*). `cache_ttft_reduction` (fração 0–1, padrão 0,7) é parâmetro de latência do operador, como `ttft`: os provedores não publicam o ganho de latência do cache (a OpenAI diz só que reduz o tempo até a resposta, sem garantia), então ele não muda nenhum campo observável além do tempo.
- A escrita no cache não muda o TTFT.

Os tokens em cache saem em `usage` no campo de cada dialeto e entram no custo (spec 007).

**Perfis prontos** (valores de referência, ajustáveis): `fast` (tudo 0), `gpt-4o-like` (ttft 350 ms, itl 12 ms), `llama-70b-like` (ttft 600 ms, itl 25 ms), `deepseek-r1-like` (ttft 900 ms, reasoning\_itl 15 ms, itl 20 ms, raciocínio 500–3000 tokens), `slow-flaky` (lognormal com p99 de 8 s).

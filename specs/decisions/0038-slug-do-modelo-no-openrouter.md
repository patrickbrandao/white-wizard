# 0038 — Slug do modelo no driver `openrouter`

Status: aceita com a mudança da 0042, corrigida pela [0052](0052-openrouter-aceita-id-sem-autor.md) (o real aceita o id sem autor; o driver também) (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
O OpenRouter identifica modelos por slug `autor/modelo` (`openai/gpt-4o`) e devolve esse slug em `model` da resposta (spec 006, 16.4: em fallback de `models: [...]`, o modelo usado). Fato novo (revisão da task 129): o driver `openrouter` lista `white-wizard/<id>` em `GET /api/v1/models`, mas só aceita `<id>` no corpo (`config.FindModel` compara `id` e `aliases`), devolve `"model": "<id>"` e `GET /api/v1/models/white-wizard/<id>` dá 404. Um cliente que lista os modelos e usa o `id` listado recebe 404; um roteador que confere `model` da resposta contra o slug pedido vê divergência.

## Decisão
- **Slug de um modelo no driver `openrouter`:** `models[].id` quando ele já contém `/` (permite configurar slugs com aparência real, como `anthropic/claude-sonnet-4.5`); senão `white-wizard/<id>`. É o `id` e o `canonical_slug` de `GET /api/v1/models`.
- **Entrada:** o driver aceita o slug, o `id` e os `aliases`. A resolução tenta primeiro o valor exato (`id`/alias); sem acerto e com prefixo `white-wizard/`, tenta de novo sem o prefixo. Vale para o corpo (Chat Completions, Responses, Messages) e para `GET /api/v1/models/{slug}`. Aceitar o `id` curto é desvio deliberado (o OpenRouter real recusa id sem autor), mantido por compatibilidade com configs, testes e o `wwz-load`. *Nota (0052): a premissa "o real recusa id sem autor" era falsa; o real aceita o nome depois do autor, sem diferenciar maiúsculas, e devolve o slug completo. A regra vigente está na [0052](0052-openrouter-aceita-id-sem-autor.md).*
- **Saída:** `model` da resposta (não-stream, cada chunk SSE e o envelope Messages do dialeto Anthropic do OpenRouter) é sempre o slug, qualquer que seja a forma pedida.
- **Observabilidade:** labels de métricas, eventos do `ev:stream`, limites e cenários continuam usando `models[].id` (sem cardinalidade nova nem mudança de contrato do asyncapi).
- Os demais drivers não mudam: devolvem o `id` (ou alias) como hoje.
- **Raciocínio (task 129):** `reasoning_details[]` do driver sai com `type: "reasoning.text"`, `format: "unknown"` e `signature: null`; a forma exata é provisória até as fixtures reais da fase 5 confirmá-la (ou trocá-la pelo formato do provedor upstream, como `anthropic-claude-v1`).

## Alternativas consideradas
- Listar `<id>` sem prefixo: perde a forma `autor/modelo` que clientes do OpenRouter (e o próprio `wwz-drift`) esperam.
- Recusar o `id` curto como o real: quebraria `tests/sdk`, drivertest, cenários e o `wwz-load` sem ganho para testar roteadores.
- Ecoar a forma pedida: o real não faz isso (devolve o slug do modelo usado).

## Consequências
- Spec 006 (16.4, "Detalhes para o driver `openrouter`") atualizada.
- Tarefa de implementador: resolução do slug e `model` da resposta no driver `openrouter` e no engine; drivertest, `tests/sdk` e `wwz-load` ajustados ao slug na resposta.

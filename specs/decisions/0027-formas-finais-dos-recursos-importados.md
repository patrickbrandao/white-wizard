# 0027 — Formas finais dos recursos importados divergentes (I2, I5, I8, I10, I12, I13, I16, I22, I23, I24)

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A task 070 testou I1–I24 contra o código e a task 071 listou itens da spec 017 sem implementação ou com nome divergente. A ADR 0001 manda entregar o MVP inteiro (I1–I24) e a ADR 0014 exige fidelidade bit a bit. Para cada item foi escolhido entre implementar, ajustar a spec ao código ou adiar.

## Decisão
- **I2 — camada de headers do caos (implementar).** Headers por requisição, sujeitos a `server.request_headers`: `x-wwz-chaos-profile: <nome>` (troca o perfil de erros pelo de `profiles.errors.<nome>`), `x-wwz-chaos-rate: 0–100`, `x-wwz-chaos-types: a,b`, `x-wwz-chaos-mid-stream-rate: 0–100`, `x-wwz-chaos-mid-stream-types: a,b`. Valor inválido, perfil ou tipo desconhecido são ignorados, como nos demais `x-wwz-*`. Camadas, da mais fraca à mais forte: global → modelo → chave → passo de cenário → headers `x-wwz-chaos-*` → `POST /admin/chaos` (eventos ativos) → `POST /admin/chaos/next` (one-shot por `X-Test-Id`). `x-wwz-return-error` (I4) força um erro determinístico e dispensa o sorteio.
- **I5 — nomes do pacing (spec segue o código).** `latency.pacing` usa durações Go como o resto do YAML: `tokens_per_second`, `jitter`, `inter_chunk_delays: [10ms, 20ms]`, `total_duration`, `chunk_runes`, `subword_streaming`, `dribble_chunks` + `dribble_total`. Os nomes `*_ms` e `dribble: {chunks, total_ms}` da pesquisa não existem.
- **I8 — formato dos ids de tool call (implementar).** Campo de cenário `tool_call_id_format: encoded | native`, padrão `encoded`. `encoded`: `<prefixo>_<cenário>_<passo>_<n>_<nonce>`, com o prefixo do dialeto da rota (`toolu` nas rotas Messages, `call` nas demais); o decodificador aceita os dois prefixos. `native`: id nativo do driver (`call_` + 24 base62, `toolu_01…`), idêntico ao provedor real; nesse modo `expect.last_step` e a fixação do cenário pelo id não existem, e `wwz validate` acusa `last_step` como erro.
- **I10 — marcador (spec segue o código).** Forma canônica `[[wwz:nome seed=N speed=N]]`; `[[sim:…]]` e `[[llmsim:…]]` aceitos como apelidos. O marcador é removido da última mensagem do usuário antes da geração.
- **I12 — `semantic_match` (implementar, lexical).** `expect.semantic_match: {text, threshold}` (ou só o texto, com `threshold` 0,5): similaridade de cosseno entre vetores de frequência de palavras (minúsculas, sequências de letras e dígitos) da última mensagem do usuário e de `text`; casa com similaridade ≥ `threshold` (0 < t ≤ 1). Determinístico, sem modelo de embeddings nem dependência nova.
- **I13 — semântica de esgotamento (ajustar o código).** `repeat_last` significa, em `on_no_match` e em `on_exhausted`, o **último passo declarado no arquivo**. `on_exhausted` é consultado antes do casamento sempre que a conversa já tem `step_limit` turnos do assistente (padrão 50): `repeat_last` responde com o último passo, `error` devolve 400 `invalid_request` no formato do driver, `fallback` usa a geração do modelo. `on_no_match` vale abaixo do limite, quando nenhum passo casa. O "turno misto" (`turn.type: mixed`) é um passo com `content` e `tool_calls` juntos, sem campo próprio.
- **I16 — decomposição da entrada (implementar só como header).** Nova seção `usage: {extended: false}`; com `true`, toda resposta leva o header `x-wwz-input-breakdown` com JSON compacto `{"system", "tool_schemas", "user", "assistant", "tool_calls", "tool_results", "images", "cached", "total"}`. O corpo e o `usage` do provedor nunca mudam (ADR 0014). A exibição no painel usa um campo do evento `request` do `ev:stream`, especificado pelo guardião junto com as specs 012/016 (task 084).
- **I22 — `metrics.max_series` (spec segue o código).** Teto por métrica (cada métrica limita as próprias combinações de labels), padrão 5 000, lido só no boot: mudar exige restart. Série nova acima do teto é descartada e contada em `wwz_metrics_series_dropped_total`.
- **I23 — `--validate-on-load` (implementar).** Flag de `wwz serve` (env `WWZ_VALIDATE_ON_LOAD=1`). Sem a flag: configuração inválida aborta o boot e é recusada na recarga (a anterior continua); arquivo de cenário inválido é logado e pulado. Com a flag: o boot roda as mesmas verificações de `wwz validate` (configuração e todos os cenários) e sai com código 1 em qualquer erro; na recarga (SIGHUP, `POST /admin/config`) um cenário inválido recusa a recarga inteira e a configuração anterior continua.
- **I24 — drift (confirmado).** `cmd/wwz-drift` na fase 4 e `wwz-drift record` com fixtures na fase 5 (ADR 0014); a "fase 3" da spec 017 é corrigida.

## Alternativas consideradas
- I8 só com `call_` em todos os dialetos: mantém o código, mas um roteador que valida o prefixo `toolu_` na Anthropic falharia só no simulador.
- I12 com embeddings: exige modelo ou serviço externo, contra "sem GPU e sem custo".
- I16 no corpo (`usage.input_breakdown`): quebra a comparação byte a byte.
- I5 com nomes `*_ms`: dois estilos de duração no mesmo YAML.
- I13 mantendo `repeat_last` = passo que casou: `on_exhausted` padrão viraria um no-op e `step_limit` não protegeria contra loop.

## Consequências
- Specs 003, 008, 009, 011, 013 e 017 atualizadas.
- Tarefas 081 (cenário: I8, I12, I13), 082 (headers do caos), 083 (I16, header), 084 (guardião: I16 no `ev:stream` e no painel) e 085 (I23).

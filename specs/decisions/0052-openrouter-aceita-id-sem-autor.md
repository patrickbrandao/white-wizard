# 0052 — OpenRouter real aceita o id sem autor

Status: provisória (revisar quando uma fixture real mostrar outra regra de resolução, de empate entre autores ou de variante `:`)
Data: 2026-10-31

## Contexto
A [ADR 0042](0042-fidelidade-total-ao-provedor-real.md) (aceita pelo humano) fixa o princípio "todo comportamento observável de um driver é idêntico ao do provedor real" e, entre as consequências, escreve "0038: o driver `openrouter` recusa o id sem autor exatamente como o real". A premissa vinha de relatos públicos e da sonda `error_model` do primeiro drift, que usou um nome **inexistente** (`wwz-drift-modelo-inexistente`), não um id curto de modelo existente ([ADR 0046](0046-cache-de-prompt-real-e-detalhes-dos-drivers.md) já a marcava "não conferido").

Fato novo (task 163, 2026-10-31, 20 fixtures em `testdata/real/openrouter/models/`, comparadas byte a byte por `internal/driver/openrouter/models_fixture_test.go`):
- O real **aceita** o id sem autor: `gpt-4o-mini` e `GPT-4o-mini` → 200 com `model: "openai/gpt-4o-mini"`; `claude-haiku-4.5` → `anthropic/claude-haiku-4.5`. O slug completo também ignora a caixa (`openai/GPT-4o-mini` → `openai/gpt-4o-mini`).
- Nome parcial (`4o-mini`), desconhecido com ou sem autor (`nonexistent-wwz`, `openai/gpt-nonexistent-wwz`) e autor errado (`x/gpt-4o-mini`) → 400 `{"error":{"message":"<model> is not a valid model ID","code":400},"user_id":"user_…"}` em `/api/v1/chat/completions` (com e sem stream) e `/api/v1/completions`.
- `/api/v1/messages` → 400 `{"type":"error","error":{"type":"invalid_request_error","message":"<model> is not a valid model ID","error_type":"invalid_request"},"request_id":"gen-…"}`.
- `GET /api/v1/model/{author}/{slug}` ignora a caixa; modelo ausente ou autor errado → 404 `{"error":{"message":"Model not found: <author>/<slug>","code":404}}`; sem autor (`/api/v1/model/gpt-4o-mini`), com segmento a mais ou `GET /api/v1/models/{...}` → 404 `{"error":{"message":"Not Found","code":404}}`.

## Decisão
- **Corrige a consequência da 0042 sobre a 0038.** O princípio da 0042 prevalece: o driver `openrouter` faz o que o real faz, isto é, **aceita** o id sem autor. A decisão principal da 0042 não muda.
- **Resolução do `model` no corpo** (Chat Completions, Completions, Responses, Messages), sem diferenciar maiúsculas, só entre os modelos servidos no dialeto da rota ([ADR 0048](0048-dialetos-catalogos-e-templates.md)):
  1. igual ao slug de um `id` ou alias (`models[].id`/alias se contém `/`, senão `white-wizard/<nome>`, [ADR 0038](0038-slug-do-modelo-no-openrouter.md)/0046);
  2. sem `/` e igual à parte do slug depois do autor;
  3. senão, 400 com os corpos reais acima. Nome com autor diferente do slug nunca casa pela regra 2.
- `model` da resposta é sempre o slug completo do modelo resolvido, como o real.
- **Empate:** dois modelos (ou aliases) com o mesmo nome depois do autor → vence o primeiro na ordem de `models[]`; o slug completo (regra 1) vence qualquer casamento curto. "Não conferido": o real tem nomes únicos depois do autor nas fixtures gravadas, e a regra dele para colisões não foi observada.
- **Variante `:free`** (resolvida pela task 186 e conferida: ver [0060](0060-lote-de-specs-da-rodada-5-segundo.md) e spec 006, 16.4): o real responde 404 `This model is unavailable for free…` a `<slug>:free` de modelo sem variante gratuita (relatado pela task 163, sem fixture); o driver ainda não trata sufixos `:` (`:free`, `:nitro`…) e os resolve como nome desconhecido. Fica para tarefa própria, com fixture real.

## Alternativas consideradas
- Manter a recusa escrita na 0042: contraria o próprio princípio da 0042 diante do fato verificado.
- Escalar ao humano: a 0042 manda seguir o real; a correção só alinha a consequência ao real, sem mudar a decisão nem contrato incompatível (passa a aceitar o que antes era recusado).

## Consequências
- Spec 006 (16.4 e 16.7, critério de aceite) com a regra real. Implementação já feita na task 163 (`config.FindModelBySlug`).
- Notas "corrigida pela 0052" na 0042, na 0038 e na 0046; HISTORY.md atualizado.
- Tarefa nova para a variante `:free` (e demais sufixos) com fixture real.

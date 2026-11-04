# 007 — Geração de respostas

Status: rascunho
Depende de: 003, 004, 005, 012
Origem: seções 5 do documento Claude Docs congelado em 2026-10-30

## Contexto
Conteúdo, tamanho, tool calls, raciocínio, contagem de tokens e multimodal (seção 5).

## Requisitos
- R5, R6, R7, R20
- I18

## Interfaces e contratos
- `internal/generate`, `internal/toolargs`

## Dados
- Datasets de frases opcionais

## Critérios de aceite
- [ ] Argumentos de tool call validam contra o JSON Schema enviado
- [ ] Raciocínio vem antes do conteúdo no stream e entra em `usage`
- [ ] Driver `openai` com `reasoning_effort` não serializa `reasoning_content` nem `reasoning` por padrão, mas `usage.completion_tokens_details.reasoning_tokens` > 0 e o primeiro chunk visível atrasa pelo `reasoning_itl`
- [ ] OpenRouter com `reasoning.exclude: true` omite `reasoning`/`reasoning_details` e mantém `reasoning_tokens` e `cost`
- [ ] Gemini sem `includeThoughts` não devolve partes `thought` mas informa `thoughtsTokenCount`; com `includeThoughts: true`, as partes `thought: true` vêm antes do texto
- [ ] Cache de prompt: a mesma sequência de requisições produz no wwz e no provedor real os mesmos campos de cache (`cached_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `cache_creation.*`, `cachedContentTokenCount`, `cache_write_tokens`), conferida com fixture ou marcada "não conferido" (ADR 0042)
- [ ] Prompt abaixo do mínimo do provedor nunca gera tokens em cache; acerto só depois de uma requisição anterior com o mesmo prefixo, na mesma conta e dentro do TTL
- [ ] Com `store: valkey`, um prefixo gravado por uma instância é lido por outra
- [ ] Saída que alcança `max_tokens` termina com `finish_reason: length` / `stop_reason: max_tokens` / `MAX_TOKENS`; abaixo do teto, nunca
- [ ] `prompt_tokens` inclui o overhead do template (3 + role por mensagem, 3 por requisição); com `tiktoken:o200k_base` (padrão `auto` do `gpt-4o-mini`), a sonda do drift dá 13
- [ ] `models[].tokenizer: auto` mapeia `gpt-4`, `gpt-4-*` (`gpt-4-0613`, `gpt-4-32k`), `gpt-3.5-turbo*` e `text-embedding-*` para `cl100k_base`, e `gpt-4o*`/`gpt-4.1*` para `o200k_base`; só a sonda `prompt_tokens = 13` (`o200k_base`, OpenAI) está conferida no real, os demais tokenizadores e a Anthropic/Gemini ficam "não conferido" (ADR 0053/0058)
- [ ] `internal/tokenize` conta igual ao `tiktoken` de referência (`o200k_base`, `cl100k_base`) em todas as fixtures douradas, e `Split` nunca parte um caractere UTF-8 ([ADR 0053](decisions/0053-tokenizador-bpe-e-ordem-chave-corpo.md))
- [ ] Anthropic com a forma do Claude Code 2.1.308: `thinking` `adaptive` com `display` `omitted` e `updates`, `role: "system"` em `messages` e `context_management` produzem a resposta (corpo e stream) igual à fixture real gravada pela task 441 ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md))
- [ ] `model` da resposta = `models[].version` ou `models[].id` nos drivers `openai`, `azure` e `anthropic`, mesmo quando a chamada usa um alias

## Fora de escopo
- Respostas semanticamente corretas

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0014-fidelidade-bit-a-bit.md](decisions/0014-fidelidade-bit-a-bit.md)
- [specs/decisions/0022-raciocinio-fiel-ao-provedor.md](decisions/0022-raciocinio-fiel-ao-provedor.md)
- [specs/decisions/0029-escopo-do-server-strict.md](decisions/0029-escopo-do-server-strict.md) (validação sempre ligada pela 0042)
- [specs/decisions/0042-fidelidade-total-ao-provedor-real.md](decisions/0042-fidelidade-total-ao-provedor-real.md) (substitui a 0030)
- [specs/decisions/0043-drivers-gemini-e-azure.md](decisions/0043-drivers-gemini-e-azure.md)
- [specs/decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)
- [specs/decisions/0049-formas-reais-do-primeiro-drift.md](decisions/0049-formas-reais-do-primeiro-drift.md)
- [specs/decisions/0055-contrato-da-responses-api.md](decisions/0055-contrato-da-responses-api.md)
- [specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)

## Comportamento

### 5. Geração de respostas

Cada requisição passa por um pipeline fixo: resolver modelo → casar cenário → decidir tool calls → decidir raciocínio → gerar texto → aplicar latência → serializar no protocolo. Só o cenário (seção 9) altera a ordem.

**Modos de conteúdo**

| Modo | Comportamento | Uso |
| --- | --- | --- |
| `random` (padrão) | Concatena frases de uma lista embutida até atingir o alvo de tokens sorteado do histograma | Carga geral |
| `echo` | Devolve a última mensagem do usuário | Testar que o roteador não corrompe o corpo |
| `lorem` | Lorem ipsum com tamanho exato | Medir bytes/s |
| `dataset` | Busca resposta por hash do histórico em arquivo JSONL (`generation.dataset_path`, obrigatório com este modo; uma linha `{"prompt": ..., "response": ...}` por entrada; SQLite fica fora do MVP, [ADR 0068](decisions/0068-lote-de-specs-da-auditoria-do-done.md)); sem casamento, resposta aleatória do dataset | Respostas com cara real (ShareGPT convertido) |
| `template` | Texto Go `text/template` com acesso à requisição (`{{.Model}}`, `{{.LastUserMessage}}`, `{{rand.word}}`) | Cenários |
| `json` | Objeto gerado a partir do `response_format.json_schema`, válido contra o schema | Structured outputs |

**Tamanho da saída.** `max_tokens` (ou `max_completion_tokens`, `max_output_tokens`, `maxOutputTokens`) é o teto. Abaixo dele, o alvo vem de uma distribuição configurável: `histogram` (buckets com pesos), `uniform`, `normal` ou `fixed`. Quando o alvo sorteado alcança o teto, a saída é cortada exatamente no teto, e o motivo de término é o de truncamento do dialeto: `finish_reason: length` (OpenAI, Azure, OpenRouter, OpenCode), `stop_reason: max_tokens` (Anthropic) ou `finishReason: MAX_TOKENS` (Gemini). O texto pode parar no meio da palavra, como no real. A regra vale sempre que `completion_tokens` (raciocínio incluído, porque o teto real também o conta) for igual ao teto. Abaixo do teto, o término é `stop`/`end_turn`/`STOP`, ou o de tool call. O drift de 31/10/2026 mostrou o wwz respondendo `stop` com exatamente 16 tokens e o texto cortado, o que o real não faz ([ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)). No OpenRouter, `native_finish_reason` segue o vocabulário do upstream (spec 006, 16.7).

**Tool calls (R6)**

1. Se a requisição não traz `tools`, nada é chamado.
2. `tool_choice: none` → nunca chama. `required`/`any` → ao menos uma. Tool nomeada → exatamente aquela. `auto` → sorteio contra `tool_calls.probability`.
3. Quantidade: 1 chamada, depois repete um sorteio com `extra_call_probability` até falhar ou esgotar as tools (distribuição geométrica truncada). Com `parallel_tool_calls: false`, no máximo 1.
4. Argumentos: gerados do `parameters` de cada tool. Tipos cobertos: `string` (com `enum`, `format` date/date-time/uuid/email/uri, `minLength`/`maxLength`, `pattern` simples), `integer`/`number` (com `minimum`/`maximum`, `multipleOf`), `boolean`, `array` (com `minItems`/`maxItems`, `items`), `object` (recursivo, `required` sempre presente, opcionais por `optional_param_probability`), `null`, `oneOf`/`anyOf` (escolhe um ramo), `$ref` local. Schemas fora do subconjunto (`not`, `if/then`, `patternProperties`…) são tratados por melhor esforço: a API real os aceita, então o simulador não inventa erro ([ADR 0029](decisions/0029-escopo-do-server-strict.md)). Só as recusas reais geram 400, sempre ligadas (regras de `strict: true` da OpenAI e do Azure, spec 004).
5. Streaming OpenAI: um `delta.tool_calls[{index, id, type, function.name}]` inicial, depois `function.arguments` em pedaços de `tool_args_chunk_bytes` (padrão 24) com ITL; `finish_reason: tool_calls`. Streaming Anthropic: `content_block_start` com `tool_use` e `input: {}`, depois `input_json_delta` com `partial_json` em pedaços; `stop_reason: tool_use`.
6. Não-streaming: `tool_calls[]` completo (OpenAI) ou blocos `tool_use` com `input` já como objeto (Anthropic).
   Gemini: cada chamada é uma parte `functionCall {id, name, args}` (args como objeto, nunca em pedaços; no stream a parte chega inteira num chunk), `finishReason: STOP`; `functionCallingConfig.mode` `NONE` → nunca, `ANY` → ao menos uma, `allowedFunctionNames` restringe; `AUTO`/`VALIDATED` → sorteio.
7. Combinação texto + tool: `text_before_tool_probability` controla se um texto curto precede a chamada (ambos os provedores permitem).

**Raciocínio (R7)**

- Ativado por modelo (`capabilities.reasoning: true`) e, por requisição, por `reasoning_effort` (OpenAI, OpenCode), `reasoning: {effort, max_tokens, enabled}` (OpenRouter) ou `thinking: {type: enabled, budget_tokens}` (Anthropic). `budget_tokens`/`max_tokens` é o teto. `reasoning.enabled: false` desliga.
- `opencode` (conferido, task 175): o raciocínio é padrão nos modelos com `capabilities.reasoning` (DeepSeek, Qwen), mesmo sem `reasoning_effort`, `reasoning` ou `thinking` na requisição; `reasoning.enabled: false`/`thinking: {type: disabled}` desliga.
- Tokens de raciocínio: sorteio em `reasoning_tokens: {min, max, probability}`; `reasoning_effort` low/medium/high multiplica por 0,5 / 1 / 2.
- Formato fiel ao provedor real (ADR 0014, ADR 0022), escolhido por `reasoning_field` do driver (`drivers[].options.reasoning_field`; o nome antigo `opts` é recusado, [ADR 0048](decisions/0048-dialetos-catalogos-e-templates.md)). Sob a [ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md) ([ADR 0046](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)), o valor só pode ser um formato que o provedor real usa: `openai` e `azure` aceitam só `hidden`; `anthropic` e `gemini` têm formato fixo (blocos `thinking`, partes `thought`); os gateways `opencode` e `openrouter` aceitam os valores abaixo para imitar o formato do modelo upstream servido. O valor vem de `drivers[].options.reasoning_field` ou do perfil `generation` do modelo (`models[].generation.reasoning_field`, padrão `reasoning_content`); a regra acima vale para os dois, e `reasoning_content` num modelo `opencode` ou `openrouter` é válido ([ADR 0057](decisions/0057-schema-resp-e-ajustes-da-anthropic.md)). Outro valor torna a configuração inválida:
  - `hidden` (padrão do `openai`): o raciocínio é gerado e consome `reasoning_itl` em silêncio (nenhum chunk é emitido; o atraso entra no TTFT), mas o texto não é serializado em lugar nenhum. Só aparece `usage.completion_tokens_details.reasoning_tokens` (no último chunk com `stream_options.include_usage`, ou na resposta não-streaming), incluído em `completion_tokens` e no custo — como a Chat Completions real da OpenAI.
  - `reasoning_content` (padrão do `opencode`, formato de modelos upstream como DeepSeek): chunks com `delta.reasoning_content` antes do primeiro `delta.content`; não-streaming `message.reasoning_content`.
  - `reasoning` (padrão do `openrouter`): `delta.reasoning` / `message.reasoning`, mais `reasoning_details[]` (spec 006).
  - `inline_tag`: `<thinking>…</thinking>` no início do `content` (modelos upstream sem campo separado; só gateways).
- OpenRouter `reasoning.exclude: true`: o raciocínio é gerado, consome `reasoning_itl` e conta em `reasoning_tokens`, `completion_tokens` e `cost` (o provedor cobra), mas `reasoning`, `reasoning_details[]` e os deltas de raciocínio são omitidos — mesmo efeito de `hidden` para aquela requisição.
- Anthropic: bloco `thinking` em índice 0, com `signature: ""` no `content_block_start`, `thinking_delta` e `signature_delta` ao final; depois bloco `text` ou `tool_use`. `usage.output_tokens` inclui o raciocínio. `usage.output_tokens_details.thinking_tokens` informa a parte do raciocínio (só quando há raciocínio; ADR 0057). `type: redacted_thinking` com `redacted_probability`.
- Anthropic, forma do Claude Code ([ADR 0074](decisions/0074-assinatura-oauth-e-cliente-claude-code.md)): `thinking: {type: adaptive}` ativa o raciocínio nos modelos com `capabilities.reasoning` (o teto é o de `output_config.effort`, `low`/`medium`/`high` como os demais esforços), e `thinking.display` decide o que o bloco `thinking` carrega: `summarized` (padrão) o texto, `omitted` o bloco sem texto (só `signature`), `updates` a forma do beta `thinking-display-updates-2026-08-18`. Mensagens `role: "system"` em `messages` contam como `system` no tokenizador, no cache de prompt (prefixo na posição em que estão) e em `x-wwz-input-breakdown`; `output_config` por mensagem e `safeguards` não mudam a geração. `context_management` com `clear_thinking_20251015` não muda a geração; a resposta traz o campo `context_management` só se o real trouxer. Forma de `omitted`/`updates`, do teto do `adaptive` e de `context_management` na resposta: conferida no real pela task 441 (haiku, `max_tokens` mínimo), exceto o que a [spec 006](006-fidelidade-provedores.md#não-conferidos-no-real) lista em "Não conferidos no real" (`display: updates` com raciocínio longo, `applied_edits` não vazio).
- Modo `interleaved_thinking: true` (Anthropic) intercala raciocínio entre tool calls.
- Gemini: ativado por `thinkingConfig` (`thinkingBudget` é o teto; 0 desliga onde o real deixa; `thinkingLevel` MINIMAL/LOW/MEDIUM/HIGH multiplica como low/medium/high) ou pelo padrão do modelo; `usageMetadata.thoughtsTokenCount` sempre que houver raciocínio; o texto só sai com `includeThoughts: true`, em partes `thought: true` antes do texto; `thoughtSignature` na primeira `functionCall` ou na última parte (spec 006, 16.5).
- Azure: como o driver `openai` (`hidden`).

**Contagem de tokens ([ADR 0053](decisions/0053-tokenizador-bpe-e-ordem-chave-corpo.md)).** Um tokenizador por modelo (`models[].tokenizer`, spec 003; padrão `auto`): BPE real do `tiktoken` (`o200k_base`, `cl100k_base`) no pacote próprio `internal/tokenize` (ranks embutidos, sem rede e sem dependência nova) para os modelos OpenAI, e `simulated` (palavras e pontuação, ~2 µs/KB) para os modelos cujo provedor não publica tokenizador. Anthropic, Gemini, upstreams do OpenCode e autores não OpenAI do OpenRouter ficam "não conferido": medido com o `count_tokens` da Anthropic em 31/10/2026, o real deu 13, 296 e 36 (sonda, 1 KB em português, inglês com código), o simulado 12, 272 e 36 e o `o200k_base` 13, 260 e 32. O mesmo tokenizador conta o prompt (`CountPrompt`), a saída gerada, o raciocínio, o `count_tokens`, os blocos do cache de prompt e os `{{ tokens }}` dos cenários; `Split` (streaming) agrupa tokens até formar UTF-8 válido. `prompt_tokens`, `completion_tokens` e os campos de cache abaixo, no lugar de cada dialeto.

**Overhead do template de chat ([ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)).** O `prompt_tokens` do real inclui os tokens do template de chat, além do texto. A regra é a documentada pela OpenAI para o Chat Completions:
- cada mensagem (inclusive `system`/`developer` e o `system` de topo da Anthropic) custa `3 + tokens(role) + tokens(conteúdo)`, mais 1 quando tem `name`;
- a requisição soma mais 3 tokens, de abertura da resposta do assistente;
- imagens mantêm os custos próprios de `CountPrompt`;
- tools, `json_schema`, `tool_choice` e o histórico de chamadas seguem a regra derivada de 110 chamadas reais ao `gpt-4o-mini` (`testdata/real/openai/tools/`, task 252, `internal/generate/tools.go`; todas exatas, comparadas nas fixtures da Responses sem exclusão). As funções são renderizadas no namespace TypeScript que o modelo lê (`namespace functions { … } // namespace functions`; objeto em uma linha, com linhas de comentário quando uma propriedade tem `description` ou `default`) e custam `tokens(texto) + 3`; o `json_schema` vira `# Response Formats` + nome + descrição + schema em JSON compacto sem `required` nem `additionalProperties`, e custa `tokens(texto)` + `tokens(name)` no Chat ou + 1 na Responses. Chat Completions com tools ou `json_schema` abre uma seção de instruções de sistema de 6 tokens (2 quando a requisição já tem `system`/`developer`); a Responses não tem essa seção. `tool_choice` nomeando uma função custa `7 + tokens(nome)` (na Responses, `required` com uma só tool custa o mesmo), `none` custa 1 e `required`/`auto` nada; `parallel_tool_calls: false` custa 3 na Responses e nada no Chat. Uma chamada anterior e seu resultado custam `8 + resultado(5 no Chat, 3 na Responses) + 2 × tokens(nome) + tokens(argumentos compactos) + tokens(saída)` (13 e 11 de quadro); no Chat, um turno com várias chamadas vira uma chamada `multi_tool_use` (20 de quadro, mais 2 por resultado). Medido só na OpenAI; nos demais drivers a mesma regra vale e fica "não conferido" até haver fixture própria.

Com `o200k_base`, a sonda do drift (uma mensagem `user` com `Responda só: ok`) dá os 13 tokens do real; o wwz contava 9, sem os 3 da requisição e com o tokenizador simulado. Situação por driver:
- `openai`, `azure` e `openrouter` com autor `openai/`: regra documentada.
- `anthropic` e os demais autores do OpenRouter: a mesma regra. O real deu 13 na mesma sonda, mas as constantes não são publicadas (não conferido até uma fixture com várias mensagens e `system`).
- `opencode`: a mesma regra. O real deu 35 com `deepseek-v4.1-flash`, porque o template upstream é maior (não conferido).
- `anthropic`: template próprio por geração do Claude ([ADR 0063](decisions/0063-lote-da-rodada-6-b-cachedcontents-tools-no-cache-e-endpoint.md), task 264), medido com o `count_tokens` (grátis) e guardado em `internal/generate/template_anthropic.go` (a regra comum acima vale só como ponto de partida):

  | Geração | Cabeçalho por turno | Resposta | Cabeçalho do system | Tools (auto) | Texto das tools |
  | --- | --- | --- | --- | --- | --- |
  | 4.5 e anteriores | 3 | 4 | 0 | 489 | ASCII (escapa não ASCII) |
  | 4.6 | 3 | 4 | 1 | 489 | UTF-8 |
  | 4.7 | 5 | 6 | 1 | 665 | UTF-8; texto ×1,32, tools ×1,26 (contagem simulada) |
  | 4.8 e 5.x | 2 | 4 | 1 | 280 (Opus 276, Sonnet 344) | UTF-8; texto ×1,32, tools ×1,26 |
  | 5.5 | 2 | 6 | 1 | 276 | UTF-8; texto ×1,32, tools ×1,26 |

  Mensagens seguidas do mesmo papel dividem o cabeçalho (união +1); o prefill final do `assistant` não tem cabeçalho; `tool_choice` `any`/`tool` somam à constante das tools; o histórico de `tool_use`/`tool_result` tem custo próprio por chamada, chave do `input` e resultado vazio ou com erro. Constantes são tokens reais de 31/10/2026, exatas quando a contagem do texto é; a 4.7 em diante conta mais tokens que o simulado, que aproxima a 4.5: o `Simulated` é multiplicado por 1,32 no texto e por 1,26 no texto das tools (calibração de 12 textos de `testdata/real/anthropic/tokenizer/`; igual de 4.7 a 5.5; erro médio 26,6% → 15,7%, task 301, [ADR 0065](decisions/0065-lote-da-rodada-6-d-tokenizador-claude-tool-choice-e-usage.md)). A escala do 4.7 em diante vale também na **saída** (`output_tokens`, o corte por `max_tokens` e o stream) e nos pontos de cache; os **gateways** (OpenRouter, OpenCode) não aplicam escala, nem na entrada nem na saída. Não conferido: a escala de saída medida (task 342, ADR 0066). Uma conversa que **começa por `assistant`** custa a mais 12 (4.5/4.6), 24 (4.7) ou 21 (4.8 e 5.x), com ou sem `system` e qualquer número de turnos depois. Blocos de texto seguidos da mesma mensagem são unidos sem `\n`.
- `gemini`: cada `Content` custa 1 + suas partes; `systemInstruction` só o texto; sem priming (o `promptTokenCount` da geração é o do `countTokens`); `toolConfig` e `responseSchema` não contam; imagem 1 089 tokens; áudio ~32/s + 1; declarações e chamadas de função no formato do Gemma 4 (spec 006, 16.5). `tokenizer: auto` usa o `gtok` calibrado (erro médio 4,9% em 63 textos); o tokenizador real segue "não conferido" ([ADR 0063](decisions/0063-lote-da-rodada-6-b-cachedcontents-tools-no-cache-e-endpoint.md)).

A igualdade exata depende do tokenizador. Com `simulated` o total é aproximado; `tiktoken:o200k_base` (o `auto` dos modelos OpenAI atuais) reproduz a OpenAI.

**`model` da resposta ([ADR 0049](decisions/0049-formas-reais-do-primeiro-drift.md)).** O real devolve o snapshot resolvido, não o nome pedido: `gpt-4o-mini` → `gpt-4o-mini-2024-07-18`; `claude-haiku-4-5` → `claude-haiku-4-5-20251001`. Nos drivers `openai`, `azure` e `anthropic`, `model` (não-stream, cada chunk, `message_start`) é `models[].version` quando definido e, senão, `models[].id`, nunca o alias usado na chamada. Nos drivers `openrouter` e `opencode`, `model` é o slug ou o `id` de `models[]`, como o real (spec 006, 16.7). Gemini: `modelVersion` = `id`. Métricas e eventos continuam com `models[].id` ([ADR 0040](decisions/0040-label-de-modelo-e-get-de-modelo-openrouter.md)).

**Cache de prompt ([ADR 0042](decisions/0042-fidelidade-total-ao-provedor-real.md), [ADR 0046](decisions/0046-cache-de-prompt-real-e-detalhes-dos-drivers.md)).** O cache segue as regras do provedor real; não há sorteio nem taxa de acerto configurável (`cache_hit_probability` não existe mais). Um acerto só acontece quando uma requisição anterior da **mesma conta** (a entidade de limite do driver: org, workspace, projeto ou recurso) gravou o mesmo prefixo e a entrada ainda não expirou. Os prefixos ficam no store (`memory` ou `valkey`, compartilhado entre instâncias) como hashes encadeados do prompt tokenizado, com o TTL do provedor renovado a cada leitura; o conteúdo nunca é guardado (chaves, hash, pontos de corte e limites em [cache/valkey/README.md](../cache/valkey/README.md), seção 2.4). A entrada só passa a valer quando a resposta que a criou começa (primeiro byte), como no real. Regras por driver (padrões da geração atual de modelos; `models[].prompt_cache` troca `min_tokens`, `granularity` e `kind` para imitar outro modelo real, spec 003):

| Driver | Como o prefixo entra no cache | Mínimo e granularidade | TTL | Campos no `usage` |
| --- | --- | --- | --- | --- |
| `openai`, `azure` | Automático: todo prompt grava seu prefixo; o acerto é o maior prefixo já gravado; `prompt_cache_key` entra no hash de roteamento (sem ele, só o prefixo) | 1 024 tokens e passos de 128 (conferido no real em 31/10/2026); fronteira exata do gpt-5.6 em diante (não conferido; padrão do wwz: passos de 128) | `prompt_cache_retention: in_memory` 5–10 min sem uso, no máx. 1 h (wwz: 10 min renovados, teto 1 h); `prompt_cache_retention: "24h"` até 24 h (wwz: 24 h com teto de 24 h desde a criação); `prompt_cache_options.ttl` `30m`/`12h` (gpt-5.6+, `12h` só Responses) | Chat: `prompt_tokens_details.cached_tokens`; Responses: `input_tokens_details.{cached_tokens, cache_write_tokens}`, `cache_write_tokens_details.{ttl_30m_tokens, ttl_12h_tokens}` (gpt-5.6+) |
| `anthropic` | Explícito: `cache_control: {type: "ephemeral", ttl: "5m"\|"1h"}` em blocos de `tools`, `system` ou `messages` (até 4 breakpoints), ou no topo do corpo (automático no último bloco elegível, ocupa 1 dos 4); ordem tools → system → messages; leitura procura entrada em até 20 blocos antes de cada breakpoint; entradas de 1 h antes das de 5 min | Padrão do wwz 512 tokens (Opus/Sonnet 5.5, Fable/Mythos 5.x); **`claude-haiku-4-5`: 4 096 (conferido no real em 31/10/2026, task 162)**; 1 024, 2 048 ou 4 096 nos modelos anteriores (spec 006). O padrão é por driver, não por modelo: para imitar o Haiku 4.5 ou um modelo antigo, o operador define `models[].prompt_cache.min_tokens`. Abaixo do mínimo, segue sem cache e sem erro | 5 min ou 1 h, renovado a cada leitura | `cache_creation_input_tokens`, `cache_read_input_tokens`, `cache_creation.{ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}`; `input_tokens` = só o que vem depois do último breakpoint |
| `gemini` | Implícito automático (como o da OpenAI) e explícito por `cachedContents` + `cachedContent` na requisição | 4 096 tokens nos modelos atuais, 2 048 no 2.5 | Implícito: não documentado (não conferido; wwz: 5 min); explícito: `ttl`/`expireTime`, padrão 1 h | `usageMetadata.cachedContentTokenCount` (incluído em `promptTokenCount`), `cacheTokensDetails` |
| `openrouter` | As regras do provedor upstream do modelo: `openai/*` como `openai`, `anthropic/*` como `anthropic` (inclusive `cache_control` no topo), `google/*` como `gemini` implícito; outros autores, `models[].prompt_cache` (padrão `none`) | do upstream | do upstream | `prompt_tokens_details.cached_tokens`, `cache_write_tokens`, `cache_discount` (posição exata não conferida); `cost` com o preço de leitura e escrita |
| `opencode` | As regras da API nativa do modelo (Messages → `anthropic`, Chat/Responses → `openai`, Gemini → `gemini`); afinidade por `x-opencode-session` (regras de TTL e mínimo não documentadas: não conferido) | da API nativa | da API nativa | o campo do dialeto + `usage.opencode.tokens.{cache_read, cache_write_5m, cache_write_1h}` (`input` exclui leitura e escrita) |

**Estado da conferência (task 200).** Conferido no real: mínimo de 1 024 e passos de 128 da OpenAI; mínimo de 4 096 do `claude-haiku-4-5`; `prompt_cache_key` distinto = miss. Não conferido: expiração real dos TTLs (OpenAI 5–10 min e 24 h, Anthropic 5 min e 1 h, Gemini implícito), lookback de 20 blocos, limite de 4 breakpoints, `cache_control` no topo do corpo, ordem dos blocos na OpenAI, e todo o cache de OpenRouter, OpenCode, Gemini e Azure (regras herdadas do upstream). Cada item segue "não conferido" (lista na [spec 006](006-fidelidade-provedores.md#não-conferidos-no-real), seção "Não conferidos no real") até uma fixture.

**Ordem dos blocos no hash ([ADR 0063](decisions/0063-lote-da-rodada-6-b-cachedcontents-tools-no-cache-e-endpoint.md), task 246).** No real da OpenAI as tools ficam no cabeçalho do prompt, antes das mensagens, e mudar as tools invalida o cache inteiro. O hash do cache de `openai`, `azure` e gateways no dialeto `openai` põe o bloco de tools **primeiro**, depois system e mensagens; a Anthropic usa tools → system → messages; o Gemini segue o template do `gtok`. A contagem de `prompt_tokens` não depende da ordem. O custo com leitura e escrita de cache do OpenRouter (`cost` e `cache_discount`) não foi comparado com o real: "não conferido".

Tokens lidos do cache entram no custo com `cache_read_per_m` e os escritos com `cache_write_per_m`; a escrita de 1 h da Anthropic custa 2× o preço de entrada (`core.Usage.CacheWrite1hTokens` = parte de `CacheWriteTokens` gravada com TTL de 1 h; o restante cobra `cache_write_per_m`), como no real (a de 5 min, 1,25×, é o `cache_write_per_m` esperado); o ITPM da Anthropic ignora a leitura (spec 010). O efeito no TTFT está na spec 008.

**Responses API ([ADR 0055](decisions/0055-contrato-da-responses-api.md)).** O pipeline é o mesmo do Chat; muda a serialização (spec 004, "Responses API"). `instructions` vira a primeira mensagem (`developer`) e `input` string vira uma mensagem `user`, com o mesmo overhead de template do Chat (conferido: `"Say hi in two words."` = 13, como no Chat). Com `previous_response_id`, a conversa gravada (entradas e a saída da resposta anterior como mensagem `assistant`) precede a entrada nova na contagem (conferido: 27 = 10 + 7 + 7 + 3) e no cache de prompt: o prefixo da conversa gravada conta como já escrito na conta, então a chamada encadeada acerta o cache pelas regras da tabela acima (não conferido até uma fixture com conversa ≥ 1 024 tokens). Na Responses API o cache de prompt nunca guarda o último bloco de 128 tokens do prompt: os cortes válidos ficam a pelo menos 128 tokens do fim (conferido; um prompt de 1 697 tokens lê 1 536, não 1 664; no Chat o mesmo tamanho lê o corte de 128 mais próximo), regra em `cache/valkey/README.md` 2.4. O teto `max_output_tokens` termina com `status: "incomplete"` e `incomplete_details.reason: "max_output_tokens"` (conferido); raciocínio sai como item `reasoning` antes da mensagem e em `output_tokens_details.reasoning_tokens` (não conferido). A fixture de cache `testdata/real/openai/responses/cache_automatic_prefix.json` mostra `input_tokens_details` com `cache_write_tokens: 0` e `cached_tokens` 0, 0, 1536 nos três envios do mesmo prefixo de 1 697 tokens (6 s de intervalo) e 0 no 4º, em stream: o acerto do real não é determinístico entre réplicas; o wwz acerta a partir do 2º envio, como na regra da tabela (a diferença fica marcada "não conferido" até uma série maior).

**Multimodal.** `image_url` e `input_audio` são aceitos e contados com custo fixo configurável; o conteúdo é ignorado.

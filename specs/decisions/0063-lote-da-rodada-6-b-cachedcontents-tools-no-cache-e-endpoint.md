# 0063 — Lote da rodada 6 (B): chaves de `cachedContents` do código, tools no início do cache de prompt, template da Anthropic e `endpoint` no evento

Status: provisória (revisar se uma fixture mostrar a OpenAI lendo cache com as tools depois das mensagens, se o `gtok` mudar o template do Gemini, ou se um consumidor do `ev:stream` não aceitar campo novo)
Data: 2026-10-31

## Contexto
Tarefa 302 do guardião reuniu os achados das tasks 246, 156, 265, 264 e 267. Princípio: ADR 0042 (igual ao real; senão "não conferido"). Regra da 265: o código mergeado manda nas chaves do Valkey.

## Decisão
1. **`cachedContents` no store (substitui o item 5 da [0061](0061-lote-da-rodada-6-achados-de-spec.md)).** O código da 265 guarda o **conteúdo** (`systemInstruction`, mensagens e tools no campo `body`), porque a geração que lê o cache precisa dele (as tools do cache habilitam chamadas; a contagem soma o cache). Chaves reais: `P:gcc:{gcc:<dono>}` (zset por criação) e `P:gcc:{gcc:<dono>}:<nome>` (hash com `PEXPIREAT` no `expireTime`), escritas em `MULTI` (as duas têm a mesma hash tag). O dono é o escopo do driver, não só a conta. Interface `limits.CachedContentStore` com Put/Get/List/Delete (o `PATCH` reusa o Put). O README 2.6 foi reescrito a partir do código; a 0061 fica valendo no resto. Fato novo: o real só deixa usar o cache com o conteúdo.
2. **Tools no início do cache de prompt (task 246).** Hoje `pcAutoBlocks` põe o bloco de tools depois das mensagens. No real da OpenAI as tools são renderizadas no cabeçalho do prompt, antes das mensagens, e mudar as tools invalida o cache inteiro. O wwz passa a posicionar o bloco de tools **primeiro** (OpenAI, Azure e gateways no dialeto `openai`); a Anthropic já usa tools → system → messages; o Gemini alinha a ordem dos blocos ao template do `gtok` (a 303 faz as duas coisas). A contagem de tokens do prompt não muda (só a ordem dos blocos). Como os hashes gravados mudam, a constante do hash inicial passa de `wwz-pc/1` para `wwz-pc/2`: entradas antigas não geram acerto falso (miss único após a atualização; TTL máximo 24 h). Tarefa de código 303.
3. **Custo com cache no OpenRouter (task 246).** O custo com leitura e escrita de cache do OpenRouter fica "não conferido" (a fórmula do `cost` com `cache_discount` não foi comparada com o real). Registrado na spec 007.
4. **Template da Anthropic (task 264).** A spec 007 passa a descrever o template por geração (4.5, 4.6, 4.7, 4.8/5, 5.5) e o texto das tools em UTF-8 a partir da 4.6; constantes ficam em `internal/generate/template_anthropic.go`, medidas com `count_tokens`.
5. **Gemini (task 265).** Spec 006 16.5 ganha os fatos de 31/10/2026 (rota desconhecida 404 vazio antes da credencial, `candidateCount`, template de contagem); spec 003/007: `tokenizer: auto` no dialeto `gemini` usa o `gtok` calibrado (erro médio 4,9%, soma −2,3%). O tokenizador real segue "não conferido".
6. **Drift (task 156).** Spec 017 registra `opencode-google` (padrão), `azure` fora do padrão (só com `--base-url`; sem ele `skipped`/`no_key`), `AZURE_OPENAI_API_KEY`, alias `GEMENI_API_KEY` e o modelo do Gemini `gemini-3.5-flash-lite` (o 2.5 saiu da tabela).
7. **`endpoint` no evento (task 267).** O `limits.Event` ganha o campo opcional `endpoint` (`chat`, `messages`, `responses`, `generate_content`, ...; ausente em eventos sem requisição). Aditivo: leitores antigos ignoram o campo. As métricas e os logs das Responses dos gateways usam `endpoint=responses`, e o status da métrica é o do próprio erro. Tarefa 306.

## Consequências
- Tarefas de código 303 (tools primeiro e `wwz-pc/2`), 304 (gancho de checagens de backend do Gemini depois da resolução do modelo), 305 (drift: sincronizar com a spec 017), 306 (`endpoint` no evento). Specs 003, 006, 007, 017, README do Valkey, asyncapi e `docs/drivers.md` atualizados; HISTORY.md atualizado.

## Alternativas descartadas
- Manter as tools depois das mensagens: diverge do real e faz uma mudança de tools parecer cache parcial.
- Manter `wwz-pc/1`: acertos falsos por até 24 h após atualizar.
- Guardar só tokens no `cachedContents` (0061): a geração com `cachedContent` não teria as tools nem o texto.

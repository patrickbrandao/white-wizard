# 011 — Motor de cenários sequenciais

Status: rascunho
Depende de: 007, 009
Origem: seções 9 do documento Claude Docs congelado em 2026-10-30

## Contexto
Cenários YAML stateless, seleção, passos, variáveis e timing por passo (seção 9).

## Requisitos
- R11, R24
- I6–I14

## Interfaces e contratos
- `internal/scenario`
- `scenarios/*.yaml` e os 6 cenários embutidos (`scenarios/embed.go`, pacote `scenarios`)
- `/admin/scenarios`

## Dados
- Apenas `attempts:{conv}:{step}` no store (TTL 1 h)

## Critérios de aceite
- [ ] `file-agent-flow` roda 1 000 conversas simultâneas preservando ordem e ids de tool call
- [ ] Com `step_limit: N`, o turno N+1 do assistente segue `on_exhausted` mesmo quando um passo casa
- [ ] `tool_call_id_format: native` produz ids no formato do driver; `encoded` usa `toolu_` nas rotas Messages
- [ ] Sem `scenarios.dir` nem `scenarios.files`, os 6 cenários embutidos estão em `GET /admin/scenarios`, em `wwz validate` e em `wwz simulate --scenario <nome>`, também na imagem Docker; `scenarios.builtin: false` os remove; arquivo de `dir`/`files` com o mesmo `name` substitui o embutido
- [ ] `semantic_match` casa mensagens com as mesmas palavras em outra ordem e rejeita abaixo do limiar

## Fora de escopo
- Estado de conversa no servidor

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0027-formas-finais-dos-recursos-importados.md](decisions/0027-formas-finais-dos-recursos-importados.md)
- [specs/decisions/0048-dialetos-catalogos-e-templates.md](decisions/0048-dialetos-catalogos-e-templates.md)

## Comportamento

### 9. Motor de cenários sequenciais (R11)

Um cenário é uma lista ordenada de passos; a cada requisição o motor olha o histórico de mensagens, deduz em que passo a conversa está e emite a resposta daquele passo. O servidor não guarda nada entre requisições, então o motor escala como qualquer outra rota.

**Como um cenário é escolhido.** Na ordem de `scenarios.select` (padrão `[header, metadata, marker, model_suffix, match]`): header `x-wwz-scenario: <nome>` > campo `metadata.scenario` (OpenAI) ou `metadata.user_id` com prefixo `scenario:` (Anthropic) > marcador `[[wwz:nome seed=N speed=N]]` na última mensagem do usuário (apelidos `[[sim:` e `[[llmsim:`; o marcador é removido antes da geração) > model id `<modelo>-scenario-<nome>` (o modelo é resolvido pela parte antes do último `-scenario-` quando o nome inteiro não é id nem alias; métricas e eventos levam o `models[].id`, e a seleção vê o nome pedido, [ADR 0040](decisions/0040-label-de-modelo-e-get-de-modelo-openrouter.md)) > lista `scenarios` do modelo, testando o `match` de cada um (ou o cenário codificado no último id de tool call) > `fallback` do modelo (geração aleatória). O cenário escolhido é informado em `x-wwz-scenario` e o passo em `x-wwz-step`.

**Como o passo é escolhido.** O motor calcula um estado a partir das mensagens: `turn` (número de mensagens do usuário), `tool_results` (quantas mensagens `role: tool` / blocos `tool_result`), `last_tool` (nome da última tool chamada pelo assistente), `last_tool_result` (conteúdo do último resultado), `last_step` (decodificado do último id de tool call), `tools_available` (nomes das tools na requisição) e o número de turnos do assistente. Em ordem ([ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)):

1. Se a conversa já tem `step_limit` turnos do assistente (padrão 50), vale `on_exhausted` (padrão `repeat_last`): `repeat_last` responde com o último passo declarado no arquivo, `error` devolve 400 `invalid_request` no formato do driver, `fallback` usa a geração do modelo.
2. Senão, o primeiro passo cujo `expect` casa é usado (ou sorteio por `weight`).
3. Sem casamento, vale `on_no_match` (padrão `fallback`): `repeat_last` (último passo declarado no arquivo), `error` ou `fallback`.

**`system_matches`** (task 245, ADR 0061): nas rotas OpenAI (chat, completions, responses) casa a **primeira** mensagem `system`/`developer` de `messages` (o parser mantém o system só ali; `core.Request.SystemPrompt()`); nas rotas Anthropic e Gemini casa o `system`/`systemInstruction` de fora das mensagens. Sem system, o campo não casa.

**Campos do cenário:** `name`, `description`, `match` (`tools_include`, `model`, `driver`, `user_message_matches`, `system_matches`, `header`), `vars`, `steps`, `on_no_match`, `on_exhausted`, `step_limit`, `fallback` (`random` ou `echo`) e `tool_call_id_format`.

**Ids de tool call (I8).** `tool_call_id_format: encoded` (padrão) gera `<prefixo>_<cenário>_<passo>_<n>_<nonce>`, com prefixo `toolu` nas rotas Messages e `call` nas demais, e o próximo turno identifica o passo pelo id (`expect.last_step`). `native` usa o id real do driver (`call_` + 24 base62, `toolu_01…`), idêntico ao provedor; nesse modo `last_step` não existe e `wwz validate` acusa o seu uso. `tool_calls[].id` literal vence os dois.

**Exemplo: agente de arquivos (ler → gravar → editar → apagar)**

```yaml
name: file-agent-flow
match:
  tools_include: [read_file, write_file]   # só casa se o agente oferece essas tools
vars:
  path: "/tmp/{{rand.word}}-{{rand.int 1000 9999}}.txt"
steps:
  - id: read
    expect: { tool_results: 0 }
    reasoning: { tokens: 40..120 }
    tool_calls:
      - { name: read_file, args: { path: "{{vars.path}}" } }

  - id: write
    expect: { last_tool: read_file }
    tool_calls:
      - { name: write_file, args: { path: "{{vars.path}}", content: "{{lorem 300}}" } }

  - id: edit
    expect: { last_tool: write_file, last_tool_result_matches: "(?i)ok|written|success" }
    tool_calls:
      - { name: edit_file, args: schema }        # argumentos gerados do schema da tool

  - id: edit-failed
    expect: { last_tool: write_file }           # resultado não bateu no regex acima
    content: "A gravação falhou: {{state.last_tool_result}}. Vou tentar de novo."
    tool_calls:
      - { name: write_file, args: { path: "{{vars.path}}", content: "{{lorem 100}}" } }

  - id: delete
    expect: { last_tool: edit_file }
    latency: { ttft: 2s }                        # override só neste passo
    tool_calls:
      - { name: delete_file, args: { path: "{{vars.path}}" } }

  - id: done
    expect: { last_tool: delete_file }
    content: { tokens: 50..200 }
    finish_reason: stop

  - id: give-up
    expect: { turn_gte: 12 }                     # guarda contra loops infinitos do agente
    content: "Não consegui concluir a tarefa."
    finish_reason: stop

on_no_match: fallback
```

**Variáveis.** `vars` são calculadas uma vez por conversa, de forma determinística: a semente é o hash da primeira mensagem do usuário + `seed` global, então o mesmo `path` aparece em todos os passos sem o servidor guardar estado. Templates têm acesso a `vars`, `state`, `req` (modelo, última mensagem, tools) e funções `rand.*`, `lorem`, `now`, `uuid`, `pick [a b c]`.

**Campos de um passo**

| Campo | Significado |
| --- | --- |
| `expect` | Condições: `turn`, `turn_gte`, `tool_results`, `tool_results_gte`, `last_tool`, `last_step`, `last_tool_result_matches`, `tool_result_for: <tool>`, `user_message_matches`, `latest_message_role`, `semantic_match`, `model`, `header: {nome: valor}`, `always` |
| `reasoning` | `{tokens: a..b}` ou texto literal |
| `content` | Texto literal, template, ou `{tokens: a..b}` para texto aleatório; junto com `tool_calls` forma o turno misto (texto antes das chamadas) |
| `tool_calls` | Lista; `name` literal ou classe `any`, `any_read`, `any_write` (I9); `args` literal, template, `schema` (gerar do schema) ou `schema+` (gerar e sobrescrever com `overlay`); `id` literal opcional |
| `parallel` | `true` emite todas as tool calls na mesma resposta; `false` uma por turno |
| `finish_reason` | `stop`, `length`, `tool_calls`, `content_filter` |
| `error` | Tipo de erro da seção 7, para forçar erro neste passo |
| `latency` | Override do perfil só neste passo |
| `repeat` | `{times: N}` repete o passo N turnos antes de avançar (agente que insiste) |
| `goto` | Nome de outro passo para loops explícitos |
| `weight` | Quando vários passos casam, sorteio ponderado em vez do primeiro |
| `speed`, `stall`, `cut_after_tokens` | Timing do passo (I6): multiplicador de velocidade, pausa `{after_tokens, duration}` e corte do stream após N tokens |
| `fail_first` | `{times, error, retry_after_ms, message}`: erro só nas N primeiras tentativas (I7, `attempts:` no store) |
| `headers` | Headers extras na resposta |
| `tool_misbehavior`, `moderation` | Força um tipo de I1 ou um modo de content filter neste passo |
| `timeline` | Sequência explícita de eventos (I14; ver abaixo) |

**Timeline (I14).** `timeline: [...]` define a resposta do passo evento a evento e substitui `content`, `reasoning`, `tool_calls` e `finish_reason` do passo. Tipos: `text` e `reasoning` (`text`), `tool_call` (`name`, `args` literal), `ping`, `delay` (`ms`), `usage`, `error` (`error` = tipo da seção 7, padrão `server_error`; `text` = mensagem) e `finish` (`reason`).
- O conteúdo, o raciocínio e as tool calls da resposta são a concatenação dos eventos até o primeiro `finish`; eventos depois dele são ignorados (no stream e no não-stream). Sem `finish`, o motivo é `tool_calls` quando há chamadas e `stop` nos demais casos.
- No stream os eventos são reproduzidos em ordem pelo `StreamWriter`: `text`/`reasoning` viram um delta cada (com o `itl`/`reasoning_itl` do perfil), `tool_call` emite a chamada inteira (início, argumentos em pedaços, fim), `ping` um keep-alive do dialeto e `delay` uma pausa sem frame. `usage` não gera frame próprio: o `usage` é calculado do conteúdo da timeline e vai junto com o `finish` (ou no chunk final de `include_usage`). `error` emite o erro no meio do stream, no formato do dialeto, e encerra sem `finish`.
- No não-stream, um `error` antes do `finish` vira a resposta inteira: depois do tempo simulado, sai o erro HTTP do tipo indicado no formato do driver, no lugar do corpo de sucesso. `ping`, `delay` e `usage` não têm efeito no não-stream além do tempo.
- O texto da timeline (`text` e `name` do `tool_call`) recebe só os placeholders simples `{{model}}`, `{{last_user_message}}`, `{{request_id}}` e `{{prompt_tokens}}`; demais expressões (`vars`, `state`, `rand.*`…) ficam literais. `args` vai como está, serializado em JSON.

**`semantic_match` (I12).** `{text: "...", threshold: 0.5}` ou só o texto (`threshold` 0,5). Casa quando a similaridade de cosseno entre os vetores de frequência de palavras (minúsculas; palavra = sequência de letras e dígitos) da última mensagem do usuário e de `text` é ≥ `threshold` (0 < t ≤ 1). Lexical e determinística, sem embeddings.

**Cenários embutidos** ([ADR 0048](decisions/0048-dialetos-catalogos-e-templates.md)). Seis cenários vêm dentro do binário `wwz` (`go:embed` dos YAML de `scenarios/`, pacote `scenarios`), então funcionam sem `scenarios.dir`, em `wwz simulate` e na imagem Docker:

| Nome | Fluxo |
| --- | --- |
| `file-agent-flow` | O fluxo acima: list → read → write → resposta |
| `search-then-answer` | `web_search` → `fetch` → resposta |
| `multi-tool-parallel` | 3 tool calls na mesma resposta (`parallel: true`) → resposta depois dos 3 resultados |
| `retry-on-error` | 429 com `retry-after` na primeira tentativa do passo da tool (`fail_first`, I7) → a nova tentativa traz a tool call → resposta |
| `long-reasoning` | Raciocínio de ~2 000 tokens (`reasoning: {tokens: 1800..2200}`) antes de uma tool call → resposta |
| `refusal` | 1.º turno normal; 2.º turno com `finish_reason: content_filter` (recusa na forma de cada dialeto, spec 009) |

Carga: embutidos primeiro (salvo `scenarios.builtin: false`), depois `scenarios.dir`, depois `scenarios.files`; um cenário com o mesmo `name` de um já carregado o substitui, com log `info` (`scenario overridden`). Os embutidos são validados por teste (o mesmo de `wwz validate`), então nunca são pulados. O diretório `scenarios/` do repositório é a fonte dos embutidos e continua utilizável como `scenarios.dir`.

**Origem não é registrada ([ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md)).** O registro guarda só o nome e o cenário; não marca se veio do binário, de `dir`, de `files` ou de `POST /admin/scenarios`. Por isso, numa recarga com `scenarios.builtin: false`, o wwz remove do registro todo cenário com o nome de um dos seis embutidos, mesmo que o operador o tenha substituído por um arquivo de `dir`/`files` (que é recarregado em seguida e volta) ou o tenha carregado por `POST /admin/scenarios` com o mesmo nome (esse some até ser enviado de novo). Quem quer um cenário próprio com nome de embutido usa `dir`/`files`, não o POST, ao desligar `builtin`. Os seis nomes estão em `scenarios/embed.go` (`scenarios.Files()`); `docs/configuracao.md` lista a chave `scenarios.builtin` e os seis.

**Validação.** `wwz validate scenarios/*.yaml` confere sintaxe, passos inalcançáveis e templates; `wwz simulate --scenario file-agent-flow --turns 6` roda o cenário em memória e imprime cada resposta sem subir servidor.

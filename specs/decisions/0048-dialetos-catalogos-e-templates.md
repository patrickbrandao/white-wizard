# 0048 — Dialetos e catálogo por modelo, `drivers[].options` e cenários embutidos

Status: provisória (o trecho sobre o OpenCode servir só o dialeto nativo foi substituído pela [0062](0062-opencode-serve-todo-modelo-em-toda-rota.md))
Data: 2026-10-31
Gatilho de revisão: conferência com chave real mostrar que o OpenCode aceita um modelo fora do endpoint nativo dele, ou que o OpenRouter recusa um modelo numa das rotas dele; alguém depender de `opts` num YAML anterior à `v0.1.0`; decisão sobre limites por conta (abaixo; tomada na [0051](0051-limites-na-conta-e-promocao-de-tier.md)).

## Contexto
Ao escrever `docs/` (task 125) apareceram divergências entre specs e código (task 168):

1. `models[].protocols`: a spec 003 define a lista como dialetos de wire (`openai`, `anthropic`, `gemini`); o código a compara com o **nome do driver** (`ModelSupports(drv.Protocol())`), então um modelo só aparece no `openrouter` ou no `opencode` se esses nomes estiverem na lista (senão 404 "No endpoints found"/401 "not supported"). Os padrões embutidos e `internal/drift/wwz.yaml` usam a forma de nomes de driver, inclusive para restringir catálogo (`openai/gpt-4.1-nano` só no OpenRouter, `glm-4.6` só no OpenCode). Pela ADR 0042 vale o real: cada provedor tem catálogo próprio (a OpenAI não lista `glm-4.6`), o OpenRouter atende qualquer modelo do catálogo em todas as rotas (chat, completions, responses, messages) e o OpenCode expõe cada modelo no endpoint da API nativa dele (`/openai/v1/...`, `/anthropic/v1/messages`, `/google/v1beta/...`, spec 006, 16.3).
2. A spec 007 e as ADRs 0022/0046 usam `drivers[].options.reasoning_field`; o código lê `drivers[].opts` e recusa `options` como campo desconhecido. Nenhum YAML do repositório, de `docs/` ou de `examples/` usa `opts`, e nada foi lançado (a `v0.1.0` não existe).
3. A spec 003 citava `PATCH /admin/config`, que não existe (é `POST` com merge patch, spec 013 e openapi).
4. A spec 010 descrevia `server.target_rps`, `server.rps_shape`, `limit_share`, `shared_limiter` e "limites locais à instância", nada disso existe; o código conta limites no store (cluster com `valkey`).
5. A spec 011 prometia 6 cenários "entregues com o binário"; há 3 em `scenarios/` e nada embutido (a imagem não traz nenhum).

## Decisão
1. **Dois conceitos separados no modelo.**
   - `models[].protocols` = dialetos de wire (`openai`, `anthropic`, `gemini`; `*` = todos). O dialeto de uma requisição vem do tipo da rota (`Kind`: chat/completions/responses/embeddings → `openai`; messages/count_tokens → `anthropic`; rotas Gemini → `gemini`), não do nome do driver.
   - `models[].drivers` (campo novo, opcional, compatível) = drivers em cujo catálogo o modelo aparece (listagem e aceitação). Ausente = todos os drivers que falam pelo menos um dos dialetos do modelo.
   - Drivers diretos (`openai`, `azure`, `anthropic`, `gemini`) e `opencode` servem o modelo só nas rotas de um dialeto da lista (no OpenCode, como o real, por endpoint nativo; "não conferido" até a chave real). O `openrouter` serve todo modelo do seu catálogo em todas as rotas dele, qualquer que seja `protocols`.
   - Compatibilidade: `openrouter`/`opencode` em `protocols` (forma antiga) geram aviso e são traduzidos: a lista inteira (nomes de driver) vira `drivers`, e os dialetos presentes nela (`openai`, `anthropic`, `gemini`; nenhum → `openai`) viram `protocols`. Isso preserva exatamente o comportamento atual dos YAML existentes. Outros valores tornam a configuração inválida. Padrões embutidos e `internal/drift/wwz.yaml` migram para a forma nova.
2. **`drivers[].options`** é o nome (spec é a fonte da verdade, e o resto do YAML não abrevia). `opts` é recusado com `config: drivers[i]: field "opts" renamed to "options"`, sem alias: nada foi lançado e nenhum YAML conhecido usa `opts`. Opção desconhecida para o driver torna a configuração inválida.
3. Spec 003 corrigida: recarga por `SIGHUP` ou `POST /admin/config` (corpo vazio ou merge patch).
4. Spec 010 reescrita conforme o código e a spec 012. Fato registrado sem mudar código: limites de taxa e concorrência contam na entidade `key`; gasto, créditos e cota em dólares na `account`. Com a conta padrão por chave (`acct-<label>`) isso equivale ao real, mas diverge quando várias chaves compartilham `account` (o real limita a organização/projeto). Decidir onde declarar limites por conta (seção `accounts:` ou limites do tier valendo na conta) fica para uma tarefa própria do guardião. `tiers.*.promote_when` está no YAML mas não é avaliado; idem.
5. **Seis cenários embutidos** no `wwz` via `go:embed` num pacote `scenarios` (`scenarios/embed.go`), que tem os YAML como fonte: `file-agent-flow`, `search-then-answer`, `multi-tool-parallel`, `retry-on-error` (fica o significado já entregue e documentado: 429 com `retry-after` na primeira tentativa, I7), `long-reasoning`, `refusal`. Ordem de carga: embutidos, `scenarios.dir`, `scenarios.files`; mesmo `name` substitui o anterior com log `info`. `scenarios.builtin: false` (campo novo, padrão `true`) desliga os embutidos. Como vão no binário, a imagem não precisa copiar `scenarios/`.

## Alternativas consideradas
- Manter `protocols` como nomes de driver e só corrigir a spec: mistura dialeto com catálogo e não expressa o OpenCode por endpoint nativo nem o OpenRouter em todas as rotas.
- Gateways servirem todo modelo configurado, sem campo `drivers`: perde o catálogo por provedor (a OpenAI listaria `glm-4.6`), contra a ADR 0042, e quebra `internal/drift/wwz.yaml`.
- Aceitar `opts` como alias de `options`: dois nomes para o mesmo contrato sem usuário que precise dele.
- Copiar `scenarios/` na imagem em vez de embutir: não resolve `wwz simulate` nem o binário avulso do release.

## Consequências
- Specs 003, 007, 010 e 011 atualizadas (task 168).
- Tarefas de código: dialeto/catálogo por modelo (implementador config+engine), renomeação `opts` → `options` (junto da task 154, mesmos arquivos), cenários embutidos (implementador scenario+cmd). `docs/configuracao.md` perde o aviso de "divergência conhecida" quando a primeira entregar.
- Pendências do guardião: limites por conta e avaliação de `promote_when`.

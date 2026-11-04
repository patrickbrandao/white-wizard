# 0059 — Lote da rodada 5: achados de spec e decisões de comportamento

Status: provisória (revisar se um fato novo do real contradisser alguma linha, em especial a base não-Go do OpenCode sem `x-opencode-session`, a forma `chatcmpl-` do upstream sem fixture, e a regra de `weights`)
Data: 2026-10-31

## Contexto
As tarefas 175, 177, 178, 193, 151, 238 e 232 deixaram divergências entre código, fixtures e specs, reunidas na task 255. Decide-se pelo princípio da [ADR 0042](0042-fidelidade-total-ao-provedor-real.md): igual ao real quando há provedor; sem provedor (configuração do wwz), vale o menor atrito sem esconder erro.

## Decisão
- **`models[].protocols` ausente ou vazio vale `["*"]`.** É o que o código faz (`ModelConfig.Speaks`); a spec 003 o chamava de obrigatório. Configuração do wwz, sem equivalente no real: o atalho evita uma lista de três itens em todo modelo e não esconde erro (valores fora do conjunto continuam inválidos). Só a spec muda.
- **`drivers[].options.require_session` do `opencode`: só `true`.** O real (Go) responde 400 `MissingSessionID` sem `x-opencode-session`, e o padrão `false` do wwz era um desvio que a ADR 0042 proíbe. O padrão passa a `true` e `false` torna a configuração inválida, pela mesma regra de `reasoning_field` (o valor só pode ser um comportamento que o real tem). A base não-Go (`/inference` sem `go`) não tem fixture sem sessão: segue igual à Go e fica "não conferido". Tarefa de código derivada.
- **`weights` de perfil de erros são validados.** Chave fora de `types` (ou da lista padrão), peso ≤ 0 ou não finito tornam a configuração inválida, como `types` desconhecidos desde a task 151. Hoje o código ignora a chave e trata peso ≤ 0 como 1, o que esconde erro de digitação (`rate_limt: 5`). Tarefa de código derivada.
- **Drift do `opencode` com upstream variável.** A sonda fixa uma sessão por execução e aceita as três formas conhecidas de `id` (`gw-…`, 32 hex, `chatcmpl-…`) como equivalentes; se a resposta do real só trouxer `chatcmpl-` em 3 sessões, o provedor sai `skipped`/`upstream_form_unreproduced`, pois o wwz reproduz `gw-` e acusar drift não ajuda. Qualquer outra divergência com a forma `gw-` segue como drift. Tarefa de código derivada.
- **Registro de cenários sem origem.** Mantém-se: `builtin: false` remove por nome, inclusive um cenário de mesmo nome carregado por `POST /admin/scenarios`. Marcar a origem custaria um campo em `Registry` só para um caso raro; a spec 011 e o openapi passam a avisar.
- **Rotas de conta do OpenRouter.** `GET /api/v1/credits`, `GET`/`POST /api/v1/keys` entram no MVP (a ADR 0054 as deixara de fora por falta de fixture; as fixtures da task 193 existem): `credits` devolve o comprado e o gasto da conta; `keys` responde sempre 401 `Invalid API key`, porque o wwz só tem chaves de inferência. `GET /api/v1/key` e `/api/v1/auth/key` passam a seguir a fixture (campos, ordem, `limit` = teto da chave, `cache-control: private, no-store`).
- **Textos alinhados ao código e às fixtures, sem mudança de comportamento:** contrato da Responses (ordem de validação, limites impressos `2.0`/`1.0`, 404 no leiaute da API, `conversation` 404, `instructions` não herdadas, `strict` omitido, eco de `json_schema`, cache sem o último bloco de 128 tokens, `output_tokens`), forma do `/v1/usage` e dos erros do Messages do OpenCode, raciocínio padrão do OpenCode, `wwz-drift check --real` e `providers[].base`, nomes e pesos de perfil de erros.

## Alternativas consideradas
- `protocols` obrigatório: quebra todas as configurações existentes e exemplos sem ganho de fidelidade.
- Manter `require_session: false` por padrão com aviso: é o desvio que a 0042 proíbe; clientes sem sessão são justamente o caso que o teste de roteador precisa ver falhar como no real.
- Pesos inválidos viram aviso: o aviso se perde em log de boot; a regra dos `types` já é erro.
- No drift, fixar um modelo e reduzir o `id` a "string": perde a checagem do prefixo `gw-` que o wwz reproduz.

## Consequências
- Specs 003, 004, 006, 007, 009, 011, 017, 018, `cache/valkey/README.md` (2.4) e a descrição de `loadScenarios` em `cmd/wwz/openapi.yaml`.
- Tarefas de código: (1) opencode `require_session` só `true`; (2) config: validar `weights`; (3) drift: sessão fixa e `id` equivalente; (4) openrouter: `/api/v1/key`, `credits`, `keys`; (5) docs: `docs/configuracao.md` (`scenarios.builtin`, `protocols`, `require_session`). Descritas no relatório da task 255.

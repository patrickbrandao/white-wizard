# 0032 — Campos de uso do evento `request` no `ev:stream`

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A task 075 pôs em `fields` do evento `request` os campos `reasoning_tokens`, `cached_tokens`, `cost_usd` e `tool_calls` (asyncapi 0.3.0), e a ADR 0027 decidiu que o painel mostra a decomposição da entrada (I16) a partir do mesmo evento. Fatos novos, conferidos no código:
- `FinishRequest.event` (`internal/limits/store.go`) grava `reasoning_tokens` e `cached_tokens` sempre, inclusive 0, nas requisições atendidas; nas negações (`Denial`) nenhum dos quatro campos aparece. O asyncapi 0.3.0 dizia "presentes quando não nulos", e o painel calcula a taxa de cache hit só sobre eventos que trazem `cached_tokens` (`internal/panel/store/usage.go`): com o texto do contrato, a taxa daria 100 %.
- `cost_usd` sai só quando positivo. Em `models[].pricing` os preços são `float64` sem sinal de "configurado": modelo gratuito (preços 0) e modelo sem preço são indistinguíveis, e o gasto simulado debitado é 0 nos dois casos. Um grupo só com esses modelos aparece como "—" no painel, como se o wwz não publicasse o campo.
- `internal/config/load.go` preenche `keys[].account` com `acct-<label>` quando a chave não tem conta, e os limites e o gasto valem para essa conta; quase todo evento leva `account`.
- `x-wwz-input-breakdown` (`internal/engine/generate.go`) só existe com `usage.extended: true` e a entrada contada; o JSON tem nove inteiros em ordem fixa.

## Decisão
- **Contadores sempre presentes.** Em `request` e em `error` que não é negação, `fields` traz sempre `duration_ms`, `prompt_tokens`, `output_tokens`, `reasoning_tokens`, `cached_tokens` e `cost_usd` (número em dólares, 0 incluído: é o gasto simulado debitado, também 0 para modelo gratuito ou sem preço). `tool_calls` continua ausente quando nenhuma tool call foi emitida (economiza bytes no caso comum). Em negação (`fields.denied: true`) nenhum de `reasoning_tokens`, `cached_tokens`, `cost_usd`, `tool_calls` e `input_breakdown` aparece.
- **Decomposição da entrada (I16).** Novo campo `fields.input_breakdown`, objeto com as mesmas nove chaves e valores do header `x-wwz-input-breakdown` (`system`, `tool_schemas`, `user`, `assistant`, `tool_calls`, `tool_results`, `images`, `cached`, `total`), presente só quando a resposta levou o header (`usage.extended: true` e entrada contada). Desligado por padrão para não pesar o stream.
- **Conta padrão.** A conta `acct-<label>` vai ao evento como qualquer outra e entra no agrupamento "por conta" do painel: é a entidade real em que os limites e o gasto são contados, e a tela de Limites já a mostra.
- **Painel.** "—" só quando nenhum evento do grupo traz o campo (eventos de wwz anterior ao asyncapi 0.4.0); a partir do 0.4.0 (evento atendido com `cost_usd`), `tool_calls` ausente conta como 0.
- asyncapi sobe para 0.4.0 (mudança compatível: só acrescenta e torna explícito o que já era gravado).

## Alternativas consideradas
- Sinal "tem preço" no `FinishRequest` para gravar `cost_usd: 0` só em modelos com preço configurado: exige mudar `Pricing` para ponteiros ou um campo novo no YAML, por uma distinção que o gasto debitado não faz.
- `tool_calls: []` sempre: ~16 bytes a mais por evento sem informação nova.
- `input_breakdown` sempre: dobra o tamanho médio do evento em carga alta.
- Esconder a conta padrão (gravar só contas explícitas): o agrupamento "por conta" divergiria da tela de Limites, que mostra a conta padrão.

## Consequências
- asyncapi 0.4.0, specs 012 e 016 atualizadas (a spec 013 já remete o painel ao evento `request`).
- Tarefas: `cost_usd` sempre e `input_breakdown` em `limits.FinishRequest`/engine; painel com a decomposição da entrada e `tool_calls` ausente = 0.

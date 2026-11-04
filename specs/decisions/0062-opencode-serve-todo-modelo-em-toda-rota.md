# 0062 — O `opencode` serve todo modelo em toda rota

Status: provisória (revisar se o real passar a recusar um modelo em alguma rota, ou se a rota de um dialeto fora do catálogo do modelo se mostrar diferente das fixtures)
Data: 2026-10-31
Substitui: parte da [0048](0048-dialetos-catalogos-e-templates.md) (o `opencode` servir cada modelo só no endpoint do dialeto nativo).

## Contexto
A 0048 supôs, "não conferido", que o OpenCode expõe cada modelo só no endpoint nativo. A task 241 gravou 25 fixtures em `testdata/real/opencode/google/` e viu o gateway servir qualquer modelo (inclusive `gpt-*`, fixture `gen_gpt_model`) na rota `google`; a documentação diz o mesmo ("cada modelo tem uma API nativa, mas todos funcionam pelas outras"). Isso é o fato novo exigido para mudar a decisão.

## Decisão
- O `opencode` serve todo modelo do seu catálogo em **todas** as rotas (OpenAI chat/responses, Anthropic Messages, Gemini), qualquer que seja `models[].protocols`, como o `openrouter` (0048). `protocols` continua valendo para os drivers diretos (`openai`, `azure`, `anthropic`, `gemini`); no `opencode` ele só define o dialeto **nativo**, que decide a forma da resposta traduzida (spec 006, 16.3: forma Vertex só para `gemini-*`).
- O catálogo (`/v1/models` do `opencode`) segue `models[].drivers`.
- `countTokens` e métodos desconhecidos na rota Gemini continuam 400 sem corpo (conferido).

## Consequências
- Tarefa de código (281): `internal/driver/opencode` e `internal/engine` deixam de filtrar por dialeto; `drivertest` (tarefa 283) precisa suportar driver com vários dialetos.
- Specs 003 e 006 atualizadas.

## Alternativas descartadas
- Manter o filtro por dialeto: contra a ADR 0042 (o real serve).

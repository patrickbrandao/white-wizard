# 0028 — Dialetos Gemini e Azure fora do MVP (I17 e I18 parciais)

Status: substituída pela 0043 (revisão com o humano em 2026-10-31)
Data: 2026-10-31
Substitui: parte do item "recursos I1–I24" da [ADR 0001](0001-decisoes-iniciais.md) (só os dialetos Gemini e Azure de I17 e I18)

## Contexto
A ADR 0001 põe I1–I24 inteiros no MVP. I17 pede content filter em `SAFETY` (Gemini) e `innererror` (Azure); I18 pede `thought` parts (Gemini). Fato novo: a spec 005 restringe o MVP aos quatro drivers (`openai`, `anthropic`, `opencode`, `openrouter`) e põe Gemini e Azure OpenAI em "candidatos futuros"; não há rota, envelope nem fixture desses provedores onde o dialeto possa existir.

## Decisão
- I17 e I18 ficam completos para os quatro drivers: content filter por `moderation.mode` no dialeto de cada um; raciocínio por `reasoning_field` (`hidden`, `reasoning_content`, `reasoning`, `inline_tag`) e bloco `thinking` com `signature` na Anthropic.
- Gemini (`SAFETY`, `thought` parts) e Azure (`innererror`) entram junto com os drivers respectivos, cada um por ADR própria; sem código nem tarefa no MVP.

## Alternativas consideradas
- Simular os campos Gemini/Azure dentro dos drivers atuais: nenhum provedor real mistura esses dialetos; contradiz a ADR 0014.
- Criar os drivers Gemini e Azure agora: escopo grande (rotas, autenticação, erros, limites, fixtures) fora dos quatro provedores do MVP.

## Consequências
- Spec 017 marca as partes Gemini/Azure de I17 e I18 como adiadas.
- O critério de saída da fase 5 conta I17/I18 pelos quatro drivers.

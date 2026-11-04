# 0043 — Drivers Gemini e Azure OpenAI no MVP

Status: aceita
Data: 2026-10-31

## Contexto
A [ADR 0028](0028-gemini-e-azure-fora-do-mvp.md) deixava os dialetos Gemini e Azure fora do MVP. Na entrevista de 2026-10-31 o humano decidiu incluí-los.

## Decisão
- Dois drivers novos no MVP: `gemini` (Gemini API: `generateContent`, `streamGenerateContent`, `countTokens`, `models`, `SAFETY`, `thought` parts) e `azure` (Azure OpenAI: rotas por deployment com `api-version`, header `api-key`, `innererror` e content filter do Azure).
- Fidelidade conforme a [ADR 0042](0042-fidelidade-total-ao-provedor-real.md). **Gemini** com chave real (`GEMINI_API_KEY` em `real-keys.env`, a adicionar pelo humano; até lá, implementar pela documentação e marcar "não conferido"). **Azure** pela documentação oficial, sem fixtures reais: exceção à ADR 0014 só para esse driver, até existir chave.
- Os drivers entram na spec 005/006, no drivertest, nos SDKs (`google-genai` Python/Node; `openai` com `AzureOpenAI`), no `wwz-drift` e nos `examples/` multi-provedor.

## Alternativas consideradas
- Manter fora do MVP (0028); Azure também com chave real (humano não tem chave).

## Consequências
- Supera a [0028](0028-gemini-e-azure-fora-do-mvp.md). Tarefas 164–165.

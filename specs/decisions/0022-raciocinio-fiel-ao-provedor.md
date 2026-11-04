# 0022 — Raciocínio fiel ao provedor real

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31
Substitui: item "Campo de raciocínio OpenAI" da [ADR 0001](0001-decisoes-iniciais.md)

## Contexto
A ADR 0001 e a spec 007 (R7) mandavam o driver `openai` devolver o texto do raciocínio em `reasoning_content` (chunks `delta.reasoning_content` e `message.reasoning_content`) quando o cliente envia `reasoning_effort`. A Chat Completions real da OpenAI não expõe esse campo: o raciocínio dos modelos o-series/GPT-5 fica oculto e só aparece como contagem em `usage.completion_tokens_details.reasoning_tokens`; o stream fica em silêncio enquanto o modelo raciocina e o primeiro chunk visível já é conteúdo. `reasoning_content` é o dialeto de servidores compatíveis (DeepSeek, vLLM, OpenCode Go). Além disso, no OpenRouter `reasoning.exclude: true` só omite o texto: os tokens de raciocínio continuam sendo gerados, cobrados e contados em `usage` (spec 006, "raciocínio cobrado como saída mesmo com `exclude: true`"), mas o wwz trata `exclude` como "não raciocinar" e zera a contagem.

Fato novo: a [ADR 0014](0014-fidelidade-bit-a-bit.md) (aceita depois da 0001) exige que a forma serializada seja idêntica à do provedor real; um campo que a OpenAI nunca envia quebra essa comparação e engana o roteador sob teste (um parser que depende de `reasoning_content` passaria aqui e quebraria em produção).

## Decisão
- Novo valor `reasoning_field: hidden`. O raciocínio é gerado, consome `reasoning_itl` (silêncio antes do primeiro chunk visível, que entra no TTFT), conta em `usage` e no custo, mas o texto não é serializado.
- Padrões por driver: `openai` → `hidden`; `opencode` → `reasoning_content`; `openrouter` → `reasoning` (mais `reasoning_details[]`, spec 006); `anthropic` → bloco `thinking` (sem mudança).
- `reasoning_field: reasoning_content | reasoning | inline_tag` continua disponível como opção do driver `openai` (`drivers[].options.reasoning_field`) para emular gateways compatíveis.
- OpenRouter `reasoning.exclude: true`: o raciocínio é gerado e contado em `usage.completion_tokens_details.reasoning_tokens`, `completion_tokens` e `cost`, mas `message.reasoning`, `reasoning_details[]` e os deltas de raciocínio são omitidos. `reasoning.enabled: false` (ou `effort: none`) continua desligando o raciocínio.
- Ativação continua por requisição (`reasoning_effort`, `reasoning`, `thinking`) e por `capabilities.reasoning`. Raciocínio padrão sem pedido explícito (modelos que raciocinam por padrão) fica para as fixtures reais.

## Alternativas consideradas
- Manter `reasoning_content` como padrão do `openai`: contradiz a ADR 0014.
- Remover `reasoning_content` do driver `openai`: perde a emulação de gateways compatíveis, pedida em I18.
- Não gerar raciocínio quando oculto: o TTFT e a `usage` deixariam de refletir o provedor real.

## Consequências
- Spec 007 (R7) e spec 000 (R7) atualizadas; I18 ganha o valor `hidden`.
- Tarefa de implementador para `reasoning_field: hidden`, padrão do `openai` e `exclude` do OpenRouter (task 022).
- Testes que esperam `reasoning_content` no driver `openai` passam a configurar `reasoning_field: reasoning_content` explicitamente ou a esperar a ausência do campo.

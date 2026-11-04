# 0054 — Escopo de rotas do MVP: Responses entra; batches, embeddings e rotas auxiliares ficam fora

Status: provisória (revisar quando um roteador-alvo depender de batches, embeddings, `GET /api/v1/generation` ou das rotas de recurso da Responses; ou quando o humano pedir)
Data: 2026-10-31

## Contexto
A auditoria da task 231 comparou a tabela de rotas da spec 004 (e da 006 para OpenRouter, OpenCode, Gemini e Azure) com o `Routes()` de cada driver em `internal/driver/*`:

| Driver | Na spec, sem código | No código, sem spec |
| --- | --- | --- |
| `openai` | `POST /v1/responses` (fase 2), `POST /v1/embeddings` (fase 3) | — |
| `anthropic` | `/v1/messages/batches` (fase 3) | — |
| `openrouter` | Responses (`/api/v1/responses`, spec 006 16.4), `GET /api/v1/generation`, `GET /api/v1/models/{author}/{slug}/endpoints` | `GET /api/v1/auth/key` (alias legado de `/api/v1/key`) |
| `opencode` | `/openai/v1/responses`, `/google/v1beta/models/{m}:generateContent` e `:streamGenerateContent` (spec 006 16.3) | `/v1/chat/completions` e `/v1/messages` sem o segmento do dialeto, `GET /v1/models/{id}` |
| `gemini`, `azure` | todas (tasks 164 e 165, abertas) | — |

A tabela também se contradizia: listava `/v1/embeddings` enquanto "Fora de escopo" (spec 004 e 000) exclui embeddings. Fato novo: a Responses API é o caminho padrão do SDK `openai` para modelos novos e dos roteadores que já migraram (Codex, Agents SDK), e a `/openai/v1/responses` é rota de primeira classe da API v1 do Azure; o MVP não a tinha.

## Decisão
- **Entra no MVP:** `POST /v1/responses` no driver `openai` (contrato na spec 004, seção "Responses API", e [ADR 0055](0055-contrato-da-responses-api.md)); `POST /openai/v1/responses` no `azure` (herda o motor do `openai`, task 165 ou seguinte); `POST /api/v1/responses` no `openrouter` e `POST <base>/openai/v1/responses` no `opencode`, com as diferenças de cada gateway marcadas "não conferido" até a fixture; as rotas Gemini do `opencode` (`<base>/google/v1beta/models/{m}:generateContent` e `:streamGenerateContent`), que reaproveitam o motor do driver `gemini` (depois da 164).
- **Fora do MVP:** embeddings (`/v1/embeddings` e a rota por deployment do Azure), batches (`/v1/batches`, `/v1/messages/batches`), arquivos, `GET /api/v1/generation` e `GET /api/v1/models/{author}/{slug}/endpoints` do OpenRouter, e as rotas de recurso da Responses (`GET`/`DELETE /v1/responses/{id}`, `/cancel`, `/input_items`, `/compact`, `/v1/conversations`). Nenhuma delas é tráfego de inferência que um roteador encaminha sob carga.
- **Como se comportam as rotas fora do escopo:** como rota inexistente no provedor real (o 404 de rota desconhecida do driver, com o envelope e o texto do real quando conferidos). Isso é um limite de escopo, não uma opção de desvio: a ADR 0042 continua valendo para tudo que o wwz serve.
- **Rotas no código sem spec:** `GET /api/v1/auth/key` (OpenRouter) e as formas sem o segmento do dialeto do OpenCode (`<base>/v1/chat/completions`, `<base>/v1/messages`, `<base>/v1/models/{id}`) entram na tabela da spec 006 como existem no código, marcadas "não conferido" até a fixture mostrar que o real as atende; se o real as recusar, saem do código.
- A coluna "Fase" da tabela da spec 004 sai: o MVP é entregue inteiro (ADR 0001) e a coluna estava desatualizada.

## Alternativas consideradas
- Embeddings determinísticos por hash (texto antigo da spec 004): útil para RAG, mas fora do propósito (roteador de modelos de chat) e contraditório com a spec 000; fica para extensão.
- Batches com estado assíncrono: exigiria arquivos, fila e resultados de 24 h no store; nenhum roteador-alvo os encaminha.
- Responses só no `openai`: o real dos gateways também a serve; deixar de fora seria uma rota a menos no simulador, mas o motor é o mesmo e o custo é pequeno.

## Consequências
- Spec 004: tabela sem a coluna "Fase", sem embeddings e batches, com a seção "Responses API" e a lista "Fora do MVP". Spec 006: rotas do OpenRouter e do OpenCode auditadas. Spec 000 já exclui embeddings.
- Tarefas de código (criadas pelo orquestrador): Responses no `openai` (+ drivertest, SDK, sonda do drift); Responses no `openrouter` e no `opencode`; rotas Gemini do `opencode`; conferência das rotas extras do OpenRouter/OpenCode.

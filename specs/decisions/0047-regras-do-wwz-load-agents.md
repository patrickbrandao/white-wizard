# 0047 — Conversation id reproduzível e regras do `wwz-load agents`

Status: provisória
Data: 2026-10-31
Gatilho de revisão: o wwz ou o `wwz-tools` deixarem de misturar o id da conversa/sessão no sorteio, ou alguém precisar de ids distintos entre execuções com a mesma semente (por exemplo, duas execuções simultâneas contra o mesmo wwz com estado por conversa).

## Contexto
A spec 015 dizia que a parte `<run>` de `x-wwz-conversation-id` é sempre aleatória (8 hex por execução). O wwz e o executor de ferramentas (`internal/tools`, em processo ou `wwz-tools serve`) misturam o id da conversa/sessão no sorteio. Com `<run>` aleatório, a mesma `--seed` não reproduzia o relatório, e o critério de aceite "em processo e `--tools-url` dão o mesmo relatório de ferramentas para a mesma semente" não podia ser testado. A implementação (task 116) também precisou fixar pontos que a spec deixava abertos.

## Decisão
- Com `--seed N ≠ 0`, `<run>` = 8 hex derivados de N (`loadgen.Draw(N, 0x72756e, 2^32)`, com `0x72756e` = "run" em ASCII, função `agents.RunID`). A mesma semente dá os mesmos ids e as mesmas respostas, no wwz e nas ferramentas. Com `--seed 0`, `<run>` continua aleatório.
- Executor remoto (`--tools-url`): 200 = resultado; 504 = resultado `timeout` (`is_error`); 404 = `natural_error` (ferramenta desconhecida, mesma mensagem do executor em processo); outro status, corpo inválido ou falha de conexão encerram a sessão como `failed` com motivo `tools_unavailable`.
- `tool_errors` conta os resultados com `is_error` devolvidos ao modelo (`natural_error`, `error`, `timeout`); não conta `tools_unavailable`.
- Interrupção (`SIGINT`/`SIGTERM`) durante a espera de uma nova tentativa: a sessão fica `interrupted` e a última tentativa conta como a requisição final (erro em `errors`/`errors_by_type`).
- `--sessions` explícito com `--duration > 0` é recusado com código 2.
- `tool_choice` não é enviado (o padrão dos provedores já é `auto`); as ferramentas do perfil são declaradas sem ele.
- `loadgen.Result` guarda os headers `retry-after-ms` e `retry-after` brutos só nas respostas com status ≥ 400.

## Alternativas consideradas
- `<run>` sempre aleatório (texto anterior da spec): mesma semente não reproduz.
- Flag `--run-id`: mais uma flag para o mesmo efeito; o campo `RunID` da `agents.Config` continua disponível para testes.
- Enviar `tool_choice: "auto"` explícito: igual em efeito e diverge do que clientes reais costumam mandar.
- Repetir a chamada de ferramenta em erro do `wwz-tools`: mascararia a indisponibilidade do executor, que não é o alvo do teste.

## Consequências
- Spec 015, seção `wwz-load agents`, atualizada (conversation id, perfis, sessão, contadores, códigos de saída).
- Código já conforme (`internal/loadgen/agents`, `cmd/wwz-load/agents.go`, `internal/loadgen/client.go`).

# 0036 — Forma do `wwz-load agents` e do `wwz-load top`

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A [ADR 0015](0015-wwz-load-agents-e-top.md) fixa os subcomandos, mas não define os perfis, a política de retry, o relatório nem a biblioteca de terminal ("decisão do implementador, registrar se não for padrão"). A [ADR 0024](0024-percentis-e-cliente-do-wwz-load.md) diz que o `wwz-load` não ganha dependência nova, mas ler `q` sem Enter exige modo raw do terminal, que a stdlib não oferece.

## Decisão
- **`agents`** reutiliza o laço do modo conversa (`internal/loadgen`), com as ferramentas executadas por `internal/tools` (ADR 0034) em processo ou em `--tools-url`. Uma sessão tem `--turns` turnos de usuário; cada turno repete requisições enquanto vierem tool calls, até `--max-tool-rounds`. Perfis: `coder`, `pesquisa`, `chat` e `mixed` (rodízio por índice). Cada sessão manda `x-wwz-conversation-id` (I11). Com `--seed N ≠ 0`, manda também `x-wwz-seed: N + g` (g = índice global da sessão), como o modo conversa.
- **Retry**: `--retries` (padrão 2), backoff exponencial com jitter total e `retry-after-ms`/`retry-after` respeitados até `--retry-after-max`. Repete as classes transitórias (`http_408`, `http_429/*`, `http_5xx/*`, `connect`, `reset`, `timeout`, `stream_truncated`, `stream_error/*`), nunca os demais 4xx nem `parse`. `--retries 0` mede falhas sem mascarar (spec 017, ideias de teste).
- **Relatório**: JSON `version: 1` com `kind: "agents"`, por agente e total; CSV com uma linha por agente e a linha `total`. Mesmos códigos de saída do `run`.
- **`agents` sem `--instances`** na fase 4 (código 2).
- **`top`**: TUI sem framework, com `golang.org/x/term` (só modo raw e tamanho do terminal) e sequências ANSI. É um subrepositório oficial do Go e sua única dependência, `golang.org/x/sys`, já está no `go.mod`. Fica registrada como exceção à ADR 0024, restrita a essa dependência. Fora de TTY ou com `--once`, a saída sai sem ANSI (testável); `--once --output json` dá um quadro em JSON. Várias `--admin` somam os contadores; os percentis por modelo são o máximo entre instâncias.

## Alternativas consideradas
- `tcell`/`bubbletea`: árvores de dependências grandes para uma tela só.
- `stty` via `os/exec`: sem dependência, mas frágil e acoplado ao sistema.
- Retry sem respeitar `retry-after`: distorce exatamente o que o roteador sob teste precisa mostrar.

## Consequências
- A spec 015 ganha "Contrato de `agents` e `top` (fase 4)".
- O `go.mod` ganha `golang.org/x/term` (tarefa do `top`).

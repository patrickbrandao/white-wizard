# White Wizard (WWZ)

Servidor em Go que finge ser um provedor de LLM (OpenAI, Anthropic, OpenCode Go, OpenRouter) para testar roteadores de modelos sob carga de milhares a dezenas de milhões de requisições por minuto, sem GPU e sem custo de API. Devolve respostas inventadas no formato exato de cada provedor e simula latência, filas, erros, limites e bloqueios; acompanha o cliente de benchmark `wwz-load` e o painel `wwz-panel`.

## Fonte da verdade
As especificações em ./specs/ são a fonte da verdade. Toda mudança de comportamento começa pela spec.
Carregue cada spec sob demanda a partir do índice.

@specs/INDEX.md
@specs/PLAN.md
@specs/002-organizacao-do-trabalho.md

## Antes de propor mudanças
Consulte ./specs/decisions/ (comece pelo HISTORY.md). Não reabra uma decisão aceita sem um fato novo. Para mudá-la, proponha uma nova ADR.

## Fluxo de trabalho
- Pegue tarefas em ./tasks/ com status "aberta" e sem dependências pendentes.
- Marque a tarefa como "em-andamento" antes de começar.
- Trabalhe num worktree/branch `task/nnn-slug`; altere apenas os arquivos listados na tarefa e as pastas do seu papel.
- Ao concluir, atenda a Definition of Done, remova o arquivo da tarefa, faça o commit citando o ID (`tipo(área): resumo (task nnn)`) e o merge fast-forward na `main`.
- Pare e pergunte ao humano só nos casos de escalonamento de specs/002.

## Comandos
- Instalar: `go mod download`
- Rodar: `go run ./cmd/wwz` (a partir da fase 1); dependências locais: `docker compose up -d`
- Testar: `make verify-quick` (padrão); `make verify-full` e `make verify-heavy` só sob pedido. Enquanto o Makefile não existir (fase 1): `go build ./... && go vet ./... && go test ./...`
- Lint e formatação: `gofumpt -l -w .` e `golangci-lint run`

## Estrutura
- ./specs/: especificações (fonte da verdade)
- ./specs/decisions/: registro de decisões (ADRs) e HISTORY.md
- ./tasks/: fila de trabalho (fora do Git)
- ./tmp/: arquivos temporários (fora do Git)
- ./autor/: arquivos de implementação do autor (servidor de produção, notas e loop; fora do Git, spec 019)
- ./docs/: documentação derivada
- ./internal/: pacotes Go; ./cmd/: binários; ./scenarios/: cenários YAML prontos
- ./cache/valkey/, ./eventbus/valkey/: schema de chaves e eventos do Valkey
- ./deploy/: Prometheus, Grafana e templates do servidor pesado

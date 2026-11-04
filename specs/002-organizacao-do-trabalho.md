# 002 — Organização do trabalho

Status: aprovada
Depende de: 000, 001

## Contexto
Regras para vários agentes (Claude Code) trabalharem em paralelo sem conflito e sem reabrir decisões. Substitui o processo autônomo da seção 23 do documento original nos pontos indicados pela [ADR 0011](decisions/0011-fluxo-git-e-agentes.md).

## Requisitos
- RF-01: toda mudança de comportamento começa por uma alteração em `./specs/`.
- RF-02: nenhum agente pega tarefa `em-andamento`.
- RF-03: toda mudança vem com o teste que a cobre.

## Comportamento

### Papéis

| Papel | Arquivo | Responsabilidade | Pode alterar |
| --- | --- | --- | --- |
| Guardião das specs | `.claude/agents/guardiao-specs.md` | Mantém `./specs/`, ADRs, `openapi.yaml`, `asyncapi.yaml`; transforma mudança de spec em tarefas | `specs/`, `cmd/*/openapi.yaml`, `eventbus/`, `cache/*/README.md`, `tasks/` |
| Implementador | `.claude/agents/implementador.md` | Implementa uma tarefa numa área (server, drivers, store, cmd, panel, load, tools, drift) | Só os `arquivos` listados na tarefa, dentro de `internal/`, `cmd/`, `scenarios/` |
| Testador/revisor | `.claude/agents/testador.md` | Testes de contrato, SDK e cenários; roda `verify-full` sob pedido; revisa entregas contra a spec | `**/*_test.go`, `tests/`, `internal/driver/drivertest/` |
| Infra e CI | `.claude/agents/infra-ci.md` | Dockerfile, compose, Actions, `scripts/verify.sh`, Makefile, `deploy/` | `Dockerfile*`, `docker-compose.yml`, `cache/*/docker-compose.yml`, `deploy/`, `.github/`, `scripts/`, `Makefile`, `.golangci.yml` |

Qualquer papel pode criar tarefas em `./tasks/` para problemas encontrados por inspeção e atualizar `CHANGELOG.md` e `./docs/` da própria entrega.

### Quem altera o quê
- `./specs/` e `./specs/decisions/`: só o guardião das specs; outros papéis propõem por tarefa.
- Contratos (`openapi.yaml`, `asyncapi.yaml`): guardião das specs; o implementador ajusta o código ao contrato.
- Formato do YAML de configuração e headers `x-wwz-*`: contrato público; mudança incompatível escala ao humano.
- Não há migrations de banco versionadas: o SQLite do painel cria o schema no boot (`CREATE TABLE IF NOT EXISTS` + versão em `PRAGMA user_version`).

### Git
- Branch principal `main`; uma branch `task/nnn-slug` em worktree próprio por tarefa.
- Conventional Commits: `tipo(área): resumo (task nnn)`, ex.: `feat(driver/openrouter): envelope de erro (task 012)`.
- Antes do merge: `make verify-quick` verde, `git rebase main`, merge fast-forward na `main`. Sem PR obrigatório.
- Push para o remoto e tags de release: só com o humano (ver escalonamento).

### Fluxo das tarefas
1. Problema ou mudança de spec vira `tasks/nnn-titulo.md` (template da skill; pasta fora do git).
2. O agente marca `status: em-andamento` e preenche `responsavel` antes de começar.
3. Trabalha no worktree, só nos arquivos listados.
4. Ao concluir: Definition of Done, remove o arquivo da tarefa, commit citando o id, merge.
5. Tarefa descartada é removida; se o descarte for uma decisão, vira ADR.
6. Tarefas da fase seguinte do [PLAN](PLAN.md) são criadas pelo guardião quando a fase atual fecha.

### Paralelismo
- Tarefas sem dependência pendente e com `arquivos` disjuntos rodam em paralelo.
- Duas tarefas que tocam o mesmo arquivo são sequenciais; o guardião ajusta `depende-de`.

### Definition of Done
- [ ] Código implementado.
- [ ] Testes do nível acordado escritos e passando (`make verify-quick`; informar o nível rodado e o resultado).
- [ ] gofumpt e golangci-lint limpos.
- [ ] Spec e contratos atualizados, se mudaram.
- [ ] `./docs/` atualizado, quando aplicável.
- [ ] `CHANGELOG.md` com o resumo compactado da entrega em `[Não lançado]`.
- [ ] Arquivo da tarefa removido.

### Escalonamento para o humano
Durante a execução autônoma ([ADR 0012](decisions/0012-execucao-autonoma-noturna.md)) o agente **nunca para**: decide, registra ADR provisória e anota nas notas do autor (`autor/notas/MORNING.md`, fora do git; spec [019](019-exemplos-e-producao.md#arquivos-de-implementação-do-autor)) o que só o humano resolve. As notas não são fonte da verdade: o que for vital vai para as specs e ADRs (o não conferido no real para a spec 006, os passos do humano para a spec 018). As ações irreversíveis abaixo continuam proibidas ao loop. Fora do loop, o agente para e pergunta somente quando:
- a tarefa conflita com uma spec ou ADR aceita;
- precisa mudar de forma incompatível um contrato público (openapi, asyncapi, YAML de configuração, headers `x-wwz-*`);
- a ação é irreversível: apagar dados, reescrever histórico do git, push forçado, criar tag de release, publicar imagem versionada;
- precisa de um segredo (tokens, credenciais do servidor pesado, chaves reais de provedor);
- o usuário pediu "modo entrevista".

Em qualquer outro caso, decide, registra uma ADR provisória com o gatilho de revisão e segue.

## Interfaces e contratos
- Templates de tarefa, spec e ADR: ver [AGENTS.md](../AGENTS.md) e a skill `interview-to-specs`.

## Dados
- `./tasks/`, `./tmp/` e `./autor/` fora do git ([ADR 0076](decisions/0076-dados-do-autor-fora-do-git.md)).
- Nenhum arquivo versionado cita hash de commit nem dado do servidor do autor; cita a task, a data ou o conteúdo.

## Critérios de aceite
- [ ] Cada papel tem seu arquivo em `.claude/agents/`.
- [ ] Nenhum commit na `main` sem `verify-quick` verde.

## Fora de escopo
- Revisão humana obrigatória por PR.

## Decisões relacionadas
- [0002](decisions/0002-fonte-da-verdade-specs.md), [0008](decisions/0008-verificacao-e-ci.md), [0011](decisions/0011-fluxo-git-e-agentes.md), [0012](decisions/0012-execucao-autonoma-noturna.md), [0076](decisions/0076-dados-do-autor-fora-do-git.md)

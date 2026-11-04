# 0012 — Execução autônoma noturna

Status: aceita
Data: 2026-10-30
Atualizada por: [0076](0076-dados-do-autor-fora-do-git.md) (arquivos do loop e notas fora do git)

## Contexto
O MVP será implementado por um loop autônomo enquanto o humano dorme. O escalonamento da [ADR 0011](0011-fluxo-git-e-agentes.md) faria o loop parar à espera de resposta.

## Decisão
- Substitui a lista de escalonamento da ADR 0011: o loop **nunca para nem pergunta**.
- Diante de conflito com spec/ADR, contrato incompatível ou dúvida, o agente decide, registra uma ADR provisória (pelo guardião das specs) com o gatilho de revisão e segue.
- O que só o humano resolve (credenciais novas, push, compra, ação fora dos limites do servidor, tag de release) vai para as notas do autor (`autor/notas/MORNING.md`), sem bloquear o restante.
- Ações irreversíveis continuam proibidas ao loop: reescrever histórico do git, push (forçado ou não), tag de release, apagar dados fora de `tmp/` e `/opt/white-wizard`.
- Estado entre rodadas em `autor/notas/PROGRESS.md`; o loop termina ao criar `autor/notas/DONE.md`, quando o critério de saída da última fase do [PLAN](../PLAN.md) passa.
- Os arquivos do loop (watchdog, prompt de cada rodada, agente do launchd) e as notas são **arquivos de implementação do autor**, em `autor/loop/` e `autor/notas/`, fora do git ([ADR 0076](0076-dados-do-autor-fora-do-git.md), spec [019](../019-exemplos-e-producao.md#arquivos-de-implementação-do-autor)). O que for vital nas notas (o não conferido no real, passos de release, decisões) vai para as specs e ADRs; as notas não são fonte da verdade.

## Alternativas consideradas
- Manter o escalonamento da 0011 (o loop ficaria parado a noite toda).

## Consequências
- Mais ADRs provisórias para o humano revisar de manhã; o [HISTORY](HISTORY.md) marca cada uma como provisória.

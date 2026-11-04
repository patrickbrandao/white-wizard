# 0033 — Sementes intercaladas e parada do `wwz-load` distribuído

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
O protocolo controlador/worker (task 045, `internal/loadgen/dist`, `cmd/wwz-load`) dá ao worker `index` a semente `seed + index` e o worker manda `x-wwz-seed: seed + index + i` na requisição (ou conversa) local `i`. Fatos novos:
- As sementes dos workers se sobrepõem: a requisição 1 do worker 0 e a 0 do worker 1 levam a mesma semente, então o wwz gera a mesma resposta, latência e erro para elas. Com N workers cada semente aparece até N vezes, e a execução distribuída não reproduz a de uma máquina com o mesmo `--seed`, ao contrário do que a spec 015 promete ("i = índice global da requisição").
- Não há rota de parada: com o controlador interrompido (`SIGINT`) depois do início, os workers seguem até o fim do plano (para sempre com `--duration 0` sem `--requests`) e o controlador emite na hora um relatório sem nenhum worker (todos em `missing`, código 4).

## Decisão
- **Sementes intercaladas.** O worker `k` de `N` manda `x-wwz-seed: seed + k + N · i` na sua requisição (ou conversa) local `i`. Os conjuntos são disjuntos e a união é `seed + j` para j = 0, 1, …, como numa máquina só. Os sorteios locais (`draw_seed + k`) não mudam: o misturador (`loadgen.Draw`) já os separa.
- **Parada pelo controlador.** Sem rota nova: `GET /v1/status` ganha o estado `stopping`. Interrompido depois de `start_at`, o controlador passa a `stopping` e espera os resultados por até `--timeout + 30 s`. Durante o run, cada worker consulta `GET /v1/status` a cada 1 s; ao ver `stopping`, para como num `SIGINT` (para as partidas, espera as requisições em voo até `--timeout`) e envia o resultado parcial com `"interrupted": true`. Falha na consulta não para o worker (o controlador pode estar só lento). Relatório do controlador com `interrupted: true`; quem não respondeu entra em `missing`.

## Alternativas consideradas
- Semente com deslocamento largo por worker (`seed + k · 2^32 + i`): disjunta, mas a união não coincide com a execução numa máquina.
- `POST /v1/stop` empurrado aos workers: exige que o worker ouça numa porta, o que o protocolo evita (só o controlador escuta).
- Aceitar sem parada: um `--duration 0` interrompido deixaria workers rodando sem controle.

## Consequências
- Spec 015 atualizada (plano, estados, falhas).
- Tarefa: passo de semente no plano e sondagem/estado `stopping` em `internal/loadgen` e `cmd/wwz-load`.

# 0069 — Pacing de streams pelo fim planejado do chunk anterior, GOGC=200 e meta de 8 000 streams

Status: provisória (ADR 0012, loop autônomo de 2026-10-31; revisar nos gatilhos abaixo)
Data: 2026-10-31
Atualiza: [0067](0067-meta-de-streams-pelo-custo-medido.md), só no número de streams da rodada `streams` (9 000 passa a 8 000). Origem: task 378 (proposta em `tmp/spec-batch/378-adr.md`).

> Nota: o número de streams do item 4 (8 000) foi atualizado para 7 000 pela [ADR 0071](0071-overhead-em-malha-aberta-e-7000-streams.md) (task 409: 8 000 deram 82–84 % de CPU contando só as amostras sob carga).

## Contexto
A ADR 0067 previu como gatilho de revisão "a rodada com o wwz ≤ 80 % ainda ficar acima de 22 ms". Foi o que aconteceu (task 376): ITL p99 30,5 ms a 77 % de CPU. A task 378 mediu no servidor (amd64, bridge do Docker), separando servidor e cliente com tcpdump no bridge:

- **Sem throttling de CFS:** `nr_throttled` 0 com 4 + 4 CPUs e 4–8 períodos em 40 s com 12 + 14; `--cpuset-cpus` dá o mesmo ITL.
- **O servidor emitia em rajadas:** com 3 000 streams a 60 % de CPU, coeficiente de variação dos pacotes por ms de 0,71–0,76 (Poisson daria 0,08), em ciclos de ~20 ms (~14 ms emitindo, ~6 ms parado).
- **Causa:** cada espera era `agora + ITL`, contada depois da escrita. O atraso de agendamento passa a todos os chunks seguintes do stream, e streams periódicos com o período reiniciado depois do processamento se sincronizam (Floyd e Jacobson, 1993). A posição na rajada vira jitter do ITL.
- **Correção:** a espera termina em `fim planejado da anterior + ITL`. Com 3 000 streams o CV caiu a 0,43, sem ciclo.
- **O resto da cauda era GC:** o wwz-load decodificava cada chunk num `oaChunk` novo (2 alocações por chunk); o wwz coletava a cada ~2,5 s. Chunk reusado: cliente de 28,6 para 23,0 ms; `GOGC=200` no wwz: 21,8–21,9 ms.

| 9 000 streams, wwz 12 / load 14 CPUs | ITL p50 / p90 / p99 |
| --- | --- |
| antes (task 376) | 20,0 / 22,2 / 30,5 ms |
| pacing pelo fim planejado | 20,0 / 20,3 / 28,6 ms |
| + wwz-load sem alocação por chunk | 20,0 / 20,3 / 23,0 ms |
| + `GOGC=200` no wwz | 20,0 / 20,3 / 21,8 ms |

Rodadas finais (`tmp/heavy/`): 9 000 streams (`20261031T192026Z`) ITL p99 21,8 ms mas wwz a **86 %** de CPU, reprovado no critério de CPU; 8 000 streams (`20261031T192604Z`) ITL p99 21,8 ms, wwz a **80 %**, aprovado. Como o pacing agora entrega o ITL médio exatamente igual ao configurado (antes era maior), cada stream custa mais CPU que na medição da ADR 0067.

## Decisão
1. **Spec 008:** o pacing de um stream é contado do fim planejado da espera anterior (`marca + d`), não do fim da escrita. Atraso menor que `d` é descontado da espera seguinte; maior que `d` reinicia o relógio no instante atual (na saturação não há rajada de recuperação). Espera cancelada descarta a marca. O ITL médio é exatamente o configurado. Sem jitter artificial para quebrar a fase (`fixed` continua fixa).
2. **Spec 014:** `GOGC=200` vale de fato no wwz, aplicado pelo binário/imagem (não só pelo `deploy/heavy`), para a produção medir o mesmo que o heavy. Escolha: `ENV GOGC=200` no estágio `wwz` do Dockerfile (o `GOGC` do ambiente continua mandando; não muda o código Go).
3. **Spec 015:** o `wwz-load` não aloca por chunk no caminho OpenAI (decodificador reusa o chunk); a medida de ITL não pode depender do GC do próprio cliente. O caminho Anthropic ainda aloca por evento (follow-up, task 400).
4. **Meta do heavy `streams`: 8 000 streams** (≈ 400 mil chunks/s, ~8,2 núcleos de 12, ≤ 80 %), no lugar dos 9 000 da ADR 0067. Medido: 8 000 passou a 80 %, 9 000 deu 86 %. O limiar de CPU (80 %) e o de ITL (22 ms) não mudam; `HEAVY_STREAMS` padrão passa a 8000. A capacidade de 20 000 streams (`streams-max`, informativa) não muda.

## Alternativas consideradas
- Jitter no ITL: muda a distribuição configurada. Rejeitado.
- `cpuset` no lugar de `--cpus`: sem ganho medido.
- `GOGC` alto no wwz-load em vez de não alocar: esconde o custo e gasta memória.
- `GOGC=400` no wwz (ITL 21,7 ms, TTFT menor): mais memória; fica como opção do heavy.
- Subir o limiar de CPU para 90 % para manter 9 000: rejeitado (ADR 0067 fixou 80 %).
- Meta de 8 500 streams: sem medição; 8 000 é o ponto medido que passa.

## Consequências
- Margem pequena no ITL (21,8 contra 22 ms) e na CPU (80 % no limite): a rodada de 8 000 pode oscilar; se reprovar só por CPU, baixar `HEAVY_STREAMS` e registrar.
- Na saturação o comportamento não muda (o TTFT absorve a fila).
- Depois de um chunk atrasado o seguinte sai antes (intervalo < ITL).
- Tarefas: 399 (infra-ci: `ENV GOGC=200` e `HEAVY_STREAMS` 8000), 400 (load: decodificador Anthropic sem alocação).

## Gatilho de revisão
- O streams voltar acima de 22 ms com o wwz ≤ 80 %, ou 8 000 streams passarem de 80 % de CPU de forma recorrente.
- O padrão de GOGC do binário ficar diferente do heavy.
- Fato novo que baixe o custo por chunk (rede `host`, kernel), permitindo voltar a 9 000 ou mais.

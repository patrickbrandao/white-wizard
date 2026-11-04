# 0067 — Meta de streams pelo custo medido: ITL com o wwz ≤ 80 % de CPU

Status: provisória (ADR 0012, loop autônomo de 2026-10-31; revisar nos gatilhos abaixo)
Data: 2026-10-31
Substitui: a meta "≥ 20 000 streams simultâneos com ITL de 20 ms por instância de 8 vCPUs" da spec 014 como critério de ITL, e o passo `streams` da spec 018 (8 CPUs, 20 000 streams com ITL p99 ≤ 22 ms). Registra também a proposta da task 269 (nunca numerada).
Atualizada pela [ADR 0069](0069-pacing-pelo-fim-planejado-e-gogc.md) só no número de streams da rodada `streams` (9 000 → 8 000).

## Contexto
A spec 014 pedia, por instância de 8 vCPUs, ≥ 20 000 streams simultâneos com ITL de 20 ms, e o verify-heavy (spec 018, ADR 0037) cobrava ITL p99 ≤ 22 ms nessa carga. O servidor (ADR 0017) tem teto de 32 núcleos e 64 GB somados, contando a produção.

Fatos medidos (relatórios em `tmp/heavy/`):

| Medição | wwz | wwz-load | tokens/s | tokens/s/núcleo | ITL p50 / p90 / p99 | TTFT p99 | wwz ocupado |
| --- | --- | --- | --- | --- | --- | --- | --- |
| task 270 (20261031T162400Z) | 8 | 16 | ~488 mil | — | 20,0 / 20,5 / 29,2 ms | 1,99 s | 7,74/8 |
| task 269 (20261031T163204Z) | 12 | 14 | 548,6 mil | 47,9 mil | 20,0 / 20,4 / 28,9 ms | 1,50 s | 11,46/12 (96 %) |
| task 269 (20261031T163502Z) | 14 | 12 | 585,1 mil | — | 20,4 / 23,0 / 33,9 ms | 1,27 s | 13,97/14; load 11,97/12 saturado |
| task 341, escalonador por núcleo (20261031T180632Z) | 12 | 14 | 591,6 mil | 51,3 mil | 32,2 / 45,7 / 73,0 ms | 0,13 s | 11,54/12 |
| task 341, entrega final (20261031T181146Z) | 12 | 14 | 564,7 mil | 49,4 mil | 20,0 / 20,4 / 28,7 ms | 1,39 s | 11,43/12 (96 %) |

- **Mais núcleos não tiram o wwz da saturação** (task 269): o wwz-load roda em malha fechada; com mais CPU o TTFT cai, os streams terminam antes e a taxa sobe até o wwz voltar a 96–100 %. 20 000 streams a 20 ms custam ~1 M chunks/s enquanto abertos, qualquer que seja a forma da carga.
- **O kernel é o piso** (task 341): o `write` de cada chunk é 58–63 % da CPU do wwz; mesmo com custo zero fora do kernel, o servidor (amd64, bridge do Docker) não chega aos ~11,8 µs/chunk que 85 mil chunks/s/núcleo pediriam. O pacing exige uma escrita por chunk por conexão.
- **Escalonador próprio piora o que o roteador mede**: +7 % por núcleo, mas na saturação o atraso sai do TTFT e vai para o ITL (p99 73 ms).
- Com ~49 mil chunks/s/núcleo, 20 000 streams com ITL no plano pedem ~21 núcleos de wwz e outro tanto de wwz-load (~1 núcleo por 45 mil eventos/s): ~44 núcleos, acima do teto de 32.

## Decisão
1. **Custo de referência, não meta de vazão por núcleo.** A meta "≥ 85 mil chunks/s/núcleo" (saída (a) da proposta da 269) é abandonada. A spec 014 registra o custo de referência: **~49 mil chunks/s por núcleo** no servidor de verificação (amd64, rede bridge do Docker), com escala linear (núcleos ≈ chunks/s ÷ 49 mil).
2. **Meta de ITL fora da saturação.** ITL p99 ≤ 22 ms (20 ms + 2 ms de overhead) vale com o wwz a **≤ 80 % da CPU dada** (média da rodada). Na saturação o wwz atrasa o TTFT e mantém o ITL p50/p90 no plano; o p99 não é meta.
3. **Capacidade de conexões continua:** uma instância aceita e mantém ≥ 20 000 streams simultâneos sem erro; com ITL no plano isso pede ~21 núcleos.
4. **Modelo de execução mantido:** uma goroutine e um `time.Timer` reusado por stream; sem escalonador próprio de streams.
5. **Rodada `streams` do verify-heavy (opção (b) da 269):** wwz com 12 núcleos, wwz-load com 14, `wwz-admin` parado só durante a rodada (orçamento 6 + 12 + 14 = 32), `HEAVY_STREAMS` padrão **8 000** (≈ 400 mil chunks/s, ~80 %; eram 9 000 antes da [ADR 0069](0069-pacing-pelo-fim-planejado-e-gogc.md), que mediu 86 % de CPU com 9 000). Passa com erros < `HEAVY_MAX_ERROR_PCT`, `total.itl_ms.p99 ≤ 22` e `wwz_cpu_util_mean ≤ HEAVY_MAX_WWZ_CPU_PCT/100` (padrão 80); acima de 80 % a medida não vale e a rodada reprova (baixe `HEAVY_STREAMS`).
6. **Rodada `streams-max` (informativa):** mesma configuração com `HEAVY_STREAMS_MAX` (padrão 20 000) streams; grava TTFT, ITL, tokens/s, chunks/s por núcleo e CPU no `summary.json`, mas nunca reprova o verify (só falha se não conseguir rodar). Entra no `HEAVY_ROUNDS` padrão.
7. Afrouxar o limiar para 30 ms continua rejeitado.
8. A opção (c) da 269 (mais que 32 núcleos para o heavy, mudando a ADR 0017) fica com o humano (spec 018, "Passos do humano").

## Alternativas consideradas
- Escalonador por núcleo (heap por P, escrita não bloqueante; patch em `tmp/spec-batch/341-pacer.patch`): rejeitado (+7 %, ITL degrada na saturação).
- io_uring / envio em lote entre conexões: sem suporte na biblioteca padrão, dependência nova, ganho só na entrada do kernel. Rejeitado.
- Desligar `TCP_NODELAY`: o Nagle segura o chunk até o ACK atrasado (até 40 ms). Rejeitado.
- 14 núcleos para o wwz: o wwz-load (12) satura e o p99 piora (33,9 ms). Rejeitado.
- ITL p99 ≤ 30 ms: esconde regressão real. Rejeitado.
- GOGC alto: 45,6 mil contra 47,3 mil chunks/s/núcleo; não muda a conta.
- Rede `host` na rodada (sem veth/bridge/conntrack): não testada; experimento do infra-ci que pode disparar a revisão.

## Consequências
- Spec 014: RNF e meta da seção 11 reescritos; spec 018: passos `streams` e `streams-max`, variáveis `HEAVY_STREAMS` (9000), `HEAVY_STREAMS_MAX` (20000), `HEAVY_MAX_WWZ_CPU_PCT` (80), `HEAVY_STREAMS_WWZ_CPUS` (12), `HEAVY_STREAMS_LOAD_CPUS` (14), `HEAVY_PROD_PAUSE_STREAMS` (`wwz-admin`).
- A URL principal da produção (`wwz-admin`) fica fora do ar durante as rodadas de streams; o cleanup religa.
- Tarefa 376 (infra-ci): ajustar `scripts/heavy.sh` e `deploy/heavy/` e rodar até verde.
- Follow-ups de desempenho (não bloqueiam): obfuscator do OpenAI (~3–4 % por chunk), encoder de string (~2–4 %), leitura do wwz-load.

## Gatilho de revisão
- Rede `host`, outro kernel ou NIC baixar o custo por chunk a ponto de 20 000 streams caberem em 12 núcleos a ≤ 80 %.
- O Go ganhar envio em lote de sockets (io_uring) na biblioteca padrão.
- A rodada com o wwz ≤ 80 % ainda ficar acima de 22 ms (o gargalo deixou de ser CPU).
- O humano mudar o teto de 32 núcleos/64 GB da ADR 0017 (opção (c)) ou proibir parar o `wwz-admin` durante o heavy.

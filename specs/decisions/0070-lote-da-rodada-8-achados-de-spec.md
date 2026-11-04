# 0070 — Lote da rodada 8: faults de quadro, header de erro, registro cortado e memória dos streams

Status: provisória (ADR 0012, loop autônomo de 2026-10-31; revisar nos gatilhos abaixo)
Data: 2026-10-31
Origem: task 404 (achados das tasks 391 e 393). Princípio: [ADR 0042](0042-fidelidade-total-ao-provedor-real.md) (a forma de cada resposta é a do real; os recursos de caos são extras explícitos).

## Decisões
1. **`duplicate_frames` vale nas duas listas.** É falha só de stream. Em `errors.types` (I19) o sorteio vira falha de meio de stream, sem efeito numa resposta sem stream; em `mid_stream_types` age direto. A spec 009 passa a listar o tipo na tabela do meio do stream. Mantém o código atual (`StreamOnlyTypes` e `midKinds`).
2. **A validação é uma só.** `chaos.ValidateProfile` (admin, `POST /admin/chaos`) aceita em `types` o mesmo conjunto da validação da config: `KnownType` ou tipo de quadro. O admin não pode recusar o que o YAML aceita.
3. **`x-wwz-return-error` segue a tabela de tipos.** O nome é um tipo de erro e a resposta tem a forma do tipo naquele driver. `content_filter` devolve o 200 com `finish_reason: content_filter` / `stop_reason: refusal` (como `errors.types`, task 393), não o 400. O 400 `content_filter` do Azure (filtro de prompt, spec 006) continua sendo a forma do Azure. Motivo: o real não tem erro 400 de filtro de saída na OpenAI nem na Anthropic.
4. **O registro da Responses guarda a saída entregue.** Com `finish_length` os tokens do registro (`toks`) são os da saída cortada, porque `previous_response_id` soma o que o cliente viu. Nas falhas que abortam o stream (truncado, reset, erro, hang sem retomada) o real não foi conferido; o wwz grava o registro só quando a resposta termina (`completed`/`incomplete`), e isso fica "não conferido".
5. **Limiar de memória: 2 048 MiB por 10 000 streams.** Medido 1,287 GiB com 8 000 streams (`tmp/heavy/20261031T192604Z`), ~1,61 GiB por 10 000; margem ~27 %. `HEAVY_MAX_MEM_PER_10K_MB` padrão `2048` (spec 018), meta na spec 014. A spec 018 também corrige `HEAVY_STREAMS` para `8000` (ADR 0069; o texto ainda dizia 9000).
6. **Overhead p99 dos streams = ITL p99 − 20 ms, < 2 ms.** Confirmado nas specs 014 e 018 (é o mesmo critério de `HEAVY_MAX_ITL_P99_MS = 22`).

## Consequências
Código: tarefas 405 (engine: header e registro cortado), 406 (chaos: validação do admin) e 407 (infra-ci: limiar de memória no heavy).

## Gatilhos de revisão
- Conferir no real se uma resposta interrompida fica consultável por `previous_response_id`.
- A memória por stream passar de ~165 KiB depois de mudanças no pacing ou no buffer.

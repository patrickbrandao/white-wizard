# 0035 — `wwz-drift check`: comparação de forma ao vivo, só na máquina do humano

Status: aceita (orçamento US$ 20 pela 0042) (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
I24 (spec 017) pede um "job agendado que compara os dialetos dos drivers com as APIs reais". A [ADR 0027](0027-formas-finais-dos-recursos-importados.md) põe `cmd/wwz-drift` na fase 4 e `record` + fixtures na fase 5 ([ADR 0014](0014-fidelidade-bit-a-bit.md)). Fatos novos que impedem uma tarefa concreta:
- A spec 018 põe o drift no `verify-heavy`, que roda **no servidor**, mas a ADR 0014 proíbe chaves reais no servidor.
- Não há definição de "divergência" antes das fixtures (a comparação byte a byte só existe na fase 5), nem um agendamento compatível com a [ADR 0018](0018-publicacao-local.md), que proíbe o remoto.

## Decisão
- **Fase 4 = `wwz-drift check`**: para cada provedor com chave, envia as mesmas sondas ao provedor real e ao wwz: lista de modelos, chat mínimo com e sem stream, erro de autenticação e modelo inexistente. Compara a **assinatura de forma** (caminhos JSON com tipo e ordem, sequência de eventos SSE, nomes de headers relevantes, status), nunca os valores. Detalhe na spec 017.
- **Onde roda**: só na máquina do humano (Mac) ou no CI com secrets. As chaves vêm de `~/.config/white-wizard/real-keys.env` ou do ambiente. O passo `drift` do `verify-heavy` executa localmente, não no servidor, e fica "pulado" sem o arquivo de chaves.
- **Agendamento**: `.github/workflows/drift.yml` (cron diário) pronto e inativo até o humano criar o remoto e os secrets (ADR 0018). Sem launchd.
- **wwz comparado**: em processo (servidor embutido com configuração interna) por padrão, ou `--wwz URL`.
- **Orçamento**: o mesmo `tmp/real-spend.json` e o teto de US$ 20 (ADR 0014, elevado pela 0042). As sondas usam `max_tokens: 16`.
- **Códigos de saída**: `0` sem divergência, `1` falha de execução, `2` uso inválido, `3` divergência encontrada (vence o `1`).
- `wwz-drift record` sai com código 2 e "não implementado" até a fase 5.

## Alternativas consideradas
- Drift dentro do servidor pesado: viola a ADR 0014.
- Esperar as fixtures da fase 5 para ter qualquer drift: deixa I24 sem entrega na fase 4.
- Comparar valores (texto, contagens): sempre diverge, porque o simulador inventa o conteúdo.

## Consequências
- Specs 017 e 018 atualizadas: contrato do `wwz-drift` e passo `drift` local no heavy.
- Tarefas: `internal/drift`, `cmd/wwz-drift` e `drift.yml`.

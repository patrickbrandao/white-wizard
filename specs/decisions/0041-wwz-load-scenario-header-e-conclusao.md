# 0041 — `wwz-load --scenario`: header padrão, conversa concluída e raciocínio oculto

Status: provisória (revisar se um roteador sob teste precisar de outro meio de seleção como padrão, ex. `metadata.scenario` ou sufixo `-scenario-`, ou se as fixtures reais da fase 5, ADR 0014, mostrarem outra forma de contar o raciocínio oculto)
Data: 2026-10-31

## Contexto
A verificação da saída da fase 3 (task 055) trouxe três fatos novos sobre o modo conversa da spec 015:

1. `wwz-load --scenario scenarios/file-agent-flow.yaml` sem `--header 'x-wwz-scenario: file-agent-flow'` não aplica o cenário. O `match.tools_include` não basta quando o modelo do YAML não lista o cenário em `scenarios`, e a ordem de seleção da spec 011 (`header > metadata > marker > model_suffix > match`) cai no `fallback`. O resultado foram conversas de 1 turno e 71 `order_violations`, e o relatório ainda deu 50 conversas "concluídas".
2. Uma conversa que termina com `stop` no primeiro turno conta como `completed`, mesmo quando o passo esperado pedia tool calls. Por isso o relatório esconde o fato de que o cenário não rodou.
3. `reasoning_missing` conta falso contra o driver `openai` no padrão `reasoning_field: hidden` (ADR 0022). Nesse modo o texto do raciocínio nunca vem, e só `usage.completion_tokens_details.reasoning_tokens` mostra que ele existiu. O mesmo vale para o OpenRouter com `reasoning.exclude: true`.

## Decisão
- **Header padrão.** Com `--scenario`, toda requisição de todos os turnos leva `x-wwz-scenario: <name do YAML>`. É o primeiro critério de `scenarios.select`, então funciona sem depender de `match` nem da lista `scenarios` do modelo. Um `--header` explícito com o mesmo nome (sem diferenciar maiúsculas) vence. Valor vazio (`--header 'x-wwz-scenario:'`) suprime o envio, para testar a seleção por `match`, `metadata`, marcador ou sufixo através do roteador. `config.headers` lista `x-wwz-scenario` quando o header é enviado.
- **Aviso de cenário não aplicado.** `conversations.scenario_not_applied` conta as respostas 2xx sem `x-wwz-scenario` ou com valor diferente do `name`. Quando é > 0, a saída texto ganha uma linha de aviso. Isso não muda o código de saída, porque um roteador pode remover headers de resposta.
- **Conversa concluída.** Uma conversa só conta como `completed` quando a resposta final, sem tool calls e com `finish_reason` final, cai num passo terminal. Passo terminal é um passo sem `tool_calls` declaradas. O passo vem do `x-wwz-step` da resposta. Sem esse header, vem da previsão do cliente (a mesma que confere `order_violations`). Se o passo pedia tool calls, ou se o `x-wwz-step` não existe no cenário, a conversa conta como `failed` com o motivo `incomplete`. Quando o passo não é conhecido (sem header e sem previsão), a conversa conta como concluída, como hoje.
- **Raciocínio oculto.** O raciocínio conta como presente quando vem texto de raciocínio (`reasoning_content`/`reasoning`, blocos `thinking` ou `redacted_thinking`) **ou** quando `reasoning_tokens` do `usage` da resposta é > 0. `reasoning_missing` só conta quando nenhum dos dois aparece.

## Alternativas consideradas
- **Exigir `--header` do usuário.** Mantém o cliente simples, mas foi exatamente o erro que a verificação encontrou. O padrão deve funcionar.
- **Selecionar por sufixo de modelo (`<modelo>-scenario-<nome>`).** Muda o `model` que o roteador vê e quebra roteadores que validam o nome do modelo. O header é transparente para quem só repassa.
- **Concluída = número de turnos ≥ número de passos do YAML.** Cenários têm ramos (`edit-failed`, `give-up`) e passos repetidos, então a contagem de passos não mede o caminho. O passo terminal mede.
- **Falhar a execução (código ≠ 0) quando o cenário não é aplicado.** Seria rígido demais atrás de roteadores que removem headers. O aviso e o motivo `incomplete` já tornam o problema visível.

## Consequências
- Spec 015: tabela de flags, modo conversa (header, `incomplete`, `scenario_not_applied`, `reasoning_missing`), saída texto e critérios de aceite.
- `wwz-load agents` (fase 4) herda o laço e as mesmas regras.
- Tarefa de implementador (load): `internal/loadgen/conversation.go` e `cmd/wwz-load/run.go`, com testes.

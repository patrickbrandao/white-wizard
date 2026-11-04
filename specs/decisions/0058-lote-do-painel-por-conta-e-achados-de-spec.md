# 0058 — Painel por conta e lote de achados de spec

Status: provisória (revisar se o real aplicar o limite novo na hora ao mudar de tier e a diferença aparecer em fixture; ou se um usuário precisar de `slow_headers` mais curto que o padrão)
Data: 2026-10-31

## Contexto
A task 192 (painel por conta) e as tasks 190, 191, 196, 204 e 237 deixaram divergências entre código e specs, listadas na task 247. Decide-se o que muda de comportamento pelo princípio da [ADR 0042](0042-fidelidade-total-ao-provedor-real.md): igual ao real; sem como conferir, "não conferido".

## Decisão
- **Compra é da conta.** `POST /admin/entities/key/{id}/credits` com `add_usd` credita a conta da chave (saldo e `purchased`), avalia `promote_when` dela e responde com o `EntityState` da conta, como `.../tier` (chave desconhecida ou sem conta → 400). Em todo provedor real os créditos são da organização, do workspace ou da conta, nunca da chave. `usd` (definir saldo) continua agindo na entidade nomeada. Tarefa de código derivada (admin).
- **Balde ao mudar de tier.** Mantém-se o comportamento atual: tokens atuais preservados, capacidade e taxa novas na próxima avaliação (`lib.lua` `bucket`, corte em `cap`). Se o real aplica o limite novo na hora não se confere sem uma conta real que mude de tier (acima do teto de US$ 20); fica "não conferido" na spec 006, seção "Não conferidos no real". Sem tarefa de código.
- **`slow_headers` configurável.** Nova chave `errors.slow_headers_delay` (duração Go, padrão 15 s, 0 = padrão, negativo inválido) ao lado de `timeout_delay` e `hang_duration`. É falha de transporte injetada, sem equivalente no real; o teste de timeout de conexão precisa de um atraso maior ou menor que o do cliente, e 15 s fixos (`server.Options.HangFor`) não o permitem. Tarefa de código derivada (config, chaos, engine/server).
- **Capacidade da LRU de respostas (`Memory.SetResponseCapacity`).** Não vira opção de YAML: é proteção interna do store em memória (1 000 000 registros, ADR 0055), sem equivalente no real (retenção de 30 dias). Só testes mudam o valor.
- **Texto das specs alinhado ao código**, sem mudança de comportamento: `retry-after` das janelas de outage só em 429/503/529 (como o real); com `server.seed ≠ 0` o sorteio depende do id da conversa e do número de mensagens, então requisições idênticas têm o mesmo destino; `tier:<nome>` das políticas carrega só limites e o tier do YAML mantém `promote_when`/`next`; `ent:*` não é gravado na configuração efetiva (`GET /admin/config` não mostra a atribuição; o `tier` está no `EntityState`); `AddCredits` no Valkey é pipeline, não `MULTI`; `local_first` soma as requisições não sincronizadas ao uso reportado a `promote_when`; `tokenizer: auto` mapeia todo `gpt-4-*` para `cl100k_base`, e só a sonda de 13 tokens da OpenAI está conferida.

## Alternativas consideradas
- Promover a conta quando `add_usd` cai numa chave sem resolver para a conta: deixa saldo na chave, que o limite `credits_usd` do tier (que conta na conta) nunca vê.
- Manter `slow_headers` fixo e só corrigir o texto: perde o objetivo do tipo (testar timeout de conexão contra um cliente configurável).
- Encher o balde ao subir de tier: inventa um comportamento que ninguém conferiu no real.

## Consequências
- Contratos: `cmd/wwz-panel/openapi.yaml` (`CreditsRequest` com `add_usd`, `purchased_usd`, `account_id`, `effective_tier`, tier por chave) e `cmd/wwz/openapi.yaml` (descrição de `setCredits`); specs 003, 007, 009, 010, 012, 013, 016 e `cache/valkey/README.md`.
- Tarefas de código: (1) admin: `add_usd` com `tipo = key` resolve para a conta; (2) config/chaos/server: `errors.slow_headers_delay`.

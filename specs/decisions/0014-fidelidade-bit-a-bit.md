# 0014 — Fidelidade bit a bit com fixtures reais

Status: aceita (orçamento elevado a US$ 20 pela 0042) (revisão com o humano em 2026-10-31)
Data: 2026-10-30

## Contexto
A fidelidade aos provedores ([006](../006-fidelidade-provedores.md)) era descrita a partir da documentação. O roteador sob teste precisa receber exatamente o que o provedor real envia.

## Decisão
- Fixtures reais gravadas com chaves do humano em `testdata/real/<provedor>/` (openai, anthropic, openrouter, opencode): headers, envelopes de erro, sequência e formato exato dos eventos SSE (incluindo linhas `event:`, comentários, keep-alives e espaçamento), `/models`.
- Valores voláteis (ids, timestamps, conteúdo gerado, contagens de tokens, ids de requisição, datas de reset) são normalizados para marcadores antes de gravar.
- O teste compara byte a byte a forma serializada do wwz contra a fixture normalizada: campo a campo e na ordem dos campos, mesma ordem de eventos.
- Chaves lidas só de `~/.config/white-wizard/real-keys.env`, carregadas em memória/ambiente; nunca no repositório, logs, commits ou servidor.
- Orçamento máximo de US$ 20 por provedor ([0042](0042-fidelidade-total-ao-provedor-real.md); era US$ 5), rastreado em `tmp/real-spend.json`; atingido o teto, para de chamar aquele provedor.
- A gravação é uma ferramenta (`wwz-drift record`), não roda no `verify-quick`; a comparação contra as fixtures já gravadas roda no `verify-quick` quando o driver muda.

## Alternativas consideradas
- Só documentação dos provedores; comparação estrutural (chaves sem ordem).

## Consequências
- Fixtures normalizadas entram no git; respostas brutas ficam em `tmp/real/` (fora do git).
- Mudança no provedor real aparece como divergência no `wwz-drift` (I24).

## Nota (task 181, 2026-10-31)
Sem mudança da decisão. O primeiro conjunto gravado (task 161) é o de validação, em `testdata/real/<provedor>/validation/*.json`, com o formato descrito na spec 018 ("Fixtures reais"): marcadores `<key>`, `<session>`, `<request_id>` e `<user_id>`; requisição aceita guarda só `status: 200`. Os gateways (`openrouter`, `opencode`) já comparam byte a byte; `openai` e `anthropic` comparam por campos e valores enquanto a serialização do envelope não for igual à do real (a OpenAI indenta com 4 espaços e termina em `\n`), o que é dívida contra esta ADR, não exceção a ela. Exemplo de caminho real da Anthropic para campo desconhecido num bloco de texto: `messages.0.content.0.text.foo` (o tipo do bloco entra no caminho).

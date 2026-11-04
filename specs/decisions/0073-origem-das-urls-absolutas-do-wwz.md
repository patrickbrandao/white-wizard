# 0073 — Origem das URLs absolutas que apontam para o próprio wwz

Status: provisória (ADR 0012; gatilhos no fim)
Data: 2026-11-03
Atualiza: [0072](0072-recursos-do-opencode-ai-no-driver-opencode.md), item 16 (montagem do `base_url`) e item 17 (`verification_uri_complete`). Origem: task 434, sobre o achado da task 423.

## Contexto
O real devolve URLs absolutas de si mesmo em dois lugares que o wwz imita: o `api.<protocolo>.base_url` da listagem do `opencode` (`https://opencode.ai/inference/...`) e o `verification_uri_complete` do Console (`https://opencode.ai/...`). O wwz troca `https://opencode.ai` pela própria origem, para o cliente que segue a URL cair de volta no wwz.

A task 423 montou essa origem no driver, a partir de `Host`, `X-Forwarded-Host`, `X-Forwarded-Proto` e `X-Forwarded-Prefix`, e a task 424 repete a conta no Console. Três falhas:
1. Falta o prefixo de montagem do driver (`drivers[].prefix`). Na produção, o `opencode` do `wwz-admin` está em `/opencode` (`deploy/prod/config/todos.yaml`), então o `base_url` sai `https://wwz.…/inference/go/openai/v1`, que é uma rota inexistente, em vez de `https://wwz.…/opencode/inference/go/openai/v1`.
2. O esquema sai `http` quando o próprio wwz serve TLS (`server.tls`) sem proxy na frente.
3. Os `X-Forwarded-*` valem vindos de qualquer cliente. O `X-Forwarded-For`, por outro lado, só vale de `server.trusted_proxies` (spec 003).

O driver não tem como corrigir sozinho: o prefixo de montagem, o TLS da conexão e o par TCP só existem no servidor (`internal/server`).

## Decisão
1. **Origem.** Toda URL absoluta que o real devolve apontando para a própria API ou para o próprio site que o wwz imita sai com a origem do wwz: `<esquema>://<host><prefixo externo><prefixo de montagem>`, seguida do caminho relativo ao mount do driver.
   - **Esquema:** com o par TCP em `server.trusted_proxies`, o primeiro valor de `X-Forwarded-Proto`, se for `https` ou `http` (sem distinção de caixa). Sem isso, `https` quando a conexão chegou por TLS (`server.tls`) e `http` nos demais casos.
   - **Host:** com o par confiável, o primeiro valor de `X-Forwarded-Host`; senão, `Host`. Sem nenhum dos dois, a origem fica sem esquema e host, e a URL sai relativa à raiz: `<prefixo externo><prefixo de montagem><caminho>`.
   - **Prefixo externo:** com o par confiável, o primeiro valor de `X-Forwarded-Prefix`, se começar com `/`, sem a `/` final. Sem isso, vazio. É o caminho que um proxy com strip-prefix tirou antes de repassar.
   - **Prefixo de montagem:** `drivers[].prefix` normalizado (sem `/` final; vazio na raiz).
   - Fora de `trusted_proxies`, os três `X-Forwarded-*` são ignorados, como o `X-Forwarded-For`. `Forwarded` (RFC 7239) não é lido.
2. **Quem calcula.** O servidor calcula a origem uma vez por requisição, depois do roteamento, e a entrega ao driver num header interno que o cliente não forja (prefixo `\x00`, como `driver.QueryHeader`). O driver lê a origem por `driver.Origin(req)` e não lê `X-Forwarded-*` nem `Host` por conta própria. Fora do servidor (teste de driver sem o header), a origem é vazia e a URL sai relativa.
3. **Alcance.** A regra vale para todo driver. Hoje só o `opencode` devolve URLs desse tipo: o `base_url` da listagem (`<origem>/inference[/go]/<família>`, spec 006, 16.7) e o `verification_uri_complete` do Console (`<origem>/console/device?code=<user_code>`, spec 004). Não entram:
   - links de documentação e de site dentro de textos de erro reais (`https://platform.openai.com/...`, `https://opencode.ai/go` no `FreeUsageLimitError`, `https://openrouter.ai/settings/credits`…). São texto conferido do provedor, não endereço da API imitada, e continuam iguais ao real;
   - URLs de recursos que o wwz não serve (arquivos, imagens), que seguem fora do MVP ([ADR 0054](0054-escopo-de-rotas-do-mvp.md)).
   Um driver novo que devolva URL da própria API usa `driver.Origin` (spec 005).

**Contrato.** Nenhum campo YAML, header `x-wwz-*` ou rota muda. Muda o comportamento de duas formas, que vão ao CHANGELOG como correção:
- o `base_url` e o `verification_uri_complete` passam a levar o prefixo de montagem e o esquema TLS;
- quem manda `X-Forwarded-*` direto, sem proxy em `trusted_proxies`, deixa de mudar a URL.
A produção já declara `trusted_proxies` com as faixas privadas, e o Traefik manda `X-Forwarded-Proto` e `X-Forwarded-Host`.

## Consequências
- Task 436: o servidor calcula a origem e o driver `opencode` passa a usá-la na listagem e no Console. Depende da 424 (`internal/driver/driver.go` e o Console) e da 435 (`internal/driver/opencode/models.go`).
- Specs 003 (`trusted_proxies`), 004 (seção "URLs do próprio wwz" e tabela do Console), 005 (mount) e 006 (16.7) atualizadas nesta tarefa (434).

## Alternativas descartadas
- **O driver lê o prefixo da configuração.** O driver não conhece a própria montagem, nem o TLS e o par TCP da conexão. Pôr isso em cada driver repetiria a regra de confiança do `clientip`.
- **`X-Forwarded-*` de qualquer cliente**, como na task 423. Destoa do `X-Forwarded-For` e deixa um cliente mudar a URL que o wwz anuncia. O risco é pequeno, porque a URL volta para quem mandou o header, mas a regra única de confiança é mais simples de explicar e testar.
- **Opção `server.public_url` fixa.** Com vários hosts (o `wwz-admin` responde por HTTPS no Traefik e por IP nas portas do haproxy), uma URL fixa erra num dos dois. O que a requisição traz é suficiente.
- **Manter `https://opencode.ai` como o real.** O cliente oficial segue o `base_url` da listagem, e com a URL real sairia do wwz para o provedor de verdade.

## Gatilhos de revisão
- Um cliente sob teste que dependa de `Forwarded` (RFC 7239).
- Um driver que precise devolver URL de recurso servido pelo wwz (arquivos, imagens).
- O `wwz-admin` passar a ficar atrás de um proxy fora das faixas privadas.

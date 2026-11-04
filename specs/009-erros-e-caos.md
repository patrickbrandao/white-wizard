# 009 — Simulação de erros e caos

Status: rascunho
Depende de: 004, 005
Origem: seções 7 do documento Claude Docs congelado em 2026-10-30

## Contexto
Erros pré-resposta, mid-stream, transporte, escopo por teste e headers de retry (seção 7).

## Requisitos
- R9, R20, R21
- I1, I2, I17, I19

## Interfaces e contratos
- `internal/chaos`
- `POST /admin/chaos` em [013](013-observabilidade-admin.md)

## Dados
- Estado one-shot por `X-Test-Id` no store

## Critérios de aceite
- [ ] Cada tipo de erro listado é reproduzível por taxa, header e admin
- [ ] Dois testes com `X-Test-Id` distintos não interferem
- [ ] Cada header `x-wwz-chaos-*` altera só a requisição que o traz e é ignorado com `server.request_headers: false`
- [ ] `errors.outages` com janelas em segundos, minutos e horas derruba o alvo só dentro das janelas, com `retry-after` até o fim da janela nos tipos que o enviam (429, 503, 529)
- [ ] `slow_headers` espera `slow_headers_delay` (padrão 15 s) antes dos cabeçalhos; com `server.seed ≠ 0`, requisições idênticas sorteiam o mesmo erro
- [ ] Janelas da raiz, do modelo, da chave e do perfil de `x-wwz-chaos-profile` se somam; o mesmo perfil em dois níveis conta uma vez; na sobreposição vale a primeira na ordem, com o `retry-after` dela
- [ ] `drivers` restringe a janela aos drivers listados; admin e passo de cenário vencem a janela; a janela vence o sorteio do perfil; `rate` da janela que erra devolve o comportamento normal
- [ ] Cron: todas as formas de campo aceitas, regra dia do mês OU dia da semana, horário inexistente na mudança de horário não dispara

## Fora de escopo
- Falhas de rede abaixo do TCP

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0026-precedencia-chave-sobre-modelo.md](decisions/0026-precedencia-chave-sobre-modelo.md)
- [specs/decisions/0027-formas-finais-dos-recursos-importados.md](decisions/0027-formas-finais-dos-recursos-importados.md)

## Comportamento

### 7. Simulação de erros e caos (R9)

Erros são sorteados por requisição contra `errors.rate` (0–100) e, quando o sorteio passa, um tipo é escolhido da lista `types` com pesos; erros no meio do stream têm sorteio separado (`mid_stream_rate`). Cada erro sai no formato exato do provedor (`{"error": {"message", "type", "code", "param"}}` na OpenAI; `{"type": "error", "error": {"type", "message"}}` na Anthropic).

**Erros antes do primeiro byte**

| Tipo | OpenAI | Anthropic | Observação |
| --- | --- | --- | --- |
| `invalid_request` | 400 `invalid_request_error` | 400 `invalid_request_error` | Mensagem genérica de parâmetro inválido |
| `context_length` | 400 `context_length_exceeded` | 400 `invalid_request_error` (prompt too long) | Também disparado deterministicamente quando a entrada excede `context_window` |
| `invalid_api_key` | 401 `invalid_api_key` | 401 `authentication_error` |  |
| `permission` | 403 `permission_denied` | 403 `permission_error` |  |
| `model_not_found` | 404 `model_not_found` | 404 `not_found_error` |  |
| `request_too_large` | 413 | 413 `request_too_large` |  |
| `rate_limit` | 429 `rate_limit_exceeded` + `retry-after` | 429 `rate_limit_error` + `retry-after` | Diferente do limite real (seção 8): aqui é aleatório |
| `quota` | 429 `insufficient_quota` | 429 (billing) | Testa se o roteador distingue cota de rate limit |
| `server_error` | 500 `server_error` | 500 `api_error` |  |
| `bad_gateway` | 502 | 502 | Corpo HTML, não JSON (como um proxy real devolveria) |
| `overloaded` | 503 `service_unavailable` | 529 `overloaded_error` | A Anthropic usa 529, o roteador precisa tratar |
| `gateway_timeout` | 504 | 504 | Após `timeout_delay` (padrão 30 s) |
| `content_filter` | 200 com `finish_reason: content_filter` | 200 com `stop_reason: refusal` | Resposta "bem-sucedida" que o cliente deve tratar |
| `connection_reset` | TCP RST antes da resposta | idem | Testa retry de transporte |
| `slow_headers` | Cabeçalhos só após `slow_headers_delay` (padrão 15 s) | idem | Testa timeout de conexão vs. de leitura. Falha de transporte, não existe no provedor real: o atraso é do perfil ([ADR 0058](decisions/0058-lote-do-painel-por-conta-e-achados-de-spec.md)); o código atual usa 15 s fixos até a tarefa de código |

**Erros no meio do stream**

| Tipo | Comportamento |
| --- | --- |
| `stream_error_event` | Após N chunks (`after_chunks` ou fração `after_ratio`), envia evento `error` (Anthropic) ou chunk `{"error": …}` (OpenAI) e fecha |
| `stream_truncated` | Fecha a conexão sem `[DONE]`/`message_stop` |
| `stream_hang` | Para de enviar por `hang_duration` sem fechar; opcionalmente retoma |
| `stream_malformed` | Injeta um chunk com JSON inválido, `data:` sem corpo ou evento SSE duplicado |
| `duplicate_frames` | Repete quadros SSE já enviados (I19). Falha só de stream: vale em `types` e em `mid_stream_types`, e nas duas listas age sobre os quadros do stream (em `types`, sorteado como erro de antes do primeiro byte, vira falha de meio de stream; resposta sem stream passa intacta) ([ADR 0070](decisions/0070-lote-da-rodada-8-achados-de-spec.md)) |
| `stream_reset` | TCP RST no meio do stream |
| `stream_slow_tail` | Os últimos 10 % dos tokens com ITL 20× maior |
| `finish_length` | Corta em `length` antes do alvo, sem erro. O que o cliente recebeu é o que vale: o registro da Responses API guarda os tokens da saída cortada, não da inteira (spec 012) |
| `usage_missing` | Omite `usage` no último chunk |

**Nomes e pesos válidos.** Os nomes das duas tabelas são os únicos aceitos em `types` (tabela de antes do primeiro byte, as falhas de transporte e os tipos de quadro do I19) e em `mid_stream_types` (tabela do meio do stream); outro nome torna a configuração inválida com o caminho (`config: profiles.errors.flaky.types[1]: unknown error type "x"`), e os headers `x-wwz-chaos-types`/`x-wwz-chaos-mid-stream-types` descartam em silêncio os nomes desconhecidos (sobram só os válidos; nenhum válido = o header não age). `weights` só aceita chaves presentes em `types` (ou na lista padrão: `rate_limit`, `server_error`, `overloaded`, `bad_gateway`, `gateway_timeout`, `invalid_request`, `model_not_found`, `context_length`) com peso > 0 e finito; tipo sem peso pesa 1 ([ADR 0059](decisions/0059-lote-da-rodada-5-achados-de-spec.md); regra completa na spec 003).

**Escopo e determinismo.** Erros podem ser configurados globalmente, por modelo, por chave e por cenário (um passo pode dizer `error: rate_limit`).

**Camadas (I2, [ADR 0027](decisions/0027-formas-finais-dos-recursos-importados.md)).** Da mais fraca à mais forte: perfil global → modelo → chave ([ADR 0026](decisions/0026-precedencia-chave-sobre-modelo.md)) → passo de cenário → headers `x-wwz-chaos-*` → eventos de `POST /admin/chaos` → `POST /admin/chaos/next` (one-shot do próximo request do `X-Test-Id`, header configurável em `chaos.scope_header`). Os headers valem só para a requisição que os traz e respeitam `server.request_headers`:

| Header | Efeito |
| --- | --- |
| `x-wwz-chaos-profile: <nome>` | Troca o perfil de erros resolvido por `profiles.errors.<nome>` |
| `x-wwz-chaos-rate: 0–100` | Sobrescreve `rate` |
| `x-wwz-chaos-types: a,b` | Sobrescreve `types` |
| `x-wwz-chaos-mid-stream-rate: 0–100` | Sobrescreve `mid_stream_rate` |
| `x-wwz-chaos-mid-stream-types: a,b` | Sobrescreve `mid_stream_types` |

A camada que decidiu um erro injetado vai ao log da requisição no campo `chaos` (`profile`, `step`, `header`, `admin`; spec 013 "Logs"): um erro sorteado pelo perfil depois que headers `x-wwz-chaos-*` o alteraram sai como `header`, e headers vencem o passo de cenário. Valor inválido, perfil ou tipo desconhecido são ignorados, como nos demais `x-wwz-*`. `x-wwz-return-error` (I4) força um erro determinístico, sem sorteio. O nome é um tipo da tabela de erros e a resposta tem a forma que o tipo tem naquele driver: `x-wwz-return-error: content_filter` devolve o mesmo 200 com `finish_reason: content_filter` (OpenAI) ou `stop_reason: refusal` (Anthropic) de `errors.types: [content_filter]`, não um 400 ([ADR 0070](decisions/0070-lote-da-rodada-8-achados-de-spec.md)); `x-wwz-content-filter` segue como o atalho de mesmo efeito. Com `seed` fixo, a mesma sequência de requisições produz os mesmos erros. Com `server.seed` diferente de 0 o sorteio não depende do `request_id`: depende do id da conversa (os headers `x-wwz-conversation-id` e equivalentes, ou o hash de modelo, primeiro prompt do usuário e `system`) e do número de mensagens, então requisições idênticas têm o mesmo destino (erro ou sucesso) e só mudar o prompt ou o histórico muda o sorteio. Para variar entre repetições use `seed: 0` ou um `x-wwz-conversation-id` distinto (ADR 0058). `burst: {every: 60s, duration: 5s, rate: 100}` simula uma janela de indisponibilidade periódica; `after_n: 1000` começa a falhar só depois de N requisições (ex.: esgotar cota).

**Janelas de indisponibilidade (`errors.outages`, [ADR 0016](decisions/0016-diretorio-examples.md)).** Lista de janelas programadas em que as requisições do alvo falham com um tipo de erro. Campos de cada janela:

| Campo | Padrão | Significado |
| --- | --- | --- |
| `schedule` | obrigatório | Cron de seis campos com segundos: `segundo minuto hora dia-do-mês mês dia-da-semana` |
| `duration` | obrigatório | Duração Go > 0 (`5s`, `2m`, `1h`) |
| `type` | `overloaded` | Um tipo injetável desta spec; tipo desconhecido torna a configuração inválida |
| `rate` | 100 | 0–100: probabilidade de a requisição dentro da janela receber o erro |
| `timezone` | UTC | Nome IANA (`America/Sao_Paulo`) em que o `schedule` é avaliado |
| `drivers` | vazio | Nomes de `drivers[]`: a janela só vale para requisições servidas por esses drivers; vazio = todos; nome inexistente torna a configuração inválida |

*Onde se declara (spec 003).* No bloco `errors:` da raiz (só `outages`), em `profiles.errors.<nome>.outages` e no `errors` inline de um modelo. A chave não tem campo próprio: `keys[].errors` nomeia um perfil, e as janelas desse perfil valem para a chave.

*Níveis somam.* Ao contrário do resto do perfil de erros (substituído por inteiro, ADR 0026), as janelas de quatro níveis se somam: raiz, modelo (perfil nomeado ou inline), chave (perfil de `keys[].errors`) e perfil trocado por `x-wwz-chaos-profile`. Um mesmo perfil alcançado por dois níveis (modelo e chave apontando para `flaky`, por exemplo) é avaliado uma vez. As janelas são avaliadas nessa ordem de níveis e, dentro do nível, na ordem da lista; vale a primeira janela ativa, do driver da requisição, cujo sorteio de `rate` acerta, e o `retry-after` é o tempo até o fim dessa janela, enviado como o provedor real envia: só nas respostas 429, 503 e 529 (um 500 ou 502 de janela sai sem `retry-after`, como o real; ADR 0058). Se o sorteio de `rate` de uma janela erra, a avaliação segue para as janelas seguintes; se nenhuma acerta, a requisição segue o comportamento normal (sorteio de `errors.rate`, mid-stream etc.).

*Janela.* Uma janela está ativa em `now` quando existe um disparo `t0 ≤ now` do `schedule` com `now < t0 + duration`; disparos são em segundos inteiros. Com `duration` maior que o intervalo entre disparos, a janela vai do disparo mais recente. Dentro da janela o erro sai sem sorteio de `errors.rate` do perfil (só `rate` da janela conta). Com `seed` e relógio injetado o comportamento é determinístico.

*Precedência.* Admin (`POST /admin/chaos/next`, eventos de `POST /admin/chaos`, perfil por `X-Test-Id`) > passo de cenário com `error` > janela ativa > sorteio do perfil. Os headers `x-wwz-chaos-rate`, `-types`, `-mid-stream-*` não mudam as janelas (só o perfil sorteado); `x-wwz-chaos-profile` acrescenta as janelas do perfil nomeado como quarto nível. Negações do controle de taxa (spec 010) acontecem antes e não passam pelas janelas.

*Observação.* O erro de janela conta em `wwz_errors_injected_total{phase="pre"}` e sai no log com `chaos=profile` (nunca `header`).

*Sintaxe do cron.* Cada campo aceita `*` (ou `?`), `*/n`, `a`, `a-b`, `a-b/n`, `a/n` (de `a` até o máximo, de `n` em `n`) e listas separadas por vírgula desses itens. Faixas: segundo e minuto 0–59, hora 0–23, dia do mês 1–31, mês 1–12, dia da semana 0–7 (0 e 7 = domingo); nomes (`MON`, `JAN`), `L`, `W` e `#` não são aceitos. Como no cron clássico, quando dia do mês e dia da semana são ambos restritos (nenhum é `*`/`?`), o dia casa se **qualquer um** casar. Um horário que não existe no fuso (o salto da mudança de horário) não dispara naquele dia. Expressão inválida, faixa invertida, passo ≤ 0 ou valor fora da faixa tornam a configuração inválida, com o caminho da janela (`errors.outages[0]`, `profiles.errors.flaky.outages[1]`, `models[id].errors.outages[0]`).

Exemplo:

```yaml
errors:
  outages:
    - {schedule: "*/30 * * * * *", duration: 5s, type: overloaded}
    - {schedule: "0 */10 * * * *", duration: 1m, type: server_error, rate: 50}
    - {schedule: "0 0 */6 * * *", duration: 1h, type: bad_gateway, drivers: [openai]}
    - {schedule: "0 0 9 * * 1-5", duration: 15m, timezone: America/Sao_Paulo}
```

**Headers de retry.** `retry-after` em segundos e, para OpenAI, `x-should-retry: true|false`; para Anthropic, `x-should-retry` também. Configuráveis por tipo.

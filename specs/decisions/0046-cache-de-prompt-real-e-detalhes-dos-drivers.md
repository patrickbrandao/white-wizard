# 0046 — Cache de prompt real e detalhes dos drivers sob a 0042

Status: provisória (revisar quando as fixtures reais de cada provedor mostrarem outra regra, ou quando a chave do Gemini ou do Azure existir)
Data: 2026-10-31

## Contexto
As ADRs [0042](0042-fidelidade-total-ao-provedor-real.md) e [0043](0043-drivers-gemini-e-azure.md) (aceitas pelo humano) mandam o cache de prompt seguir o real, retiram `server.strict` e incluem os drivers `gemini` e `azure`. Ao alinhar as specs (task 160) apareceram escolhas que nenhuma das duas fixa:
- os provedores não publicam tudo de que o simulador precisa (TTL do cache implícito do Gemini, ganho de TTFT, separador SSE do Gemini, textos de vários erros do Azure);
- os modelos do wwz são fictícios (`gpt-5-sim`), mas o mínimo e a granularidade do cache dependem do modelo real;
- o Azure separa deployment e modelo, e a doc se contradiz sobre o TPM reservar `max_tokens`;
- a ADR 0040 manteve `GET /api/v1/models/{slug}` no OpenRouter como desvio de compatibilidade, que a 0042 não permite.

## Decisão
- **Cache como registro de prefixos.** O driver declara as regras; o núcleo guarda no store (`memory` ou `valkey`) hashes encadeados do prompt tokenizado, por conta (a entidade de limite do driver), com o TTL do provedor renovado a cada leitura; nada de conteúdo. A entrada vale a partir do primeiro byte da resposta que a criou. OpenAI/Azure/Gemini implícito gravam todo prompt (o acerto é o maior prefixo gravado, respeitando mínimo e granularidade); Anthropic grava só nos breakpoints `cache_control` (lookback de 20 blocos); Gemini explícito usa `cachedContents`.
- **Regras por modelo.** Padrão do driver = geração atual de modelos reais: OpenAI/Azure 1 024 tokens e granularidade 128; Anthropic 512; Gemini 4 096. `models[].prompt_cache {min_tokens, granularity, kind}` (campo novo e compatível) troca a regra para imitar outro modelo real. No OpenRouter a regra vem do autor do slug (`openai/`, `anthropic/`, `google/`); outros autores usam `models[].prompt_cache` (padrão `none`). No OpenCode, a regra da API nativa do modelo.
- **TTL onde o real é vago.** OpenAI `in_memory` "5–10 min sem uso, no máximo 1 h": 10 min renovados, teto 1 h. Gemini implícito (não documentado): 5 min. Marcados "não conferido" até a fixture.
- **TTFT.** `cache_ttft_reduction` (padrão 0,7) continua como parâmetro de latência do operador, aplicado na proporção dos tokens lidos do cache; não altera nenhum campo observável além do tempo. `cache_hit_probability` sai.
- **Azure.** O deployment da rota (ou `model` na v1) é o `id` ou um alias de `models[]`; `model` da resposta é o `id`, como o real devolve o nome do modelo e não o do deployment. O TPM reserva `max_tokens` na admissão e ajusta pelo consumo real (a doc se contradiz; "não conferido").
- **Gemini.** `name: "models/<id>"`, `modelVersion` = `id`. Prefixos `/gemini` e `/azure` nos exemplos; na raiz, os caminhos exclusivos (`/v1beta/...`, `/openai/deployments/...`, `/openai/v1/...`) e as credenciais (`x-goog-api-key`/`?key=`, `api-key`) escolhem o driver.
- **OpenRouter.** Sai o desvio `GET /api/v1/models/{slug}` da ADR 0040: o caminho responde como o real responde a rota inexistente (a conferir). Aliases sem `/` ganham o prefixo `white-wizard/`, como o `id`. `model` sem autor: 400 `{"error":{"message":"<model> is not a valid model ID","code":400},"user_id":…}` (relatos públicos; status inferido; "não conferido"). *Corrigida pela [0052](0052-openrouter-aceita-id-sem-autor.md): o real aceita o id sem autor; o 400 vale para nome desconhecido, parcial ou de outro autor.*
- **`reasoning_field`** (ajuste da [0022](0022-raciocinio-fiel-ao-provedor.md) pela 0042): só valores que o provedor real usa. `openai` e `azure` aceitam só `hidden` (a OpenAI real nunca manda `reasoning_content`); `anthropic` e `gemini` têm formato fixo; `opencode` e `openrouter` aceitam `reasoning_content`, `reasoning`, `hidden` e `inline_tag` para imitar o modelo upstream. Outro valor é configuração inválida.
- **`auth: none`** continua: é condição de teste do operador, como `errors` e `limits`; com chave válida, a resposta é a do real.

## Alternativas consideradas
- Guardar só o hash do prompt inteiro: erraria o acerto parcial, que o real dá.
- Regras de cache fixas por driver, sem `models[].prompt_cache`: não imita modelos reais com outro mínimo.
- Deployment = `model` da resposta no Azure: diverge do real.

## Consequências
- Specs 003, 004, 005, 006, 007 e 008 atualizadas (task 160). Parte do `GET /api/v1/models/{slug}` da ADR 0040 e a opção `reasoning_field` livre no `openai` da ADR 0022 superadas.
- `cache/valkey/README.md` ganha as chaves de prefixo (tarefa do guardião junto da 162).
- Tarefas 161–165 implementam; tarefa nova para remover a rota de compatibilidade do OpenRouter.

## Nota de 2026-10-31 (task 153)
Contrato das chaves de prefixo em [cache/valkey/README.md](../../cache/valkey/README.md) (seções 2.4 e 3.14). Escolhas do contrato, provisórias como esta ADR: leitura somente leitura antes do `Check` (`ZMSCORE`) e gravação mais renovação num só script no primeiro byte, para uma requisição negada não renovar nada; hash tag da conta (`{account:<id>}`), e `{ip:<ip>}` sem chave (`auth: none`); com `granularity: 0`, candidatos a cada 128 tokens e nas fronteiras de bloco (o real dá o prefixo comum exato; o wwz, o maior candidato comum); no máximo 1 024 candidatos por requisição e 65 536 entradas por conta, removendo as de menor expiração. Revisar junto com a ADR, quando as fixtures reais mostrarem outro comportamento.

## Nota de 2026-10-31 (task 200, contrato como implementado na task 162)
- `prompt_cache_key` entra no hash inicial (junto de driver e modelo); mesma prompt com outra chave é miss (observado no real). A cadeia de hashes passa por todos os pontos de corte da regra, não só pelos candidatos consultados.
- A afirmação "Anthropic 512" é só o padrão do driver: o `claude-haiku-4-5` real tem mínimo de **4 096** (conferido). O padrão continua por driver; `models[].prompt_cache.min_tokens` imita o mínimo de cada modelo real. Não se criou tabela de padrões por modelo (os ids do wwz são fictícios).
- OpenAI: mínimo 1 024 e passos de 128 conferidos. `prompt_cache_retention: "24h"` (24 h, teto 24 h) e `prompt_cache_options.ttl` (`30m`, `12h`) implementados.
- Custo: escrita de 1 h = 2× a entrada; `Usage.CacheWrite1hTokens` separa a parte de 1 h da escrita total.
- Seguem "não conferidos": expiração real dos TTLs, lookback de 20 blocos, 4 breakpoints, cache de OpenRouter/OpenCode/Gemini/Azure, ordem dos blocos da OpenAI, `cache_control` no topo.

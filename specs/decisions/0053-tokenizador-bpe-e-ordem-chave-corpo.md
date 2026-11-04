# 0053 — Tokenizador BPE real e ordem chave × corpo

Status: provisória (revisar quando: `wwz-drift record` gravar fixtures de `usage` com vários textos por provedor; um provedor sem tokenizador público passar a publicar o dele; o benchmark do `internal/tokenize` passar de 20 µs/KB; ou uma fixture real mostrar outra ordem de validação)
Data: 2026-10-31

## Contexto
Três pontos abertos depois da task 171 (task 187):

1. **Ordem chave × corpo.** A 171 adotou "chave primeiro" (`driver.Conventions.AuthFirst`) em `openai`, `anthropic` e `openrouter`, mas as fixtures de `testdata/real/*/validation/` usam chave válida e não decidem.
2. **Tokenizador.** A spec 003 descreve `models[].tokenizer: tiktoken:<encoding>` e a spec 007 diz que `o200k_base` reproduz a OpenAI, mas o código só tem `generate.Simulated` e ignora `models[].tokenizer`. A sonda do drift dá 12 `prompt_tokens` no wwz contra 13 no real. A [ADR 0042](0042-fidelidade-total-ao-provedor-real.md) não aceita divergência do real; a [ADR 0001](0001-decisoes-iniciais.md) deixou `tiktoken` "opcional depois"; a spec 014 avisa que tokenizar de verdade custa mais que todo o resto.
3. **Modelo aposentado na validação antecipada.** `internal/server/validate.go` (`knownModel`) não olha `retires_at`; na OpenAI, que confere o modelo antes do resto do corpo, um modelo aposentado com corpo inválido mostra o erro de corpo em vez do 404.

Fatos novos, conferidos no real em 31/10/2026 a custo zero (só respostas 401/400/404; fixtures em `testdata/real/<provedor>/auth-order/`):
- 25 requisições com chave inválida (`sk-invalid-wwz-…`, `sk-ant-invalid-wwz-…`, `sk-or-v1-` + 64 zeros) ou sem chave, combinadas com JSON inválido, modelo inexistente, campo obrigatório ausente e tipo errado: **todas 401** nos três provedores, com o texto da credencial (inválida, ausente, formato da chave no OpenRouter). Na Anthropic, a chave vem antes de `anthropic-version`.
- 12 contrastes com chave válida: Anthropic confere o JSON antes de `anthropic-version` e `anthropic-version` (`anthropic-version: header is required`) antes de campos e modelo; OpenRouter confere `messages`/`prompt` (JSON inválido conta como `{}`), depois o modelo e só então os tipos (`temperature`, `stream`); OpenAI: JSON → modelo → resto.
- Textos que o wwz ainda não reproduz: OpenAI sem chave (mensagem longa, `application/json; charset=utf-8`, serialização do 404, sem `x-error-json`); Anthropic sem chave (`x-api-key header is required`); máscara da OpenAI com um `*` por caractere escondido (comprimento − 12, não 8 fixos); OpenRouter `/api/v1/messages` com 401 no envelope Anthropic (`error_type: "authentication"`, `request_id: null`).
- Tokenizador, medido no Mac de desenvolvimento (Apple Silicon) com a biblioteca de referência `github.com/tiktoken-go/tokenizer` v0.10.0 (MIT, Go puro, vocabulários embutidos como mapas Go) num módulo descartável em `tmp/`:
  - sonda `Responda só: ok`: o200k = 6 tokens de texto + 1 de role + 3 + 3 = **13**, igual ao real (OpenAI e Anthropic); simulado = 5 + 7 = 12;
  - custo: ~62 µs/KB (~66 µs num prompt de 1 KB); o simulado custa ~2 µs/KB;
  - binário: +14,7 MB (2,4 MB → 17,1 MB num programa vazio), porque embute r50k, p50k, cl100k e o200k como código Go;
  - ranks em formato compacto (comprimento + bytes, ordenados por rank): o200k_base 1,60 MB (1,05 MB com gzip), cl100k_base 0,74 MB (0,46 MB com gzip); arquivos oficiais `openaipublic` com os sha256 publicados no `tiktoken` (MIT): o200k `446a9538…1a2d`, cl100k `223921b7…b2a7`.
- Anthropic não publica tokenizador. `count_tokens` (gratuito) contra o200k e simulado, com o overhead de 7 da spec 007: sonda 13 / 13 / 12; 1 KB em português 296 / 260 / 272; inglês com código (89 bytes) 36 / 32 / 36. Nenhum dos dois reproduz a Anthropic; o simulado fica mais perto em textos longos.

## Decisão
- **Ordem chave × corpo** (spec 006, 16.7): confirmada a da task 171 em `openai`, `anthropic` e `openrouter` (credencial antes de tudo do corpo e, na Anthropic, antes de `anthropic-version`); OpenCode segue sessão → corpo → modelo → chave. A ordem depois da chave, por provedor, é a das fixtures `validkey_*`. Os textos de credencial ausente e a máscara passam a ser os reais.
- **Modelo aposentado = inexistente em toda etapa.** A validação antecipada de `internal/server` usa a mesma regra do engine (`servable`: dialeto do driver e `!Retired(now)`), de modo que o 404 (ou o 400 do OpenRouter) de um modelo aposentado sai na mesma posição da ordem em que sai o de um nome desconhecido.
- **Tokenizador BPE próprio, sem dependência nova.** Pacote `internal/tokenize` com o BPE byte a byte do `tiktoken` para `o200k_base` e `cl100k_base`:
  - ranks embutidos (`go:embed`) no formato compacto com gzip (~1,5 MB no binário), gerados por `go generate` a partir dos arquivos oficiais com conferência do sha256; rede só na geração, nunca em runtime; atribuição MIT do `tiktoken` em `THIRD_PARTY_NOTICES` (ou no `LICENSE`);
  - pré-tokenizador escrito à mão para os padrões de `o200k_base` e `cl100k_base` (o `regexp` do Go não tem lookahead), sem `regexp2`;
  - tabela de ranks carregada na primeira requisição que usa o encoding (só os usados ocupam memória);
  - cache limitado de contagens por conteúdo de mensagem (hash de `maphash`, LRU de 64 Ki entradas por encoding), porque os geradores de carga repetem prompts;
  - meta: ≤ 20 µs/KB num prompt de 1 KB no Mac de desenvolvimento, medido por benchmark; a referência gasta ~62.
  - `Split` (streaming) agrupa tokens até formar UTF-8 válido, para nenhum delta sair com caractere partido.
- **Escolha por modelo** (`models[].tokenizer`, spec 003): `auto` (padrão novo), `simulated`, `tiktoken:o200k_base`, `tiktoken:cl100k_base`; outro valor torna a configuração inválida. `auto` olha `version`, `id` e aliases (sem o prefixo de autor do OpenRouter): família OpenAI atual (`gpt-4o*`, `chatgpt-4o*`, `gpt-4.1*`, `gpt-4.5*`, `gpt-5*`, `o1*`, `o3*`, `o4*`, `gpt-oss*`) → `o200k_base`; `gpt-4`, `gpt-4-turbo*`, `gpt-3.5-turbo*`, `text-embedding-*` → `cl100k_base`; o resto → `simulated`. Assim os modelos OpenAI saem iguais ao real sem configuração, e o custo do BPE só aparece neles.
- **Provedores sem tokenizador público** (Anthropic, Gemini, upstreams do OpenCode e os autores não OpenAI do OpenRouter): `auto` = `simulated`, marcado "não conferido" na spec 007. Não é desvio deliberado: não há como reproduzir offline um tokenizador não publicado. O simulado fica, pelos números acima, mais perto da Anthropic que o o200k. `simulated` explícito num modelo OpenAI é escolha do operador por custo (spec 014), como `limits` e `latency`.
- Rejeitado como dependência: `github.com/tiktoken-go/tokenizer` (+14,7 MB por binário, ~62 µs/KB, `regexp2`) e `github.com/pkoukk/tiktoken-go` (baixa os arquivos da rede por padrão; o carregador offline `tiktoken-go-loader` embute os `.tiktoken` em base64, módulo de 3,3 MB compactado, e o pacote traz `regexp2` e `google/uuid`). Os dois servem de referência para gerar as fixtures douradas fora do módulo.

## Alternativas consideradas
- Aceitar a aproximação do simulado sob a 0042: contraria a 0042 justamente nos modelos em que o real é reproduzível offline.
- `tiktoken-go/tokenizer` como dependência: menos código, mas dobra o binário (spec 014 fala em ~15 MB) e custa 3× a meta por KB.
- `o200k_base` como padrão para todos os drivers: acerta a sonda da Anthropic por coincidência, mas erra mais que o simulado nos textos longos medidos (260 contra 296; simulado 272).

## Consequências
- Spec 006 (16.7: ordem por provedor, textos de credencial ausente, máscara, 401 do OpenRouter Messages, modelo aposentado; critério de aceite), spec 003 (`models[].tokenizer`), spec 007 (contagem de tokens), spec 014 (linha do tokenizador) e spec 018 (fixtures `auth-order/`).
- 37 fixtures em `testdata/real/{openai,anthropic,openrouter}/auth-order/`, no formato das de validação; a credencial inválida fica literal (é falsa), a válida vira `<key>`.
- Tarefas de código (descritas na task 187): validação e textos de credencial (engine, `oai`, `anth`, `openrouter`, `server/validate*.go`); `internal/tokenize` + `models[].tokenizer` no engine e nos cenários; teste `TestRealFixturesAuthOrder`.

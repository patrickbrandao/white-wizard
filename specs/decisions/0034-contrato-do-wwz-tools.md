# 0034 — Contrato do `wwz-tools`

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
As specs 001 e 017 só dizem que o `wwz-tools` é um "servidor de ferramentas fake (sucesso, timeout, erro, payload inválido)". A [ADR 0015](0015-wwz-load-agents-e-top.md) manda o `wwz-load agents` executar as tool calls "via `wwz-tools` em sandbox", com o `wwz-load` dependendo de `internal/` do `wwz-tools`. Sem catálogo, protocolo, sandbox nem códigos de saída, as tarefas da fase 4 não são concretas.

## Decisão
- **Núcleo em `internal/tools`**, usado em processo pelo `wwz-load agents` (padrão) e servido por HTTP pelo binário `cmd/wwz-tools`.
- **Catálogo fixo por perfil**: `coder` (`read_file`, `write_file`, `edit_file`, `list_dir`, `search_code`, `run_command`), `pesquisa` (`web_search`, `fetch_url`, `read_file`), `chat` (nenhuma), `all` (união). Schemas JSON fixos (spec 017).
- **Sandbox simulada, nunca real**: sistema de arquivos virtual em memória por sessão, semeado de forma determinística. `run_command` nunca executa nada e `web_search`/`fetch_url` nunca acessam a rede. Nada toca o disco nem a rede do host.
- **Resultados injetados por taxa** (`error`, `timeout`, `invalid_payload`) além dos erros naturais (arquivo inexistente, argumento inválido), com latência por ferramenta e determinismo por semente. O header `x-wwz-tools-outcome` força o resultado de uma chamada.
- **Protocolo HTTP/JSON próprio** (`net/http`, porta padrão `:9200`), descrito em `cmd/wwz-tools/openapi.yaml` (ADR 0006 estendida a este binário), com bearer opcional `WWZ_TOOLS_TOKEN`. MCP fica fora do MVP.
- **Sem imagem própria**: o binário `wwz-tools` vai na imagem do `wwz-load` como entrypoint alternativo, mantendo as três imagens da spec 018.
- Sem dependência nova no `go.mod`.

## Alternativas consideradas
- Executar comandos e arquivos reais num diretório temporário: não determinístico, inseguro sob carga e caro em disco.
- Expor as ferramentas por MCP: protocolo extra sem consumidor no MVP.
- Imagem Docker própria: mais uma imagem multi-arch com o disco apertado, sem uso em produção.

## Consequências
- A spec 017 ganha a seção "Contrato do `wwz-tools` (fase 4)" e a spec 015 referencia o executor em processo e `--tools-url`.
- Tarefas: `internal/tools`, `cmd/wwz-tools/openapi.yaml`, `cmd/wwz-tools` e o alvo do Dockerfile.

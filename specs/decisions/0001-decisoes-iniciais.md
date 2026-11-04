# 0001 — Decisões iniciais

Status: aceita
Data: 2026-10-30

## Contexto
Decisões menores tomadas no documento Claude Docs original (seções 1–23) e na entrevista de 2026-10-30, agrupadas aqui para não serem reabertas.

## Decisão
- Nome White Wizard; prefixos `wwz` (binários, headers, métricas) e `WWZ` (env). O nome `llmsim` foi abandonado.
- Linguagem Go (Rust descartado), versão mínima 1.27, `CGO_ENABLED=0`; remover o `replace golang.org/x/sys` do `go.mod` e rodar `go mod tidy`.
- Repositório e módulo `github.com/patrickbrandao/white-wizard`; licença Apache-2.0.
- Binários: `wwz` (servidor), `wwz-load` (benchmark), `wwz-panel` (painel Go + HTMX + SQLite), `wwz-tools`, `wwz-drift`.
- MVP completo, sem entregas por fase: todos os drivers (`openai`, `anthropic`, `opencode`, `openrouter`), cenários, store Valkey ou memória, painel, `wwz-load`, recursos I1–I24. *(Os dialetos Gemini e Azure de I17/I18 saem do MVP pela [ADR 0028](0028-gemini-e-azure-fora-do-mvp.md).)*
- Store: `store.kind: valkey | memory`; a interface `limits.Store` tem as duas implementações; scripts Lua do Valkey têm equivalente em Go na memória.
- Servidor sem estado de conversa: o passo do cenário é derivado do histórico; `seed` configurável para reprodução.
- Campo de raciocínio OpenAI: `reasoning_content` por padrão; `reasoning` e `<thinking>` inline configuráveis por modelo. *(Substituído pela [ADR 0022](0022-raciocinio-fiel-ao-provedor.md): o driver `openai` oculta o raciocínio por padrão.)*
- Tokenizador regex (`simulated`) no MVP; `tiktoken` em Go puro opcional depois.
- Imagens multi-arch (amd64, arm64) em `docker.io/patrickbrandao/white-wizard`, base `scratch`/distroless. *(Namespace substituído pela [ADR 0075](0075-github-publico-e-imagens-tmsoftbrasil.md): `docker.io/tmsoftbrasil`.)*
- Ambientes: local com `docker compose`; pesado em servidor Linux designado pelo usuário, credenciais fora do git.
- Documentação de uso em Markdown em `./docs/`, lida direto no GitHub.
- Repositório git local criado na `main` com commit inicial; remoto e push ficam com o usuário.

## Alternativas consideradas
- Rust; entregas por fase; `reasoning` como campo padrão; MkDocs + GitHub Pages.

## Consequências
- Agentes não discutem mais linguagem, escopo do MVP, nome ou licença sem fato novo.

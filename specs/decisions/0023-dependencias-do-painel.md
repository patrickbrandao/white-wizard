# 0023 — Dependências do painel

Status: aceita (SQLite substituído pela 0044) (revisão com o humano em 2026-10-31)
Data: 2026-10-31
Complementa: item "Painel: Go + HTMX + SQLite" da [ADR 0001](0001-decisoes-iniciais.md)

## Contexto
A ADR 0001 fixou Go + HTMX + SQLite para o `wwz-panel`, sem escolher bibliotecas. Restrições: imagem distroless com `CGO_ENABLED=0` (ADR 0007), máquina de desenvolvimento com ~1,5–2 GiB de disco livre, sem npm e sem CDN, e a ADR 0003 (net/http no control plane). O painel precisa de SQLite com `VACUUM INTO` (ADR 0009), hash argon2 e OIDC (spec 016).

## Decisão
- **SQLite:** `github.com/ncruces/go-sqlite3` v0.36+ (só o pacote `driver` é importado; o SQLite é compilado para wasm e traduzido para Go puro pelo wasm2go no módulo `ncruces/go-sqlite3-wasm/v6`, dependência indireta; sem wazero, sem o pacote `embed`, que está deprecado, e sem CGO). Suporta `VACUUM INTO`, WAL e `PRAGMA user_version`. Módulo e compilação bem menores que `modernc.org/sqlite` + `modernc.org/libc`. Fallback para `modernc.org/sqlite` só diante de bloqueio técnico, registrado em nova ADR.
- **Senhas:** argon2id de `golang.org/x/crypto/argon2` (parâmetros na spec 016).
- **OIDC:** `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` (`go-jose/v4` transitivo). Verificação de JWT/JWKS não é escrita à mão.
- **Web:** `html/template` + `embed`; `htmx.min.js` vendorizado em `internal/panel/web/static/` (versão fixada, SHA-256 anotado no CHANGELOG); CSS próprio pequeno; roteamento pelo `http.ServeMux` da stdlib (padrões com método e curinga do Go ≥ 1.22). Sem framework de roteamento.
- **Sessão e CSRF:** stdlib (`crypto/rand`, `crypto/hmac`, `crypto/sha256`): id aleatório em cookie `HttpOnly`/`SameSite=Lax` assinado com HMAC, revogável na tabela `sessions`; token CSRF por sessão.
- **Valkey:** o mesmo `valkey-io/valkey-go` do wwz. **Prometheus:** HTTP API via `net/http` (sem client_golang). `/metrics` do painel escrito à mão no formato texto.

## Alternativas consideradas
- `modernc.org/sqlite`: maduro, também sem CGO, mas o módulo e o tempo de compilação são várias vezes maiores (problema de disco e de CI).
- `mattn/go-sqlite3`: exige CGO; quebra a imagem distroless estática.
- `bbolt`/arquivo JSON no lugar de SQLite: contradiz a ADR 0001 e perde consultas de agregação.
- `golang-jwt` + JWKS manual para OIDC: mais código de segurança próprio.
- Framework web (chi, echo) e templ: dependência sem ganho para ~30 rotas.

## Consequências
- `go.mod` ganha `ncruces/go-sqlite3` v0.36 (tarefa 046; indiretas `ncruces/go-sqlite3-wasm/v6` e `ncruces/julianday`) e `x/crypto`, `go-oidc/v3`, `x/oauth2` (tarefa 047, `go-jose/v4` indireta), cada tarefa dona do `go.mod` em sequência.
- Sem compilação de wasm em tempo de execução: o SQLite traduzido é código Go comum, compilado junto com o binário.
- Spec 001 (tabela de stack) e spec 016 atualizadas.

## Revisão de 2026-10-31 (task 096)
Fato novo: a partir da v0.36 o `ncruces/go-sqlite3` deixou de executar o SQLite pelo wazero; o wasm é traduzido para Go (wasm2go) e o pacote `embed` ficou deprecado. A decisão (biblioteca, motivos, alternativas) não muda; só a descrição técnica e a consequência de inicialização foram corrigidas para casar com o `go.mod` (tarefas 046 e 047).

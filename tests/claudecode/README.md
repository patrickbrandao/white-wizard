# Claude Code real contra o wwz

Roteiro manual (fora do `verify`) que confere o Claude Code instalado contra o driver `anthropic` do wwz, no modo estrito do cliente (`client_verification: claude-code`, spec 020) e sem ele ([ADR 0074](../../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md)). Rode uma vez por versão do Claude Code; o que o roteiro não alcança vai para a [spec 006, "Não conferidos no real"](../../specs/006-fidelidade-provedores.md#não-conferidos-no-real), e uma recusa do cliente real vira tarefa.

```sh
tests/claudecode/real-claude.sh          # pula (saída 0) sem `claude` no PATH
WWZ_REAL_CLAUDE_KEEP=1 tests/claudecode/real-claude.sh   # mantém o log do wwz
```

## O que faz

1. Compila `wwz` e `wwz-load` num diretório temporário fora do repositório e sobe o wwz em `127.0.0.1` com o driver `anthropic` em dois mounts: `/strict` (`client_verification: claude-code`) e `/off` (sem a opção). Os dois têm `oauth_key` (segredo aleatório por execução) e modelos que respondem `ok` sem tool call.
2. Emite um access token `sk-ant-oat01-…` com `wwz-load claude-code login --setup-token` contra o próprio wwz. O token é do wwz e só vale nele; **nenhuma credencial real é usada**, nem a do humano, nem o keychain, nem `~/.claude`.
3. Roda o `claude` com `env -i` (só `HOME` e `PATH` temporários), `ANTHROPIC_BASE_URL=http://127.0.0.1:<porta>/<mount>`, `CLAUDE_CODE_OAUTH_TOKEN=<token do wwz>`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` e `CLAUDE_CONFIG_DIR` vazio, em três modos, nos dois mounts:

| Modo | Comando | Esperado |
| --- | --- | --- |
| `minimo` | `claude -p "say ok" --system-prompt "You are a terse test assistant." --tools ""` (a forma das fixtures; o prompt vem antes de `--tools`, que é variádico) | 200 e `ok` nos dois mounts |
| `padrao` | `claude -p "say ok"` (prompt e tools padrão) | 200 e `ok` nos dois mounts (no `/strict`, só depois da task 457) |
| `projeto` | o mínimo num cwd com `CLAUDE.md` | idem |

Cada caso passa com saída 0 e `ok` no stdout; na falha, a tabela mostra o `reason` e o detalhe do 403 tirados do log do wwz. O script sai 0 só se todos os casos passarem. Variáveis: `CLAUDE_BIN`, `WWZ_REAL_CLAUDE_MODES`, `WWZ_REAL_CLAUDE_MOUNTS`, `WWZ_REAL_CLAUDE_KEEP`.

## O que vai para fora do wwz

O Claude Code só segue `ANTHROPIC_BASE_URL` em `/v1/messages` (e na sonda `HEAD /api/hello`). As rotas de conta e OAuth usam o host fixo `api.anthropic.com` e podem receber o token falso do wwz (que o real recusa com 401); isso está aceito na ADR 0074. Com `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` o tráfego não essencial (telemetria, atualização) fica desligado.

## Resultado (2026-11-03, Claude Code 2.1.308)

| Modo | `/strict` | `/off` |
| --- | --- | --- |
| `minimo` | 200 `ok` | 200 `ok` |
| `padrao` | 403 `fingerprint: expected 031, got 7f2` | 200 `ok` |
| `projeto` | 403 `fingerprint: expected d54, got 7f2` | 200 `ok` |

Causa das duas falhas (task 457): o Claude Code calcula o fingerprint do texto da primeira mensagem `user` que não é meta, mas manda os `<system-reminder>` meta (prompt padrão, `CLAUDE.md` do cwd e dos diretórios acima) como blocos `text` antes dela, na mesma mensagem; o verificador pega o primeiro bloco. Por isso o roteiro roda fora do repositório: dentro dele, o `CLAUDE.md` do projeto faz até o modo mínimo falhar.

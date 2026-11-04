# Claude Code 2.1.308: requisições cruas (fixtures)

Spec 020 e [ADR 0074](../../../../specs/decisions/0074-assinatura-oauth-e-cliente-claude-code.md). Cada `.raw` é uma requisição HTTP/1.1 inteira (cabeça com CRLF, linha vazia e corpo) que o Claude Code 2.1.308 (`/opt/homebrew/bin/claude`, Bun 1.4.4, SDK Stainless 0.128.0) mandou a um servidor de captura TCP local em 2026-11-03, com `env -i`, `CLAUDE_CONFIG_DIR` vazio, `ANTHROPIC_BASE_URL=http://127.0.0.1:<porta>` e credencial falsa (`CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-WWZFAKE…` ou `ANTHROPIC_API_KEY=sk-ant-api03-WWZFAKE…`; não são segredos). Sem custo e sem tráfego à Anthropic.

O teste `TestRealFixturesClaudeCode` (`internal/claudecode`) reconstrói cada arquivo com `Extract` + `Build` (mesmo sha256 desta tabela) e passa as requisições de inferência pelo `Verify`.

## Normalização

Só no corpo, depois o `Content-Length` recalculado (o fingerprint não muda: depende só da primeira mensagem `user`):

- diretório de trabalho → `/work`; diretório de configuração (`CLAUDE_CONFIG_DIR`) → `/cfg`;
- diretório de scratchpad do bloco de ambiente (derivado do diretório de trabalho e da sessão) → `/private/tmp/claude-501/-work/<sessão>/scratchpad`;
- `safeguards[0].classifier_context.home_dir` → `/home/user`; `user_identity` → `user`.

A captura com o prompt padrão do Claude Code e a do título da sessão ficam fora do git (ADR 0074, item 13). Nenhuma fixture traz o prompt padrão nem descrições de tools do Claude Code (`--system-prompt "You are a terse test assistant."`; `--tools ""`, ou `--tools Read` nas de count_tokens, cujo corpo leva `tools: []` e o conteúdo do arquivo lido, `lorem ipsum…`).

## Arquivos

| Arquivo | Origem | Modo | Tipo | Normalização | sha256 |
| --- | --- | --- | --- | --- | --- |
| `001-api-hello-head.raw` | captura 001 | sonda antes de `claude -p` | `hello` (`HEAD /api/hello`) | nenhuma | `680faeb982acf41d80976328a9f9504bf7e78a738cba7e0585a03bce17986cf4` |
| `002-sdk-cli-oauth-turn.raw` | captura 002 | `claude -p`, OAuth | `turn` | diretório de trabalho | `5644b70ff35f1ac004b1a44597fef60c5a4fae949c14026b92ab3b749a42014d` |
| `004-sdk-cli-apikey-turn.raw` | captura 004 | `claude -p`, API key | `turn` | diretório de trabalho | `fc5f169cdfa15ca0fdcdabd94edfddef7e00a593b86566e3e780ad66848e8a0b` |
| `006-sdk-cli-oauth-turn1.raw` | captura 006 | `claude -p`, OAuth, 1º turno | `turn` | diretório de trabalho | `888c86dab4248a65198e56a627763a8f5cdc80f9e1d1c70feb61e1228c66347e` |
| `008-sdk-cli-oauth-turn2.raw` | captura 008 | `claude -p --resume`, OAuth, 2º turno | `turn` | diretório de trabalho | `a6b4506a695b417b72ed6c87846b1dae2da80726aaccdd653d043b82b9baf774` |
| `010-cli-oauth-quota.raw` | captura 010 | interativo, OAuth | `quota` | nenhuma | `ec83de91f043ead8f988672c135c351b5fe7f8164a404ce5de3090feb3407104` |
| `012-cli-oauth-turn-safeguards.raw` | captura 012 | interativo, OAuth | `turn_safeguards` | diretório de trabalho, configuração, scratchpad, home, usuário | `3887b919924306c4b3fe8658b120d9d7d65b810c22dca12b6496051210161e3c` |
| `013-sdk-cli-oauth-count-tokens.raw` | captura nova (task 442): `claude -p --tools Read`, o servidor manda ler um arquivo de 35 KB e o Claude Code conta os tokens antes de anexá-lo | `claude -p`, OAuth | `count_tokens` | nenhuma | `750deb15c0ed09b491c7b235743c5cd5ef33558d5a67b9f91f91b008742e633c` |
| `014-sdk-cli-apikey-count-tokens.raw` | idem | `claude -p`, API key | `count_tokens` | nenhuma | `65e08db5a6fffbb6d818a6fecbaab8a63ecb8ab0cfeff0af77bc8d30a2219db8` |

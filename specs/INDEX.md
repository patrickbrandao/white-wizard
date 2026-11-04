# Índice das especificações

`./specs/` é a fonte da verdade. Carregue cada spec sob demanda. Decisões em [decisions/HISTORY.md](decisions/HISTORY.md).

| Arquivo | Título | Escopo | Depende de | Status |
| --- | --- | --- | --- | --- |
| [000-visao-geral.md](000-visao-geral.md) | Visão geral | Propósito, escopo, R1–R24, matriz de cobertura | — | rascunho |
| [001-arquitetura.md](001-arquitetura.md) | Arquitetura | Binários, pacotes, dono dos dados, auth, contratos, stack, qualidade | 000 | rascunho |
| [002-organizacao-do-trabalho.md](002-organizacao-do-trabalho.md) | Organização do trabalho | Papéis, git, tarefas, DoD, escalonamento | 000, 001 | aprovada |
| [003-configuracao.md](003-configuracao.md) | Configuração | YAML, env, recarga, precedência; sem opção de desvio do real | 000, 001 | rascunho |
| [004-apis-e-endpoints.md](004-apis-e-endpoints.md) | APIs e endpoints | Rotas de inferência (OpenAI, Anthropic, Gemini, Azure), auth, headers, validação sempre como o real; modo `public` e Console do OpenCode | 000, 001, 003 | rascunho |
| [005-drivers.md](005-drivers.md) | Drivers | Interface e registro dos 6 drivers, detecção na raiz, deployments do Azure | 001, 004 | rascunho |
| [006-fidelidade-provedores.md](006-fidelidade-provedores.md) | Fidelidade aos provedores | Princípio da fidelidade total (ADR 0042); limites, bloqueios e erros de OpenAI, Anthropic (com a assinatura OAuth do Claude Code, ADR 0074), OpenCode, OpenRouter, Gemini e Azure; lista única dos não conferidos no real (ADR 0076) | 005 | rascunho |
| [007-geracao-respostas.md](007-geracao-respostas.md) | Geração de respostas | Texto, tool calls, raciocínio, tokens, cache de prompt real por provedor | 003, 004 | rascunho |
| [008-latencia.md](008-latencia.md) | Latência | Distribuições, carga, pacing, efeito do cache no TTFT | 003 | rascunho |
| [009-erros-e-caos.md](009-erros-e-caos.md) | Erros e caos | Erros, faults, escopo por teste | 004, 005 | rascunho |
| [010-controle-de-taxa.md](010-controle-de-taxa.md) | Controle de taxa | rps/rpm/tpm/concorrência, headers | 005, 012 | rascunho |
| [011-cenarios.md](011-cenarios.md) | Cenários | Motor stateless de cenários | 007, 009 | rascunho |
| [012-estado-valkey.md](012-estado-valkey.md) | Estado em Valkey | Entidades, chaves, scripts, abuso, políticas e tier por entidade | 001 | rascunho |
| [013-observabilidade-admin.md](013-observabilidade-admin.md) | Observabilidade e admin | Métricas, API admin, logs, tracing | 001, 003 | rascunho |
| [014-desempenho-e-escala.md](014-desempenho-e-escala.md) | Desempenho e escala | Metas, decisões Go, escala horizontal | 001, 012 | rascunho |
| [015-wwz-load.md](015-wwz-load.md) | wwz-load | Benchmark e metodologia; flags, medidas, erros, saída JSON/CSV, conversa por cenário, instâncias distribuídas; `agents` e `top` (fase 4); `claude-code` (ADR 0074) | 004, 011, 017 | rascunho |
| [016-painel.md](016-painel.md) | Painel | wwz-panel: configuração, schema SQLite, fontes, sessão/OIDC, ações via admin, backup, telas | 012, 013 | rascunho |
| [017-recursos-importados.md](017-recursos-importados.md) | Recursos importados | I1–I24; contratos do `wwz-tools` e do `wwz-drift check` | 007, 009, 011 | rascunho |
| [018-verificacao-ci-release.md](018-verificacao-ci-release.md) | Verificação, CI e release | verify quick/full/heavy (passos), Actions, ensaio de release, SDKs, SemVer | 001, 002 | rascunho |
| [019-exemplos-e-producao.md](019-exemplos-e-producao.md) | Exemplos e produção | `examples/`, deploy de produção, loop autônomo, arquivos de implementação do autor (`autor/`, fora do git; ADR 0076) | 009, 015, 018 | rascunho |
| [020-cliente-claude-code.md](020-cliente-claude-code.md) | Cliente Claude Code | Perfil do Claude Code 2.1.308 (headers, betas, fingerprint, billing header, `JSON.stringify`), fixtures do cliente e verificação estrita `client_verification: claude-code` (403 `forged_request_error`) | 004, 005, 006 | rascunho |
| [090-pesquisa-e-referencias.md](090-pesquisa-e-referencias.md) | Pesquisa e referências | Inventário, pesquisas externas, roadmap original, fontes (informativa) | — | aprovada |
| [PLAN.md](PLAN.md) | Plano | Fases de implementação | todas | rascunho |

## Correspondência com o documento original

As specs migradas citam seções do documento Claude Docs ("Entra em: 7, 10" na tabela I1–I24, por exemplo). Correspondência: 1, 2, 13 → 000 · 3 → 004 · 4 → 003 · 5 → 007 · 6 → 008 · 7 → 009 · 8 → 010 · 9 → 011 · 10 → 013 · 11, 20 → 014 · 12, 15, 21, Fontes → 090 · 14 → 015 · 16 → 006 · 17 → 005 · 18 → 012 · 19 → 016 · 22 → 017 · 23 → 002, 018 e ADRs.

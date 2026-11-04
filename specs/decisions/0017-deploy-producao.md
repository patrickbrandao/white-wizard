# 0017 — Deploy de produção

Status: aceita
Data: 2026-10-30
Atualizada por: [0074](0074-assinatura-oauth-e-cliente-claude-code.md) (porta 8008) e [0076](0076-dados-do-autor-fora-do-git.md) (dados do servidor fora do git)

## Contexto
O humano cedeu um servidor próprio (x86_64, 56 núcleos, 125 GB), compartilhado com outras aplicações dele, com Docker e Traefik para a produção e os benchmarks pesados (`verify-heavy`).

## Decisão
- Acesso por SSH como root ao servidor do autor; tudo em `/opt/white-wizard`, projeto compose `wwz`, containers/volumes/redes `wwz*`. O host, a porta SSH, o IP público, o IP privado e os nomes HTTPS concretos não ficam no repositório: são **arquivos de implementação do autor** (`autor/prod.env` e `autor/haproxy.cfg`, fora do git; template versionado em `deploy/prod/autor.env.example`), ver [ADR 0076](0076-dados-do-autor-fora-do-git.md) e a spec [019](../019-exemplos-e-producao.md#arquivos-de-implementação-do-autor).
- Imagens amd64 construídas localmente (`docker buildx --platform linux/amd64`) e enviadas por `docker save | ssh … docker load`.
- HTTPS via o Traefik já existente no servidor (rede `network_public`, labels no padrão do Traefik do servidor), quatro nomes: inferência (todos os drivers por prefixo, `WWZ_HOST_INFERENCIA`), control plane (`WWZ_HOST_ADMIN`), painel (`WWZ_HOST_PANEL`) e Grafana (`WWZ_HOST_GRAFANA`).
- Portas diretas no IP público do servidor (`WWZ_PROD_PUBLIC_IP`): 8000 openai-perfeito, 8001 falhas-20pct, 8002 latência intermitente, 8003 indisponibilidade por janelas, 8004 anthropic, 8005 openrouter, 8006 opencode, 8007 admin; 8008 anthropic no modo estrito do Claude Code ([ADR 0074](0074-assinatura-oauth-e-cliente-claude-code.md) item 19). O haproxy `wwz-ports` (host network) publica as portas; o versionado em `deploy/prod/haproxy/` é o genérico (só o wwz, `mode tcp`) e o do servidor do autor fica em `autor/haproxy.cfg`.
- Inferência exige chaves `wwz-key-*` aleatórias; `WWZ_ADMIN_TOKEN` e senha do painel gerados; anotados só em `/opt/white-wizard/CREDENTIALS.md` (chmod 600), nunca no git.
- Valkey e Prometheus só na rede interna do projeto.
- Limites: só `/opt/white-wizard`, recursos `wwz*` e conexão à `network_public`; proibido tocar o Traefik, outros containers e diretórios de outras aplicações do host, configuração do Docker, firewall ou pacotes. Teto de 32 núcleos e 64 GB somados; benchmark pesado dentro do servidor pela rede interna, até 10 min por rodada, abortado se o load average passar de 48.
- Detalhes em [019](../019-exemplos-e-producao.md).

## Alternativas consideradas
- Só Docker Desktop local (não atinge as metas de vazão da 014).

## Consequências
- `deploy/prod/` guarda o compose e os templates (sem segredos e sem nenhum dado que identifique o servidor do autor; [ADR 0076](0076-dados-do-autor-fora-do-git.md)).
- Os scripts de produção e do heavy leem `autor/prod.env` e falham sem ele.

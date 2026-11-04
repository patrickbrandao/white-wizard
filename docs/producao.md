# Produção

Instância pública de testes do autor ([ADR 0017](../specs/decisions/0017-deploy-producao.md), [spec 019](../specs/019-exemplos-e-producao.md)). Os arquivos de deploy, o passo a passo e as regras do que não fazer estão em [deploy/prod/README.md](../deploy/prod/README.md). Este guia é o lado de quem consome.

Os hosts, o IP público e o acesso ao servidor não ficam no repositório: são arquivos de implementação do autor, em `autor/prod.env`, fora do git ([ADR 0076](../specs/decisions/0076-dados-do-autor-fora-do-git.md); modelo em [deploy/prod/autor.env.example](../deploy/prod/autor.env.example)). Abaixo eles aparecem com os valores de exemplo do modelo (`wwz.example.com`, `admin-wwz.example.com`, `panel-wwz.example.com`, `grafana-wwz.example.com` e o IP `203.0.113.10`); troque pelos da produção.

As chaves de inferência, o token admin e as senhas nascem no servidor (`/opt/white-wizard/CREDENTIALS.md`, chmod 600) e nunca ficam no repositório. Peça-as ao mantenedor.

## URLs

| Para quê | Endereço |
| --- | --- |
| Inferência, todos os drivers por prefixo | `https://wwz.example.com/<driver>` com `<driver>` em `openai`, `anthropic`, `openrouter`, `opencode`, `gemini`, `azure` |
| Control plane (admin, `/health`, `/metrics`) | `https://admin-wwz.example.com` (`/admin/*` exige o bearer admin) |
| Painel | `https://panel-wwz.example.com` |
| Grafana | `https://grafana-wwz.example.com` |

```sh
W=https://wwz.example.com
curl -s $W/openai/v1/chat/completions \
  -H "Authorization: Bearer $WWZ_KEY" -H 'content-type: application/json' \
  -d '{"model":"wwz-fast","messages":[{"role":"user","content":"oi"}],"max_tokens":20}'
```

Os caminhos por driver são os de [drivers.md](drivers.md) e [uso.md](uso.md#chamar-cada-driver) com o prefixo na frente (`$W/anthropic/v1/messages`, `$W/gemini/v1beta/models/wwz-fast:generateContent`, `$W/azure/openai/deployments/wwz-fast/chat/completions?api-version=2024-10-21`). No OpenCode lembre do `x-opencode-session`.

## Portas 8000 a 8008

Cada porta do IP público (`203.0.113.10` no exemplo) é uma instância com um papel, na raiz (sem prefixo, drivers resolvidos como em [drivers.md](drivers.md#mounts-e-detecção-na-raiz)). Usam store em memória, com contadores isolados.

| Porta | Papel |
| --- | --- |
| 8000 | OpenAI sem erros, latência realista |
| 8001 | 20 % de erros variados |
| 8002 | Latência de cauda pesada |
| 8003 | Indisponibilidade: 2 min a cada 10 min (UTC, minutos 0 e 1 de cada dezena) |
| 8004 | Driver Anthropic |
| 8005 | Driver OpenRouter |
| 8006 | Driver OpenCode Go |
| 8007 | Control plane da instância principal (`/admin/*`, bearer admin) |
| 8008 | A mesma instância da 8004 em TCP puro, com o modo estrito do Claude Code em `/claude-code` ([claude-code.md](claude-code.md)) |

## Medir a produção

```sh
wwz-load run --target http://203.0.113.10:8000 --key "$WWZ_KEY" --rps 50 --duration 30s
wwz-load top --admin https://admin-wwz.example.com --token "$WWZ_ADMIN_TOKEN"
```

Confira as flags com `wwz-load run -h` e `wwz-load top -h` ([wwz-load.md](wwz-load.md)). O smoke externo é `scripts/prod-smoke.sh` ([deploy/prod/README.md](../deploy/prod/README.md)).

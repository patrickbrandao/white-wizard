# 000 — Visão geral

Status: rascunho
Depende de: —
Origem: seções 1, 2, 13 do documento Claude Docs congelado em 2026-10-30

## Contexto
Propósito, escopo, requisitos obrigatórios R1–R24 e matriz de cobertura do White Wizard (seções 1, 2 e 13 do documento original).

## Requisitos
- R1–R24 (tabelas abaixo), todos condição de aceite

## Interfaces e contratos
- Endpoints de inferência: [004](004-apis-e-endpoints.md); control plane: [013](013-observabilidade-admin.md)

## Dados
- Nenhum dado persistente próprio; estado de controle em [012](012-estado-valkey.md)

## Critérios de aceite
- [ ] Todos os critérios de aceite de R1–R24 atendidos
- [ ] Matriz de cobertura sem item descoberto

## Fora de escopo
- Ver "Fora do escopo" na seção 1 abaixo

## Decisões relacionadas
- [specs/decisions/0001-decisoes-iniciais.md](decisions/0001-decisoes-iniciais.md)
- [specs/decisions/0002-fonte-da-verdade-specs.md](decisions/0002-fonte-da-verdade-specs.md)

## Comportamento

### 1. Objetivo e escopo

O sistema é um servidor em Go que finge ser um provedor de LLM (OpenAI, Anthropic, OpenCode Go, OpenRouter, Gemini e Azure OpenAI) para testar o roteador de modelos sob carga de milhares a dezenas de milhares de requisições por segundo, sem GPU e sem custo de API.

Ele recebe requisições reais do roteador, devolve respostas inventadas no formato exato de cada provedor (texto, raciocínio, chamadas de ferramentas, streaming), e simula o que um provedor faz de pior: latência variável, filas, erros, limites de taxa e quedas no meio do stream.

**Nome de trabalho:** `wwz` (binário único, sem dependências externas em runtime).

**Está no escopo**

- Endpoints de chat/mensagens da OpenAI e da Anthropic, com e sem streaming.
- Listagem de modelos declarados em configuração (`/v1/models` e equivalente Anthropic).
- Geração de tool calls válidas contra o JSON Schema enviado na requisição.
- Cenários sequenciais com estado derivado do histórico da conversa (agentes com ferramentas de arquivo).
- Controle de latência por etapa, injeção de erros, limites de req/s e tokens/s por chave.
- Métricas Prometheus e API administrativa para mudar a configuração em tempo de execução.

**Fora do escopo**

- Inferência real ou respostas semanticamente corretas.
- Embeddings, imagens, áudio, fine-tuning, vector stores (podem entrar depois como extensões; ver inventário na seção 12).
- Gerador de carga (ferramenta separada; ver seção 14).

### 2. Requisitos obrigatórios

Todos os itens abaixo são condição de aceite; nenhum é opcional.

| # | Requisito | Critério de aceite |
| --- | --- | --- |
| R1 | Escrito em Go, binário único | `go build` gera um executável estático; imagem Docker `FROM scratch` ou `distroless` |
| R2 | Compatível com OpenAI | SDK oficial `openai` (Python e Node) funciona apontando `base_url` para o simulador, incluindo helpers de streaming |
| R3 | Compatível com Anthropic | SDK oficial `anthropic` funciona apontando `base_url`, incluindo `messages.stream()` e eventos tipados |
| R4 | Modelos declarados em configuração | `GET /v1/models` (OpenAI) e `GET /v1/models` (Anthropic) listam exatamente os modelos do YAML, com os campos de cada formato |
| R5 | Respostas inventadas | Texto gerado de frases pré-definidas, lorem ipsum ou dataset; tamanho controlado por `max_tokens` e histograma configurável |
| R6 | Tool calls aleatórias no schema | Argumentos gerados a partir do JSON Schema de cada tool, válidos contra o schema; probabilidade e quantidade configuráveis; `tool_choice` respeitado (`auto`, `required`, `none`, tool nomeada) |
| R7 | Stream de raciocínio e resposta | Raciocínio no formato do provedor real (ADR 0022): OpenAI oculto, só contado em `usage` (com `delta.reasoning_content` opcional para gateways compatíveis); OpenCode `delta.reasoning_content` e OpenRouter `delta.reasoning` antes de `delta.content`; Anthropic: bloco `thinking` antes do bloco `text`; tokens de raciocínio sempre contados em `usage` |
| R8 | Latência variável em todas as etapas | Fila, TTFT, ITL do raciocínio, ITL do conteúdo, emissão de tool call, fechamento do stream — cada uma com distribuição e desvio configuráveis, mais fator de carga por concorrência |
| R9 | Simulação de erros | Erros HTTP (400, 401, 404, 413, 429, 500, 502, 503, 504, 529), erros no meio do stream, truncamento, chunk malformado, stream pendurado, desconexão TCP — todos com probabilidade e por chave/modelo |
| R10 | Controle de requisições por segundo | Limite de req/s, req/min, tokens/min e conexões simultâneas por chave de API e global, com 429 e headers de rate limit no formato de cada provedor |
| R11 | Templates de testes sequenciais | Cenários em YAML com passos ordenados; o passo é escolhido pelo estado da conversa (quantas tool results, última tool chamada); templates para ler, gravar, editar e apagar arquivos em sequência |
| R12 | Milhares a dezenas de milhares de req/s | Uma instância sustenta ≥ 10 000 req/s em modo sem latência e ≥ 20 000 streams abertos; escala horizontal sem estado compartilhado |
| R13 | Configuração via arquivo + runtime | YAML na inicialização; `POST /admin/config` altera latência, erros, limites e cenários sem reiniciar |
| R14 | Observabilidade | `/metrics` Prometheus com req/s, erros injetados por tipo, TTFT/ITL observados, conexões abertas, fila; `/health` e `/health/ready` |

**Decisões já tomadas**

- Linguagem: Go (R1). Rust foi considerado e descartado pelo usuário.
- Sem estado no servidor: o estado de um cenário é derivado do histórico de mensagens da própria requisição (R11 + R12).
- Semente aleatória configurável (`seed`) para reproduzir uma execução de teste.

**Requisitos adicionais** (segunda rodada)

| # | Requisito | Critério de aceite |
| --- | --- | --- |
| R15 | Drivers de provedor | OpenAI, Anthropic, OpenCode Go, OpenRouter, Gemini e Azure OpenAI ([ADR 0043](decisions/0043-drivers-gemini-e-azure.md)) como drivers separados sobre uma interface comum; um driver novo não altera o núcleo (seção 17) |
| R16 | Fidelidade aos provedores reais | Limites, janelas, bloqueios, banimentos, headers e erros de cada driver reproduzem o que o provedor documenta (seção 16) |
| R17 | Estado de controle em Valkey | Chaves, contas, usuários e IPs com contadores, bloqueios e banimentos no Valkey, válidos para o cluster inteiro; modo local sem Valkey para uma instância (seção 18) |
| R18 | Painel e dashboards | Uso da inferência falsa, erros, entidades bloqueadas e banidas, em tempo real, com ações de operador (seção 19) |
| R19 | Milhões a dezenas de milhões de req/min | Topologia validada a 10 milhões de req/min (167 000 req/s) com 20 instâncias e Valkey Cluster; caminho para 30 milhões (seção 20) |

**Requisitos adicionais** (terceira rodada, importados das pesquisas externas — seção 22)

| # | Requisito | Critério de aceite |
| --- | --- | --- |
| R20 | Tool calls deliberadamente quebradas | Catálogo I1 (JSON inválido por estilo, violação de schema por tipo, tool desconhecida, id duplicado, corte no meio dos argumentos) acionável por taxa, por passo e por header |
| R21 | Caos escopado por teste | Toda injeção de erro e latência aceita `X-Test-Id`; dois testes paralelos no mesmo cluster não interferem; erro one-shot por `X-Test-Id` |
| R22 | Controle por header de requisição | `x-wwz-response`, `x-wwz-tool-calls`, `x-wwz-return-error`, `x-wwz-delay-ms`, `x-wwz-seed` funcionam e podem ser desligados globalmente |
| R23 | Relatório de resiliência do cliente | Para um `X-Test-Id`, o simulador informa retries, backoff observado, respeito a `Retry-After` e reenvio após stream consumido |
| R24 | Cenários com timing e falha por passo | `stall`, `cut_after_tokens`, `fail_first`, `speed`, `step_limit`, `on_exhausted` e ids de tool call decodificáveis, tudo stateless (apenas `attempts` no Valkey) |

### 13. Matriz de cobertura

O `wwz` cobre as 14 linhas; a melhor ferramenta pronta (llm-d-inference-sim) cobre 8, e nenhuma cobre cenários sequenciais com estado.

| Recurso | llm-d-sim | FakeAI | llmposter | Phoenix mock | acus9345 | mockllm e cia. | **WWZ** |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Go, binário único | Sim | Não (Python) | Não (Rust) | Não (TS) | Não (TS) | Não (Python) | **Sim** |
| OpenAI Chat + `/v1/models` | Sim | Sim | Sim | Sim | Sim | Sim | **Sim** |
| Anthropic Messages + `/v1/models` | Não | Não | Sim | Sim | Sim | Parcial | **Sim** |
| Modelos declarados em config | Sim | Catálogo fixo | Fixture | Fixo | Fixo | Sim | **Sim** |
| Respostas aleatórias (inventadas) | Sim | Sim | Não | Sim | Não | Não | **Sim** |
| Tool calls aleatórias no schema | Sim | Sim | Não | Sim | Não | Não | **Sim** |
| Reasoning em stream (OpenAI) | Não | Sim | Não | Não | Sim | Não | **Sim** |
| Thinking em stream (Anthropic) | Não | Não | Não | Não | Sim | Não | **Sim** |
| Latência por etapa com distribuição | TTFT, ITL, fila | TTFT, ITL | Jitter | Delay fixo | Delay fixo | Lag | **7 etapas** |
| Fator de carga por concorrência | Sim | Sim | Não | Não | Não | Não | **Sim** |
| Erros pré-resposta | 6 tipos | 6 tipos | Sim | Não | Sim | Não | **15 tipos** |
| Erros mid-stream | Não | Não | Sim | Não | Sim | Não | **8 tipos** |
| Rate limit por chave (rpm/tpm) | Não | Sim | Não | Sim | Não | Não | **Sim + rps** |
| Cenários sequenciais com estado | Não | Não | Não | Não | Roteiro linear | `times: N` | **Sim** |
| Config em runtime | Sim | Não | Não | Dashboard | API | Hot reload | **Sim** |
| Métricas Prometheus | Sim (vLLM) | Sim | Não | Não | Não | Não | **Sim** |
| ≥ 10 000 req/s por instância | Sim | Não | Provável | Não | Não | Não | **Meta R12** |

# 0039 — Remoção de tier atribuído em `PUT /api/v1/policies`

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
O contrato do painel (`cmd/wwz-panel/openapi.yaml`, task 041) dizia que `PUT /api/v1/policies` recusa a remoção de um tier "ainda atribuído a uma entidade e que não exista no YAML". Fato novo (task 053): `internal/panel/api/control.go` (`PublishPolicies`) recusa a remoção de **qualquer** tier de `cfg:policies` ainda atribuído a uma entidade (`ent:*`), exista ou não no YAML. O painel só enxerga a configuração efetiva do wwz, em que os tiers do YAML e os das políticas já vêm mesclados, e não consegue saber se a atribuição sobreviveria; uma versão com atribuição órfã seria descartada inteira pelo wwz (`wwz_policies_errors_total`).

## Decisão
- O contrato segue o código: `PUT /api/v1/policies` responde 400 (com a lista `"<tier>: atribuído a <tipo>:<id>"` em `details`) quando um tier presente em `cfg:policies` e ausente do corpo ainda está atribuído a alguma entidade, independentemente do YAML.
- Tiers que só existem no YAML não estão em `cfg:policies` e por isso nunca contam como removidos.
- Para remover um tier atribuído, o operador primeiro troca ou limpa o tier das entidades (`POST /api/v1/entities/{kind}/{id}/tier`, `tier: null`) e depois publica.

## Alternativas consideradas
- Ler os tiers do YAML por uma rota nova do admin para permitir a remoção quando o YAML cobre o nome: amplia o contrato do wwz por um caso raro.
- Deixar o wwz decidir: o erro só apareceria como métrica, depois da publicação, sem retorno ao operador.

## Consequências
- `cmd/wwz-panel/openapi.yaml` (descrição de `PUT /api/v1/policies`) atualizado. Sem mudança de código.

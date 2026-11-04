# 0026 — Precedência de parâmetros: chave antes do modelo

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-31

## Contexto
A spec 017 (I3) fixava a ordem `header > passo > modelo > chave > global`; a spec 008 ("Por chave e por modelo") dizia `chave > modelo > default`. O código (`internal/engine/engine.go`) resolve só o perfil do modelo e ignora `keys[].latency` e `keys[].errors` (achado da task 070, `imported_i3_test.go`). A divergência entre specs impede a correção (task 080).

## Decisão
- Ordem única para latência, erros e `seed`: **header por requisição > passo de cenário > chave > modelo > global (perfil `default`)**. Quem vem antes vence.
- A unidade sobreposta é o perfil inteiro (`keys[].latency`/`keys[].errors` nomeiam um perfil de `profiles.*`); não há mescla campo a campo entre chave, modelo e global. Os headers `x-wwz-*` e os campos de passo sobrescrevem campos isolados do perfil resolvido.
- Motivo: o uso principal do wwz é um roteador que trata cada chave como um provedor distinto apontando para a mesma instância; o perfil da chave precisa valer para todos os modelos que ela chama. Para que a mesma chave veja modelos com latências diferentes, basta não declarar `latency`/`errors` na chave.

## Alternativas consideradas
- `modelo > chave` (spec 017): a chave só valeria para modelos sem perfil explícito; como todo modelo resolve ao menos o perfil `default`, a chave quase nunca teria efeito.
- Mescla campo a campo: mais flexível, porém ambígua para distribuições (trocar `mean` sem trocar `dist`) e custosa no caminho quente.

## Consequências
- Specs 008 e 017 alinhadas.
- Task 080 implementa a cadeia no engine.

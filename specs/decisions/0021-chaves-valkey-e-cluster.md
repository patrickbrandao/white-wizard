# 0021 — Layout de chaves no Valkey e avaliação em Valkey Cluster

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-30

## Contexto
A spec 012 previa um único `EVALSHA` avaliando todas as entidades da requisição (ip, chave, usuário, projeto, conta). Em Valkey Cluster as chaves de entidades diferentes caem em slots diferentes, e um script só toca chaves de um slot. A tabela de chaves do documento original também usava uma chave por contador (`rl:`, `tb:`, `spend:`, `block:`, `ban:`, `credits:`), o que multiplica `KEYS` por requisição e deixa janelas antigas espalhadas.

## Decisão
- **Hash tag por entidade:** toda chave de uma entidade tem a forma `<prefix>:<sub>:{<tipo>:<id-escapado>}`, e todas caem no mesmo slot.
- **Chaves consolidadas:** estado, bloqueio, banimento e créditos ficam num hash `ent`; todos os token buckets num hash `tb`; as janelas fixas (minuto, dia, mês) num hash `win`, com início e contagem por unidade; as janelas deslizantes (5 h, semana, rampa) em hashes de baldes; `conc` e `strikes` ficam separados. Esquema literal em `cache/valkey/README.md`, que é normativo.
- **Standalone (`store.cluster: false`):** um único `EVALSHA check` com as chaves de todas as entidades. O script lê tudo, decide e só grava se todas permitirem (não precisa reverter nada).
- **Cluster (`store.cluster: true`):** um `EVALSHA check_entity` por entidade, todos no mesmo pipeline. Se alguma negar, vale a primeira negação na ordem dos sujeitos, e o store envia em pipeline um `release` para cada entidade que permitiu, devolvendo exatamente o que ela consumiu.
- `check` e `check_entity` compartilham a mesma biblioteca Lua: a lógica por entidade é escrita uma vez.
- `finish` é sempre por entidade, em pipeline, nos dois modos.
- Os índices (`idx:ent`, `idx:status:*`, `idx:inst`) ficam fora da hash tag e são atualizados por comandos comuns no mesmo pipeline, sem atomicidade; `List` corrige índices obsoletos ao ler.
- O relógio é o da instância (`now` em ARGV), não o `TIME` do Valkey, para que o store em memória e o Valkey passem a mesma suíte com relógio injetado.

## Alternativas consideradas
- Uma hash tag comum a todas as entidades da requisição: impossível, as entidades são compartilhadas entre requisições diferentes.
- `MULTI`/`WATCH` entre slots: não existe em cluster.
- Sempre um script por entidade, inclusive em standalone: código mais simples, porém N scripts por requisição onde um basta.

## Consequências
- Em cluster, uma requisição negada consome cota de outras entidades por alguns microssegundos, até o `release`. Requisições concorrentes nesse intervalo podem ser negadas a mais, mas nunca liberadas a mais.
- A tabela de chaves da spec 012 passa a apontar para o README.

## Nota de 2026-10-31 (task 056, após a implementação da task 013)
A decisão segue válida; o store implementado precisou de três esclarecimentos, já refletidos no README:
- Banimento e bloqueio são marcas independentes no hash `ent` (campos `ban_*` e `blk_*`), como no store em memória; o campo `status` é só um cache gravado por `set_status`, e os índices `idx:status:*` significam "tem a marca".
- Em cluster, "devolvendo exatamente o que ela consumiu" vale para buckets, janelas, rampa, `conc` e `requests`: o `release` não desfaz `count` (base de `after_n`) nem a inicialização de `credits`. Como todos os `check_entity` rodam, `count` cresce em todas as entidades da requisição, inclusive nas posteriores à que negou.
- Uma negação servida pelo cache local de ban/block não chega ao Valkey e não incrementa `count`.

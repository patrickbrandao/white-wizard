# 0019 — Formato dos eventos do `ev:stream`

Status: aceita (revisão com o humano em 2026-10-31)
Data: 2026-10-30

## Contexto
O rascunho do `eventbus/valkey/asyncapi.yaml` descrevia eventos com `kind`, `ts` e `instance`. O código (`limits.Event`, já servido pelo store em memória e lido por `Store.Events`) usa `t`, `type`, `driver`, `model`, `entity`, `status`, `error` e `fields`. Com dois formatos para a mesma coisa, o painel e o `/admin` teriam de converter, e o store Valkey (tarefa 013) não saberia qual gravar.

## Decisão
- O formato canônico é o JSON de `limits.Event`, acrescido de três campos de topo: `instance` (quem gravou), `request_id` e `test_id` (quando existirem). `request_id` sai de `fields` e vira campo de topo; os demais itens de `fields` continuam livres.
- Cada entrada do stream tem **um único campo** `d` com esse JSON (`XADD <prefix>:ev:stream MAXLEN ~ <store.event_max_len> * d <json>`). Campos planos foram descartados porque `entity` e `fields` são aninhados.
- `type` ∈ `request`, `error`, `block`, `unblock`, `ban`, `unban`, `strike`. O `limit` do rascunho sai: negações de limite aparecem como `error` com o `status` HTTP gravado pelo `Finish`.
- A amostragem `store.event_sample` só vale para `request` e `error` (log por requisição); `block`, `unblock`, `ban`, `unban` e `strike` nunca são amostrados. O evento amostrado leva `sample` (a taxa) para o painel extrapolar.
- `instance` = `POD_NAME` se definido, senão `os.Hostname()` (sem variável nova).
- O painel consome pelo grupo `panel` (`XREADGROUP`, `XACK`), criado com `XGROUP CREATE ... $ MKSTREAM`; `cfg:changed` permanece como no rascunho.

## Alternativas consideradas
- Manter o rascunho (`kind`/`ts`) e mudar o código: quebraria `/admin` e o store em memória já testados.
- Campos planos no XADD: filtro mais barato no Valkey, mas exige achatar `entity`/`fields` e versionar cada campo.

## Consequências
- `limits.Event` ganha `Instance`, `RequestID` e `TestID`; `FinishRequest` ganha `TestID` (tarefa 013). O store em memória passa a preencher os mesmos campos.
- O painel decodifica um JSON por entrada; custo aceito.

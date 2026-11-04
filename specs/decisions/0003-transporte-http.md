# 0003 — fasthttp no plano de dados, net/http no plano de controle

Status: aceita
Data: 2026-10-30

## Contexto
A meta é ≥ 10 000 req/s e ≥ 20 000 streams por instância com overhead p99 < 2 ms ([014](../014-desempenho-e-escala.md)). A seção 11 inicialmente preferia net/http e deixou a escolha aberta.

## Decisão
Data plane em `fasthttp` com `reuseport` (um listener por núcleo) e streaming via `SetBodyStreamWriter`; admin, `/metrics`, `/health` e painel em `net/http`. `core.ResponseWriter` isola drivers do transporte.

## Alternativas consideradas
- Só net/http: mais simples, suporta HTTP/2, mas aloca mais por requisição.

## Consequências
- Sem HTTP/2 no data plane (roteadores não usam com provedores).
- Trocar o transporte não exige mudar drivers.

# latencia-intermitente

Picos de latência periódicos: 3 s lentos a cada 10 s. Serve para testar timeout,
hedging, circuit breaker e reação do roteador a um provedor que "engasga" de
tempos em tempos.

## O que demonstra

O perfil de latência (spec [008](../../specs/008-latencia.md)) descreve o tempo de
cada etapa, mas não varia no tempo. O gatilho periódico do wwz é o `burst` do
perfil de erros (spec [009](../../specs/009-erros-e-caos.md)):
`burst: {every: 10s, duration: 3s, rate: 100}` troca a taxa de falha por 100%
durante os primeiros 3 s de cada ciclo de 10 s (a fase conta desde a subida do
processo). O tipo sorteado na janela é `gateway_timeout` com `timeout_delay: 1s`:

- **Fora da janela (7 s de cada 10):** resposta 200 com TTFT ~30 ms (normal, desvio 9 ms).
- **Na janela (3 s de cada 10):** a chamada fica 1 s pendurada e termina em 504.

Ou seja, o "pico" é uma chamada lenta que falha (um provedor que trava e estoura o
timeout do gateway), não uma resposta lenta bem-sucedida: o wwz não tem hoje um
pico de latência periódico com sucesso (ver o relatório da task 204).
`slow_headers` também funciona com `burst`, e o atraso dele vem de
`errors.slow_headers_delay` (padrão 15 s).

## Como subir

```bash
make image
cd examples/latencia-intermitente
EX_PORT=18103 docker compose up -d --wait
# uma chamada por 0,2 s, mostrando o tempo total: 1 s aparece em blocos de ~3 s a cada 10 s
for i in $(seq 1 100); do
  curl -s -o /dev/null -w '%{time_total} %{http_code}\n' -H 'Authorization: Bearer wwz-key-demo' \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"gpt-4o-mini\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"oi $i\"}]}" \
    http://127.0.0.1:18103/v1/chat/completions; sleep 0.2
done
docker compose down -v
```

Chave: `wwz-key-demo`; modelo `gpt-4o-mini`. Para outro ritmo, mude `every`,
`duration` e `timeout_delay` em `config.yaml`.

## Smoke

`./smoke.sh` (ou `make example NAME=latencia-intermitente`, ~1 min):

- 300 chamadas a ~10/s em segundo plano (uma chamada lenta não atrasa as seguintes).
- razão p95/p50 do tempo total >= 5 (esperado ~20: p50 ~ 50 ms, p95 ~ 1 s);
- lentas (>= 0,8 s) ~30% e rápidas (< 0,4 s) ~70% do total, nenhuma intermediária,
  e toda rápida responde 200;
- janelas lentas alternadas com rápidas: o espaço entre inícios de janelas
  consecutivas é de 10 s +- 2 s e o número de janelas bate com a duração da amostra.

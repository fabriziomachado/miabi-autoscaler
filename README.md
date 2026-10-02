# miabi-autoscaler

Escala horizontalmente apps **service** (Docker Swarm) do [Miabi](https://miabi.io) a partir do
**trafego HTTP** do proprio app. Um binario Go estatico, sem Docker CLI, SSH ou systemd: fala so com a API do Miabi.

## Como decide
1. Le `GET /workspaces/{ws}/analytics/summary?app=ID` e calcula o req/s medio dos ultimos minutos **completos**
   (o minuto em andamento e ignorado). O analytics so publica cada minuto ~98s depois de ele fechar; por isso os
   `analyticsDelay` (padrao 2m) minutos mais recentes tambem sao ignorados, senao viram "sem trafego". Guarda tambem o p95 e a taxa de 5xx.
2. `recomendado = ceil(req/s / targetRPSPerReplica)`, com tolerancia (padrao 10%).
3. Estabilizacao no estilo do HPA do Kubernetes: **sobe** so se a recomendacao ficou alta durante toda a janela
   `scaleUp.stabilization`; **desce** so ate a maior recomendacao vista em `scaleDown.stabilization`.
   Passo maximo por ajuste (`maxStep`) e limites `min`/`max`.
4. Aplica com `POST /apps/{id}/scale` (imediato, sem rollout).

Falha segura: sem dados do analytics, com tasks ainda subindo ou com app fora do modo service, **nao escala**.
Ao reiniciar, assume as replicas atuais como recomendacao recente, entao nao reduz de imediato.
O sinal (trafego total) nao depende do numero de replicas, por isso nao oscila quando ele muda.

## Configuracao
Veja `config.example.yaml`. Tudo e por app, com `defaults`. Variaveis de ambiente: `MIABI_URL`, `MIABI_WORKSPACE`,
`MIABI_TOKEN` (chave de API de workspace com escopos Read, Write e Deploy), `CONFIG_FILE`, `LISTEN_ADDR`.

## Endpoints (porta 8080)
- `/healthz`: vivo.
- `/metrics`: Prometheus (`miabi_autoscaler_*`: replicas, desejadas, req/s, p95, erros, eventos de escala).
- `/status`: ultima decisao de cada app, em JSON.

## Build e testes
```sh
docker build -t miabi-autoscaler:dev --build-arg VERSION=0.1.0 .   # roda vet + testes no build
docker run --rm -v "$PWD":/src -w /src golang:1-alpine go test ./...
```
Teste sem risco: `dryRun: true` mostra nos logs o que seria feito.

## Limitacoes
- O analytics tem granularidade de 1 minuto e atraso de publicacao (~98s): a reacao a um pico leva de 3 a 5 minutos
  (`analyticsDelay` + janela + `scaleUp`).
- Apps com volume local ficam como container fixo (nao sao service) e sao ignorados.
- A chave de API pode ter "Allowed IPs"; o IP que o Miabi enxerga depende da rede (NAT do roteador, por exemplo).

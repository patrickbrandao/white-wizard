# syntax=docker/dockerfile:1
# White Wizard: imagens do wwz, wwz-panel e wwz-load (ADR 0007, spec 018).
#
#   docker build -t white-wizard:dev --build-arg VERSION=$(git describe --tags --always) \
#     --build-arg COMMIT=$(git rev-parse HEAD) .
#   docker build --target wwz-panel -t white-wizard-panel:dev .
#   docker build --target wwz-load  -t white-wizard-load:dev .
#   docker buildx build --platform linux/amd64,linux/arm64 -t white-wizard:dev .
#
# Um único estágio de build compila os cinco binários (wwz, wwz-panel, wwz-load,
# wwz-tools, wwz-drift; a mesma camada serve aos três alvos); o alvo padrão
# (último estágio) é o wwz. A imagem do wwz-load leva também o wwz-tools (ADR
# 0034, sem imagem própria) e o wwz-drift (ferramentas do lado do cliente).
# VERSION e COMMIT entram por -X main.version e -X main.commit. Build cruzado no
# BUILDPLATFORM (sem emulação); binários estáticos (CGO_ENABLED=0; o SQLite do
# painel é o modernc.org/sqlite, Go puro, ADR 0044); imagens finais distroless
# sem shell nem toolchain, usuário nonroot (65532). Cada imagem leva /LICENSE
# (Apache-2.0) e /THIRD_PARTY_NOTICES (dependências de terceiros; task 260).

ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly

# Dependências antes do código: camada reaproveitada enquanto go.mod/go.sum não mudam.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Health check mínimo para a imagem sem shell: GET na URL, sai 0 em 2xx.
COPY <<'GO' /healthcheck/main.go
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	url := "http://127.0.0.1:9000/health/ready"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		os.Exit(1)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		os.Exit(1)
	}
}
GO

COPY . .

ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG COMMIT=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/ \
      ./cmd/wwz ./cmd/wwz-panel ./cmd/wwz-load ./cmd/wwz-tools ./cmd/wwz-drift && \
    cd /healthcheck && GOOS=$TARGETOS GOARCH=$TARGETARCH GOFLAGS= GO111MODULE=off \
      go build -trimpath -ldflags "-s -w" -o /out/healthcheck . && \
    mkdir -p /vol/data /vol/backup

# Painel: estado só em volume (/data para WWZ_PANEL_DB, /backup para os
# snapshots). Os diretórios saem da imagem com dono nonroot e o volume nomeado
# herda o dono na primeira montagem.
FROM gcr.io/distroless/static-debian12:nonroot AS wwz-panel
ARG VERSION=dev
LABEL org.opencontainers.image.title="white-wizard-panel" \
      org.opencontainers.image.description="Painel do cluster White Wizard (wwz-panel)" \
      org.opencontainers.image.source="https://github.com/patrickbrandao/white-wizard" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/wwz-panel /out/healthcheck /usr/local/bin/
COPY --from=build /src/LICENSE /src/THIRD_PARTY_NOTICES /
COPY --from=build --chown=65532:65532 /vol/data /data
COPY --from=build --chown=65532:65532 /vol/backup /backup
ENV WWZ_PANEL_LISTEN=:8100 \
    WWZ_PANEL_DB=/data/panel.db \
    WWZ_PANEL_BACKUP_DIR=/backup
VOLUME ["/data", "/backup"]
USER nonroot:nonroot
EXPOSE 8100
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 \
  CMD ["/usr/local/bin/healthcheck", "http://127.0.0.1:8100/health/ready"]
ENTRYPOINT ["/usr/local/bin/wwz-panel"]
CMD ["serve"]

# Ferramentas do lado do cliente: wwz-load (benchmark, processo de vida curta),
# wwz-tools (ADR 0034; `wwz-tools serve` em :9200, saúde em /healthz pelo
# healthcheck) e wwz-drift. Sem ENTRYPOINT: o comando escolhe o binário pelo
# PATH (/usr/local/bin), p.ex. `docker run --rm white-wizard-load:dev wwz-tools
# version` ou `... wwz-load run --target http://wwz:8000`.
FROM gcr.io/distroless/static-debian12:nonroot AS wwz-load
ARG VERSION=dev
LABEL org.opencontainers.image.title="white-wizard-load" \
      org.opencontainers.image.description="Cliente de benchmark do White Wizard (wwz-load, wwz-tools, wwz-drift)" \
      org.opencontainers.image.source="https://github.com/patrickbrandao/white-wizard" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/wwz-load /out/wwz-tools /out/wwz-drift /out/healthcheck /usr/local/bin/
COPY --from=build /src/LICENSE /src/THIRD_PARTY_NOTICES /
USER nonroot:nonroot
EXPOSE 9200
CMD ["wwz-load", "help"]

# Alvo padrão (último estágio): o simulador.
FROM gcr.io/distroless/static-debian12:nonroot AS wwz
ARG VERSION=dev
LABEL org.opencontainers.image.title="white-wizard" \
      org.opencontainers.image.description="Simulador de provedores de LLM (wwz)" \
      org.opencontainers.image.source="https://github.com/patrickbrandao/white-wizard" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/wwz /out/healthcheck /usr/local/bin/
COPY --from=build /src/LICENSE /src/THIRD_PARTY_NOTICES /
USER nonroot:nonroot
ENV GOGC=200
EXPOSE 8000 9000
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=10 \
  CMD ["/usr/local/bin/healthcheck"]
ENTRYPOINT ["/usr/local/bin/wwz"]
CMD ["serve"]

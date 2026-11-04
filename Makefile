# White Wizard: verificação e build (specs/018, ADR 0008).
#
#   make verify-quick                                   # padrão
#   make verify-quick -- ./internal/scenario -run TestX # alvos explícitos
#   make verify-quick ARGS="./internal/scenario -run TestX"
#   make verify-full | verify-heavy                     # sob pedido
#   make build                                          # binários em ./bin/
#   make image | image-panel | image-load | image-multiarch  # imagens locais (sem push, ADR 0018)
#   make examples | make example NAME=falhas-20pct     # smokes de examples/ (ADR 0016)
#   make smoke                                        # compose + smoke (WWZ_STORE=memory make smoke)
#   make prod-build | prod-deploy | prod-smoke | prod-test  # produção (ADR 0017; deploy e smoke mexem no servidor)
#   make release-dryrun                                 # ensaio da release: 3 imagens multi-arch + 20 binários (sem push)

SHELL := /bin/bash
GO    ?= go

VERIFY_GOALS := verify-quick verify-full verify-heavy test-quick test-full

# Tudo o que vem depois de um alvo de verificação na linha de comando é repassado
# ao scripts/verify.sh (o `--` é consumido pelo make) e vira alvo no-op.
ifneq ($(filter $(VERIFY_GOALS),$(firstword $(MAKECMDGOALS))),)
EXTRA_ARGS := $(wordlist 2,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS))
ifneq ($(strip $(EXTRA_ARGS)),)
.PHONY: $(EXTRA_ARGS)
$(eval $(EXTRA_ARGS):;@:)
endif
endif
VERIFY_ARGS := $(strip $(EXTRA_ARGS) $(ARGS))

.DEFAULT_GOAL := verify-quick
.PHONY: verify-quick verify-full verify-heavy examples example test-quick test-full build image image-panel image-load image-multiarch smoke release-dryrun prod-build prod-deploy prod-smoke prod-test

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse HEAD 2>/dev/null)
BUILD_ARGS = --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT)
IMAGE       ?= white-wizard:dev
PANEL_IMAGE ?= white-wizard-panel:dev
LOAD_IMAGE  ?= white-wizard-load:dev
PLATFORMS ?= linux/amd64,linux/arm64

verify-quick:
	@scripts/verify.sh quick $(if $(VERIFY_ARGS),-- $(VERIFY_ARGS))

verify-full:
	@scripts/verify.sh full

verify-heavy:
	@scripts/verify.sh heavy

test-quick: verify-quick

test-full: verify-full

build:
	@mkdir -p bin
	@if [ -d cmd ] && ls -d cmd/*/ >/dev/null 2>&1; then \
		$(GO) build -trimpath -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/ ./cmd/...; \
	else \
		$(GO) build ./...; \
	fi

image:
	docker build $(BUILD_ARGS) -t $(IMAGE) .

# Mesmo Dockerfile multi-stage, alvos do painel e do cliente de carga (sem push, ADR 0018).
image-panel:
	docker build --target wwz-panel $(BUILD_ARGS) -t $(PANEL_IMAGE) .

image-load:
	docker build --target wwz-load $(BUILD_ARGS) -t $(LOAD_IMAGE) .

# Multi-arch só no cache do buildx (sem --push; publicação é do humano, ADR 0018).
image-multiarch:
	docker buildx build --platform $(PLATFORMS) $(BUILD_ARGS) -t $(IMAGE) .

smoke:
	@WWZ_VERSION=$${WWZ_VERSION:-$(VERSION)} scripts/smoke.sh

# Ensaio local do release.yml (spec 018, ADR 0018): OCI multi-arch das 3 imagens
# conferido e apagado, 20 binários com SHA256SUMS em tmp/release/. Sem push, sem tag.
release-dryrun:
	@VERSION=$(VERSION) COMMIT=$(COMMIT) scripts/release-dryrun.sh

# Smokes de examples/ (ADR 0016): todos, ou um com NAME=<exemplo>. Exige a imagem (make image).
examples:
	@scripts/verify.sh examples

example:
	@test -n "$(NAME)" || { echo "uso: make example NAME=<exemplo>" >&2; exit 64; }
	@scripts/verify.sh examples $(NAME)

# Produção (ADR 0017, spec 019, deploy/prod/README.md); servidor em autor/prod.env (ADR 0076).
# prod-build: só imagens amd64 locais; prod-deploy: build, envio, sync, credenciais e up
# (escreve só em /opt/white-wizard e nos recursos wwz*); prod-smoke: smoke externo;
# prod-test: testes de deploy/prod sem acessar o servidor (entra no verify-quick).
prod-build:
	@scripts/prod-deploy.sh build

prod-deploy:
	@scripts/prod-deploy.sh deploy

prod-smoke:
	@scripts/prod-smoke.sh --ssh

prod-test:
	@deploy/prod/test.sh

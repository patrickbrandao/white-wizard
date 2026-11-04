#!/usr/bin/env bash
# Ensaio local da release (spec 018, "Ensaio local da release"; ADR 0018: sem
# push e sem tag). Reproduz o que o .github/workflows/release.yml faria:
#
#   1. Confere o CHANGELOG.md ([Não lançado] não vazio) e que `wwz version`
#      imprime a versão e o commit injetados por -ldflags.
#   2. Para cada alvo (wwz, wwz-load, wwz-panel), um por vez: buildx multi-arch
#      (linux/amd64 + linux/arm64) num builder docker-container `wwz-builder`
#      com saída OCI em tmp/release/<alvo>.tar; confere no index.json as duas
#      arquiteturas, grava os digests em tmp/release/manifests.txt e apaga o tar.
#   3. Compila os 20 binários (wwz, wwz-load, wwz-panel, wwz-tools, wwz-drift ×
#      linux/darwin × amd64/arm64) um por vez, grava o sha256 em
#      tmp/release/SHA256SUMS e apaga cada binário.
#   4. Ao fim (sucesso ou falha): `docker buildx prune -af --builder wwz-builder`
#      e remoção de qualquer tar, binário ou cache de compilação que tenha
#      sobrado. Os binários cruzados usam um GOCACHE próprio em
#      tmp/release/gocache (apagado no fim) para não inchar o cache do host.
#
#   make release-dryrun
#   VERSION=v0.1.0 make release-dryrun
#
# Variáveis: VERSION (padrão git describe), COMMIT (padrão git rev-parse HEAD),
# RELEASE_OUT (padrão tmp/release), RELEASE_MIN_FREE_GB (padrão 3),
# RELEASE_TARGETS (padrão "wwz wwz-load wwz-panel"), RELEASE_SKIP_IMAGES=1 e
# RELEASE_SKIP_BINARIES=1 (pulam uma das metades; o resultado sai incompleto).
# PLATFORMS vem de .github/workflows/release.yml (env.PLATFORMS); sem a chave o
# ensaio falha. Cada binário tem de sair com CGO_ENABLED=0 (go version -m) e o
# estágio final das imagens tem de ser distroless/static ou scratch (R1).
# RELEASE_SOURCE_ONLY=1 só define as funções (selftest do verify).
# Sai ≠ 0 se faltar alguma arquitetura, algum binário ou a seção do CHANGELOG.
# Requer docker (buildx), go, jq e tar.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
OUT="${RELEASE_OUT:-tmp/release}"
MIN_FREE_GB="${RELEASE_MIN_FREE_GB:-3}"
BUILDER=wwz-builder
read -r -a TARGETS <<<"${RELEASE_TARGETS:-wwz wwz-load wwz-panel}"
CMDS=(wwz wwz-load wwz-panel wwz-tools wwz-drift)

# release_platforms <release.yml>: valor de PLATFORMS no bloco env do workflow.
release_platforms() {
	awk '$1 == "PLATFORMS:" { print $2; exit }' "$1" 2>/dev/null
}

# dockerfile_final_base <Dockerfile> <estágio>: imagem base do estágio (FROM ... AS estágio).
dockerfile_final_base() {
	awk -v t="$2" 'toupper($1) == "FROM" { for (i = 2; i < NF; i++) if (toupper($i) == "AS" && $(i + 1) == t) { b = $(i - 1); if (b ~ /^--platform=/) b = ""; print b; exit } }' "$1"
}

# final_base_static <imagem>: distroless/static (qualquer tag) ou scratch.
final_base_static() {
	case "$1" in
	scratch | gcr.io/distroless/static | gcr.io/distroless/static:* | gcr.io/distroless/static-*) return 0 ;;
	*) return 1 ;;
	esac
}

# binary_static_ok <binário>: o build info registra CGO_ENABLED=0.
binary_static_ok() {
	go version -m "$1" 2>/dev/null | grep -Eq '^[[:space:]]+build[[:space:]]+CGO_ENABLED=0$'
}

[[ "${RELEASE_SOURCE_ONLY:-0}" == "1" ]] && return 0

# Nunca um valor fixo aqui: o ensaio segue o workflow.
PLATFORMS="$(release_platforms .github/workflows/release.yml)"
if [[ -z "$PLATFORMS" ]]; then
	echo "release-dryrun: PLATFORMS ausente em .github/workflows/release.yml" >&2
	exit 1
fi

for tool in docker go jq tar; do
	command -v "$tool" >/dev/null || {
		echo "release-dryrun: $tool é obrigatório" >&2
		exit 2
	}
done

if command -v sha256sum >/dev/null; then
	sha256() { sha256sum -- "$1" | awk '{print $1}'; }
else
	sha256() { shasum -a 256 -- "$1" | awk '{print $1}'; }
fi

free_gb() { df -Pk "$ROOT" | awk 'NR == 2 { printf "%.1f", $4 / 1048576 }'; }

FAILS=0
pass() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*" >&2
	FAILS=$((FAILS + 1))
}

mkdir -p "$OUT"
# Artefatos de uma execução anterior não contam.
rm -f "$OUT"/*.tar "$OUT/manifests.txt" "$OUT/SHA256SUMS"
rm -rf "$OUT/bin"
# Cache de compilação descartável: 4 alvos cruzados enchem o GOCACHE do host.
case "$OUT" in
/*) export GOCACHE="$OUT/gocache" ;;
*) export GOCACHE="$ROOT/$OUT/gocache" ;;
esac

BUILDER_USED=0
cleanup() {
	local code=$?
	rm -f "$OUT"/*.tar
	rm -rf "$OUT/bin"
	if [[ -d "$OUT/gocache" ]]; then
		chmod -R u+w "$OUT/gocache" 2>/dev/null || true
		rm -rf "$OUT/gocache"
	fi
	if ((BUILDER_USED)); then
		docker buildx prune -af --builder "$BUILDER" >/dev/null 2>&1 || true
	fi
	echo "== release-dryrun: disco livre no fim ${START_FREE:+(início ${START_FREE} GiB) }$(free_gb) GiB"
	exit "$code"
}
trap cleanup EXIT

START_FREE="$(free_gb)"
echo "== release-dryrun: versão $VERSION, commit ${COMMIT:0:12}, disco livre ${START_FREE} GiB"
if awk -v f="$START_FREE" -v m="$MIN_FREE_GB" 'BEGIN { exit !(f < m) }'; then
	echo "release-dryrun: só ${START_FREE} GiB livres (mínimo ${MIN_FREE_GB}; RELEASE_MIN_FREE_GB)" >&2
	exit 1
fi

# --- 1. CHANGELOG e `wwz version`.
unreleased=$(awk '
	index($0, "## [Não lançado]") == 1 { on = 1; next }
	on && /^## \[/ { exit }
	on { print }
' CHANGELOG.md 2>/dev/null || true)
if grep -q '[^[:space:]]' <<<"$unreleased"; then
	pass "CHANGELOG.md com [Não lançado] não vazio ($(grep -c '^- ' <<<"$unreleased") itens)"
else
	fail "CHANGELOG.md sem a seção [Não lançado] ou com ela vazia"
fi

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}"
mkdir -p "$OUT/bin"
if CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/bin/wwz" ./cmd/wwz; then
	got=$("$OUT/bin/wwz" version 2>&1 || true)
	if [[ "$got" == *"$VERSION"* && "$got" == *"$COMMIT"* ]]; then
		pass "wwz version: $got"
	else
		fail "wwz version sem a versão $VERSION e o commit injetados: $got"
	fi
else
	fail "go build ./cmd/wwz"
fi
rm -rf "$OUT/bin"

# --- 1b. R1: estágio final das imagens estático (distroless/static ou scratch).
for target in "${TARGETS[@]}"; do
	base="$(dockerfile_final_base Dockerfile "$target")"
	if final_base_static "$base"; then
		pass "Dockerfile: estágio $target sobre $base"
	else
		fail "Dockerfile: estágio $target sobre '${base:-?}' (esperado distroless/static ou scratch)"
	fi
done

# --- 2. Imagens multi-arch em OCI, uma por vez.
# blob TAR DIGEST: conteúdo de um blob do layout OCI dentro do tar.
blob() { tar -xOf "$1" "blobs/sha256/${2#sha256:}"; }

if [[ "${RELEASE_SKIP_IMAGES:-0}" == "1" ]]; then
	echo "--    imagens puladas (RELEASE_SKIP_IMAGES=1)"
else
	if ! docker buildx inspect "$BUILDER" >/dev/null 2>&1; then
		docker buildx create --name "$BUILDER" --driver docker-container >/dev/null
		echo "--    builder $BUILDER criado (docker-container)"
	fi
	BUILDER_USED=1
	{
		echo "# release-dryrun $VERSION (commit $COMMIT), $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "# alvo plataforma digest tamanho_bytes(config+camadas)"
	} >"$OUT/manifests.txt"
	for target in "${TARGETS[@]}"; do
		tarball="$OUT/${target}.tar"
		echo "--    buildx $target ($PLATFORMS)"
		if ! docker buildx build --builder "$BUILDER" --platform "$PLATFORMS" --target "$target" \
			--build-arg VERSION="$VERSION" --build-arg COMMIT="$COMMIT" \
			--output "type=oci,dest=${tarball}" --progress plain . >"$OUT/${target}.build.log" 2>&1; then
			tail -n 30 "$OUT/${target}.build.log" >&2
			fail "buildx $target"
			rm -f "$tarball"
			continue
		fi
		rm -f "$OUT/${target}.build.log"
		tar_mb=$(du -m "$tarball" | awk '{print $1}')
		# index.json -> índice multi-arch (um nível a mais quando o buildx aninha).
		index=$(tar -xOf "$tarball" index.json)
		top=$(jq -r '.manifests[0].digest' <<<"$index")
		if [[ "$(jq -r '.manifests[0].mediaType' <<<"$index")" == *index* ]]; then
			index=$(blob "$tarball" "$top")
		fi
		echo "$target index $top -" >>"$OUT/manifests.txt"
		archs=()
		while IFS=$'\t' read -r plat digest; do
			[[ "$plat" == unknown/unknown ]] && continue # atestados do buildx
			size=$(blob "$tarball" "$digest" | jq '[.config.size] + [.layers[].size] | add')
			echo "$target $plat $digest $size" >>"$OUT/manifests.txt"
			archs+=("$plat")
			printf '      %-9s %-12s %s %s MiB\n' "$target" "$plat" "${digest:0:19}" "$(awk -v s="$size" 'BEGIN { printf "%.1f", s / 1048576 }')"
		done < <(jq -r '.manifests[] | [(.platform.os // "unknown") + "/" + (.platform.architecture // "unknown"), .digest] | @tsv' <<<"$index")
		rm -f "$tarball"
		missing=()
		for want in ${PLATFORMS//,/ }; do
			[[ " ${archs[*]} " == *" $want "* ]] || missing+=("$want")
		done
		if ((${#missing[@]})); then
			fail "imagem $target sem ${missing[*]} no index.json"
		else
			pass "imagem $target: ${PLATFORMS//,/ + } (tar ${tar_mb} MiB, apagado)"
		fi
	done
fi

# --- 3. Os 20 binários, um por vez.
if [[ "${RELEASE_SKIP_BINARIES:-0}" == "1" ]]; then
	echo "--    binários pulados (RELEASE_SKIP_BINARIES=1)"
else
	: >"$OUT/SHA256SUMS"
	mkdir -p "$OUT/bin"
	for os in linux darwin; do
		for arch in amd64 arm64; do
			for cmd in "${CMDS[@]}"; do
				bin="$OUT/bin/$cmd"
				if CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" \
					-o "$bin" "./cmd/$cmd"; then
					binary_static_ok "$bin" || fail "$cmd ($os/$arch) sem CGO_ENABLED=0 no go version -m"
					printf '%s  %s_%s/%s\n' "$(sha256 "$bin")" "$os" "$arch" "$cmd" >>"$OUT/SHA256SUMS"
				else
					fail "go build $cmd ($os/$arch)"
				fi
				rm -f "$bin"
			done
			# Pacote do release.yml: LICENSE e THIRD_PARTY_NOTICES vão junto (task 260).
			pkg="white-wizard_dryrun_${os}_${arch}"
			mkdir -p "$OUT/bin/$pkg"
			cp LICENSE THIRD_PARTY_NOTICES "$OUT/bin/$pkg/" 2>/dev/null || fail "LICENSE/THIRD_PARTY_NOTICES ausentes na raiz"
			tar -C "$OUT/bin" -czf "$OUT/bin/$pkg.tar.gz" "$pkg"
			listing=$(tar -tzf "$OUT/bin/$pkg.tar.gz")
			for f in LICENSE THIRD_PARTY_NOTICES; do
				grep -qx "$pkg/$f" <<<"$listing" || fail "pacote $os/$arch sem $f"
			done
			rm -rf "$OUT/bin/$pkg" "$OUT/bin/$pkg.tar.gz"
		done
	done
	rm -rf "$OUT/bin"
	lines=$(wc -l <"$OUT/SHA256SUMS" | tr -d ' ')
	if [[ "$lines" == "20" ]]; then
		pass "20 binários estáticos (CGO_ENABLED=0; 5 × linux/darwin × amd64/arm64) em $OUT/SHA256SUMS, apagados"
	else
		fail "SHA256SUMS com $lines linhas (esperado 20)"
	fi
fi

if ((FAILS != 0)); then
	echo "== release-dryrun: $FAILS falha(s)" >&2
	exit 1
fi
echo "== release-dryrun: tudo verde (sem push, sem tag; ADR 0018)"

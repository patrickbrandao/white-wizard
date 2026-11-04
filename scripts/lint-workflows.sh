#!/usr/bin/env bash
# Lint dos workflows do GitHub Actions (specs/018, passo `workflows` do verify-full).
#
# Uso: scripts/lint-workflows.sh [arquivos...]   (padrão: .github/workflows/*.yml)
#
# Roda o actionlint (com shellcheck embutido) pela imagem rhysd/actionlint numa
# versão fixa. Sem Docker, imprime "pulado" e sai 0. Se a imagem não existia
# antes, remove-a no fim (disco apertado), com sucesso ou falha.
#
# Saída: 0 sem erros ou pulado; 1 com erros do actionlint; 2 com erro de uso.

set -uo pipefail

ACTIONLINT_IMAGE="${ACTIONLINT_IMAGE:-rhysd/actionlint:1.7.7}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2

# Docker Desktop no macOS: o CLI e o docker-credential-desktop (usado no pull)
# ficam nessa pasta, que nem sempre está no PATH.
app_bin=/Applications/Docker.app/Contents/Resources/bin
if [ -d "$app_bin" ]; then
	PATH="$PATH:$app_bin"
fi

files=()
if [ "$#" -gt 0 ]; then
	files=("$@")
else
	for f in .github/workflows/*.yml .github/workflows/*.yaml; do
		[ -f "$f" ] && files+=("$f")
	done
fi
if [ "${#files[@]}" -eq 0 ]; then
	echo "workflows: pulado (nenhum workflow em .github/workflows)"
	exit 0
fi
for f in "${files[@]}"; do
	case "$f" in
	/* | ../*)
		echo "workflows: use caminhos relativos dentro do repositório: $f" >&2
		exit 2
		;;
	esac
	if [ ! -f "$f" ]; then
		echo "workflows: arquivo não encontrado: $f" >&2
		exit 2
	fi
done

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	echo "workflows: pulado (Docker indisponível)"
	exit 0
fi

had_image=no
if docker image inspect "$ACTIONLINT_IMAGE" >/dev/null 2>&1; then
	had_image=yes
fi
cleanup() {
	if [ "$had_image" = no ]; then
		docker image rm "$ACTIONLINT_IMAGE" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

if [ "$had_image" = no ] && ! docker pull -q "$ACTIONLINT_IMAGE" >/dev/null; then
	echo "workflows: pulado (não foi possível baixar $ACTIONLINT_IMAGE)"
	exit 0
fi

docker run --rm --network none --read-only \
	-v "$ROOT:/repo:ro" -w /repo \
	"$ACTIONLINT_IMAGE" -no-color "${files[@]}"
rc=$?
if [ "$rc" -eq 0 ]; then
	echo "workflows: ok (${#files[@]} arquivos, $ACTIONLINT_IMAGE)"
	exit 0
fi
echo "workflows: falha (actionlint saiu com $rc)" >&2
exit 1

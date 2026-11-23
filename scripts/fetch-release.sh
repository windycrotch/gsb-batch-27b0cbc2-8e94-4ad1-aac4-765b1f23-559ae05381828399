#!/usr/bin/env bash
set -euo pipefail
version="${1:?version required, without v}"
destination="${2:?destination required}"
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
# Public packages cannot install from a private GitHub download URL.
test "$(gh api "repos/$GH_REPO" --jq .private)" = false
mkdir -p "$destination"
gh release download "v$version" --dir "$destination"
(cd "$destination" && sha256sum --check --strict checksums.txt)

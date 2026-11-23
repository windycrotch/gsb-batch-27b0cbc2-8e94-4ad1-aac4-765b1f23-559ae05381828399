#!/usr/bin/env bash
set -euo pipefail

# Earlier just install recipes used go install. Preserve that command's public
# location if it is still the peep selected by PATH, including custom GOBINs.
go_bin=$(go env GOBIN)
if [[ -z "$go_bin" ]]; then
    go_path=$(go env GOPATH)
    go_bin="${go_path%%:*}/bin"
fi
active=$(command -v peep || true)
args=(--install)
if [[ -n "$active" && "$active" -ef "$go_bin/peep" ]]; then
    printf 'Upgrading existing Go installation: %s/peep\n' "$go_bin"
    args+=(--bin-dir "$go_bin")
fi

exec ./bin/peep "${args[@]}" "$@"

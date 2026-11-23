#!/usr/bin/env bash
# Package the generated npm directories, or publish a verified release's npm tarballs.
set -euo pipefail
case "${1:-}" in
  pack)
    root="${2:-dist/distribution}"
    mkdir -p "$root/npm-tarballs"
    destination="$(cd "$root/npm-tarballs" && pwd)"
    for platform in darwin-arm64 darwin-x64 linux-arm64 linux-x64 peep; do
      npm pack "$root/npm/$platform" --ignore-scripts --pack-destination "$destination"
    done
    ;;
  publish-npm)
    root="${2:?release assets directory required}"
    version="${3:?version required}"
    node scripts/npm-publish.cjs "$root" "$version"
    ;;
  *) echo 'usage: distribution.sh pack [generated directory] | publish-npm RELEASE_DIR VERSION' >&2; exit 2 ;;
esac

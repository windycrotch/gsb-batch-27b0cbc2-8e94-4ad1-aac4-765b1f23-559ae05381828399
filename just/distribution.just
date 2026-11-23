# Release remote main through CI and publish all three package channels.
release version="patch":
    @gh workflow run release.yml --repo lancekrogers/peep --ref main -f version={{quote(version)}}

# Generate all package-manager inputs from the four versioned archives.
distribution version: (package-all version)
    go run ./cmd/peep-release --version '{{version}}'
    bash scripts/distribution.sh pack

# Verify npm installation and embedded Mermaid without registry downloads.
test-distribution:
    node --test scripts/npm-launcher.test.cjs scripts/npm-publish.test.cjs
    python3 scripts/publish-retry-smoke.py
    python3 scripts/release-workflow-smoke.py
    node scripts/npm-install-smoke.cjs dist/distribution

# Validate .SRCINFO, build and install the AUR package in disposable Arch Linux.
test-aur:
    docker run --rm --platform linux/amd64 -v "$PWD/dist:/release:ro" -v "$PWD/scripts/aur-package-smoke.sh:/test.sh:ro" archlinux:base-devel bash /test.sh

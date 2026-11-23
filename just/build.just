# Build the local binary.
build:
    mkdir -p bin
    go build -trimpath -o bin/peep ./cmd/peep

# Install peep, upgrading an active legacy Go-bin copy when present.
install: build
    bash scripts/install.sh

# Compile the supported OS/architecture combinations.
build-all:
    mkdir -p dist
    for os in darwin linux; do for arch in arm64 amd64; do GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -o dist/peep-$os-$arch ./cmd/peep; done; done

# Build a self-contained archive for the current host.
package version="dev":
    go run ./cmd/peep-package --version '{{version}}'

# Build archives for all supported targets.
package-all version="dev":
    for os in darwin linux; do for arch in arm64 amd64; do go run ./cmd/peep-package --os "$os" --arch "$arch" --version '{{version}}'; done; done

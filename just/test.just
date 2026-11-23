# Unit tests, including race detection.
test:
    go test -race ./...

# Source upgrades from legacy Go-bin installs, with isolated homes and PATHs.
test-install: build
    python3 scripts/install-smoke.py

# Piped rg results, NUL filenames, keyboard input, and editor handoff in a PTY.
test-stdin: build
    python3 scripts/stdin-smoke.py

# Formatting, static analysis, unit tests, and build.
check:
    test -z "$(gofmt -l cmd internal)"
    go vet ./...
    go test -race ./...
    mkdir -p bin
    go build -o bin/peep ./cmd/peep

# Generate small synthetic media fixtures (no personal files).
fixtures:
    bash scripts/fixtures.sh

# Exercise actual ffmpeg and mpv using a null audio device.
test-integration: fixtures
    PEEP_INTEGRATION=1 go test -race -count=1 ./internal/media ./internal/player

# Exercise the real TUI in a PTY with synthetic fixtures.
test-tui: build fixtures
    python3 scripts/tui-smoke.py
    python3 scripts/iterm-preview-smoke.py
    python3 scripts/gif-replay-smoke.py
    python3 scripts/video-playback-smoke.py
    python3 scripts/document-smoke.py
    python3 scripts/document-scroll-smoke.py
    python3 scripts/browser-smoke.py
    python3 scripts/explorer-smoke.py

# Browser filters, sorting, symlinks, and focused text search.
test-explorer: build
    python3 scripts/explorer-smoke.py

# GIF replay and directory switching regressions.
test-gif-replay: build fixtures
    python3 scripts/gif-replay-smoke.py

# Focused document-preview acceptance, including playback while reading.
test-documents: build fixtures
    python3 scripts/document-smoke.py
    python3 scripts/document-scroll-smoke.py

# Compiled-in renderer plus native-protocol TUI checks (no mmdg installation).
test-mermaid: build
    PEEP_TEST_BINARY="$PWD/bin/peep" go test -race -count=1 -run Mermaid ./internal/document
    python3 scripts/mermaid-smoke.py

# Isolated filesystem operations and external editor handoff in a PTY.
test-file-operations: build
    python3 scripts/file-operations-smoke.py

# Extract, relocate, install, and exercise a single-binary package with no tools on PATH.
test-package: package
    python3 scripts/package-smoke.py

# Native video clock/FPS, output backpressure, signals, and interactive editor TTY.
test-video-playback: build
    python3 scripts/video-playback-smoke.py

# iTerm graphics replies must not restart previews or act as keyboard input.
test-iterm-previews: build fixtures
    python3 scripts/iterm-preview-smoke.py

# Run peep (pass CLI arguments after --).
run *args:
    go run ./cmd/peep {{args}}

# Report renderer and media dependency availability.
doctor:
    go run ./cmd/peep --doctor

# Format Go source.
fmt:
    go fmt ./...

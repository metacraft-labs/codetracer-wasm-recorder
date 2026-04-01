default:
    @just --list

alias t := test
alias fmt := format

# Build wazero binary
build:
    go build -o wazero ./cmd/wazero

# Run all Go tests (delegates to Makefile)
test:
    make test

# Run tracewriter package tests only (pure Go, no FFI)
test-tracewriter-go:
    CGO_ENABLED=0 go test ./tracewriter/ -v -run 'TestGoWriter'

# Run all tracewriter tests including Rust FFI (requires cgo + FFI library)
test-tracewriter:
    CGO_ENABLED=1 go test ./tracewriter/ -v

# Lint Go code
lint-go:
    go vet -stdmethods=false ./...

# Lint Nix files
lint-nix:
    if command -v nixfmt >/dev/null; then find . -name '*.nix' -print0 | xargs -0 nixfmt --check; fi

# Lint all code
lint: lint-go lint-nix

# Format Go code
format-go:
    gofmt -w .

# Format Nix files
format-nix:
    if command -v nixfmt >/dev/null; then find . -name '*.nix' -print0 | xargs -0 nixfmt; fi

# Format all code
format: format-go format-nix

# Run all local checks (lint + tracewriter tests with FFI)
check-all: lint test-tracewriter

# Verify the Nix flake builds successfully
nix-build:
    nix build .#default

# Run cross-repo integration tests against sibling codetracer repo
cross-test *ARGS:
    bash scripts/run-cross-repo-tests.sh {{ ARGS }}

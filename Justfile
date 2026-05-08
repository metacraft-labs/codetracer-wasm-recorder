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

# Run tracewriter package tests (CTFS-only — the Rust FFI writer was
# removed in the 2026-05-08 convention compliance pass; see
# AUDIT-CTFS-2026-05.md).  This recipe is preserved as the canonical
# pure-Go writer test entry point and is kept stable so external CI does
# not break.
test-tracewriter-go:
    CGO_ENABLED=0 go test ./tracewriter/ -v -run 'TestGoWriter'

# Convention compliance follow-up — 2026-05-08: the Rust FFI writer was
# removed alongside the `--format` flag.  This alias is retained so older
# wrappers / docs that invoke `just test-tracewriter` keep working — it
# now runs the same single CTFS-only writer test set as
# `test-tracewriter-go`.
test-tracewriter: test-tracewriter-go

# Verify the CLI matches the conventions in
# `codetracer-specs/Recorder-CLI-Conventions.md` (no `--format`, env-var
# fallback, `ct print` mention, etc.).  See
# `tests/verify-cli-convention-no-silent-skip.sh` for the assertion list.
verify-cli-convention:
    bash tests/verify-cli-convention-no-silent-skip.sh

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

# Run all local checks (lint + tracewriter tests + CLI convention).
check-all: lint test-tracewriter verify-cli-convention

# Verify the Nix flake builds successfully
nix-build:
    nix build .#default

# Run cross-repo integration tests against sibling codetracer repo
cross-test *ARGS:
    bash scripts/run-cross-repo-tests.sh {{ ARGS }}

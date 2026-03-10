# Run all Go tests (delegates to Makefile)
test:
  make test

# Run tracewriter package tests only (pure Go, no FFI)
test-tracewriter-go:
  CGO_ENABLED=0 go test ./tracewriter/ -v -run 'TestGoWriter'

# Run all tracewriter tests including Rust FFI (requires cgo + FFI library)
test-tracewriter:
  CGO_ENABLED=1 go test ./tracewriter/ -v

# Run pre-commit checks (delegates to Makefile)
check:
  make check

# Build wazero binary
build:
  go build -o wazero ./cmd/wazero

# Run all local checks
check-all: check test-tracewriter

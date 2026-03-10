# Run all Go tests (delegates to Makefile)
test:
  make test

# Run tracewriter package tests only (no cgo required)
test-tracewriter:
  go test ./tracewriter/ -v -run 'TestGoWriter'

# Run tracewriter tests including Rust FFI (requires cgo + FFI library)
test-tracewriter-ffi:
  CGO_ENABLED=1 go test ./tracewriter/ -v

# Run pre-commit checks (delegates to Makefile)
check:
  make check

# Build wazero binary
build:
  go build -o wazero ./cmd/wazero

# Run all local checks
check-all: check test-tracewriter

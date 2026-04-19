#!/usr/bin/env bash
# =============================================================================
# Run cross-repo integration tests against the sibling codetracer repo.
#
# This script builds the wazero binary and then runs the WASM flow integration
# tests in the codetracer repo's db-backend crate.
#
# Usage:
#   bash scripts/run-cross-repo-tests.sh
#
# Prerequisites:
#   - Sibling codetracer repo checked out at ../codetracer (or ../../codetracer)
#   - Rust toolchain with wasm32-wasip1 target installed
#   - codetracer dev shell available (for test dependencies)
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# ---------------------------------------------------------------------------
# Locate the sibling codetracer repo.
# ---------------------------------------------------------------------------
CODETRACER_DIR=""
for candidate in "$REPO_ROOT/../codetracer" "$REPO_ROOT/../../codetracer"; do
  if [ -d "$candidate/src/db-backend" ]; then
    CODETRACER_DIR="$(cd "$candidate" && pwd)"
    break
  fi
done

if [ -z "$CODETRACER_DIR" ]; then
  echo "ERROR: sibling codetracer repo not found." >&2
  echo "  Expected at ../codetracer or ../../codetracer" >&2
  exit 1
fi

echo "Using codetracer at: $CODETRACER_DIR"

# ---------------------------------------------------------------------------
# Build the wazero binary.
# ---------------------------------------------------------------------------
echo "Building wazero..."
cd "$REPO_ROOT"
go build -o wazero ./cmd/wazero
WAZERO_PATH="$REPO_ROOT/wazero"
echo "Built wazero: $WAZERO_PATH"

# ---------------------------------------------------------------------------
# Run WASM flow integration tests in the codetracer repo.
# ---------------------------------------------------------------------------
echo "Running WASM flow integration tests..."
cd "$CODETRACER_DIR"

# Ensure the wasm32-wasip1 target is available.
rustup target add wasm32-wasip1 2>/dev/null || true

export CODETRACER_WASM_VM_PATH="$WAZERO_PATH"
echo "CODETRACER_WASM_VM_PATH=$CODETRACER_WASM_VM_PATH"

cd src/db-backend
OUTPUT=$(cargo test --test wasm_flow_integration -- --nocapture 2>&1) || {
  echo "$OUTPUT"
  exit 1
}
echo "$OUTPUT"

if echo "$OUTPUT" | grep -q 'running 0 tests'; then
  echo "ERROR: wasm_flow_integration test binary compiled but no tests ran" >&2
  exit 1
fi

echo "WASM flow integration tests passed!"

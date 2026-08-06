// rust_fixture_test.go — compiles the `rust_test` wasm fixture from
// its committed Rust source, during the test run that consumes it.
//
// This is M55's mechanism applied to the last compiled `.wasm` in the
// repo that was still checked in. `cmd/wazero/testdata/rust_test.wasm`
// was 1.8 MB of `go:embed`ed build output whose source — 40 lines of
// Rust at `test_code/rust_test.rs` — sat in the tree beside it with
// nothing verifying the two still corresponded. They had in fact
// drifted out of anyone's reach: the committed binary was produced by
// a rustup `rustc 1.86.0` that exists on no machine here, and carried
// a contributor's home directory (`/home/pesho/...`) baked into its
// DWARF, exactly as the recorder-golden fixtures did.
//
// # Where the source lives
//
// Deliberately NOT copied next to this package. M55 had to delete two
// byte-identical copies of every golden fixture, because duplicated
// blobs mean deleting one copy alone frees nothing. So this builder
// reaches up to the single canonical source at `test_code/rust_test.rs`
// (`go test` runs with the package directory as cwd) rather than
// creating a second copy of it here.
//
// # Compiler flags
//
// `-g -C debuginfo=2 -C opt-level=0 --edition 2021` — identical to the
// recorder-golden fixtures, and load-bearing for the same reason. The
// consuming test asserts on Step / Call / Variable events that the
// interpreter derives from DWARF; an optimised or stripped build would
// erase the per-statement line table and produce an empty trace.
//
// # Failure policy
//
// A missing rustc, or a rustc without the wasm target, FAILS — it never
// skips. The toolchain resolution is shared with the recorder-golden
// builder (`goldenRustc`), so there is exactly one definition of that
// policy and one place where its message can go stale.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	// rustFixtureSrc is the canonical Rust source, relative to this
	// package's directory. It is the same file the consuming test's
	// documentation quotes line numbers from.
	rustFixtureSrc = "../../test_code/rust_test.rs"

	// rustFixtureOutDir receives the compiled `.wasm`. Gitignored: it
	// is build output, which is the entire point of this file.
	rustFixtureOutDir = "testdata/build"

	// rustFixtureTarget is the wasm target the recorder consumes.
	rustFixtureTarget = "wasm32-wasip1"
)

// rustFixtureFlags are passed to rustc ahead of `--target`, `-o` and
// the source path.
var rustFixtureFlags = []string{
	"-g",
	"-C", "debuginfo=2",
	"-C", "opt-level=0",
	"--edition", "2021",
}

var (
	// rustFixtureOnce makes the compilation happen exactly once per
	// test binary, matching the recorder-golden builder's sharing.
	rustFixtureOnce sync.Once

	// rustFixtureErr is the (single, shared) build outcome.
	rustFixtureErr error
)

// rustTestFixture returns the compiled wasm bytes for the `rust_test`
// fixture, compiling it on first use.
//
// It fails the test — loudly, naming the missing component — when the
// toolchain cannot produce it.
func rustTestFixture(t *testing.T) []byte {
	t.Helper()

	rustFixtureOnce.Do(func() {
		rustFixtureErr = buildRustFixture()
	})
	if rustFixtureErr != nil {
		t.Fatalf("the rust_test wasm fixture could not be built: %v", rustFixtureErr)
	}

	path := filepath.Join(rustFixtureOutDir, "rust_test.wasm")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("rust_test.wasm was reported built but could not be read "+
			"at %s: %v", path, err)
	}
	if len(blob) < 4 || blob[0] != 0x00 || blob[1] != 'a' || blob[2] != 's' || blob[3] != 'm' {
		t.Fatalf("rust_test.wasm lacks the WASM magic header; rustc produced "+
			"%d bytes of something else at %s", len(blob), path)
	}
	return blob
}

// buildRustFixture compiles the fixture, reusing an existing build
// whose stamp matches the current source and compiler.
func buildRustFixture() error {
	// goldenRustc is shared with recorder_golden_fixtures_test.go: it
	// resolves rustc, verifies the wasm target's std is actually
	// installed, and returns the hard-failure message naming whichever
	// of the two is missing.
	rustc, err := goldenRustc()
	if err != nil {
		return err
	}

	stamp, err := rustFixtureStamp(rustc)
	if err != nil {
		return err
	}

	out := filepath.Join(rustFixtureOutDir, "rust_test.wasm")
	stampPath := filepath.Join(rustFixtureOutDir, "rust_test.stamp")
	if fresh, _ := os.ReadFile(stampPath); string(fresh) == stamp {
		// A matching stamp beside a deleted or truncated `.wasm` must
		// still rebuild, so the output is checked too.
		if st, err := os.Stat(out); err == nil && st.Size() > 0 {
			return nil
		}
	}

	if err := os.MkdirAll(rustFixtureOutDir, 0o755); err != nil {
		return fmt.Errorf("creating fixture build dir %s: %w", rustFixtureOutDir, err)
	}

	// A stale stamp must not survive a failed rebuild, or the next run
	// would accept whatever half-built output is lying around.
	_ = os.Remove(stampPath)

	// Compile to a process-private path and rename into place, so the
	// two build variants' suites (`just test` and `just test-snapshots`
	// share this package) cannot observe a half-written fixture.
	// Identical inputs make the outputs interchangeable, so the race is
	// benign.
	tmp := fmt.Sprintf("%s.tmp%d", out, os.Getpid())

	args := append([]string{}, rustFixtureFlags...)
	args = append(args, "--target", rustFixtureTarget, "-o", tmp, rustFixtureSrc)

	cmd := exec.Command(rustc, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compiling %s to %s failed: %w\n    %s %s\nrustc output:\n%s",
			rustFixtureSrc, rustFixtureTarget, err,
			rustc, strings.Join(args, " "), combined)
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing built fixture %s: %w", out, err)
	}

	if err := os.WriteFile(stampPath, []byte(stamp), 0o644); err != nil {
		return fmt.Errorf("writing fixture build stamp %s: %w", stampPath, err)
	}
	return nil
}

// rustFixtureStamp fingerprints everything that determines the built
// output: the compiler identity, the flags, and the source bytes.
func rustFixtureStamp(rustc string) (string, error) {
	body, err := os.ReadFile(rustFixtureSrc)
	if err != nil {
		return "", fmt.Errorf("reading fixture source %s: %w", rustFixtureSrc, err)
	}

	h := sha256.New()
	// goldenFixturesRustc is set by goldenRustc, called above.
	fmt.Fprintf(h, "rustc\x00%s\x00%s\x00", goldenFixturesRustc, rustc)
	fmt.Fprintf(h, "target\x00%s\x00", rustFixtureTarget)
	fmt.Fprintf(h, "flags\x00%s\x00", strings.Join(rustFixtureFlags, " "))
	fmt.Fprintf(h, "src\x00%d\x00", len(body))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)) + "\n", nil
}

// TestRustFixtureBuilds is the guard that replaces the old "is the
// go:embed directive still valid" check. The embed was a compile-time
// error when the file went missing; a path read is not, so the
// buildability of the fixture — and the presence of its source — is
// asserted explicitly.
func TestRustFixtureBuilds(t *testing.T) {
	if _, err := os.Stat(rustFixtureSrc); err != nil {
		t.Fatalf("the rust_test fixture source has gone missing at %s: %v\n"+
			"This file is the only definition of the program that "+
			"TestRecordedTraceViaCtPrintJson asserts decoded values from; "+
			"without it that test asserts on nothing.", rustFixtureSrc, err)
	}

	blob := rustTestFixture(t)
	if len(blob) == 0 {
		t.Fatal("the built rust_test fixture is empty")
	}

	// The consuming test needs DWARF: the interpreter derives its
	// Step / Call / Variable events from the line table, and a build
	// that lost `-g` would produce an empty trace and silently retire
	// every value assertion in TestRecordedTraceViaCtPrintJson.
	if !strings.Contains(string(blob), ".debug_info") {
		t.Fatalf("the built rust_test fixture carries no .debug_info section "+
			"(%d bytes) — the compile flags %v must keep full DWARF, or the "+
			"recorder emits no source-level events at all",
			len(blob), rustFixtureFlags)
	}
}

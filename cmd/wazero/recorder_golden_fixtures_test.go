// recorder_golden_fixtures_test.go — compiles the recorder-golden
// wasm fixtures from their committed Rust sources, during the test
// run that consumes them.
//
// These fixtures used to be checked in as ~3.7 MB of pre-built
// `.wasm` sitting next to ~5 KB of `.rs`.  Nothing verified that the
// binaries still corresponded to the sources: the repo's own dev
// shell shipped no `wasm32-wasip1` standard library, so nobody could
// rebuild them to find out, and the committed objects carried the
// building contributor's home directory baked into their DWARF.  A
// fixture that cannot be reproduced is not evidence.
//
// The fixtures are now produced by running the compiler the flake
// pins — `shell.nix` combines `targets.wasm32-wasip1.stable.rust-std`
// with the `stable.rustc` it already provided, both resolved through
// `flake.lock` — so the `.wasm` the golden assertions read is always
// the compilation of the `.rs` in the tree, by a toolchain that is a
// property of the repo rather than of whoever ran the build.
//
// # Compiler flags
//
// `-g -C debuginfo=2 -C opt-level=0 --edition 2021`.  The debug build
// with full DWARF is load-bearing, not incidental: the recorder's
// source-level stepping in
// `internal/engine/interpreter/interpreter.go` maps wasm PC offsets
// to source lines through DWARF, and an optimised build would erase
// the per-statement line table that the golden assertions read.
//
// # Failure policy
//
// A missing rustc, or a rustc without the wasm target, FAILS.  It
// never skips.  A skip here would silently retire the entire golden
// suite — the one that was red for six weeks — the moment the dev
// shell drifted, which is exactly the failure mode the pre-built
// binaries were hiding in the first place.
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
	// goldenFixtureSrcDir holds the `.rs` sources, relative to this
	// package's directory (`go test` runs with that as the cwd).
	goldenFixtureSrcDir = "testdata/recorder-golden"

	// goldenFixtureOutDir receives the compiled `.wasm`.  It is
	// gitignored: it is build output, and the point of this file is
	// that build output does not get committed.
	goldenFixtureOutDir = goldenFixtureSrcDir + "/build"

	// goldenFixtureTarget is the wasm target the recorder consumes.
	goldenFixtureTarget = "wasm32-wasip1"
)

// goldenFixtureNames lists every fixture, and doubles as the
// completeness check in TestRecorderGoldenFixturesBuild.
var goldenFixtureNames = []string{
	"collections",
	"column_aware",
	"control_flow",
	"nested_calls",
	"panic_path",
}

// goldenFixtureFlags are passed to rustc for every fixture, ahead of
// `--target`, `-o` and the source path.
var goldenFixtureFlags = []string{
	"-g",
	"-C", "debuginfo=2",
	"-C", "opt-level=0",
	"--edition", "2021",
}

var (
	// goldenFixturesOnce makes the compilation happen exactly once
	// per test binary.  Six tests read these five fixtures; without
	// this they would be rebuilt per test, turning a ~9 s suite into
	// a multi-minute one.
	goldenFixturesOnce sync.Once

	// goldenFixturesRustc is the first line of `rustc -vV` for the
	// compiler that produced the fixtures, reported on failure so a
	// toolchain-driven change in the assertions is legible.
	goldenFixturesRustc string

	// goldenFixturesErr is the (single, shared) build outcome.
	goldenFixturesErr error
)

// goldenFixture returns the compiled wasm bytes for the named
// fixture, compiling every fixture on first use.
//
// It fails the test — loudly, naming the missing component — when the
// toolchain cannot produce them.
func goldenFixture(t *testing.T, name string) []byte {
	t.Helper()

	goldenFixturesOnce.Do(func() {
		goldenFixturesErr = buildGoldenFixtures()
	})
	if goldenFixturesErr != nil {
		t.Fatalf("recorder-golden fixtures could not be built: %v", goldenFixturesErr)
	}

	path := filepath.Join(goldenFixtureOutDir, name+".wasm")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("recorder-golden fixture %s.wasm was reported built but "+
			"could not be read at %s: %v", name, path, err)
	}
	if len(blob) < 4 || blob[0] != 0x00 || blob[1] != 'a' || blob[2] != 's' || blob[3] != 'm' {
		t.Fatalf("recorder-golden fixture %s lacks the WASM magic header; "+
			"rustc produced %d bytes of something else at %s",
			name, len(blob), path)
	}
	return blob
}

// noteGoldenToolchain arranges for a failing golden test to say which
// compiler built the program it was asserting on.
//
// The golden assertions pin exact source lines, columns and step
// counts, and those come out of the DWARF that rustc emitted.  That
// couples them to the compiler version — deliberately: a step-table
// change IS recorder-visible output changing, and the test should say
// so.  What it must not do is say so as an inscrutable numeric diff,
// so the reader gets told where to look.
func noteGoldenToolchain(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		// Nothing to add when the test passed — nor when the fixtures
		// never built, where goldenFixture's own error is the whole
		// story and a note about DWARF line tables would bury it.
		if !t.Failed() || goldenFixturesErr != nil || goldenFixturesRustc == "" {
			return
		}
		t.Logf("\n"+
			"NOTE: the wasm under test was compiled during this run by:\n"+
			"    %s\n"+
			"from %s/*.rs.\n"+
			"The assertions above pin exact source lines, columns and step\n"+
			"counts, which come from that compiler's DWARF line table.  If\n"+
			"this failed without a change to the recorder, check whether the\n"+
			"Rust toolchain moved (`git diff flake.lock shell.nix`) — a rustc\n"+
			"bump legitimately shifts the line table, and this test reporting\n"+
			"it is the test working.  The fix is then to re-review the\n"+
			"expectations against the new output and update them\n"+
			"deliberately.  Do NOT relax them to make the colour change.",
			goldenFixturesRustc, goldenFixtureSrcDir)
	})
}

// buildGoldenFixtures compiles every `.rs` fixture to wasm, reusing an
// existing build whose stamp matches the current sources and compiler.
func buildGoldenFixtures() error {
	rustc, err := goldenRustc()
	if err != nil {
		return err
	}

	stamp, err := goldenBuildStamp(rustc)
	if err != nil {
		return err
	}

	stampPath := filepath.Join(goldenFixtureOutDir, "stamp.txt")
	if fresh, _ := os.ReadFile(stampPath); string(fresh) == stamp && goldenOutputsPresent() {
		// Sources and toolchain both unchanged since the last build.
		return nil
	}

	if err := os.MkdirAll(goldenFixtureOutDir, 0o755); err != nil {
		return fmt.Errorf("creating fixture build dir %s: %w", goldenFixtureOutDir, err)
	}

	// A stale stamp must not survive a failed rebuild, or the next run
	// would accept whatever half-built outputs are lying around.
	_ = os.Remove(stampPath)

	for _, name := range goldenFixtureNames {
		src := filepath.Join(goldenFixtureSrcDir, name+".rs")
		out := filepath.Join(goldenFixtureOutDir, name+".wasm")

		// Compile to a process-private path and rename into place, so
		// two concurrently running test binaries (`just test` and
		// `just test-snapshots` share this package) cannot observe a
		// partially written fixture.  Identical inputs make the
		// outputs interchangeable, so the race is benign.
		tmp := fmt.Sprintf("%s.tmp%d", out, os.Getpid())

		args := append([]string{}, goldenFixtureFlags...)
		args = append(args, "--target", goldenFixtureTarget, "-o", tmp, src)

		cmd := exec.Command(rustc, args...)
		if combined, err := cmd.CombinedOutput(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("compiling %s to %s failed: %w\n"+
				"    %s %s\nrustc output:\n%s",
				src, goldenFixtureTarget, err,
				rustc, strings.Join(args, " "), combined)
		}
		if err := os.Rename(tmp, out); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("installing built fixture %s: %w", out, err)
		}
	}

	if err := os.WriteFile(stampPath, []byte(stamp), 0o644); err != nil {
		return fmt.Errorf("writing fixture build stamp %s: %w", stampPath, err)
	}
	return nil
}

// goldenRustc resolves the Rust compiler and verifies it can actually
// target wasm, failing with a message that names the missing piece.
func goldenRustc() (string, error) {
	name := os.Getenv("RUSTC")
	if name == "" {
		name = "rustc"
	}

	rustc, err := exec.LookPath(name)
	if err != nil {
		// Deliberately not naming one fixture set: this resolver is
		// shared by the recorder-golden builder and by the rust_test
		// builder in rust_fixture_test.go, and a message naming only
		// the former misdirects whoever hits it from the latter.
		return "", fmt.Errorf("rustc (%q) is not on PATH: %w\n"+
			"The wasm test fixtures (%s/*.rs and %s) are compiled from their "+
			"committed Rust sources during the test run — they are no longer "+
			"checked in as binaries.  Run the suite inside this repo's Nix dev "+
			"shell, which pins both rustc and the %s standard library:\n"+
			"    direnv exec . just test        (or: nix develop -c just test)",
			name, err, goldenFixtureSrcDir, rustFixtureSrc, goldenFixtureTarget)
	}

	verOut, err := exec.Command(rustc, "-vV").Output()
	if err != nil {
		return "", fmt.Errorf("%s -vV failed: %w", rustc, err)
	}
	goldenFixturesRustc = strings.TrimSpace(strings.SplitN(string(verOut), "\n", 2)[0])

	// `--print target-libdir` answers "where would the std for this
	// target live", whether or not it is actually installed — so the
	// stat is the real check.
	libdirOut, err := exec.Command(rustc, "--print", "target-libdir",
		"--target", goldenFixtureTarget).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s does not recognise the %s target: %w\n"+
			"output:\n%s", goldenFixturesRustc, goldenFixtureTarget, err, libdirOut)
	}
	libdir := strings.TrimSpace(string(libdirOut))
	if st, err := os.Stat(libdir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s has no %s standard library installed "+
			"(looked for %s).\n"+
			"This repo's dev shell ships it: shell.nix combines "+
			"`targets.%s.stable.rust-std` with `stable.rustc`, both pinned by "+
			"flake.lock.  Run the suite through the dev shell:\n"+
			"    direnv exec . just test        (or: nix develop -c just test)\n"+
			"Outside Nix: rustup target add %s",
			goldenFixturesRustc, goldenFixtureTarget, libdir,
			goldenFixtureTarget, goldenFixtureTarget)
	}

	return rustc, nil
}

// goldenBuildStamp fingerprints everything that determines the built
// output: the compiler identity, the flags, and the source bytes.  A
// change to any of them invalidates the cached build.
func goldenBuildStamp(rustc string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "rustc\x00%s\x00%s\x00", goldenFixturesRustc, rustc)
	fmt.Fprintf(h, "target\x00%s\x00", goldenFixtureTarget)
	fmt.Fprintf(h, "flags\x00%s\x00", strings.Join(goldenFixtureFlags, " "))

	for _, name := range goldenFixtureNames {
		src := filepath.Join(goldenFixtureSrcDir, name+".rs")
		body, err := os.ReadFile(src)
		if err != nil {
			return "", fmt.Errorf("reading fixture source %s: %w", src, err)
		}
		fmt.Fprintf(h, "src\x00%s\x00%d\x00", name, len(body))
		h.Write(body)
	}
	return hex.EncodeToString(h.Sum(nil)) + "\n", nil
}

// goldenOutputsPresent reports whether every fixture is on disk, so a
// matching stamp beside a deleted `.wasm` still triggers a rebuild.
func goldenOutputsPresent() bool {
	for _, name := range goldenFixtureNames {
		st, err := os.Stat(filepath.Join(goldenFixtureOutDir, name+".wasm"))
		if err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

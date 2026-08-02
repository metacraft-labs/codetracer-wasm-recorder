// recorder_defects_test.go — regression tests for the two user-facing
// recorder defects M40 fixed
// (`codetracer-specs/Planned-Features/Value-Origin-Tracking.milestones.org`
// § M40).  Both were reproduced before being fixed and both are pinned
// here so a reintroduction is loud.
//
//  1. A step whose line lay outside the file its `path_id` names — a
//     time-travel destination the GUI cannot display.
//  2. `--stylus` with no output directory dereferencing a nil recorder,
//     surviving on wazero's own recover, and exiting 0 anyway.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// ===========================================================================
// verify_no_step_lands_outside_its_source_file
// ===========================================================================

// TestVerifyNoStepLandsOutsideItsSourceFile asserts the M40 property
// directly: every `step` event in a materialised trace carries a line
// within the bounds of the file its `path_id` names.
//
// The defect it guards against was not a DWARF misreading — the
// interpreter's own step sequence was correct throughout.  It was an
// *encoding* fault.  The column-aware wire format addresses a position
// as a byte offset into the source file, recovered at read time by
// prefix-summing the per-line byte counts in `paths.dat` Layout A.  The
// recorder was handing it DWARF columns that no such offset can name:
//
//   - columns on a path whose source it could not read, so no per-line
//     table was registered and there was nothing to invert against; and
//   - columns one past the end of their line, which DWARF emits
//     routinely (a function epilogue's `}` on a one-byte line is
//     reported at column 2).
//
// Both surfaced as *line* corruption, because that is what a byte
// offset the reader cannot place turns into.  The demo module's last
// step read line 3368 of a 75-line `lib.rs` — exactly the file's 3443
// bytes less its 75 newlines, i.e. the offset one past the whole table.
// Its two closing braces read 61 and 66 instead of 59 and 64: one past
// the end of a `}` line is the first offset of the next line, and an
// empty line after it is indistinguishable again.
//
// See `addressableColumn` in
// `internal/engine/interpreter/interpreter.go` for the fix.
//
// The check is deliberately phrased over the *trace*, not over the
// recorder's internals, because "a step the user cannot navigate to" is
// a property of the artefact and any future encoding change has to keep
// it true.  Every recorder-golden fixture is covered — each compiled
// from its checked-in `.rs` by the run itself, so the source the line
// bounds are taken from is by construction the source the DWARF
// describes — plus the boundary-log demo, whose materialised trace is
// where the defect was first observed.
func TestVerifyNoStepLandsOutsideItsSourceFile(t *testing.T) {
	requireCtPrint(t)

	type fixture struct {
		name   string
		source string // path to the .rs the trace's path_id must name
	}
	fixtures := []fixture{
		{"control_flow", "testdata/recorder-golden/control_flow.rs"},
		{"nested_calls", "testdata/recorder-golden/nested_calls.rs"},
		{"collections", "testdata/recorder-golden/collections.rs"},
		{"column_aware", "testdata/recorder-golden/column_aware.rs"},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			doc, _ := recordAndDumpFull(t, f.name)
			requireEveryStepInsideItsFile(t, doc, map[string]int{
				filepath.Base(f.source): sourceLineCount(t, f.source),
			})
		})
	}

	// `panic_path` exits non-zero by design, so it cannot go through
	// `recordAndDumpFull`'s clean-exit helper — but a trace preserved
	// across a trap is exactly where a bad step attribution would be
	// least noticed, so it is covered on its own terms.
	t.Run("panic_path", func(t *testing.T) {
		tmpDir := t.TempDir()
		wasmPath := filepath.Join(tmpDir, "panic_path.wasm")
		noteGoldenToolchain(t)
		require.NoError(t, os.WriteFile(wasmPath, goldenFixture(t, "panic_path"), 0o700))
		outDir := filepath.Join(tmpDir, "traces")
		runMain(t, "", []string{"run", "--out-dir=" + outDir, wasmPath})
		doc := dumpFull(t, outDir)
		requireEveryStepInsideItsFile(t, doc, map[string]int{
			"panic_path.rs": sourceLineCount(t, "testdata/recorder-golden/panic_path.rs"),
		})
	})

	// The boundary-log demo: the trace this defect was reported on.
	t.Run("boundary_log_demo", func(t *testing.T) {
		outDir := filepath.Join(t.TempDir(), "traces")
		exitCode, _, stderr := runMain(t, "", []string{
			"run", "--boundary-log=" + demoRecording, "--out-dir=" + outDir, demoWasm})
		require.Equal(t, 0, exitCode, "replay should succeed; stderr:\n%s", stderr)
		doc := dumpFull(t, outDir)
		// The demo's DWARF names the wasm-src `lib.rs` in the codetracer
		// sibling; `balance_calc.rs` in this repo's testdata is the same
		// file, checked in beside the module.
		requireEveryStepInsideItsFile(t, doc, map[string]int{
			"lib.rs": sourceLineCount(t, "testdata/boundary-log/balance_calc.rs"),
		})
	})
}

// requireEveryStepInsideItsFile fails for any step event whose line is
// outside [1, lineCount] of the file its path names.  `lineCounts` is
// keyed by the path's base name, since the DWARF-embedded directory is
// the build machine's and is not reproducible here.
func requireEveryStepInsideItsFile(t *testing.T, doc *goldenDoc, lineCounts map[string]int) {
	t.Helper()
	require.True(t, len(doc.Paths) > 0, "trace declares no source paths")

	events := decodeEvents(t, doc)
	var stepCount int
	for _, ev := range events {
		if ev.Kind != "step" {
			continue
		}
		stepCount++
		base := filepath.Base(ev.Path)
		limit, known := lineCounts[base]
		require.True(t, known,
			"step %d names path %q, which this test has no line count for; "+
				"either the fixture gained a source file or the recorder "+
				"attributed a step to an unexpected one", ev.StepIndex, ev.Path)
		require.True(t, ev.Line >= 1 && ev.Line <= int64(limit),
			"step %d lands on line %d of %q, which has %d lines.  A step "+
				"outside its own file is a time-travel destination the GUI "+
				"cannot display; see `addressableColumn` in "+
				"internal/engine/interpreter/interpreter.go for the encoding "+
				"fault that produced these.",
			ev.StepIndex, ev.Line, base, limit)
	}
	require.True(t, stepCount > 0,
		"the trace carries no step events, so this assertion proved nothing")
}

// sourceLineCount counts the lines of a checked-in Rust fixture the way
// a source viewer would: a trailing newline does not open a further
// line.
func sourceLineCount(t *testing.T, relPath string) int {
	t.Helper()
	data, err := os.ReadFile(relPath)
	require.NoError(t, err, "fixture source %s must be checked in", relPath)
	trimmed := bytes.TrimSuffix(data, []byte("\n"))
	return bytes.Count(trimmed, []byte("\n")) + 1
}

// ===========================================================================
// verify_stylus_without_out_dir_does_not_panic
// ===========================================================================

// TestVerifyStylusWithoutOutDirDoesNotPanic runs the Stylus entrypoint
// path with recording switched off entirely.
//
// `doRun` only builds a `tracewriter.TraceRecorder` when an output
// directory is configured, so with neither `--out-dir` nor
// `CODETRACER_WASM_RECORDER_OUT_DIR` it handed `stylus.Instantiate` a
// nil interface — and all 34 host hooks in `internal/stylus` then called
// `record.RegisterRecordEvent(...)` on it.  The nil dereference happened
// inside a wasm host call, where wazero's own recover turns a Go panic
// into a trap, so the process printed a Go stack trace, unwound out of
// the hook with the EVM event cursor half-advanced, reported a bogus
// "mismatched return result in trace and execution" caused by that
// desync, and exited 0.
//
// Recording is an option.  Running a Stylus module without it must
// simply not record.  See `eventSink` in
// `internal/stylus/stylus_funcs.go`.
func TestVerifyStylusWithoutOutDirDoesNotPanic(t *testing.T) {
	// Make sure an ambient env-var fallback cannot quietly supply the
	// output directory this test is about the absence of.
	t.Setenv("CODETRACER_WASM_RECORDER_OUT_DIR", "")

	exitCode, stdout, stderr := runMain(t, "", []string{
		"run",
		"--stylus=testdata/stylus/entrypoint_trace.json",
		"testdata/stylus/entrypoint.wasm",
	})

	combined := stdout + stderr
	require.False(t, strings.Contains(combined, "invalid memory address or nil pointer dereference"),
		"a nil recorder must not be dereferenced; output:\n%s", combined)
	require.False(t, strings.Contains(combined, "goroutine 1 [running]"),
		"no Go stack trace may reach the user; output:\n%s", combined)
	require.False(t, strings.Contains(combined, "error mismatched return result in trace and execution"),
		"the entrypoint's result matches the trace; the mismatch report was "+
			"an artefact of the panic unwinding a half-serviced hook.  "+
			"output:\n%s", combined)
	require.Equal(t, 0, exitCode,
		"a Stylus run that agrees with its trace exits 0; output:\n%s", combined)
}

// ===========================================================================
// verify_stylus_entrypoint_failure_exits_non_zero
// ===========================================================================

// TestVerifyStylusEntrypointFailureExitsNonZero pins the other half of
// the same defect: every failure branch of the Stylus entrypoint block
// used to print to stderr and fall through to `return 0`.  A diagnostic
// no exit status carries is invisible to CI, to a shell `&&`, and to
// `ct arb record`, which shells out to this binary.
//
// The failure is induced from the EVM trace rather than from the module,
// because the trace is the input a Stylus replay is driven by: a trace
// whose recorded `user_returned` value disagrees with what the module
// actually returns is exactly the "the replay did not reproduce the
// recorded execution" case, and it is the one a user hits when the
// debug build and the on-chain build have drifted apart.
func TestVerifyStylusEntrypointFailureExitsNonZero(t *testing.T) {
	t.Setenv("CODETRACER_WASM_RECORDER_OUT_DIR", "")

	// The committed fixture, with only the recorded return value
	// changed, so the module's genuine result cannot match it.
	raw, err := os.ReadFile("testdata/stylus/entrypoint_trace.json")
	require.NoError(t, err)
	var events []map[string]any
	require.NoError(t, json.Unmarshal(raw, &events))
	var patched bool
	for _, ev := range events {
		if ev["name"] == "user_returned" {
			require.Equal(t, "0xefbeadde", ev["outs"],
				"fixture drifted; update the value this test perturbs")
			ev["outs"] = "0x00c0ffee"
			patched = true
		}
	}
	require.True(t, patched, "fixture has no user_returned event to perturb")

	dir := t.TempDir()
	tracePath := filepath.Join(dir, "mismatched_trace.json")
	out, err := json.Marshal(events)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tracePath, out, 0o600))

	exitCode, stdout, stderr := runMain(t, "", []string{
		"run", "--stylus=" + tracePath, "testdata/stylus/entrypoint.wasm",
	})
	combined := stdout + stderr

	require.True(t, strings.Contains(combined,
		"error mismatched return result in trace and execution"),
		"the mismatch must still be reported; output:\n%s", combined)
	require.True(t, exitCode != 0,
		"a Stylus run whose entrypoint disagrees with its trace must exit "+
			"non-zero — a failure reported only on stderr is invisible to "+
			"every caller.  Got exit %d; output:\n%s", exitCode, combined)
}

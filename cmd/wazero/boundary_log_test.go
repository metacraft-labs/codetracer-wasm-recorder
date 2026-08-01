// boundary_log_test.go — the M37 verification suite for `--boundary-log`.
//
// These are the five checks the milestone names, driven through the real
// CLI entry point (`doMain`) and asserted on the real `.ct` container
// decoded by `ct-print --full`, the same way recorder_golden_test.go does.
//
// NO MOCKS. Every test compiles a real module, replays a real recording and
// inspects a real trace bundle. The one recording that stands in for "a
// browser session" is not a stand-in at all: it is the committed output of
// an actual browser recording, copied unmodified from
// `codetracer/src/db-backend/tests/fixtures/cross_process/`.
// `testdata/boundary-log/README.md` records its provenance.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/tracewriter"
)

const (
	boundaryTestdata = "testdata/boundary-log"
	demoWasm         = boundaryTestdata + "/balance_calc.wasm"
	demoRecording    = boundaryTestdata + "/frontend-wasm.ct"
	demoManifest     = boundaryTestdata + "/balance_calc.wasm.manifest.json"
	hookImportsWasm  = boundaryTestdata + "/hook_imports.wasm"
	// The M39 fixture: a module whose `host_ping` import has the signature
	// `() -> ()`, and a recording of two crossings into it. Both the module
	// and the recording are described in
	// `internal/boundarylog/void_import_fixture_test.go`, which also holds
	// the guard that keeps the committed `.ct` equal to what this suite's
	// producer replica builds.
	voidImportWasm       = boundaryTestdata + "/void_import.wasm"
	voidImportRecording  = boundaryTestdata + "/void-import.ct"
	stylusEntrypoint     = "testdata/stylus/entrypoint.wasm"
	stylusEntryTracePath = "testdata/stylus/entrypoint_trace.json"
)

// requireCtPrint skips only when `ct-print` is genuinely absent, which is
// the one audit-approved skip condition for these golden tests (see
// `tests/verify-cli-convention-no-silent-skip.sh` and the same guard in
// recorder_golden_test.go).
func requireCtPrint(t *testing.T) string {
	t.Helper()
	p := ctPrintPath(t)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.", p)
	}
	return p
}

// dumpFull pipes a produced `.ct` bundle through `ct-print --full
// --strip-paths` and decodes it.
func dumpFull(t *testing.T, outDir string) *goldenDoc {
	t.Helper()
	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	require.True(t, len(candidates) == 1,
		"expected exactly one .ct artefact under %s; got %v", outDir, candidates)

	out, err := exec.Command(requireCtPrint(t), "--full", "--strip-paths", candidates[0]).CombinedOutput()
	require.NoError(t, err, "ct-print --full should succeed; output:\n%s", string(out))

	var doc goldenDoc
	require.NoError(t, json.Unmarshal(out, &doc),
		"ct-print --full should emit valid JSON; got:\n%s", string(out))
	require.False(t, doc.Counts["steps"] == -1,
		"ct-print returned sentinel counts (-1) — the container could not be parsed")
	return &doc
}

// tracePaths lists the `.ct` artefacts under a directory. Used by the
// divergence test to prove nothing partial was left behind.
func tracePaths(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// A missing root is the strongest possible "no trace": report
			// it as no entries rather than as a walk failure.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if p != dir {
			found = append(found, p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	return found
}

// ===========================================================================
// verify_boundary_log_materialises_a_trace
// ===========================================================================

// TestVerifyBoundaryLogMaterialisesATrace replays the demo's `frontend-wasm`
// boundary log — a real browser recording of one call,
// `compute_balance(42, 100) -> 620` — against the original, uninstrumented
// module, and asserts the materialised trace carries source-level steps in
// `lib.rs` resolved through DWARF.
//
// The point of the assertion is the CONTRAST with the input. The browser
// recording contains three steps, all on line 71, and knows only
// `compute_balance`; the two private helpers `loyalty_bonus` (line 57) and
// `amount_credit` (line 62) never crossed the boundary and are absent from
// it entirely. Re-execution is what recovers them, so the test insists on
// finding them.
func TestVerifyBoundaryLogMaterialisesATrace(t *testing.T) {
	requireCtPrint(t)
	outDir := filepath.Join(t.TempDir(), "traces")

	exitCode, stdout, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording, "--out-dir=" + outDir, demoWasm})
	require.Equal(t, 0, exitCode, "replay should succeed; stderr:\n%s", stderr)
	require.True(t, strings.Contains(stdout, "replayed 1 exported call(s)"),
		"the CLI should report what it replayed; stdout:\n%s", stdout)

	doc := dumpFull(t, outDir)

	// ----- the trace resolves the module's own source through DWARF ------
	require.Equal(t, 1, len(doc.Paths), "expected one source path; got %v", doc.Paths)
	require.True(t, strings.HasSuffix(doc.Paths[0], "lib.rs"),
		"paths[0] should be the module's Rust source; got %q", doc.Paths[0])

	// ----- the interior the browser could not see ------------------------
	//
	// `compute_balance` is the only function the recording names. The two
	// private helpers exist only because the module was re-executed.
	require.Equal(t, []string{"compute_balance", "loyalty_bonus", "amount_credit"},
		doc.Functions,
		"the materialised trace must recover the private helpers the boundary "+
			"log never saw; got %v", doc.Functions)

	events := decodeEvents(t, doc)
	stepIndicesMonotonic(t, events)

	require.Equal(t, []string{"compute_balance", "loyalty_bonus", "amount_credit"},
		callSequence(events), "call_entry sequence")

	exits := callExitSequence(t, events)
	require.Equal(t, 3, len(exits), "call_exit count")
	require.Equal(t, "loyalty_bonus", exits[0].Function)
	require.Equal(t, int64(420), *exits[0].Return.I, "loyalty_bonus(42) = 42*10 = 420")
	require.Equal(t, "amount_credit", exits[1].Function)
	require.Equal(t, int64(200), *exits[1].Return.I, "amount_credit(100) = 100*2 = 200")
	require.Equal(t, "compute_balance", exits[2].Function)
	require.Equal(t, int64(620), *exits[2].Return.I,
		"compute_balance(42,100) = 420+200 = 620 — the value the browser recorded")

	// ----- exact counts and exact step lines -----------------------------
	//
	// Pinned literally so a recorder regression cannot go unnoticed. The
	// input recording carried three steps, all on line 71; re-execution
	// produces ten, walking the two helper bodies.
	require.Equal(t, 10, doc.Counts["steps"], "counts.steps")
	require.Equal(t, 3, doc.Counts["calls"], "counts.calls")
	require.Equal(t, 3, doc.Counts["functions"], "counts.functions")
	require.Equal(t, 1, doc.Counts["paths"], "counts.paths")
	require.Equal(t, 10, doc.Counts["values"], "counts.values")
	require.Equal(t, 0, doc.Counts["io_events"], "counts.io_events")
	require.Equal(t, []string{"user_id", "amount", "bonus", "credit"}, doc.Varnames,
		"the variable table must carry the helper locals, not just the boundary values")

	var lines []int64
	for _, ev := range events {
		if ev.Kind == "step" {
			lines = append(lines, ev.Line)
		}
	}
	// 72/73/74 are compute_balance's three statements, 75 its closing brace;
	// 57-58 and 62-63 are the two helpers' bodies, 59 and 64 their closing
	// braces.
	//
	// This sequence used to read `{72, 57, 58, 61, 73, 62, 63, 66, 74, 3368}`
	// — every closing brace displaced by two lines and the last one landing
	// on line 3368 of a 75-line file, a step no user could navigate to. Both
	// came from the recorder emitting a column the column-aware wire encoding
	// cannot address: DWARF reports a one-byte `}` line's epilogue at column
	// 2, the writer folds `column - 1` into the step's byte offset, and that
	// offset is the first offset of the *next* line — or, on the file's last
	// line, past the end of the per-line table, where the reader surfaces the
	// raw byte offset as a line number. 3368 is exactly the file's 3443 bytes
	// less its 75 newlines. See `addressableColumn` in
	// internal/engine/interpreter/interpreter.go.
	require.Equal(t, []int64{72, 57, 58, 59, 73, 62, 63, 64, 74, 75}, lines,
		"step lines, in trace order")

	// ----- the recorded arguments really drove the replay ----------------
	vars := collectVars(t, events)
	seen := map[string][]int64{}
	for _, v := range vars {
		if v.Kind == "Int" {
			seen[v.Varname] = append(seen[v.Varname], v.I)
		}
	}
	require.True(t, containsInt(seen["user_id"], 42),
		"the replayed `user_id` must be the recorded 42; got %v", seen["user_id"])
	require.True(t, containsInt(seen["amount"], 100),
		"the replayed `amount` must be the recorded 100; got %v", seen["amount"])
	require.True(t, containsInt(seen["bonus"], 420),
		"expected the intermediate `bonus` = 420; got %v", seen["bonus"])
	require.True(t, containsInt(seen["credit"], 200),
		"expected the intermediate `credit` = 200; got %v", seen["credit"])
}

func containsInt(hay []int64, needle int64) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

// TestBoundaryLogWorksWithoutAManifest pins that the sidecar manifest is a
// cross-check, not a requirement: spec §6 calls "a boundary recording plus
// the original .wasm" a complete input, and the signatures are read off the
// module's own type section.
func TestBoundaryLogWorksWithoutAManifest(t *testing.T) {
	requireCtPrint(t)
	tmp := t.TempDir()

	// Copy the module WITHOUT its manifest so discovery finds nothing.
	wasm, err := os.ReadFile(demoWasm)
	require.NoError(t, err)
	lonely := filepath.Join(tmp, "balance_calc.wasm")
	require.NoError(t, os.WriteFile(lonely, wasm, 0o600))
	require.Equal(t, "", func() string {
		if _, err := os.Stat(lonely + ".manifest.json"); err == nil {
			return lonely + ".manifest.json"
		}
		return ""
	}(), "the copied module must have no sidecar manifest")

	outDir := filepath.Join(tmp, "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording, "--out-dir=" + outDir, lonely})
	require.Equal(t, 0, exitCode, "replay should succeed without a manifest; stderr:\n%s", stderr)

	doc := dumpFull(t, outDir)
	require.Equal(t, []string{"compute_balance", "loyalty_bonus", "amount_credit"}, doc.Functions)
}

// TestManifestFromADifferentBuildIsRejected pins the cross-check when a
// manifest IS supplied.
func TestManifestFromADifferentBuildIsRejected(t *testing.T) {
	tmp := t.TempDir()
	bogus := filepath.Join(tmp, "bogus.manifest.json")
	require.NoError(t, os.WriteFile(bogus, []byte(
		`{"paths":[],"functions":[],"sites":[],"boundaries":[`+
			`{"fnKind":1,"fnIndex":1,"name":"compute_balance","module":"",`+
			`"params":["i64","i64"],"results":["i64"]}]}`), 0o600))

	outDir := filepath.Join(tmp, "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording, "--boundary-manifest=" + bogus,
		"--out-dir=" + outDir, demoWasm})
	require.Equal(t, 1, exitCode, "a mismatched manifest must fail")
	require.True(t, strings.Contains(stderr, "disagree about export"),
		"stderr should name the disagreement; got:\n%s", stderr)
	require.Equal(t, 0, len(tracePaths(t, outDir)), "no trace may be written")
}

// ===========================================================================
// verify_cross_modality_parity  (spec §10)
// ===========================================================================

// recordDirectInvocation is the "record M directly under wazero with a live
// host" leg of the §10 parity property. It drives wazero's ordinary public
// API — compile, instantiate with the recorder attached, call the exported
// function, write the bundle — with no boundary log anywhere in the path.
//
// It deliberately does NOT go through internal/boundarylog: comparing a
// boundary-log replay against itself would prove nothing.
func recordDirectInvocation(t *testing.T, wasmPath, outDir, traceName, export string, args ...uint64) []uint64 {
	t.Helper()
	ctx := context.Background()

	wasm, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { require.NoError(t, rt.Close(ctx)) }()

	compiled, err := rt.CompileModule(ctx, wasm)
	require.NoError(t, err)

	recorder := tracewriter.NewCtfsTraceWriter()
	mod, err := rt.InstantiateModuleWithRecord(ctx, compiled,
		wazero.NewModuleConfig().WithStartFunctions(), recorder)
	require.NoError(t, err)

	fn := mod.ExportedFunction(export)
	require.True(t, fn != nil, "module exports no %q", export)
	res, err := fn.Call(ctx, args...)
	require.NoError(t, err)

	workdir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, recorder.ProduceTrace(outDir, traceName, workdir))
	return res
}

// TestVerifyCrossModalityParity is the spec §10 property, stated there as:
//
//	Record module M in the browser, producing a boundary log. Re-execute M
//	from that log in codetracer-wasm-recorder, producing trace A. Record M
//	directly in codetracer-wasm-recorder with a live host, producing trace
//	B. A and B must be equal modulo timestamps.
//
// The browser leg is real: `frontend-wasm.ct` is the committed output of an
// actual browser session, not something this test synthesises. Trace A is
// produced by the `--boundary-log` CLI from that recording; trace B by
// driving wazero's public API directly with the same arguments.
//
// "Modulo timestamps" costs nothing to honour here because `ct-print
// --full` surfaces no timestamps at all: the comparison below is over the
// ENTIRE decoded document — path table, function table, variable-name
// table, every count, and every event with all its fields — with only the
// bundle's own identity (program name, workdir) excluded, since those name
// where the trace was written rather than what was executed.
func TestVerifyCrossModalityParity(t *testing.T) {
	requireCtPrint(t)
	tmp := t.TempDir()

	// --- trace A: materialised from the browser boundary recording -------
	outA := filepath.Join(tmp, "from-boundary-log")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording, "--out-dir=" + outA, demoWasm})
	require.Equal(t, 0, exitCode, "boundary-log replay failed; stderr:\n%s", stderr)
	docA := dumpFull(t, outA)

	// --- trace B: recorded by running the same module directly -----------
	outB := filepath.Join(tmp, "direct")
	res := recordDirectInvocation(t, demoWasm, outB, "balance_calc.wasm",
		"compute_balance", 42, 100)
	require.Equal(t, []uint64{620}, res,
		"the direct run must compute the same result the browser recorded")
	docB := dumpFull(t, outB)

	// ----- the two documents must agree in every particular --------------
	require.Equal(t, docB.Paths, docA.Paths, "path tables differ")
	require.Equal(t, docB.Functions, docA.Functions, "function tables differ")
	require.Equal(t, docB.Varnames, docA.Varnames, "variable-name tables differ")
	require.Equal(t, docB.Counts, docA.Counts, "counts differ")
	// The type table, by CONTENT — `counts["types"]` above is not a
	// substitute for it.
	//
	// The recorder's known way of getting types wrong preserves their
	// number: a recorder swapped onto a live `ModuleInstance` without its
	// `TypesIndex` produced slices "declaring `i64` where linear replay
	// declared `u32`, every other stream byte-identical" (AGENTS.md, "Two
	// traps" under Slices). One `i64` for one `u32` leaves `counts["types"]`
	// at exactly what it was, so a count comparison — which is all this
	// test had — would have watched that regression go by. What reaches the
	// debugger is the NAME behind each `type_id`, so that is what parity
	// has to be stated over.
	require.Equal(t, docB.Types, docA.Types, "type tables differ")
	require.Equal(t, docB.Metadata.Flags, docA.Metadata.Flags, "meta.dat flags differ")

	require.Equal(t, len(docB.Events), len(docA.Events), "event counts differ")
	for i := range docB.Events {
		require.Equal(t, string(docB.Events[i]), string(docA.Events[i]),
			"event %d differs between the boundary-log replay and the direct run.\n"+
				"  direct:       %s\n  boundary-log: %s",
			i, string(docB.Events[i]), string(docA.Events[i]))
	}

	// A parity test that compared two empty documents would pass
	// vacuously. Assert the traces are substantial.
	require.True(t, docA.Counts["steps"] > 0, "trace A has no steps")
	require.True(t, docA.Counts["calls"] >= 3, "trace A should carry all three frames")
	// Same argument, applied to the two comparisons added above: an empty
	// `types` array or an empty `flags` object would make each of them a
	// comparison of nothing against nothing, and both would pass.
	// `compute_balance` and its two helpers are all `u32`-typed, so the
	// table is exactly one entry deep — small, but it is the whole of what
	// the module's DWARF describes, and zero would mean the type stream
	// never reached the reader.
	require.True(t, len(docA.Types) > 0,
		"trace A carries no types, so the type-table comparison above compared "+
			"two empty tables and proved nothing; counts.types=%d",
		docA.Counts["types"])
	require.Equal(t, len(docA.Types), docA.Counts["types"],
		"the decoded type table must hold one entry per counts.types")
	require.True(t, len(docA.Metadata.Flags) > 0,
		"trace A surfaced no meta.dat flags, so the flag comparison above "+
			"compared two empty maps and proved nothing")
}

// ===========================================================================
// verify_divergence_is_a_hard_error
// ===========================================================================

// corruptRecording copies a `.ct` recording, replacing every occurrence of
// `old` in trace.json with `new`.
//
// Replacing EVERY occurrence matters: a recording is internally
// cross-referenced (a function's name appears in its `Function` record and
// again in each of its `<name>:arg<n>` bindings), so a partial substitution
// produces a structurally inconsistent recording that is rejected at parse
// time rather than replayed. Those inconsistencies are worth rejecting, but
// they are not what this test is about.
func corruptRecording(t *testing.T, src, dstParent, old, new string) string {
	t.Helper()
	dst := filepath.Join(dstParent, filepath.Base(src))
	require.NoError(t, os.MkdirAll(dst, 0o755))
	for _, name := range []string{"trace.json", "trace_metadata.json", "trace_paths.json"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		require.NoError(t, err)
		if name == "trace.json" {
			replaced := strings.ReplaceAll(string(data), old, new)
			require.True(t, replaced != string(data),
				"corruption target %q not found in %s — the fixture has changed", old, name)
			data = []byte(replaced)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dst, name), data, 0o600))
	}
	return dst
}

// TestVerifyDivergenceIsAHardError pins spec §6: divergence is an error,
// never a warning, and no partial trace is left behind.
//
// The milestone is explicit that asserting a non-zero exit alone is
// insufficient, so each case also asserts that the output directory holds
// NOTHING — not an empty `.ct`, not a half-written one, not a stray file.
func TestVerifyDivergenceIsAHardError(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		// wantIn are fragments the diagnostic must contain: the milestone
		// requires the recorded-vs-actual pair to be NAMED.
		wantIn []string
	}{
		{
			// The recording claims compute_balance returned 621; the module
			// really computes 620.
			name: "corrupted export return value",
			old:  `"kind":"Int","i":"620"`,
			new:  `"kind":"Int","i":"621"`,
			wantIn: []string{
				"diverged from the recording",
				"exported return value 0",
				"recorded: i32:621",
				"actual:   i32:620",
				"No trace has been written",
			},
		},
		{
			// The recording claims the host passed 43; it passed 42. This
			// one diverges on the RESULT, because the replayed call is
			// driven by the corrupted argument: 43*10 + 200 = 630.
			name: "corrupted export argument",
			old:  `"kind":"Int","i":"42"`,
			new:  `"kind":"Int","i":"43"`,
			wantIn: []string{
				"diverged from the recording",
				"recorded: i32:620",
				"actual:   i32:630",
			},
		},
		{
			// The recording names an export the module does not have.
			name: "unknown exported function",
			old:  "compute_balance",
			new:  "compute_balanc3",
			wantIn: []string{
				"exports no function named",
				"compute_balanc3",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			bad := corruptRecording(t, demoRecording, tmp, tc.old, tc.new)
			outDir := filepath.Join(tmp, "traces")

			exitCode, stdout, stderr := runMain(t, "", []string{
				"run", "--boundary-log=" + bad, "--out-dir=" + outDir, demoWasm})

			// (1) a hard error, not a warning
			require.Equal(t, 1, exitCode,
				"a divergence must exit non-zero; stdout:\n%s\nstderr:\n%s", stdout, stderr)
			for _, frag := range tc.wantIn {
				require.True(t, strings.Contains(stderr, frag),
					"the diagnostic must contain %q; stderr:\n%s", frag, stderr)
			}
			require.False(t, strings.Contains(strings.ToLower(stderr), "warning"),
				"a divergence must never be reported as a warning; stderr:\n%s", stderr)
			require.False(t, strings.Contains(stdout, "replayed"),
				"a diverged replay must not report success; stdout:\n%s", stdout)

			// (2) NO PARTIAL TRACE. This is the assertion the milestone
			//     calls out as the one that makes the test meaningful.
			leftovers := tracePaths(t, outDir)
			require.Equal(t, 0, len(leftovers),
				"a diverged replay must leave nothing behind, but %s contains: %v",
				outDir, leftovers)
		})
	}
}

// TestSuccessfulReplayDoesWriteATrace is the control for the test above: it
// proves the "no trace" assertions there are detecting the divergence
// policy rather than a recorder that never writes anything.
func TestSuccessfulReplayDoesWriteATrace(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording, "--out-dir=" + outDir, demoWasm})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)

	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	require.Equal(t, 1, len(candidates),
		"a successful replay must write exactly one .ct; got %v", candidates)
	// The same helper the divergence test uses to assert "nothing was
	// left behind" must SEE something here. Without this the negative
	// assertion could be passing because `tracePaths` looks in a place the
	// replay never writes to, which is a way for a divergence test to be
	// vacuously green.
	require.True(t, len(tracePaths(t, outDir)) > 0,
		"tracePaths must find the artefacts of a successful replay, or its use "+
			"as a negative assertion proves nothing; %s contained nothing", outDir)
}

// ===========================================================================
// M39 — a `() -> ()` import crossing is checked, and its divergence is hard
// ===========================================================================

// TestVerifyZeroArityImportDivergenceIsAHardError is the CLI-level proof
// that M39 turned an unchecked call into a checked one.
//
// The fixture records `ping_n(2)`: one exported call and two crossings into
// `host_ping`, whose signature is `() -> ()`. Neither crossing carries a
// single value — their realm markers are their whole trace on disk — so
// before M39 both were replayed unchecked, and the CLI said so on stderr
// and exited 0 whatever the module did.
//
// Both corruptions below are corruptions of a value-less crossing:
//
//   - the first changes how MANY of them the replayed module makes (the
//     export's argument is the only number in the recording that decides
//     it, and `ping_n` returns it, so the recording stays internally
//     consistent — nothing but the crossing count differs);
//   - the second changes WHICH import one of them names, by editing the
//     marker label that is the only place the index appears.
//
// Each must exit non-zero, name the recorded-vs-actual pair, and leave
// NOTHING on disk. `TestSuccessfulReplayDoesWriteATrace` is the control
// that the last of those can fail.
func TestVerifyZeroArityImportDivergenceIsAHardError(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		wantIn   []string
	}{
		{
			// `ping_n(3)` pings three times; the recording carries two
			// crossings. The third finds the recording exhausted.
			name: "more value-less crossings than the recording carries",
			old:  `"i":"2"`,
			new:  `"i":"3"`,
			wantIn: []string{
				"diverged from the recording",
				"import call",
				"the recording ends after 3 crossing(s)",
				"env.host_ping",
				"more times than it did when recorded",
				"No trace has been written",
			},
		},
		{
			// The markers claim the crossings were into import #1
			// (`host_add`); the module calls import #0 (`host_ping`).
			name: "a marker naming a different import index",
			old:  `wasm import #0`,
			new:  `wasm import #1`,
			wantIn: []string{
				"diverged from the recording",
				"import index",
				"recorded: import #1",
				"actual:   import #0 (env.host_ping)",
				"No trace has been written",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			bad := corruptRecording(t, voidImportRecording, tmp, tc.old, tc.new)
			outDir := filepath.Join(tmp, "traces")

			exitCode, stdout, stderr := runMain(t, "", []string{
				"run", "--boundary-log=" + bad, "--out-dir=" + outDir, voidImportWasm})

			require.Equal(t, 1, exitCode,
				"a divergence must exit non-zero; stdout:\n%s\nstderr:\n%s", stdout, stderr)
			for _, frag := range tc.wantIn {
				require.True(t, strings.Contains(stderr, frag),
					"the diagnostic must contain %q; stderr:\n%s", frag, stderr)
			}
			// The M37 fallback must be gone for this recording: an
			// unmatched `() -> ()` call is a divergence, not a note.
			require.False(t, strings.Contains(stderr, "replayed unchecked"),
				"a recording whose markers name the import edge must never fall back "+
					"to the unchecked-call note; stderr:\n%s", stderr)
			require.False(t, strings.Contains(strings.ToLower(stderr), "warning"),
				"a divergence must never be reported as a warning; stderr:\n%s", stderr)
			require.False(t, strings.Contains(stdout, "replayed"),
				"a diverged replay must not report success; stdout:\n%s", stdout)

			leftovers := tracePaths(t, outDir)
			require.Equal(t, 0, len(leftovers),
				"a diverged replay must leave nothing behind, but %s contains: %v",
				outDir, leftovers)
		})
	}
}

// TestZeroArityImportReplayIsCheckedNotCounted is the positive control for
// the test above, and the one that shows the crossings are *consumed*: the
// uncorrupted fixture replays, reports two imported calls, and prints no
// unchecked-call note at all.
func TestZeroArityImportReplayIsCheckedNotCounted(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "traces")
	exitCode, stdout, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + voidImportRecording, "--out-dir=" + outDir, voidImportWasm})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)
	require.True(t, strings.Contains(stdout, "replayed 1 exported call(s) and 2 imported call(s)"),
		"both `() -> ()` crossings must be counted as imported calls serviced "+
			"from the recording; stdout:\n%s", stdout)
	require.False(t, strings.Contains(stderr, "replayed unchecked"),
		"nothing may be taken on trust; stderr:\n%s", stderr)

	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	require.Equal(t, 1, len(candidates),
		"a successful replay must write exactly one .ct; got %v", candidates)
}

// ===========================================================================
// verify_uninstrumented_module_is_used
// ===========================================================================

// TestVerifyUninstrumentedModuleIsUsed pins spec §6.1 from both sides.
func TestVerifyUninstrumentedModuleIsUsed(t *testing.T) {
	// (1) The module replay is handed really has no `__codetracer`
	//     imports — it is the raw `cargo build` output, not the
	//     `ct-instrument` output.
	wasm, err := os.ReadFile(demoWasm)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(wasm), "__codetracer"),
		"the replay fixture must be the ORIGINAL module: it must not mention "+
			"the instrumenter's host module anywhere")
	require.False(t, strings.Contains(string(wasm), "__ct_emit_call"),
		"the replay fixture must not carry the instrumenter's hooks")

	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { require.NoError(t, rt.Close(ctx)) }()
	compiled, err := rt.CompileModule(ctx, wasm)
	require.NoError(t, err)
	require.Equal(t, 0, len(compiled.ImportedFunctions()),
		"the original module imports nothing; got %v", compiled.ImportedFunctions())

	// (2) And replaying it succeeds.
	outDir := filepath.Join(t.TempDir(), "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording, "--out-dir=" + outDir, demoWasm})
	require.Equal(t, 0, exitCode,
		"replay of the uninstrumented module must succeed; stderr:\n%s", stderr)

	// (3) Handing it the INSTRUMENTED module instead is refused with an
	//     explanation, rather than silently stubbing the hooks out and
	//     replaying against shifted import indices.
	exitCode, _, stderr = runMain(t, "", []string{
		"run", "--boundary-log=" + demoRecording,
		"--out-dir=" + filepath.Join(t.TempDir(), "traces2"), hookImportsWasm})
	require.Equal(t, 1, exitCode, "the instrumented module must be refused")
	require.True(t, strings.Contains(stderr, "__codetracer"),
		"the diagnostic must name the hook import; stderr:\n%s", stderr)
	require.True(t, strings.Contains(stderr, "uninstrumented"),
		"the diagnostic must say which module to pass; stderr:\n%s", stderr)
}

// ===========================================================================
// verify_stylus_replay_still_passes
// ===========================================================================

// TestVerifyStylusReplayStillPasses exercises the `--stylus` path end to
// end after the M37 generalisation.
//
// NOTE ON WHAT THIS TEST IS. Before M37 this repo had NO Go test for the
// Stylus replay path at all — `internal/stylus` was covered only by
// cross-repo suites in the `codetracer` repo (a devnode-backed recording
// test and a db-backend fixture test), neither of which runs here. So
// "the existing Stylus replay tests pass unchanged" could not be
// demonstrated from inside this repo. This test creates that missing
// coverage: it drives the real `--stylus` CLI against a real contract-shaped
// module and a real EVM trace, and asserts the hooks replayed the recorded
// data. See AGENTS.md, "Boundary-log replay vs. Stylus replay", for why the
// two paths stay separate.
func TestVerifyStylusReplayStillPasses(t *testing.T) {
	requireCtPrint(t)
	outDir := filepath.Join(t.TempDir(), "traces")

	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--stylus=" + stylusEntryTracePath, "--out-dir=" + outDir, stylusEntrypoint})
	require.Equal(t, 0, exitCode, "stylus replay should succeed; stderr:\n%s", stderr)
	require.False(t, strings.Contains(stderr, "mismatched return result"),
		"the recorded user_returned value must match what the module returned; "+
			"stderr:\n%s", stderr)
	require.False(t, strings.Contains(stderr, "error executing stylus entrypoint"),
		"stderr:\n%s", stderr)

	doc := dumpFull(t, outDir)

	// The EVM interaction events the `vm_hooks` stubs recorded. One per
	// replayed hook: read_args and write_result.
	//
	// The fixture has no DWARF, so the trace is event-only — the shape
	// AGENTS.md documents for Stylus traces built without debug info — and
	// `ct-print --full` emits an empty `events` array because io events are
	// anchored to steps and there are none. The counts are therefore the
	// assertion surface here.
	require.Equal(t, 2, doc.Counts["io_events"],
		"expected one io_event per replayed vm_hook (read_args, write_result)")
	require.Equal(t, 0, doc.Counts["steps"],
		"a Stylus module without DWARF produces an event-only trace")

	// The strongest assertion available: `user_entrypoint` returns the word
	// `read_args` wrote into linear memory, and `wazero.go` compares that
	// against the recorded `user_returned` result. So an exit without the
	// "mismatched return result" diagnostic proves the stub really replayed
	// the recorded calldata 0xdeadbeef into the module's memory — had it
	// written nothing, the module would have returned 0 and the comparison
	// against the recorded 0xefbeadde would have failed.
}

// TestStylusAndBoundaryLogAreMutuallyExclusive pins the guard the new mode
// added to the shared `run` path: each supplies the module's imports from a
// different recording, so accepting both would silently let one win.
func TestStylusAndBoundaryLogAreMutuallyExclusive(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--stylus=" + stylusEntryTracePath, "--boundary-log=" + demoRecording,
		"--out-dir=" + outDir, demoWasm})
	require.Equal(t, 1, exitCode)
	require.True(t, strings.Contains(stderr, "mutually exclusive"), "stderr:\n%s", stderr)
	require.Equal(t, 0, len(tracePaths(t, outDir)), "no trace may be written")
}

// TestBoundaryLogReportsAMissingRecordingClearly covers the ordinary
// user-error paths.
func TestBoundaryLogReportsAMissingRecordingClearly(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + filepath.Join(t.TempDir(), "nope.ct"),
		"--out-dir=" + outDir, demoWasm})
	require.Equal(t, 1, exitCode)
	require.True(t, strings.Contains(stderr, "error reading boundary log"), "stderr:\n%s", stderr)
	require.Equal(t, 0, len(tracePaths(t, outDir)))
}

// TestBoundaryLogUsageIsDocumented pins that the new mode is discoverable,
// per Recorder-CLI-Conventions.
func TestBoundaryLogUsageIsDocumented(t *testing.T) {
	_, _, stderr := runMain(t, "", []string{"run", "-h"})
	require.True(t, strings.Contains(stderr, "-boundary-log"),
		"`run -h` must document --boundary-log; got:\n%s", stderr)
	require.True(t, strings.Contains(stderr, "-boundary-manifest"),
		"`run -h` must document --boundary-manifest; got:\n%s", stderr)

	_, _, stderr = runMain(t, "", []string{})
	require.True(t, strings.Contains(stderr, "--boundary-log"),
		"the top-level usage must mention --boundary-log; got:\n%s", stderr)
}

// TestDemoManifestIsTheOneCtInstrumentEmitted guards the fixture itself: if
// the committed manifest ever stops describing the committed module, the
// tests above would be validating a coincidence.
func TestDemoManifestIsTheOneCtInstrumentEmitted(t *testing.T) {
	raw, err := os.ReadFile(demoManifest)
	require.NoError(t, err)
	var m struct {
		Boundaries []struct {
			FnKind  int      `json:"fnKind"`
			FnIndex uint32   `json:"fnIndex"`
			Name    string   `json:"name"`
			Params  []string `json:"params"`
			Results []string `json:"results"`
		} `json:"boundaries"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, 1, len(m.Boundaries))
	require.Equal(t, 1, m.Boundaries[0].FnKind, "fnKind 1 == export")
	require.Equal(t, uint32(1), m.Boundaries[0].FnIndex)
	require.Equal(t, "compute_balance", m.Boundaries[0].Name)
	require.Equal(t, []string{"i32", "i32"}, m.Boundaries[0].Params)
	require.Equal(t, []string{"i32"}, m.Boundaries[0].Results)
}

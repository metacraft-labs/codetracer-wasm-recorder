package boundarylog

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

// Fixtures. All four are real modules, compiled from the `.wat` sources
// committed beside them (or, for balance_calc, from the demo's `lib.rs`);
// nothing here is a stand-in for a module.
const (
	importsDemoWasm = "../../cmd/wazero/testdata/boundary-log/imports_demo.wasm"
	hookImportsWasm = "../../cmd/wazero/testdata/boundary-log/hook_imports.wasm"
	hostStateWasm   = "../../cmd/wazero/testdata/boundary-log/host_state.wasm"
	voidImportWasm  = "../../cmd/wazero/testdata/boundary-log/void_import.wasm"
	balanceCalcWasm = "../../cmd/wazero/testdata/boundary-log/balance_calc.wasm"
	balanceManifest = "../../cmd/wazero/testdata/boundary-log/balance_calc.wasm.manifest.json"
)

// replayFixture compiles a module and replays a recording against it. The
// recorder is nil throughout this package's tests: they assert on the
// replay's *control* behaviour (divergence detection, stub sequencing,
// host-state application), while the materialised trace is asserted
// end-to-end through the real CLI in cmd/wazero/boundary_log_test.go.
func replayFixture(t *testing.T, wasmPath, ctDir string, manifest *Manifest) (Result, error) {
	t.Helper()
	ctx := context.Background()

	wasm, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })

	compiled, err := rt.CompileModule(ctx, wasm)
	require.NoError(t, err)

	rec, err := LoadRecording(ctDir)
	require.NoError(t, err)

	return Replay(ctx, Options{
		Runtime:      rt,
		Compiled:     compiled,
		Recording:    rec,
		Manifest:     manifest,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	})
}

// importsDemoRecording builds the faithful recording of one
// `run(5)` call: host_add(5, 10) -> 15, host_note(15), returns 30.
func importsDemoRecording(t *testing.T) *recordingBuilder {
	t.Helper()
	b := newRecordingBuilder("imports-demo")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(15)}, nil)
		})
	return b
}

// TestReplayDrivesImportsFromTheRecording is the positive case for the
// generic import stubs: the module's host calls are serviced entirely from
// the recording, with no live host anywhere.
func TestReplayDrivesImportsFromTheRecording(t *testing.T) {
	dir := importsDemoRecording(t).write(t, t.TempDir())
	res, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.ExportCalls)
	require.Equal(t, 2, res.ImportCalls)
	require.Equal(t, 0, res.UncheckedImportCalls)
}

// TestReplayFeedsTheRecordedImportResultBack proves the stub's return value
// really drives the module: changing the recorded result of `host_add`
// changes what `run` computes, so the recorded export result has to move
// with it. If the stub were returning zero (or ignoring the recording), the
// module could not produce 62.
func TestReplayFeedsTheRecordedImportResultBack(t *testing.T) {
	b := newRecordingBuilder("fed-back")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(62)}, func() {
			// host_add(5, 10) is recorded as returning 31 — an answer no
			// real adder would give. run() must therefore return 62.
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(31)})
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(31)}, nil)
		})
	dir := b.write(t, t.TempDir())

	res, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.NoError(t, err,
		"the module must compute 62 from the recorded host result 31; if this "+
			"fails the stub is not feeding the recording back")
	require.Equal(t, 2, res.ImportCalls)
}

// TestDivergentImportArgumentIsAHardError pins spec §6: a mismatched
// argument aborts the replay naming the recorded-vs-actual pair.
func TestDivergentImportArgumentIsAHardError(t *testing.T) {
	b := newRecordingBuilder("bad-arg")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			// The module calls host_add(5, 10); the recording claims 99.
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(99)}, []jsValue{jsInt(15)})
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(15)}, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.Error(t, err)

	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "import argument 1", d.What)
	require.Equal(t, "i32:99", d.Recorded)
	require.Equal(t, "i32:10", d.Actual)
	require.True(t, strings.Contains(err.Error(), "env.host_add"),
		"the diagnostic must name the import; got: %v", err)
	require.True(t, strings.Contains(err.Error(), "No trace has been written"),
		"the diagnostic must state the trace policy; got: %v", err)
}

// TestDivergentImportIndexIsAHardError pins the "mismatched import index"
// arm of spec §6: replay reached a different host call than the recording
// says it should have.
func TestDivergentImportIndexIsAHardError(t *testing.T) {
	b := newRecordingBuilder("bad-index")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			// The module calls import #0 first; the recording says #1.
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(15)}, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "import index", d.What)
	require.Equal(t, "import #1", d.Recorded)
	require.True(t, strings.Contains(d.Actual, "import #0"), "got %q", d.Actual)
}

// TestUnrecordedImportCallIsAHardError covers the module crossing the
// boundary more times than the recording knows about.
func TestUnrecordedImportCallIsAHardError(t *testing.T) {
	b := newRecordingBuilder("too-few")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			// host_note is never recorded.
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.True(t, strings.Contains(err.Error(), "env.host_note"),
		"the diagnostic must name the unrecorded call; got: %v", err)
}

// TestUnconsumedCrossingIsAHardError is the mirror image: the recording
// carries a crossing the module never made.
func TestUnconsumedCrossingIsAHardError(t *testing.T) {
	b := newRecordingBuilder("too-many")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(15)}, nil)
			// One more than the module makes.
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(15)}, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "crossing count", d.What)
}

// TestDivergentExportReturnValueIsAHardError pins the §3.1 return-value
// check — "the cheapest possible check that the replay stayed faithful".
func TestDivergentExportReturnValueIsAHardError(t *testing.T) {
	b := newRecordingBuilder("bad-ret")
	b.export("run", 0, "/src/imports_demo.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(31)}, func() { // 30 is correct
			b.importCall(0, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			b.importCall(1, "/src/imports_demo.wat", 1,
				[]jsValue{jsInt(15)}, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "exported return value 0", d.What)
	require.Equal(t, "i32:31", d.Recorded)
	require.Equal(t, "i32:30", d.Actual)
}

// TestInstrumentedModuleIsRejected pins spec §6.1: replay runs the
// ORIGINAL module. Handing it the instrumented one — recognisable by its
// `__codetracer` hook imports — is refused with an explanation, not
// silently stubbed out.
func TestInstrumentedModuleIsRejected(t *testing.T) {
	b := newRecordingBuilder("instrumented")
	b.export("compute_balance", 1, "/src/lib.rs", 71,
		[]jsValue{jsInt(42), jsInt(100)}, []jsValue{jsInt(620)}, nil)
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, hookImportsWasm, dir, nil)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "__codetracer"),
		"the diagnostic must name the hook import; got: %v", err)
	require.True(t, strings.Contains(err.Error(), "uninstrumented"),
		"the diagnostic must say which module to pass; got: %v", err)
}

// TestMissingExportIsNamed covers a recording that does not match the
// module at all.
func TestMissingExportIsNamed(t *testing.T) {
	b := newRecordingBuilder("missing")
	b.export("does_not_exist", 0, "/src/imports_demo.wat", 1, nil, nil, nil)
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, importsDemoWasm, dir, nil)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "does_not_exist"), "got: %v", err)
	require.True(t, strings.Contains(err.Error(), "it exports: run"),
		"the diagnostic must list what the module does export; got: %v", err)
}

// TestManifestSignatureMismatchIsRejected pins the cross-check: a manifest
// from a different build of the module is a hard error, because replaying
// against it would mis-slice every value tuple.
func TestManifestSignatureMismatchIsRejected(t *testing.T) {
	dir := importsDemoRecording(t).write(t, t.TempDir())
	manifest := &Manifest{Boundaries: []ManifestBoundary{
		{FnKind: FuncKindImport, FnIndex: 0, Name: "host_add", Module: "env",
			Params: []string{"i32"}, Results: []string{"i32"}}, // module says (i32,i32)
	}}
	_, err := replayFixture(t, importsDemoWasm, dir, manifest)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "disagree about import #0"), "got: %v", err)
	require.True(t, strings.Contains(err.Error(), "different build"), "got: %v", err)
}

// TestManifestUnsupportedTypeIsRejected pins spec §8 on the replay side.
func TestManifestUnsupportedTypeIsRejected(t *testing.T) {
	dir := importsDemoRecording(t).write(t, t.TempDir())
	manifest := &Manifest{Boundaries: []ManifestBoundary{
		{FnKind: FuncKindImport, FnIndex: 0, Name: "host_add", Module: "env",
			UnsupportedType: "a reference type"},
	}}
	_, err := replayFixture(t, importsDemoWasm, dir, manifest)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "unsupported type"), "got: %v", err)
}

// TestTheRealDemoManifestValidatesTheRealDemoModule runs the cross-check in
// its intended configuration: the committed `ct-instrument` manifest
// against the module it was generated from.
func TestTheRealDemoManifestValidatesTheRealDemoModule(t *testing.T) {
	m, err := LoadManifest(balanceManifest)
	require.NoError(t, err)

	b := m.ExportBoundaryByName("compute_balance")
	require.NotNil(t, b)
	require.Equal(t, uint32(1), b.FnIndex)
	sig, err := b.Signature()
	require.NoError(t, err)
	require.Equal(t, "(i32, i32) -> (i32)", sig.String())

	res, err := replayFixture(t, balanceCalcWasm, demoRecordingPath, m)
	require.NoError(t, err)
	require.Equal(t, 1, res.ExportCalls)
	require.Equal(t, 0, res.ImportCalls)
}

// ---------------------------------------------------------------------------
// M39 — a `() -> ()` import crossing, recovered from its realm markers
// ---------------------------------------------------------------------------
//
// `void_import.wasm` imports `host_ping: () -> ()` at index 0. A crossing
// into it carries no arguments, no results, no `Call` and no `Return`, so
// its pair of `wasm import #0` realm markers is the whole of it on disk.
// The tests below check that the crossing is CHECKED and not merely
// counted: its index, its position in the interleaving and its call count
// all have to match, and each of them has a negative control.

// TestZeroArityImportCrossingIsConsumedFromTheRecording is the positive
// case: `ping_n(3)` makes three value-less crossings and every one of them
// is serviced from a recovered crossing.
func TestZeroArityImportCrossingIsConsumedFromTheRecording(t *testing.T) {
	b := newRecordingBuilder("void-ok")
	b.export("ping_n", 0, "/src/void_import.wat", 1,
		[]jsValue{jsInt(3)}, []jsValue{jsInt(3)}, func() {
			for i := 0; i < 3; i++ {
				b.importCall(0, "/src/void_import.wat", 1, nil, nil)
			}
		})
	dir := b.write(t, t.TempDir())

	res, err := replayFixture(t, voidImportWasm, dir, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.ExportCalls)
	require.Equal(t, 3, res.ImportCalls,
		"each `() -> ()` crossing is consumed from the recording")
	require.Equal(t, 0, res.UncheckedImportCalls,
		"nothing may be taken on trust once the markers name the edge")
}

// TestZeroArityImportCrossingIsCheckedAgainstItsIndex is the negative
// control the milestone asks for by name: a log whose marker names a
// DIFFERENT import index is a hard error naming the recorded-vs-actual
// pair.
//
// It is what proves the crossing is checked rather than counted: the
// recording has the right NUMBER of crossings, and only the index of one
// of them is wrong.
func TestZeroArityImportCrossingIsCheckedAgainstItsIndex(t *testing.T) {
	b := newRecordingBuilder("void-bad-index")
	b.export("ping_n", 0, "/src/void_import.wat", 1,
		[]jsValue{jsInt(2)}, []jsValue{jsInt(2)}, func() {
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
			// The module calls import #0 again; the recording says #1.
			b.importCall(1, "/src/void_import.wat", 1, nil, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, voidImportWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "import index", d.What)
	require.Equal(t, "import #1", d.Recorded)
	require.True(t, strings.Contains(d.Actual, "import #0"), "got %q", d.Actual)
	require.True(t, strings.Contains(d.Actual, "env.host_ping"),
		"the diagnostic must name the import the module actually called; got %q", d.Actual)
}

// TestZeroArityImportCrossingIsCheckedAgainstItsPosition proves the
// crossing joins the *cursor* stream and not a per-import tally: the
// recording carries the right crossings in the wrong order.
func TestZeroArityImportCrossingIsCheckedAgainstItsPosition(t *testing.T) {
	b := newRecordingBuilder("void-bad-order")
	// `run(7)` pings first and then calls host_add. The recording has them
	// the other way round.
	b.export("run", 1, "/src/void_import.wat", 1,
		[]jsValue{jsInt(7)}, []jsValue{jsInt(107)}, func() {
			b.importCall(1, "/src/void_import.wat", 1,
				[]jsValue{jsInt(7), jsInt(100)}, []jsValue{jsInt(107)})
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, voidImportWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "import index", d.What)
	require.Equal(t, "import #1", d.Recorded)
	require.True(t, strings.Contains(d.Actual, "import #0"), "got %q", d.Actual)
}

// TestZeroArityImportInterleavingIsReplayedFaithfully is that test's
// positive control: with the two crossings in the order the module makes
// them, the same recording replays.
func TestZeroArityImportInterleavingIsReplayedFaithfully(t *testing.T) {
	b := newRecordingBuilder("void-order-ok")
	b.export("run", 1, "/src/void_import.wat", 1,
		[]jsValue{jsInt(7)}, []jsValue{jsInt(107)}, func() {
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
			b.importCall(1, "/src/void_import.wat", 1,
				[]jsValue{jsInt(7), jsInt(100)}, []jsValue{jsInt(107)})
		})
	dir := b.write(t, t.TempDir())

	res, err := replayFixture(t, voidImportWasm, dir, nil)
	require.NoError(t, err)
	require.Equal(t, 2, res.ImportCalls)
	require.Equal(t, 0, res.UncheckedImportCalls)
}

// TestUnrecordedZeroArityImportCallDiverges is the milestone's second
// named check: a module that calls a `() -> ()` import MORE times than the
// recording carries fails loudly instead of being counted as unchecked.
func TestUnrecordedZeroArityImportCallDiverges(t *testing.T) {
	b := newRecordingBuilder("void-too-few")
	// `ping_n(3)` pings three times; the recording carries two.
	b.export("ping_n", 0, "/src/void_import.wat", 1,
		[]jsValue{jsInt(3)}, []jsValue{jsInt(3)}, func() {
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
		})
	dir := b.write(t, t.TempDir())

	res, err := replayFixture(t, voidImportWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "import call", d.What)
	require.True(t, strings.Contains(d.Recorded, "the recording ends after 3 crossing(s)"),
		"the diagnostic must say the recording ran out; got %q", d.Recorded)
	require.True(t, strings.Contains(err.Error(), "env.host_ping"),
		"the diagnostic must name the unrecorded call; got: %v", err)
	require.Equal(t, 0, res.UncheckedImportCalls,
		"the call must not be counted as unchecked — that is what M39 removed")
}

// TestUnconsumedZeroArityCrossingDiverges is the mirror image: the
// recording carries a value-less crossing the module never makes. Before
// M39 no crossing existed to be left over, so this could not be detected
// at all.
func TestUnconsumedZeroArityCrossingDiverges(t *testing.T) {
	b := newRecordingBuilder("void-too-many")
	b.export("ping_n", 0, "/src/void_import.wat", 1,
		[]jsValue{jsInt(1)}, []jsValue{jsInt(1)}, func() {
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
		})
	dir := b.write(t, t.TempDir())

	_, err := replayFixture(t, voidImportWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "crossing count", d.What)
	require.True(t, strings.Contains(d.Recorded, "import #0"), "got %q", d.Recorded)
}

// TestLegacyRecordingStillReplaysWithTheUncheckedNote is the milestone's
// third named check, and the backward-compatibility contract in one test.
//
// The recording is the same one `TestZeroArityImportInterleavingIsReplayedFaithfully`
// drives, rendered with the pre-M39 marker spelling. The parser recovers no
// crossing for the value-less call — it cannot, the markers do not say
// which edge they are — so the call is replayed unchecked and reported,
// exactly as under M37. What must NOT happen is a rejection, or a
// divergence blamed on the module.
func TestLegacyRecordingStillReplaysWithTheUncheckedNote(t *testing.T) {
	b := newRecordingBuilder("void-legacy")
	b.export("run", 1, "/src/void_import.wat", 1,
		[]jsValue{jsInt(7)}, []jsValue{jsInt(107)}, func() {
			b.legacyImportCall(0, "/src/void_import.wat", 1, nil, nil)
			b.legacyImportCall(1, "/src/void_import.wat", 1,
				[]jsValue{jsInt(7), jsInt(100)}, []jsValue{jsInt(107)})
		})
	dir := b.write(t, t.TempDir())

	rec, err := LoadRecording(dir)
	require.NoError(t, err)
	require.False(t, rec.MarkersIdentifyImports)
	require.Equal(t, 2, len(rec.Crossings),
		"the export and host_add; the value-less crossing is not recoverable here")

	res, err := replayFixture(t, voidImportWasm, dir, nil)
	require.NoError(t, err, "an older recording must replay, not be rejected")
	require.Equal(t, 1, res.ExportCalls)
	require.Equal(t, 1, res.ImportCalls)
	require.Equal(t, 1, res.UncheckedImportCalls,
		"the M37 behaviour, reported rather than hidden")
}

// ---------------------------------------------------------------------------
// spec §3.3 / §3.4 — host-supplied initial state and host mutation
// ---------------------------------------------------------------------------

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// le32 renders a little-endian i32 as bytes, matching `i32.load`.
func le32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// hostStateRecording drives host_state.wasm: `sum()` observes only the
// §3.3 initial state; `after_tick(1)` observes the §3.4 mutations the host
// made while servicing `tick`.
//
//	initial: memory[0..4] = 100, memory[4..8] = 7, counter = 5
//	sum()             -> 100 + 5   = 105
//	after_tick(1): tick(1) -> 0, and while servicing it the host writes
//	                  memory[4..8] = 70 and sets counter = 9
//	                  -> 70 + 9    = 79
func hostStateRecording(t *testing.T) (string, *recordingBuilder) {
	t.Helper()
	b := newRecordingBuilder("host-state")
	b.export("sum", 0, "/src/host_state.wat", 1, nil, []jsValue{jsInt(105)}, nil)
	b.export("after_tick", 1, "/src/host_state.wat", 2,
		[]jsValue{jsInt(1)}, []jsValue{jsInt(79)}, func() {
			b.importCall(0, "/src/host_state.wat", 2,
				[]jsValue{jsInt(1)}, []jsValue{jsInt(0)})
		})
	dir := b.write(t, t.TempDir())

	writeHostState(t, dir, map[string]any{
		"version": 1,
		"initial": map[string]any{
			"memories": []any{map[string]any{
				"module": "env", "name": "memory", "minPages": 1,
				"data": []any{
					map[string]any{"offset": 0, "bytesB64": b64(le32(100))},
					map[string]any{"offset": 4, "bytesB64": b64(le32(7))},
				},
			}},
			"globals": []any{map[string]any{
				"module": "env", "name": "counter", "type": "i32",
				"mutable": true, "value": "5",
			}},
		},
		// The `tick` call is crossing #2 (0 = sum, 1 = after_tick).
		"mutations": []any{map[string]any{
			"afterCrossing": 2,
			"memoryWrites": []any{map[string]any{
				"module": "env", "name": "memory", "offset": 4, "bytesB64": b64(le32(70)),
			}},
			"globalSets": []any{map[string]any{
				"module": "env", "name": "counter", "type": "i32", "value": "9",
			}},
		}},
	})
	return dir, b
}

// TestInitialStateAndMutationsAreApplied is the §3.3/§3.4 proof. Both
// exported results are checked against the recording by the replayer
// itself, so the test passing means the module really observed the recorded
// memory and globals: had the initial state not been applied, `sum` would
// have returned 0 and diverged; had the mutation not been applied at the
// recorded point, `after_tick` would have returned 7 + 5 = 12.
func TestInitialStateAndMutationsAreApplied(t *testing.T) {
	dir, _ := hostStateRecording(t)
	res, err := replayFixture(t, hostStateWasm, dir, nil)
	require.NoError(t, err)
	require.Equal(t, 2, res.ExportCalls)
	require.Equal(t, 1, res.ImportCalls)
}

// TestMissingInitialStateIsRefused pins the spec §8 discipline for the
// §3.3 input: with the sidecar removed, the module's imported memory has no
// recorded contents. Replay refuses up front and names what is missing,
// rather than supplying a zeroed memory and diverging later at a point
// unrelated to the cause.
func TestMissingInitialStateIsRefused(t *testing.T) {
	dir, _ := hostStateRecording(t)
	require.NoError(t, os.Remove(dir+"/"+HostStateFileName))

	_, err := replayFixture(t, hostStateWasm, dir, nil)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "imports memory env.memory"),
		"the diagnostic must name the memory; got: %v", err)
	require.True(t, strings.Contains(err.Error(), HostStateFileName),
		"the diagnostic must name the sidecar that should carry it; got: %v", err)
}

// TestZeroedInitialStateDiverges is the control that proves
// TestInitialStateAndMutationsAreApplied is really exercising §3.3: keep
// the sidecar's structure but empty the recorded contents, and `sum`
// computes 0 instead of the recorded 105.
func TestZeroedInitialStateDiverges(t *testing.T) {
	dir, _ := hostStateRecording(t)
	writeHostState(t, dir, map[string]any{
		"version": 1,
		"initial": map[string]any{
			"memories": []any{map[string]any{
				"module": "env", "name": "memory", "minPages": 1, "data": []any{},
			}},
			"globals": []any{map[string]any{
				"module": "env", "name": "counter", "type": "i32",
				"mutable": true, "value": "0",
			}},
		},
		"mutations": []any{},
	})

	_, err := replayFixture(t, hostStateWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "exported return value 0", d.What)
	require.Equal(t, "i32:105", d.Recorded)
	require.Equal(t, "i32:0", d.Actual)
}

// TestMissingMutationDiverges is the same control for §3.4: keep the
// initial state, drop only the mutation, and `after_tick` computes
// 7 + 5 = 12 instead of the recorded 79.
func TestMissingMutationDiverges(t *testing.T) {
	dir, _ := hostStateRecording(t)
	writeHostState(t, dir, map[string]any{
		"version": 1,
		"initial": map[string]any{
			"memories": []any{map[string]any{
				"module": "env", "name": "memory", "minPages": 1,
				"data": []any{
					map[string]any{"offset": 0, "bytesB64": b64(le32(100))},
					map[string]any{"offset": 4, "bytesB64": b64(le32(7))},
				},
			}},
			"globals": []any{map[string]any{
				"module": "env", "name": "counter", "type": "i32",
				"mutable": true, "value": "5",
			}},
		},
		"mutations": []any{},
	})

	_, err := replayFixture(t, hostStateWasm, dir, nil)
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d), "want a *DivergenceError, got %T: %v", err, err)
	require.Equal(t, "i32:79", d.Recorded)
	require.Equal(t, "i32:12", d.Actual)
}

// TestHostStateRejectsAnUnknownVersion pins the §8 refusal rule.
func TestHostStateRejectsAnUnknownVersion(t *testing.T) {
	dir, _ := hostStateRecording(t)
	writeHostState(t, dir, map[string]any{"version": 99})
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "version 99"), "got: %v", err)
}

// TestHostStateRejectsImportedTables pins the §8 list: imported tables
// mutated by the host are refused rather than silently degraded.
func TestHostStateRejectsImportedTables(t *testing.T) {
	dir, _ := hostStateRecording(t)
	writeHostState(t, dir, map[string]any{
		"version": 1,
		"initial": map[string]any{"tables": []any{map[string]any{"name": "t"}}},
	})
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "imported-table state"), "got: %v", err)
}

// TestHostStateRejectsAMutationOfANonMutableGlobal covers the validator.
func TestHostStateRejectsAMutationOfANonMutableGlobal(t *testing.T) {
	dir, _ := hostStateRecording(t)
	writeHostState(t, dir, map[string]any{
		"version": 1,
		"initial": map[string]any{
			"globals": []any{map[string]any{
				"module": "env", "name": "counter", "type": "i32",
				"mutable": false, "value": "5",
			}},
		},
		"mutations": []any{map[string]any{
			"afterCrossing": 2,
			"globalSets": []any{map[string]any{
				"module": "env", "name": "counter", "type": "i32", "value": "9",
			}},
		}},
	})
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "mutable imported global"), "got: %v", err)
}

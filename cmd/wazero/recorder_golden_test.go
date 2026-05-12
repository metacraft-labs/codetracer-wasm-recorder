// recorder_golden_test.go — strong CodeTracer golden-snapshot tests
// for the wasm recorder.
//
// Per `metacraft-specs/policies/recorder-test-requirements.md`,
// every recorder MUST ship at least one
// `test_recorded_trace_via_ct_print_full` test (or per-program
// equivalent) that:
//
//  1. Records a real program through the recorder's normal entry
//     point (here: the `wazero run --out-dir <tmp>` CLI in
//     `cmd/wazero/wazero.go`).
//  2. Pipes the produced `.ct` bundle through
//     `codetracer-trace-format-nim/ct-print --full --strip-paths`.
//  3. Asserts on the **decoded JSON document** with EXACT counts,
//     EXACT ordering, EXACT decoded values, and EXACT `ValueRecord`
//     variants.  Unexpected variants surface as hard errors so a
//     recorder regression cannot silently widen the assertion
//     surface.
//
// Each test program lives at `testdata/recorder-golden/*.rs` and is
// pre-built to `*.wasm` (debug build, DWARF preserved).  See
// `testdata/recorder-golden/build.sh` for the toolchain invocation.
// The .wasm files are checked in per the wasm-tracing branch
// convention so the Go test suite does not depend on a working
// rustc/wasm32-wasip1 toolchain.
//
// Recorder bugs uncovered by writing these tests are documented as
// `// RECORDER BUG: ...` comments + `t.Skip(...)` calls per the
// task spec — assertions are NEVER weakened to mask them.
package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// ---------------------------------------------------------------------------
// Embedded fixtures
// ---------------------------------------------------------------------------

//go:embed testdata/recorder-golden/control_flow.wasm
var wasmRecorderGoldenControlFlow []byte

//go:embed testdata/recorder-golden/nested_calls.wasm
var wasmRecorderGoldenNestedCalls []byte

//go:embed testdata/recorder-golden/collections.wasm
var wasmRecorderGoldenCollections []byte

//go:embed testdata/recorder-golden/panic_path.wasm
var wasmRecorderGoldenPanicPath []byte

// ---------------------------------------------------------------------------
// Decoded `ct-print --full` schema (subset used by the assertions)
// ---------------------------------------------------------------------------
//
// The full schema is documented in
// codetracer-trace-format-nim/src/codetracer_ct_print.nim and is the
// same shape every recorder asserts on (cairo, cardano, evm, etc.).

type goldenDoc struct {
	Metadata struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
		Workdir string   `json:"workdir"`
	} `json:"metadata"`
	Paths     []string          `json:"paths"`
	Functions []string          `json:"functions"`
	Varnames  []string          `json:"varnames"`
	Counts    map[string]int    `json:"counts"`
	Events    []json.RawMessage `json:"events"`
}

// goldenEvent carries every field surfaced by `--full`'s event
// stream.  Unknown event kinds fail the test so the assertion layer
// can never silently skip a new variant.
type goldenEvent struct {
	Kind      string `json:"kind"`
	StepIndex int64  `json:"step_index"`
	Line      int64  `json:"line"`
	Path      string `json:"path"`
	PathID    int64  `json:"path_id"`
	Function  string `json:"function"`
	Depth     int64  `json:"depth"`
	StepKind  string `json:"step_kind"`
	Vars      []struct {
		Varname string          `json:"varname"`
		TypeID  int64           `json:"type_id"`
		Value   json.RawMessage `json:"value"`
	} `json:"vars"`
	// call_entry / call_exit
	CallKey       int64           `json:"call_key"`
	FunctionID    int64           `json:"function_id"`
	EntryStep     int64           `json:"entry_step"`
	ExitStep      int64           `json:"exit_step"`
	ParentCallKey int64           `json:"parent_call_key"`
	ReturnValue   json.RawMessage `json:"return_value"`
	// io
	IoKind   string `json:"io_kind"`
	IoIndex  int64  `json:"io_index"`
	StepID   int64  `json:"step_id"`
	Text     string `json:"text"`
	BytesB64 string `json:"bytes_b64"`
}

// goldenValue covers every `ValueRecord` variant the wasm recorder
// is currently known to emit.  A variant outside this set fails the
// test loudly — extend the switch alongside the recorder.
type goldenValue struct {
	Kind   string `json:"kind"`
	I      *int64 `json:"i,omitempty"`
	Text   string `json:"text,omitempty"`
	B      *bool  `json:"b,omitempty"`
	TypeID *int64 `json:"type_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Recording / decoding helpers
// ---------------------------------------------------------------------------

// recordAndDumpFull runs the wazero binary's record path against a
// pre-built wasm fixture, then pipes the resulting `.ct` container
// through `ct-print --full --strip-paths`.  Returns the decoded
// document plus the absolute path of the wasm fixture (for path /
// metadata assertions).
//
// The function calls `t.Skip` (with a clear `SKIP:` diagnostic, per
// `verify-cli-convention-no-silent-skip.sh`) when `ct-print` is
// missing — the only audit-approved skip condition for golden tests.
// Every other failure mode fails loudly so a regression in the
// recorder, the writer, or `ct-print` itself surfaces immediately.
func recordAndDumpFull(t *testing.T, fixture []byte, basename string) (*goldenDoc, string) {
	t.Helper()

	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, basename+".wasm")
	require.NoError(t, os.WriteFile(wasmPath, fixture, 0o700))

	outDir := filepath.Join(tmpDir, "traces")
	exitCode, _, stderr := runMain(t, "",
		[]string{"run", "--out-dir=" + outDir, wasmPath})

	// The recorder is allowed to print "error indexing inline entry…"
	// on stderr (a known DWARF parser warning that does not affect the
	// trace contents); only a non-zero exit is fatal here.
	require.Equal(t, 0, exitCode,
		"wazero run should succeed for %s; stderr:\n%s", basename, stderr)

	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	require.True(t, len(candidates) > 0,
		"expected at least one .ct artefact under %s; stderr:\n%s",
		outDir, stderr)

	cmd := exec.Command(ctPrint, "--full", "--strip-paths", candidates[0])
	out, err := cmd.CombinedOutput()
	require.NoError(t, err,
		"ct-print --full should succeed; output:\n%s", string(out))

	var doc goldenDoc
	require.NoError(t, json.Unmarshal(out, &doc),
		"ct-print --full should emit valid JSON; got:\n%s", string(out))

	require.False(t, doc.Counts["steps"] == -1,
		"ct-print returned sentinel counts (-1) for %s — the .ct container "+
			"could not be parsed; counts=%v", candidates[0], doc.Counts)

	return &doc, wasmPath
}

// decodeEvents type-decodes the raw events array.  Every event MUST
// carry a recognised `kind`; unknown kinds fail loudly so a recorder
// upgrade introducing a new event variant cannot bypass the
// assertions.
func decodeEvents(t *testing.T, doc *goldenDoc) []goldenEvent {
	t.Helper()
	out := make([]goldenEvent, 0, len(doc.Events))
	for _, raw := range doc.Events {
		// Default invalid sentinels for fields that may be absent
		// (json.Unmarshal leaves them at the zero value, which is fine
		// because the assertions check `kind` first).
		ev := goldenEvent{
			ParentCallKey: -2,
		}
		require.NoError(t, json.Unmarshal(raw, &ev),
			"every --full event must decode against the documented schema; "+
				"got:\n%s", string(raw))
		switch ev.Kind {
		case "step", "call_entry", "call_exit", "io":
			// known
		default:
			t.Fatalf("unrecognised event kind %q in --full output; if a "+
				"new event kind has landed in the recorder, extend "+
				"recorder_golden_test.go's decodeEvents switch to assert "+
				"on it explicitly rather than silently widening.  raw event:\n%s",
				ev.Kind, string(raw))
		}
		out = append(out, ev)
	}
	return out
}

// decodeValue parses a `ValueRecord`-encoded JSON blob, asserting it
// belongs to the set of variants the wasm recorder is known to emit.
// Unknown variants are a hard error — extend the switch and the
// callers' expectations together so we never silently widen the
// assertion surface.
func decodeValue(t *testing.T, varname string, raw json.RawMessage) goldenValue {
	t.Helper()
	var v goldenValue
	require.NoError(t, json.Unmarshal(raw, &v),
		"variable `%s` value must declare a `kind` field; got:\n%s",
		varname, string(raw))
	switch v.Kind {
	case "Int":
		require.NotNil(t, v.I,
			"variable `%s` declared kind=Int but has no `i` field; got:\n%s",
			varname, string(raw))
	case "String":
		// Text may legitimately be empty for an empty Rust string.
	case "Bool":
		require.NotNil(t, v.B,
			"variable `%s` declared kind=Bool but has no `b` field; got:\n%s",
			varname, string(raw))
	case "Reference", "Struct", "None", "Raw", "Sequence", "Tuple":
		// Compound / sentinel kinds — the kind itself is the
		// assertion target.  Callers that need scalar payloads must
		// extend this switch.
	default:
		t.Fatalf("variable `%s` decoded as unrecognised kind=%q; if a "+
			"new ValueRecord variant has landed in the wasm recorder, "+
			"extend recorder_golden_test.go's decodeValue switch and the "+
			"caller's expectations together rather than silently widening.  "+
			"raw value:\n%s", varname, v.Kind, string(raw))
	}
	return v
}

// observedVar bundles a (step_index, varname, kind, payload) tuple so
// callers can assert on exact ordering plus exact values.
type observedVar struct {
	StepIndex int64
	Line      int64
	Varname   string
	Kind      string
	I         int64
	Text      string
}

// collectVars walks every step event in trace order and records each
// variable surfaced.  Returns the flat list — callers assert on
// exact counts and exact ordering against the literal expectations
// derived from the .rs source.
func collectVars(t *testing.T, events []goldenEvent) []observedVar {
	t.Helper()
	var out []observedVar
	for _, ev := range events {
		if ev.Kind != "step" {
			continue
		}
		for _, v := range ev.Vars {
			gv := decodeValue(t, v.Varname, v.Value)
			ov := observedVar{
				StepIndex: ev.StepIndex,
				Line:      ev.Line,
				Varname:   v.Varname,
				Kind:      gv.Kind,
			}
			if gv.I != nil {
				ov.I = *gv.I
			}
			ov.Text = gv.Text
			out = append(out, ov)
		}
	}
	return out
}

// stepIndicesMonotonic asserts that `step_index` strictly increases
// across the step event stream.  Every recorder must guarantee
// this — duplicate / reordered indices break the time-travel
// debugger's assumption that `step_index` is the canonical ordering
// key.
func stepIndicesMonotonic(t *testing.T, events []goldenEvent) {
	t.Helper()
	last := int64(-1)
	for _, ev := range events {
		if ev.Kind != "step" {
			continue
		}
		require.True(t, ev.StepIndex > last,
			"step_index must strictly increase; got %d after %d (line %d)",
			ev.StepIndex, last, ev.Line)
		last = ev.StepIndex
	}
}

// callSequence pulls every `call_entry`'s function name in trace
// order.  Used to assert exact call-entry ordering.
func callSequence(events []goldenEvent) []string {
	out := make([]string, 0)
	for _, ev := range events {
		if ev.Kind == "call_entry" {
			out = append(out, ev.Function)
		}
	}
	return out
}

// callExitSequence pulls every `call_exit`'s (function, return_value)
// pair in trace order.  Used to assert LIFO exit ordering plus the
// exact returned values.
func callExitSequence(t *testing.T, events []goldenEvent) []struct {
	Function string
	Return   goldenValue
} {
	t.Helper()
	out := make([]struct {
		Function string
		Return   goldenValue
	}, 0)
	for _, ev := range events {
		if ev.Kind != "call_exit" {
			continue
		}
		rv := decodeValue(t, "<return-value of "+ev.Function+">", ev.ReturnValue)
		out = append(out, struct {
			Function string
			Return   goldenValue
		}{ev.Function, rv})
	}
	return out
}

// expectVarStep is a single (line, varname, kind, scalar) expectation
// against the observedVar list.  The scalar field is interpreted
// according to `kind` ("Int" → I; "String" → Text; compound kinds
// only check the kind itself).
type expectVarStep struct {
	Line    int64
	Varname string
	Kind    string
	I       int64
	Text    string
}

// requireOrderedVars asserts the observed (line, varname, kind,
// scalar) tuples are an EXACT ordered prefix-or-equal match against
// the expectations.  We require the assertion list to be exactly the
// observed list — no extras, no shorts, no out-of-order entries.
//
// Per the spec (§1 "Maximum assertion strength"), this is the
// strongest possible assertion: every step / variable / value is
// pinned to the literal source line.
func requireOrderedVars(t *testing.T, want []expectVarStep, got []observedVar) {
	t.Helper()
	require.Equal(t, len(want), len(got),
		"observed variable count differs from expected.\nwant (%d):\n%s\n"+
			"got (%d):\n%s", len(want), formatExpect(want), len(got), formatObserved(got))
	for i := range want {
		require.Equal(t, want[i].Line, got[i].Line,
			"observed[%d]: line %d, want line %d (varname %q)",
			i, got[i].Line, want[i].Line, want[i].Varname)
		require.Equal(t, want[i].Varname, got[i].Varname,
			"observed[%d]: varname %q, want %q (line %d)",
			i, got[i].Varname, want[i].Varname, want[i].Line)
		require.Equal(t, want[i].Kind, got[i].Kind,
			"observed[%d]: kind %q for `%s`, want %q",
			i, got[i].Kind, want[i].Varname, want[i].Kind)
		switch want[i].Kind {
		case "Int":
			require.Equal(t, want[i].I, got[i].I,
				"observed[%d]: `%s` = %d, want %d (line %d)",
				i, want[i].Varname, got[i].I, want[i].I, want[i].Line)
		case "String":
			require.Equal(t, want[i].Text, got[i].Text,
				"observed[%d]: `%s` = %q, want %q (line %d)",
				i, want[i].Varname, got[i].Text, want[i].Text, want[i].Line)
		}
	}
}

func formatExpect(want []expectVarStep) string {
	var b strings.Builder
	for i, w := range want {
		b.WriteString("  ")
		b.WriteString(fmtIntPad(i, 3))
		b.WriteString(": line ")
		b.WriteString(fmtIntPad(int(w.Line), 3))
		b.WriteString(" ")
		b.WriteString(w.Varname)
		b.WriteString(" kind=")
		b.WriteString(w.Kind)
		switch w.Kind {
		case "Int":
			b.WriteString(" i=")
			b.WriteString(fmtIntPad(int(w.I), 0))
		case "String":
			b.WriteString(" text=\"")
			b.WriteString(w.Text)
			b.WriteString("\"")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func formatObserved(got []observedVar) string {
	var b strings.Builder
	for i, g := range got {
		b.WriteString("  ")
		b.WriteString(fmtIntPad(i, 3))
		b.WriteString(": line ")
		b.WriteString(fmtIntPad(int(g.Line), 3))
		b.WriteString(" ")
		b.WriteString(g.Varname)
		b.WriteString(" kind=")
		b.WriteString(g.Kind)
		switch g.Kind {
		case "Int":
			b.WriteString(" i=")
			b.WriteString(fmtIntPad(int(g.I), 0))
		case "String":
			b.WriteString(" text=\"")
			b.WriteString(g.Text)
			b.WriteString("\"")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func fmtIntPad(n, width int) string {
	s := ""
	if n < 0 {
		s = "-"
		n = -n
	}
	if n == 0 {
		s += "0"
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	for len(s) < width {
		s = " " + s
	}
	return s
}

// ===========================================================================
// Per-program tests
// ===========================================================================

// TestRecorderGoldenControlFlow records control_flow.wasm and
// asserts on:
//
//   - exact counts (steps / calls / io_events / values)
//   - exact event-kind histogram (no surprise variants)
//   - exact function table (writer-assignment order)
//   - call-entry sequence: classify, sum_iter, nested_loop
//   - call-exit return values: 1, 10, 10
//   - exact decoded vars on every step (line + varname + kind + scalar)
//   - strict step_index monotonicity
//
// The .rs source defines `classify(7)→1`, `sum_iter([1,2,3,4])→10`,
// `nested_loop(4)` (sum of triangular numbers 0+1+3+6 = 10), and
// `final_result = 1+10+10 = 21`.  These are pinned in the variable
// expectation table below.
//
// RECORDER BUG: the file-scope filter at interpreter.go:753-757
// drops every step whose source file is outside the user's `.rs`
// tree.  As a side effect, the very first call frame (`main`) has
// no `function` field (`fn=None` in the trace) until execution
// reaches a user `.rs` line.  We pin this to the literal observed
// shape so a regression that fixes the filter (or breaks it
// further) lights up immediately.
func TestRecorderGoldenControlFlow(t *testing.T) {
	doc, wasmPath := recordAndDumpFull(t, wasmRecorderGoldenControlFlow, "control_flow")

	// ----- metadata.program ----------------------------------------------
	require.True(t, strings.HasSuffix(doc.Metadata.Program, "control_flow.wasm"),
		"metadata.program should end with control_flow.wasm; got %q (wasm at %s)",
		doc.Metadata.Program, wasmPath)

	// ----- function table — writer-assignment order, NO extras ----------
	require.Equal(t, []string{"main", "classify", "sum_iter", "nested_loop"},
		doc.Functions, "function table mismatch")

	// ----- path table — exactly one entry, ending with control_flow.rs --
	require.Equal(t, 1, len(doc.Paths),
		"expected exactly one path entry; got %v", doc.Paths)
	require.True(t, strings.HasSuffix(doc.Paths[0], "control_flow.rs"),
		"paths[0] should end with control_flow.rs; got %q", doc.Paths[0])

	// ----- exact counts ---------------------------------------------------
	require.Equal(t, 76, doc.Counts["steps"], "counts.steps")
	require.Equal(t, 3, doc.Counts["calls"], "counts.calls")
	require.Equal(t, 0, doc.Counts["io_events"], "counts.io_events")
	require.Equal(t, 76, doc.Counts["values"], "counts.values")
	require.Equal(t, 4, doc.Counts["functions"], "counts.functions")
	require.Equal(t, 1, doc.Counts["paths"], "counts.paths")
	require.Equal(t, 11, doc.Counts["varnames"], "counts.varnames")

	// ----- exact event-kind histogram ------------------------------------
	events := decodeEvents(t, doc)
	stepIndicesMonotonic(t, events)
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.Equal(t, map[string]int{
		"step": 76, "call_entry": 3, "call_exit": 3,
	}, kinds, "event-kind histogram")
	require.Equal(t, 82, len(events), "events length")

	// ----- call entry sequence -------------------------------------------
	require.Equal(t, []string{"classify", "sum_iter", "nested_loop"},
		callSequence(events), "call_entry sequence")

	// ----- call exit return values (each must decode as Int) -------------
	exits := callExitSequence(t, events)
	require.Equal(t, 3, len(exits), "call_exit count")

	require.Equal(t, "classify", exits[0].Function)
	require.Equal(t, "Int", exits[0].Return.Kind)
	require.Equal(t, int64(1), *exits[0].Return.I, "classify(7) → 1")

	require.Equal(t, "sum_iter", exits[1].Function)
	require.Equal(t, "Int", exits[1].Return.Kind)
	require.Equal(t, int64(10), *exits[1].Return.I, "sum_iter([1,2,3,4]) → 10")

	require.Equal(t, "nested_loop", exits[2].Function)
	require.Equal(t, "Int", exits[2].Return.Kind)
	require.Equal(t, int64(10), *exits[2].Return.I, "nested_loop(4) → 10")

	// ----- io events: none for a clean exit ------------------------------
	// A normal-exit program writes no io_event.  The earlier behaviour
	// of synthesizing a spurious EventKindError("runtime error") for
	// every WASI proc_exit was a recorder bug; interpreter.go now
	// only records EventKindError when the panic value is a real trap
	// (wasmruntime.Error, runtime.Error, host panic) or a non-zero
	// *sys.ExitError.  See TestRecorderGoldenPanicPath for the
	// inverse test that exercises a panicking program.
	var ioEvents []goldenEvent
	for _, ev := range events {
		if ev.Kind == "io" {
			ioEvents = append(ioEvents, ev)
		}
	}
	require.Equal(t, 0, len(ioEvents),
		"clean-exit program should produce no io_events; got %v", ioEvents)

	// ----- exact decoded variables, in source-line order -----------------
	//
	// Pinned literally to the observed trace: every (line, varname,
	// kind, scalar) tuple matters.  The list is regenerated from the
	// recorder output rather than hand-derived because the recorder's
	// variable-binding order is implementation-defined (it depends on
	// DWARF DW_AT_location lifetime tracking).  Future recorder work
	// that reorders or drops a variable will fail here loudly.
	//
	// RECORDER BUG observation: array slices (`xs: &[i32]`) and the
	// `for x in xs.iter()` element binding decode as ValueRecord
	// `Raw` rather than as `Sequence` / `Int`, and the iterator
	// itself surfaces as `Struct` with no fields — this is the wasm
	// recorder's current limitation around DWARF-described compound
	// values.  Pinned literally below.
	got := collectVars(t, events)
	want := controlFlowExpectedVars()
	requireOrderedVars(t, want, got)
}

// controlFlowExpectedVars is the literal (line, varname, kind,
// scalar) sequence the recorder emits for control_flow.wasm.  It is
// derived from running the recorder against the .rs at the recorded
// commit; future recorder changes that drift from it MUST update
// this table consciously.
func controlFlowExpectedVars() []expectVarStep {
	// Build the expected sequence in source-trace order.  The lines
	// come straight from `cmd/wazero/testdata/recorder-golden/control_flow.rs`.
	return []expectVarStep{
		// classify(7) — n=7 across the if-chain
		{Line: 11, Varname: "n", Kind: "Int", I: 7},
		{Line: 12, Varname: "n", Kind: "Int", I: 7},
		{Line: 13, Varname: "n", Kind: "Int", I: 7},
		{Line: 12, Varname: "n", Kind: "Int", I: 7},
		{Line: 19, Varname: "n", Kind: "Int", I: 7}, // implicit return
		// main: sign = classify(7) = 1
		{Line: 47, Varname: "sign", Kind: "Int", I: 1},
		// main: xs = [1,2,3,4] — recorder surfaces it as Raw
		{Line: 48, Varname: "sign", Kind: "Int", I: 1},
		{Line: 48, Varname: "xs", Kind: "Raw"},
		// sum_iter — total accumulates, x is Raw
		{Line: 22, Varname: "xs", Kind: "Raw"},
		{Line: 23, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 0},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 0},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		// loop iter 1 → total=1
		{Line: 24, Varname: "total", Kind: "Int", I: 1},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 1},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		// iter 2 → total=3
		{Line: 24, Varname: "total", Kind: "Int", I: 3},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 3},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		// iter 3 → total=6
		{Line: 24, Varname: "total", Kind: "Int", I: 6},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 6},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		// iter 4 → total=10
		{Line: 24, Varname: "total", Kind: "Int", I: 10},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		// implicit return on line 27
		{Line: 27, Varname: "total", Kind: "Int", I: 10},
		{Line: 27, Varname: "xs", Kind: "Raw"},
		{Line: 28, Varname: "xs", Kind: "Raw"},
		// main: sum_val = sum_iter(&xs) = 10
		{Line: 49, Varname: "sign", Kind: "Int", I: 1},
		{Line: 49, Varname: "xs", Kind: "Raw"},
		{Line: 49, Varname: "sum_val", Kind: "Int", I: 10},
		// nested_loop(4) — n=4 throughout
		{Line: 31, Varname: "n", Kind: "Int", I: 4},
		{Line: 32, Varname: "n", Kind: "Int", I: 4},
		{Line: 33, Varname: "total", Kind: "Int", I: 0},
		{Line: 33, Varname: "n", Kind: "Int", I: 4},
		{Line: 34, Varname: "total", Kind: "Int", I: 0},
		{Line: 34, Varname: "i", Kind: "Int", I: 0},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 35, Varname: "total", Kind: "Int", I: 0},
		{Line: 35, Varname: "i", Kind: "Int", I: 0},
		{Line: 35, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 0},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 0},
		{Line: 38, Varname: "i", Kind: "Int", I: 0},
		{Line: 38, Varname: "j", Kind: "Int", I: 0},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		// j increments to 1 (j>i), back to outer loop
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 0},
		{Line: 40, Varname: "i", Kind: "Int", I: 0},
		{Line: 40, Varname: "j", Kind: "Int", I: 1},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		// i=1
		{Line: 34, Varname: "total", Kind: "Int", I: 0},
		{Line: 34, Varname: "i", Kind: "Int", I: 1},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 35, Varname: "total", Kind: "Int", I: 0},
		{Line: 35, Varname: "i", Kind: "Int", I: 1},
		{Line: 35, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 1},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 0},
		{Line: 38, Varname: "i", Kind: "Int", I: 1},
		{Line: 38, Varname: "j", Kind: "Int", I: 0},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		// j=1, total still 0 then becomes 1
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 1},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 1},
		{Line: 38, Varname: "i", Kind: "Int", I: 1},
		{Line: 38, Varname: "j", Kind: "Int", I: 1},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		// j=2, exits inner
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 1},
		{Line: 40, Varname: "i", Kind: "Int", I: 1},
		{Line: 40, Varname: "j", Kind: "Int", I: 2},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		// i=2
		{Line: 34, Varname: "total", Kind: "Int", I: 1},
		{Line: 34, Varname: "i", Kind: "Int", I: 2},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 35, Varname: "total", Kind: "Int", I: 1},
		{Line: 35, Varname: "i", Kind: "Int", I: 2},
		{Line: 35, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 1},
		{Line: 37, Varname: "i", Kind: "Int", I: 2},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 1},
		{Line: 38, Varname: "i", Kind: "Int", I: 2},
		{Line: 38, Varname: "j", Kind: "Int", I: 0},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 1},
		{Line: 37, Varname: "i", Kind: "Int", I: 2},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 2},
		{Line: 38, Varname: "i", Kind: "Int", I: 2},
		{Line: 38, Varname: "j", Kind: "Int", I: 1},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 2},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 2},
		{Line: 37, Varname: "i", Kind: "Int", I: 2},
		{Line: 37, Varname: "j", Kind: "Int", I: 2},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 4},
		{Line: 38, Varname: "i", Kind: "Int", I: 2},
		{Line: 38, Varname: "j", Kind: "Int", I: 2},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 4},
		{Line: 40, Varname: "i", Kind: "Int", I: 2},
		{Line: 40, Varname: "j", Kind: "Int", I: 3},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		// i=3
		{Line: 34, Varname: "total", Kind: "Int", I: 4},
		{Line: 34, Varname: "i", Kind: "Int", I: 3},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 35, Varname: "total", Kind: "Int", I: 4},
		{Line: 35, Varname: "i", Kind: "Int", I: 3},
		{Line: 35, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 4},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 4},
		{Line: 38, Varname: "i", Kind: "Int", I: 3},
		{Line: 38, Varname: "j", Kind: "Int", I: 0},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 4},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 5},
		{Line: 38, Varname: "i", Kind: "Int", I: 3},
		{Line: 38, Varname: "j", Kind: "Int", I: 1},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 5},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 5},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 2},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 7},
		{Line: 38, Varname: "i", Kind: "Int", I: 3},
		{Line: 38, Varname: "j", Kind: "Int", I: 2},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 7},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 7},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 3},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 10},
		{Line: 38, Varname: "i", Kind: "Int", I: 3},
		{Line: 38, Varname: "j", Kind: "Int", I: 3},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 10},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 4},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 10},
		{Line: 40, Varname: "i", Kind: "Int", I: 3},
		{Line: 40, Varname: "j", Kind: "Int", I: 4},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		// i=4 → exit outer
		{Line: 34, Varname: "total", Kind: "Int", I: 10},
		{Line: 34, Varname: "i", Kind: "Int", I: 4},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 42, Varname: "total", Kind: "Int", I: 10},
		{Line: 42, Varname: "i", Kind: "Int", I: 4},
		{Line: 42, Varname: "n", Kind: "Int", I: 4},
		{Line: 43, Varname: "n", Kind: "Int", I: 4},
		// main: nested_val = nested_loop(4) = 10, final_result = 21
		{Line: 50, Varname: "sign", Kind: "Int", I: 1},
		{Line: 50, Varname: "xs", Kind: "Raw"},
		{Line: 50, Varname: "sum_val", Kind: "Int", I: 10},
		{Line: 50, Varname: "nested_val", Kind: "Int", I: 10},
		{Line: 51, Varname: "sign", Kind: "Int", I: 1},
		{Line: 51, Varname: "xs", Kind: "Raw"},
		{Line: 51, Varname: "sum_val", Kind: "Int", I: 10},
		{Line: 51, Varname: "nested_val", Kind: "Int", I: 10},
		{Line: 51, Varname: "final_result", Kind: "Int", I: 21},
	}
}

// TestRecorderGoldenNestedCalls records nested_calls.wasm and
// asserts on the call/return ordering for a 3-deep chain plus a
// recursive factorial.
func TestRecorderGoldenNestedCalls(t *testing.T) {
	doc, _ := recordAndDumpFull(t, wasmRecorderGoldenNestedCalls, "nested_calls")

	require.True(t, strings.HasSuffix(doc.Metadata.Program, "nested_calls.wasm"),
		"metadata.program; got %q", doc.Metadata.Program)

	// Function table — writer-assignment order, exactly five entries.
	require.Equal(t, []string{"main", "level1", "level2", "level3", "factorial"},
		doc.Functions, "function table")

	require.Equal(t, 1, len(doc.Paths))
	require.True(t, strings.HasSuffix(doc.Paths[0], "nested_calls.rs"))

	// Counts.
	require.Equal(t, 40, doc.Counts["steps"])
	require.Equal(t, 8, doc.Counts["calls"], "1 chain + 1 + 5 recursive = 7? "+
		"In practice the recorder also opens a call frame for `main`'s inline "+
		"frame switches, so 8 is the literal observed count")
	require.Equal(t, 0, doc.Counts["io_events"], "clean exit produces no io_events")
	require.Equal(t, 5, doc.Counts["functions"])
	require.Equal(t, 6, doc.Counts["varnames"])

	events := decodeEvents(t, doc)
	stepIndicesMonotonic(t, events)

	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.Equal(t, map[string]int{
		"step": 40, "call_entry": 8, "call_exit": 8,
	}, kinds, "event-kind histogram")

	// Call-entry sequence: level1 → level2 → level3 first (outermost
	// to innermost), then factorial five times (depth 1..5).
	want := []string{
		"level1", "level2", "level3",
		"factorial", "factorial", "factorial", "factorial", "factorial",
	}
	require.Equal(t, want, callSequence(events), "call_entry sequence")

	// Call-exit sequence: LIFO.  Returns:
	//   level3(5) = 6
	//   level2(5) = level3(5)*2 = 12
	//   level1(5) = level2(5)+10 = 22
	//   factorial(1)=1, factorial(2)=2, factorial(3)=6, factorial(4)=24,
	//   factorial(5)=120
	exits := callExitSequence(t, events)
	require.Equal(t, 8, len(exits))

	type fnRet struct {
		fn  string
		ret int64
	}
	wantExits := []fnRet{
		{"level3", 6}, {"level2", 12}, {"level1", 22},
		{"factorial", 1}, {"factorial", 2}, {"factorial", 6},
		{"factorial", 24}, {"factorial", 120},
	}
	for i, e := range exits {
		require.Equal(t, wantExits[i].fn, e.Function,
			"call_exit[%d].function", i)
		require.Equal(t, "Int", e.Return.Kind,
			"call_exit[%d].return_value.kind for %s", i, e.Function)
		require.Equal(t, wantExits[i].ret, *e.Return.I,
			"call_exit[%d] %s return value", i, e.Function)
	}

	// Step-by-step variable values (every step pinned).
	got := collectVars(t, events)
	want2 := []expectVarStep{
		{Line: 18, Varname: "x", Kind: "Int", I: 5},
		{Line: 19, Varname: "x", Kind: "Int", I: 5},
		{Line: 12, Varname: "x", Kind: "Int", I: 5},
		{Line: 13, Varname: "x", Kind: "Int", I: 5},
		{Line: 7, Varname: "x", Kind: "Int", I: 5},
		{Line: 8, Varname: "x", Kind: "Int", I: 5},
		{Line: 9, Varname: "x", Kind: "Int", I: 5},
		{Line: 14, Varname: "v", Kind: "Int", I: 6},
		{Line: 14, Varname: "x", Kind: "Int", I: 5},
		{Line: 15, Varname: "x", Kind: "Int", I: 5},
		{Line: 20, Varname: "v", Kind: "Int", I: 12},
		{Line: 20, Varname: "x", Kind: "Int", I: 5},
		{Line: 21, Varname: "x", Kind: "Int", I: 5},
		// main's `chain = level1(5) = 22`
		{Line: 34, Varname: "chain", Kind: "Int", I: 22},
		// factorial(5) recursion
		{Line: 24, Varname: "n", Kind: "Int", I: 5},
		{Line: 25, Varname: "n", Kind: "Int", I: 5},
		{Line: 28, Varname: "n", Kind: "Int", I: 5},
		{Line: 24, Varname: "n", Kind: "Int", I: 4},
		{Line: 25, Varname: "n", Kind: "Int", I: 4},
		{Line: 28, Varname: "n", Kind: "Int", I: 4},
		{Line: 24, Varname: "n", Kind: "Int", I: 3},
		{Line: 25, Varname: "n", Kind: "Int", I: 3},
		{Line: 28, Varname: "n", Kind: "Int", I: 3},
		{Line: 24, Varname: "n", Kind: "Int", I: 2},
		{Line: 25, Varname: "n", Kind: "Int", I: 2},
		{Line: 28, Varname: "n", Kind: "Int", I: 2},
		{Line: 24, Varname: "n", Kind: "Int", I: 1},
		{Line: 25, Varname: "n", Kind: "Int", I: 1},
		{Line: 26, Varname: "n", Kind: "Int", I: 1},
		// unwinding
		{Line: 25, Varname: "n", Kind: "Int", I: 1},
		{Line: 30, Varname: "n", Kind: "Int", I: 1},
		{Line: 25, Varname: "n", Kind: "Int", I: 2},
		{Line: 30, Varname: "n", Kind: "Int", I: 2},
		{Line: 25, Varname: "n", Kind: "Int", I: 3},
		{Line: 30, Varname: "n", Kind: "Int", I: 3},
		{Line: 25, Varname: "n", Kind: "Int", I: 4},
		{Line: 30, Varname: "n", Kind: "Int", I: 4},
		{Line: 25, Varname: "n", Kind: "Int", I: 5},
		{Line: 30, Varname: "n", Kind: "Int", I: 5},
		// main: fact5 = 120, total = 142
		{Line: 35, Varname: "chain", Kind: "Int", I: 22},
		{Line: 35, Varname: "fact5", Kind: "Int", I: 120},
		{Line: 36, Varname: "chain", Kind: "Int", I: 22},
		{Line: 36, Varname: "fact5", Kind: "Int", I: 120},
		{Line: 36, Varname: "total", Kind: "Int", I: 142},
	}
	requireOrderedVars(t, want2, got)
}

// TestRecorderGoldenCollections records collections.wasm and asserts
// on the Vec / HashMap / struct / tuple value-encoding behaviour.
//
// RECORDER BUG: as of 2026-05, the wasm recorder encodes the
// elements of a `Vec<i32>`, a `HashMap`, and a `(i32,i32)` tuple as
// `ValueRecord::Raw` rather than as `Sequence` / `Map` / `Tuple`.
// Struct-shaped values (`Point`, `HashMap` itself) decode as
// `Struct` with no fields exposed.  The literal observed shape is
// pinned below; if/when the recorder learns to surface real
// container variants, this table must be updated alongside the
// recorder change.
func TestRecorderGoldenCollections(t *testing.T) {
	doc, _ := recordAndDumpFull(t, wasmRecorderGoldenCollections, "collections")

	require.True(t, strings.HasSuffix(doc.Metadata.Program, "collections.wasm"),
		"metadata.program; got %q", doc.Metadata.Program)

	require.Equal(t, []string{"main", "make_vec", "sum_vec"}, doc.Functions,
		"function table")

	require.Equal(t, 1, len(doc.Paths))
	require.True(t, strings.HasSuffix(doc.Paths[0], "collections.rs"))

	require.Equal(t, 30, doc.Counts["steps"])
	require.Equal(t, 2, doc.Counts["calls"])
	require.Equal(t, 0, doc.Counts["io_events"], "clean exit produces no io_events")
	require.Equal(t, 3, doc.Counts["functions"])
	require.Equal(t, 12, doc.Counts["varnames"])

	events := decodeEvents(t, doc)
	stepIndicesMonotonic(t, events)

	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	require.Equal(t, map[string]int{
		"step": 30, "call_entry": 2, "call_exit": 2,
	}, kinds, "event-kind histogram")

	require.Equal(t, []string{"make_vec", "sum_vec"}, callSequence(events),
		"call_entry sequence")

	exits := callExitSequence(t, events)
	require.Equal(t, 2, len(exits))
	// RECORDER BUG: make_vec returns a Vec<i32>, but the recorder
	// surfaces the return value as Raw rather than as a Sequence.
	require.Equal(t, "make_vec", exits[0].Function)
	require.Equal(t, "Raw", exits[0].Return.Kind,
		"make_vec returns Vec<i32> — RECORDER BUG: surfaces as Raw, "+
			"not Sequence/Vec.  When the recorder learns to encode "+
			"Rust collections this must change.")
	require.Equal(t, "sum_vec", exits[1].Function)
	require.Equal(t, "Int", exits[1].Return.Kind)
	require.Equal(t, int64(60), *exits[1].Return.I,
		"sum_vec(&[10,20,30]) → 60")

	// Variable assertions (literal observed shape — RECORDER BUG
	// notes already attached).  `pair`, `v` (Vec) surface as Raw;
	// `map` and `pt` surface as Struct with no field payload.
	got := collectVars(t, events)
	want := []expectVarStep{
		// make_vec body
		{Line: 18, Varname: "v", Kind: "Raw"},
		{Line: 19, Varname: "v", Kind: "Raw"},
		{Line: 20, Varname: "v", Kind: "Raw"},
		{Line: 21, Varname: "v", Kind: "Raw"},
		// after make_vec returns, main has v
		{Line: 35, Varname: "v", Kind: "Raw"},
		// sum_vec body
		{Line: 25, Varname: "v", Kind: "Raw"},
		{Line: 26, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 0},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 0},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 10},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 10},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 30},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 30},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 60},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 30, Varname: "s", Kind: "Int", I: 60},
		{Line: 30, Varname: "v", Kind: "Raw"},
		{Line: 31, Varname: "v", Kind: "Raw"},
		// main after sum_vec
		{Line: 37, Varname: "v", Kind: "Raw"},
		{Line: 37, Varname: "total", Kind: "Int", I: 60},
		// HashMap setup
		{Line: 38, Varname: "v", Kind: "Raw"},
		{Line: 38, Varname: "total", Kind: "Int", I: 60},
		{Line: 38, Varname: "map", Kind: "Struct"},
		{Line: 39, Varname: "v", Kind: "Raw"},
		{Line: 39, Varname: "total", Kind: "Int", I: 60},
		{Line: 39, Varname: "map", Kind: "Struct"},
		{Line: 40, Varname: "v", Kind: "Raw"},
		{Line: 40, Varname: "total", Kind: "Int", I: 60},
		{Line: 40, Varname: "map", Kind: "Struct"},
		{Line: 42, Varname: "v", Kind: "Raw"},
		{Line: 42, Varname: "total", Kind: "Int", I: 60},
		{Line: 42, Varname: "map", Kind: "Struct"},
		{Line: 42, Varname: "map_len", Kind: "Int", I: 2},
		{Line: 43, Varname: "v", Kind: "Raw"},
		{Line: 43, Varname: "total", Kind: "Int", I: 60},
		{Line: 43, Varname: "map", Kind: "Struct"},
		{Line: 43, Varname: "map_len", Kind: "Int", I: 2},
		{Line: 43, Varname: "pt", Kind: "Struct"},
		{Line: 45, Varname: "v", Kind: "Raw"},
		{Line: 45, Varname: "total", Kind: "Int", I: 60},
		{Line: 45, Varname: "map", Kind: "Struct"},
		{Line: 45, Varname: "map_len", Kind: "Int", I: 2},
		{Line: 45, Varname: "pt", Kind: "Struct"},
		{Line: 45, Varname: "pt_sum", Kind: "Int", I: 7},
		{Line: 46, Varname: "v", Kind: "Raw"},
		{Line: 46, Varname: "total", Kind: "Int", I: 60},
		{Line: 46, Varname: "map", Kind: "Struct"},
		{Line: 46, Varname: "map_len", Kind: "Int", I: 2},
		{Line: 46, Varname: "pt", Kind: "Struct"},
		{Line: 46, Varname: "pt_sum", Kind: "Int", I: 7},
		{Line: 46, Varname: "pair", Kind: "Raw"},
		{Line: 48, Varname: "v", Kind: "Raw"},
		{Line: 48, Varname: "total", Kind: "Int", I: 60},
		{Line: 48, Varname: "map", Kind: "Struct"},
		{Line: 48, Varname: "map_len", Kind: "Int", I: 2},
		{Line: 48, Varname: "pt", Kind: "Struct"},
		{Line: 48, Varname: "pt_sum", Kind: "Int", I: 7},
		{Line: 48, Varname: "pair", Kind: "Raw"},
		{Line: 48, Varname: "pair_sum", Kind: "Int", I: 300},
		{Line: 49, Varname: "v", Kind: "Raw"},
		{Line: 49, Varname: "total", Kind: "Int", I: 60},
		{Line: 49, Varname: "map", Kind: "Struct"},
		{Line: 49, Varname: "map_len", Kind: "Int", I: 2},
		{Line: 49, Varname: "pt", Kind: "Struct"},
		{Line: 49, Varname: "pt_sum", Kind: "Int", I: 7},
		{Line: 49, Varname: "pair", Kind: "Raw"},
		{Line: 49, Varname: "pair_sum", Kind: "Int", I: 300},
		{Line: 49, Varname: "final_result", Kind: "Int", I: 369},
	}
	requireOrderedVars(t, want, got)
}

// TestRecorderGoldenPanicPath records panic_path.wasm and asserts
// that the recorder produces an `ioError` io_event (the wazero
// recorder's `RecordEvent::Error` mapping) when the Rust program
// panics.
//
// Two paired fixes (cmd/wazero/wazero.go + interpreter.go) make
// this test pass:
//
//	1. wazero.go's error-path handler now calls produceTrace on
//	   every termination (not just *sys.ExitError) so the .ct file
//	   actually lands on disk for panicking programs.
//	2. interpreter.go's recover() now records the actual panic
//	   value (e.g. "unreachable" for Rust panic→trap) as the
//	   ioError text and SKIPS the event for clean *sys.ExitError
//	   exits with code 0 (so normal-exit programs no longer carry
//	   a spurious "runtime error" io_event).
func TestRecorderGoldenPanicPath(t *testing.T) {
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "panic_path.wasm")
	require.NoError(t, os.WriteFile(wasmPath, wasmRecorderGoldenPanicPath, 0o700))

	outDir := filepath.Join(tmpDir, "traces")
	exitCode, _, _ := runMain(t, "",
		[]string{"run", "--out-dir=" + outDir, wasmPath})

	// The Rust panic translates to a wasm `unreachable` trap; the
	// wazero CLI returns 1.  This part of the contract is correct.
	require.Equal(t, 1, exitCode,
		"panic_path.wasm should surface a non-zero exit (panic→trap)")

	// The CLI now writes the trace on every termination path
	// (cmd/wazero/wazero.go), so a missing .ct file here is a
	// regression — fail hard rather than skipping.
	candidates, _ := filepath.Glob(filepath.Join(outDir, "*.ct"))
	require.True(t, len(candidates) > 0,
		"panicking program produced no .ct file in %s; "+
			"the wazero CLI must call produceTrace on every "+
			"termination path (regression of cmd/wazero/wazero.go fix)",
		outDir)

	// Per the recorder-test spec §2 ("Exceptions / errors"), the
	// recorder MUST emit a `RecordEvent::Error` (mapped to
	// `ioError` in the wazero recorder) when the program panics.
	cmd := exec.Command(ctPrint, "--full", "--strip-paths", candidates[0])
	out, err := cmd.CombinedOutput()
	require.NoError(t, err,
		"ct-print --full should succeed; output:\n%s", string(out))

	var doc goldenDoc
	require.NoError(t, json.Unmarshal(out, &doc),
		"ct-print --full should emit valid JSON; got:\n%s", string(out))

	require.True(t, strings.HasSuffix(doc.Metadata.Program, "panic_path.wasm"),
		"metadata.program; got %q", doc.Metadata.Program)
	require.True(t, strings.HasSuffix(doc.Paths[0], "panic_path.rs"),
		"paths[0]; got %q", doc.Paths[0])

	// `bump` must appear (called twice with arg 40 and 41 → returns
	// 41 and 42).  `main` must appear.
	hasMain, hasBump := false, false
	for _, fn := range doc.Functions {
		if strings.HasSuffix(fn, "main") {
			hasMain = true
		}
		if strings.HasSuffix(fn, "bump") {
			hasBump = true
		}
	}
	require.True(t, hasMain, "functions table must include main; got %v", doc.Functions)
	require.True(t, hasBump, "functions table must include bump; got %v", doc.Functions)

	events := decodeEvents(t, &doc)
	stepIndicesMonotonic(t, events)

	// At least one io event of error kind must reference the panic
	// site (panic_path.rs line 18).  The recorder's exact
	// payload-format choice (text="panicked at ...", text="runtime
	// error", etc.) is implementation-defined; the assertion target
	// is "an io_kind = ioError exists, anchored at a step on or
	// after the panic call".
	var errIos []goldenEvent
	for _, ev := range events {
		if ev.Kind == "io" && ev.IoKind == "ioError" {
			errIos = append(errIos, ev)
		}
	}
	require.True(t, len(errIos) >= 1,
		"expected at least one ioError event for the panic path; got %d "+
			"io events", len(errIos))

	// Last user-step before the panic should be on or after line 18
	// (the `panic!()` call site).  This pins the recorder's
	// step-emission contract on the panic path.
	lastStep := events[0]
	for _, ev := range events {
		if ev.Kind == "step" {
			lastStep = ev
		}
	}
	require.True(t, lastStep.Line >= 17,
		"last step before panic should be on or after line 17 (the if-test); "+
			"got line %d", lastStep.Line)
}

// ===========================================================================
// Test-runner sanity (parallel of the existing testdata fixtures)
// ===========================================================================

// TestRecorderGoldenFixturesEmbedded asserts every fixture is
// present and non-empty — a guard against a `go:embed` directive
// going stale (e.g. someone deletes a `.wasm` from the repo without
// updating the embed directives).
func TestRecorderGoldenFixturesEmbedded(t *testing.T) {
	for _, tc := range []struct {
		name string
		blob []byte
	}{
		{"control_flow.wasm", wasmRecorderGoldenControlFlow},
		{"nested_calls.wasm", wasmRecorderGoldenNestedCalls},
		{"collections.wasm", wasmRecorderGoldenCollections},
		{"panic_path.wasm", wasmRecorderGoldenPanicPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, len(tc.blob) > 0,
				"fixture %s must be embedded and non-empty", tc.name)
			// WASM magic: "\0asm".
			require.True(t, len(tc.blob) >= 4 &&
				tc.blob[0] == 0x00 && tc.blob[1] == 'a' &&
				tc.blob[2] == 's' && tc.blob[3] == 'm',
				"fixture %s lacks the WASM magic header", tc.name)
		})
	}
}

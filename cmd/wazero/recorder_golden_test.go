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
// compiled to wasm (debug build, DWARF preserved) by the test run
// itself — see `recorder_golden_fixtures_test.go`, which owns the
// toolchain invocation and the once-per-binary build cache.  The
// `.wasm` are deliberately NOT checked in: a pre-built binary beside
// its source is an unverifiable claim that the two match, and this
// repo's dev shell now pins the `wasm32-wasip1` toolchain that
// settles it.
//
// Recorder bugs uncovered by writing these tests are documented as
// `// RECORDER BUG: ...` comments + `t.Skip(...)` calls per the
// task spec — assertions are NEVER weakened to mask them.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

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
		// Flags surfaces the meta.dat flag bits the Nim reader exposes
		// under `metadata.flags`.  `has_column_aware_steps` is bit 4 and
		// gates column-aware navigation; the wasm recorder sets it on
		// every trace per FU-Column-Aware-Nav-Wasm.
		//
		// It is a MAP rather than a struct of named booleans, and that is
		// deliberate.  `codetracer_ct_print.nim` emits nine keys today
		// (`has_column_aware_steps`, `has_alternate_source_views`,
		// `supports_column_breakpoints`, `supports_column_motions`,
		// `has_call_stream`, `has_step_stream`, `has_value_stream`,
		// `has_io_event_stream`, `has_interning_tables`) and this struct
		// declared two of them — so `encoding/json` silently dropped the
		// other seven and every comparison of `Metadata.Flags` was a
		// comparison of two ninths of the flag word.  A map cannot drift
		// that way: whatever the reader gains a flag for lands here and is
		// compared, without anyone having to remember to widen a struct.
		//
		// The price of a map is that a mistyped key reads as `false`
		// instead of failing to compile.  `goldenFlags.get` buys that back
		// by failing the test when the key is absent from the document.
		Flags goldenFlags `json:"flags"`
	} `json:"metadata"`
	Paths     []string `json:"paths"`
	Functions []string `json:"functions"`
	Varnames  []string `json:"varnames"`
	// Types is the trace's type table, in the writer's assignment order:
	// the resolved DWARF type name behind every `type_id` an event refers
	// to (`["u32", "struct Slot", "[3]struct Slot", …]`).
	//
	// Comparing it MATTERS and comparing `counts["types"]` instead does
	// not.  The recorder's known failure mode here is count-preserving by
	// construction: swapping one recorder for another on a live
	// `ModuleInstance` without carrying its `TypesIndex` over produced
	// slices "declaring `i64` where linear replay declared `u32`, every
	// other stream byte-identical" (see AGENTS.md, "Two traps" under
	// Slices).  Same number of types, different types — invisible to a
	// count, and the difference between a debugger that renders a value
	// correctly and one that renders it as a wrong-width integer.
	Types  []string          `json:"types"`
	Counts map[string]int    `json:"counts"`
	Events []json.RawMessage `json:"events"`
}

// goldenFlags is the decoded `metadata.flags` object: every meta.dat flag
// bit `ct-print --full` surfaces, under the reader's own spelling.
//
// Decoding into a map rather than into named fields is what makes the flag
// comparison total — see the comment on `goldenDoc.Metadata.Flags`.
type goldenFlags map[string]bool

// get returns the named flag, failing loudly when the document carries no
// such key.
//
// This is the guard that makes the map as safe as the named-field struct
// it replaced: a typo, or a flag the reader stopped emitting, surfaces as
// an explicit "no such flag" failure rather than as a silent `false` that
// happens to satisfy a `require.False`.
func (f goldenFlags) get(t *testing.T, name string) bool {
	t.Helper()
	v, ok := f[name]
	require.True(t, ok,
		"`ct-print --full` emitted no `metadata.flags.%s`; the reader's flag "+
			"set has changed and this assertion is naming a flag that no "+
			"longer exists.  flags present: %v", name, f)
	return v
}

// goldenEvent carries every field surfaced by `--full`'s event
// stream.  Unknown event kinds fail the test so the assertion layer
// can never silently skip a new variant.
type goldenEvent struct {
	Kind      string `json:"kind"`
	StepIndex int64  `json:"step_index"`
	Line      int64  `json:"line"`
	// Column is the 1-based source column the step landed on.  Surfaced
	// by the column-aware reader on traces with bit 4 of `meta.dat`
	// set; legacy line-only traces leave it zero.  See
	// FU-Column-Aware-Nav-Wasm.
	Column   int64  `json:"column"`
	Path     string `json:"path"`
	PathID   int64  `json:"path_id"`
	Function string `json:"function"`
	Depth    int64  `json:"depth"`
	StepKind string `json:"step_kind"`
	Vars     []struct {
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

// recordAndDumpFull compiles the named fixture (once per test binary;
// see `recorder_golden_fixtures_test.go`), runs the wazero binary's
// record path against it, then pipes the resulting `.ct` container
// through `ct-print --full --strip-paths`.  Returns the decoded
// document plus the absolute path of the wasm fixture (for path /
// metadata assertions).
//
// The function calls `t.Skip` (with a clear `SKIP:` diagnostic, per
// `verify-cli-convention-no-silent-skip.sh`) when `ct-print` is
// missing — the only audit-approved skip condition for golden tests.
// A missing Rust toolchain is NOT such a condition: it fails.  Every
// other failure mode fails loudly so a regression in the recorder,
// the writer, or `ct-print` itself surfaces immediately.
func recordAndDumpFull(t *testing.T, basename string) (*goldenDoc, string) {
	t.Helper()

	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	noteGoldenToolchain(t)
	fixture := goldenFixture(t, basename)

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
	case "Reference", "Struct", "None", "Raw", "Sequence", "Tuple", "Void":
		// Compound / sentinel kinds — the kind itself is the
		// assertion target.  Callers that need scalar payloads must
		// extend this switch.
		//
		// "Void" surfaces from the writer's close-time PendingCall
		// drain (`@[VoidReturnMarker]`).  The wasm recorder leaves
		// `main` open at end-of-execution; the Nim trace writer's
		// `close()` flushes the unclosed frame with VoidReturnMarker
		// as its return value so `counts.calls` matches
		// `call_entry` / `call_exit` counts.  See
		// codetracer-trace-format-nim/src/codetracer_trace_writer/multi_stream_writer.nim::close.
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
	doc, wasmPath := recordAndDumpFull(t, "control_flow")

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
	// Re-pinned on 2026-07-31 (M40).  Two changes moved these numbers,
	// neither of them a change in what the recorder observes:
	//
	//  1. codetracer-trace-format-nim 8a6ee4d folded `register_step` +
	//     `register_delta_column` into ONE wire event.  Before it, every
	//     column-bearing step produced two (an empty line step followed by
	//     a `sekDeltaColumn` carrying the values), which is why `counts.steps`
	//     and the `events[]` step histogram used to disagree; ct-print
	//     94f774b then stopped rendering the nudges in `events[]` at all.
	//     The event histogram therefore drops to what `counts.steps`
	//     always said, and the last step of each frame picks up the
	//     locals that used to be stranded on the empty half of the pair.
	//  2. The recorder stopped emitting columns the wire encoding cannot
	//     address — see `addressableColumn` in
	//     internal/engine/interpreter/interpreter.go.  These fixtures'
	//     sources are not on disk at the path their DWARF names, so they
	//     carry no per-line table and now record no columns at all,
	//     instead of columns that decoded into out-of-range line numbers.
	require.Equal(t, 142, doc.Counts["steps"], "counts.steps")
	// `main` now surfaces as a 4th completed call.  The Nim trace
	// writer's `close()` drains any unclosed PendingCalls (LIFO) so
	// partial-trace recordings still produce balanced
	// call_entry/call_exit pairs.  The wasm recorder leaves the
	// outermost `main` frame open at end-of-execution (no DWARF
	// `RegisterReturn` is synthesised for module exit), so before
	// the writer fix `close()` silently dropped it and counts.calls
	// stopped at 3.  See
	// codetracer-trace-format-nim/src/codetracer_trace_writer/multi_stream_writer.nim::close.
	require.Equal(t, 4, doc.Counts["calls"], "counts.calls")
	require.Equal(t, 0, doc.Counts["io_events"], "counts.io_events")
	require.Equal(t, 142, doc.Counts["values"], "counts.values")
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
	// 4 call_entry + 4 call_exit = `main` now appears in both
	// streams thanks to the writer's close-time PendingCall drain.
	require.Equal(t, map[string]int{
		"step": 142, "call_entry": 4, "call_exit": 4,
	}, kinds, "event-kind histogram")
	require.Equal(t, 150, len(events), "events length")

	// ----- call entry sequence -------------------------------------------
	// `main` is now the first entry — it is opened at module entry
	// and flushed at `close()` by the writer's PendingCall drain.
	require.Equal(t, []string{"main", "classify", "sum_iter", "nested_loop"},
		callSequence(events), "call_entry sequence")

	// ----- call exit return values (each must decode as Int) -------------
	// `main` is the deepest (latest to exit) frame and so appears
	// last in the LIFO exit sequence.  Its return value is the
	// synthetic VoidReturnMarker that the writer's close-time drain
	// emits in lieu of an explicit return value.
	exits := callExitSequence(t, events)
	require.Equal(t, 4, len(exits), "call_exit count")

	require.Equal(t, "classify", exits[0].Function)
	require.Equal(t, "Int", exits[0].Return.Kind)
	require.Equal(t, int64(1), *exits[0].Return.I, "classify(7) → 1")

	require.Equal(t, "sum_iter", exits[1].Function)
	require.Equal(t, "Int", exits[1].Return.Kind)
	require.Equal(t, int64(10), *exits[1].Return.I, "sum_iter([1,2,3,4]) → 10")

	require.Equal(t, "nested_loop", exits[2].Function)
	require.Equal(t, "Int", exits[2].Return.Kind)
	require.Equal(t, int64(10), *exits[2].Return.I, "nested_loop(4) → 10")

	require.Equal(t, "main", exits[3].Function, "main exits last (deepest frame)")

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
	// Re-pinned on 2026-07-31 (M40).  Two changes moved these numbers,
	// neither of them a change in what the recorder observes:
	//
	//  1. codetracer-trace-format-nim 8a6ee4d folded `register_step` +
	//     `register_delta_column` into ONE wire event.  Before it, every
	//     column-bearing step produced two (an empty line step followed by
	//     a `sekDeltaColumn` carrying the values), which is why `counts.steps`
	//     and the `events[]` step histogram used to disagree; ct-print
	//     94f774b then stopped rendering the nudges in `events[]` at all.
	//     The event histogram therefore drops to what `counts.steps`
	//     always said, and the last step of each frame picks up the
	//     locals that used to be stranded on the empty half of the pair.
	//  2. The recorder stopped emitting columns the wire encoding cannot
	//     address — see `addressableColumn` in
	//     internal/engine/interpreter/interpreter.go.  These fixtures'
	//     sources are not on disk at the path their DWARF names, so they
	//     carry no per-line table and now record no columns at all,
	//     instead of columns that decoded into out-of-range line numbers.
	return []expectVarStep{
		{Line: 11, Varname: "n", Kind: "Int", I: 7},
		{Line: 12, Varname: "n", Kind: "Int", I: 7},
		{Line: 13, Varname: "n", Kind: "Int", I: 7},
		{Line: 12, Varname: "n", Kind: "Int", I: 7},
		{Line: 19, Varname: "n", Kind: "Int", I: 7},
		{Line: 47, Varname: "sign", Kind: "Int", I: 1},
		{Line: 48, Varname: "sign", Kind: "Int", I: 1},
		{Line: 48, Varname: "xs", Kind: "Raw"},
		{Line: 22, Varname: "xs", Kind: "Raw"},
		{Line: 23, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 0},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 0},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 0},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 0},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 0},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 1},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 1},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 1},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 1},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 1},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 3},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 3},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 3},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 3},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 3},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 6},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 6},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 6},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 6},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 25, Varname: "total", Kind: "Int", I: 6},
		{Line: 25, Varname: "iter", Kind: "Struct"},
		{Line: 25, Varname: "x", Kind: "Raw"},
		{Line: 25, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 10},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 24, Varname: "total", Kind: "Int", I: 10},
		{Line: 24, Varname: "iter", Kind: "Struct"},
		{Line: 24, Varname: "xs", Kind: "Raw"},
		{Line: 27, Varname: "total", Kind: "Int", I: 10},
		{Line: 27, Varname: "xs", Kind: "Raw"},
		{Line: 28, Varname: "xs", Kind: "Raw"},
		{Line: 49, Varname: "sign", Kind: "Int", I: 1},
		{Line: 49, Varname: "xs", Kind: "Raw"},
		{Line: 49, Varname: "sum_val", Kind: "Int", I: 10},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 0},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 0},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 0},
		{Line: 38, Varname: "i", Kind: "Int", I: 0},
		{Line: 38, Varname: "j", Kind: "Int", I: 0},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 0},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 0},
		{Line: 40, Varname: "i", Kind: "Int", I: 0},
		{Line: 40, Varname: "j", Kind: "Int", I: 1},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		{Line: 34, Varname: "total", Kind: "Int", I: 0},
		{Line: 34, Varname: "i", Kind: "Int", I: 1},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 1},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 1},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 0},
		{Line: 38, Varname: "i", Kind: "Int", I: 1},
		{Line: 38, Varname: "j", Kind: "Int", I: 0},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 0},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 1},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 0},
		{Line: 37, Varname: "i", Kind: "Int", I: 1},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
		{Line: 38, Varname: "total", Kind: "Int", I: 1},
		{Line: 38, Varname: "i", Kind: "Int", I: 1},
		{Line: 38, Varname: "j", Kind: "Int", I: 1},
		{Line: 38, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 1},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 1},
		{Line: 40, Varname: "i", Kind: "Int", I: 1},
		{Line: 40, Varname: "j", Kind: "Int", I: 2},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		{Line: 34, Varname: "total", Kind: "Int", I: 1},
		{Line: 34, Varname: "i", Kind: "Int", I: 2},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 1},
		{Line: 37, Varname: "i", Kind: "Int", I: 2},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 1},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 1},
		{Line: 37, Varname: "i", Kind: "Int", I: 2},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 2},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 2},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 2},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 2},
		{Line: 37, Varname: "i", Kind: "Int", I: 2},
		{Line: 37, Varname: "j", Kind: "Int", I: 2},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 2},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 4},
		{Line: 40, Varname: "i", Kind: "Int", I: 2},
		{Line: 40, Varname: "j", Kind: "Int", I: 3},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		{Line: 34, Varname: "total", Kind: "Int", I: 4},
		{Line: 34, Varname: "i", Kind: "Int", I: 3},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 0},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 4},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 0},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 4},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 1},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 4},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 1},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 5},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 5},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 5},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 2},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 5},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 2},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 7},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 7},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 7},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 3},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 37, Varname: "total", Kind: "Int", I: 7},
		{Line: 37, Varname: "i", Kind: "Int", I: 3},
		{Line: 37, Varname: "j", Kind: "Int", I: 3},
		{Line: 37, Varname: "n", Kind: "Int", I: 4},
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
		{Line: 36, Varname: "total", Kind: "Int", I: 10},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 4},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 10},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 4},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 36, Varname: "total", Kind: "Int", I: 10},
		{Line: 36, Varname: "i", Kind: "Int", I: 3},
		{Line: 36, Varname: "j", Kind: "Int", I: 4},
		{Line: 36, Varname: "n", Kind: "Int", I: 4},
		{Line: 40, Varname: "total", Kind: "Int", I: 10},
		{Line: 40, Varname: "i", Kind: "Int", I: 3},
		{Line: 40, Varname: "j", Kind: "Int", I: 4},
		{Line: 40, Varname: "n", Kind: "Int", I: 4},
		{Line: 34, Varname: "total", Kind: "Int", I: 10},
		{Line: 34, Varname: "i", Kind: "Int", I: 4},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 34, Varname: "total", Kind: "Int", I: 10},
		{Line: 34, Varname: "i", Kind: "Int", I: 4},
		{Line: 34, Varname: "n", Kind: "Int", I: 4},
		{Line: 42, Varname: "total", Kind: "Int", I: 10},
		{Line: 42, Varname: "i", Kind: "Int", I: 4},
		{Line: 42, Varname: "n", Kind: "Int", I: 4},
		{Line: 43, Varname: "n", Kind: "Int", I: 4},
		{Line: 50, Varname: "sign", Kind: "Int", I: 1},
		{Line: 50, Varname: "xs", Kind: "Raw"},
		{Line: 50, Varname: "sum_val", Kind: "Int", I: 10},
		{Line: 50, Varname: "nested_val", Kind: "Int", I: 10},
		{Line: 51, Varname: "sign", Kind: "Int", I: 1},
		{Line: 51, Varname: "xs", Kind: "Raw"},
		{Line: 51, Varname: "sum_val", Kind: "Int", I: 10},
		{Line: 51, Varname: "nested_val", Kind: "Int", I: 10},
		{Line: 51, Varname: "final_result", Kind: "Int", I: 21},
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
	doc, _ := recordAndDumpFull(t, "nested_calls")

	require.True(t, strings.HasSuffix(doc.Metadata.Program, "nested_calls.wasm"),
		"metadata.program; got %q", doc.Metadata.Program)

	// Function table — writer-assignment order, exactly five entries.
	require.Equal(t, []string{"main", "level1", "level2", "level3", "factorial"},
		doc.Functions, "function table")

	require.Equal(t, 1, len(doc.Paths))
	require.True(t, strings.HasSuffix(doc.Paths[0], "nested_calls.rs"))

	// Counts.
	// Re-pinned on 2026-07-31 (M40).  Two changes moved these numbers,
	// neither of them a change in what the recorder observes:
	//
	//  1. codetracer-trace-format-nim 8a6ee4d folded `register_step` +
	//     `register_delta_column` into ONE wire event.  Before it, every
	//     column-bearing step produced two (an empty line step followed by
	//     a `sekDeltaColumn` carrying the values), which is why `counts.steps`
	//     and the `events[]` step histogram used to disagree; ct-print
	//     94f774b then stopped rendering the nudges in `events[]` at all.
	//     The event histogram therefore drops to what `counts.steps`
	//     always said, and the last step of each frame picks up the
	//     locals that used to be stranded on the empty half of the pair.
	//  2. The recorder stopped emitting columns the wire encoding cannot
	//     address — see `addressableColumn` in
	//     internal/engine/interpreter/interpreter.go.  These fixtures'
	//     sources are not on disk at the path their DWARF names, so they
	//     carry no per-line table and now record no columns at all,
	//     instead of columns that decoded into out-of-range line numbers.
	require.Equal(t, 49, doc.Counts["steps"])
	// 8 = 3 chain (level1/2/3) + 5 recursive factorial frames.  The
	// 9th is `main` itself — the wasm recorder leaves the outermost
	// `main` frame open at end-of-execution (no DWARF
	// `RegisterReturn` is synthesised for module exit), so before
	// the Nim trace writer's close-time PendingCall drain landed,
	// `close()` silently dropped that frame and `counts.calls`
	// stopped at 8.  The writer now flushes the unclosed `main`, so
	// the count is 9 and the call_entry/call_exit streams pick up
	// the outer frame as well.  See
	// codetracer-trace-format-nim/src/codetracer_trace_writer/multi_stream_writer.nim::close.
	require.Equal(t, 9, doc.Counts["calls"], "3 chain + 5 recursive + 1 outer "+
		"`main` flushed by writer's close-time PendingCall drain = 9")
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
		"step": 49, "call_entry": 9, "call_exit": 9,
	}, kinds, "event-kind histogram")

	// Call-entry sequence: `main` is opened first (at module
	// entry), then the level1→level2→level3 chain (outermost to
	// innermost), then factorial five times (depth 1..5).
	want := []string{
		"main",
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
	require.Equal(t, 9, len(exits))

	type fnRet struct {
		fn   string
		ret  int64
		kind string // "Int" for natural returns; "Void" for the close-time drain
	}
	wantExits := []fnRet{
		{"level3", 6, "Int"}, {"level2", 12, "Int"}, {"level1", 22, "Int"},
		{"factorial", 1, "Int"}, {"factorial", 2, "Int"}, {"factorial", 6, "Int"},
		{"factorial", 24, "Int"}, {"factorial", 120, "Int"},
		// `main` is the deepest (latest to exit) frame and so
		// appears last.  Its return value is the synthetic
		// VoidReturnMarker the writer's close-time PendingCall
		// drain emits in lieu of an explicit return value.
		{"main", 0, "Void"},
	}
	for i, e := range exits {
		require.Equal(t, wantExits[i].fn, e.Function,
			"call_exit[%d].function", i)
		require.Equal(t, wantExits[i].kind, e.Return.Kind,
			"call_exit[%d].return_value.kind for %s", i, e.Function)
		if wantExits[i].kind == "Int" {
			require.Equal(t, wantExits[i].ret, *e.Return.I,
				"call_exit[%d] %s return value", i, e.Function)
		}
	}

	// Step-by-step variable values (every step pinned).
	got := collectVars(t, events)
	// Re-pinned on 2026-07-31 (M40).  Two changes moved these numbers,
	// neither of them a change in what the recorder observes:
	//
	//  1. codetracer-trace-format-nim 8a6ee4d folded `register_step` +
	//     `register_delta_column` into ONE wire event.  Before it, every
	//     column-bearing step produced two (an empty line step followed by
	//     a `sekDeltaColumn` carrying the values), which is why `counts.steps`
	//     and the `events[]` step histogram used to disagree; ct-print
	//     94f774b then stopped rendering the nudges in `events[]` at all.
	//     The event histogram therefore drops to what `counts.steps`
	//     always said, and the last step of each frame picks up the
	//     locals that used to be stranded on the empty half of the pair.
	//  2. The recorder stopped emitting columns the wire encoding cannot
	//     address — see `addressableColumn` in
	//     internal/engine/interpreter/interpreter.go.  These fixtures'
	//     sources are not on disk at the path their DWARF names, so they
	//     carry no per-line table and now record no columns at all,
	//     instead of columns that decoded into out-of-range line numbers.
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
		{Line: 34, Varname: "chain", Kind: "Int", I: 22},
		{Line: 24, Varname: "n", Kind: "Int", I: 5},
		{Line: 25, Varname: "n", Kind: "Int", I: 5},
		{Line: 28, Varname: "n", Kind: "Int", I: 5},
		{Line: 28, Varname: "n", Kind: "Int", I: 5},
		{Line: 24, Varname: "n", Kind: "Int", I: 4},
		{Line: 25, Varname: "n", Kind: "Int", I: 4},
		{Line: 28, Varname: "n", Kind: "Int", I: 4},
		{Line: 28, Varname: "n", Kind: "Int", I: 4},
		{Line: 24, Varname: "n", Kind: "Int", I: 3},
		{Line: 25, Varname: "n", Kind: "Int", I: 3},
		{Line: 28, Varname: "n", Kind: "Int", I: 3},
		{Line: 28, Varname: "n", Kind: "Int", I: 3},
		{Line: 24, Varname: "n", Kind: "Int", I: 2},
		{Line: 25, Varname: "n", Kind: "Int", I: 2},
		{Line: 28, Varname: "n", Kind: "Int", I: 2},
		{Line: 28, Varname: "n", Kind: "Int", I: 2},
		{Line: 24, Varname: "n", Kind: "Int", I: 1},
		{Line: 25, Varname: "n", Kind: "Int", I: 1},
		{Line: 26, Varname: "n", Kind: "Int", I: 1},
		{Line: 25, Varname: "n", Kind: "Int", I: 1},
		{Line: 30, Varname: "n", Kind: "Int", I: 1},
		{Line: 28, Varname: "n", Kind: "Int", I: 2},
		{Line: 25, Varname: "n", Kind: "Int", I: 2},
		{Line: 30, Varname: "n", Kind: "Int", I: 2},
		{Line: 28, Varname: "n", Kind: "Int", I: 3},
		{Line: 25, Varname: "n", Kind: "Int", I: 3},
		{Line: 30, Varname: "n", Kind: "Int", I: 3},
		{Line: 28, Varname: "n", Kind: "Int", I: 4},
		{Line: 25, Varname: "n", Kind: "Int", I: 4},
		{Line: 30, Varname: "n", Kind: "Int", I: 4},
		{Line: 28, Varname: "n", Kind: "Int", I: 5},
		{Line: 25, Varname: "n", Kind: "Int", I: 5},
		{Line: 30, Varname: "n", Kind: "Int", I: 5},
		{Line: 35, Varname: "chain", Kind: "Int", I: 22},
		{Line: 35, Varname: "fact5", Kind: "Int", I: 120},
		{Line: 36, Varname: "chain", Kind: "Int", I: 22},
		{Line: 36, Varname: "fact5", Kind: "Int", I: 120},
		{Line: 36, Varname: "total", Kind: "Int", I: 142},
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
	doc, _ := recordAndDumpFull(t, "collections")

	require.True(t, strings.HasSuffix(doc.Metadata.Program, "collections.wasm"),
		"metadata.program; got %q", doc.Metadata.Program)

	require.Equal(t, []string{"main", "make_vec", "sum_vec"}, doc.Functions,
		"function table")

	require.Equal(t, 1, len(doc.Paths))
	require.True(t, strings.HasSuffix(doc.Paths[0], "collections.rs"))

	// Re-pinned on 2026-07-31 (M40).  Two changes moved these numbers,
	// neither of them a change in what the recorder observes:
	//
	//  1. codetracer-trace-format-nim 8a6ee4d folded `register_step` +
	//     `register_delta_column` into ONE wire event.  Before it, every
	//     column-bearing step produced two (an empty line step followed by
	//     a `sekDeltaColumn` carrying the values), which is why `counts.steps`
	//     and the `events[]` step histogram used to disagree; ct-print
	//     94f774b then stopped rendering the nudges in `events[]` at all.
	//     The event histogram therefore drops to what `counts.steps`
	//     always said, and the last step of each frame picks up the
	//     locals that used to be stranded on the empty half of the pair.
	//  2. The recorder stopped emitting columns the wire encoding cannot
	//     address — see `addressableColumn` in
	//     internal/engine/interpreter/interpreter.go.  These fixtures'
	//     sources are not on disk at the path their DWARF names, so they
	//     carry no per-line table and now record no columns at all,
	//     instead of columns that decoded into out-of-range line numbers.
	require.Equal(t, 43, doc.Counts["steps"])
	// The 3rd call is `main` — the wasm recorder leaves the
	// outermost `main` frame open at end-of-execution and the Nim
	// trace writer's `close()` now flushes any unclosed
	// PendingCalls (LIFO) so partial-trace recordings still
	// produce balanced call_entry/call_exit pairs.  See
	// codetracer-trace-format-nim/src/codetracer_trace_writer/multi_stream_writer.nim::close.
	require.Equal(t, 3, doc.Counts["calls"])
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
		"step": 43, "call_entry": 3, "call_exit": 3,
	}, kinds, "event-kind histogram")

	// `main` is opened first at module entry; the writer flushes
	// it at close().  make_vec / sum_vec appear in source order.
	require.Equal(t, []string{"main", "make_vec", "sum_vec"}, callSequence(events),
		"call_entry sequence")

	exits := callExitSequence(t, events)
	require.Equal(t, 3, len(exits))
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
	// `main` is the deepest frame and exits last.  The
	// VoidReturnMarker is the writer's close-time drain emitting
	// the synthetic return for an unclosed frame.
	require.Equal(t, "main", exits[2].Function, "main exits last (deepest frame)")
	require.Equal(t, "Void", exits[2].Return.Kind,
		"main's return surfaces as VoidReturnMarker — the writer's "+
			"close-time PendingCall drain emits Void when no explicit "+
			"return value was registered before close()")

	// Variable assertions (literal observed shape — RECORDER BUG
	// notes already attached).  `pair`, `v` (Vec) surface as Raw;
	// `map` and `pt` surface as Struct with no field payload.
	//
	// Re-pinned on 2026-07-31 (M40).  Two changes moved these numbers,
	// neither of them a change in what the recorder observes:
	//
	//  1. codetracer-trace-format-nim 8a6ee4d folded `register_step` +
	//     `register_delta_column` into ONE wire event.  Before it, every
	//     column-bearing step produced two (an empty line step followed by
	//     a `sekDeltaColumn` carrying the values), which is why `counts.steps`
	//     and the `events[]` step histogram used to disagree; ct-print
	//     94f774b then stopped rendering the nudges in `events[]` at all.
	//     The event histogram therefore drops to what `counts.steps`
	//     always said, and the last step of each frame picks up the
	//     locals that used to be stranded on the empty half of the pair.
	//  2. The recorder stopped emitting columns the wire encoding cannot
	//     address — see `addressableColumn` in
	//     internal/engine/interpreter/interpreter.go.  These fixtures'
	//     sources are not on disk at the path their DWARF names, so they
	//     carry no per-line table and now record no columns at all,
	//     instead of columns that decoded into out-of-range line numbers.
	got := collectVars(t, events)
	want := []expectVarStep{
		{Line: 18, Varname: "v", Kind: "Raw"},
		{Line: 19, Varname: "v", Kind: "Raw"},
		{Line: 20, Varname: "v", Kind: "Raw"},
		{Line: 21, Varname: "v", Kind: "Raw"},
		{Line: 35, Varname: "v", Kind: "Raw"},
		{Line: 25, Varname: "v", Kind: "Raw"},
		{Line: 26, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 0},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 0},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 0},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 0},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 0},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 0},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 10},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 10},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 10},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 10},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 10},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 30},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 30},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 30},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 30},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 28, Varname: "s", Kind: "Int", I: 30},
		{Line: 28, Varname: "iter", Kind: "Struct"},
		{Line: 28, Varname: "x", Kind: "Raw"},
		{Line: 28, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 60},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 27, Varname: "s", Kind: "Int", I: 60},
		{Line: 27, Varname: "iter", Kind: "Struct"},
		{Line: 27, Varname: "v", Kind: "Raw"},
		{Line: 30, Varname: "s", Kind: "Int", I: 60},
		{Line: 30, Varname: "v", Kind: "Raw"},
		{Line: 31, Varname: "v", Kind: "Raw"},
		{Line: 37, Varname: "v", Kind: "Raw"},
		{Line: 37, Varname: "total", Kind: "Int", I: 60},
		{Line: 38, Varname: "v", Kind: "Raw"},
		{Line: 38, Varname: "total", Kind: "Int", I: 60},
		{Line: 38, Varname: "map", Kind: "Struct"},
		{Line: 39, Varname: "v", Kind: "Raw"},
		{Line: 39, Varname: "total", Kind: "Int", I: 60},
		{Line: 39, Varname: "map", Kind: "Struct"},
		{Line: 40, Varname: "v", Kind: "Raw"},
		{Line: 40, Varname: "total", Kind: "Int", I: 60},
		{Line: 40, Varname: "map", Kind: "Struct"},
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
//  1. wazero.go's error-path handler now calls produceTrace on
//     every termination (not just *sys.ExitError) so the .ct file
//     actually lands on disk for panicking programs.
//  2. interpreter.go's recover() now records the actual panic
//     value (e.g. "unreachable" for Rust panic→trap) as the
//     ioError text and SKIPS the event for clean *sys.ExitError
//     exits with code 0 (so normal-exit programs no longer carry
//     a spurious "runtime error" io_event).
func TestRecorderGoldenPanicPath(t *testing.T) {
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	noteGoldenToolchain(t)

	tmpDir := t.TempDir()
	wasmPath := filepath.Join(tmpDir, "panic_path.wasm")
	require.NoError(t, os.WriteFile(wasmPath, goldenFixture(t, "panic_path"), 0o700))

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
// Column-aware step emission (FU-Column-Aware-Nav-Wasm)
// ===========================================================================

// TestRecorderColumnAwareSteps verifies the wasm recorder cooperates
// with the column-aware step stream introduced in the trace format's
// P6.3 / P6.4 milestones.  The acceptance criterion has three parts:
//
//  1. `meta.dat` bit 4 (`FLAG_HAS_COLUMN_AWARE_STEPS`) MUST be set on
//     every produced `.ct` container so downstream readers know to
//     surface columns to the user.  Verified through `ct-print --full`'s
//     `metadata.flags.has_column_aware_steps`.
//
//  2. At least one step event must carry a column ≥ 2 — proof the
//     recorder is threading real DWARF column data through, not just
//     flipping the metadata flag while emitting line-only steps.
//
//  3. At least one COLUMN-ONLY transition surfaces: two consecutive
//     steps on the same path and the same line whose columns differ.
//     That is the behaviour column-aware navigation exists for — a
//     multi-statement source line becoming several navigable steps —
//     and it cannot be produced by a recorder that drops columns.
//
// **This test used to assert (3) by counting `step_kind ==
// "sekDeltaColumn"` events, and that evidence no longer exists.**  Two
// deliberate changes in codetracer-trace-format-nim removed it:
// `8a6ee4d` made the FFI writer fold `register_step` +
// `register_delta_column` into a single wire event (the pair used to
// emit an empty line step plus a `sekDeltaColumn` carrying the values,
// which stranded locals on the empty half and broke line-granular
// step-over), and `94f774b` made `ct-print --full` stop rendering
// column nudges in `events[]` so its step entries match `counts.steps`.
// A standalone `sekDeltaColumn` is therefore unreachable through this
// path by design.  The `column` field is the stronger replacement: it
// is the decoded position a user would navigate to, not an encoding
// detail, and asserting a same-line column *change* is strictly more
// than "one nudge event exists" was.
//
// The fixture moved from `nested_calls` to `column_aware` for the same
// reason its sibling test uses it: `column_aware.rs` line 17 packs
// three statements onto one line, which is what makes a column-only
// transition observable at all.  Note the shared precondition — the
// reader can only decode a column when the module's source is on disk
// at the path its DWARF names, because the column axis is a byte offset
// resolved through `paths.dat` Layout A.  See
// TestRecorderColumnAwareMultipleStatementsPerLine, which carries the
// same dependency.
//
// Mirrors the JS recorder's `tests/integration/column-aware.test.ts`,
// the EVM recorder's `tests/test_column_aware.rs`, the Solana recorder's
// `tests/test_column_aware_steps.rs`, and the Cairo recorder's
// `tests/test_column_aware.rs` "multiple statements on one line each
// record distinct columns" fixtures.
func TestRecorderColumnAwareSteps(t *testing.T) {
	doc, _ := recordAndDumpFull(t, "column_aware")

	// ----- meta.dat bit 4: FLAG_HAS_COLUMN_AWARE_STEPS ------------------
	require.True(t, doc.Metadata.Flags.get(t, "has_column_aware_steps"),
		"trace metadata must advertise has_column_aware_steps=true; got %+v",
		doc.Metadata.Flags)

	events := decodeEvents(t, doc)

	// ----- (2) a real, non-default column reaches the reader ------------
	var maxColumn int64
	for _, ev := range events {
		if ev.Kind == "step" && ev.Column > maxColumn {
			maxColumn = ev.Column
		}
	}
	require.True(t, maxColumn >= 2,
		"expected at least one step with a column >= 2; the highest column "+
			"observed across %d events was %d.  Either the interpreter's "+
			"RegisterStepWithColumn call site is not feeding DWARF columns "+
			"to the writer, or `addressableColumn` rejected every one of "+
			"them because the fixture's source is not readable at the path "+
			"its DWARF names (paths.dat Layout A is what makes a column "+
			"decodable).", len(events), maxColumn)

	// ----- (3) a column-only transition: same line, different column ----
	var columnOnlyTransitions int
	var prev *goldenEvent
	for i := range events {
		ev := events[i]
		if ev.Kind != "step" {
			continue
		}
		if prev != nil && prev.PathID == ev.PathID && prev.Line == ev.Line &&
			prev.Column != ev.Column {
			columnOnlyTransitions++
		}
		e := ev
		prev = &e
	}
	require.True(t, columnOnlyTransitions > 0,
		"expected at least one column-only step transition (two consecutive "+
			"steps on the same path and line with different columns) — that "+
			"is the whole point of column-aware navigation, and "+
			"column_aware.rs line 17 packs three statements onto one line "+
			"precisely so one is observable.  Observed 0 across %d events.",
		len(events))
}

// TestRecorderColumnAwareMultipleStatementsPerLine is the FU-Mwasm-tests
// follow-up to TestRecorderColumnAwareSteps.  It records the
// `column_aware.wasm` fixture (built from
// `testdata/recorder-golden/column_aware.rs`, whose line 17 packs
// `let a: i32 = 1; let b: i32 = 2; let c: i32 = 3;` onto a single
// source line) and asserts the recorder surfaces a strictly distinct
// 1-based column for each of the three statements — the acceptance
// criterion from `codetracer-specs/Planned-Features/
// Column-Aware-Navigation-Other-Languages.plan.md` and the mirror of
// the JS, EVM, Solana, and Cairo siblings' "multiple statements on
// one line each record distinct columns" assertions.
//
// Why this is the load-bearing test: TestRecorderColumnAwareSteps
// exercises the meta.dat flag + a `sekDeltaColumn` event from
// arbitrary single-statement Rust source.  Such an event surfaces on
// every Rust function entry (col 0 → col N), so even a recorder that
// silently drops all DWARF column data EXCEPT the very first row of
// each function would still pass that test.  The genuine
// column-aware-navigation behaviour — splitting a multi-statement
// source line into N distinct steps — is only observable on a fixture
// authored to put N>1 statements on one line.
func TestRecorderColumnAwareMultipleStatementsPerLine(t *testing.T) {
	doc, _ := recordAndDumpFull(t, "column_aware")

	require.True(t, doc.Metadata.Flags.get(t, "has_column_aware_steps"),
		"trace metadata must advertise has_column_aware_steps=true; got %+v",
		doc.Metadata.Flags)

	// Line 17 in column_aware.rs is:
	//     "    let a: i32 = 1; let b: i32 = 2; let c: i32 = 3;"
	// The three `let` statements start at 1-based columns 5, 21, and 37
	// in the source.  DWARF (verified with llvm-dwarfdump --debug-line)
	// emits three line-table rows for line 17 at columns 9, 25, and 41
	// — the *initializer-expression* columns rather than the `let`
	// keyword columns, because rustc maps each `let <name> = <expr>;`
	// statement's first instruction to the expression rather than the
	// keyword.  Either set is acceptable as "three distinct columns";
	// the load-bearing assertion is that THREE distinct columns
	// surface, not which specific column values they have.
	const multiStmtLine int64 = 17
	events := decodeEvents(t, doc)

	columnsByLine := map[int64]map[int64]struct{}{}
	for _, ev := range events {
		if ev.Kind != "step" {
			continue
		}
		if _, ok := columnsByLine[ev.Line]; !ok {
			columnsByLine[ev.Line] = map[int64]struct{}{}
		}
		columnsByLine[ev.Line][ev.Column] = struct{}{}
	}

	cols := columnsByLine[multiStmtLine]
	require.True(t, len(cols) >= 3,
		"expected >= 3 distinct 1-based columns on line %d "+
			"(one per `let` statement in `let a = 1; let b = 2; let c = 3;`), "+
			"got %d distinct column(s): %v.  Either DWARF failed to "+
			"distinguish the statements (check llvm-dwarfdump --debug-line "+
			"output for the .wasm fixture) or the recorder is dropping "+
			"non-is_stmt line-table rows (check the DWARF indexer at "+
			"internal/wasmdebug/dwarf_indexing.go line 699 — the "+
			"`prevLe.IsStmt && ...` guard skips column-only refinement "+
			"rows even though they carry the per-statement columns we "+
			"need).  Per-line column map across the whole trace: %v",
		multiStmtLine, len(cols), cols, columnsByLine)
}

// ===========================================================================
// Test-runner sanity (parallel of the existing testdata fixtures)
// ===========================================================================

// TestRecorderGoldenFixturesBuild asserts every fixture compiles and
// is a real wasm module — the same presence/non-emptiness/magic guard
// that used to protect the `go:embed` directives, now protecting the
// build instead.  It is also the shortest way to reproduce the
// fixtures by hand:
//
//	go test -run TestRecorderGoldenFixturesBuild ./cmd/wazero/
//
// Every `.rs` in the source directory must appear in
// goldenFixtureNames, so a fixture added to the tree without being
// wired up here cannot go unnoticed.
func TestRecorderGoldenFixturesBuild(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join(goldenFixtureSrcDir, "*.rs"))
	require.NoError(t, err)
	require.Equal(t, len(goldenFixtureNames), len(sources),
		"every %s/*.rs must be listed in goldenFixtureNames; found %v "+
			"on disk against %v listed", goldenFixtureSrcDir, sources,
		goldenFixtureNames)

	for _, name := range goldenFixtureNames {
		t.Run(name+".wasm", func(t *testing.T) {
			// goldenFixture already fails on a missing toolchain, an
			// unreadable output or a bad magic header, and returns the
			// compiled bytes only when all three hold.
			require.True(t, len(goldenFixture(t, name)) > 0,
				"fixture %s must build to a non-empty module", name)
		})
	}
}

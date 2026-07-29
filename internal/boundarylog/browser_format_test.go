package boundarylog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// This file builds boundary recordings in the exact on-disk shape the
// browser pipeline produces, so the tests in this package exercise the real
// format rather than a convenient stand-in.
//
// NO MOCKING IS INVOLVED and none is justified here: the builder is a
// *producer replica*, not a test double for the code under test. It is a
// second, independent implementation of two real producers —
//
//	codetracer-wasm-instrumenter/recorder-runtime/browser_session.js
//	    (the `__ct_emit_*` handlers, `flushValues`, `boundaryBindingName`,
//	     `emitRealmBoundary`)
//	codetracer/src/backend-manager/src/browser_stream_host.rs
//	    (`JsonFileCtfsWriter`: `TraceLowLevelEvent`, `translate_value`,
//	     the correlation-marker `RecordEvent`)
//
// — and `TestBuilderReproducesTheCommittedBrowserRecording` pins it against
// the real, browser-produced recording committed at
// `cmd/wazero/testdata/boundary-log/frontend-wasm.ct`. If the builder ever
// drifts from the true format, that test fails and every recording built
// with it is suspect. The parser under test never sees the builder; it only
// ever sees the JSON bytes.
//
// # What that pin does and does NOT cover
//
// The committed recording contains exactly one EXPORT crossing and no
// import crossings at all, so pinning against it constrains `export()` and
// nothing else. `importCall()` is therefore held to the producers' source
// directly, and the reading is recorded here so the next reader does not
// have to re-derive it:
//
//   - `replace_imported_call` in
//     `codetracer-wasm-instrumenter/crates/codetracer-wasm-instrumenter/src/lib.rs`
//     wraps an import edge with `push_realm_boundary(..., FUNC_KIND_IMPORT,
//     ...)` on BOTH sides, exactly as it does for an export edge.
//   - `browser_session.js`'s `__ct_emit_realm_boundary(direction, _fnKind,
//     fnIndex, token)` ignores `_fnKind` entirely and unconditionally emits
//     a `CorrelationMarker`.
//
// So a real import crossing DOES leave two correlation-marker records on
// disk — an earlier reading of this package had it that it left none —
// and `importCall` emits them. They carry no boundary values, so the
// parser treats them as run delimiters exactly like the `Step` records
// that also bracket every run; `TestImportCrossingCarriesItsRealmMarkers`
// and `TestFaithfulImportRecordingRoundTrips` pin both halves.

// jsValue is a value as `browser_session.js` puts it on the wire: a JSON
// payload plus a `typeKind` tag.
type jsValue struct {
	value    any
	typeKind string
}

// jsInt is the encoding for an i32: `recordValue(slot, value | 0, "Int")`.
func jsInt(v int32) jsValue { return jsValue{value: float64(v), typeKind: "Int"} }

// jsBigInt is the encoding for an i64: the hooks receive a JS BigInt, which
// `recordValue` stringifies and tags `"BigInt"` so the exact value survives.
func jsBigInt(v int64) jsValue {
	return jsValue{value: fmt.Sprintf("%d", v), typeKind: "BigInt"}
}

// jsFloat is the encoding for an f32/f64.
func jsFloat(v float64) jsValue { return jsValue{value: v, typeKind: "Float"} }

// onDisk applies `browser_stream_receiver.rs::translate_value`, which maps
// "Int"→Int, "Float"→Float and EVERYTHING ELSE — including "BigInt" — to
// Raw. `type_id` is always 0 on this path.
func (v jsValue) onDisk() map[string]any {
	switch v.typeKind {
	case "Int":
		return map[string]any{"kind": "Int", "i": fmt.Sprintf("%d", int64(v.value.(float64))), "type_id": float64(0)}
	case "Float":
		return map[string]any{"kind": "Float", "f": formatJSFloat(v.value.(float64)), "type_id": float64(0)}
	case "None":
		return map[string]any{"kind": "None", "type_id": float64(0)}
	default:
		return map[string]any{"kind": "Raw", "r": fmt.Sprintf("%v", v.value), "type_id": float64(0)}
	}
}

// formatJSFloat renders a float the way `serde_json` renders the JS Number
// the browser sent.
func formatJSFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// recordingBuilder assembles a `<program>.ct` directory.
type recordingBuilder struct {
	program  string
	workdir  string
	paths    []string
	events   []map[string]any
	funcs    []string // Function records emitted, in order
	varNames map[string]int
	nextVar  int
	token    int
}

func newRecordingBuilder(program string) *recordingBuilder {
	return &recordingBuilder{
		program:  program,
		workdir:  "/tmp/browser-session",
		varNames: map[string]int{},
	}
}

// pathID registers a source path, emitting the `Path` record the host
// writes the first time it resolves one.
func (b *recordingBuilder) pathID(path string) int {
	for i, p := range b.paths {
		if p == path {
			return i
		}
	}
	b.paths = append(b.paths, path)
	b.events = append(b.events, map[string]any{"Path": path})
	return len(b.paths) - 1
}

func (b *recordingBuilder) step(pathID int, line int) {
	b.events = append(b.events, map[string]any{
		"Step": map[string]any{"path_id": float64(pathID), "line": float64(line)},
	})
}

// functionID registers a function, emitting the `Function` record the host
// writes the first time it resolves an `fnId`.
func (b *recordingBuilder) functionID(name string, pathID, line int) int {
	for i, f := range b.funcs {
		if f == name {
			return i
		}
	}
	b.funcs = append(b.funcs, name)
	b.events = append(b.events, map[string]any{
		"Function": map[string]any{
			"name": name, "path_id": float64(pathID), "line": float64(line),
		},
	})
	return len(b.funcs) - 1
}

// values emits one flushed value run: a `Step`, then a
// `VariableName`+`Value` pair per slot. This is `flushValues`.
func (b *recordingBuilder) values(pathID, line int, label, role string, vs []jsValue) {
	if len(vs) == 0 {
		return
	}
	b.step(pathID, line)
	for slot, v := range vs {
		name := fmt.Sprintf("%s:%s%d", label, role, slot)
		id, seen := b.varNames[name]
		if !seen {
			id = b.nextVar
			b.nextVar++
			b.varNames[name] = id
			b.events = append(b.events, map[string]any{"VariableName": name})
		}
		b.events = append(b.events, map[string]any{
			"Value": map[string]any{"variable_id": float64(id), "value": v.onDisk()},
		})
	}
}

// marker emits the correlation-marker `RecordEvent` that `emitRealmBoundary`
// produces for an export crossing. `kind: 12` is
// EVENT_KIND_TRACE_LOG_EVENT; both `metadata` and `content` are JSON
// strings nested inside the JSON.
func (b *recordingBuilder) marker(direction, label, showText string) {
	b.token++
	key := fmt.Sprintf("%d", b.token)
	meta := map[string]any{
		"boundary_id": "js-wasm-realm",
		"description": nil,
		"direction":   direction,
		"format":      nil,
		"key_text":    "key",
		"key_value":   key,
		"marker_id":   0,
		"show_text":   nil,
		"show_value":  label,
	}
	if showText != "" {
		meta["show_text"] = showText
	}
	metaJSON, _ := json.Marshal(meta)
	contentJSON, _ := json.Marshal(map[string]any{"key": key, "payload": label})
	b.events = append(b.events, map[string]any{
		"Event": map[string]any{
			"kind": float64(12), "metadata": string(metaJSON), "content": string(contentJSON),
		},
	})
}

// export emits one export crossing. `body` runs between the ENTER and
// LEAVE markers and may emit nested import crossings.
//
// `exportIndex` is the module's WASM export index, which is what
// `emitRealmBoundary` labels its correlation markers with — NOT the
// daemon's own function-table id that `Call.function_id` carries. The two
// coincide only by accident; the committed fixture has function_id 0 and
// export index 1.
func (b *recordingBuilder) export(name string, exportIndex int, path string, line int, args, results []jsValue, body func()) {
	pid := b.pathID(path)
	b.step(pid, line)
	fnID := b.functionID(name, pid, line)
	b.events = append(b.events, map[string]any{
		"Call": map[string]any{"function_id": float64(fnID), "args": []any{}},
	})
	b.values(pid, line, name, "arg", args)
	b.marker("recv", fmt.Sprintf("wasm export #%d", exportIndex), "")
	if body != nil {
		body()
	}
	b.values(pid, line, name, "ret", results)
	ret := map[string]any{"kind": "None", "type_id": float64(0)}
	if len(results) == 1 {
		ret = results[0].onDisk()
	}
	b.events = append(b.events, map[string]any{
		"Return": map[string]any{"return_value": ret},
	})
	// `emitRealmBoundary` reads `lastResultBinding.get(fnIndex)`, which
	// `flushValues` sets for slot 0 of ANY non-empty export result run —
	// so the `send` marker names `<export>:ret0` whenever the export
	// returned at least one value, not only when it returned exactly one.
	showText := ""
	if len(results) >= 1 {
		showText = fmt.Sprintf("%s:ret0", name)
	}
	b.marker("send", fmt.Sprintf("wasm export #%d", exportIndex), showText)
}

// importCall emits one import crossing.
//
// `browser_session.js` emits no `Call` and no `Return` for an import — it
// returns early for any `fnKind` that is not `FUNC_KIND_EXPORT`. It DOES
// emit the two realm markers, because `__ct_emit_realm_boundary` ignores
// its `fnKind` argument and the instrumenter pushes the hook on both sides
// of an import edge just as it does for an export (see the file header).
// The `show_value` label is the same `wasm export #<n>` template in both
// cases — the runtime has no separate import spelling — so on disk an
// import crossing's markers are indistinguishable from an export's except
// by their surroundings.
func (b *recordingBuilder) importCall(index int, path string, line int, args, results []jsValue) {
	pid := b.pathID(path)
	label := fmt.Sprintf("import #%d", index)
	// `__ct_emit_call(FUNC_KIND_IMPORT, n)` leaves nothing on disk.
	b.values(pid, line, label, "arg", args)
	// `__ct_emit_realm_boundary(ENTER, FUNC_KIND_IMPORT, n, tok)`.
	b.marker("recv", fmt.Sprintf("wasm export #%d", index), "")
	b.values(pid, line, label, "ret", results)
	// `__ct_emit_return(FUNC_KIND_IMPORT, n)` leaves nothing on disk;
	// `__ct_emit_realm_boundary(LEAVE, FUNC_KIND_IMPORT, n, tok)` does.
	b.marker("send", fmt.Sprintf("wasm export #%d", index), "")
}

// write materialises the `<program>.ct` directory under `dir` and returns
// its path.
func (b *recordingBuilder) write(t *testing.T, dir string) string {
	t.Helper()
	ct := filepath.Join(dir, b.program+".ct")
	require.NoError(t, os.MkdirAll(ct, 0o755))

	events, err := json.Marshal(b.events)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ct, "trace.json"), events, 0o644))

	meta, err := json.Marshal(map[string]any{
		"program": b.program, "args": []string{}, "workdir": b.workdir,
		"recorder": map[string]any{"name": "codetracer-js-recorder-browser", "version": "0.1.0"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ct, "trace_metadata.json"), meta, 0o644))

	paths, err := json.Marshal(b.paths)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ct, "trace_paths.json"), paths, 0o644))

	return ct
}

// writeHostState adds the optional `boundary_state.json` sidecar.
func writeHostState(t *testing.T, ctDir string, state any) {
	t.Helper()
	b, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ctDir, HostStateFileName), b, 0o644))
}

// demoRecordingPath is the real, browser-produced recording committed under
// cmd/wazero/testdata. It records one export call:
// `compute_balance(42, 100) -> 620`.
const demoRecordingPath = "../../cmd/wazero/testdata/boundary-log/frontend-wasm.ct"

// TestBuilderReproducesTheCommittedBrowserRecording pins the producer
// replica in this file against the real browser output.
//
// This is the test that makes every other recording built here trustworthy:
// it rebuilds the demo's single crossing with the builder and asserts the
// resulting `trace.json` is structurally identical to the one the browser
// and the backend-manager actually wrote. A drift in the builder — or in
// this package's understanding of the format — fails here first.
func TestBuilderReproducesTheCommittedBrowserRecording(t *testing.T) {
	realRaw, err := os.ReadFile(filepath.Join(demoRecordingPath, "trace.json"))
	require.NoError(t, err)
	var real []map[string]any
	require.NoError(t, json.Unmarshal(realRaw, &real))

	srcPath := "/home/zahary/m/js-support/codetracer/src/db-backend/tests/fixtures/" +
		"cross_process/account-balance-with-wasm/wasm-src/lib.rs"

	b := newRecordingBuilder("frontend-wasm")
	b.export("compute_balance", 1, srcPath, 71,
		[]jsValue{jsInt(42), jsInt(100)},
		[]jsValue{jsInt(620)}, nil)

	built, err := json.Marshal(b.events)
	require.NoError(t, err)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(built, &got))

	require.Equal(t, len(real), len(got),
		"the builder emitted %d records, the real browser recording has %d.\n"+
			"real:  %s\nbuilt: %s", len(got), len(real), string(realRaw), string(built))
	for i := range real {
		require.Equal(t, real[i], got[i],
			"record %d differs from the real browser recording", i)
	}
}

// TestCommittedBrowserRecordingParsesToTheExpectedCrossing asserts what the
// parser recovers from the real fixture, independently of the builder.
func TestCommittedBrowserRecordingParsesToTheExpectedCrossing(t *testing.T) {
	rec, err := LoadRecording(demoRecordingPath)
	require.NoError(t, err)

	require.Equal(t, "frontend-wasm", rec.Program)
	require.Equal(t, "codetracer-js-recorder-browser", rec.Recorder)
	require.Equal(t, 1, len(rec.Paths))
	require.Nil(t, rec.HostState)

	require.Equal(t, 1, len(rec.Crossings))
	c := rec.Crossings[0]
	require.Equal(t, 0, c.Seq)
	require.Equal(t, 0, c.Depth)
	require.Equal(t, CrossingExport, c.Kind)
	require.Equal(t, "compute_balance", c.Name)
	require.Equal(t, []rawValue{{Kind: "Int", Text: "42"}, {Kind: "Int", Text: "100"}}, c.Args)
	require.Equal(t, []rawValue{{Kind: "Int", Text: "620"}}, c.Results)
	require.True(t, c.hasResults)

	require.Equal(t, 1, len(rec.TopLevelExports()))
	require.Equal(t, 0, len(rec.NestedExports()))
}

// TestImportCrossingCarriesItsRealmMarkers pins the half of the format the
// committed fixture cannot pin, because it has no import crossings.
//
// The correction matters beyond bookkeeping: it means an import's INDEX is
// on disk even when the crossing carries no values at all. A `() -> ()`
// import is therefore not, as this package previously recorded, invisible
// to a browser recording — it is invisible to THIS PARSER, which drops the
// markers. See `TestZeroArityImportIsRecordedButNotRecovered`.
func TestImportCrossingCarriesItsRealmMarkers(t *testing.T) {
	b := newRecordingBuilder("marker-shape")
	b.export("run", 0, "/src/x.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			b.importCall(3, "/src/x.wat", 1, []jsValue{jsInt(5)}, []jsValue{jsInt(7)})
		})

	var markers []string
	for _, ev := range b.events {
		raw, ok := ev["Event"]
		if !ok {
			continue
		}
		meta := raw.(map[string]any)["metadata"].(string)
		var payload struct {
			Direction string `json:"direction"`
			ShowValue string `json:"show_value"`
		}
		require.NoError(t, json.Unmarshal([]byte(meta), &payload))
		markers = append(markers, payload.Direction+" "+payload.ShowValue)
	}
	// Export ENTER, import ENTER, import LEAVE, export LEAVE — the order
	// `replace_imported_call` and `instrument_exported_function` produce.
	require.Equal(t, []string{
		"recv wasm export #0",
		"recv wasm export #3",
		"send wasm export #3",
		"send wasm export #0",
	}, markers, "an import crossing must carry its own pair of realm markers")
}

// TestFaithfulImportRecordingRoundTrips proves the markers do not disturb
// crossing recovery: they carry no boundary values, so the parser treats
// them as run delimiters exactly like the `Step` records that already
// bracket every run.
func TestFaithfulImportRecordingRoundTrips(t *testing.T) {
	b := newRecordingBuilder("faithful")
	b.export("run", 0, "/src/x.wat", 1,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			b.importCall(0, "/src/x.wat", 1, []jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			b.importCall(1, "/src/x.wat", 1, []jsValue{jsInt(15)}, nil)
		})
	rec, err := LoadRecording(b.write(t, t.TempDir()))
	require.NoError(t, err)

	require.Equal(t, 3, len(rec.Crossings))
	require.Equal(t, CrossingExport, rec.Crossings[0].Kind)
	require.Equal(t, CrossingImport, rec.Crossings[1].Kind)
	require.Equal(t, uint32(0), rec.Crossings[1].Index)
	require.Equal(t, []rawValue{{Kind: "Int", Text: "5"}, {Kind: "Int", Text: "10"}},
		rec.Crossings[1].Args)
	require.Equal(t, []rawValue{{Kind: "Int", Text: "15"}}, rec.Crossings[1].Results)
	require.Equal(t, CrossingImport, rec.Crossings[2].Kind)
	require.Equal(t, uint32(1), rec.Crossings[2].Index)
	require.False(t, rec.Crossings[2].hasResults)
}

// TestZeroArityImportIsRecordedButNotRecovered documents the known gap
// precisely, so it is not mistaken for a limit of the recording format.
//
// The crossing IS on disk: its two markers name import index 7. This
// parser does not recover it, because the marker's `show_value` uses the
// same `wasm export #<n>` template for both edges and so cannot be told
// apart from an export's marker by its own content. Closing the gap needs
// the producer to spell the two differently; until then a `() -> ()`
// import is replayed unchecked (`Result.UncheckedImportCalls`).
func TestZeroArityImportIsRecordedButNotRecovered(t *testing.T) {
	b := newRecordingBuilder("void-import")
	b.export("run", 0, "/src/x.wat", 1, nil, nil, func() {
		b.importCall(7, "/src/x.wat", 1, nil, nil)
	})
	raw, err := json.Marshal(b.events)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(raw), `wasm export #7`),
		"the import's index reaches disk through its realm markers; got:\n%s", string(raw))

	rec, err := LoadRecording(b.write(t, t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, 1, len(rec.Crossings),
		"only the export is recovered; the value-less import crossing is dropped")
	require.Equal(t, CrossingExport, rec.Crossings[0].Kind)
}

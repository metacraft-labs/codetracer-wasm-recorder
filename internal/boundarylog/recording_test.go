package boundarylog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfsffi"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

func TestParseBindingName(t *testing.T) {
	tests := []struct {
		in    string
		label string
		role  string
		slot  int
		ok    bool
	}{
		{"compute_balance:arg0", "compute_balance", "arg", 0, true},
		{"compute_balance:ret0", "compute_balance", "ret", 0, true},
		{"import #3:arg12", "import #3", "arg", 12, true},
		// An exported function name may itself contain a colon, so the
		// split is anchored on the LAST one.
		{"ns::fn:ret1", "ns::fn", "ret", 1, true},
		// Not boundary bindings.
		{"total", "", "", 0, false},
		{"x:arg", "", "", 0, false},
		{"x:argx", "", "", 0, false},
		{"x:other0", "", "", 0, false},
		{":arg0", "", "", 0, false},
		{"x:", "", "", 0, false},
	}
	for _, tc := range tests {
		label, role, slot, ok := parseBindingName(tc.in)
		require.Equal(t, tc.ok, ok, "parseBindingName(%q) ok", tc.in)
		if tc.ok {
			require.Equal(t, tc.label, label, "parseBindingName(%q) label", tc.in)
			require.Equal(t, tc.role, role, "parseBindingName(%q) role", tc.in)
			require.Equal(t, tc.slot, slot, "parseBindingName(%q) slot", tc.in)
		}
	}
}

func TestParseImportLabel(t *testing.T) {
	idx, ok := parseImportLabel("import #7")
	require.True(t, ok)
	require.Equal(t, uint32(7), idx)

	_, ok = parseImportLabel("compute_balance")
	require.False(t, ok)

	_, ok = parseImportLabel("import #x")
	require.False(t, ok)
}

// TestReconstructsNestedImportCrossings pins the crossing recovery against
// the shape a browser recording really has: an export bracketed by
// Call/Return with import crossings nested inside, visible only as their
// value runs.
func TestReconstructsNestedImportCrossings(t *testing.T) {
	b := newRecordingBuilder("nested")
	b.export("run", 0, "/src/lib.rs", 10,
		[]jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
			b.importCall(0, "/src/lib.rs", 10,
				[]jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
			b.importCall(1, "/src/lib.rs", 10,
				[]jsValue{jsInt(15)}, nil)
		})
	dir := b.write(t, t.TempDir())

	rec, err := LoadRecording(dir)
	require.NoError(t, err)
	require.Equal(t, 3, len(rec.Crossings))

	// Call order: the export opens first, then the two imports.
	require.Equal(t, CrossingExport, rec.Crossings[0].Kind)
	require.Equal(t, "run", rec.Crossings[0].Name)
	require.Equal(t, 0, rec.Crossings[0].Depth)
	require.Equal(t, []rawValue{{"Int", "5"}}, rec.Crossings[0].Args)
	require.Equal(t, []rawValue{{"Int", "30"}}, rec.Crossings[0].Results)

	require.Equal(t, CrossingImport, rec.Crossings[1].Kind)
	require.Equal(t, uint32(0), rec.Crossings[1].Index)
	require.Equal(t, 1, rec.Crossings[1].Depth, "an import inside an export is at depth 1")
	require.Equal(t, []rawValue{{"Int", "5"}, {"Int", "10"}}, rec.Crossings[1].Args)
	require.Equal(t, []rawValue{{"Int", "15"}}, rec.Crossings[1].Results)
	require.True(t, rec.Crossings[1].hasResults)

	require.Equal(t, CrossingImport, rec.Crossings[2].Kind)
	require.Equal(t, uint32(1), rec.Crossings[2].Index)
	require.Equal(t, []rawValue{{"Int", "15"}}, rec.Crossings[2].Args)
	require.False(t, rec.Crossings[2].hasResults,
		"an import that returns nothing must be distinguishable from one whose "+
			"results were dropped")

	require.Equal(t, 1, len(rec.TopLevelExports()))
	require.Equal(t, 0, len(rec.NestedExports()))
}

// TestReconstructsBackToBackResultlessImports pins the disambiguation rule
// documented in recording.go: a second argument run for the same import
// closes the crossing already open, rather than being appended to it.
func TestReconstructsBackToBackResultlessImports(t *testing.T) {
	b := newRecordingBuilder("backtoback")
	b.export("run", 0, "/src/lib.rs", 1, nil, nil, func() {
		b.importCall(0, "/src/lib.rs", 1, []jsValue{jsInt(1)}, nil)
		b.importCall(0, "/src/lib.rs", 1, []jsValue{jsInt(2)}, nil)
	})
	dir := b.write(t, t.TempDir())

	rec, err := LoadRecording(dir)
	require.NoError(t, err)
	require.Equal(t, 3, len(rec.Crossings))
	require.Equal(t, []rawValue{{"Int", "1"}}, rec.Crossings[1].Args)
	require.Equal(t, []rawValue{{"Int", "2"}}, rec.Crossings[2].Args)
}

// TestReconstructsAnArgumentlessImport covers an import whose only on-disk
// trace is its result run.
func TestReconstructsAnArgumentlessImport(t *testing.T) {
	b := newRecordingBuilder("noargs")
	b.export("run", 0, "/src/lib.rs", 1, nil, []jsValue{jsInt(9)}, func() {
		b.importCall(2, "/src/lib.rs", 1, nil, []jsValue{jsInt(9)})
	})
	dir := b.write(t, t.TempDir())

	rec, err := LoadRecording(dir)
	require.NoError(t, err)
	require.Equal(t, 2, len(rec.Crossings))
	require.Equal(t, CrossingImport, rec.Crossings[1].Kind)
	require.Equal(t, uint32(2), rec.Crossings[1].Index)
	require.Equal(t, 0, len(rec.Crossings[1].Args))
	require.Equal(t, []rawValue{{"Int", "9"}}, rec.Crossings[1].Results)
}

// TestRejectsAnUnbalancedRecording pins the spec §8 discipline: a recording
// whose crossings never close (the M35b "exit by branching to the function
// label" hole) is refused, not replayed with a silent gap.
func TestRejectsAnUnbalancedRecording(t *testing.T) {
	events := []map[string]any{
		{"Path": "/src/lib.rs"},
		{"Step": map[string]any{"path_id": 0, "line": 1}},
		{"Function": map[string]any{"name": "run", "path_id": 0, "line": 1}},
		{"Call": map[string]any{"function_id": 0, "args": []any{}}},
		// ... and no Return.
	}
	dir := writeRawRecording(t, events)
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "structurally unbalanced"),
		"error should name the imbalance; got: %v", err)
	require.True(t, strings.Contains(err.Error(), "never closed"),
		"error should say what never closed; got: %v", err)
}

func TestRejectsAReturnWithNoOpenFrame(t *testing.T) {
	events := []map[string]any{
		{"Return": map[string]any{"return_value": map[string]any{"kind": "None", "type_id": 0}}},
	}
	dir := writeRawRecording(t, events)
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "no open export frame"),
		"got: %v", err)
}

func TestRejectsADanglingVariableID(t *testing.T) {
	events := []map[string]any{
		{"Value": map[string]any{"variable_id": 3,
			"value": map[string]any{"kind": "Int", "i": "1", "type_id": 0}}},
	}
	dir := writeRawRecording(t, events)
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "variable_id"), "got: %v", err)
}

func TestRejectsAnOutOfOrderValueRun(t *testing.T) {
	events := []map[string]any{
		{"Path": "/src/lib.rs"},
		{"Function": map[string]any{"name": "run", "path_id": 0, "line": 1}},
		{"Call": map[string]any{"function_id": 0, "args": []any{}}},
		{"VariableName": "run:arg1"},
		{"Value": map[string]any{"variable_id": 0,
			"value": map[string]any{"kind": "Int", "i": "1", "type_id": 0}}},
	}
	dir := writeRawRecording(t, events)
	_, err := LoadRecording(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "slot"), "got: %v", err)
}

// TestIgnoresNonBoundaryBindings pins that a recording carrying variable
// bindings outside the boundary vocabulary still parses: replay re-derives
// everything inside the module anyway, so an unrecognised binding is not a
// reason to refuse the recording.
func TestIgnoresNonBoundaryBindings(t *testing.T) {
	events := []map[string]any{
		{"Path": "/src/lib.rs"},
		{"Function": map[string]any{"name": "run", "path_id": 0, "line": 1}},
		{"Call": map[string]any{"function_id": 0, "args": []any{}}},
		{"VariableName": "some_local"},
		{"Value": map[string]any{"variable_id": 0,
			"value": map[string]any{"kind": "Int", "i": "7", "type_id": 0}}},
		{"VariableName": "run:ret0"},
		{"Value": map[string]any{"variable_id": 1,
			"value": map[string]any{"kind": "Int", "i": "3", "type_id": 0}}},
		{"Return": map[string]any{"return_value": map[string]any{"kind": "Int", "i": "3", "type_id": 0}}},
	}
	dir := writeRawRecording(t, events)
	rec, err := LoadRecording(dir)
	require.NoError(t, err)
	require.Equal(t, 1, len(rec.Crossings))
	require.Equal(t, []rawValue{{"Int", "3"}}, rec.Crossings[0].Results)
}

// TestLoadRecordingRefusesTheRetiredJSONLayout: a `.ct` directory, or a
// `trace.json`, is the three-file JSON layout `record-web` no longer writes.
// It is refused by name rather than probed, so nobody mistakes it for a
// recording this reader could have opened.
func TestLoadRecordingRefusesTheRetiredJSONLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "old.ct")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "trace.json"), []byte("[]"), 0o644))

	for _, path := range []string{dir, filepath.Join(dir, "trace.json")} {
		_, err := LoadRecording(path)
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "retired three-file JSON layout"),
			"%s: got: %v", path, err)
	}
}

// TestLoadRecordingRefusesAContainerWithoutABoundaryLog: a CTFS container
// that is not a `record-web` recording — any recorder's ordinary trace — is
// named as such.
func TestLoadRecordingRefusesAContainerWithoutABoundaryLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.ct")
	require.NoError(t, ctfsffi.Create(path, 4096))
	require.NoError(t, ctfsffi.Append(path, map[string][]byte{"meta.dat": {0}}))
	_, err := LoadRecording(path)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "carries no boundary.log"), "got: %v", err)
}

// TestLoadRecordingRefusesAnUnfinishedLog: a finished recording's log ends
// with End; one that stops short was never finalised.
func TestLoadRecordingRefusesAnUnfinishedLog(t *testing.T) {
	b := newRecordingBuilder("unfinished")
	b.export("run", 0, "/src/lib.rs", 1, nil, nil, nil)
	path := filepath.Join(t.TempDir(), "unfinished.ct")
	WriteTestRecording(t, path, EncodeTestLog(b.program, b.workdir, b.events, false))
	_, err := LoadRecording(path)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "without its End frame"), "got: %v", err)
}

// writeRawRecording materialises a `.ct` from hand-written event
// records, for the malformed cases the producer replica cannot express.
func writeRawRecording(t *testing.T, events []map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.ct")
	WriteTestRecording(t, path, EncodeTestLog("raw", "/", events, true))
	return path
}

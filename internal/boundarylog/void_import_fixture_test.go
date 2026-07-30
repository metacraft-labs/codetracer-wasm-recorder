package boundarylog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// The committed recording of `void_import.wasm`, and the guard that keeps it
// honest.
//
// `cmd/wazero/boundary_log_test.go` drives the CLI end to end — it is the
// only place the "a diverged replay writes no trace" policy can be checked
// at all, because `internal/boundarylog` replays with a nil recorder and
// writes nothing either way. But the producer replica that builds every
// other recording in this suite lives in this package's tests and is not
// importable from `package main`, so that one recording is committed under
// `cmd/wazero/testdata/` instead.
//
// A committed recording is a copy, and a copy drifts. This file is what
// stops it: `voidImportRecording` is the single definition of the fixture's
// content, `TestCommittedVoidImportRecordingMatchesTheReplica` asserts the
// bytes on disk are exactly what it produces today, and the replica itself
// is pinned to the real browser output by
// `TestBuilderReproducesTheCommittedBrowserRecording`. So the committed
// `.ct` is held to the real producers transitively, not taken on trust.

// voidImportRecordingPath is where the fixture lives, relative to this
// package.
const voidImportRecordingPath = "../../cmd/wazero/testdata/boundary-log/void-import.ct"

// voidImportRecording builds the recording of one `ping_n(2)` call against
// `void_import.wasm`: two crossings into `host_ping`, whose signature is
// `() -> ()`, and nothing else.
//
// Every crossing in it is value-less, which is the point — the export's
// argument is the ONLY number in the recording that says how many there
// should be, so corrupting it changes the number of value-less import calls
// the replayed module makes and nothing else.
func voidImportRecording() *recordingBuilder {
	b := newRecordingBuilder("void-import")
	b.export("ping_n", 0, "/src/void_import.wat", 1,
		[]jsValue{jsInt(2)}, []jsValue{jsInt(2)}, func() {
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
			b.importCall(0, "/src/void_import.wat", 1, nil, nil)
		})
	return b
}

// TestCommittedVoidImportRecordingMatchesTheReplica fails the moment the
// committed `.ct` and the producer replica disagree — i.e. the moment the
// CLI suite starts replaying a recording that is not the format the browser
// produces.
func TestCommittedVoidImportRecordingMatchesTheReplica(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join(voidImportRecordingPath, "trace.json"))
	require.NoError(t, err)
	var committed []map[string]any
	require.NoError(t, json.Unmarshal(onDisk, &committed))

	built, err := json.Marshal(voidImportRecording().events)
	require.NoError(t, err)
	var rebuilt []map[string]any
	require.NoError(t, json.Unmarshal(built, &rebuilt))

	require.Equal(t, len(committed), len(rebuilt),
		"the committed recording has %d records, the replica builds %d.\n"+
			"committed: %s\nbuilt:     %s",
		len(committed), len(rebuilt), string(onDisk), string(built))
	for i := range committed {
		require.Equal(t, committed[i], rebuilt[i],
			"record %d of the committed recording differs from the replica", i)
	}
}

// TestCommittedVoidImportRecordingParsesToTwoValueLessCrossings states what
// the CLI suite is entitled to assume about the fixture, read through the
// parser rather than through the builder.
func TestCommittedVoidImportRecordingParsesToTwoValueLessCrossings(t *testing.T) {
	rec, err := LoadRecording(voidImportRecordingPath)
	require.NoError(t, err)
	require.True(t, rec.MarkersIdentifyImports)
	require.Equal(t, 3, len(rec.Crossings))
	require.Equal(t, CrossingExport, rec.Crossings[0].Kind)
	for _, i := range []int{1, 2} {
		c := rec.Crossings[i]
		require.Equal(t, CrossingImport, c.Kind)
		require.Equal(t, uint32(0), c.Index)
		require.Equal(t, 0, len(c.Args))
		require.Equal(t, 0, len(c.Results))
	}
}

// Streaming boundary-log consumption — `WASM-Replay-Snapshots-And-Slices.md`
// §2. These tests exercise the reader in isolation; `stream_replay_test.go`
// drives a real module off it.
//
// NO MOCKS: the bytes fed in are produced by the same `recordingBuilder` that
// `TestBuilderReproducesTheCommittedBrowserRecording` pins against the real
// browser output, and they are fed through the real scanner, the real
// assembler and, where a growing file is involved, a real file on disk.
package boundarylog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// streamBytes renders a builder's records as the producer would write them.
func streamBytes(t *testing.T, b *recordingBuilder) []byte {
	t.Helper()
	raw, err := json.Marshal(b.events)
	require.NoError(t, err)
	return raw
}

// threeCallStream builds a recording of three `compute_balance` calls.
func threeCallStream(t *testing.T) []byte {
	t.Helper()
	b := newRecordingBuilder("stream")
	for _, a := range [][2]int32{{42, 100}, {7, 3}, {1000, 1}} {
		b.export("compute_balance", 1, "/src/lib.rs", 71,
			[]jsValue{jsInt(a[0]), jsInt(a[1])},
			[]jsValue{jsInt(a[0]*10 + a[1]*2)}, nil)
	}
	return streamBytes(t, b)
}

// collectGroups drains a reader, returning the groups and the terminating
// error.
func collectGroups(t *testing.T, r *StreamReader) ([][]Crossing, error) {
	t.Helper()
	var out [][]Crossing
	for {
		g, err := r.NextGroup()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		// The reader hands out a view into its own slice, which later appends
		// may reallocate; copy so the assertions below see what was yielded.
		out = append(out, append([]Crossing(nil), g...))
	}
}

// TestStreamYieldsOneGroupPerExportedCall is the reader's core contract: a call
// group is one top-level exported call plus everything nested inside it, and it
// is yielded as soon as that call closes.
func TestStreamYieldsOneGroupPerExportedCall(t *testing.T) {
	raw := threeCallStream(t)
	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw)))
	require.NoError(t, err)
	require.Equal(t, 3, len(groups))
	for i, g := range groups {
		require.Equal(t, 1, len(g))
		require.Equal(t, CrossingExport, g[0].Kind)
		require.Equal(t, 0, g[0].Depth)
		require.Equal(t, i, g[0].Seq)
		require.Equal(t, "compute_balance", g[0].Name)
	}
	require.Equal(t, []rawValue{{Kind: "Int", Text: "42"}, {Kind: "Int", Text: "100"}},
		groups[0][0].Args)
	require.Equal(t, []rawValue{{Kind: "Int", Text: "620"}}, groups[0][0].Results)
}

// TestStreamGroupsCarryTheirNestedImports: the imports a call made must arrive
// with it, because the replay services them from inside that call.
func TestStreamGroupsCarryTheirNestedImports(t *testing.T) {
	b := newRecordingBuilder("nested")
	b.export("run", 0, "/src/x.wat", 1, []jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
		b.importCall(0, "/src/x.wat", 1, []jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
		b.importCall(1, "/src/x.wat", 1, []jsValue{jsInt(15)}, []jsValue{jsInt(30)})
	})
	b.export("run", 0, "/src/x.wat", 1, []jsValue{jsInt(1)}, []jsValue{jsInt(2)}, nil)

	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(streamBytes(t, b))))
	require.NoError(t, err)
	require.Equal(t, 2, len(groups))
	require.Equal(t, 3, len(groups[0]))
	require.Equal(t, CrossingExport, groups[0][0].Kind)
	require.Equal(t, CrossingImport, groups[0][1].Kind)
	require.Equal(t, uint32(0), groups[0][1].Index)
	require.Equal(t, CrossingImport, groups[0][2].Kind)
	require.Equal(t, uint32(1), groups[0][2].Index)
	require.Equal(t, 1, len(groups[1]))
}

// TestStreamAgreesWithTheBatchParser is the anti-drift property. The streaming
// reader and `LoadRecording` must recover exactly the same crossings from the
// same bytes, or the streaming pipeline would be materialising something
// subtly different from what the offline one does.
//
// They share `assembler`, so this is a check that the sharing is real rather
// than a check of two implementations — which is the point: it fails the moment
// someone reintroduces a second copy of the reconstruction rules.
func TestStreamAgreesWithTheBatchParser(t *testing.T) {
	b := newRecordingBuilder("agree")
	b.export("run", 0, "/src/x.wat", 1, []jsValue{jsInt(5)}, []jsValue{jsInt(30)}, func() {
		b.importCall(0, "/src/x.wat", 1, []jsValue{jsInt(5), jsInt(10)}, []jsValue{jsInt(15)})
		b.importCall(1, "/src/x.wat", 1, []jsValue{jsInt(15)}, nil)
	})
	b.export("other", 2, "/src/x.wat", 9, nil, []jsValue{jsBigInt(1 << 40)}, nil)
	dir := b.write(t, t.TempDir())

	batch, err := LoadRecording(dir)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, "trace.json"))
	require.NoError(t, err)
	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw)))
	require.NoError(t, err)

	var streamed []Crossing
	for _, g := range groups {
		streamed = append(streamed, g...)
	}
	require.Equal(t, batch.Crossings, streamed)
}

// byteAtATime hands out one byte per Read, which is the worst case for a
// scanner that splits a growing document: every record's braces, strings and
// escapes are seen across as many reads as they have bytes.
type byteAtATime struct {
	b []byte
	i int
}

func (r *byteAtATime) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	p[0] = r.b[r.i]
	r.i++
	return 1, nil
}

// TestStreamSurvivesArbitraryChunking: a record split across reads must not be
// decoded until it is whole.
func TestStreamSurvivesArbitraryChunking(t *testing.T) {
	raw := threeCallStream(t)
	groups, err := collectGroups(t, NewStreamReader(&byteAtATime{b: raw}))
	require.NoError(t, err)
	require.Equal(t, 3, len(groups))
}

// TestStreamHandlesBracesInsideStrings guards the scanner's string tracking.
// The producer's correlation markers embed whole JSON documents as strings, so
// `{`, `}` and escaped quotes inside a string value are the normal case, not an
// exotic one.
func TestStreamHandlesBracesInsideStrings(t *testing.T) {
	b := newRecordingBuilder("braces")
	// `marker` nests two JSON documents as strings; `export` emits two of them.
	b.export("weird\"name{}", 0, "/src/x.wat", 1,
		[]jsValue{jsInt(1)}, []jsValue{jsInt(2)}, nil)
	raw := streamBytes(t, b)
	require.True(t, bytes.Contains(raw, []byte(`\"`)),
		"the fixture does not actually contain an escaped quote")

	groups, err := collectGroups(t, NewStreamReader(&byteAtATime{b: raw}))
	require.NoError(t, err)
	require.Equal(t, 1, len(groups))
	require.Equal(t, "weird\"name{}", groups[0][0].Name)
}

// ---------------------------------------------------------------------------
// Truncation
// ---------------------------------------------------------------------------

// TestTruncatedMidRecordIsNamed: the producer died halfway through writing a
// record.
func TestTruncatedMidRecordIsNamed(t *testing.T) {
	raw := threeCallStream(t)
	// Cut inside the last record, past the point where two calls are complete.
	cut := bytes.LastIndex(raw, []byte(`{"Return"`)) + 12
	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw[:cut])))
	require.Error(t, err)

	trunc, ok := IsTruncation(err)
	require.True(t, ok, "expected a truncation error, got %T: %v", err, err)
	require.Equal(t, TruncatedMidRecord, trunc.Kind)
	require.True(t, trunc.PendingBytes > 0)
	// The complete calls before the cut are still delivered: a truncated
	// recording is a prefix, not a write-off.
	require.Equal(t, 2, len(groups))
	require.Equal(t, 2, trunc.Groups)
	require.True(t, strings.Contains(err.Error(), "middle of a record"), err.Error())
}

// TestTruncatedMidCrossingIsNamed: the producer stopped while an exported call
// was open — records all complete, crossing not.
func TestTruncatedMidCrossingIsNamed(t *testing.T) {
	b := newRecordingBuilder("open")
	b.export("run", 0, "/src/x.wat", 1, []jsValue{jsInt(1)}, []jsValue{jsInt(2)}, nil)
	// A second call whose `Call` record lands but whose `Return` never does.
	b.step(0, 1)
	b.events = append(b.events, map[string]any{
		"Call": map[string]any{"function_id": float64(0), "args": []any{}},
	})
	raw := streamBytes(t, b)

	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw)))
	require.Error(t, err)
	trunc, ok := IsTruncation(err)
	require.True(t, ok, "expected a truncation error, got %T: %v", err, err)
	require.Equal(t, TruncatedMidCrossing, trunc.Kind)
	require.Equal(t, 1, trunc.OpenCrossings)
	require.Equal(t, 1, len(groups), "the completed call must still be delivered")
}

// TestTruncatedUnterminatedArrayIsNamedAndBenign: the producer stopped cleanly
// between calls without closing the array. Everything it did write is faithful,
// and the diagnostic says so.
func TestTruncatedUnterminatedArrayIsNamedAndBenign(t *testing.T) {
	raw := threeCallStream(t)
	require.Equal(t, byte(']'), raw[len(raw)-1])

	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw[:len(raw)-1])))
	require.Error(t, err)
	trunc, ok := IsTruncation(err)
	require.True(t, ok, "expected a truncation error, got %T: %v", err, err)
	require.Equal(t, TruncatedUnterminated, trunc.Kind)
	require.Equal(t, 3, len(groups), "every complete call must still be delivered")
	require.Equal(t, 3, trunc.Groups)
	require.True(t, strings.Contains(err.Error(), "faithful"), err.Error())
}

// TestStreamRefusesNonArrayInput and its sibling below keep a malformed stream
// from being read as an empty one.
func TestStreamRefusesNonArrayInput(t *testing.T) {
	_, err := collectGroups(t, NewStreamReader(bytes.NewReader([]byte(`{"Path":"x"}`))))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "JSON array"), err.Error())
}

func TestStreamRefusesNonObjectRecords(t *testing.T) {
	_, err := collectGroups(t, NewStreamReader(bytes.NewReader([]byte(`[1,2]`))))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "JSON objects"), err.Error())
}

// TestEmptyStreamIsNotAnError: a recording of a page that loaded and unloaded
// without calling into the module is `[]` — zero groups, clean EOF.
func TestEmptyStreamIsNotAnError(t *testing.T) {
	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader([]byte("[]"))))
	require.NoError(t, err)
	require.Equal(t, 0, len(groups))
}

// ---------------------------------------------------------------------------
// Following a growing file
// ---------------------------------------------------------------------------

// TestFollowFileWaitsForTheProducer is the "recording still in progress vs.
// genuinely cut off" distinction, made concrete.
//
// The file is written a record at a time with the reader already consuming it.
// The reader must block at each end-of-file rather than declaring the recording
// over, and must return EOF only once `done` is closed.
func TestFollowFileWaitsForTheProducer(t *testing.T) {
	raw := threeCallStream(t)
	path := filepath.Join(t.TempDir(), "trace.json")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	done := make(chan struct{})
	src, err := FollowFile(path, done)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	// The writer dribbles the document out in small pieces with a pause
	// between each, so the reader is forced to wait at end-of-file repeatedly.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		for i := 0; i < len(raw); i += 17 {
			end := i + 17
			if end > len(raw) {
				end = len(raw)
			}
			if _, err := f.Write(raw[i:end]); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()

	groups, err := collectGroups(t, NewStreamReader(src))
	wg.Wait()
	require.NoError(t, err)
	require.Equal(t, 3, len(groups))
}

// TestFollowFileReportsAProducerThatDiesMidWrite: `done` closing while the
// document is incomplete is exactly the "genuinely cut off" case, and it is
// reported rather than silently accepted as the end of the recording.
func TestFollowFileReportsAProducerThatDiesMidWrite(t *testing.T) {
	raw := threeCallStream(t)
	cut := bytes.LastIndex(raw, []byte(`{"Return"`))
	path := filepath.Join(t.TempDir(), "trace.json")
	require.NoError(t, os.WriteFile(path, raw[:cut], 0o644))

	done := make(chan struct{})
	close(done) // the producer is already gone
	src, err := FollowFile(path, done)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	groups, err := collectGroups(t, NewStreamReader(src))
	require.Error(t, err)
	trunc, ok := IsTruncation(err)
	require.True(t, ok, "expected a truncation error, got %T: %v", err, err)
	require.Equal(t, TruncatedMidCrossing, trunc.Kind)
	require.Equal(t, 2, len(groups))
}

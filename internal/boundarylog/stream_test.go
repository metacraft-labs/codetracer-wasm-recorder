// Streaming boundary-log consumption — `WASM-Replay-Snapshots-And-Slices.md`
// §2. These tests exercise the reader in isolation; `stream_replay_test.go`
// drives a real module off it.
//
// NO MOCKS: the bytes fed in are produced by the same `recordingBuilder` that
// `TestBuilderReproducesTheCommittedBrowserRecording` pins against the real
// browser output, encoded as CTBL by `EncodeTestLog`, and they are fed through
// the real frame decoder and the real assembler.
package boundarylog

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// streamBytes renders a builder's records as the boundary log the producer
// writes to its consumer, `End` frame included.
func streamBytes(t *testing.T, b *recordingBuilder) []byte {
	t.Helper()
	return b.log()
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

	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(ReadTestLog(t, dir))))
	require.NoError(t, err)

	var streamed []Crossing
	for _, g := range groups {
		streamed = append(streamed, g...)
	}
	require.Equal(t, batch.Crossings, streamed)
}

// byteAtATime hands out one byte per Read, which is the worst case for a
// decoder of a growing stream: every frame's length prefix and payload are
// seen across as many reads as they have bytes.
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

// ---------------------------------------------------------------------------
// Truncation
// ---------------------------------------------------------------------------

// TestTruncatedMidRecordIsNamed: the stream stops halfway through a frame.
func TestTruncatedMidRecordIsNamed(t *testing.T) {
	raw := threeCallStream(t)
	_, frames, tags := SplitTestLogFrames(t, raw)
	// Cut inside the last `Return` frame, past the point where two calls are
	// complete.
	offset := len(raw)
	lastReturn := -1
	for i := len(frames) - 1; i >= 0; i-- {
		offset -= len(frames[i])
		if tags[i] == tagReturn {
			lastReturn = offset
			break
		}
	}
	require.True(t, lastReturn > 0)
	cut := lastReturn + 6
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
	require.True(t, strings.Contains(err.Error(), "middle of a frame"), err.Error())
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

// TestTruncatedUnterminatedStreamIsNamedAndBenign: the producer stopped
// cleanly between records without sending `End` — what a killed page leaves.
// Everything it did write is faithful, and the diagnostic says so.
func TestTruncatedUnterminatedStreamIsNamedAndBenign(t *testing.T) {
	raw := threeCallStream(t)
	_, frames, tags := SplitTestLogFrames(t, raw)
	require.Equal(t, byte(tagEnd), tags[len(tags)-1])
	unterminated := raw[:len(raw)-len(frames[len(frames)-1])]

	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(unterminated)))
	require.Error(t, err)
	trunc, ok := IsTruncation(err)
	require.True(t, ok, "expected a truncation error, got %T: %v", err, err)
	require.Equal(t, TruncatedUnterminated, trunc.Kind)
	require.Equal(t, 3, len(groups), "every complete call must still be delivered")
	require.Equal(t, 3, trunc.Groups)
	require.True(t, strings.Contains(err.Error(), "faithful"), err.Error())
}

// TestStreamRefusesTheRetiredJSONLayout: a `trace.json` piped in where a
// boundary log belongs is refused by name, not read as an empty stream.
func TestStreamRefusesTheRetiredJSONLayout(t *testing.T) {
	_, err := collectGroups(t, NewStreamReader(bytes.NewReader([]byte(`[{"Path":"x"}]`))))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "not a CTBL boundary log"), err.Error())
}

// TestStreamRefusesAnUnknownVersion: a partly-understood boundary log replays
// into a divergence far from its cause, so a version this reader does not
// know is a hard error.
func TestStreamRefusesAnUnknownVersion(t *testing.T) {
	raw := threeCallStream(t)
	raw[4] = 2
	_, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw)))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "version 2"), err.Error())
}

// TestStreamRefusesAStreamWithoutAHeader: the first frame names the
// recording; a stream that starts anywhere else is not one.
func TestStreamRefusesAStreamWithoutAHeader(t *testing.T) {
	raw := []byte("CTBL\x01")
	raw = append(raw, EncodeTestRecord(map[string]any{"Path": "x"})...)
	_, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw)))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "first frame must be its Header"), err.Error())
}

// TestEmptyStreamIsNotAnError: a recording of a page that loaded and unloaded
// without calling into the module is a Header and an End — zero groups,
// clean EOF.
func TestEmptyStreamIsNotAnError(t *testing.T) {
	raw := EncodeTestLog("empty", "/", nil, true)
	groups, err := collectGroups(t, NewStreamReader(bytes.NewReader(raw)))
	require.NoError(t, err)
	require.Equal(t, 0, len(groups))
}

// TestReadHeaderReturnsTheRecordingsMetadataBeforeAnyCall: the CLI starts a
// streaming replay from the Header alone, so it must be available without
// waiting for — or consuming — a single crossing.
func TestReadHeaderReturnsTheRecordingsMetadataBeforeAnyCall(t *testing.T) {
	raw := threeCallStream(t)
	_, frames, _ := SplitTestLogFrames(t, raw)
	headerOnly := raw[:ctblPrefixLen+len(frames[0])]
	r := NewStreamReader(&byteAtATime{b: headerOnly})
	rec, err := r.ReadHeader("<test>")
	require.NoError(t, err)
	require.Equal(t, "stream", rec.Program)
	require.Equal(t, "codetracer-js-recorder-browser", rec.Recorder)
	require.Equal(t, 0, len(rec.Crossings))
}

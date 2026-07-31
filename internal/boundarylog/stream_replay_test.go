// The streaming pipeline end to end — `WASM-Replay-Snapshots-And-Slices.md` §2
// together with §7: a replaying recorder that consumes the boundary stream as
// it arrives, emits a snapshot at each quiescent point *as it is reached*, and
// seals slices while the recording is still running.
//
// NO MOCKS. The producer is a goroutine writing the real bytes
// `recordingBuilder` produces into a real `io.Pipe`; the consumer is the real
// replay driver against the real committed modules and the real CTFS writer.
// The one thing that is arranged rather than incidental is the *pipe*, and it
// is arranged precisely because it is what makes the timing observable: an
// `io.Pipe` write completes only when a reader has taken the bytes, so the
// producer's own progress is a witness to the consumer's.
package boundarylog_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
	"github.com/tetratelabs/wazero/tracewriter"
)

// pipeProducer writes a recording into an `io.Pipe`, chunk by chunk, recording
// what the consumer had achieved by the time each write completed.
type pipeProducer struct {
	w      *io.PipeWriter
	chunks [][]byte
	// observe is sampled after each chunk's write returns. Because an
	// `io.Pipe` write does not return until a reader has consumed the bytes,
	// the sample is a lower bound on the consumer's progress at that moment.
	observe func() int
	// observed[i] is the sample taken after chunk i was written.
	observed []int
	// stopAfter, when > 0, cuts the stream off after that many chunks and
	// closes the pipe — a producer that dies mid-recording.
	stopAfter int
	err       error
}

func (p *pipeProducer) run(wg *sync.WaitGroup) {
	defer wg.Done()
	for i, c := range p.chunks {
		if p.stopAfter > 0 && i >= p.stopAfter {
			break
		}
		if _, err := p.w.Write(c); err != nil {
			p.err = err
			break
		}
		p.observed = append(p.observed, p.observe())
	}
	_ = p.w.Close()
}

// streamHarness bundles a runtime, a compiled module and a recording's
// metadata for a streaming replay.
func streamHarness(t *testing.T, wasmPath, ctDir string) (context.Context, wazero.Runtime, wazero.CompiledModule, *boundarylog.Recording) {
	t.Helper()
	ctx := context.Background()
	bin, err := os.ReadFile(wasmPath)
	require.NoError(t, err)
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	compiled, err := rt.CompileModule(ctx, bin)
	require.NoError(t, err)

	// The metadata only — the crossings are what the stream delivers.
	rec, err := boundarylog.LoadRecordingMetadata(ctDir)
	require.NoError(t, err)
	require.Equal(t, 0, len(rec.Crossings))
	return ctx, rt, compiled, rec
}

// TestSnapshotsAreEmittedWhileTheStreamIsStillArriving is deliverable 4's
// headline property, and the reason it is asserted this way rather than by
// counting snapshots at the end.
//
// "Snapshots are produced as each quiescent point is reached, not in a pass at
// the end" is a statement about *timing*, and a test that only inspects the
// final state cannot tell the two apart. So the test makes the producer and the
// consumer share a synchronous pipe and then reads the timing off the
// producer's own progress:
//
//   - an `io.Pipe` write returns only once a reader has consumed the bytes;
//   - the reader consumes chunk k only when the driver asks for call group k;
//   - the driver asks for group k only after call k-1 returned and the
//     quiescent-point hook for point k ran.
//
// Therefore, by the time the producer's k-th write returns, snapshots 0..k must
// already exist. The assertion `observed[k] >= k+1` is exactly that. Deferring
// snapshot derivation to the end of the recording — the M38 behaviour — makes
// every sample 0 and fails this test at the first chunk.
func TestSnapshotsAreEmittedWhileTheStreamIsStillArriving(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	chunks := boundarylog.StreamChunksForRecording(t, ctDir)
	// One chunk per exported call, plus the document tail.
	require.Equal(t, len(callArgs)+1, len(chunks))

	ctx, rt, compiled, rec := streamHarness(t, balanceCalcWasm, ctDir)

	var snapshots int64
	builder, err := wasmsnapshot.NewIncrementalBuilder(
		wasmsnapshot.NewTiers(false), wasmsnapshot.EncodeOptions{})
	require.NoError(t, err)

	pr, pw := io.Pipe()
	producer := &pipeProducer{
		w: pw, chunks: chunks,
		observe: func() int { return int(atomic.LoadInt64(&snapshots)) },
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go producer.run(&wg)

	res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			if err := builder.AddPoint(wasmsnapshot.QuiescentPoint{
				Ordinal: p, ExportsBefore: p, CrossingSeq: -1,
			}); err != nil {
				return err
			}
			snap, err := wasmsnapshot.Capture(mod, p)
			if err != nil {
				return err
			}
			if err := builder.Add(snap); err != nil {
				return err
			}
			atomic.AddInt64(&snapshots, 1)
			return nil
		},
	}, boundarylog.NewStreamReader(pr))
	require.NoError(t, err)
	wg.Wait()
	require.NoError(t, producer.err)
	require.Nil(t, res.Truncation)

	require.Equal(t, len(callArgs), res.ExportCalls)
	require.Equal(t, len(callArgs)+1, builder.SnapshotCount())

	// The timing assertion. `observed[k]` was sampled after chunk k's write
	// completed, which cannot happen before the consumer asked for group k.
	require.Equal(t, len(chunks), len(producer.observed))
	for k := 0; k < len(callArgs); k++ {
		require.True(t, producer.observed[k] >= k+1,
			"by the time the producer had written call %d, %d snapshot(s) existed; "+
				"at least %d must, or snapshots are not being derived during the stream "+
				"(all samples: %v)", k, producer.observed[k], k+1, producer.observed)
	}
}

// TestStreamingReplayProducesTheSameTraceAsBatchReplay: streaming must be a
// change of *when*, not of *what*. The two containers are compared stream by
// stream, byte for byte.
func TestStreamingReplayProducesTheSameTraceAsBatchReplay(t *testing.T) {
	for _, tc := range []struct {
		name, wasm string
		build      func(t *testing.T, dir string) string
	}{
		{"balance_calc", balanceCalcWasm, func(t *testing.T, dir string) string {
			return boundarylog.BuildComputeBalanceRecording(t, dir, callArgs)
		}},
		{"grow_mem", growMemWasm, func(t *testing.T, dir string) string {
			return boundarylog.BuildGrowMemRecording(t, dir, growMemCalls)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctDir := tc.build(t, t.TempDir())
			work := t.TempDir()
			program := "streamed.wasm"

			// Batch.
			hb := newHarnessFor(t, tc.wasm, ctDir)
			bw := tracewriter.NewCtfsTraceWriter()
			_, err := boundarylog.Replay(hb.ctx, boundarylog.Options{
				Runtime: hb.rt, Compiled: hb.compiled, Recording: hb.rec,
				ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: bw,
			})
			require.NoError(t, err)
			batch := produceAs(t, bw, filepath.Join(work, "batch"), program)

			// Streamed, one chunk at a time through a synchronous pipe.
			ctx, rt, compiled, rec := streamHarness(t, tc.wasm, ctDir)
			sw := tracewriter.NewCtfsTraceWriter()
			pr, pw := io.Pipe()
			producer := &pipeProducer{
				w: pw, chunks: boundarylog.StreamChunksForRecording(t, ctDir),
				observe: func() int { return 0 },
			}
			var wg sync.WaitGroup
			wg.Add(1)
			go producer.run(&wg)

			res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
				Runtime: rt, Compiled: compiled, Recording: rec,
				ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: sw,
			}, boundarylog.NewStreamReader(pr))
			require.NoError(t, err)
			wg.Wait()
			require.NoError(t, producer.err)
			require.Nil(t, res.Truncation)
			require.Equal(t, len(hb.rec.TopLevelExports()), res.ExportCalls)
			streamed := produceAs(t, sw, filepath.Join(work, "streamed"), program)

			compareContainers(t, batch, streamed, "batch replay", "streaming replay")

			// And the recording the streaming driver assembled is the one the
			// batch parser recovers.
			require.Equal(t, hb.rec.Crossings, rec.Crossings)
		})
	}
}

// TestStreamingSlicesAreSealedBeforeTheRecordingEnds is §2 and §7 together: the
// full pipeline of `MCR-Client-Side-Splitting.md`'s timeline, where a slice
// container is on disk and ready to upload while the page is still running.
//
// The producer samples the number of sealed slices after each write, so the
// assertion is again about timing rather than about the final state.
func TestStreamingSlicesAreSealedBeforeTheRecordingEnds(t *testing.T) {
	ctDir := boundarylog.BuildGrowMemRecording(t, t.TempDir(), growMemCalls)
	chunks := boundarylog.StreamChunksForRecording(t, ctDir)
	require.Equal(t, len(growMemCalls)+1, len(chunks))

	ctx, rt, compiled, rec := streamHarness(t, growMemWasm, ctDir)
	dir := filepath.Join(t.TempDir(), "slices")

	var sealed int64
	var sealedFiles []string
	sw, err := wasmsnapshot.NewSliceWriter(wasmsnapshot.SliceOptions{
		Dir:         dir,
		Program:     "grow_mem.wasm",
		Workdir:     "/fixed/workdir",
		Policy:      wasmsnapshot.SlicePolicy{EveryPoints: 3},
		NewRecorder: func() tracewriter.TraceRecorder { return tracewriter.NewCtfsTraceWriter() },
		OnSealed: func(info wasmsnapshot.SliceInfo) error {
			if _, err := os.Stat(filepath.Join(dir, info.File)); err != nil {
				return fmt.Errorf("slice %d handed over before it existed: %w", info.Index, err)
			}
			sealedFiles = append(sealedFiles, info.File)
			atomic.AddInt64(&sealed, 1)
			return nil
		},
	})
	require.NoError(t, err)

	pr, pw := io.Pipe()
	producer := &pipeProducer{
		w: pw, chunks: chunks,
		observe: func() int { return int(atomic.LoadInt64(&sealed)) },
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go producer.run(&wg)

	res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: sw.AtQuiescentPoint,
	}, boundarylog.NewStreamReader(pr))
	require.NoError(t, err)
	wg.Wait()
	require.NoError(t, producer.err)
	require.Nil(t, res.Truncation)

	m, _, err := sw.Finish()
	require.NoError(t, err)
	require.Equal(t, 4, len(m.Slices)) // twelve calls, sealed every three

	// Slice 0 was sealed while the producer still had calls to write: after
	// chunk 2's write completed (the third call), the replay had passed
	// quiescent point 3 and sealed it.
	require.True(t, producer.observed[3] >= 1,
		"no slice existed after the producer had written 4 of %d calls; slices are "+
			"not being sealed during the recording (samples: %v)",
		len(growMemCalls), producer.observed)
	require.True(t, producer.observed[len(chunks)-2] >= 3,
		"only %d slice(s) existed by the producer's last call (samples: %v)",
		producer.observed[len(chunks)-2], producer.observed)

	// Every slice sealed mid-stream is a complete, independently materialisable
	// container — checked here, not merely assumed, because the whole point of
	// sealing early is that the file is ready to upload at that moment.
	for i, f := range sealedFiles {
		set, diag, err := wasmsnapshot.Load(filepath.Join(dir, f), false)
		require.NoError(t, err)
		require.Equal(t, "", diag)
		base, _, hasBase := set.Range()
		require.True(t, hasBase, "slice %d has no base snapshot", i)
		r, ok := set.Nearest(base)
		require.True(t, ok)
		_, err = set.Snapshot(r)
		require.NoError(t, err)
	}
}

// TestStreamingReplayReportsATruncatedProducer: a page that is killed rather
// than unloaded leaves a prefix. The replay keeps everything the prefix earned
// — the trace of the calls that completed and the snapshots taken after them —
// and reports the truncation rather than either failing or hiding it.
func TestStreamingReplayReportsATruncatedProducer(t *testing.T) {
	ctDir := boundarylog.BuildGrowMemRecording(t, t.TempDir(), growMemCalls)
	chunks := boundarylog.StreamChunksForRecording(t, ctDir)

	const stopAfter = 5
	ctx, rt, compiled, rec := streamHarness(t, growMemWasm, ctDir)

	var points []int
	pr, pw := io.Pipe()
	producer := &pipeProducer{
		w: pw, chunks: chunks, stopAfter: stopAfter,
		observe: func() int { return 0 },
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go producer.run(&wg)

	res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			points = append(points, p)
			return nil
		},
	}, boundarylog.NewStreamReader(pr))
	require.NoError(t, err, "a truncated producer is reported, not returned as an error")
	wg.Wait()

	require.NotNil(t, res.Truncation)
	require.Equal(t, boundarylog.TruncatedUnterminated, res.Truncation.Kind)
	require.Equal(t, stopAfter, res.ExportCalls)
	require.Equal(t, stopAfter, res.ToPoint)
	// Point 0 plus one per completed call: every one of them is a real
	// quiescent point of a real execution, so a snapshot taken at it is valid.
	require.Equal(t, stopAfter+1, len(points))
	require.Equal(t, stopAfter, points[len(points)-1])
	require.Equal(t, stopAfter, len(rec.Crossings))
}

// TestStreamingReplayRefusesASeek: the seek fields describe a range of a
// recording that exists. While one is being produced, honouring them silently
// would be a lie about what was materialised.
func TestStreamingReplayRefusesASeek(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	ctx, rt, compiled, rec := streamHarness(t, balanceCalcWasm, ctDir)

	_, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		FromPoint:    2,
		Resume:       func(api.Module) error { return nil },
	}, boundarylog.NewStreamReader(nopReader{}))
	require.Error(t, err)
	require.True(t, errors.Is(err, err))
	require.Contains(t, err.Error(), "not")
}

// TestStreamingReplayRefusesAPrefilledRecording: the driver appends the
// crossings it consumes, so a recording that already carries some would be
// replayed with a duplicated prefix.
func TestStreamingReplayRefusesAPrefilledRecording(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	h := newHarness(t, ctDir)
	_, err := boundarylog.StreamingReplay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	}, boundarylog.NewStreamReader(nopReader{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already carries")
}

// nopReader is an immediately-empty stream, for the argument-validation tests
// that must fail before any byte is read.
type nopReader struct{}

func (nopReader) Read([]byte) (int, error) { return 0, io.EOF }

// TestStreamingReplayFollowsAGrowingFile exercises `FollowFile` — the adapter
// for a producer that appends to a file rather than offering a pipe — with a
// real file on disk.
//
// **This is not the shape `record-web` has today.** Its
// `JsonFileCtfsWriter::flush` (`codetracer/src/backend-manager/src/
// browser_stream_host.rs`) buffers every event in memory and writes
// `trace.json` with a single `fs::write`, in a one-shot flush guarded by
// `session_ended`; the file does not exist until the session ends and is never
// appended to. So `FollowFile` is the adapter for a producer that *could* be
// changed to append, not a way to consume the daemon as it stands — feeding the
// streaming path needs the writer to serialise incrementally either way. See
// the milestone's "What the `record-web` daemon must do" note.
func TestStreamingReplayFollowsAGrowingFile(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	chunks := boundarylog.StreamChunksForRecording(t, ctDir)

	live := filepath.Join(t.TempDir(), "live.ct")
	require.NoError(t, os.MkdirAll(live, 0o755))
	tracePath := filepath.Join(live, "trace.json")
	require.NoError(t, os.WriteFile(tracePath, nil, 0o644))

	ctx, rt, compiled, rec := streamHarness(t, balanceCalcWasm, live)

	done := make(chan struct{})
	src, err := boundarylog.FollowFile(tracePath, done)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(tracePath, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		for _, c := range chunks {
			if _, err := f.Write(c); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		close(done)
	}()

	var points []int
	res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			points = append(points, p)
			return nil
		},
	}, boundarylog.NewStreamReader(src))
	wg.Wait()
	require.NoError(t, err)
	require.Nil(t, res.Truncation)
	require.Equal(t, len(callArgs), res.ExportCalls)
	require.Equal(t, len(callArgs)+1, len(points))
}

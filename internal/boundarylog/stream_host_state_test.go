// Host-supplied state over the streaming replay path — M44b.
//
// Spec §3.3 state is only known at the module's first exported call, which
// happens *after* the daemon opened the stream and spawned its consumer, so it
// cannot be metadata the consumer reads at startup. It rides in the boundary
// log itself, as host-state records. These tests drive the consequence for a
// streaming replay, over the `vault_apply` corpus recording — a real
// headless-Chromium recording of a module that imports its memory, reads
// its calldata out of it, and calls a host function that answers by
// writing into it.
//
// NO MOCKS. The recording is committed browser output; the modules, the
// interpreter, the CTFS writer and the `.ct` containers are all real. The
// property under test is byte-level equality of two materialised traces,
// which a stubbed writer could not express.
package boundarylog_test

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
	"github.com/tetratelabs/wazero/tracewriter"
)

const (
	vaultApplyWasm      = corpusDir + "/vault_apply/vault_apply.wasm"
	vaultApplyRecording = corpusDir + "/vault_apply/vault-apply.ct"
	vaultApplyProgram   = "vault_apply.wasm"
	// The recording's shape: three exported `apply_slot` calls, each
	// making one `host.fetch_rate` call.
	vaultApplyExports = 3
	vaultApplyImports = 3
)

// withoutHostStateRecords copies the recording with every host-state record
// of `kind` removed, and fails if there was none to remove.
//
// The negative controls below need the replay to diverge on a *missing
// input*, so the removal works on whole frames and cannot leave a malformed
// log behind.
func withoutHostStateRecords(t *testing.T, dst, kind string) string {
	t.Helper()
	out, dropped := boundarylog.CopyTestRecording(t, vaultApplyRecording, dst,
		`"boundary_id":"wasm-host-state"`, `"record":"`+kind+`"`)
	require.True(t, dropped > 0,
		"no %q host-state record was found to withhold — the fixture has "+
			"changed and this negative control is no longer testing anything", kind)
	return out
}

// streamContainer materialises a whole recording through the STREAMING
// driver and returns the produced `.ct`.
func streamContainer(t *testing.T, ctDir, outDir string) (string, boundarylog.StreamResult) {
	t.Helper()
	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, ctDir)
	raw, err := boundarylog.ReadTestLogFile(ctDir)
	require.NoError(t, err)

	w := tracewriter.NewCtfsTraceWriter()
	res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		Recorder:     w,
	}, boundarylog.NewStreamReader(bytes.NewReader(raw)))
	require.NoError(t, err)
	return produceAs(t, w, outDir, vaultApplyProgram), res
}

// batchContainer materialises the same recording through the BATCH driver.
func batchContainer(t *testing.T, ctDir, outDir string) string {
	t.Helper()
	h := newHarnessFor(t, vaultApplyWasm, ctDir)
	w := tracewriter.NewCtfsTraceWriter()
	res, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		Recorder:     w,
	})
	require.NoError(t, err)
	require.Equal(t, vaultApplyExports, res.ExportCalls)
	require.Equal(t, vaultApplyImports, res.ImportCalls)
	return produceAs(t, w, outDir, vaultApplyProgram)
}

// TestStreamingReplayAppliesHostState is M44b's
// `verify_streaming_replay_applies_host_state`: the container the streaming
// path materialises is byte-identical to the one the batch path
// materialises from the same recording. The recording's host state reaches
// the streaming driver only through the records as they arrive, which is
// the shape a recording has while it is still being produced.
func TestStreamingReplayAppliesHostState(t *testing.T) {
	work := t.TempDir()

	want := batchContainer(t, vaultApplyRecording, filepath.Join(work, "batch"))
	got, res := streamContainer(t, vaultApplyRecording, filepath.Join(work, "streamed"))
	require.Equal(t, vaultApplyExports, res.ExportCalls)
	require.Equal(t, vaultApplyImports, res.ImportCalls)
	require.Nil(t, res.Truncation)

	compareContainers(t, want, got, "batch replay", "streaming replay")
	requireNonEmptyTraceStreams(t, got)
}

// TestStreamingReplayWithoutTheInitialStateRecordDiverges is the first of
// two negative controls, and it is what earns the test above.
//
// With the §3.3 record withheld the module reads `key = 0` out of a zeroed memory and passes it to the
// host, so the very first import call diverges. The same withholding on
// the batch path produces the same class of failure, which is the point:
// the two drivers must be equally strict about a missing input.
func TestStreamingReplayWithoutTheInitialStateRecordDiverges(t *testing.T) {
	work := t.TempDir()
	stripped := withoutHostStateRecords(t, filepath.Join(work, "no-initial.ct"), "initial")

	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, stripped)
	raw, err := boundarylog.ReadTestLogFile(stripped)
	require.NoError(t, err)

	_, err = boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	}, boundarylog.NewStreamReader(bytes.NewReader(raw)))
	require.Error(t, err,
		"replaying without the spec §3.3 record must not succeed")
	// The module imports a memory nothing describes, so the refusal is
	// `checkImportedMemories`' — the same one that used to fire for EVERY
	// imported-memory module on this path, and which now fires only when
	// the state is genuinely absent.
	require.True(t,
		strings.Contains(err.Error(), "carries") &&
			strings.Contains(err.Error(), "no initial contents for it"),
		"expected the imported-memory refusal, got: %v", err)
}

// TestStreamingReplayWithoutTheMutationRecordsDiverges is the second
// negative control, and the sharper one.
//
// Withholding the §3.4 mutations leaves a *valid* recording whose module
// starts from the right memory — so it replays, and answers wrongly. It
// must therefore be caught as a divergence on the export's return value,
// not as a missing input. `468000` is the answer with the recorded rate
// applied; `480000` is the principal with a zero rate, which is exactly
// the wrong-but-plausible trace that would have been materialised if a
// divergence were a warning.
func TestStreamingReplayWithoutTheMutationRecordsDiverges(t *testing.T) {
	work := t.TempDir()
	stripped := withoutHostStateRecords(t, filepath.Join(work, "no-mutations.ct"), "mutation")

	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, stripped)
	raw, err := boundarylog.ReadTestLogFile(stripped)
	require.NoError(t, err)

	_, err = boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	}, boundarylog.NewStreamReader(bytes.NewReader(raw)))
	require.Error(t, err, "replaying without the spec §3.4 records must not succeed")
	var div *boundarylog.DivergenceError
	require.True(t, errorsAs(err, &div),
		"withholding the host's writes must be a divergence, got %T: %v", err, err)
	require.True(t, strings.Contains(err.Error(), "480000"),
		"the diagnostic must name the wrong-but-plausible answer the module "+
			"computed from a zero rate; got: %v", err)
}

// TestSnapshotsAreDerivedDuringAnImportedMemoryRecording closes M44b's
// fourth deliverable: snapshot derivation *during* recording, for the
// module class it was previously impossible for.
//
// The timing is read off the producer's own progress, exactly as
// `TestSnapshotsAreEmittedWhileTheStreamIsStillArriving` reads it for
// `balance_calc`: an `io.Pipe` write returns only once a reader has taken
// the bytes, the reader takes chunk k only when the driver asks for call
// group k, and the driver asks for group k only after the quiescent-point
// hook for point k has run. So by the time the producer's k-th write
// returns, snapshots 0..k must already exist.
//
// The distinction from that test is the module: `balance_calc` defines its
// own memory, so its recording needs no §3.3 state and the streaming path
// could always serve it. This one imports its memory, which is what the
// streaming path refused outright before M44b — so a snapshot taken here
// is one that could not previously be taken at all, not merely one taken
// earlier.
func TestSnapshotsAreDerivedDuringAnImportedMemoryRecording(t *testing.T) {
	live := vaultApplyRecording
	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, live)

	var taken []int
	pr, pw := io.Pipe()
	producer := &pipeProducer{
		w:       pw,
		chunks:  boundarylog.StreamChunksForRecording(t, live),
		observe: func() int { return len(taken) },
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go producer.run(&wg)

	res, err := boundarylog.StreamingReplay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(point int, mod api.Module) error {
			snap, err := wasmsnapshot.Capture(mod, point)
			if err != nil {
				return err
			}
			require.True(t, snap.MemoryBytes > 0,
				"a snapshot of an imported-memory module must carry its memory")
			taken = append(taken, point)
			return nil
		},
	}, boundarylog.NewStreamReader(pr))
	require.NoError(t, err)
	wg.Wait()
	require.NoError(t, producer.err)

	require.Equal(t, vaultApplyExports, res.ExportCalls)
	// One quiescent point before the first call and one after each.
	require.Equal(t, vaultApplyExports+1, len(taken))

	// The bound starts at k=1, and the reason is the mechanism this test
	// exists for. For a module that imports its memory, quiescent point 0
	// CANNOT precede the first chunk: the spec §3.3 record that says what
	// the memory contains arrives inside that chunk, so instantiation waits
	// for it. Write #0 therefore returns before snapshot 0 is taken, and
	// asserting `observed[0] >= 1` would be asserting the deferral does not
	// happen. From write #1 on the ordinary bound applies and is exactly
	// what "during the recording" means: the producer is still writing
	// chunk k when snapshots 0..k already exist on this side.
	require.True(t, len(producer.observed) >= vaultApplyExports,
		"the producer wrote only %d chunk(s); the timing bound below has "+
			"nothing to check", len(producer.observed))
	for k := 1; k < vaultApplyExports; k++ {
		require.True(t, producer.observed[k] >= k+1,
			"by the producer's write #%d only %d snapshot(s) had been taken; "+
				"derivation is not keeping up with the stream, so it is not "+
				"happening DURING the recording", k, producer.observed[k])
	}
}

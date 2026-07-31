// Host-supplied state over the streaming replay path — M44b.
//
// `--boundary-log` (batch) reads the spec §3.3 / §3.4 state from the
// `boundary_state.json` sidecar at startup. `--boundary-stream` reads
// `LoadRecordingMetadata` once, at startup too — and while a recording is
// still being produced the sidecar cannot exist yet, because §3.3 is only
// known at the module's first exported call, which happens *after* the
// daemon opened the stream and spawned its consumer. So the streaming path
// refused every module whose linear memory is imported: every Stylus
// contract and every `wasm-bindgen`-style glue layer, which is exactly the
// class of module snapshot derivation during recording exists for.
//
// M44b carries the two records in the stream itself. These tests drive the
// consequence, over the `vault_apply` corpus recording — a real
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
	"encoding/json"
	"io"
	"os"
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

// copyRecording copies a `.ct` directory, optionally rewriting
// `trace.json` and optionally dropping the sidecar.
//
// Dropping the sidecar is how the *live* shape is reproduced from a
// finished recording: a still-growing `.ct` has a `trace.json` and no
// `boundary_state.json`, because the daemon writes the sidecar as a
// rendering of records it has already put into the stream.
func copyRecording(
	t *testing.T, src, dst string, dropSidecar bool, rewrite func([]byte) []byte,
) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dst, 0o755))
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if dropSidecar && e.Name() == "boundary_state.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		if e.Name() == "trace.json" && rewrite != nil {
			data = rewrite(data)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600))
	}
	return dst
}

// withoutHostStateRecords returns a `trace.json` with every in-stream
// host-state record of `kind` removed.
//
// It works on the decoded record array rather than on the raw bytes so a
// removal cannot leave a malformed document behind — the point of each
// negative control is that the replay diverges on a *missing input*, not
// that it fails to parse.
func withoutHostStateRecords(t *testing.T, kind string) func([]byte) []byte {
	t.Helper()
	return func(raw []byte) []byte {
		var records []json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &records))
		kept := make([]json.RawMessage, 0, len(records))
		dropped := 0
		for _, r := range records {
			if strings.Contains(string(r), `\"boundary_id\":\"wasm-host-state\"`) &&
				strings.Contains(string(r), `\"record\":\"`+kind+`\"`) {
				dropped++
				continue
			}
			kept = append(kept, r)
		}
		require.True(t, dropped > 0,
			"no %q host-state record was found to withhold — the fixture has "+
				"changed and this negative control is no longer testing anything", kind)
		out, err := json.Marshal(kept)
		require.NoError(t, err)
		return out
	}
}

// streamContainer materialises a whole recording through the STREAMING
// driver and returns the produced `.ct`.
func streamContainer(t *testing.T, ctDir, outDir string) (string, boundarylog.StreamResult) {
	t.Helper()
	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, ctDir)
	raw, err := os.ReadFile(filepath.Join(ctDir, "trace.json"))
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
// `verify_streaming_replay_applies_host_state`.
//
// It asserts the property in the shape `TestStreamAgreesWithTheBatchParser`
// asserts its own: the container the streaming path materialises from a
// recording carrying **no sidecar at all** is byte-identical to the one
// the batch path materialises from the same recording *with* its sidecar.
//
// The "no sidecar" copy is the load-bearing half. With the sidecar in
// place the streaming path already worked, because `LoadRecordingMetadata`
// reads it — that is what a *finished* recording being streamed from a
// file looks like. What never worked, and what the whole streaming
// pipeline was excluded from, is the shape a recording has while it is
// still being produced: a growing `trace.json` and nothing else.
func TestStreamingReplayAppliesHostState(t *testing.T) {
	work := t.TempDir()

	// The batch reference, from the committed recording as it stands.
	want := batchContainer(t, vaultApplyRecording, filepath.Join(work, "batch"))

	// The live shape: everything but the sidecar.
	live := copyRecording(t, vaultApplyRecording, filepath.Join(work, "live"), true, nil)
	require.False(t, fileExists(filepath.Join(live, "boundary_state.json")),
		"the live copy must not carry a sidecar, or it proves nothing")

	got, res := streamContainer(t, live, filepath.Join(work, "streamed"))
	require.Equal(t, vaultApplyExports, res.ExportCalls)
	require.Equal(t, vaultApplyImports, res.ImportCalls)
	require.Nil(t, res.Truncation)

	compareContainers(t, want, got,
		"batch replay with the sidecar", "streaming replay with no sidecar")
	requireNonEmptyTraceStreams(t, got)
}

// TestStreamingReplayWithoutTheInitialStateRecordDiverges is the first of
// two negative controls, and it is what earns the test above.
//
// With the §3.3 record withheld — from the stream *and* from the sidecar —
// the module reads `key = 0` out of a zeroed memory and passes it to the
// host, so the very first import call diverges. The same withholding on
// the batch path produces the same class of failure, which is the point:
// the two drivers must be equally strict about a missing input.
func TestStreamingReplayWithoutTheInitialStateRecordDiverges(t *testing.T) {
	work := t.TempDir()
	stripped := copyRecording(t, vaultApplyRecording, filepath.Join(work, "no-initial"),
		true, withoutHostStateRecords(t, "initial"))

	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, stripped)
	raw, err := os.ReadFile(filepath.Join(stripped, "trace.json"))
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
	stripped := copyRecording(t, vaultApplyRecording, filepath.Join(work, "no-mutations"),
		true, withoutHostStateRecords(t, "mutation"))

	ctx, rt, compiled, rec := streamHarness(t, vaultApplyWasm, stripped)
	raw, err := os.ReadFile(filepath.Join(stripped, "trace.json"))
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

// TestTheSidecarIsARenderingOfTheStream pins the invariant that lets both
// carriers exist at once.
//
// A recording made by a current producer carries its host state twice.
// The sidecar is rendered from the same records the stream carries, so the
// two must agree — and a recording whose two carriers disagree is refused
// rather than resolved, because picking one would serve two different
// programs to the two drivers from one recording.
func TestTheSidecarIsARenderingOfTheStream(t *testing.T) {
	work := t.TempDir()

	// (1) As committed: both carriers, and they agree.
	h := newHarnessFor(t, vaultApplyWasm, vaultApplyRecording)
	require.NotNil(t, h.rec.HostState)
	require.Equal(t, 1, len(h.rec.HostState.Initial.Memories))
	require.Equal(t, vaultApplyImports, len(h.rec.HostState.Mutations))

	// (2) Perturbed sidecar: a hard error naming both renderings.
	bad := copyRecording(t, vaultApplyRecording, filepath.Join(work, "bad"), false, nil)
	sidecar := filepath.Join(bad, "boundary_state.json")
	data, err := os.ReadFile(sidecar)
	require.NoError(t, err)
	// Move the first mutation to a different crossing. The document stays
	// valid, so this is a disagreement rather than a decode failure.
	perturbed := bytes.Replace(data,
		[]byte(`"afterCrossing":1`), []byte(`"afterCrossing":3`), 1)
	require.False(t, bytes.Equal(data, perturbed),
		"the sidecar no longer contains the anchor this control perturbs")
	require.NoError(t, os.WriteFile(sidecar, perturbed, 0o600))

	_, err = boundarylog.LoadRecording(bad)
	require.Error(t, err, "a recording whose two host-state carriers disagree must be refused")
	require.True(t, strings.Contains(err.Error(), "disagrees with the host-state records"),
		"expected the reconciliation refusal, got: %v", err)
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
	work := t.TempDir()
	// The live shape again: a growing `trace.json` and no sidecar.
	live := copyRecording(t, vaultApplyRecording, filepath.Join(work, "live"), true, nil)

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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/tracewriter"
)

// boundaryReplayRequest bundles everything `doBoundaryLogReplay` needs, so
// the long `doRun` call site stays readable.
type boundaryReplayRequest struct {
	runtime      wazero.Runtime
	compiled     wazero.CompiledModule
	moduleConfig wazero.ModuleConfig
	recorder     tracewriter.TraceRecorder
	// outDir is where the CTFS bundle is written; empty means "check the
	// replay but produce no trace".
	outDir string
	// traceName is the program name the bundle is written under.
	traceName string
	// wasmPath is the module path, used to find the sidecar manifest.
	wasmPath string
	// logPath is the `--boundary-log` argument.
	logPath string
	// streamPath is the `--boundary-stream` argument: "-" to read the
	// boundary log from stdin as the producer writes it. Empty means the
	// recording is complete and is read from its `.ct` in one pass.
	streamPath string
	// stdin is the stream source for `--boundary-stream -`.
	stdin io.Reader
	// manifestPath is the `--boundary-manifest` argument; empty means
	// "discover the conventional sidecar".
	manifestPath string
	// snapshots carries the `--snapshots` / `--seek-*` surface. Whether it
	// can be honoured at all depends on the build variant — see
	// `snapshots.go` / `snapshots_disabled.go`.
	snapshots snapshotOptions
}

// doBoundaryLogReplay implements `--boundary-log`: re-execute the original
// module against a recorded boundary log and materialise a CTFS trace.
//
// This is the consumer half of the boundary-capture model specified in
// `codetracer-specs/Recording-Backends/WASM-Instrumentation-Layer.md` §6.
// The heavy lifting lives in `internal/boundarylog`; this function is the
// CLI adapter around it.
//
// # Trace-on-divergence policy
//
// Spec §6 makes a divergence a hard error, "never a warning". This function
// writes the trace ONLY on a fully successful replay. That is deliberate
// and load-bearing: a trace materialised from a diverged replay would
// describe an execution that never happened, and would be indistinguishable
// on disk from a faithful one. The trace writer buffers everything in
// memory until `ProduceTrace`, so simply not calling it leaves nothing
// partial behind.
func doBoundaryLogReplay(ctx context.Context, req boundaryReplayRequest, stdOut io.Writer, stdErr io.Writer) int {
	streaming := req.streamPath != ""

	var recording *boundarylog.Recording
	var stream *boundarylog.StreamReader
	var err error
	if streaming {
		src, serr := openBoundaryStream(req)
		if serr != nil {
			fmt.Fprintf(stdErr, "%v\n", serr)
			return 1
		}
		// A streaming replay starts from the recording's *metadata* only —
		// the crossings are what the stream delivers, and waiting for them
		// all would be the pass at the end that snapshot spec §2 exists to
		// avoid. The metadata is the stream's own Header frame, which the
		// producer sends before anything else.
		stream = boundarylog.NewStreamReader(src)
		recording, err = stream.ReadHeader("<stdin>")
	} else {
		recording, err = boundarylog.LoadRecording(req.logPath)
	}
	if err != nil {
		fmt.Fprintf(stdErr, "error reading boundary log: %v\n", err)
		return 1
	}

	manifestPath := req.manifestPath
	if manifestPath == "" {
		// Discovery is best-effort: the spec (§6) treats "a boundary
		// recording plus the original .wasm" as a complete input, and the
		// signatures can be read off the module itself. The manifest is a
		// cross-check when it happens to be there.
		manifestPath = boundarylog.FindManifest(req.wasmPath)
	}
	var manifest *boundarylog.Manifest
	if manifestPath != "" {
		manifest, err = boundarylog.LoadManifest(manifestPath)
		if err != nil {
			fmt.Fprintf(stdErr, "error reading instrumentation manifest: %v\n", err)
			return 1
		}
	}

	// Replay drives the recorded exported calls itself, so no start
	// function may run: `_start` would execute the program a second time,
	// off the recording's rails.
	cfg := req.moduleConfig.WithStartFunctions()

	opts := boundarylog.Options{
		Runtime:      req.runtime,
		Compiled:     req.compiled,
		Recording:    recording,
		Manifest:     manifest,
		ModuleConfig: cfg,
		Recorder:     req.recorder,
	}

	// Snapshot derivation and seeking (`WASM-Replay-Snapshots-And-Slices.md`).
	// `configure` installs the quiescent-point hook and, when seeking, the
	// state restore; `finishSnapshots` runs after the container exists,
	// because derived snapshots go inside it (§6) and it is only written on a
	// fully successful replay.
	if streaming && req.snapshots.seeking() {
		fmt.Fprintln(stdErr,
			"--boundary-stream and --seek-from are incompatible: seeking materialises a "+
				"range of a recording that already exists, and a stream is one that does "+
				"not yet")
		return 1
	}

	plan, err := req.snapshots.configure(recording, &opts, stdOut, stdErr)
	if err != nil {
		fmt.Fprintf(stdErr, "%v\n", err)
		return 1
	}

	var result boundarylog.Result
	if streaming {
		streamed, err := boundarylog.StreamingReplay(ctx, opts, stream)
		if err != nil {
			fmt.Fprintf(stdErr, "%v\n", err)
			return 1
		}
		if streamed.Truncation != nil {
			// Not a failure. Everything replayed before the cut came from
			// complete crossings, so the trace and the snapshots taken for it
			// are faithful; the recording is a prefix and the user is told so.
			fmt.Fprintf(stdErr, "warning: %v\n", streamed.Truncation)
		}
		result = streamed.Result
	} else {
		result, err = boundarylog.Replay(ctx, opts)
		if err != nil {
			fmt.Fprintf(stdErr, "%v\n", err)
			return 1
		}
	}

	if result.UncheckedImportCalls > 0 {
		// Not a failure, but the user should know a part of the replay was
		// taken on trust — and, since M39, why.
		//
		// A `() -> ()` import contributes no boundary values and no
		// Call/Return record, so its realm markers are its whole trace on
		// disk. A recording made before M39 spelled those markers the same
		// as an export's, so they cannot be attributed and no crossing is
		// recovered.
		//
		// The note must NOT assert the recording's age, because the witness
		// cannot establish it: `MarkersIdentifyImports` is false both for a
		// pre-M39 recording and for a current one that happens to carry no
		// import crossing at all, and the two are indistinguishable from the
		// records. Saying "this recording is old" to someone who just made it
		// would send them re-recording a page that is already current.
		fmt.Fprintf(stdErr,
			"note: %d call(s) to imports with an empty `() -> ()` signature were "+
				"replayed unchecked. This boundary log carries no realm marker that "+
				"names an import edge, so such a crossing leaves nothing on disk that "+
				"can be matched to it — either it was recorded before the recorder "+
				"named the two edges apart, or it contains no imported call at all. "+
				"Re-record the page with a current `browser_session.js` to have these "+
				"calls checked\n",
			result.UncheckedImportCalls)
	}

	fmt.Fprintf(stdOut, "replayed %d exported call(s) and %d imported call(s) from %s\n",
		result.ExportCalls, result.ImportCalls, req.logPath)
	if result.FromPoint != 0 || result.ToPoint != len(recording.TopLevelExports()) {
		fmt.Fprintf(stdOut, "materialised quiescent-point range [%d,%d] of 0..%d\n",
			result.FromPoint, result.ToPoint, len(recording.TopLevelExports()))
	}

	if plan.slicing {
		// The trace was written as slice containers, one per range, by the
		// per-slice recorders the plan installed. There is no whole-trace
		// recorder to drain, and `--out-dir` names no container.
		if err := plan.finish(""); err != nil {
			fmt.Fprintf(stdErr, "producing slices: %v\n", err)
			return 1
		}
		return 0
	}

	if !produceTrace(req.outDir, req.traceName, req.recorder, stdErr) {
		return 1
	}

	if req.outDir != "" {
		containerPath, err := containerPathFor(req.outDir, req.traceName)
		if err != nil {
			fmt.Fprintf(stdErr, "%v\n", err)
			return 1
		}
		if err := plan.finish(containerPath); err != nil {
			fmt.Fprintf(stdErr, "deriving replay snapshots: %v\n", err)
			return 1
		}
	} else if err := plan.finish(""); err != nil {
		// With no --out-dir there is no container to attach snapshots to.
		// Reporting rather than silently dropping them keeps the failure
		// visible: derived-and-discarded looks identical to never-derived.
		fmt.Fprintf(stdErr, "%v\n", err)
		return 1
	}
	return 0
}

// openBoundaryStream resolves `--boundary-stream` into a reader whose `io.EOF`
// means "the producer has finished".
//
// The only shape is `-`: the CTBL boundary log on **stdin**, which is what
// `record-web --snapshot-consumer` writes. Closing the pipe is an unambiguous
// end of stream, and backpressure comes for free — the replayer reads only
// between exported calls, so a producer that outruns it blocks on the pipe
// rather than queueing without bound. The stream carries its own Header, so
// `--boundary-log` is refused alongside it rather than silently ignored.
func openBoundaryStream(req boundaryReplayRequest) (io.Reader, error) {
	if req.logPath != "" {
		return nil, fmt.Errorf(
			"--boundary-stream reads the whole recording, metadata included, from " +
				"the stream; drop --boundary-log, which names a finished .ct")
	}
	if req.streamPath != "-" {
		return nil, fmt.Errorf(
			"--boundary-stream %s: a boundary stream is read from stdin "+
				"(`--boundary-stream -`), where closing the pipe ends it; following a "+
				"file is not supported", req.streamPath)
	}
	if req.stdin == nil {
		return nil, fmt.Errorf("--boundary-stream - needs stdin")
	}
	return req.stdin, nil
}

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

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
	// recording's `trace.json` from stdin as the producer writes it, or the
	// path of a file the producer is appending to. Empty means the recording
	// is complete and is read from disk in one pass.
	streamPath string
	// streamDone is the `--stream-done` argument: a marker file whose
	// appearance means the producer has finished. Required when streamPath
	// names a file, meaningless when it is "-".
	streamDone string
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

	// A streaming replay starts from the recording's *metadata* only — the
	// crossings are what the stream delivers, and waiting for them all would
	// be the pass at the end that snapshot spec §2 exists to avoid.
	var recording *boundarylog.Recording
	var err error
	if streaming {
		recording, err = boundarylog.LoadRecordingMetadata(req.logPath)
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
		src, closeSrc, err := openBoundaryStream(req)
		if err != nil {
			fmt.Fprintf(stdErr, "%v\n", err)
			return 1
		}
		defer closeSrc()
		streamed, err := boundarylog.StreamingReplay(ctx, opts, boundarylog.NewStreamReader(src))
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
		// taken on trust: a `() -> ()` import contributes no boundary
		// values and no Call/Return record, so the recording yields no
		// crossing to check the call against.
		fmt.Fprintf(stdErr,
			"note: %d call(s) to imports with an empty `() -> ()` signature were "+
				"replayed unchecked — such a crossing contributes no values to a "+
				"browser boundary log, so there is nothing to check it against\n",
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

	produceTrace(req.outDir, req.traceName, req.recorder)

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
// Two shapes are supported, and the difference matters:
//
//   - `-` reads the recording's `trace.json` from **stdin**. This is the shape
//     a daemon-side tee has: the `record-web` receiver writes the same bytes to
//     the `.ct` and to this process's stdin, and closing the pipe is an
//     unambiguous end of stream. It also gives backpressure for free — the
//     replayer reads only between exported calls, so a producer that outruns it
//     blocks on the pipe rather than queueing without bound.
//   - a path follows a file the producer is appending to. A file has no end of
//     stream, so `--stream-done <marker>` must name a file whose appearance
//     means the producer has stopped. Without it there is no way to tell a
//     recording still in progress from one that is over, and this refuses
//     rather than guessing — guessing wrong in one direction hangs forever and
//     in the other truncates the recording.
func openBoundaryStream(req boundaryReplayRequest) (io.Reader, func(), error) {
	if req.streamPath == "-" {
		if req.stdin == nil {
			return nil, nil, fmt.Errorf("--boundary-stream - needs stdin")
		}
		if req.streamDone != "" {
			return nil, nil, fmt.Errorf(
				"--stream-done has no meaning with --boundary-stream -: closing stdin " +
					"already ends the stream")
		}
		return req.stdin, func() {}, nil
	}
	if req.streamDone == "" {
		return nil, nil, fmt.Errorf(
			"--boundary-stream %s follows a file the producer is still appending to, "+
				"which has no end of stream. Pass --stream-done <marker> naming a file "+
				"the producer creates when it has finished, or use --boundary-stream - "+
				"and pipe the recording in, where closing the pipe ends it",
			req.streamPath)
	}
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(req.streamDone); err == nil {
				close(done)
				return
			}
			select {
			case <-stop:
				// The replay is over; stop watching. `done` is deliberately
				// left open — nothing reads it after this.
				return
			case <-time.After(boundarylog.FollowPoll):
			}
		}
	}()
	src, err := boundarylog.FollowFile(req.streamPath, done)
	if err != nil {
		close(stop)
		return nil, nil, err
	}
	return src, func() {
		close(stop)
		_ = src.Close()
	}, nil
}

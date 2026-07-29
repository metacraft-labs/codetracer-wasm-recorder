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
	// manifestPath is the `--boundary-manifest` argument; empty means
	// "discover the conventional sidecar".
	manifestPath string
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
	recording, err := boundarylog.LoadRecording(req.logPath)
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

	result, err := boundarylog.Replay(ctx, boundarylog.Options{
		Runtime:      req.runtime,
		Compiled:     req.compiled,
		Recording:    recording,
		Manifest:     manifest,
		ModuleConfig: cfg,
		Recorder:     req.recorder,
	})
	if err != nil {
		fmt.Fprintf(stdErr, "%v\n", err)
		return 1
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

	produceTrace(req.outDir, req.traceName, req.recorder)
	return 0
}

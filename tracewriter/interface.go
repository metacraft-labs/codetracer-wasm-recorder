// Package tracewriter provides the trace-recording surface used by the
// wazero CodeTracer fork.
//
// The recorder is CTFS-only per `Recorder-CLI-Conventions.md` §4: every run
// produces a single multi-stream `.ct` container via the C FFI exposed by
// `codetracer-trace-format-nim`'s `codetracer_trace_writer_ffi.nim`.  See
// `ctfs_writer.go` for the cgo binding and `AUDIT-CTFS-2026-05.md`
// (Follow-up A) for the migration record.
//
// Pre-2026-05-08 this package also exposed a Rust FFI writer
// (`RustTraceWriter` / `RustFormat`) and an in-process pure-Go writer
// (`GoWriter`) that wrote the legacy three-file JSON layout
// (`trace.json` + `trace_metadata.json` + `trace_paths.json`).  Both were
// removed by the convention compliance pass — see AUDIT-CTFS-2026-05.md
// for the full record.
package tracewriter

import (
	"github.com/tetratelabs/wazero/internal/tracetypes"
)

// TraceRecorder abstracts the trace-recording surface used by the wazero
// interpreter and Stylus host hooks.  The wazero CodeTracer fork ships a
// single concrete writer (`CtfsTraceWriter`); the interface is preserved so
// alternative back-ends (e.g. an in-memory mock for unit tests) can be
// plugged in without changing the call sites.
type TraceRecorder interface {
	// RegisterStep records a step event at the given source path and line.
	RegisterStep(path string, line tracetypes.Line)

	// RegisterStepWithPathId records a step event using a pre-resolved path ID.
	RegisterStepWithPathId(pathId tracetypes.PathId, line tracetypes.Line)

	// RegisterStepWithColumn records a column-aware step event.  The `column`
	// argument is a 1-based column number; pass `nil` to record a column-less
	// step (back-compat with line-only recorders).  The writer must have been
	// switched into column-aware mode via `EnableColumnAwareSteps` before any
	// column-bearing step is registered.  See the FU-Column-Aware-Nav-Wasm
	// plan in `codetracer-specs/Planned-Features/
	// Column-Aware-Navigation-Other-Languages.plan.md` and the trace-format
	// spec at `codetracer-trace-format-spec/trace-events.md` §"Column
	// Encoding — `DeltaColumn` (chosen)" for the wire encoding contract.
	RegisterStepWithColumn(path string, line tracetypes.Line, column *tracetypes.Line)

	// EnableColumnAwareSteps flips the writer into column-aware mode.  Must
	// be called before the first column-bearing step is registered.  After
	// it returns, the `.ct` container will carry `FLAG_HAS_COLUMN_AWARE_STEPS`
	// in `meta.dat` (bit 4) so downstream readers know to surface columns.
	// Calling it on a writer that has already emitted line-only steps is a
	// no-op preserved here for symmetry with the FFI surface — callers
	// should treat this as "call once at trace start".
	EnableColumnAwareSteps()

	// RegisterPathWithLineLengths registers a source path together with its
	// per-line byte counts so the writer's global position table can decode
	// column-aware steps back into (line, column) pairs at read time.  The
	// `lineLengths` slice is indexed by 0-based line number; pass an empty
	// slice when per-line data isn't available (column resolution then falls
	// back to surfacing `None` at read time).  Outside column-aware mode the
	// `lineLengths` argument is ignored — see the FFI doc-comment on
	// `trace_writer_register_path_with_line_lengths`.
	RegisterPathWithLineLengths(path string, lineLengths []uint32)

	// RegisterCall records a function call event.
	RegisterCall(name string, definitionPath string, definitionLine tracetypes.Line, args []tracetypes.FullValueRecord)

	// RegisterCallWithPathId records a function call using a pre-resolved path ID.
	RegisterCallWithPathId(name string, pathId tracetypes.PathId, line tracetypes.Line, args []tracetypes.FullValueRecord)

	// RegisterReturn records a function return event.
	RegisterReturn(value tracetypes.ValueRecord)

	// RegisterVariable records a variable assignment event.
	RegisterVariable(name string, value tracetypes.ValueRecord)

	// RegisterRecordEvent records a special event (I/O, EVM, error, etc.).
	RegisterRecordEvent(kind tracetypes.RecordEventKind, metadata string, content string)

	// EnsureFunctionId returns the ID for a function, registering it if new.
	EnsureFunctionId(name string, pathId tracetypes.PathId, line tracetypes.Line) tracetypes.FunctionId

	// RegisterFunctionWithNewId registers a new function and returns its ID
	// without checking if it already exists.
	RegisterFunctionWithNewId(name string, pathId tracetypes.PathId, line tracetypes.Line) tracetypes.FunctionId

	// EnsureVariableId returns the ID for a variable name, registering it if new.
	EnsureVariableId(name string) tracetypes.VariableId

	// RegisterVariableNameWithNewId registers a new variable name and returns its ID.
	RegisterVariableNameWithNewId(name string) tracetypes.VariableId

	// EnsurePathId returns the ID for a source file path, registering it if new.
	EnsurePathId(path string) tracetypes.PathId

	// RegisterPathWithNewId registers a new path and returns its ID.
	RegisterPathWithNewId(path string) tracetypes.PathId

	// EnsureTypeId returns the ID for a type, registering it if new.
	EnsureTypeId(name string, typeRecord tracetypes.TypeRecord) tracetypes.TypeId

	// RegisterTypeWithNewId registers a new type and returns its ID.
	RegisterTypeWithNewId(name string, typeRecord tracetypes.TypeRecord) tracetypes.TypeId

	// RegisterFullValue records a full variable value event.
	RegisterFullValue(variableId tracetypes.VariableId, value tracetypes.ValueRecord)

	// ProduceTrace writes the collected trace data to the given directory
	// as a `<program-basename>.ct` (CTFS) container.
	ProduceTrace(traceDir string, programName string, workdir string) error

	// CurrentCallsCount returns the current depth of the call stack.
	CurrentCallsCount() int

	// Arg creates a FullValueRecord for a function argument.
	Arg(name string, value tracetypes.ValueRecord) tracetypes.FullValueRecord
}

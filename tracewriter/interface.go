// Package tracewriter provides an abstraction layer for trace recording in
// the wazero CodeTracer fork.
//
// The recorder is CTFS-only per `Recorder-CLI-Conventions.md` §4: today the
// canonical multi-stream `.ct` bundle is produced through the in-process
// `GoWriter` which delegates to `github.com/metacraft-labs/trace_record`.
// The interface stays writer-agnostic so a future FFI- or Nim-backed CTFS
// implementation can be plugged in without touching call sites.
//
// Pre-2026-05-08 this package also exposed a Rust FFI writer
// (`RustTraceWriter` / `RustFormat`) that selected between
// `FMT_JSON`/`FMT_BINARY_V0`/`FMT_BINARY` via the `--format` CLI flag.  Both
// the Rust writer and the `--format` flag were removed by the convention
// compliance pass — see AUDIT-CTFS-2026-05.md for the full record.
package tracewriter

import (
	"github.com/metacraft-labs/trace_record"
)

// TraceRecorder abstracts the trace-recording surface used by the wazero
// interpreter and Stylus host hooks.  The wazero CodeTracer fork ships a
// single concrete writer (`GoWriter`); the interface is preserved so a
// future FFI- or Nim-backed CTFS implementation can be plugged in without
// changing the call sites.
type TraceRecorder interface {
	// RegisterStep records a step event at the given source path and line.
	RegisterStep(path string, line trace_record.Line)

	// RegisterStepWithPathId records a step event using a pre-resolved path ID.
	RegisterStepWithPathId(pathId trace_record.PathId, line trace_record.Line)

	// RegisterCall records a function call event.
	RegisterCall(name string, definitionPath string, definitionLine trace_record.Line, args []trace_record.FullValueRecord)

	// RegisterCallWithPathId records a function call using a pre-resolved path ID.
	RegisterCallWithPathId(name string, pathId trace_record.PathId, line trace_record.Line, args []trace_record.FullValueRecord)

	// RegisterReturn records a function return event.
	RegisterReturn(value trace_record.ValueRecord)

	// RegisterVariable records a variable assignment event.
	RegisterVariable(name string, value trace_record.ValueRecord)

	// RegisterRecordEvent records a special event (I/O, EVM, error, etc.).
	RegisterRecordEvent(kind trace_record.RecordEventKind, metadata string, content string)

	// EnsureFunctionId returns the ID for a function, registering it if new.
	EnsureFunctionId(name string, pathId trace_record.PathId, line trace_record.Line) trace_record.FunctionId

	// RegisterFunctionWithNewId registers a new function and returns its ID
	// without checking if it already exists.
	RegisterFunctionWithNewId(name string, pathId trace_record.PathId, line trace_record.Line) trace_record.FunctionId

	// EnsureVariableId returns the ID for a variable name, registering it if new.
	EnsureVariableId(name string) trace_record.VariableId

	// RegisterVariableNameWithNewId registers a new variable name and returns its ID.
	RegisterVariableNameWithNewId(name string) trace_record.VariableId

	// EnsurePathId returns the ID for a source file path, registering it if new.
	EnsurePathId(path string) trace_record.PathId

	// RegisterPathWithNewId registers a new path and returns its ID.
	RegisterPathWithNewId(path string) trace_record.PathId

	// EnsureTypeId returns the ID for a type, registering it if new.
	EnsureTypeId(name string, typeRecord trace_record.TypeRecord) trace_record.TypeId

	// RegisterTypeWithNewId registers a new type and returns its ID.
	RegisterTypeWithNewId(name string, typeRecord trace_record.TypeRecord) trace_record.TypeId

	// RegisterFullValue records a full variable value event.
	RegisterFullValue(variableId trace_record.VariableId, value trace_record.ValueRecord)

	// ProduceTrace writes the collected trace data to the given directory.
	ProduceTrace(traceDir string, programName string, workdir string) error

	// CurrentCallsCount returns the current depth of the call stack.
	CurrentCallsCount() int

	// Arg creates a FullValueRecord for a function argument.
	Arg(name string, value trace_record.ValueRecord) trace_record.FullValueRecord
}

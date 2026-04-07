// Package tracewriter provides an abstraction layer for trace recording,
// allowing the wazero recorder to use either a pure Go trace writer
// (trace_record.TraceRecord) or a Rust FFI-based trace writer
// (codetracer_trace_writer_ffi) interchangeably.
package tracewriter

import (
	"github.com/metacraft-labs/trace_record"
)

// TraceRecorder is the interface that abstracts the common API between the
// Go trace_record.TraceRecord and the Rust FFI-based RustTraceWriter.
// Both implementations can be used interchangeably wherever trace recording
// is needed.
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

//go:build cgo

package tracewriter

/*
#cgo LDFLAGS: -lcodetracer_trace_writer_ffi -ldl -lm -lpthread
#include "codetracer_trace_writer.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"github.com/metacraft-labs/trace_record"
)

// --- Internal event types for buffering ---

type rustEventKind int

const (
	rustEventStep rustEventKind = iota
	rustEventCall
	rustEventReturn
	rustEventVariable
	rustEventFullValue
	rustEventRecordEvent
	rustEventFunction
	rustEventPath
	rustEventVariableName
	rustEventType
)

type rustBufferedEvent struct {
	kind rustEventKind

	// Step fields
	pathId trace_record.PathId
	line   trace_record.Line

	// Call fields
	functionId trace_record.FunctionId
	args       []trace_record.FullValueRecord

	// Return fields
	returnValue trace_record.ValueRecord

	// Variable fields
	variableId trace_record.VariableId
	value      trace_record.ValueRecord
	varName    string

	// RecordEvent fields
	recordEventKind trace_record.RecordEventKind
	metadata        string
	content         string

	// Function fields
	funcName string
	funcPath trace_record.PathId
	funcLine trace_record.Line

	// Path fields
	path string

	// Type fields
	typeName   string
	typeRecord trace_record.TypeRecord
}

// RustTraceWriter implements TraceRecorder using the Rust FFI trace writer library.
//
// During recording, events are buffered in memory (same as the Go TraceRecord).
// When ProduceTrace() is called, the Rust writer handle is created and all
// buffered events are replayed through the FFI in the correct phase order
// (metadata -> events -> paths), then the handle is freed.
type RustTraceWriter struct {
	events            []rustBufferedEvent
	functions         map[string]trace_record.FunctionId
	paths             map[string]trace_record.PathId
	pathsByIndex      []string // ordered list of paths for serialization
	variables         map[string]trace_record.VariableId
	variableNames     map[trace_record.VariableId]string // reverse lookup
	types             map[string]trace_record.TypeId
	currentCallsCount int
}

// NewRustTraceWriter creates a new RustTraceWriter.
func NewRustTraceWriter() (TraceRecorder, error) {
	return &RustTraceWriter{
		events:        make([]rustBufferedEvent, 0),
		functions:     make(map[string]trace_record.FunctionId),
		paths:         make(map[string]trace_record.PathId),
		pathsByIndex:  make([]string, 0),
		variables:     make(map[string]trace_record.VariableId),
		variableNames: make(map[trace_record.VariableId]string),
		types:         make(map[string]trace_record.TypeId),
	}, nil
}

func (w *RustTraceWriter) RegisterStep(path string, line trace_record.Line) {
	pathId := w.EnsurePathId(path)
	w.RegisterStepWithPathId(pathId, line)
}

func (w *RustTraceWriter) RegisterStepWithPathId(pathId trace_record.PathId, line trace_record.Line) {
	w.events = append(w.events, rustBufferedEvent{
		kind:   rustEventStep,
		pathId: pathId,
		line:   line,
	})
}

func (w *RustTraceWriter) RegisterCall(name string, definitionPath string, definitionLine trace_record.Line, args []trace_record.FullValueRecord) {
	definitionPathId := w.EnsurePathId(definitionPath)
	w.RegisterCallWithPathId(name, definitionPathId, definitionLine, args)
}

func (w *RustTraceWriter) RegisterCallWithPathId(name string, pathId trace_record.PathId, line trace_record.Line, args []trace_record.FullValueRecord) {
	functionId := w.EnsureFunctionId(name, pathId, line)
	w.events = append(w.events, rustBufferedEvent{
		kind:       rustEventCall,
		functionId: functionId,
		args:       args,
	})
	w.currentCallsCount++
}

func (w *RustTraceWriter) RegisterReturn(value trace_record.ValueRecord) {
	w.events = append(w.events, rustBufferedEvent{
		kind:        rustEventReturn,
		returnValue: value,
	})
}

func (w *RustTraceWriter) RegisterVariable(name string, value trace_record.ValueRecord) {
	variableId := w.EnsureVariableId(name)
	w.RegisterFullValue(variableId, value)
}

func (w *RustTraceWriter) RegisterRecordEvent(kind trace_record.RecordEventKind, metadata string, content string) {
	w.events = append(w.events, rustBufferedEvent{
		kind:            rustEventRecordEvent,
		recordEventKind: kind,
		content:         content,
		metadata:        metadata,
	})
}

func (w *RustTraceWriter) EnsureFunctionId(name string, pathId trace_record.PathId, line trace_record.Line) trace_record.FunctionId {
	functionId, ok := w.functions[name]
	if !ok {
		functionId = w.RegisterFunctionWithNewId(name, pathId, line)
	}
	return functionId
}

func (w *RustTraceWriter) RegisterFunctionWithNewId(name string, pathId trace_record.PathId, line trace_record.Line) trace_record.FunctionId {
	newFunctionId := trace_record.FunctionId(len(w.functions))
	w.functions[name] = newFunctionId
	w.events = append(w.events, rustBufferedEvent{
		kind:     rustEventFunction,
		funcName: name,
		funcPath: pathId,
		funcLine: line,
	})
	return newFunctionId
}

func (w *RustTraceWriter) EnsureVariableId(name string) trace_record.VariableId {
	variableId, ok := w.variables[name]
	if !ok {
		variableId = w.RegisterVariableNameWithNewId(name)
	}
	return variableId
}

func (w *RustTraceWriter) RegisterVariableNameWithNewId(name string) trace_record.VariableId {
	newVariableId := trace_record.VariableId(len(w.variables))
	w.variables[name] = newVariableId
	w.variableNames[newVariableId] = name
	w.events = append(w.events, rustBufferedEvent{
		kind:    rustEventVariableName,
		varName: name,
	})
	return newVariableId
}

func (w *RustTraceWriter) EnsurePathId(path string) trace_record.PathId {
	pathId, ok := w.paths[path]
	if !ok {
		pathId = w.RegisterPathWithNewId(path)
	}
	return pathId
}

func (w *RustTraceWriter) RegisterPathWithNewId(path string) trace_record.PathId {
	newPathId := trace_record.PathId(len(w.paths))
	w.paths[path] = newPathId
	w.pathsByIndex = append(w.pathsByIndex, path)
	w.events = append(w.events, rustBufferedEvent{
		kind: rustEventPath,
		path: path,
	})
	return newPathId
}

func (w *RustTraceWriter) EnsureTypeId(name string, typeRecord trace_record.TypeRecord) trace_record.TypeId {
	typeId, ok := w.types[name]
	if !ok {
		typeId = w.RegisterTypeWithNewId(name, typeRecord)
	}
	return typeId
}

func (w *RustTraceWriter) RegisterTypeWithNewId(name string, typeRecord trace_record.TypeRecord) trace_record.TypeId {
	newTypeId := trace_record.TypeId(len(w.types))
	w.types[name] = newTypeId
	w.events = append(w.events, rustBufferedEvent{
		kind:       rustEventType,
		typeName:   name,
		typeRecord: typeRecord,
	})
	return newTypeId
}

func (w *RustTraceWriter) RegisterFullValue(variableId trace_record.VariableId, value trace_record.ValueRecord) {
	w.events = append(w.events, rustBufferedEvent{
		kind:       rustEventFullValue,
		variableId: variableId,
		value:      value,
	})
}

func (w *RustTraceWriter) CurrentCallsCount() int {
	return w.currentCallsCount
}

func (w *RustTraceWriter) Arg(name string, value trace_record.ValueRecord) trace_record.FullValueRecord {
	variableId := w.EnsureVariableId(name)
	return trace_record.FullValueRecord{VariableId: variableId, Value: value}
}

// ProduceTrace writes the collected trace data using the Rust FFI library.
// It creates the Rust writer handle, replays all buffered events through
// the FFI in the correct phase order, then frees the handle.
func (w *RustTraceWriter) ProduceTrace(traceDir string, programName string, workdir string) error {
	cProgram := C.CString(programName)
	defer C.free(unsafe.Pointer(cProgram))

	handle := C.trace_writer_new(cProgram, C.FMT_JSON)
	if handle == nil {
		return fmt.Errorf("trace_writer_new failed: %s", C.GoString(C.trace_writer_last_error()))
	}
	defer C.trace_writer_free(handle)

	// Phase 1: Metadata
	metadataPath := filepath.Join(traceDir, "trace_metadata.json")
	cMetadataPath := C.CString(metadataPath)
	defer C.free(unsafe.Pointer(cMetadataPath))

	if !bool(C.trace_writer_begin_metadata(handle, cMetadataPath)) {
		return fmt.Errorf("trace_writer_begin_metadata failed: %s", C.GoString(C.trace_writer_last_error()))
	}

	// Write the start event (first path and line from the first step event)
	firstStepPath, firstStepLine := w.findFirstStep()
	if firstStepPath != "" {
		cPath := C.CString(firstStepPath)
		C.trace_writer_start(handle, cPath, C.int64_t(firstStepLine))
		C.free(unsafe.Pointer(cPath))
	}

	cWorkdir := C.CString(workdir)
	C.trace_writer_set_workdir(handle, cWorkdir)
	C.free(unsafe.Pointer(cWorkdir))

	if !bool(C.trace_writer_finish_metadata(handle)) {
		return fmt.Errorf("trace_writer_finish_metadata failed: %s", C.GoString(C.trace_writer_last_error()))
	}

	// Phase 2: Events
	eventsPath := filepath.Join(traceDir, "trace.json")
	cEventsPath := C.CString(eventsPath)
	defer C.free(unsafe.Pointer(cEventsPath))

	if !bool(C.trace_writer_begin_events(handle, cEventsPath)) {
		return fmt.Errorf("trace_writer_begin_events failed: %s", C.GoString(C.trace_writer_last_error()))
	}

	// Replay all buffered events
	for _, event := range w.events {
		w.replayEvent(handle, event)
	}

	if !bool(C.trace_writer_finish_events(handle)) {
		return fmt.Errorf("trace_writer_finish_events failed: %s", C.GoString(C.trace_writer_last_error()))
	}

	// Phase 3: Paths
	pathsPath := filepath.Join(traceDir, "trace_paths.json")
	cPathsPath := C.CString(pathsPath)
	defer C.free(unsafe.Pointer(cPathsPath))

	if !bool(C.trace_writer_begin_paths(handle, cPathsPath)) {
		return fmt.Errorf("trace_writer_begin_paths failed: %s", C.GoString(C.trace_writer_last_error()))
	}

	if !bool(C.trace_writer_finish_paths(handle)) {
		return fmt.Errorf("trace_writer_finish_paths failed: %s", C.GoString(C.trace_writer_last_error()))
	}

	fmt.Println("generated trace in ", traceDir)
	return nil
}

// findFirstStep returns the path and line of the first step event in the buffer.
func (w *RustTraceWriter) findFirstStep() (string, trace_record.Line) {
	for _, event := range w.events {
		if event.kind == rustEventStep {
			if int(event.pathId) < len(w.pathsByIndex) {
				return w.pathsByIndex[int(event.pathId)], event.line
			}
		}
	}
	return "", 0
}

// replayEvent sends a single buffered event through the Rust FFI.
func (w *RustTraceWriter) replayEvent(handle *C.struct_TraceWriterHandle, event rustBufferedEvent) {
	switch event.kind {
	case rustEventStep:
		pathStr := ""
		if int(event.pathId) < len(w.pathsByIndex) {
			pathStr = w.pathsByIndex[int(event.pathId)]
		}
		cPath := C.CString(pathStr)
		C.trace_writer_register_step(handle, cPath, C.int64_t(event.line))
		C.free(unsafe.Pointer(cPath))

	case rustEventCall:
		// Register args as variables before the call
		for _, arg := range event.args {
			w.replayVariableValue(handle, arg)
		}
		C.trace_writer_register_call(handle, C.uintptr_t(event.functionId))

	case rustEventReturn:
		w.replayReturnValue(handle, event.returnValue)

	case rustEventVariable, rustEventFullValue:
		// Variables are registered as raw values through the FFI
		name := event.varName
		if event.kind == rustEventFullValue {
			// Look up variable name from ID
			name = w.variableNameById(event.variableId)
		}
		w.replayValueRecord(handle, name, event.value)

	case rustEventRecordEvent:
		cMetadata := C.CString(event.metadata)
		cContent := C.CString(event.content)
		ffiKind := goRecordEventKindToFfi(event.recordEventKind)
		C.trace_writer_register_special_event(handle, ffiKind, cMetadata, cContent)
		C.free(unsafe.Pointer(cMetadata))
		C.free(unsafe.Pointer(cContent))

	case rustEventFunction:
		pathStr := ""
		if int(event.funcPath) < len(w.pathsByIndex) {
			pathStr = w.pathsByIndex[int(event.funcPath)]
		}
		cName := C.CString(event.funcName)
		cPath := C.CString(pathStr)
		C.trace_writer_ensure_function_id(handle, cName, cPath, C.int64_t(event.funcLine))
		C.free(unsafe.Pointer(cName))
		C.free(unsafe.Pointer(cPath))

	case rustEventType:
		cLangType := C.CString(event.typeRecord.LangType)
		ffiKind := goTypeKindToFfi(event.typeRecord.Kind)
		C.trace_writer_ensure_type_id(handle, ffiKind, cLangType)
		C.free(unsafe.Pointer(cLangType))

	case rustEventPath:
		// Paths are handled during the paths phase; during events phase they
		// are implicitly registered via step/function path strings.

	case rustEventVariableName:
		// Variable names are registered implicitly when variables are recorded.
	}
}

// replayVariableValue sends a FullValueRecord through the FFI.
func (w *RustTraceWriter) replayVariableValue(handle *C.struct_TraceWriterHandle, fvr trace_record.FullValueRecord) {
	name := w.variableNameById(fvr.VariableId)
	w.replayValueRecord(handle, name, fvr.Value)
}

// replayValueRecord sends a single value record through the FFI as a variable registration.
func (w *RustTraceWriter) replayValueRecord(handle *C.struct_TraceWriterHandle, name string, value trace_record.ValueRecord) {
	if value == nil {
		return
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	switch v := value.(type) {
	case trace_record.IntValueRecord:
		typeKind := goTypeKindToFfi(trace_record.INT_TYPE_KIND)
		cTypeName := C.CString("i64")
		C.trace_writer_register_variable_int(handle, cName, C.int64_t(v.I), typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))

	case trace_record.FloatValueRecord:
		typeKind := goTypeKindToFfi(trace_record.FLOAT_TYPE_KIND)
		cTypeName := C.CString("f64")
		repr := fmt.Sprintf("%g", v.F)
		cRepr := C.CString(repr)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.BoolValueRecord:
		typeKind := goTypeKindToFfi(trace_record.BOOL_TYPE_KIND)
		cTypeName := C.CString("bool")
		repr := "false"
		if v.B {
			repr = "true"
		}
		cRepr := C.CString(repr)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.StringValueRecord:
		typeKind := goTypeKindToFfi(trace_record.STRING_TYPE_KIND)
		cTypeName := C.CString("string")
		cRepr := C.CString(v.Text)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.NilValueRecord:
		typeKind := C.enum_FfiTypeKind(C.TK_NONE)
		cTypeName := C.CString("None")
		cRepr := C.CString("None")
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.StructValueRecord:
		typeKind := goTypeKindToFfi(trace_record.STRUCT_TYPE_KIND)
		cTypeName := C.CString("struct")
		cRepr := C.CString("{...}")
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.SequenceValueRecord:
		typeKind := goTypeKindToFfi(trace_record.SLICE_TYPE_KIND)
		cTypeName := C.CString("[]")
		repr := fmt.Sprintf("[...%d elements]", len(v.Elements))
		cRepr := C.CString(repr)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.ReferenceValueRecord:
		typeKind := goTypeKindToFfi(trace_record.POINTER_TYPE_KIND)
		cTypeName := C.CString("*")
		repr := fmt.Sprintf("&0x%x", v.Address)
		cRepr := C.CString(repr)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.TupleValueRecord:
		typeKind := goTypeKindToFfi(trace_record.TUPLE_TYPE_KIND)
		cTypeName := C.CString("()")
		repr := fmt.Sprintf("(...%d elements)", len(v.Elements))
		cRepr := C.CString(repr)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.BigIntValueRecord:
		typeKind := goTypeKindToFfi(trace_record.INT_TYPE_KIND)
		cTypeName := C.CString("BigInt")
		repr := fmt.Sprintf("0x%x", v.Bytes)
		if v.Negative {
			repr = "-" + repr
		}
		cRepr := C.CString(repr)
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	default:
		// Unknown value type, register as raw
		typeKind := C.enum_FfiTypeKind(C.TK_RAW)
		cTypeName := C.CString("unknown")
		cRepr := C.CString(fmt.Sprintf("%v", value))
		C.trace_writer_register_variable_raw(handle, cName, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))
	}
}

// replayReturnValue dispatches the return value through the appropriate FFI function.
func (w *RustTraceWriter) replayReturnValue(handle *C.struct_TraceWriterHandle, value trace_record.ValueRecord) {
	if value == nil {
		C.trace_writer_register_return(handle)
		return
	}

	switch v := value.(type) {
	case trace_record.IntValueRecord:
		typeKind := goTypeKindToFfi(trace_record.INT_TYPE_KIND)
		cTypeName := C.CString("i64")
		C.trace_writer_register_return_int(handle, C.int64_t(v.I), typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))

	case trace_record.NilValueRecord:
		C.trace_writer_register_return(handle)

	case trace_record.FloatValueRecord:
		typeKind := goTypeKindToFfi(trace_record.FLOAT_TYPE_KIND)
		cTypeName := C.CString("f64")
		repr := fmt.Sprintf("%g", v.F)
		cRepr := C.CString(repr)
		C.trace_writer_register_return_raw(handle, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.BoolValueRecord:
		typeKind := goTypeKindToFfi(trace_record.BOOL_TYPE_KIND)
		cTypeName := C.CString("bool")
		repr := "false"
		if v.B {
			repr = "true"
		}
		cRepr := C.CString(repr)
		C.trace_writer_register_return_raw(handle, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	case trace_record.StringValueRecord:
		typeKind := goTypeKindToFfi(trace_record.STRING_TYPE_KIND)
		cTypeName := C.CString("string")
		cRepr := C.CString(v.Text)
		C.trace_writer_register_return_raw(handle, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))

	default:
		typeKind := C.enum_FfiTypeKind(C.TK_RAW)
		cTypeName := C.CString("unknown")
		cRepr := C.CString(fmt.Sprintf("%v", value))
		C.trace_writer_register_return_raw(handle, cRepr, typeKind, cTypeName)
		C.free(unsafe.Pointer(cTypeName))
		C.free(unsafe.Pointer(cRepr))
	}
}

// variableNameById returns the variable name for the given ID.
func (w *RustTraceWriter) variableNameById(id trace_record.VariableId) string {
	if name, ok := w.variableNames[id]; ok {
		return name
	}
	return fmt.Sprintf("var_%d", id)
}

// goTypeKindToFfi converts a trace_record.TypeKind to the FFI FfiTypeKind enum.
func goTypeKindToFfi(kind trace_record.TypeKind) C.enum_FfiTypeKind {
	// The integer values match between Go and Rust enums.
	return C.enum_FfiTypeKind(kind)
}

// goRecordEventKindToFfi converts a trace_record.RecordEventKind to the FFI FfiEventLogKind enum.
func goRecordEventKindToFfi(kind trace_record.RecordEventKind) C.enum_FfiEventLogKind {
	// The integer values match between Go and Rust enums.
	return C.enum_FfiEventLogKind(kind)
}

// Compile-time check that RustTraceWriter implements TraceRecorder.
var _ TraceRecorder = (*RustTraceWriter)(nil)

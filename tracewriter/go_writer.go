package tracewriter

import (
	"github.com/metacraft-labs/trace_record"
)

// GoWriter wraps a *trace_record.TraceRecord and implements the TraceRecorder
// interface. This is the default writer that delegates all calls to the
// existing pure-Go trace_record package.
type GoWriter struct {
	inner *trace_record.TraceRecord
}

// NewGoWriter creates a GoWriter wrapping the given TraceRecord pointer.
func NewGoWriter(tr *trace_record.TraceRecord) *GoWriter {
	return &GoWriter{inner: tr}
}

// Inner returns the underlying *trace_record.TraceRecord pointer.
// This is useful when existing code needs direct access to the TraceRecord.
func (w *GoWriter) Inner() *trace_record.TraceRecord {
	return w.inner
}

func (w *GoWriter) RegisterStep(path string, line trace_record.Line) {
	w.inner.RegisterStep(path, line)
}

func (w *GoWriter) RegisterStepWithPathId(pathId trace_record.PathId, line trace_record.Line) {
	w.inner.RegisterStepWithPathId(pathId, line)
}

func (w *GoWriter) RegisterCall(name string, definitionPath string, definitionLine trace_record.Line, args []trace_record.FullValueRecord) {
	w.inner.RegisterCall(name, definitionPath, definitionLine, args)
}

func (w *GoWriter) RegisterCallWithPathId(name string, pathId trace_record.PathId, line trace_record.Line, args []trace_record.FullValueRecord) {
	w.inner.RegisterCallWithPathId(name, pathId, line, args)
}

func (w *GoWriter) RegisterReturn(value trace_record.ValueRecord) {
	w.inner.RegisterReturn(value)
}

func (w *GoWriter) RegisterVariable(name string, value trace_record.ValueRecord) {
	w.inner.RegisterVariable(name, value)
}

func (w *GoWriter) RegisterRecordEvent(kind trace_record.RecordEventKind, metadata string, content string) {
	w.inner.RegisterRecordEvent(kind, metadata, content)
}

func (w *GoWriter) EnsureFunctionId(name string, pathId trace_record.PathId, line trace_record.Line) trace_record.FunctionId {
	return w.inner.EnsureFunctionId(name, pathId, line)
}

func (w *GoWriter) RegisterFunctionWithNewId(name string, pathId trace_record.PathId, line trace_record.Line) trace_record.FunctionId {
	return w.inner.RegisterFunctionWithNewId(name, pathId, line)
}

func (w *GoWriter) EnsureVariableId(name string) trace_record.VariableId {
	return w.inner.EnsureVariableId(name)
}

func (w *GoWriter) RegisterVariableNameWithNewId(name string) trace_record.VariableId {
	return w.inner.RegisterVariableNameWithNewId(name)
}

func (w *GoWriter) EnsurePathId(path string) trace_record.PathId {
	return w.inner.EnsurePathId(path)
}

func (w *GoWriter) RegisterPathWithNewId(path string) trace_record.PathId {
	return w.inner.RegisterPathWithNewId(path)
}

func (w *GoWriter) EnsureTypeId(name string, typeRecord trace_record.TypeRecord) trace_record.TypeId {
	return w.inner.EnsureTypeId(name, typeRecord)
}

func (w *GoWriter) RegisterTypeWithNewId(name string, typeRecord trace_record.TypeRecord) trace_record.TypeId {
	return w.inner.RegisterTypeWithNewId(name, typeRecord)
}

func (w *GoWriter) RegisterFullValue(variableId trace_record.VariableId, value trace_record.ValueRecord) {
	w.inner.RegisterFullValue(variableId, value)
}

func (w *GoWriter) ProduceTrace(traceDir string, programName string, workdir string) error {
	return w.inner.ProduceTrace(traceDir, programName, workdir)
}

func (w *GoWriter) CurrentCallsCount() int {
	return w.inner.CurrentCallsCount()
}

func (w *GoWriter) Arg(name string, value trace_record.ValueRecord) trace_record.FullValueRecord {
	return w.inner.Arg(name, value)
}

// Compile-time check that GoWriter implements TraceRecorder.
var _ TraceRecorder = (*GoWriter)(nil)

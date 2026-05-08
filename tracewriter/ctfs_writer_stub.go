//go:build !cgo

// CGO-disabled fallback for the CTFS writer.  The wazero CodeTracer fork is
// CTFS-only — there is no in-process JSON writer to fall back to — so the
// non-cgo build returns an error from NewCtfsTraceWriter rather than
// silently ignoring the recording request.
//
// Production builds always enable cgo via the Nix flake (see wazero.nix).
// This stub exists so `go vet` / `go build` against pure-Go targets do not
// fail with an unresolved package import; the resulting binary is missing
// the recording capability and any attempt to use it will surface a loud
// error at runtime.

package tracewriter

import (
	"fmt"

	"github.com/metacraft-labs/trace_record"
)

// CtfsTraceWriter is a non-functional placeholder when the binary is built
// without cgo.  All recording methods are no-ops and ProduceTrace returns
// an explanatory error.
type CtfsTraceWriter struct{}

func NewCtfsTraceWriter() *CtfsTraceWriter {
	return &CtfsTraceWriter{}
}

func (w *CtfsTraceWriter) RegisterStep(string, trace_record.Line) {}
func (w *CtfsTraceWriter) RegisterStepWithPathId(trace_record.PathId, trace_record.Line) {
}
func (w *CtfsTraceWriter) RegisterCall(string, string, trace_record.Line, []trace_record.FullValueRecord) {
}
func (w *CtfsTraceWriter) RegisterCallWithPathId(string, trace_record.PathId, trace_record.Line, []trace_record.FullValueRecord) {
}
func (w *CtfsTraceWriter) RegisterReturn(trace_record.ValueRecord)                                {}
func (w *CtfsTraceWriter) RegisterVariable(string, trace_record.ValueRecord)                      {}
func (w *CtfsTraceWriter) RegisterRecordEvent(trace_record.RecordEventKind, string, string)       {}
func (w *CtfsTraceWriter) RegisterFullValue(trace_record.VariableId, trace_record.ValueRecord)    {}

func (w *CtfsTraceWriter) EnsureFunctionId(string, trace_record.PathId, trace_record.Line) trace_record.FunctionId {
	return 0
}
func (w *CtfsTraceWriter) RegisterFunctionWithNewId(string, trace_record.PathId, trace_record.Line) trace_record.FunctionId {
	return 0
}
func (w *CtfsTraceWriter) EnsureVariableId(string) trace_record.VariableId        { return 0 }
func (w *CtfsTraceWriter) RegisterVariableNameWithNewId(string) trace_record.VariableId { return 0 }
func (w *CtfsTraceWriter) EnsurePathId(string) trace_record.PathId                { return 0 }
func (w *CtfsTraceWriter) RegisterPathWithNewId(string) trace_record.PathId       { return 0 }
func (w *CtfsTraceWriter) EnsureTypeId(string, trace_record.TypeRecord) trace_record.TypeId {
	return 0
}
func (w *CtfsTraceWriter) RegisterTypeWithNewId(string, trace_record.TypeRecord) trace_record.TypeId {
	return 0
}
func (w *CtfsTraceWriter) CurrentCallsCount() int { return 0 }
func (w *CtfsTraceWriter) Arg(name string, value trace_record.ValueRecord) trace_record.FullValueRecord {
	return trace_record.FullValueRecord{}
}

func (w *CtfsTraceWriter) ProduceTrace(string, string, string) error {
	return fmt.Errorf("CTFS trace writer requires cgo: build with CGO_ENABLED=1 and the codetracer-trace-format-nim FFI library")
}

var _ TraceRecorder = (*CtfsTraceWriter)(nil)

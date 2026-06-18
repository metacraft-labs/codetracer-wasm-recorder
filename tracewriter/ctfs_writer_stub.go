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

	"github.com/tetratelabs/wazero/internal/tracetypes"
)

// CtfsTraceWriter is a non-functional placeholder when the binary is built
// without cgo.  All recording methods are no-ops and ProduceTrace returns
// an explanatory error.
type CtfsTraceWriter struct{}

func NewCtfsTraceWriter() *CtfsTraceWriter {
	return &CtfsTraceWriter{}
}

func (w *CtfsTraceWriter) RegisterStep(string, tracetypes.Line) {}
func (w *CtfsTraceWriter) RegisterStepWithPathId(tracetypes.PathId, tracetypes.Line) {
}
func (w *CtfsTraceWriter) RegisterStepWithColumn(string, tracetypes.Line, *tracetypes.Line) {
}
func (w *CtfsTraceWriter) EnableColumnAwareSteps()                      {}
func (w *CtfsTraceWriter) EnableColumnBreakpointsSupport()               {}
func (w *CtfsTraceWriter) EnableColumnMotionsSupport()                   {}
func (w *CtfsTraceWriter) RegisterPathWithLineLengths(string, []uint32) {}
func (w *CtfsTraceWriter) RegisterCall(string, string, tracetypes.Line, []tracetypes.FullValueRecord) {
}
func (w *CtfsTraceWriter) RegisterCallWithPathId(string, tracetypes.PathId, tracetypes.Line, []tracetypes.FullValueRecord) {
}
func (w *CtfsTraceWriter) RegisterReturn(tracetypes.ValueRecord)                           {}
func (w *CtfsTraceWriter) RegisterVariable(string, tracetypes.ValueRecord)                 {}
func (w *CtfsTraceWriter) RegisterRecordEvent(tracetypes.RecordEventKind, string, string)  {}
func (w *CtfsTraceWriter) RegisterFullValue(tracetypes.VariableId, tracetypes.ValueRecord) {}

func (w *CtfsTraceWriter) EnsureFunctionId(string, tracetypes.PathId, tracetypes.Line) tracetypes.FunctionId {
	return 0
}
func (w *CtfsTraceWriter) RegisterFunctionWithNewId(string, tracetypes.PathId, tracetypes.Line) tracetypes.FunctionId {
	return 0
}
func (w *CtfsTraceWriter) EnsureVariableId(string) tracetypes.VariableId              { return 0 }
func (w *CtfsTraceWriter) RegisterVariableNameWithNewId(string) tracetypes.VariableId { return 0 }
func (w *CtfsTraceWriter) EnsurePathId(string) tracetypes.PathId                      { return 0 }
func (w *CtfsTraceWriter) RegisterPathWithNewId(string) tracetypes.PathId             { return 0 }
func (w *CtfsTraceWriter) EnsureTypeId(string, tracetypes.TypeRecord) tracetypes.TypeId {
	return 0
}
func (w *CtfsTraceWriter) RegisterTypeWithNewId(string, tracetypes.TypeRecord) tracetypes.TypeId {
	return 0
}
func (w *CtfsTraceWriter) CurrentCallsCount() int { return 0 }
func (w *CtfsTraceWriter) Arg(name string, value tracetypes.ValueRecord) tracetypes.FullValueRecord {
	return tracetypes.FullValueRecord{}
}

func (w *CtfsTraceWriter) ProduceTrace(string, string, string) error {
	return fmt.Errorf("CTFS trace writer requires cgo: build with CGO_ENABLED=1 and the codetracer-trace-format-nim FFI library")
}

var _ TraceRecorder = (*CtfsTraceWriter)(nil)

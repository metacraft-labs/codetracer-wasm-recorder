package tracewriter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/metacraft-labs/trace_record"
)

func TestGoWriterInterfaceCompliance(t *testing.T) {
	goRecord := trace_record.MakeTraceRecord()
	gw := NewGoWriter(&goRecord)

	// Verify it satisfies the TraceRecorder interface
	var recorder TraceRecorder = gw
	_ = recorder

	// Test basic operations
	gw.RegisterStep("main.rs", 1)
	gw.RegisterCall("main", "main.rs", 1, nil)
	if gw.CurrentCallsCount() != 1 {
		t.Errorf("expected 1 call, got %d", gw.CurrentCallsCount())
	}

	// Test ProduceTrace
	traceDir := t.TempDir()
	err := gw.ProduceTrace(traceDir, "test_go", "/tmp")
	if err != nil {
		t.Fatalf("ProduceTrace() failed: %v", err)
	}

	for _, name := range []string{"trace.json", "trace_metadata.json", "trace_paths.json"} {
		path := filepath.Join(traceDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist", path)
		}
	}
}

func TestGoWriterEnsurePathIdIdempotent(t *testing.T) {
	goRecord := trace_record.MakeTraceRecord()
	gw := NewGoWriter(&goRecord)

	id1 := gw.EnsurePathId("foo.rs")
	id2 := gw.EnsurePathId("foo.rs")
	if id1 != id2 {
		t.Errorf("EnsurePathId not idempotent: %d != %d", id1, id2)
	}

	id3 := gw.EnsurePathId("bar.rs")
	if id3 == id1 {
		t.Errorf("different paths should get different IDs")
	}
}

func TestGoWriterEnsureFunctionIdIdempotent(t *testing.T) {
	goRecord := trace_record.MakeTraceRecord()
	gw := NewGoWriter(&goRecord)

	pathId := gw.EnsurePathId("main.rs")
	fid1 := gw.EnsureFunctionId("main", pathId, 1)
	fid2 := gw.EnsureFunctionId("main", pathId, 1)
	if fid1 != fid2 {
		t.Errorf("EnsureFunctionId not idempotent: %d != %d", fid1, fid2)
	}
}

func TestGoWriterArg(t *testing.T) {
	goRecord := trace_record.MakeTraceRecord()
	gw := NewGoWriter(&goRecord)

	arg := gw.Arg("x", trace_record.IntValue(42, trace_record.TypeId(0)))
	expectedVarId := gw.EnsureVariableId("x")
	if arg.VariableId != expectedVarId {
		t.Errorf("Arg variable ID mismatch: got %d, expected %d", arg.VariableId, expectedVarId)
	}
}

func TestGoWriterInner(t *testing.T) {
	goRecord := trace_record.MakeTraceRecord()
	gw := NewGoWriter(&goRecord)

	if gw.Inner() != &goRecord {
		t.Error("Inner() should return the original TraceRecord pointer")
	}
}

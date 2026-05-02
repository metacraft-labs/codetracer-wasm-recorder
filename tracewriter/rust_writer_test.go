//go:build cgo

package tracewriter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/metacraft-labs/trace_record"
)

func TestRustTraceWriterBasic(t *testing.T) {
	rw, err := NewRustTraceWriter()
	if err != nil {
		t.Fatalf("NewRustTraceWriter() failed: %v", err)
	}

	// Record some events
	rw.RegisterStep("main.rs", 1)
	rw.RegisterCall("main", "main.rs", 1, nil)
	rw.RegisterStep("main.rs", 2)
	rw.RegisterVariable("x", trace_record.IntValue(42, trace_record.TypeId(0)))
	rw.RegisterStep("main.rs", 3)
	rw.RegisterReturn(trace_record.NilValue())

	// Produce trace to temp dir
	traceDir := t.TempDir()
	err = rw.ProduceTrace(traceDir, "test_program", "/tmp")
	if err != nil {
		t.Fatalf("ProduceTrace() failed: %v", err)
	}

	// Verify output files exist
	for _, name := range []string{"trace.json", "trace_metadata.json", "trace_paths.json"} {
		path := filepath.Join(traceDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist", path)
		}
	}

	// Verify trace_metadata.json content
	metadataBytes, err := os.ReadFile(filepath.Join(traceDir, "trace_metadata.json"))
	if err != nil {
		t.Fatalf("reading trace_metadata.json: %v", err)
	}
	var metadata map[string]interface{}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("parsing trace_metadata.json: %v", err)
	}
	if metadata["program"] != "test_program" {
		t.Errorf("expected program=test_program, got %v", metadata["program"])
	}
}

func TestRustTraceWriterInterfaceCompliance(t *testing.T) {
	rw, err := NewRustTraceWriter()
	if err != nil {
		t.Fatalf("NewRustTraceWriter() failed: %v", err)
	}

	// Verify it satisfies the TraceRecorder interface
	var recorder TraceRecorder = rw
	_ = recorder

	// Test EnsurePathId returns consistent IDs
	id1 := rw.EnsurePathId("foo.rs")
	id2 := rw.EnsurePathId("foo.rs")
	if id1 != id2 {
		t.Errorf("EnsurePathId not idempotent: %d != %d", id1, id2)
	}

	id3 := rw.EnsurePathId("bar.rs")
	if id3 == id1 {
		t.Errorf("different paths should get different IDs")
	}

	// Test EnsureFunctionId returns consistent IDs
	fid1 := rw.EnsureFunctionId("main", id1, 1)
	fid2 := rw.EnsureFunctionId("main", id1, 1)
	if fid1 != fid2 {
		t.Errorf("EnsureFunctionId not idempotent: %d != %d", fid1, fid2)
	}

	// Test CurrentCallsCount
	if rw.CurrentCallsCount() != 0 {
		t.Errorf("expected 0 calls, got %d", rw.CurrentCallsCount())
	}
	rw.RegisterCall("main", "foo.rs", 1, nil)
	if rw.CurrentCallsCount() != 1 {
		t.Errorf("expected 1 call, got %d", rw.CurrentCallsCount())
	}

	// Test Arg
	arg := rw.Arg("x", trace_record.IntValue(10, trace_record.TypeId(0)))
	if arg.VariableId != rw.EnsureVariableId("x") {
		t.Errorf("Arg variable ID mismatch")
	}
}

// TestRustTraceWriterFormatConstants pins the FfiTraceFormat enum mapping
// so a future re-cbindgen of codetracer_trace_writer.h cannot silently
// reorder the variants and corrupt the wasm recorder's on-disk format.
// See CTFS audit, /tmp/isonim-migration.txt section 1.60.
func TestRustTraceWriterFormatConstants(t *testing.T) {
	if RustFormatJSON != 0 {
		t.Errorf("RustFormatJSON drifted from FMT_JSON (0): got %d", RustFormatJSON)
	}
	if RustFormatBinaryV0 != 1 {
		t.Errorf("RustFormatBinaryV0 drifted from FMT_BINARY_V0 (1): got %d", RustFormatBinaryV0)
	}
	if RustFormatBinary != 2 {
		t.Errorf("RustFormatBinary drifted from FMT_BINARY (2): got %d", RustFormatBinary)
	}
}

// TestRustTraceWriterWithFormatBinary smoke-tests the explicit-format
// constructor.  The resulting trace artefacts only need to exist; the FFI
// library is responsible for the on-disk shape.
func TestRustTraceWriterWithFormatBinary(t *testing.T) {
	rw, err := NewRustTraceWriterWithFormat(RustFormatBinary)
	if err != nil {
		t.Fatalf("NewRustTraceWriterWithFormat(Binary) failed: %v", err)
	}
	rw.RegisterStep("main.rs", 1)
	rw.RegisterCall("main", "main.rs", 1, nil)
	rw.RegisterReturn(trace_record.NilValue())

	traceDir := t.TempDir()
	if err := rw.ProduceTrace(traceDir, "wasm_audit_format_binary", "/tmp"); err != nil {
		t.Fatalf("ProduceTrace() failed: %v", err)
	}
}

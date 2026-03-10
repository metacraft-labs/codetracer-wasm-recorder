//go:build !cgo

package tracewriter

import "fmt"

// NewRustTraceWriter returns an error when built without cgo support.
// The Rust FFI trace writer requires cgo to link against the
// codetracer_trace_writer_ffi native library.
func NewRustTraceWriter() (TraceRecorder, error) {
	return nil, fmt.Errorf("RustTraceWriter is not available: binary was built without cgo support")
}

//go:build !cgo

package tracewriter

import "fmt"

// RustFormat is the build-tag-independent re-export of the FFI's
// FfiTraceFormat enum.  See rust_writer.go for the canonical documentation;
// this stub mirrors the type so non-cgo callers (such as cmd/wazero) can
// reference the constants without conditional compilation at the call site.
type RustFormat int

const (
	RustFormatJSON     RustFormat = 0
	RustFormatBinaryV0 RustFormat = 1
	RustFormatBinary   RustFormat = 2
)

// NewRustTraceWriter returns an error when built without cgo support.
// The Rust FFI trace writer requires cgo to link against the
// codetracer_trace_writer_ffi native library.
func NewRustTraceWriter() (TraceRecorder, error) {
	return nil, fmt.Errorf("RustTraceWriter is not available: binary was built without cgo support")
}

// NewRustTraceWriterWithFormat mirrors the cgo constructor but returns an
// error -- the FFI library is not linked into a non-cgo build.
func NewRustTraceWriterWithFormat(_ RustFormat) (TraceRecorder, error) {
	return nil, fmt.Errorf("RustTraceWriter is not available: binary was built without cgo support")
}

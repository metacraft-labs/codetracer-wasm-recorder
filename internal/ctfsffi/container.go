//go:build cgo

// Package ctfsffi is the recorder's binding to the **canonical** CTFS
// container writer — `codetracer-trace-format-nim`'s
// `codetracer_ctfs/container_append.nim`, reached through the same C FFI
// (`libcodetracer_trace_writer.a`) that `tracewriter/ctfs_writer.go` uses to
// produce the `.ct` in the first place.
//
// # Why this is a binding and not an implementation
//
// The snapshot streams of `WASM-Replay-Snapshots-And-Slices.md` §6 must live
// **inside** the trace container, and they are only known after the trace
// writer has closed it. Until M38b the FFI had no "add an internal file to a
// closed container" call, so `internal/ctfs` carried a second implementation
// of the container layout in order to append.
//
// That second implementation drifted, expensively. The two writers disagreed
// about whether the multi-level block mapping of `CTFS-Binary-Format.md` §4
// is cumulative; containers written here were silently mis-read by every
// other CTFS implementation past ~511 data blocks (~2 MB at the default
// 4096-byte block), with no error raised, and the package's own round-trip
// test could not see it because it wrote and read with the same convention.
//
// So there is now exactly one CTFS **writer** in the world, and this package
// calls it. `internal/ctfs` survives as a reader only — deliberately, because
// an independent reader is what proves the writer, and a round trip through
// one implementation proves nothing. See `internal/ctfs/ffi_crossread_test.go`.
//
// # Threading
//
// The Nim runtime behind the FFI is initialised once per process; these calls
// are serialised on a package mutex, matching the FFI's own single-writer
// contract for a container, and each holds its OS thread for the duration of
// the call plus the error read that follows it (see `lastError`). The
// container must additionally be quiescent — no other process writing it —
// which holds here because the trace writer has closed it before any derived
// stream is attached.
package ctfsffi

/*
#cgo LDFLAGS: -lcodetracer_trace_writer -lzstd -lm -lpthread
#include "codetracer_trace_writer.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
	"unsafe"
)

// initOnce guards the one-shot Nim runtime initialisation. It is the same
// `codetracer_trace_writer_init` the trace writer calls; the FFI is safe to
// initialise exactly once per process, and both packages doing it
// independently is fine because each guards its own call and the underlying
// entry point is idempotent per process.
var initOnce sync.Once

// callMu serialises FFI calls from this package, so a failed call's message
// cannot be overwritten by a concurrent one before it is read.
var callMu sync.Mutex

func initFFI() {
	initOnce.Do(func() { C.codetracer_trace_writer_init() })
}

// lastError renders the FFI's error message for the call that just failed.
//
// The Nim side keeps it in a **thread-local**, so it is only readable from the
// OS thread that made the failing call. A Go goroutine may migrate between
// calls, so every call site here brackets `call + lastError` in
// `runtime.LockOSThread`; without that the message is occasionally empty or —
// worse — some other writer's. It must be called with callMu held and the
// goroutine still locked.
func lastError() string {
	msg := C.GoString(C.trace_writer_last_error())
	if msg == "" {
		return "the trace-format FFI reported a failure without a message"
	}
	return msg
}

// Create writes a new, empty CTFS container at `path`.
//
// The recorder's own traces are produced by the trace writer, not by this
// function. It exists for the container-level tests, which need containers
// whose internal files are large enough to exercise the multi-level mapping
// hierarchy — something a small demo trace never does — and it goes through
// the canonical writer for the same reason `Append` does.
//
// `blockSize` of 0 selects the format default (4096).
func Create(path string, blockSize uint32) error {
	initFFI()
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	callMu.Lock()
	defer callMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if rc := C.ct_container_create(cPath, C.uint32_t(blockSize)); rc != 0 {
		return fmt.Errorf("ctfsffi: creating container %s: %s", path, lastError())
	}
	return nil
}

// Append adds internal files to an already-closed CTFS container.
//
// Every name must be absent — CTFS is append-only (`ctfs-container.md`
// "Non-Goals": no file deletion or truncation) and a stale-but-present stream
// is exactly the "returns wrong bytes" failure the snapshot design must not
// have. Re-attaching snapshots to a container that already carries them is an
// error the caller resolves by re-materialising the trace.
//
// The whole map is attached as one batch, which is not merely convenient: the
// writer publishes it with a single rewrite of block 0, so no reader can ever
// observe a half-attached set of streams. Calling this once per stream would
// give up that property.
//
// Names are submitted in sorted order so a container's block layout is a
// function of its contents rather than of Go's map iteration order — which
// matters because `TestSnapshotMaterialisationIsByteIdentical` and its
// siblings compare whole `.ct` files byte for byte.
func Append(path string, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	initFFI()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	// The three parallel arrays and every buffer they point at are allocated
	// in C memory. cgo forbids handing C a Go pointer to memory that itself
	// contains Go pointers, and an array of pointers into Go slices is
	// precisely that; copying is also what keeps the bytes pinned for the
	// duration of a call the Go GC knows nothing about.
	n := len(names)
	cNames := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	defer C.free(cNames)
	cContents := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	defer C.free(cContents)
	cLengths := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(C.size_t(0))))
	defer C.free(cLengths)

	nameSlots := unsafe.Slice((**C.char)(cNames), n)
	contentSlots := unsafe.Slice((**C.uint8_t)(cContents), n)
	lengthSlots := unsafe.Slice((*C.size_t)(cLengths), n)

	for i, name := range names {
		nameSlots[i] = C.CString(name)
		defer C.free(unsafe.Pointer(nameSlots[i]))

		body := files[name]
		lengthSlots[i] = C.size_t(len(body))
		if len(body) == 0 {
			contentSlots[i] = nil
			continue
		}
		buf := C.CBytes(body)
		defer C.free(buf)
		contentSlots[i] = (*C.uint8_t)(buf)
	}

	callMu.Lock()
	defer callMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rc := C.ct_container_append_files(
		cPath,
		(**C.char)(cNames),
		(**C.uint8_t)(cContents),
		(*C.size_t)(cLengths),
		C.size_t(n))
	if rc != 0 {
		return fmt.Errorf("ctfsffi: appending %d internal file(s) to %s: %s",
			n, path, lastError())
	}
	return nil
}

package ctfs

import (
	"bytes"
	"path/filepath"
	"testing"
)

// The multi-level block-mapping layout is the one part of this package that
// cannot be validated by round-tripping against itself: a self-consistent but
// wrong hierarchy reads back perfectly here and is silently mis-read by every
// other CTFS implementation. This file therefore checks the reader against the
// *producer's* definition rather than against this package's.
//
// NO MOCKS. `testdata/nim-multilevel.ct` was written by the canonical Nim
// writer (`codetracer-trace-format-nim`, `codetracer_ctfs/container.nim` +
// `block_mapping.nim`) and is committed verbatim, so this test needs no Nim
// toolchain and no cgo at test time — which is exactly what makes it the
// dependency-free anchor of the reader. Its producer is
// `testdata/gen_nim_multilevel.nim`, whose header gives the command that
// regenerates it.
//
// This file used to carry a second test pinning this package's own *writer*
// against `nimLookupDataBlock`. That writer is gone (M38b: the canonical one
// gained an append entry point, see `internal/ctfsffi`), and the obligation
// moved with it to `ffi_crossread_test.go`, which drives `nimLookupDataBlock`
// over a container the FFI wrote. The transcription lives there, next to its
// only caller: that file is built only with cgo, and a helper defined here
// would be dead code in every build without it.

const (
	// The fixture uses a 512-byte block, so `usable` is 127 and a 130-block
	// file straddles the level-1/level-2 boundary in ~135 KiB rather than the
	// 2 MiB a 4096-byte block would need.
	fixtureBlockSize  = 1024
	fixtureBlockCount = 130
	fixtureName       = "big.dat"
)

// fixturePattern reproduces the bytes the Nim probe wrote.
func fixturePattern() []byte {
	n := fixtureBlockCount * fixtureBlockSize
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte((i*7 + i/fixtureBlockSize) % 251)
	}
	return b
}

// TestReadsAMultiLevelFileWrittenByTheNimWriter pins the *reader* against the
// producer. Before this test existed, this package resolved level-2 indices
// with the level-1 root re-parented under the level-2 block — a reading the
// container spec's §4 diagram admits — and read the fixture's data blocks from
// the wrong slots.
func TestReadsAMultiLevelFileWrittenByTheNimWriter(t *testing.T) {
	c, err := Open(filepath.Join("testdata", "nim-multilevel.ct"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.BlockSize() != fixtureBlockSize {
		t.Fatalf("fixture block size is %d, expected %d", c.BlockSize(), fixtureBlockSize)
	}
	got, err := c.ReadFile(fixtureName)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := fixturePattern()
	if !bytes.Equal(got, want) {
		t.Fatalf("%q round-tripped %d bytes, want %d (equal prefix %d, block %d)",
			fixtureName, len(got), len(want), commonPrefix(got, want),
			commonPrefix(got, want)/fixtureBlockSize)
	}
}

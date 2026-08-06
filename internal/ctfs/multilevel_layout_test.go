package ctfs

import (
	"bytes"
	"encoding/binary"
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
// dependency-free anchor of the reader.
//
// This file used to carry a second test pinning this package's own *writer*
// against `nimLookupDataBlock`. That writer is gone (M38b: the canonical one
// gained an append entry point, see `internal/ctfsffi`), and the obligation
// moved with it to `ffi_crossread_test.go`, which drives `nimLookupDataBlock`
// over a container the FFI wrote. The transcription stays here because both
// files use it and because it belongs next to the fixture it explains.

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

// nimLookupDataBlock is `lookupDataBlock` from
// `codetracer-trace-format-nim/src/codetracer_ctfs/block_mapping.nim`,
// transcribed. Kept deliberately literal so it stays diffable against that
// source: the levels are cumulative, the walk up subtracts each level's
// capacity, and the chain pointer lives in slot `usable` of every level.
func nimLookupDataBlock(t *testing.T, raw []byte, blockSize int, rootBlock, blockIndex uint64) uint64 {
	t.Helper()
	usable := uint64(blockSize/8 - 1)
	readPtr := func(blk, i uint64) uint64 {
		off := int(blk)*blockSize + int(i)*8
		if off+8 > len(raw) {
			t.Fatalf("pointer at block %d slot %d is outside the %d-byte container", blk, i, len(raw))
		}
		return binary.LittleEndian.Uint64(raw[off:])
	}
	capacity := func(level uint32) uint64 {
		c := uint64(1)
		for i := uint32(0); i < level; i++ {
			c *= usable
		}
		return c
	}

	idx, cur, level := blockIndex, rootBlock, uint32(1)
	for {
		if idx < capacity(level) {
			break
		}
		idx -= capacity(level)
		level++
		if level > 5 {
			return 0
		}
		chain := readPtr(cur, usable)
		if chain == 0 {
			return 0
		}
		cur = chain
	}
	for level > 1 {
		subCap := capacity(level - 1)
		child := readPtr(cur, idx/subCap)
		if child == 0 {
			return 0
		}
		cur, idx, level = child, idx%subCap, level-1
	}
	return readPtr(cur, idx)
}

//go:build cgo

package ctfs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfsffi"
)

// The partial-tail case: what a crash *inside* an append's tail write leaves,
// and the fact that this reader and the canonical Nim one now agree about it.
//
// # The disagreement this closes (M57)
//
// `CTFS-Binary-Format.md` §5d orders the append so the new blocks land first
// and block 0 — the only thing that makes them reachable — is rewritten last,
// and promises that a crash in between leaves a container that is "wasteful
// but perfectly readable". A crash *inside* the tail write is the same state
// with one extra property: the file's length is no longer a whole number of
// blocks.
//
// The two implementations disagreed about that file. The Nim reader read it
// correctly — it bounds every access against the actual byte count and derives
// no allocator state from the length — while this package's `Open` refused it
// outright, so the spec's promise was false here. M57 made `Open` accept it.
// The *append* still refuses it, deliberately: it recovers `NextFreeBlock`
// from the length, and a wrong one overwrites live data.
//
// NO MOCKS: real containers written by the canonical FFI writer, on a real
// filesystem. The interruption is produced by extending a real sealed
// container with a real partial block, which is byte-for-byte the state the
// crash leaves — block 0 untouched, trailing bytes unreferenced.

// truncatedTailContainer seals a container holding one stream, then appends
// `extra` bytes that no entry references, leaving a length that is not a block
// multiple. It returns the path and the stream's content.
func truncatedTailContainer(t *testing.T, extra int) (string, []byte) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "torn.ct")
	if err := ctfsffi.Create(path, 4096); err != nil {
		t.Fatalf("Create: %v", err)
	}
	content := deterministicBytes(77, 9000)
	if err := ctfsffi.Append(path, map[string][]byte{"meta.dat": content}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	sealed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed)%4096 != 0 {
		t.Fatalf("the sealed container is already %d bytes, not a block multiple; "+
			"the fixture cannot isolate the partial tail", len(sealed))
	}
	if err := os.WriteFile(path, append(sealed, deterministicBytes(78, extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, content
}

// TestOpenAcceptsAContainerWithAPartialTail is the reader half of the M57
// decision. Before it, this assertion failed on the first line: `Open`
// returned "not a whole number of ... blocks".
func TestOpenAcceptsAContainerWithAPartialTail(t *testing.T) {
	// One whole block plus a fragment: the shape a tail write dying part-way
	// through actually leaves, rather than a bare few bytes.
	path, content := truncatedTailContainer(t, 4096+777)

	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open refused a container with a partial tail: %v\n"+
			"CTFS-Binary-Format.md §5d requires readers to accept this state — "+
			"block 0 is the previous complete one and the trailing bytes are "+
			"unreferenced.", err)
	}

	// Prove the fixture actually exercised the case, so this cannot pass
	// vacuously against a cleanly sealed container.
	if got := c.PartialTailBytes(); got != 777 {
		t.Fatalf("expected 777 bytes outside the last whole block, got %d", got)
	}

	// The pre-existing stream must read back byte-exact.
	if !c.Has("meta.dat") {
		t.Fatal("meta.dat is absent from a container whose block 0 was never rewritten")
	}
	got, err := c.ReadFile("meta.dat")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("meta.dat came back wrong: %d bytes vs %d", len(got), len(content))
	}

	// And the unreferenced tail must not have invented a stream.
	for _, n := range c.Names() {
		if n != "meta.dat" {
			t.Errorf("the unreferenced partial tail surfaced as internal file %q", n)
		}
	}
}

// TestNoDataBlockIsServedOutOfThePartialRegion is the other half of
// "unaddressable", and the half `readBlock` does not cover.
//
// `readEntry` reads DATA blocks with a bare `ReadAt` rather than through
// `readBlock`, and clamps the final block's slice to the entry's `Size`. So
// before the bound in `resolveDataBlock`, a short final block landing in the
// partial region was read SUCCESSFULLY out of bytes that `Open` had declared
// outside the container — the one way tolerating a partial tail could have
// turned a truncated file into wrong content instead of an error.
//
// A genuine partial tail never references those bytes (block 0 is the old one,
// so every pointer is below the old EOF), which is why this needs a *truncated*
// container to provoke: the file is cut so an entry's last, short data block
// becomes the first partial block with exactly its bytes present.
func TestNoDataBlockIsServedOutOfThePartialRegion(t *testing.T) {
	const bs = 4096
	path := filepath.Join(t.TempDir(), "cut.ct")
	if err := ctfsffi.Create(path, bs); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A size whose last data block carries only 100 bytes, so the clamped read
	// is short enough to be satisfiable out of a partial block.
	const tailBytes = 100
	content := deterministicBytes(5, 3*bs+tailBytes)
	if err := ctfsffi.Append(path, map[string][]byte{"z.dat": content}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := c.find("z.dat")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := c.dataBlocks(f, e)
	if err != nil {
		t.Fatalf("dataBlocks: %v", err)
	}
	f.Close()
	last := blocks[len(blocks)-1]

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := int64(last)*bs + tailBytes
	if cut >= int64(len(full)) {
		t.Fatalf("the writer put z.dat's last data block (%d) at the end of the "+
			"%d-byte container, so this fixture cannot place it in the partial "+
			"region; adjust the layout rather than deleting the test", last, len(full))
	}
	if err := os.WriteFile(path, full[:cut], 0o600); err != nil {
		t.Fatal(err)
	}

	c2, err := Open(path)
	if err != nil {
		// Also an acceptable answer, but not the one this reader gives — flag it
		// so the test does not quietly stop exercising the read path.
		t.Fatalf("Open refused the truncated container: %v\n"+
			"This reader accepts a non-block-multiple length by design; if that "+
			"changed, this test is checking nothing.", err)
	}
	if c2.PartialTailBytes() != tailBytes {
		t.Fatalf("expected a %d-byte partial region, got %d",
			tailBytes, c2.PartialTailBytes())
	}
	if last < c2.nextFreeBlock {
		t.Fatalf("z.dat's last data block (%d) is still inside the %d whole "+
			"blocks; the fixture did not place it in the partial region",
			last, c2.nextFreeBlock)
	}

	got, err := c2.ReadFile("z.dat")
	if err == nil {
		t.Fatalf("ReadFile returned %d bytes with no error for a stream whose "+
			"last data block (%d) lies outside the container's %d whole blocks; "+
			"the partial region was served as content", len(got), last, c2.nextFreeBlock)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("the refusal does not say the container is truncated: %v", err)
	}
}

// TestThePartialTailIsUnaddressable pins the property that makes accepting it
// safe: `nextFreeBlock` truncates, so the incomplete final block cannot be
// read and an entry naming it is rejected rather than served short.
func TestThePartialTailIsUnaddressable(t *testing.T) {
	path, _ := truncatedTailContainer(t, 777)

	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wholeBlocks := uint64(st.Size()) / c.blockSize
	if c.nextFreeBlock != wholeBlocks {
		t.Fatalf("nextFreeBlock is %d; it must truncate to the %d whole blocks so "+
			"the partial block stays unaddressable", c.nextFreeBlock, wholeBlocks)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := c.readBlock(f, c.nextFreeBlock); err == nil {
		t.Fatal("the incomplete final block was readable; a short read could be " +
			"served as content")
	}
}

// TestAppendStillRefusesAPartialTail is the other half of the decision, and
// the reason it is not simply "be permissive everywhere". Reading and
// extending ask different questions: the append recovers the allocator state
// from the file length, so §5d requires it to refuse rather than round.
//
// This also pins that the two operations disagree *on purpose*, so a later
// change that relaxes the append to match the reader has to argue with a test.
func TestAppendStillRefusesAPartialTail(t *testing.T) {
	path, content := truncatedTailContainer(t, 777)

	err := ctfsffi.Append(path, map[string][]byte{"snapshot.mem": []byte("xyz")})
	if err == nil {
		t.Fatal("the append accepted a container whose length it cannot trust; " +
			"a wrong NextFreeBlock overwrites live data")
	}
	if !strings.Contains(err.Error(), "whole number") {
		t.Errorf("unhelpful refusal message: %v", err)
	}

	// The refusal must be clean — the container is still readable afterwards.
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open after the refused append: %v", err)
	}
	got, err := c.ReadFile("meta.dat")
	if err != nil {
		t.Fatalf("ReadFile after the refused append: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("the refused append changed the pre-existing stream")
	}
}

// TestACleanContainerReportsNoPartialTail is the negative control: the field
// the tests above key on must be zero for every ordinary container, or they
// would be asserting on noise.
func TestACleanContainerReportsNoPartialTail(t *testing.T) {
	path := newContainer(t)
	if err := ctfsffi.Append(path, map[string][]byte{
		"meta.dat": deterministicBytes(9, 5000),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := c.PartialTailBytes(); got != 0 {
		t.Fatalf("a cleanly sealed container reports %d partial-tail bytes", got)
	}
}

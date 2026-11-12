//go:build cgo

package ctfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfsffi"
)

// The reader's round-trip corpus, driven by the **canonical** writer.
//
// These cases used to drive this package's own writer, which is gone (M38b).
// Every assertion is unchanged; what changed is who produced the bytes, and
// that strictly strengthens them: a round trip through one implementation
// could not have caught a layout mistake, and now it is not one — the writer
// is `codetracer-trace-format-nim`'s and the reader is this package's.
//
// NO MOCKS: real containers, real filesystem, real FFI.

// newContainer creates an empty container and returns its path.
//
// It deliberately does not `Open` it: an empty container has no non-empty
// root entry, and `Open` refuses to guess an entry-array offset it cannot
// validate. Every caller writes something first.
func newContainer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ct")
	if err := ctfsffi.Create(path, 4096); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return path
}

// deterministicBytes produces reproducible pseudo-random content so a
// round-trip failure is reproducible rather than flaky.
func deterministicBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

// TestRoundTripAcrossEveryMappingLevel is the core container test. The sizes
// straddle each structural transition of `ctfs-container.md` §4: the
// small-file optimisation (`Size <= BlockSize`, `MapBlock` IS the data block),
// the level-1 mapping block (up to 511 data blocks), and the level-2 hierarchy
// beyond it. A snapshot's `snapshot.mem` routinely lands in the last of those.
func TestRoundTripAcrossEveryMappingLevel(t *testing.T) {
	const blockSize = 4096
	const usable = blockSize/8 - 1 // 511

	sizes := []int{
		1, 100, blockSize - 1, blockSize, // small-file optimisation
		blockSize + 1, blockSize * 2, blockSize * 511, // level 1
		blockSize*511 + 1, blockSize * 700, // level 2
	}
	_ = usable

	path := newContainer(t)
	files := map[string][]byte{}
	for i, n := range sizes {
		files[fmt.Sprintf("f%d.dat", i)] = deterministicBytes(int64(i)+1, n)
	}
	if err := ctfsffi.Append(path, files); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Re-open from disk: the structure must be recoverable with nothing but
	// the bytes, since that is how every other CodeTracer reader sees it.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for name, want := range files {
		got, err := reopened.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q round-tripped %d bytes, want %d (equal prefix %d)",
				name, len(got), len(want), commonPrefix(got, want))
		}
	}
}

func TestZeroLengthFileIsRepresentable(t *testing.T) {
	path := newContainer(t)
	if err := ctfsffi.Append(path, map[string][]byte{"empty.dat": nil}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := reopened.ReadFile("empty.dat")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty file read back %d bytes", len(got))
	}
}

// TestAppendIsAdditive is the property the open/commercial split rests on:
// adding streams must leave every pre-existing stream byte-identical.
func TestAppendIsAdditive(t *testing.T) {
	path := newContainer(t)
	original := map[string][]byte{
		"meta.dat":  deterministicBytes(7, 300),
		"steps.dat": deterministicBytes(8, 40000),
	}
	if err := ctfsffi.Append(path, original); err != nil {
		t.Fatalf("Append(original): %v", err)
	}
	if err := ctfsffi.Append(path, map[string][]byte{
		"snapshot.idx": deterministicBytes(9, 5000),
		"snapshot.lay": deterministicBytes(10, 1000),
	}); err != nil {
		t.Fatalf("Append(snapshots): %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open after append: %v", err)
	}
	for name, want := range original {
		got, err := reopened.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q) after append: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q changed when snapshot streams were appended", name)
		}
	}
	if !reopened.Has("snapshot.idx") || !reopened.Has("snapshot.lay") {
		t.Errorf("appended streams are missing; names are %v", reopened.Names())
	}
}

// TestAppendRefusesToOverwrite: CTFS is append-only, and a snapshot stream
// that half-replaces an older one would produce exactly the stale-bytes
// failure the CAS design forbids.
func TestAppendRefusesToOverwrite(t *testing.T) {
	path := newContainer(t)
	if err := ctfsffi.Append(path, map[string][]byte{"snapshot.idx": []byte("first")}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := ctfsffi.Append(path, map[string][]byte{"snapshot.idx": []byte("second")}); err == nil {
		t.Fatal("overwriting an existing internal file was allowed")
	}
	// And the original content is intact.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := again.ReadFile("snapshot.idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("snapshot.idx = %q after the refused overwrite", got)
	}
}

// TestAnAppendTheRootCannotHoldIsRefusedWhole: a closed container's root
// directory does not grow. `ctfs-container.md` §5, "Adding Members To A
// Closed Container", requires the appender to refuse a batch the root region
// cannot hold whole, before it writes anything, naming the first member that
// does not fit and the occupancy, and never to attach part of the batch (§1,
// rule 2: the root is fixed at creation). This reader must therefore keep
// reading exactly what was there before the refused append, and a batch that
// does fit -- the positive control -- must still be accepted and read back.
func TestAnAppendTheRootCannotHoldIsRefusedWhole(t *testing.T) {
	path := newContainer(t)
	// One file first, so the container can be opened and its entry-array
	// capacity read off the header rather than assumed.
	if err := ctfsffi.Append(path, map[string][]byte{"first.dat": {1}}); err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	capacity := c.entryCount
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// One member more than the root has free slots for.
	tooMany := map[string][]byte{}
	for i := 0; i < capacity; i++ {
		tooMany[fmt.Sprintf("f%06d.dat", i)] = deterministicBytes(int64(i), 1+i%300)
	}
	err = ctfsffi.Append(path, tooMany)
	if err == nil {
		t.Fatalf("an append of %d members to a %d-entry root holding 1 was accepted", len(tooMany), capacity)
	}
	if !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("the refusal does not name the member that does not fit: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the refused append changed the container (%d bytes before, %d after)", len(before), len(after))
	}
	refused, err := Open(path)
	if err != nil {
		t.Fatalf("Open after the refused append: %v", err)
	}
	if names := refused.Names(); len(names) != 1 || names[0] != "first.dat" {
		t.Fatalf("members after the refused append are %v, want only first.dat", names)
	}

	// Positive control: the batch that exactly fills the root is accepted,
	// so the refusal above was about capacity, not about the batch.
	fits := map[string][]byte{}
	for i := 0; i < capacity-1; i++ {
		fits[fmt.Sprintf("f%06d.dat", i)] = deterministicBytes(int64(i), 1+i%300)
	}
	if err := ctfsffi.Append(path, fits); err != nil {
		t.Fatalf("an append that fills the %d-entry root exactly was refused: %v", capacity, err)
	}
	full, err := Open(path)
	if err != nil {
		t.Fatalf("Open after filling the root: %v", err)
	}
	if got := len(full.Names()); got != capacity {
		t.Fatalf("%d members are listed, want %d", got, capacity)
	}
	fits["first.dat"] = []byte{1}
	for name, want := range fits {
		got, err := full.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%q read back %d bytes that differ from the %d appended", name, len(got), len(want))
		}
	}
}

// TestARootThatSpansSeveralBlocksIsRead: the root region is as many blocks as
// the writer declared at creation (`ctfs-container.md` §1,
// `root_blocks = ceil((header + MaxRootEntries * 24) / BlockSize)`; §5: "The
// root may be any number of blocks the writer declared"), and the streaming
// writer still declares a larger one when a recording needs it. This reader
// must read the entries past block 0 and refuse none of them.
//
// The canonical FFI creates containers with a one-block root and offers no
// way to declare a larger one, so this test declares it: it raises the
// header's `MaxRootEntries` on a freshly created, still empty container and
// materialises the root blocks that implies, which is exactly the container
// a writer declaring that many entries at creation produces. Every member is
// then written by the canonical appender, which recovers `root_blocks` from
// the header.
func TestARootThatSpansSeveralBlocksIsRead(t *testing.T) {
	const blockSize = 4096
	path := newContainer(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != blockSize {
		t.Fatalf("a new container is %d bytes, want one %d-byte block", len(raw), blockSize)
	}
	// Three blocks' worth of entries after the 16-byte header.
	entries := (3*blockSize - headerSize) / fileEntrySize
	binary.LittleEndian.PutUint32(raw[offsetMaxRootE:], uint32(entries))
	raw = append(raw, make([]byte, 2*blockSize)...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// More members than block 0 alone has room for, so entries land in every
	// block of the root.
	files := map[string][]byte{}
	for i := 0; i < entries-1; i++ {
		files[fmt.Sprintf("f%06d.dat", i)] = deterministicBytes(int64(i), 1+i%300)
	}
	if err := ctfsffi.Append(path, files); err != nil {
		t.Fatalf("an append to a declared %d-entry, 3-block root was refused: %v", entries, err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open of a container with a 3-block root: %v", err)
	}
	if c.rootBlocks != 3 {
		t.Fatalf("the root spans %d blocks, want 3", c.rootBlocks)
	}
	if got := len(c.Names()); got != len(files) {
		t.Fatalf("%d members are listed, want %d", got, len(files))
	}
	for name, want := range files {
		got, err := c.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%q read back %d bytes that differ from the %d appended", name, len(got), len(want))
		}
	}
}

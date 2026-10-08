//go:build cgo

package ctfs

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
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

// TestAnAppendPastTheEntryArrayGrowsIt: an append that needs more entries
// than the container's root holds grows the root (`ctfs-container.md` §1,
// `root_blocks`), and every member -- the ones that fit and the ones past the
// old capacity -- reads back through this reader. A stream the root had no
// room for must not be dropped, and a root that spans several blocks must not
// be refused by the reader.
func TestAnAppendPastTheEntryArrayGrowsIt(t *testing.T) {
	path := newContainer(t)
	// One file first, so the container can be opened and its entry-array
	// capacity read off the header rather than assumed.
	if err := ctfsffi.Append(path, map[string][]byte{"first.dat": []byte{1}}); err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	capacity := c.entryCount
	files := map[string][]byte{}
	for i := 0; i < capacity; i++ {
		files[fmt.Sprintf("f%06d.dat", i)] = deterministicBytes(int64(i), 1+i%300)
	}
	if err := ctfsffi.Append(path, files); err != nil {
		t.Fatalf("an append past the %d-entry root was refused: %v", capacity, err)
	}
	grown, err := Open(path)
	if err != nil {
		t.Fatalf("Open after the root grew: %v", err)
	}
	if grown.entryCount <= capacity {
		t.Fatalf("the root holds %d entries after %d members were appended to a %d-entry root",
			grown.entryCount, len(files)+1, capacity)
	}
	if got := len(grown.Names()); got != len(files)+1 {
		t.Fatalf("%d members are listed, want %d", got, len(files)+1)
	}
	files["first.dat"] = []byte{1}
	for name, want := range files {
		got, err := grown.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%q read back %d bytes that differ from the %d appended", name, len(got), len(want))
		}
	}
}

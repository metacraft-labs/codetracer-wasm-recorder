package ctfs

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// No mocks are used in this file. Every assertion runs against a real
// container on a real filesystem, because the whole point of the package is
// the on-disk byte layout: a mocked block device would test nothing that
// matters.

func TestBase40RoundTrip(t *testing.T) {
	for _, name := range []string{
		"meta.dat", "steps.dat", "threads.ns", "syncord.log", "linehits.tc",
		"memwrites.tc", "snapshot.idx", "snapshot.lay", "snapshot.mem", "snappages.ns",
		"snapglob.dat", "snaptab.dat", "a", "z-9./",
	} {
		enc, err := EncodeName(name)
		if err != nil {
			t.Fatalf("EncodeName(%q): %v", name, err)
		}
		if got := DecodeName(enc); got != name {
			t.Errorf("DecodeName(EncodeName(%q)) = %q", name, got)
		}
	}
}

func TestBase40RejectsOverlongAndInvalidNames(t *testing.T) {
	// 13 characters — one past what 40^12 < 2^64 permits. `wcp.entry.lay` is
	// the name that actually hit this ceiling: an early revision of snapshot
	// spec §6 mirrored MCR's twelve-character `cp.entry.lay` with a `w` prefix.
	// It is why the snapshot streams have short names at all; today they are
	// `snapshot.lay` / `snapshot.mem`, twelve exactly.
	if _, err := EncodeName("wcp.entry.lay"); err == nil {
		t.Error("a 13-character name was accepted; base40 packs at most 12")
	}
	for _, bad := range []string{"", "UPPER", "has space", "under_score"} {
		if _, err := EncodeName(bad); err == nil {
			t.Errorf("EncodeName(%q) was accepted", bad)
		}
	}
}

func newContainer(t *testing.T) (*Container, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ct")
	c, err := Create(path, 4096)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return c, path
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

	c, path := newContainer(t)
	files := map[string][]byte{}
	for i, n := range sizes {
		files[fmt.Sprintf("f%d.dat", i)] = deterministicBytes(int64(i)+1, n)
	}
	if err := c.AddFiles(files); err != nil {
		t.Fatalf("AddFiles: %v", err)
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

func commonPrefix(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

func TestZeroLengthFileIsRepresentable(t *testing.T) {
	c, path := newContainer(t)
	if err := c.AddFiles(map[string][]byte{"empty.dat": nil}); err != nil {
		t.Fatalf("AddFiles: %v", err)
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

// TestAddFilesIsAdditive is the property the open/commercial split rests on:
// adding streams must leave every pre-existing stream byte-identical.
func TestAddFilesIsAdditive(t *testing.T) {
	c, path := newContainer(t)
	original := map[string][]byte{
		"meta.dat":  deterministicBytes(7, 300),
		"steps.dat": deterministicBytes(8, 40000),
	}
	if err := c.AddFiles(original); err != nil {
		t.Fatalf("AddFiles(original): %v", err)
	}

	added, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := added.AddFiles(map[string][]byte{
		"snapshot.idx": deterministicBytes(9, 5000),
		"snapshot.lay": deterministicBytes(10, 1000),
	}); err != nil {
		t.Fatalf("AddFiles(snapshots): %v", err)
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

// TestAddFilesRefusesToOverwrite: CTFS is append-only, and a snapshot stream
// that half-replaces an older one would produce exactly the stale-bytes
// failure the CAS design forbids.
func TestAddFilesRefusesToOverwrite(t *testing.T) {
	c, path := newContainer(t)
	if err := c.AddFiles(map[string][]byte{"snapshot.idx": []byte("first")}); err != nil {
		t.Fatalf("AddFiles: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reopened.AddFiles(map[string][]byte{"snapshot.idx": []byte("second")}); err == nil {
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

func TestOpenRejectsNonContainers(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.ct")
	if err := os.WriteFile(junk, bytes.Repeat([]byte{0xab}, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(junk); err == nil {
		t.Error("a file without the CTFS magic was opened as a container")
	}

	short := filepath.Join(dir, "short.ct")
	if err := os.WriteFile(short, magic[:], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(short); err == nil {
		t.Error("a 5-byte file was opened as a container")
	}
}

// TestEntryArrayExhaustionIsReported: a container whose entry array is full
// must fail loudly rather than silently dropping a stream.
func TestEntryArrayExhaustionIsReported(t *testing.T) {
	c, _ := newContainer(t)
	files := map[string][]byte{}
	for i := 0; i < c.entryCount+1; i++ {
		files[fmt.Sprintf("f%06d.dat", i)] = []byte{byte(i)}
	}
	err := c.AddFiles(files)
	if err == nil {
		t.Fatal("filling the entry array past capacity was allowed")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("free file-entry slot")) {
		t.Errorf("unhelpful error: %v", err)
	}
}

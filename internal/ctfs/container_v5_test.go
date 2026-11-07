package ctfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three forms of `FileEntry.MapBlock` in container version 5
// (`ctfs-container.md` §2, "`MapBlock` has three forms") and the version gate
// ("Older versions are refused").
//
// No mocks: every case writes a real container to a real file and opens it
// with `Open`. The containers are assembled byte by byte here rather than by
// the canonical writer because several of them are damaged in ways no
// conforming writer produces (a tagged block of size above one block, a null
// pointer), and because these cases must run without cgo. The canonical
// writer's own output is checked in `ffi_crossread_test.go` and against the
// committed fixture in `multilevel_layout_test.go`.

const v5BlockSize = 1024

// v5Member is one internal file for buildV5Container.
type v5Member struct {
	name string
	data []byte
}

// buildV5Container lays out a version 5 container the way a writer that has
// closed it must: an empty member owns no block (`MapBlock = 0`), a member of
// at most one block is direct (`MapBlock = Direct | b`), and a larger one has a
// level-1 mapping block followed by its data blocks.
func buildV5Container(t *testing.T, members ...v5Member) []byte {
	t.Helper()
	const maxEntries = 8
	if len(members) > maxEntries {
		t.Fatalf("too many members for the fixture builder")
	}
	blocks := [][]byte{make([]byte, v5BlockSize)}
	root := blocks[0]
	copy(root, magic[:])
	root[offsetVersion] = SupportedVersion
	binary.LittleEndian.PutUint32(root[offsetBlockSz:], v5BlockSize)
	binary.LittleEndian.PutUint32(root[offsetMaxRootE:], maxEntries)

	claim := func(b []byte) uint64 {
		blocks = append(blocks, b)
		return uint64(len(blocks) - 1)
	}
	dataBlock := func(data []byte, from int) []byte {
		b := make([]byte, v5BlockSize)
		copy(b, data[from:])
		return b
	}
	for i, m := range members {
		var mapBlock uint64
		switch {
		case len(m.data) == 0:
		case len(m.data) <= v5BlockSize:
			mapBlock = Direct | claim(dataBlock(m.data, 0))
		default:
			n := (len(m.data) + v5BlockSize - 1) / v5BlockSize
			if n >= v5BlockSize/8 {
				t.Fatalf("the fixture builder handles level-1 mappings only")
			}
			mapping := make([]byte, v5BlockSize)
			mapBlock = claim(mapping)
			for k := 0; k < n; k++ {
				binary.LittleEndian.PutUint64(mapping[k*8:], claim(dataBlock(m.data, k*v5BlockSize)))
			}
		}
		name, err := EncodeName(m.name)
		if err != nil {
			t.Fatal(err)
		}
		off := headerSize + i*fileEntrySize
		binary.LittleEndian.PutUint64(root[off:], uint64(len(m.data)))
		binary.LittleEndian.PutUint64(root[off+8:], mapBlock)
		binary.LittleEndian.PutUint64(root[off+16:], name)
	}
	return bytes.Join(blocks, nil)
}

// patchEntry overwrites one root entry's `(Size, MapBlock)`.
func patchEntry(raw []byte, slot int, size, mapBlock uint64) {
	off := headerSize + slot*fileEntrySize
	binary.LittleEndian.PutUint64(raw[off:], size)
	binary.LittleEndian.PutUint64(raw[off+8:], mapBlock)
}

func writeContainer(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v5.ct")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func openV5(t *testing.T, raw []byte) *Container {
	t.Helper()
	c, err := Open(writeContainer(t, raw))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

func TestADirectMemberIsReadFromItsTaggedBlock(t *testing.T) {
	small := []byte("a member of at most one block")
	exact := bytes.Repeat([]byte{0x5a}, v5BlockSize)
	c := openV5(t, buildV5Container(t, v5Member{"small.dat", small}, v5Member{"exact.dat", exact}))

	for name, want := range map[string][]byte{"small.dat": small, "exact.dat": exact} {
		got, err := c.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q read back %d bytes that differ from the %d written", name, len(got), len(want))
		}
	}
}

func TestAnEmptyMemberIsPresentAndEmpty(t *testing.T) {
	c := openV5(t, buildV5Container(t, v5Member{"empty.dat", nil}, v5Member{"small.dat", []byte("x")}))

	if !c.Has("empty.dat") {
		t.Fatal("a member with (Size, MapBlock) = (0, 0) was reported absent; a null is not an absence")
	}
	if names := c.Names(); len(names) != 2 || names[0] != "empty.dat" {
		t.Errorf("Names() = %v, want [empty.dat small.dat]", names)
	}
	got, err := c.ReadFile("empty.dat")
	if err != nil {
		t.Fatalf("ReadFile(empty.dat): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty member read back %d bytes", len(got))
	}
}

func TestAMappedMemberIsReadThroughItsMappingBlock(t *testing.T) {
	big := make([]byte, 2*v5BlockSize+300)
	for i := range big {
		big[i] = byte(i*7 + 3)
	}
	c := openV5(t, buildV5Container(t, v5Member{"a.dat", []byte("x")}, v5Member{"big.dat", big}))

	got, err := c.ReadFile("big.dat")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("big.dat read back wrong (equal prefix %d of %d)", commonPrefix(got, big), len(big))
	}
}

func TestEveryContainerVersionButFiveIsRefusedByName(t *testing.T) {
	raw := buildV5Container(t, v5Member{"small.dat", []byte("abc")})
	for _, version := range []byte{2, 3, 4, 6} {
		raw[offsetVersion] = version
		_, err := Open(writeContainer(t, raw))
		if err == nil {
			t.Fatalf("a version %d container was opened", version)
		}
		for _, want := range []string{"version " + string('0'+version), "version 5"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusing version %d: %q does not mention %q", version, err, want)
			}
		}
	}
}

func TestADirectMemberLargerThanOneBlockIsRefused(t *testing.T) {
	raw := buildV5Container(t, v5Member{"small.dat", []byte("abc")})
	patchEntry(raw, 0, v5BlockSize+1, Direct|1)

	err := openOrRead(t, raw, "small.dat")
	if err == nil || !strings.Contains(err.Error(), "small.dat") || !strings.Contains(err.Error(), "one block") {
		t.Fatalf("a direct member of %d bytes was not refused for exceeding one block: %v", v5BlockSize+1, err)
	}
}

func TestATaggedNullBlockIsRefusedAsNull(t *testing.T) {
	raw := buildV5Container(t, v5Member{"small.dat", []byte("abc")})
	patchEntry(raw, 0, 3, Direct)
	assertNullRefusal(t, openOrRead(t, raw, "small.dat"), "small.dat")
}

func TestANullMapBlockWithASizeIsRefusedAsNull(t *testing.T) {
	raw := buildV5Container(t, v5Member{"small.dat", []byte("abc")})
	patchEntry(raw, 0, 3, 0)
	assertNullRefusal(t, openOrRead(t, raw, "small.dat"), "small.dat")
}

func TestATaggedBlockPastTheEndIsRefused(t *testing.T) {
	raw := buildV5Container(t, v5Member{"small.dat", []byte("abc")})
	patchEntry(raw, 0, 3, Direct|1000)
	if err := openOrRead(t, raw, "small.dat"); err == nil {
		t.Fatal("a direct member naming block 1000 of a 2-block container was read")
	}
}

// openOrRead opens the container and reads one member, returning the first
// error. A reader may refuse a damaged entry at either step.
func openOrRead(t *testing.T, raw []byte, name string) error {
	t.Helper()
	c, err := Open(writeContainer(t, raw))
	if err != nil {
		return err
	}
	_, err = c.ReadFile(name)
	return err
}

func assertNullRefusal(t *testing.T, err error, name string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: a null block pointer was not refused", name)
	}
	msg := err.Error()
	if !strings.Contains(msg, name) || !strings.Contains(msg, "null") {
		t.Errorf("the refusal does not name %q and the null pointer: %v", name, err)
	}
	if strings.Contains(strings.ToLower(msg), "truncat") {
		t.Errorf("a null pointer was blamed on truncation: %v", err)
	}
}

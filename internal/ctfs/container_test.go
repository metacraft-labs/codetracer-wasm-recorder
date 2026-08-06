package ctfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// No mocks are used in this file. Every assertion runs against a real
// container on a real filesystem, because the whole point of the package is
// the on-disk byte layout: a mocked block device would test nothing that
// matters.
//
// This file holds the cases that need no writer at all, so it builds and runs
// without cgo. The reader is checked against real containers in
// `container_write_test.go` (written through the canonical writer's FFI) and
// against a committed Nim-written fixture in `multilevel_layout_test.go`.

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

func commonPrefix(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
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

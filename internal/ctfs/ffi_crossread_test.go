//go:build cgo

package ctfs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfsffi"
)

// The proof that the canonical CTFS writer's *append* path — M38b's
// `ct_container_append_files`, which extends a container that has already
// been closed — produces bytes something other than itself can read.
//
// This replaces the writer half of `multilevel_layout_test.go`. That test
// existed because this package used to carry its own container writer; when
// the FFI gained an append entry point the second writer was deleted, and
// what has to be pinned changed with it. The obligation did not go away: the
// append is *new* writer functionality against exactly the block-mapping
// hierarchy that a second implementation already got silently wrong once, and
// a mistake there is mis-read bytes with no error raised.
//
// The evidence is deliberately three-cornered, because a round trip through
// one implementation proves nothing:
//
//  1. this package's reader — a different implementation in a different
//     language, itself pinned against a committed Nim-written fixture by
//     `TestReadsAMultiLevelFileWrittenByTheNimWriter`;
//  2. `nimLookupDataBlock`, a literal transcription of the producer's own
//     `lookupDataBlock`, which shares no code with either side;
//  3. the **production** Nim reader, compiled and run out of the sibling
//     checkout — the last degree of freedom removed.
//
// Every one of them is exercised past the multi-level threshold. NO MOCKS:
// the container is written by the real FFI into a real file.

const (
	// The production block size. `usable` is 511, so data block 511 is the
	// first that needs a level-2 mapping block — the ~2 MB boundary past
	// which the previous drift became visible.
	crossBlockSize = 4096
	crossUsable    = crossBlockSize/8 - 1 // 511

	// 1100 data blocks (4.5 MB) reach past the boundary *and* into the
	// level-2 block's second slot: 511 direct, 511 through L2 slot 0, then 78
	// through L2 slot 1. A file that merely crossed into L2 slot 0 would
	// exercise the chain hop but never the descent.
	crossBigBlocks = 1100
	crossBigSize   = crossBigBlocks * crossBlockSize
)

// crossPattern is offset-dependent, so a block delivered from the wrong place
// is visibly wrong rather than plausibly right.
func crossPattern(n, salt int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*7 + i/crossBlockSize + salt) % 251)
	}
	return b
}

// crossFixture writes a container through the FFI and returns its path and
// the exact bytes each internal file should hold.
func crossFixture(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffi-written.ct")
	if err := ctfsffi.Create(path, crossBlockSize); err != nil {
		t.Fatalf("ct_container_create: %v", err)
	}
	// A first batch, sealed, then a second one appended on top of it: the
	// second append has to recover the allocator state from a container the
	// first one closed, which is the part an append gets to be wrong about.
	first := map[string][]byte{
		"meta.dat":  crossPattern(37, 1),
		"steps.dat": crossPattern(9000, 2),
	}
	if err := ctfsffi.Append(path, first); err != nil {
		t.Fatalf("first append: %v", err)
	}
	second := map[string][]byte{
		// Straddles the level-1/level-2 boundary and the L2 descent.
		"snapshot.mem": crossPattern(crossBigSize, 3),
		// Exactly `usable` blocks: the last file that fits at level 1, so an
		// off-by-one in the level walk shows up here and nowhere else.
		"snappages.ns": crossPattern(crossUsable*crossBlockSize, 4),
		// One block past it: the first that needs the chain pointer at all.
		"snapshot.idx": crossPattern((crossUsable+1)*crossBlockSize, 5),
		// And the degenerate ends, so the same call covers them too.
		"snapglob.dat": crossPattern(3, 6),
		"snaptab.dat":  nil,
	}
	if err := ctfsffi.Append(path, second); err != nil {
		t.Fatalf("second append: %v", err)
	}
	want := map[string][]byte{}
	for k, v := range first {
		want[k] = v
	}
	for k, v := range second {
		want[k] = v
	}
	return path, want
}

// TestTheGoReaderReadsAnFfiWrittenContainer is corner 1.
func TestTheGoReaderReadsAnFfiWrittenContainer(t *testing.T) {
	path, want := crossFixture(t)
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.BlockSize() != crossBlockSize {
		t.Fatalf("block size %d, want %d", c.BlockSize(), crossBlockSize)
	}
	for name, expect := range want {
		if !c.Has(name) {
			t.Fatalf("%q is missing from the container the FFI wrote", name)
		}
		got, err := c.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if len(got) != len(expect) {
			t.Fatalf("%q is %d bytes, want %d", name, len(got), len(expect))
		}
		if !bytes.Equal(got, expect) {
			d := commonPrefix(got, expect)
			t.Fatalf("%q differs at byte %d (data block %d of the internal file)",
				name, d, d/crossBlockSize)
		}
	}
	// The first append's streams must be untouched by the second — an append
	// that grew the container by rewriting rather than extending would still
	// read back consistently through its own entries.
	if !c.Has("meta.dat") || !c.Has("steps.dat") {
		t.Fatal("the second append lost the first append's streams")
	}
}

// TestTheProducerResolverReadsAnFfiWrittenContainer is corner 2: the
// producer's own resolver, transcribed, over the container the FFI wrote.
//
// This is what would catch the two implementations agreeing with each other
// while both disagreeing with the format — which is not idle, since one of
// them *is* the format's writer and could carry the mistake into it.
func TestTheProducerResolverReadsAnFfiWrittenContainer(t *testing.T) {
	path, want := crossFixture(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"snapshot.mem", "snapshot.idx", "snappages.ns"} {
		entry, err := c.find(name)
		if err != nil {
			t.Fatal(err)
		}
		if entry.MapBlock == 0 {
			t.Fatalf("%q has no mapping block", name)
		}
		expect := want[name]
		blocks := (len(expect) + crossBlockSize - 1) / crossBlockSize
		got := make([]byte, 0, len(expect))
		for i := 0; i < blocks; i++ {
			blk := nimLookupDataBlock(t, raw, crossBlockSize, entry.MapBlock, uint64(i))
			if blk == 0 {
				t.Fatalf("the producer's resolver found no data block for %q block %d",
					name, i)
			}
			off := int(blk) * crossBlockSize
			got = append(got, raw[off:off+crossBlockSize]...)
		}
		got = got[:len(expect)]
		if !bytes.Equal(got, expect) {
			d := commonPrefix(got, expect)
			t.Fatalf("the producer's resolver read %q wrongly from byte %d (block %d)",
				name, d, d/crossBlockSize)
		}
	}
}

// TestTheProductionNimReaderReadsAnFfiWrittenContainer is corner 3.
//
// Same shape and same skip discipline as
// `internal/wasmsnapshot/nsb1_nim_crossread_test.go`: a cross-read can only
// run where both checkouts and both toolchains are present, and it says out
// loud when it does not run. **Never make a failure go away by arranging for
// the skip.**
func TestTheProductionNimReaderReadsAnFfiWrittenContainer(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(filepath.Dir(filepath.Dir(wd)), "..", "codetracer-trace-format-nim")
	checker := filepath.Join(repo, "tests", "check_ctfs_container.nim")
	if _, err := os.Stat(checker); err != nil {
		t.Skipf(
			"SKIP: the sibling codetracer-trace-format-nim checkout has no %s "+
				"(looked in %s). The FFI-written container is still adjudicated by "+
				"this package's reader and by the transcribed producer resolver; what "+
				"did NOT run is the check against the production Nim reader itself.",
			filepath.Base(checker), repo)
	}
	direnv := ""
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".nix-profile", "bin", "direnv")
		if _, err := os.Stat(p); err == nil {
			direnv = p
		}
	}
	if direnv == "" {
		if p, err := exec.LookPath("direnv"); err == nil {
			direnv = p
		}
	}
	if direnv == "" {
		t.Skip(
			"SKIP: direnv is not available, and the sibling repo's Nim toolchain is " +
				"supplied by its own nix dev shell rather than by this one, so the " +
				"production Nim reader cannot be built. The two in-repo cross-reads " +
				"still ran.")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("SKIP: no home directory, so direnv cannot be run: %v", err)
	}

	path, want := crossFixture(t)
	tmp := t.TempDir()
	var manifest strings.Builder
	for name, body := range want {
		expectPath := filepath.Join(tmp, "expect-"+strings.ReplaceAll(name, "/", "_"))
		if err := os.WriteFile(expectPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&manifest, "%s %s\n", name, expectPath)
	}
	// Negative controls: names the container must NOT carry, so a reader that
	// resolved everything cannot pass.
	for _, absent := range []string{"nothere.dat", "absent.ns", "missing.idx"} {
		fmt.Fprintf(&manifest, "!%s\n", absent)
	}
	manifestPath := filepath.Join(tmp, "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte(manifest.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// `env -i` is deliberate: this repo's dev shell is on the environment and
	// the sibling repo's is not, and letting the two mix is how a cross-read
	// ends up proving something about the wrong toolchain. Build artefacts go
	// into the test's temp dir so nothing is left behind in the sibling.
	args := []string{
		"-i", "HOME=" + home, "PATH=/run/current-system/sw/bin:/usr/bin:/bin",
		direnv, "exec", repo,
		"nim", "c", "-r", "-d:release", "-p:src", "--hints:off",
		"--nimcache:" + filepath.Join(tmp, "nimcache"),
		"-o:" + filepath.Join(tmp, "check_ctfs_container"),
		"tests/check_ctfs_container.nim", path, manifestPath,
	}
	cmd := exec.Command("env", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	t.Logf("env %s\n%s", strings.Join(args, " "), out)
	if err != nil {
		// A missing toolchain is a skip; a reader that rejected the container
		// is a failure. They are told apart by whether the checker ran at all.
		if bytes.Contains(out, []byte("check_ctfs_container:")) ||
			bytes.Contains(out, []byte("Error:")) {
			t.Fatalf("the production Nim reader could not read the container the "+
				"append FFI wrote: %v\n%s", err, out)
		}
		t.Skipf(
			"SKIP: could not run the sibling repo's Nim toolchain (%v), so the "+
				"production-Nim-reader corner of the append proof did not run. The "+
				"other two corners did.\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("check_ctfs_container: OK")) {
		t.Fatalf("the Nim checker exited 0 without reporting a pass:\n%s", out)
	}
}

// TestAppendRefusesToOverwriteAnExistingStream: CTFS is append-only, and a
// stale-but-present stream is the "returns wrong bytes" failure the snapshot
// design must not have. The refusal lives in the canonical writer now, so
// this checks it survives the FFI boundary rather than only the Nim one.
func TestAppendRefusesToOverwriteAnExistingStream(t *testing.T) {
	path, _ := crossFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = ctfsffi.Append(path, map[string][]byte{"meta.dat": []byte("replacement")})
	if err == nil {
		t.Fatal("an existing internal file was overwritten")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused append modified the container")
	}
}

// TestAppendRefusesAnUnencodableName: the Nim `base40Encode` maps anything
// outside the alphabet to padding, so `"snap!pages"` would otherwise be
// stored — silently — under the name `"snap"`.
func TestAppendRefusesAnUnencodableName(t *testing.T) {
	path, _ := crossFixture(t)
	for _, bad := range []string{"snap!pages", "SNAPSHOT.IDX", "thirteenchars"} {
		if err := ctfsffi.Append(path, map[string][]byte{bad: []byte("x")}); err == nil {
			t.Errorf("the unencodable internal filename %q was accepted", bad)
		}
	}
}

// blockCountOf is a small helper used by the sizing assertion below.
func blockCountOf(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size()%crossBlockSize != 0 {
		t.Fatalf("%s is %d bytes, not a whole number of %d-byte blocks",
			path, st.Size(), crossBlockSize)
	}
	return st.Size() / crossBlockSize
}

// TestAppendExtendsRatherThanRewrites: the append must only ever add blocks
// past the end and rewrite block 0. This is what makes a crash mid-append
// leave a readable container rather than an entry pointing at absent data,
// and it is checked from the outside — the bytes between block 0 and the old
// end of file must be identical afterwards.
func TestAppendExtendsRatherThanRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extend.ct")
	if err := ctfsffi.Create(path, crossBlockSize); err != nil {
		t.Fatal(err)
	}
	if err := ctfsffi.Append(path, map[string][]byte{
		"steps.dat": crossPattern(300000, 11),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocksBefore := blockCountOf(t, path)

	if err := ctfsffi.Append(path, map[string][]byte{
		"snapshot.mem": crossPattern(crossBigSize, 12),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(after)/crossBlockSize) <= blocksBefore {
		t.Fatalf("the container did not grow: %d blocks before, %d after",
			blocksBefore, len(after)/crossBlockSize)
	}
	if !bytes.Equal(before[crossBlockSize:], after[crossBlockSize:len(before)]) {
		d := commonPrefix(before[crossBlockSize:], after[crossBlockSize:len(before)])
		t.Fatalf("the append rewrote an existing block: first difference at "+
			"container block %d", 1+d/crossBlockSize)
	}
	// Block 0 must have changed — that is where the new entry lives.
	if bytes.Equal(before[:crossBlockSize], after[:crossBlockSize]) {
		t.Fatal("block 0 was not rewritten, so the new stream cannot be reachable")
	}
	// And the new entry must be the only difference in block 0: the header
	// and every pre-existing entry are untouched.
	diffs := 0
	for i := 0; i < crossBlockSize; i += 8 {
		if !bytes.Equal(before[i:i+8], after[i:i+8]) {
			diffs++
		}
	}
	// One 24-byte FileEntry is three u64 words.
	if diffs != 3 {
		t.Errorf("block 0 changed in %d u64 word(s); an append that adds one "+
			"internal file should change exactly the three words of its entry", diffs)
	}
	// The header word carrying the magic and block size must be byte-identical.
	if !bytes.Equal(before[:16], after[:16]) {
		t.Error("the append rewrote the container header")
	}
}

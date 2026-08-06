package wasmsnapshot

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/xxh3"
)

// The second half of the `NSB1` proof: the **real** Nim reader, not a
// transcription of it.
//
// `nsb1_crossread_test.go` holds a `snappages.ns` to a Go transcription of
// `cow_btree.nim`'s `loadCowBTree` / `lookupFrom`. That is strong — the
// transcription shares no code with the writer — but it is still this
// repository's opinion of what the producer's algorithm is. This test removes
// that last degree of freedom by handing the stream to
// `codetracer-trace-format-nim`'s production `loadCowBTree(image, cltTypeB)`
// and `lookup`, compiled and run out of the sibling checkout.
//
// This is the same shape `codetracer-trace-format-nim`'s own
// `test_nim_{step,value,io_event}_stream_crossread.nim` tests use to reach the
// Rust reader, including the skip arm: a cross-read can only run where both
// checkouts and both toolchains are present, and it says out loud when it does
// not run. **Never make a failure go away by removing the toolchain so the
// skip arm engages.**
//
// NO MOCKS: the image is the production encoder's output over real pages, and
// the reader is the production Nim one.

// nimTraceFormatRepo locates the sibling `codetracer-trace-format-nim`
// checkout, the same fixed-sibling-path convention `ctPrintPath` uses in
// `cmd/wazero`.
func nimTraceFormatRepo(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Repo root is two levels up from internal/wasmsnapshot/.
	root := filepath.Dir(filepath.Dir(wd))
	return filepath.Join(root, "..", "codetracer-trace-format-nim")
}

// direnvPath finds direnv, which is how the sibling repo's Nim toolchain is
// entered without importing this repo's dev shell (their flakes differ).
func direnvPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".nix-profile", "bin", "direnv")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("direnv"); err == nil {
		return p
	}
	return ""
}

func TestTheProductionNimReaderLooksUpEveryPage(t *testing.T) {
	repo := nimTraceFormatRepo(t)
	checker := filepath.Join(repo, "tests", "check_nsb1_namespace.nim")
	if _, err := os.Stat(checker); err != nil {
		t.Skipf(
			"SKIP: the sibling codetracer-trace-format-nim checkout has no %s "+
				"(looked in %s). The `snappages.ns` namespace is still held to a "+
				"transcription of that repo's reader by "+
				"TestTheProducersTraversalFindsEveryPageInTheStore; what did NOT run "+
				"is the check against the production Nim reader itself.",
			filepath.Base(checker), repo)
	}
	direnv := direnvPath()
	if direnv == "" {
		t.Skip(
			"SKIP: direnv is not available, and the sibling repo's Nim toolchain is " +
				"supplied by its own nix dev shell rather than by this one, so the " +
				"production Nim reader cannot be built. The transcription-based proof " +
				"in TestTheProducersTraversalFindsEveryPageInTheStore still ran.")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("SKIP: no home directory, so direnv cannot be run: %v", err)
	}

	// A store big enough to need an internal node (Leaf Type B holds 170 keys
	// per leaf), so the Nim reader really has to descend the tree.
	const n = 200
	c := NewPerTraceCache()
	hashes := make([]xxh3.Uint128, 0, n)
	for i := int64(1); i <= n; i++ {
		p := page(i)
		h := HashPage(p)
		if err := c.Insert(h, p); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	raw := encodePageStore(c)

	// The expected descriptors are derived from first principles rather than
	// read back with either of this package's readers, so the manifest cannot
	// agree with a mis-built tree by construction:
	//
	//   * every record is `pageRecordHeaderSize + PageSize` bytes, and there is
	//     exactly one per key here (no truncation collision at n = 200);
	//   * the payload region ends flush with the end of the stream, so it
	//     starts at `len(raw) - n*recordSize`;
	//   * keys are laid out in ascending order.
	const recordSize = pageRecordHeaderSize + PageSize
	payloadBase := len(raw) - n*recordSize
	if payloadBase < nsPageSize || len(raw)%nsPageSize != 0 {
		t.Fatalf("a %d-byte stream leaves %d byte(s) before its payload; expected a "+
			"page-aligned stream whose payload follows at least the header page",
			len(raw), payloadBase)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].Lo < hashes[j].Lo })

	var manifest strings.Builder
	present := map[uint64]bool{}
	for i, h := range hashes {
		if i > 0 && h.Lo == hashes[i-1].Lo {
			t.Fatalf("two pages truncate to the same key %#x; this fixture assumes "+
				"none do", h.Lo)
		}
		present[h.Lo] = true
		off := payloadBase + i*recordSize
		// Cross-check the derivation against the bytes: the record at the
		// offset the manifest will demand must be this page's.
		var hb [16]byte
		copy(hb[:], raw[off:off+16])
		if got := xxh3.Uint128FromBytes(hb); got != h {
			t.Fatalf("the derived offset %d for key %#x holds full hash %x", off, h.Lo, got)
		}
		desc := make([]byte, pageStoreDescSize)
		binary.LittleEndian.PutUint64(desc[0:], uint64(off))
		binary.LittleEndian.PutUint64(desc[8:], recordSize)
		fmt.Fprintf(&manifest, "%d %s\n", h.Lo, hex.EncodeToString(desc))
	}
	// Negative controls, so a reader that resolved everything cannot pass.
	absent := 0
	for k := uint64(1); absent < 16; k++ {
		if present[k] {
			continue
		}
		fmt.Fprintf(&manifest, "!%d\n", k)
		absent++
	}

	tmp := t.TempDir()
	imagePath := filepath.Join(tmp, "snappages.ns")
	manifestPath := filepath.Join(tmp, "manifest.txt")
	if err := os.WriteFile(imagePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifest.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// `env -i` is deliberate: this repo's dev shell is on the environment and
	// the sibling repo's is not, and letting the two mix is how a cross-read
	// ends up proving something about the wrong toolchain. The build artefacts
	// go into the test's temp dir so a test never leaves anything behind in a
	// sibling checkout.
	args := []string{
		"-i", "HOME=" + home, "PATH=/run/current-system/sw/bin:/usr/bin:/bin",
		direnv, "exec", repo,
		"nim", "c", "-r", "-d:release", "-p:src", "--hints:off",
		"--nimcache:" + filepath.Join(tmp, "nimcache"),
		"-o:" + filepath.Join(tmp, "check_nsb1_namespace"),
		"tests/check_nsb1_namespace.nim", imagePath, manifestPath,
	}
	cmd := exec.Command("env", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	t.Logf("env %s\n%s", strings.Join(args, " "), out)
	if err != nil {
		// A missing toolchain is a skip; a reader that rejected the namespace
		// is a failure. They are told apart by whether the checker ran at all.
		if bytes.Contains(out, []byte("check_nsb1_namespace:")) ||
			bytes.Contains(out, []byte("Error:")) {
			t.Fatalf("the production Nim loadCowBTree/lookup could not read the "+
				"snappages.ns namespace this package wrote: %v\n%s", err, out)
		}
		t.Skipf(
			"SKIP: could not run the sibling repo's Nim toolchain (%v), so the "+
				"production-Nim-reader half of the NSB1 proof did not run. The "+
				"transcription-based half in "+
				"TestTheProducersTraversalFindsEveryPageInTheStore did.\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("check_nsb1_namespace: OK")) {
		t.Fatalf("the Nim checker exited 0 without reporting a pass:\n%s", out)
	}
}

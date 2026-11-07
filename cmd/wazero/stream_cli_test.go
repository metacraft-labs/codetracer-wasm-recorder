package main

// The `--boundary-stream` CLI surface. It is untagged, so it runs in **both**
// build variants: streaming is a change to *when* a recording is consumed, not
// to what is derived from it, and materialisation is open
// (`WASM-Replay-Snapshots-And-Slices.md` §9).
//
// No mocks: the bytes streamed are the committed browser recording's own
// boundary log, the producer is a goroutine writing into a real pipe, and the
// trace is produced by the shipped code path.

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/internal/boundarylog/ctbltest"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

const committedRecording = "testdata/boundary-log/frontend-wasm.ct"

// repeatRecording writes a boundary recording whose record list is the
// committed one's, repeated `n` times — an `n`-call recording of the same
// exported call — and returns its `.ct` and its boundary log.
//
// Repetition is faithful rather than convenient: the producer's `Function`,
// `VariableName` and `Path` tables are append-only and its `Call` /`Value`
// records index them positionally, so a second copy of the record list
// re-registers the same names at new ids and refers to the *original* ids,
// which resolve to the same entries. The result parses to `n` identical
// exported calls — verified by the replay itself, which checks every recorded
// return value.
func repeatRecording(t *testing.T, n int) (string, []byte) {
	t.Helper()
	log, err := ctbltest.ReadLog(committedRecording)
	require.NoError(t, err)
	h, records, _, err := ctbltest.Decode(log)
	require.NoError(t, err)

	var all []map[string]any
	for i := 0; i < n; i++ {
		all = append(all, records...)
	}
	out := ctbltest.Encode(h, all, true)
	path := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, ctbltest.WriteRecording(path, out))
	return path, out
}

// runMainStreaming runs `wazero` with `raw` written to its stdin by a
// goroutine, in pieces, the way `record-web` feeds its consumer.
func runMainStreaming(t *testing.T, raw []byte, args []string) (int, string, string) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < len(raw); i += 128 {
			end := i + 128
			if end > len(raw) {
				end = len(raw)
			}
			if _, err := w.Write(raw[i:end]); err != nil {
				break
			}
		}
		_ = w.Close()
	}()
	code, stdout, stderr := runMain(t, "", args)
	_ = r.Close()
	wg.Wait()
	return code, stdout, stderr
}

// TestBoundaryStreamFromStdin is the shape `record-web --snapshot-consumer`
// has: the daemon writes the boundary log to this process's stdin as the page
// produces it, and closing the pipe ends the stream unambiguously.
//
// The produced trace must be identical to the one the non-streaming path
// produces from the same recording, because streaming changes when the bytes
// are consumed and nothing else.
func TestBoundaryStreamFromStdin(t *testing.T) {
	src, raw := repeatRecording(t, 3)

	batchOut := t.TempDir()
	code, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + src, "--out-dir=" + batchOut,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "stderr:\n%s", stderr)

	out := t.TempDir()
	code, stdout, stderr := runMainStreaming(t, raw, []string{
		"run", "--boundary-stream=-", "--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("replayed 3 exported call(s)")),
		"the streamed run did not replay every call: %s", stdout)
	require.True(t, ctfsHas(t, filepath.Join(out, "balance_calc.ct"), "steps.dat"),
		"the streamed run produced no step stream")
	requireSameTraceStreams(t,
		filepath.Join(batchOut, "balance_calc.ct"),
		filepath.Join(out, "balance_calc.ct"))
}

// TestBoundaryStreamRefusesAFileOrAFinishedRecording: the stream carries its
// own metadata and is only ever a pipe. Following a file was the retired
// `trace.json` layout's shape, and naming a finished `.ct` alongside a stream
// would give the run two recordings.
func TestBoundaryStreamRefusesAFileOrAFinishedRecording(t *testing.T) {
	src, _ := repeatRecording(t, 1)
	code, _, stderr := runMain(t, "", []string{
		"run", "--boundary-stream=" + src, "testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 1, code)
	require.True(t, bytes.Contains([]byte(stderr), []byte("read from stdin")), stderr)

	code, _, stderr = runMain(t, "", []string{
		"run", "--boundary-log=" + src, "--boundary-stream=-",
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 1, code)
	require.True(t, bytes.Contains([]byte(stderr), []byte("drop --boundary-log")), stderr)
}

// TestBoundaryStreamReportsATruncatedProducer: the pipe closes while the log
// is incomplete — the page was killed. The calls that did complete are still
// materialised — a truncated recording is a prefix, not a write-off — and the
// warning says so.
func TestBoundaryStreamReportsATruncatedProducer(t *testing.T) {
	_, raw := repeatRecording(t, 4)
	frames, err := ctbltest.Frames(raw)
	require.NoError(t, err)
	// Keep everything up to and including the second call's `Return`.
	cut, returns := 5, 0
	for _, f := range frames {
		cut += len(f)
		if f[4] == ctbltest.TagReturn {
			returns++
			if returns == 2 {
				break
			}
		}
	}
	require.Equal(t, 2, returns)

	out := t.TempDir()
	code, stdout, stderr := runMainStreaming(t, raw[:cut], []string{
		"run", "--boundary-stream=-", "--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "a truncated recording still materialises its prefix; stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stderr), []byte("warning:")), stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("replayed 2 exported call(s)")),
		"the prefix was not materialised: %s", stdout)
}

// requireSameTraceStreams asserts two containers hold identical trace streams,
// modulo the per-container UUIDv7 trace identifier in `meta.dat`, which names
// the container rather than the execution.
func requireSameTraceStreams(t *testing.T, wantPath, gotPath string) {
	t.Helper()
	want, err := ctfs.Open(wantPath)
	require.NoError(t, err)
	got, err := ctfs.Open(gotPath)
	require.NoError(t, err)
	require.Equal(t, want.Names(), got.Names())
	for _, name := range want.Names() {
		a, err := want.ReadFile(name)
		require.NoError(t, err)
		b, err := got.ReadFile(name)
		require.NoError(t, err)
		if name == "meta.dat" {
			// The trace id is a 36-byte UUID after the 12-byte header
			// (magic, version, flags, flags_ext) and its one-byte length
			// prefix; blank it in both. `TestContainerBytesDifferOnlyInTheTraceID`
			// in `internal/boundarylog` establishes it is the only field two
			// materialisations of the same range ever disagree about.
			const idLenOffset, idLen = 12, 36
			for _, m := range [][]byte{a, b} {
				require.True(t, len(m) > idLenOffset+idLen,
					"meta.dat is too short to hold the trace id")
				require.Equal(t, byte(idLen), m[idLenOffset],
					"meta.dat byte %d is not the trace-id length prefix", idLenOffset)
			}
			a, b = append([]byte(nil), a...), append([]byte(nil), b...)
			for i := idLenOffset + 1; i <= idLenOffset+idLen; i++ {
				a[i], b[i] = '?', '?'
			}
		}
		require.True(t, bytes.Equal(a, b), "stream %q differs between %s and %s",
			name, wantPath, gotPath)
	}
}

func ctfsHas(t *testing.T, path, name string) bool {
	t.Helper()
	c, err := ctfs.Open(path)
	require.NoError(t, err)
	if !c.Has(name) {
		return false
	}
	b, err := c.ReadFile(name)
	require.NoError(t, err)
	return len(b) > 0
}

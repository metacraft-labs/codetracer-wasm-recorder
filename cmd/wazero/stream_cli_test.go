package main

// The `--boundary-stream` CLI surface. It is untagged, so it runs in **both**
// build variants: streaming is a change to *when* a recording is consumed, not
// to what is derived from it, and materialisation is open
// (`WASM-Replay-Snapshots-And-Slices.md` §9).
//
// No mocks: the bytes streamed are the committed browser recording's own, the
// producer is a goroutine appending to a real file (or writing into a real
// pipe), and the trace is produced by the shipped code path.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

const committedRecording = "testdata/boundary-log/frontend-wasm.ct"

// repeatRecording writes a boundary recording whose record list is the
// committed one's, repeated `n` times — an `n`-call recording of the same
// exported call.
//
// Repetition is faithful rather than convenient: the producer's `Function`,
// `VariableName` and `Path` tables are append-only and its `Call` /`Value`
// records index them positionally, so a second copy of the record list
// re-registers the same names at new ids and refers to the *original* ids,
// which resolve to the same entries. The result parses to `n` identical
// exported calls — verified by the replay itself, which checks every recorded
// return value.
func repeatRecording(t *testing.T, n int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(committedRecording, "trace.json"))
	require.NoError(t, err)
	var records []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &records))

	var all []json.RawMessage
	for i := 0; i < n; i++ {
		all = append(all, records...)
	}
	out, err := json.Marshal(all)
	require.NoError(t, err)

	dir := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "trace.json"), out, 0o644))
	for _, f := range []string{"trace_metadata.json", "trace_paths.json"} {
		b, err := os.ReadFile(filepath.Join(committedRecording, f))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), b, 0o644))
	}
	return dir
}

// dribble writes `raw` into `path` in small pieces, then creates `doneMarker`.
// It models a producer that is still running while the recorder consumes.
func dribble(t *testing.T, wg *sync.WaitGroup, path, doneMarker string, raw []byte) {
	t.Helper()
	wg.Add(1)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		for i := 0; i < len(raw); i += 64 {
			end := i + 64
			if end > len(raw) {
				end = len(raw)
			}
			if _, err := f.Write(raw[i:end]); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(time.Millisecond)
		}
		if err := os.WriteFile(doneMarker, nil, 0o644); err != nil {
			t.Error(err)
		}
	}()
}

// TestBoundaryStreamFollowsAGrowingRecording drives the file-following shape:
// a producer appends to `trace.json` and the recorder stops when the completion
// marker appears.
//
// **No producer has this shape yet.** `record-web`'s `JsonFileCtfsWriter`
// buffers every event and writes `trace.json` once, at session end (see
// `codetracer/src/backend-manager/src/browser_stream_host.rs`), so the file it
// produces cannot be followed as it grows — it does not grow. This exercises
// the recorder's half of a contract the daemon has still to meet.
//
// The produced trace must be identical to the one the non-streaming path
// produces from the same recording, because streaming changes when the bytes
// are consumed and nothing else.
func TestBoundaryStreamFollowsAGrowingRecording(t *testing.T) {
	src := repeatRecording(t, 4)
	raw, err := os.ReadFile(filepath.Join(src, "trace.json"))
	require.NoError(t, err)

	// The reference: the same recording, replayed the ordinary way.
	batchOut := t.TempDir()
	code, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + src, "--out-dir=" + batchOut,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "stderr:\n%s", stderr)

	// The streamed run, against a `.ct` whose trace.json starts empty.
	live := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "trace.json"), nil, 0o644))
	for _, f := range []string{"trace_metadata.json", "trace_paths.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(live, f), b, 0o644))
	}
	marker := filepath.Join(t.TempDir(), "done")

	var wg sync.WaitGroup
	dribble(t, &wg, filepath.Join(live, "trace.json"), marker, raw)

	streamOut := t.TempDir()
	code, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + live,
		"--boundary-stream=" + filepath.Join(live, "trace.json"),
		"--stream-done=" + marker,
		"--out-dir=" + streamOut,
		"testdata/boundary-log/balance_calc.wasm",
	})
	wg.Wait()
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("replayed 4 exported call(s)")),
		"the streamed run did not replay every call: %s", stdout)

	requireSameTraceStreams(t,
		filepath.Join(batchOut, "balance_calc.ct"),
		filepath.Join(streamOut, "balance_calc.ct"))
}

// TestBoundaryStreamFromStdin is the shape a daemon-side tee has, and the one
// the daemon should grow: the receiver writes the same bytes to the `.ct` and
// to this process's stdin, and closing the pipe ends the stream unambiguously.
func TestBoundaryStreamFromStdin(t *testing.T) {
	src := repeatRecording(t, 3)
	raw, err := os.ReadFile(filepath.Join(src, "trace.json"))
	require.NoError(t, err)

	// A `.ct` carrying the metadata but no events: the stream carries those.
	live := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "trace.json"), nil, 0o644))
	for _, f := range []string{"trace_metadata.json", "trace_paths.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(live, f), b, 0o644))
	}

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
				t.Error(err)
				break
			}
		}
		_ = w.Close()
	}()

	out := t.TempDir()
	code, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + live,
		"--boundary-stream=-",
		"--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	wg.Wait()
	_ = r.Close()
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("replayed 3 exported call(s)")),
		"the streamed run did not replay every call: %s", stdout)
	require.True(t, ctfsHas(t, filepath.Join(out, "balance_calc.ct"), "steps.dat"),
		"the streamed run produced no step stream")
}

// TestBoundaryStreamFileWithoutDoneIsRefused: a file has no end of stream, so
// following one without being told when the producer stops would either hang
// forever or truncate the recording. Refusing is the only honest option, and
// the message says which of the two alternatives to use.
func TestBoundaryStreamFileWithoutDoneIsRefused(t *testing.T) {
	src := repeatRecording(t, 1)
	code, _, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + src,
		"--boundary-stream=" + filepath.Join(src, "trace.json"),
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 1, code)
	require.True(t, bytes.Contains([]byte(stderr), []byte("--stream-done")), stderr)
}

// TestBoundaryStreamReportsATruncatedProducer: the marker appears while the
// document is incomplete. The calls that did complete are still materialised —
// a truncated recording is a prefix, not a write-off — and the warning says so.
func TestBoundaryStreamReportsATruncatedProducer(t *testing.T) {
	src := repeatRecording(t, 4)
	raw, err := os.ReadFile(filepath.Join(src, "trace.json"))
	require.NoError(t, err)
	// Cut just after the second call's `Return`, so two calls are complete.
	cut := nthIndex(raw, []byte(`{"Return"`), 2)
	require.True(t, cut > 0)
	cut += len(`{"Return":{"return_value":{"i":"620","kind":"Int","type_id":0}}}`)

	live := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "trace.json"), raw[:cut], 0o644))
	for _, f := range []string{"trace_metadata.json", "trace_paths.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(live, f), b, 0o644))
	}
	marker := filepath.Join(t.TempDir(), "done")
	require.NoError(t, os.WriteFile(marker, nil, 0o644)) // the producer is gone

	out := t.TempDir()
	code, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + live,
		"--boundary-stream=" + filepath.Join(live, "trace.json"),
		"--stream-done=" + marker,
		"--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "a truncated recording still materialises its prefix; stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stderr), []byte("warning:")), stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("replayed 2 exported call(s)")),
		"the prefix was not materialised: %s", stdout)
}

// nthIndex returns the index of the n-th (1-based) occurrence of sep.
func nthIndex(b, sep []byte, n int) int {
	at := 0
	for i := 0; i < n; i++ {
		j := bytes.Index(b[at:], sep)
		if j < 0 {
			return -1
		}
		at += j
		if i < n-1 {
			at += len(sep)
		}
	}
	return at
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
			// The trace id is a 36-byte UUID at a fixed offset; blank it in
			// both. `TestContainerBytesDifferOnlyInTheTraceID` in
			// `internal/boundarylog` establishes it is the only field two
			// materialisations of the same range ever disagree about.
			a, b = append([]byte(nil), a...), append([]byte(nil), b...)
			for i := 9; i < 9+36 && i < len(a) && i < len(b); i++ {
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

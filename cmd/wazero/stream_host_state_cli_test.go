// stream_host_state_cli_test.go — M44b at the CLI.
//
// `internal/boundarylog/stream_host_state_test.go` drives the property at
// package level. This drives the same thing through the real `run`
// command, because the flag plumbing is its own surface: `--boundary-log`
// supplies the metadata, `--boundary-stream` supplies the bytes, and
// `--stream-done` says when the producer stopped. A package-level test
// cannot show that those three still compose once host state has to come
// from the stream rather than from a file.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

const (
	vaultCorpusDir       = corpusTestdata + "/vault_apply"
	vaultCorpusWasm      = vaultCorpusDir + "/vault_apply.wasm"
	vaultCorpusRecording = vaultCorpusDir + "/vault-apply.ct"
)

// copyRecordingWithoutSidecar reproduces the shape a recording has while
// it is still being produced: a `trace.json` the daemon is appending to,
// and no `boundary_state.json`, because the sidecar is a rendering of
// records the daemon has already put into the stream.
func copyRecordingWithoutSidecar(t *testing.T, src, dst string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dst, 0o755))
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	copied := 0
	for _, e := range entries {
		if e.IsDir() || e.Name() == "boundary_state.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600))
		copied++
	}
	require.True(t, copied >= 1, "%s held nothing to copy", src)
	require.True(t, !fileExists(filepath.Join(dst, "boundary_state.json")),
		"the copy must carry no sidecar, or it proves nothing")
	return dst
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestBoundaryStreamServesAnImportedMemoryModule is the CLI half of
// M44b's `verify_streaming_replay_applies_host_state`.
//
// Before M44b this exact invocation failed at startup with
//
//	the module imports memory env.memory, but the boundary recording
//	carries no initial contents for it
//
// which is an accurate message about a recording whose sidecar had not
// been written yet — and which excluded every Stylus contract and every
// `wasm-bindgen` glue layer from the streaming pipeline. Now the §3.3
// record arrives in the stream, immediately before the first exported
// call, and the same invocation materialises the same trace the batch
// path does.
func TestBoundaryStreamServesAnImportedMemoryModule(t *testing.T) {
	requireCtPrint(t)
	tmp := t.TempDir()

	// --- the batch reference, from the committed recording -------------
	outBatch := filepath.Join(tmp, "batch")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + vaultCorpusRecording,
		"--out-dir=" + outBatch, vaultCorpusWasm})
	require.Equal(t, 0, exitCode, "batch replay failed; stderr:\n%s", stderr)
	want := dumpFull(t, outBatch)

	// --- the live shape: a stream and no sidecar ------------------------
	live := copyRecordingWithoutSidecar(t, vaultCorpusRecording, filepath.Join(tmp, "live"))
	done := filepath.Join(live, ".complete")
	require.NoError(t, os.WriteFile(done, nil, 0o600))

	outStream := filepath.Join(tmp, "streamed")
	exitCode, stdout, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + live,
		"--boundary-stream=" + filepath.Join(live, "trace.json"),
		"--stream-done=" + done,
		"--out-dir=" + outStream, vaultCorpusWasm})
	require.Equal(t, 0, exitCode,
		"streaming replay of an imported-memory recording failed; stderr:\n%s", stderr)
	require.True(t, len(stdout) > 0, "the CLI should report what it replayed")
	got := dumpFull(t, outStream)

	require.Equal(t, want.Paths, got.Paths, "path tables differ")
	require.Equal(t, want.Functions, got.Functions, "function tables differ")
	require.Equal(t, want.Varnames, got.Varnames, "variable-name tables differ")
	require.Equal(t, want.Counts, got.Counts, "counts differ")
	require.Equal(t, len(want.Events), len(got.Events), "event counts differ")
	for i := range want.Events {
		require.Equal(t, string(want.Events[i]), string(got.Events[i]),
			"event %d differs between the batch and the streaming replay", i)
	}
	require.True(t, got.Counts["steps"] > 0,
		"the streamed trace must carry real DWARF-driven steps, or the "+
			"comparison above is between two empty documents")
}

// TestBoundaryStreamStillRefusesAModuleWhoseStateNeverArrives is the
// control: the refusal M44b relaxed must still fire when the state is
// genuinely absent rather than merely late.
//
// The recording here carries neither the sidecar nor the in-stream §3.3
// record, so no amount of waiting would produce one. A streaming replay
// that accepted it would be replaying against a zeroed memory — the
// silent degradation spec §8 exists to prevent.
func TestBoundaryStreamStillRefusesAModuleWhoseStateNeverArrives(t *testing.T) {
	tmp := t.TempDir()
	live := copyRecordingWithoutSidecar(t, vaultCorpusRecording, filepath.Join(tmp, "live"))

	// Strip the in-stream host-state records too. Done on the raw text
	// because the records are the only ones naming this boundary, and a
	// textual filter cannot accidentally drop a crossing.
	raw, err := os.ReadFile(filepath.Join(live, "trace.json"))
	require.NoError(t, err)
	stripped := stripHostStateRecords(t, raw)
	require.True(t, len(stripped) < len(raw), "nothing was stripped")
	require.NoError(t, os.WriteFile(filepath.Join(live, "trace.json"), stripped, 0o600))

	done := filepath.Join(live, ".complete")
	require.NoError(t, os.WriteFile(done, nil, 0o600))

	outDir := filepath.Join(tmp, "traces")
	exitCode, _, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + live,
		"--boundary-stream=" + filepath.Join(live, "trace.json"),
		"--stream-done=" + done,
		"--out-dir=" + outDir, vaultCorpusWasm})
	require.Equal(t, 1, exitCode,
		"a recording that never supplies its §3.3 state must still be refused")
	require.True(t, containsAll(stderr, "env.memory", "no initial contents for it"),
		"the refusal must name the memory it wanted; stderr:\n%s", stderr)
	require.Equal(t, 0, len(tracePaths(t, outDir)),
		"a refused replay must leave nothing behind, but %s contains: %v",
		outDir, tracePaths(t, outDir))
}

// stripHostStateRecords removes every in-stream host-state record from a
// `trace.json`.
//
// It decodes and re-encodes the record array rather than editing the text,
// so what it leaves behind is a well-formed document: the control above
// must fail because an INPUT is missing, not because the recording no
// longer parses.
func stripHostStateRecords(t *testing.T, raw []byte) []byte {
	t.Helper()
	var records []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &records))
	kept := make([]json.RawMessage, 0, len(records))
	for _, r := range records {
		if strings.Contains(string(r), "wasm-host-state") {
			continue
		}
		kept = append(kept, r)
	}
	require.True(t, len(kept) < len(records),
		"the fixture carries no in-stream host-state record, so this control "+
			"is no longer testing anything")
	out, err := json.Marshal(kept)
	require.NoError(t, err)
	return out
}

// containsAll reports whether `s` contains every fragment.
func containsAll(s string, fragments ...string) bool {
	for _, f := range fragments {
		if !strings.Contains(s, f) {
			return false
		}
	}
	return true
}

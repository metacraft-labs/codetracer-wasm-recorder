package boundarylog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// StreamChunksForRecording splits a recording's `trace.json` into the pieces a
// producer would emit call by call.
//
// Concatenating every chunk reproduces the file exactly. Chunk *i* (for i less
// than the number of exported calls) ends with the `Return` record that closes
// call *i*, so feeding chunks one at a time to a `StreamReader` yields exactly
// one call group per chunk; the last chunk is the document's tail and its
// closing bracket.
//
// It exists so the streaming tests can drive a producer at a granularity the
// replay can be observed against, without a second renderer of the browser
// format: the bytes are the ones `recordingBuilder` wrote, which
// `TestBuilderReproducesTheCommittedBrowserRecording` pins against the real
// browser output.
func StreamChunksForRecording(t *testing.T, ctDir string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(ctDir, "trace.json"))
	require.NoError(t, err)
	var records []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &records))

	var (
		chunks [][]byte
		cur    bytes.Buffer
		first  = true
	)
	flush := func() {
		chunks = append(chunks, append([]byte(nil), cur.Bytes()...))
		cur.Reset()
	}
	cur.WriteByte('[')
	for _, r := range records {
		if !first {
			cur.WriteByte(',')
		}
		first = false
		cur.Write(r)
		// Only an *export* crossing emits a `Return`, and it is the record
		// that closes the crossing — so this is one chunk per exported call.
		if bytes.Contains(r, []byte(`"Return"`)) {
			flush()
		}
	}
	cur.WriteByte(']')
	flush()

	var joined []byte
	for _, c := range chunks {
		joined = append(joined, c...)
	}
	require.Equal(t, len(raw), len(joined),
		"the chunks do not reassemble into the recording")
	return chunks
}

// This file exposes the producer replica in `browser_format_test.go` to the
// *external* test package `boundarylog_test`, which is where the snapshot
// seek-equivalence tests live (they need `internal/wasmsnapshot`, and that
// package imports this one, so they cannot live in `package boundarylog`).
//
// No mocking is involved: `recordingBuilder` is a second implementation of
// the real browser producers, pinned against the committed browser recording
// by `TestBuilderReproducesTheCommittedBrowserRecording`. See that file's
// header for the full statement of what the pin does and does not cover.

// BuildComputeBalanceRecording writes a boundary recording of `len(args)`
// successive top-level `compute_balance` calls, in the exact on-disk shape
// the browser pipeline produces, and returns the `.ct` directory.
//
// The module is the committed `balance_calc.wasm`, whose exported
// `compute_balance(user_id, amount)` returns `user_id*10 + amount*2` — a
// pure function of its arguments, so a faithful recording is fully
// determined by the argument pairs.
//
// A multi-call recording is what makes the snapshot tests meaningful: a
// recording with one exported call has only the trivial quiescent points
// either side of it and no sub-range to seek into.
func BuildComputeBalanceRecording(t *testing.T, dir string, args [][2]int32) string {
	t.Helper()
	b := newRecordingBuilder("balance_calc")
	for _, a := range args {
		userID, amount := a[0], a[1]
		result := userID*10 + amount*2
		b.export("compute_balance", 1,
			"/home/zahary/m/js-support/codetracer/src/db-backend/tests/fixtures/cross_process/account-balance-with-wasm/wasm-src/lib.rs",
			71,
			[]jsValue{jsInt(userID), jsInt(amount)},
			[]jsValue{jsInt(result)}, nil)
	}
	return b.write(t, dir)
}

// GrowMemCall names one exported call of the `grow_mem` fixture module
// (`internal/wasmsnapshot/testdata/grow_mem.wat`).
type GrowMemCall struct {
	// Fn is "bump", "size" or "calls".
	Fn string
	// Arg is `bump`'s stored marker value; ignored by the other two.
	Arg int32
}

// growMemExportIndex is the module's export index per function, used only for
// the realm-marker labels the browser rendering carries.
var growMemExportIndex = map[string]int{"bump": 0, "size": 1, "calls": 2}

// BuildGrowMemRecording writes a boundary recording of `calls` successive
// top-level calls into the `grow_mem` fixture module.
//
// **This fixture exists to give the slice and snapshot tests teeth.** Every one
// of `grow_mem`'s three exports returns a function of the module's accumulated
// state rather than of its arguments:
//
//	bump(v)  grows the memory by a page, bumps a global, stores v in the new
//	         page, and returns the page count BEFORE the grow
//	size()   returns the current page count
//	calls()  returns the global bump counter
//
// So a replay that resumes from a snapshot which was not faithfully restored
// produces a different return value at the very first call and fails the spec
// §6 divergence check. Contrast `BuildComputeBalanceRecording`, whose only
// export is a pure function of its arguments and therefore cannot tell a
// faithful resume from no resume at all.
//
// The expected return values are computed here by mirroring the module's
// semantics, so a mistake in this mirror surfaces as a `DivergenceError`
// naming the call — never as a silently weaker test.
func BuildGrowMemRecording(t *testing.T, dir string, calls []GrowMemCall) string {
	t.Helper()
	const src = "/fixtures/grow_mem.wat"
	b := newRecordingBuilder("grow_mem")

	// The module's own state, mirrored: it starts at its declared minimum of
	// one page with the bump counter at zero.
	pages, bumps := int32(1), int32(0)
	for _, c := range calls {
		var args, results []jsValue
		var line int
		switch c.Fn {
		case "bump":
			old := pages
			pages++
			bumps++
			args = []jsValue{jsInt(c.Arg)}
			results = []jsValue{jsInt(old)}
			line = 18
		case "size":
			results = []jsValue{jsInt(pages)}
			line = 25
		case "calls":
			results = []jsValue{jsInt(bumps)}
			line = 28
		default:
			t.Fatalf("unknown grow_mem export %q", c.Fn)
		}
		b.export(c.Fn, growMemExportIndex[c.Fn], src, line, args, results, nil)
	}
	return b.write(t, dir)
}

// BuildTamperedComputeBalanceRecording is BuildComputeBalanceRecording with
// the recorded result of call `badCall` replaced by `badResult`.
//
// Nothing else about the recording changes, so replaying it must fail at
// exactly that call's return-value check and nowhere else — which is what
// makes it a test of the check rather than of the parser.
func BuildTamperedComputeBalanceRecording(
	t *testing.T, dir string, args [][2]int32, badCall int, badResult int32,
) string {
	t.Helper()
	b := newRecordingBuilder("balance_calc")
	for i, a := range args {
		userID, amount := a[0], a[1]
		result := userID*10 + amount*2
		if i == badCall {
			result = badResult
		}
		b.export("compute_balance", 1,
			"/home/zahary/m/js-support/codetracer/src/db-backend/tests/fixtures/cross_process/account-balance-with-wasm/wasm-src/lib.rs",
			71,
			[]jsValue{jsInt(userID), jsInt(amount)},
			[]jsValue{jsInt(result)}, nil)
	}
	return b.write(t, dir)
}

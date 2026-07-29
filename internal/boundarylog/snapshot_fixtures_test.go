package boundarylog

import (
	"testing"
)

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

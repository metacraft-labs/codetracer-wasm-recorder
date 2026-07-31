// nan_payload_test.go — M52: float NaN payloads survive the browser
// recording path.
//
// `codetracer-specs/Recording-Backends/WASM-Instrumentation-Layer.md` §7
// makes a NaN *payload* mismatch a replay divergence.  Before M52 a
// browser recording could not carry one: `__ct_emit_f32` / `__ct_emit_f64`
// took a WASM float, which the WebAssembly JS API hands JavaScript as a
// `Number` with the NaN payload left implementation-defined — and
// `JSON.stringify` then rendered any NaN `null` and `-0` as `0`.
//
// The fix is producer-first.  The instrumented module now reinterprets
// the float to its integer bit pattern *before* the hook fires
// (`__ct_emit_f32_bits` / `__ct_emit_f64_bits`), so nothing crosses into
// JavaScript that a `Number` could damage, and `browser_session.js`
// records the bits as a width-tagged hex string.
//
// # What these tests are driven by
//
// `testdata/boundary-log/nan-payloads/` holds TWO recordings of the SAME
// module, both made by driving a page in headless Chromium through the
// real `record-web` daemon (see
// `codetracer/src/db-backend/tests/fixtures/wasm-nan-payloads/regenerate.sh`,
// which is the pipeline that produced them):
//
//   - `nan-payloads.ct` — recorded with the M52 producer.
//   - `legacy-encoding.ct` — the same page and the same module, recorded
//     with the pre-M52 producer.  It is the negative control, and it is
//     committed rather than synthesised so nobody has to take on trust
//     that the old path lost these values.
//
// Neither is hand-written.  A synthesised recording would prove only
// that this decoder can read what this repo's own encoder writes; the
// claim under test is about what a browser produces.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

const nanTestdata = "testdata/boundary-log/nan-payloads"

var (
	nanModule           = nanTestdata + "/nan_payloads.wasm"
	nanRecording        = nanTestdata + "/nan-payloads.ct"
	nanLegacyRecording  = nanTestdata + "/legacy-encoding.ct"
	nanRecordingTraceJS = nanRecording + "/trace.json"
)

// The bit patterns the fixture's page asked the module to produce.  They
// are written here as integers, never as float literals: `0x7F800001` is
// a signalling NaN and `0x7FF80000DEADBEEF` a payload-carrying quiet NaN,
// and Go source cannot spell either as a `float32` / `float64` constant
// without going through exactly the canonicalisation this test exists to
// rule out.
const (
	// f32 signalling NaN — the quiet bit (0x00400000) is clear, so
	// quieting it, or an f32 -> f64 -> f32 round trip, changes it.
	nanSignallingF32 = "f32:0x7f800001"
	// f32 negative zero.
	nanNegativeZeroF32 = "f32:0x80000000"
	// f64 quiet NaN carrying a payload.
	nanPayloadF64 = "f64:0x7ff80000deadbeef"
	// f64 negative zero — *computed* by the module (`-0.0 * 1.0`), not
	// declared.
	nanNegativeZeroF64 = "f64:0x8000000000000000"
)

// ===========================================================================
// verify_a_nan_payload_survives_the_browser_path
// ===========================================================================

// TestVerifyANanPayloadSurvivesTheBrowserPath is M52's headline claim, in
// three parts:
//
//  1. the browser recording carries the exact bit patterns;
//  2. replaying it against the original module does not diverge; and
//  3. the check that would have caught a mismatch is live — perturbing a
//     single recorded bit makes the replay fail.
//
// Part 3 is what stops the other two from being vacuous.  A replay that
// never compared the values would pass parts 1 and 2 unchanged.
func TestVerifyANanPayloadSurvivesTheBrowserPath(t *testing.T) {
	// ----- 1. the recording carries the bits ---------------------------
	//
	// Asserted against the recording's own bytes rather than against a
	// decoded float, because a decoded float cannot express the
	// difference: two NaNs with different payloads are both `NaN`, and
	// `-0.0 == +0.0`.
	recorded := nanRecordedFloatPayloads(t)
	require.Equal(t,
		[]string{
			// probe_f32(0x7F800001): the signalling NaN crosses out as
			// `observe_f32`'s argument, then back as the export's result.
			nanSignallingF32, nanSignallingF32,
			// probe_f32(0x80000000): f32 negative zero, same two edges.
			nanNegativeZeroF32, nanNegativeZeroF32,
			// probe_f64(0x7FF80000DEADBEEF): the payload NaN.
			nanPayloadF64, nanPayloadF64,
			// probe_negated_f64(0): the module negates the +0.0 it was
			// given and multiplies by one, so what crosses is a computed
			// -0.0.  Recording `f64:0x0000000000000000` here would be a
			// wrong answer that no float comparison could see.
			nanNegativeZeroF64, nanNegativeZeroF64,
		},
		recorded,
		"the browser recording must carry every boundary float as its exact "+
			"IEEE-754 bit pattern")

	// ----- 2. the replay agrees ----------------------------------------
	outDir := filepath.Join(t.TempDir(), "traces")
	exitCode, stdout, stderr := runMain(t, "", []string{
		"run", "--boundary-log=" + nanRecording, "--out-dir=" + outDir, nanModule})
	require.Equal(t, 0, exitCode,
		"replaying a recording of payload-carrying NaNs must not diverge.\n"+
			"stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.True(t, strings.Contains(stdout, "replayed 5 exported call(s)"),
		"the replay should report the five exported calls the page made; "+
			"stdout:\n%s", stdout)
	require.True(t, strings.Contains(stdout, "4 imported call(s)"),
		"the replay should report the four `observe_*` crossings; stdout:\n%s",
		stdout)

	// ----- 3. the divergence check is live -----------------------------
	//
	// Flip the low bit of the recorded f32 signalling NaN — 0x7F800001 ->
	// 0x7F800003, a NaN either way, indistinguishable under `==` and under
	// `math.IsNaN`.  Only a bit-level comparison can reject it, and spec
	// §7 says it must.
	t.Run("a one-bit payload change diverges", func(t *testing.T) {
		perturbed := nanRecordingWith(t, nanSignallingF32, "f32:0x7f800003")
		outDir := filepath.Join(t.TempDir(), "traces")
		exitCode, _, stderr := runMain(t, "", []string{
			"run", "--boundary-log=" + perturbed, "--out-dir=" + outDir, nanModule})
		require.True(t, exitCode != 0,
			"a recorded NaN whose payload differs from the re-executed one "+
				"is a divergence (spec §7), not a tolerable quirk.  The "+
				"replay accepted it, which means nothing above is actually "+
				"being compared.  stderr:\n%s", stderr)
		require.True(t, strings.Contains(stderr, "0x7f800003") ||
			strings.Contains(stderr, "0x7F800003"),
			"the divergence diagnostic must show the payload it rejected, "+
				"or a user cannot tell a payload mismatch from a value "+
				"mismatch; stderr:\n%s", stderr)
	})
}

// ===========================================================================
// verify_old_float_recordings_still_replay
// ===========================================================================

// TestVerifyOldFloatRecordingsStillReplay is the back-compat half of M52,
// and the reason the encoding is self-describing rather than flagged by a
// version bit: a boundary recording is an artefact users hold, and every
// one made before this change spells its floats as plain decimals.
//
// Two things are asserted, and the second is the interesting one:
//
//   - the committed demo recording, which predates M52 and carries only
//     ordinary finite floats, still replays cleanly; and
//   - the pre-M52 recording of *this* fixture's module — the same page,
//     the same module, the same headless Chromium, driven through the old
//     producer — is REFUSED, because what it carries for a NaN is `null`.
//
// The second is the negative control the milestone asks for, and it is
// stronger than "the old encoding diverges on a canonicalised value": the
// old path did not canonicalise the NaN, it lost it. `JSON.stringify(NaN)`
// is `null`, so there is no value left to compare. Refusing is the only
// correct answer — the alternative would be to invent one.
func TestVerifyOldFloatRecordingsStillReplay(t *testing.T) {
	t.Run("a pre-M52 recording of ordinary floats replays", func(t *testing.T) {
		// The committed demo recording. Its floats are absent (the module
		// is integer-only), so what this pins is that the decoder did not
		// become strict about a spelling it must keep accepting; the unit
		// coverage for the decimal spelling itself is
		// `TestDecodePreM52FloatRecordingsStillReplay` in
		// internal/boundarylog.
		outDir := filepath.Join(t.TempDir(), "traces")
		exitCode, _, stderr := runMain(t, "", []string{
			"run", "--boundary-log=" + demoRecording, "--out-dir=" + outDir, demoWasm})
		require.Equal(t, 0, exitCode,
			"a recording made before M52 must not be rejected for its age; "+
				"stderr:\n%s", stderr)
	})

	t.Run("the pre-M52 encoding could not carry a NaN at all", func(t *testing.T) {
		raw, err := os.ReadFile(nanLegacyRecording + "/trace.json")
		require.NoError(t, err,
			"the pre-M52 recording is committed as the negative control")
		require.True(t, strings.Contains(string(raw), `"f":"null"`) ||
			strings.Contains(string(raw), `"f": "null"`),
			"the negative control must actually show the loss it is the "+
				"control for: a NaN reaching JSON as `null`.  If this no "+
				"longer holds, the fixture was regenerated with the new "+
				"producer and proves nothing.")

		outDir := filepath.Join(t.TempDir(), "traces")
		exitCode, _, stderr := runMain(t, "", []string{
			"run", "--boundary-log=" + nanLegacyRecording,
			"--out-dir=" + outDir, nanModule})
		require.True(t, exitCode != 0,
			"a recording whose NaN reached disk as `null` carries no value "+
				"to replay; accepting it would mean inventing one")
		require.True(t, strings.Contains(stderr, "null"),
			"the refusal must name what it could not read; stderr:\n%s", stderr)
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nanRecordedFloatPayloads returns every `Float` value payload in the
// recording, in order, straight out of `trace.json`.
//
// Read as text on purpose: decoding to a float would destroy the very
// distinctions under test.
func nanRecordedFloatPayloads(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(nanRecordingTraceJS)
	require.NoError(t, err)

	var records []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &records),
		"the browser recording's trace.json must be a JSON array of records")

	var out []string
	for _, rec := range records {
		body, ok := rec["Value"]
		if !ok {
			continue
		}
		var v struct {
			Value struct {
				Kind string `json:"kind"`
				F    string `json:"f"`
			} `json:"value"`
		}
		require.NoError(t, json.Unmarshal(body, &v))
		if v.Value.Kind == "Float" {
			out = append(out, v.Value.F)
		}
	}
	return out
}

// nanRecordingWith copies the recording into a temp directory with one
// textual substitution applied to `trace.json`, and returns the copy's
// path.  Used to perturb a single recorded bit pattern.
//
// The committed recording is never modified: it is a real artefact of a
// real browser run, and a test that edits it in place would leave the
// repository holding something no browser produced.
func nanRecordingWith(t *testing.T, from, to string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "perturbed.ct")
	require.NoError(t, os.MkdirAll(dst, 0o755))

	entries, err := os.ReadDir(nanRecording)
	require.NoError(t, err)
	replaced := false
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(nanRecording, e.Name()))
		require.NoError(t, err)
		if e.Name() == "trace.json" {
			text := string(body)
			require.True(t, strings.Contains(text, from),
				"the recording no longer contains %q, so this perturbation "+
					"would be a no-op and the test would pass vacuously", from)
			body = []byte(strings.ReplaceAll(text, from, to))
			replaced = true
		}
		require.NoError(t, os.WriteFile(filepath.Join(dst, e.Name()), body, 0o644))
	}
	require.True(t, replaced, "the recording has no trace.json to perturb")
	return dst
}

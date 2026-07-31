//go:build crossrepo

// Do the committed boundary-log vectors still describe the producer?
//
// # Why this repo commits recordings at all, when the sibling does not
//
// `codetracer` deleted its committed recordings and now produces them
// when its tests run. That is right there: it *owns* the producer, so a
// committed recording of it was a cache of the code under test, and the
// suite went on agreeing with a recorder that had been replaced.
//
// This repo is on the other side of that boundary. It is the *replayer*.
// The recordings under `cmd/wazero/testdata/boundary-log/` are the output
// of a pipeline living in two siblings — `ct-instrument` from
// `codetracer-wasm-instrumenter`, the `record-web` daemon from
// `codetracer`, and a headless Chromium — none of which this repo builds
// or is allowed to depend on. Producing them here would mean requiring a
// sibling working copy for `just test`, and generating them with this
// package's own `recordingBuilder` would mean replaying this repo's model
// of the browser instead of the browser: `TestVerifyCrossModalityParity`
// would compare wazero against a Go re-implementation of four Rust
// modules, and `nan_payload_test.go`'s bit-pattern assertions would show
// only that the builder writes what it was told. So they stay committed,
// as deliberately captured vectors of an external producer.
//
// # What was actually wrong, and what this file fixes
//
// The vectors were not merely committed — they were *synced*. The
// sibling's `wasm-parity-corpus/regenerate.sh` ended by copying each
// fresh recording straight into this repo's testdata. That made the two
// repos agree by construction, and the agreement then held indefinitely,
// because the last sync had frozen it. Nothing anywhere asked whether the
// vectors still described what the producer emits. That is the same
// defect the sibling's committed recordings had, wearing a cross-repo
// coat.
//
// The sync is gone. This is what replaces it: an explicit comparison,
// tagged `crossrepo` so the standalone `go test ./...` never needs a
// sibling, run by `just verify-vectors` and in CI. It drives the sibling's
// `scripts/materialize-recording.sh` to record the same demos from the
// current producer and compares what the two recordings *mean* — every
// recovered crossing, its kind, name, index, depth, argument and result
// values, and the format witness — rather than their bytes. Bytes would
// be the wrong oracle: `trace_metadata.json` carries the absolute
// directory the recording was made in, so byte equality is unachievable
// and a test that demanded it would be disabled within a week.
//
// A failure here is not "the fixtures are stale". It is "the producer
// changed and this repo's replayer has not been told", which is precisely
// the report that went unmade for six weeks when four `TestRecorderGolden*`
// files went red.
//
// NO MOCKS. Both sides are real recordings: one captured, one made by
// running the real pipeline now.
package boundarylog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// siblingCodetracerRoot locates the `codetracer` checkout that owns the
// recording pipeline.
//
// Fails the test rather than skipping when it cannot: this file only
// builds under the `crossrepo` tag, so reaching it at all is a statement
// that the sibling is supposed to be here. A skip would put the check
// back where it was — nominally present, never run.
func siblingCodetracerRoot(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv("CODETRACER_ROOT"); explicit != "" {
		if _, err := os.Stat(filepath.Join(explicit, "scripts", "materialize-recording.sh")); err != nil {
			t.Fatalf("CODETRACER_ROOT=%s holds no scripts/materialize-recording.sh: %v", explicit, err)
		}
		return explicit
	}
	here, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for _, candidate := range []string{
		filepath.Join(here, "..", "..", "..", "codetracer"),
		filepath.Join(here, "..", "..", "codetracer"),
	} {
		resolved, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(resolved, "scripts", "materialize-recording.sh")); err == nil {
			return resolved
		}
	}
	t.Fatal("no sibling codetracer checkout with scripts/materialize-recording.sh; " +
		"set CODETRACER_ROOT. This check compares the committed vectors against the " +
		"live producer and cannot be run without it.")
	return ""
}

// materialize returns the directory holding a freshly recorded fixture,
// running the sibling's recorder if a runner has not already done it.
//
// The env-var hand-off exists because the pipeline needs the *sibling's*
// development environment — its Node, its wasm32 target, its Chromium —
// and this repo's shell has none of them. `scripts/verify-vectors-against-producer.sh`
// enters that environment, records, and passes the directories down. The
// direct call is kept for the case where the caller is already inside a
// shell that can record, so the check is runnable by hand.
func materialize(t *testing.T, codetracerRoot, fixture string) string {
	t.Helper()
	envKey := "CT_PRODUCED_RECORDING_" + strings.ToUpper(strings.NewReplacer("-", "_").Replace(fixture))
	if prepared := os.Getenv(envKey); prepared != "" {
		if info, err := os.Stat(prepared); err != nil || !info.IsDir() {
			t.Fatalf("%s=%q is not a directory (%v)", envKey, prepared, err)
		}
		return prepared
	}
	script := filepath.Join(codetracerRoot, "scripts", "materialize-recording.sh")
	cmd := exec.Command(script, fixture)
	cmd.Dir = codetracerRoot
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("recording %q through %s failed: %v\n"+
			"This repo's shell does not carry the sibling's recording toolchain. Run "+
			"`just verify-vectors`, which enters the sibling's development environment, "+
			"records there, and hands the directories back through %s.",
			fixture, script, err, envKey)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("%s printed no directory for %q", script, fixture)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s reported %q, which is not a directory (%v)", script, dir, err)
	}
	return dir
}

// crossingSummary is everything about a crossing that carries meaning,
// rendered so a diff reads as a sentence.
//
// Deliberately excludes the recording's `Program`, `Workdir` and `Source`
// — those name the directory the run happened in, which differs by
// construction between a captured vector and a fresh recording and says
// nothing about the producer's behaviour.
func crossingSummary(c Crossing) string {
	var b strings.Builder
	b.WriteString("seq=")
	b.WriteString(itoa(c.Seq))
	b.WriteString(" depth=")
	b.WriteString(itoa(c.Depth))
	if c.Kind == CrossingExport {
		b.WriteString(" export=")
		b.WriteString(c.Name)
	} else {
		b.WriteString(" import=#")
		b.WriteString(itoa(int(c.Index)))
	}
	b.WriteString(" args=[")
	for i, v := range c.Args {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(v.Kind)
		b.WriteString(":")
		b.WriteString(v.Text)
	}
	b.WriteString("]")
	if c.hasResults {
		b.WriteString(" results=[")
		for i, v := range c.Results {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(v.Kind)
			b.WriteString(":")
			b.WriteString(v.Text)
		}
		b.WriteString("]")
	} else {
		b.WriteString(" results=<none recorded>")
	}
	if c.markerBracketed {
		b.WriteString(" marker-bracketed")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// requireSameMeaning compares two recordings on everything that
// determines how the replayer behaves.
func requireSameMeaning(t *testing.T, label, committedPath, freshPath string) {
	t.Helper()

	committed, err := LoadRecording(committedPath)
	if err != nil {
		t.Fatalf("%s: the committed vector no longer loads: %v", label, err)
	}
	fresh, err := LoadRecording(freshPath)
	if err != nil {
		t.Fatalf("%s: the freshly recorded %s does not load: %v", label, freshPath, err)
	}

	// The format witness is the single most consequential bit in a
	// recording: it decides whether an unmatched value-less import call is
	// a divergence or an unchecked call. A producer that stopped setting
	// it, or a vector predating a producer that now sets it, changes the
	// meaning of every replay this repo performs.
	if committed.MarkersIdentifyImports != fresh.MarkersIdentifyImports {
		t.Errorf("%s: MarkersIdentifyImports is %v in the committed vector and %v in a "+
			"recording made now. The producer changed its marker spelling; this repo's "+
			"replay classifies value-less import calls differently on the two, so the "+
			"vector no longer stands for what the pipeline emits.",
			label, committed.MarkersIdentifyImports, fresh.MarkersIdentifyImports)
	}

	if len(committed.Crossings) != len(fresh.Crossings) {
		t.Fatalf("%s: the committed vector has %d crossing(s), a recording made now has %d. "+
			"The producer's boundary coverage changed.\ncommitted:\n%s\nfresh:\n%s",
			label, len(committed.Crossings), len(fresh.Crossings),
			renderCrossings(committed.Crossings), renderCrossings(fresh.Crossings))
	}
	for i := range committed.Crossings {
		want := crossingSummary(committed.Crossings[i])
		got := crossingSummary(fresh.Crossings[i])
		if want != got {
			t.Errorf("%s: crossing %d differs between the committed vector and a recording "+
				"made now:\n  committed: %s\n  fresh:     %s", label, i, want, got)
		}
	}

	// Host state, where the recording carries it. `vault_apply` and
	// `ledger-settle` exist to exercise spec §3.3/§3.4, so a producer that
	// stopped emitting the sidecar — or started emitting it for a
	// recording that had none — must be reported here rather than
	// discovered as a mysterious replay refusal.
	switch {
	case committed.HostState == nil && fresh.HostState != nil:
		t.Errorf("%s: the committed vector carries no host state but a recording made now "+
			"does. The producer learned to emit spec §3.3/§3.4 state and the vector predates it.", label)
	case committed.HostState != nil && fresh.HostState == nil:
		t.Errorf("%s: the committed vector carries host state but a recording made now "+
			"does not. The producer stopped emitting it.", label)
	}
}

func renderCrossings(crossings []Crossing) string {
	var b strings.Builder
	for _, c := range crossings {
		b.WriteString("    ")
		b.WriteString(crossingSummary(c))
		b.WriteString("\n")
	}
	return b.String()
}

// The demo recording under `boundary-log/frontend-wasm.ct` still
// describes what the browser pipeline emits.
func TestTheCommittedDemoVectorStillDescribesTheProducer(t *testing.T) {
	root := siblingCodetracerRoot(t)
	start := time.Now()
	produced := materialize(t, root, "cross-process-three-trace")
	t.Logf("recorded the three-tier demo in %s", time.Since(start).Round(time.Second))

	requireSameMeaning(t, "frontend-wasm.ct",
		"../../cmd/wazero/testdata/boundary-log/frontend-wasm.ct",
		filepath.Join(produced, "frontend-wasm.ct"))
}

// The four parity-corpus vectors still describe what the browser pipeline
// emits for the same four modules.
//
// These are the ones the deleted sync step used to overwrite. Comparing
// them is what the sync was pretending to do.
func TestTheCommittedParityCorpusStillDescribesTheProducer(t *testing.T) {
	root := siblingCodetracerRoot(t)
	start := time.Now()
	produced := materialize(t, root, "wasm-parity-corpus")
	t.Logf("recorded the parity corpus in %s", time.Since(start).Round(time.Second))

	for _, module := range []struct{ dir, program string }{
		{"loop_digest", "loop-digest"},
		{"pair_stats", "pair-stats"},
		{"vault_apply", "vault-apply"},
		{"tick_ledger", "tick-ledger"},
	} {
		t.Run(module.dir, func(t *testing.T) {
			requireSameMeaning(t, module.dir,
				filepath.Join("../../cmd/wazero/testdata/boundary-log/parity-corpus",
					module.dir, module.program+".ct"),
				filepath.Join(produced, "modules", module.dir, module.program+".ct"))
		})
	}
}

// The NaN-payload vector still describes what the browser pipeline emits.
//
// `nan-payloads.ct` is the positive control for M52: its whole content is
// evidence about what the producer wrote for a signalling NaN, a
// payload-carrying quiet NaN and a negative zero. A producer that
// regressed on any of them would leave this vector asserting a capability
// the product no longer has.
//
// Its sibling `legacy-encoding.ct` is deliberately NOT checked here. It
// was made by a `ct-instrument` built before M52 and cannot be produced
// from any current tree — it is the one recording in this workspace that
// is committed for the right reason, and re-deriving it would destroy
// exactly what makes it evidence.
func TestTheCommittedNanVectorStillDescribesTheProducer(t *testing.T) {
	root := siblingCodetracerRoot(t)
	start := time.Now()
	produced := materialize(t, root, "wasm-nan-payloads")
	t.Logf("recorded the NaN-payload demo in %s", time.Since(start).Round(time.Second))

	requireSameMeaning(t, "nan-payloads.ct",
		"../../cmd/wazero/testdata/boundary-log/nan-payloads/nan-payloads.ct",
		filepath.Join(produced, "nan-payloads.ct"))
}

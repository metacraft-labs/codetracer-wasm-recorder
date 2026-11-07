//go:build crossrepo

// Does a freshly produced host-supplied-state recording actually depend on
// its host state?
//
// `codetracer/src/db-backend/tests/fixtures/wasm-memory-calldata/verify.sh`
// produces `ledger-settle.ct` from its tree and runs this test against it
// (`CT_PRODUCED_RECORDING_WASM_MEMORY_CALLDATA` names the materialised
// directory). The withholding is done here because this package owns the
// only boundary-log decoder: CodeTracer writes boundary logs and never reads
// them.
//
// Withholding either the spec §3.3 initial state (emptying the imported
// memory's contents) or the §3.4 mutations must
// make the replay DIVERGE — not succeed, and not fail some other way. A
// module that replayed without them would not depend on them, and the
// fixture could then not tell a working implementation from none.
//
// No mocks: the recording is the producer's, the module is the one it was
// recorded against, and the replay is the shipped driver.
package boundarylog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

func TestTheMemoryCalldataRecordingDependsOnItsHostState(t *testing.T) {
	dir := os.Getenv("CT_PRODUCED_RECORDING_WASM_MEMORY_CALLDATA")
	if dir == "" {
		t.Fatal("set CT_PRODUCED_RECORDING_WASM_MEMORY_CALLDATA to the directory " +
			"`scripts/materialize-recording.sh wasm-memory-calldata` printed; " +
			"`wasm-memory-calldata/verify.sh` does")
	}
	recording := filepath.Join(dir, "ledger-settle.ct")
	module := filepath.Join(dir, "module", "ledger_settle.wasm")

	// The control the two below are measured against: as produced, it
	// replays.
	res, err := replayFixture(t, module, recording, nil)
	require.NoError(t, err)
	require.Equal(t, 3, res.ExportCalls)

	withhold := func(t *testing.T, name string, edit func(doc map[string]any) bool) {
		withheld, touched := RewriteTestRecordingEvents(t, recording,
			filepath.Join(t.TempDir(), "withheld.ct"),
			func(meta string) (string, bool) {
				if !strings.Contains(meta, hostStateBoundary) {
					return meta, true
				}
				var doc map[string]any
				require.NoError(t, json.Unmarshal([]byte(meta), &doc))
				if !edit(doc) {
					return meta, false
				}
				out, err := json.Marshal(doc)
				require.NoError(t, err)
				return string(out), true
			})
		require.True(t, touched > 0, "the recording carries no host-state record to withhold")

		_, err := replayFixture(t, module, withheld, nil)
		require.Error(t, err, "the replay SUCCEEDED without %s", name)
		var d *DivergenceError
		require.True(t, errors.As(err, &d),
			"withholding %s must be a divergence — the spec §6 failure that says "+
				"an input is missing — got %T: %v", name, err, err)
	}

	t.Run("the §3.3 initial state", func(t *testing.T) {
		// The memory stays declared and its contents go: the module then
		// reads zeroes where the host had put calldata.
		withhold(t, "the §3.3 initial state", func(doc map[string]any) bool {
			if doc["record"] == hostStateRecordInitial {
				initial := doc["initial"].(map[string]any)
				for _, m := range initial["memories"].([]any) {
					m.(map[string]any)["data"] = []any{}
				}
			}
			return true
		})
	})
	t.Run("the §3.4 host mutations", func(t *testing.T) {
		withhold(t, "the §3.4 host mutations", func(doc map[string]any) bool {
			return doc["record"] != hostStateRecordMutation
		})
	})
}

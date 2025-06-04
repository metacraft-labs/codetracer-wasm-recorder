package wasmdebug_test

import (
	"os"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/dwarftestdata"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasm/binary"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

func TestIndexDwarfData_InlinedSubroutines(t *testing.T) {

	// Capture noisy output produced during DWARF indexing.
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	_, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w

	mod, err := binary.DecodeModule(dwarftestdata.RustWasm, api.CoreFeaturesV2, wasm.MemoryLimitPages, false, true, true)
	w.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	require.NoError(t, err)
	require.NotNil(t, mod.DWARFLines)

	const instrOffset = 0x4cb
	entries, ok := mod.PCRecord.InlinedRoutines.AllIntersections(instrOffset, instrOffset)
	require.True(t, ok)
	require.Equal(t, 2, len(entries))

	// Ensure debug positions agree with the inline count.
	pos := mod.DWARFLines.DebugPositions(instrOffset)
	require.True(t, len(pos) >= len(entries))
	_ = wasmdebug.PCRecord{} // silence unused warning on import
}

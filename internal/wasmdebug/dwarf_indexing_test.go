package wasmdebug_test

import (
	"debug/dwarf"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/dwarftestdata"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasm/binary"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

func TestIndexDwarfData_InlinedSubroutines(t *testing.T) {
	if len(dwarftestdata.RustWasm) == 0 {
		t.Skip("rust wasm not available")
	}

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

func TestIndexDwarfData_NilData(t *testing.T) {
	record, err := wasmdebug.IndexDwarfData(nil)
	require.NoError(t, err)
	require.NotNil(t, record.Line)
	require.NotNil(t, record.Function)
	require.NotNil(t, record.InlinedRoutines)
	require.NotNil(t, record.Locals)
	require.NotNil(t, record.TypeParamMap)
}

// TestIndexDwarfData_IndexesEveryStatementRow holds the line index to the
// line table it was built from: every row `indexCompileUnit` selects must be
// found again at its own address, with its own file, line and column.
//
// The rows that matter are the one-byte ones, where the next row starts one
// address later, so the row covers a single instruction such as `end` or
// `drop`. Their interval is a point, and the search tree refuses points unless
// it is built to accept them. The refusal came back as an error that nothing
// checked, so each such row dropped out of the index and the instruction it
// covers had no source line. Every fixture below contains such rows; the test
// asserts it across the fixtures, so it cannot pass on fixtures that no
// longer exercise them.
//
// A sequence the linker discarded is left out of the comparison. wasm-ld
// relocates its rows to a tombstone address (see `isTombstoneAddr`), so every
// discarded sequence sits on top of the others, and an address in it is not
// an offset of any code that can execute.
//
// No mocks: the fixtures are real modules from four producers, decoded through
// the same path `CompileModule` uses, and the expected rows come from
// `debug/dwarf`'s line reader, not from the index under test.
func TestIndexDwarfData_IndexesEveryStatementRow(t *testing.T) {
	fixtures := []struct {
		name string
		wasm []byte
	}{
		{"rust", dwarftestdata.RustWasm},
		{"tinygo", dwarftestdata.TinyGoWasm},
		{"zig", dwarftestdata.ZigWasm},
		{"zig-cc", dwarftestdata.ZigCCWasm},
	}
	var oneByteRows int
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			if len(fx.wasm) == 0 {
				t.Skipf("%s wasm not available", fx.name)
			}

			// The decoder reports what it could not index on stderr; a row
			// refused by the line tree would show up there too.
			savedStderr := os.Stderr
			r, w, err := os.Pipe()
			require.NoError(t, err)
			os.Stderr = w
			captured := make(chan []byte)
			go func() {
				b, _ := io.ReadAll(r)
				captured <- b
			}()
			mod, err := binary.DecodeModule(fx.wasm, api.CoreFeaturesV2, wasm.MemoryLimitPages, false, true, true)
			os.Stderr = savedStderr
			require.NoError(t, w.Close())
			stderr := string(<-captured)
			require.NoError(t, err)
			require.NotNil(t, mod.DWARFData)
			require.False(t, strings.Contains(stderr, "error indexing line"),
				"the line index refused rows:\n%s", stderr)

			var rows int
			ur := mod.DWARFData.Reader()
			for {
				cu, err := ur.Next()
				require.NoError(t, err)
				if cu == nil {
					break
				}
				if cu.Tag != dwarf.TagCompileUnit {
					ur.SkipChildren()
					continue
				}
				ur.SkipChildren()
				lr, err := mod.DWARFData.LineReader(cu)
				require.NoError(t, err)
				if lr == nil {
					continue
				}

				// The same selection `indexCompileUnit` makes: a row is
				// indexed over [its address, the next row's address) when it
				// starts a statement or refines the previous row's column,
				// covers at least one byte, and names a source line.
				var prev, le dwarf.LineEntry
				havePrev, discarded := false, false
				for {
					err := lr.Next(&le)
					if errors.Is(err, io.EOF) {
						break
					}
					require.NoError(t, err)
					if !havePrev {
						discarded = isTombstoneAddr(le.Address)
					}
					if havePrev && !discarded && le.Address > prev.Address && prev.Line > 0 &&
						(prev.IsStmt || prev.Column != le.Column) {
						rows++
						if le.Address == prev.Address+1 {
							oneByteRows++
						}
						got, _ := mod.PCRecord.Line.AllIntersections(prev.Address, prev.Address)
						want := wasmdebug.LineRecord{
							FileName: prev.File.Name,
							Line:     int64(prev.Line),
							Column:   int64(prev.Column),
						}
						require.Equal(t, []wasmdebug.LineRecord{want}, got,
							"line index at %#x (row covers [%#x,%#x))", prev.Address, prev.Address, le.Address)
					}
					prev, havePrev = le, !le.EndSequence
				}
			}
			require.True(t, rows > 0, "the fixture has no line rows to check")
		})
	}
	require.True(t, oneByteRows > 0, "no fixture has a one-byte row, so none exercises point intervals")
}

// isTombstoneAddr mirrors the unexported helper of the same name in dwarf.go,
// which cites the encodings producers use.
func isTombstoneAddr(addr uint64) bool {
	addr32 := int32(addr)
	return addr32 == -1 || addr32 == -2 || addr32 == 0
}

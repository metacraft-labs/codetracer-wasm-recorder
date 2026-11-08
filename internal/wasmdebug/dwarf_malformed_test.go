package wasmdebug_test

import (
	"debug/dwarf"
	"encoding/binary"
	"io"
	"os"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

// The DWARF index is built while a module compiles, from whatever debug
// sections the module carries. A module is input, so `IndexDwarfData` must
// survive malformed DWARF: it may index less, but it may not panic and take
// the recorder down with it.
//
// Each test below hand-assembles a minimal DWARF 4 `.debug_info` unit
// (https://dwarfstd.org/doc/DWARF4.pdf §7.5) and decodes it with the standard
// library's `debug/dwarf`, the same decoder a real module goes through. No
// mocks: nothing in the index is substituted.

// Abbreviation codes for the DIEs the tests assemble.
const (
	abbrevSubprogram   = 1 // DW_TAG_subprogram, children, DW_AT_frame_base
	abbrevInlined      = 2 // DW_TAG_inlined_subroutine, children, no attributes
	abbrevLexicalBlock = 3 // DW_TAG_lexical_block, children, no attributes
	abbrevUndefined    = 9 // an abbreviation code the table does not define
)

// testAbbrevTable is the `.debug_abbrev` section shared by every test (§7.5.3).
var testAbbrevTable = []byte{
	abbrevSubprogram, 0x2e, 1, // DW_TAG_subprogram, DW_CHILDREN_yes
	0x40, 0x18, // DW_AT_frame_base, DW_FORM_exprloc
	0, 0,
	abbrevInlined, 0x1d, 1, // DW_TAG_inlined_subroutine, DW_CHILDREN_yes
	0, 0,
	abbrevLexicalBlock, 0x0b, 1, // DW_TAG_lexical_block, DW_CHILDREN_yes
	0, 0,
	0,
}

// wasmFunction is the DIE of a function whose frame base is WebAssembly local
// 0 (`DW_OP_WASM_location 0x00 0`). `indexFunctionEntry` reads a function's
// children only when it has a frame base.
var wasmFunction = []byte{abbrevSubprogram, 3, 0xed, 0x00, 0x00}

// dwarfWithDIEs wraps `dies` in one 32-bit DWARF 4 compilation unit with a
// 4-byte address size, as wasm32 producers emit.
func dwarfWithDIEs(t *testing.T, dies ...[]byte) *dwarf.Data {
	t.Helper()
	body := []byte{4, 0, 0, 0, 0, 0, 4} // version 4, debug_abbrev_offset 0, address_size 4
	for _, die := range dies {
		body = append(body, die...)
	}
	info := binary.LittleEndian.AppendUint32(nil, uint32(len(body)))
	info = append(info, body...)
	d, err := dwarf.New(testAbbrevTable, nil, nil, info, nil, nil, nil, nil)
	require.NoError(t, err)
	return d
}

// indexCapturingStderr runs `IndexDwarfData` and returns what it printed to
// stderr, which is where the index reports the entries it had to leave out.
func indexCapturingStderr(t *testing.T, d *dwarf.Data) (stderr string, err error) {
	t.Helper()
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	captured := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		captured <- b
	}()

	panicErr := require.CapturePanic(func() { _, err = wasmdebug.IndexDwarfData(d) })
	os.Stderr = saved
	require.NoError(t, w.Close())
	stderr = string(<-captured)
	require.NoError(t, panicErr, "IndexDwarfData panicked; stderr:\n%s", stderr)
	return stderr, err
}

// TestIndexDwarfData_InlinedSubroutineCutOffBeforeItsChildrenEnd covers a unit
// that ends inside an inlined subroutine's children, before the null entry
// that closes them. The reader then reports the end of the data as a nil
// entry, which the inlined-subroutine walk dereferenced before checking it.
func TestIndexDwarfData_InlinedSubroutineCutOffBeforeItsChildrenEnd(t *testing.T) {
	d := dwarfWithDIEs(t, wasmFunction, []byte{abbrevInlined})

	_, err := indexCapturingStderr(t, d)
	require.NoError(t, err)
}

// TestIndexDwarfData_UndecodableEntryInsideALexicalBlock covers a lexical
// block whose child cannot be decoded. The reader returns a nil entry together
// with the error, and the lexical-block walk read the nil entry's offset to
// return alongside that error. The error must reach stderr instead.
func TestIndexDwarfData_UndecodableEntryInsideALexicalBlock(t *testing.T) {
	d := dwarfWithDIEs(t, wasmFunction, []byte{abbrevLexicalBlock}, []byte{abbrevUndefined})

	stderr, err := indexCapturingStderr(t, d)
	require.NoError(t, err)
	require.Contains(t, stderr, "error indexing lexical block")
	require.Contains(t, stderr, "unknown abbreviation table index")
}

// TestIndexDwarfData_UndecodableEntryInsideAnInlinedSubroutine is the same
// defect reached through an inlined subroutine, whose walk checked the entry
// for nil only after it had read the entry's offset.
func TestIndexDwarfData_UndecodableEntryInsideAnInlinedSubroutine(t *testing.T) {
	d := dwarfWithDIEs(t, wasmFunction, []byte{abbrevInlined}, []byte{abbrevUndefined})

	stderr, err := indexCapturingStderr(t, d)
	require.NoError(t, err)
	require.Contains(t, stderr, "error indexing inlined subroutine")
	require.Contains(t, stderr, "unknown abbreviation table index")
}

package interpreter

import (
	"debug/dwarf"
	"fmt"
	"io"
	"strings"

	"github.com/tetratelabs/wazero/internal/wasm"
)

type dwarfWasmLocationInstr struct {

	// opcode
	opcode uint8

	// The type of location expression
	// 0x0 => Local
	// 0x1 || 0x3 => Global
	// ox2 => Operand stack
	locationType uint8

	location uint8
}

type dwarfLocalVariableEntry struct {

	// opcode
	opcode uint8

	// Offset from the frame base
	offset uint8
}

func newDwarfWasmLocationInstr(data []uint8) dwarfWasmLocationInstr {
	p := dwarfWasmLocationInstr{opcode: data[0], locationType: data[1], location: data[2]}

	return p
}

func newDwarfLocalVariableEntry(data []uint8) dwarfLocalVariableEntry {
	p := dwarfLocalVariableEntry{opcode: data[0], offset: data[1]}

	return p
}

func (ce *callEngine) getLocal(m *wasm.ModuleInstance, localIndex int, frameBaseLocalIdx int) ([]byte, error) {

	// if frameBaseLocalIdx >= len(ce.stack) {
	// 	return 0, errors.New("too early")
	// }

	if frameBaseLocalIdx >= len(ce.stack) {
		return nil, fmt.Errorf("still too early to get the local vars")
	}

	frameBase := ce.stack[frameBaseLocalIdx]

	effectiveOffset := frameBase + uint64(localIndex)

	mem := m.Memory()

	for i, v := range ce.stack {
		fmt.Printf("Index: %d, Value: %d\n", i, v)
	}

	val, _ := mem.Read(uint32(effectiveOffset), 4)

	return val, nil

}

func (ce *callEngine) getLocalVariableAddress(m *wasm.ModuleInstance, localIndex uint8, frameBaseLocalIdx int) (uint64, error) {

	if frameBaseLocalIdx >= len(ce.stack) {
		return 0, fmt.Errorf("still too early to get the local vars")
	}

	frameBase := ce.stack[frameBaseLocalIdx]

	effectiveOffset := frameBase + uint64(localIndex)

	return effectiveOffset, nil

}

func (ce *callEngine) getFunctionLocals(dwarfData *dwarf.Data, f *function, m *wasm.ModuleInstance, frameBase int) map[string]uint64 {

	entryReader := dwarfData.Reader()

	funcName := f.definition().Name()
	// fmt.Printf("Curr func has name: %s\n", funcName)

	res := make(map[string]uint64)

	for {
		entry, err := entryReader.Next()

		if err == io.EOF || entry == nil {
			break
		}

		if entry.Tag == dwarf.TagSubprogram {
			// Find the function's name.

			nameField := entry.AttrField(dwarf.AttrName)

			if nameField == nil || !strings.HasPrefix(funcName, nameField.Val.(string)) {

				fmt.Println("NOT THE RIGHT ONE")
				continue
			}

			fmt.Println("RIGHT ONE")

			// Get location attribute of the current DWARF entry
			locationEntry := entry.AttrField(dwarf.AttrFrameBase)

			// For some reason the SubProgram entry does not have a location entry
			// We opt to ignore it rather than panic and terminate execution
			if locationEntry == nil {
				continue
			}

			locationData := newDwarfWasmLocationInstr(locationEntry.Val.([]uint8))

			// Check if our local variable should be accessed from the module's "local" linear memory
			// TODO: Handle the 2 other types of WASM locations, namely: stack-operand and global
			if locationData.locationType == 0 {

				// frameBaseLocalIdx := locationData[2]

				// Now iterate over this subprogram's children.
				for {
					child, err := entryReader.Next()
					if err != nil {
						println("Failed to read DWARF entry")
					}
					// A nil entry or a zero-tag entry signals the end of children.
					if child == nil || child.Tag == 0 {
						break
					}

					// Check if the child is a variable local variable.
					if child.Tag == dwarf.TagVariable {
						varNameField := child.AttrField(dwarf.AttrName)
						varLocationField := child.AttrField(dwarf.AttrLocation)

						fmt.Println("WWWWWWHAT")

						if varNameField != nil {

							localVarLocationEntry := newDwarfLocalVariableEntry(varLocationField.Val.([]uint8))

							address, err := ce.getLocalVariableAddress(m, localVarLocationEntry.offset, frameBase+2)

							fmt.Printf("FOUND LOCAL: %s WITH ADDRESS: %d\n", varNameField.Val.(string), address)

							if err == nil {
								fmt.Printf("FOUND LOCAL: %s WITH ADDRESS: %d\n", varNameField.Val.(string), address)
								res[varNameField.Val.(string)] = address
							}

						}
					}
				}

				// Break out once we've processed the target subprogram.
				break

			} else {
				// If it’s not the target function, skip its children if any.
				entryReader.SkipChildren()
			}
		}
	}

	return res

}

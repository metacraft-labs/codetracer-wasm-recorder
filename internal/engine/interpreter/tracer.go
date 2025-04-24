package interpreter

import (
	"debug/dwarf"
	"fmt"
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

// func (ce *callEngine) getFunctionLocals(dwarfData *dwarf.Data, f *function, m *wasm.ModuleInstance, frameBase int, int pc) map[string]uint64 {
func (ce *callEngine) getFunctionLocalVarOffsets(entryReader *dwarf.Reader) (uint64, map[string]uint64, error) {

	res := make(map[string]uint64)

	var stackPointerLocalIndex uint64

	for {

		entry, err := entryReader.Next()

		if entry == nil || err != nil {
			return 0, res, fmt.Errorf("Entry was nil")
		}

		if entry.Tag == dwarf.TagSubprogram {
			// Find the function's name.

			// nameField := entry.AttrField(dwarf.AttrName)

			// Get location attribute of the current DWARF entry
			locationEntry := entry.AttrField(dwarf.AttrFrameBase)

			// For some reason the SubProgram entry does not have a location entry
			// We opt to ignore it rather than panic and terminate execution
			if locationEntry == nil {
				return 0, res, fmt.Errorf("Location entry was nil")
			}

			locationData := newDwarfWasmLocationInstr(locationEntry.Val.([]uint8))


			// Check if our local variable should be accessed from the module's "local" linear memory
			// TODO: Handle the 2 other types of WASM locations, namely: stack-operand and global
			if locationData.locationType == 0 {

				stackPointerLocalIndex = uint64(locationData.location)

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

						if varNameField != nil && varLocationField != nil {
							res[varNameField.Val.(string)] = uint64((varLocationField.Val.([]uint8))[1])
						}
					}
				}

				// Break out once we've processed the target subprogram.
				break
			}

		}
	}

	return stackPointerLocalIndex, res, nil
}

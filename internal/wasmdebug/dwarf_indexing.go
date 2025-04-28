package wasmdebug

import (
	"debug/dwarf"
	"errors"
	"fmt"
	"io"
	"sort"
	"os"

	"github.com/rdleal/intervalst/interval"
)

type LineRecord struct {
	FileName string
	Line     int64
	Column   int64
}

type VariableRecord struct {
	Name string
	Offset uint64
}

type FunctionRecord struct {
	Name     string
	FileName string
	Line     int64
	FrameBaseIndex uint64
	// TODO: params
	Locals []VariableRecord
}

type InlineRecord struct {
	Name         string
	FileName     string
	Line         int64
	CallFileName string
	CallLine     int64
	CallColumn   int64
}

type PCRecord struct {
	Line            *interval.SearchTree[LineRecord, uint64]
	Function        *interval.SearchTree[FunctionRecord, uint64]
	InlinedRoutines *interval.SearchTree[[]InlineRecord, uint64]
	Locals          *interval.SearchTree[[]VariableRecord, uint64]
}

func IndexDwarfData(d *dwarf.Data) (ret PCRecord, err error) {
	cmpFn := func(x, y uint64) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}

	ret = PCRecord{
		Line:            interval.NewSearchTree[LineRecord](cmpFn),
		Function:        interval.NewSearchTree[FunctionRecord](cmpFn),
		InlinedRoutines: interval.NewSearchTree[[]InlineRecord](cmpFn),
		Locals:          interval.NewSearchTree[[]VariableRecord](cmpFn),
	}

	r := d.Reader()
	var files []*dwarf.LineFile

	for {
		ent, err := r.Next()
		if err != nil || ent == nil {
			break
		}

		switch ent.Tag {
		case dwarf.TagCompileUnit, dwarf.TagInlinedSubroutine, dwarf.TagSubprogram:
		default:
			// Only CompileUnit, InlinedSubroutines ans Subprograms are relevant.
			continue
		}

		// Check if the entry spans the range which contains the target instruction.
		ranges, err := d.Ranges(ent)
		if err != nil {
			continue
		}

		for _, pcs := range ranges {
			start, end := pcs[0], pcs[1]

			if isTombstoneAddr(start) || isTombstoneAddr(end) {
				continue
			}

			switch ent.Tag {
			case dwarf.TagCompileUnit:
				if files, err = indexCompileUnit(ent, d, &ret); err != nil {
					fmt.Fprintf(os.Stderr, "error indexing function: %v\n", err)
					continue
				}


			case dwarf.TagInlinedSubroutine:

			case dwarf.TagSubprogram:
				if err := indexFunctionEntry(r, ent, files, &ret); err != nil {
					fmt.Fprintf(os.Stderr, "error indexing function: %v\n", err)
					continue
				}
			}
		}
	}

	return
}



func indexFunctionEntry(r *dwarf.Reader, ent *dwarf.Entry, files []*dwarf.LineFile, tree *PCRecord) error {
	lowPcWrapped := ent.AttrField(dwarf.AttrLowpc)
	highPcWrapped := ent.AttrField(dwarf.AttrHighpc)

	if lowPcWrapped == nil || highPcWrapped == nil {
		return fmt.Errorf("malformed non-inlined function entry: no low or high pc attributes present")
	}


	var lowPc, highPc uint64

	switch v := lowPcWrapped.Val.(type) {
		case uint64:
			lowPc = v
		case int64:
			lowPc = uint64(v)
		default:
			// TODO: Handle
			return fmt.Errorf("unrecognized lowPc format")
	}


	switch highPcWrapped.Class {
		case dwarf.ClassAddress: // we assume it's an absolute offset
			highPc = highPcWrapped.Val.(uint64)
		case dwarf.ClassConstant:
			highPc = lowPc + uint64(highPcWrapped.Val.(int64))
		default:
			return fmt.Errorf("unrecognized highPc format")
	}

	functionNameAttr := ent.AttrField(dwarf.AttrName)

	if functionNameAttr == nil {
		return fmt.Errorf("malformed non-inlined function entry: no name attribute")
	}

	functionName := functionNameAttr.Val.(string)

	// fmt.Printf("func %s has raw pcs: %v %v\n", functionName, lowPcWrapped, highPcWrapped)

	// For some reason the DeclLine attribute of our dwarf entry can be int64 ? Not too sure what's going on here
	// functionFile := ent.AttrField(dwarf.AttrDeclFile).Val.(string)
	fileIndex, ok := ent.AttrField(dwarf.AttrDeclFile).Val.(int64)
	if !ok || files == nil {
		return fmt.Errorf("can't extract function file")
	}
	functionFile := files[fileIndex].Name
	functionLine := ent.AttrField(dwarf.AttrDeclLine).Val.(int64)

	locationField := ent.AttrField(dwarf.AttrFrameBase)

	locationFieldType := (locationField.Val.([]uint8))[1]
	locationLocal := (locationField.Val.([]uint8))[2]
	// We only handle "Locals" locations
	// See: https://yurydelendik.github.io/webassembly-dwarf/#location-descriptions-locals
	if locationFieldType != 0 {
		r.SkipChildren()
		return fmt.Errorf("malformed non-inlined function entry: no location attribute present")
	}

	locals := make([]VariableRecord, 0)

	// Read children of Subprogram tag
	for {

		child, err := r.Next()

		// End of subprogram's children
		if err != nil {
			return err
		}

		if child.Tag == 0 {
			break
		}

		// if child.Tag == dwarf.TagLexDwarfBlock {
		//
		// }

		if child.Tag == dwarf.TagVariable {
			varNameField := child.AttrField(dwarf.AttrName)
			varLocationField := child.AttrField(dwarf.AttrLocation)

			if varNameField == nil || varLocationField == nil {
				continue;
			}

			varName := varNameField.Val.(string)

			var varLocation uint64
			// := (varLocationField.Val.([]uint8))[1]

			switch v := varLocationField.Val.(type) {
				case uint64:
					varLocation = v
				case []uint8:
					varLocation = uint64(v[1])
				default:
					fmt.Fprintf(os.Stderr, "unsupported Location attribute. Func with name %s has a vriable %s with location field: %v\n",
					functionName,
					varName,
					varLocationField.Val)
					continue
			}

			// TODO: This is not always a correct assertion

			locals = append(locals, VariableRecord{
				Name: varName,
				Offset: uint64(varLocation),
			})

		}

	}

	// fmt.Printf("Func %s (%d %d)\n", functionName, lowPc, highPc)

	tree.Function.Insert(lowPc, highPc, FunctionRecord{
		Name: functionName,
		FileName: functionFile,
		Line: int64(functionLine),
		FrameBaseIndex: uint64(locationLocal),
		Locals: locals,
	})

	return nil
}

func indexCompileUnit(cu *dwarf.Entry, d *dwarf.Data, tree *PCRecord) ([]*dwarf.LineFile, error) {
	lineReader, err := d.LineReader(cu)
	if err != nil || lineReader == nil {
		return nil, fmt.Errorf("can't initialize line reader: %v", err)
	}

	var le dwarf.LineEntry
	// Get the lines inside the entry.
	lines := make([]dwarf.LineEntry, 0)
	for {
		err = lineReader.Next(&le)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		// TODO: Maybe we should ignore tombstone addresses by using isTombstoneAddr,
		//  but not sure if that would be an issue in practice.
		lines = append(lines, le)
	}

	sort.Slice(lines, func(i, j int) bool { return lines[i].Address < lines[j].Address })

	// start := 0
	// for i, le := range lines {
	// 	if le.File != lines[start].File || le.Line != lines[start].Line || le.Column != lines[start].Column {
	// 		fmt.Printf("BLQBLQ %v:%v:%v (%v <-> %v)-> %v %v\n", lines[start].File.Name, lines[start].Line, lines[start].Column, start, i-1, lines[start].Address, lines[i-1].Address)
	// 		tree.Line.Insert(lines[start].Address, lines[i-1].Address, LineRecord{
	// 			FileName: lines[start].File.Name,
	// 			Line:     int64(lines[start].Line),
	// 			Column:   int64(lines[start].Column),
	// 		})
	// 		start = i
	// 	}
	// }

	for i, _ := range lines {
		if i-1 >= 0 {
			tree.Line.Insert(lines[i-1].Address, lines[i].Address-1, LineRecord{
				FileName: lines[i - 1].File.Name,
				Line:     int64(lines[i - 1].Line),
				Column:   int64(lines[i - 1].Column),
			})
		}
	}

	return lineReader.Files(), nil
}

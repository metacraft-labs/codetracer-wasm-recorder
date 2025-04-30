package wasmdebug

import (
	"debug/dwarf"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/rdleal/intervalst/interval"
)

type TypeRecord interface {
	isTypeRecord()
}

type BaseTypeRecord struct {
	Name string
	Size uint64
}

type StructTypeRecord struct {
	Name   string
	Fields []TypeRecord
}

type PointerTypeRecord struct {
	Name    string
	Address uint64
}

func (BaseTypeRecord) isTypeRecord()    {}
func (StructTypeRecord) isTypeRecord()  {}
func (PointerTypeRecord) isTypeRecord() {}

type LineRecord struct {
	FileName string
	Line     int64
	Column   int64
}

type VariableRecord struct {
	Name   string
	Offset uint64
	Type   dwarf.Type
}

type FunctionRecord struct {
	Name           string
	FileName       string
	Line           int64
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

type Offset uint64

type PCRecord struct {
	Line            *interval.SearchTree[LineRecord, uint64]
	Function        *interval.SearchTree[FunctionRecord, uint64]
	InlinedRoutines *interval.SearchTree[[]InlineRecord, uint64]
	Locals          *interval.SearchTree[[]VariableRecord, uint64]
	Types           map[Offset]TypeRecord
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

		// fmt.Printf("TAG: %s\n", ent.Tag.String())

		for _, pcs := range ranges {
			start, end := pcs[0], pcs[1]

			if isTombstoneAddr(start) || isTombstoneAddr(end) {
				continue
			}

			switch ent.Tag {

			case dwarf.TagCompileUnit:
				if files, err = indexCompileUnit(ent, d, &ret); err != nil {
					fmt.Fprintf(os.Stderr, "error indexing compile unit: %v\n", err)
					continue
				}

			case dwarf.TagInlinedSubroutine:

			case dwarf.TagSubprogram:
				if err := indexFunctionEntry(r, ent, d, files, &ret); err != nil {
					fmt.Fprintf(os.Stderr, "error indexing function: %v\n", err)
					continue
				}

			case dwarf.TagBaseType:
				if err := indexBaseType(r, ent, &ret); err != nil {
					fmt.Fprintf(os.Stderr, "error indexing type: %v\n", err)
					continue
				}

			case dwarf.TagStructType:

			case dwarf.TagPointerType:

			}
		}
	}

	return
}

func indexBaseType(r *dwarf.Reader, ent *dwarf.Entry, tree *PCRecord) error {

	typeNameAttribute := ent.AttrField(dwarf.AttrName)
	if typeNameAttribute == nil {
		return fmt.Errorf("malformed type entry: type has no name")
	}

	typeName := typeNameAttribute.Val.(string)

	typeSizeAttribute := ent.AttrField(dwarf.AttrByteSize)
	if typeSizeAttribute == nil {
		return fmt.Errorf("malformed type entry: type has no size")
	}

	typeSize := typeSizeAttribute.Val.(uint64)

	record := BaseTypeRecord{
		Name: typeName,
		Size: typeSize,
	}

	fmt.Printf("Indexed type %s with size %d and offset in DWARF %d", typeName, typeSize, ent.Offset)
	tree.Types[Offset(ent.Offset)] = record

	return nil
}

func indexFunctionEntry(r *dwarf.Reader, ent *dwarf.Entry, d *dwarf.Data, files []*dwarf.LineFile, tree *PCRecord) error {
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

		if child == nil {
			break
		}

		if child.Tag == 0 {
			break
		}

		// if child.Tag == dwarf.TagLexDwarfBlock {
		//
		// }

		if child.Tag == dwarf.TagVariable {

			varTypeField := child.AttrField(dwarf.AttrType)

			varNameField := child.AttrField(dwarf.AttrName)
			varLocationField := child.AttrField(dwarf.AttrLocation)

			if varNameField == nil || varLocationField == nil {
				continue
			}

			varName := varNameField.Val.(string)

			fmt.Printf("VAR %s HAS TYPE: %v\n", varName, varTypeField)

			varTypeOffset := varTypeField.Val.(dwarf.Offset)

			varType, _ := d.Type(varTypeOffset)

			// We can not do type assertions on varTypeTrue

			switch t := varType.(type) {
			case *dwarf.IntType:
				fmt.Printf("WE HAVE A BASIC TYPE: %v\n", t)
			case *dwarf.PtrType:
				fmt.Printf("WE HAVE A PTR TYPE: %v\n", t)

			default:
				fmt.Printf("WE HAVE SOMETHING ELSE: %T\n", t)
			}

			var varLocation uint64

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
				Name:   varName,
				Offset: uint64(varLocation),
				Type:   varType,
			})

		}

	}

	tree.Function.Insert(lowPc, highPc, FunctionRecord{
		Name:           functionName,
		FileName:       functionFile,
		Line:           int64(functionLine),
		FrameBaseIndex: uint64(locationLocal),
		Locals:         locals,
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
				FileName: lines[i-1].File.Name,
				Line:     int64(lines[i-1].Line),
				Column:   int64(lines[i-1].Column),
			})
		}
	}

	return lineReader.Files(), nil
}

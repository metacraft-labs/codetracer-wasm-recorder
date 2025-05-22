package wasmdebug

import (
	"debug/dwarf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/rdleal/intervalst/interval"
)

type LocationType uint8

const (
	LocationTypeLocal LocationType = iota
	LocationTypeGlobal
	OperandStack
)

type MemoryLocation struct {
	Typ   LocationType
	Index uint32
}

type TypeRecord interface {
	isTypeRecord()
}

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
	Name      string
	FileName  string
	Line      int64
	FrameBase MemoryLocation
	Params    []VariableRecord
	Locals    []VariableRecord
	LowPC     uint64
	HighPC    uint64

	// When nil, then the function is void / doesn't return value
	ReturnType *dwarf.Type
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

	offset2function := make(map[dwarf.Offset]*FunctionRecord, 0)

	for {
		ent, err := r.Next()
		if err != nil || ent == nil {
			break
		}

		switch ent.Tag {

		case dwarf.TagCompileUnit:
			if files, err = indexCompileUnit(ent, d, &ret); err != nil {
				fmt.Fprintf(os.Stderr, "error indexing compile unit: %v\n", err)
			}

		case dwarf.TagInlinedSubroutine:
			// TODO

		case dwarf.TagSubprogram:
			if err := indexFunctionEntry(r, ent, d, files, offset2function); err != nil {
				fmt.Fprintf(os.Stderr, "error indexing function:\n\t%#v\n\t%v\n", ent, err)
			}
		}
	}

	for _, record := range offset2function {
		// TODO: are these all the cases when an entry is invalid?
		if record == nil || isTombstoneAddr(record.LowPC) || isTombstoneAddr(record.HighPC) || record.Name == "" {
			fmt.Fprintf(os.Stderr, "Malformed function entry %#v\n", record)
			continue
		}

		fmt.Printf("INDEXED %#v\n", record)

		ret.Function.Insert(record.LowPC, record.HighPC, *record)
	}

	return
}

func indexFunctionEntry(r *dwarf.Reader, ent *dwarf.Entry, d *dwarf.Data, files []*dwarf.LineFile, offset2function map[dwarf.Offset]*FunctionRecord) error {
	functionOffset := ent.Offset
	if specWrapped := ent.AttrField(dwarf.AttrSpecification); specWrapped != nil {
		functionOffset = specWrapped.Val.(dwarf.Offset)
	}

	record := offset2function[functionOffset]
	if record == nil {
		tmp := -1
		record = &FunctionRecord{
			LowPC:  uint64(tmp),
			HighPC: uint64(tmp),
		}
		offset2function[functionOffset] = record
	}

	if lowPcWrapped := ent.AttrField(dwarf.AttrLowpc); lowPcWrapped != nil {
		switch v := lowPcWrapped.Val.(type) {
		case uint64:
			record.LowPC = v
		// case int64:
		//	record.LowPC = uint64(v)
		default:
			return fmt.Errorf("unrecognized lowPc format")
		}
	}

	if highPcWrapped := ent.AttrField(dwarf.AttrHighpc); highPcWrapped != nil {
		switch highPcWrapped.Class {
		case dwarf.ClassAddress: // we assume it's an absolute offset
			record.HighPC = highPcWrapped.Val.(uint64)
		case dwarf.ClassConstant:
			// TODO: This is wrong assumption if there is a case when pc record are in different entries
			record.HighPC = record.LowPC + uint64(highPcWrapped.Val.(int64))
		default:
			return fmt.Errorf("unrecognized highPc format")
		}
	}

	if functionNameAttr := ent.AttrField(dwarf.AttrName); functionNameAttr != nil {
		record.Name = functionNameAttr.Val.(string)
	}

	if fileIndexWrapped := ent.AttrField(dwarf.AttrDeclFile); fileIndexWrapped != nil {
		fileIndex, ok := fileIndexWrapped.Val.(int64)
		if !ok {
			return fmt.Errorf("unrecognized fileIndex format")
		}
		record.FileName = files[fileIndex].Name
	}

	if functionLineWrapped := ent.AttrField(dwarf.AttrDeclLine); functionLineWrapped != nil {
		functionLine, ok := functionLineWrapped.Val.(int64)
		if !ok {
			return fmt.Errorf("unrecognized functionLine format")
		}

		record.Line = functionLine
	}

	if locationField := ent.AttrField(dwarf.AttrFrameBase); locationField != nil {
		// TODO: handle more formats
		locationFieldValue := locationField.Val.([]uint8)
		locationFieldType := locationFieldValue[1]

		if locationFieldType == 0 {
			record.FrameBase.Typ = LocationTypeLocal
			record.FrameBase.Index = uint32(parseLEB128(locationFieldValue[2:]))
		} else if locationFieldType == 1 {
			record.FrameBase.Typ = LocationTypeGlobal
			record.FrameBase.Index = uint32(parseLEB128(locationFieldValue[2:]))
		} else if locationFieldType == 3 {
			record.FrameBase.Typ = LocationTypeGlobal
			record.FrameBase.Index = binary.LittleEndian.Uint32(locationFieldValue[2:6])
		} else if locationFieldType == 2 {
			record.FrameBase.Typ = OperandStack
			record.FrameBase.Index = uint32(parseLEB128(locationFieldValue[2:]))
		} else {
			return fmt.Errorf("found invalid WASM location")
		}

		params := make([]VariableRecord, 0)
		locals := make([]VariableRecord, 0)

		// Read children of Subprogram tag
		for ent.Children {

			child, err := r.Next()
			fmt.Printf("CHILD %v %v if of %v\n", child.Tag.GoString(), child.AttrField(dwarf.AttrName), ent.AttrField(dwarf.AttrName))

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

			if child.Tag == dwarf.TagVariable || child.Tag == dwarf.TagFormalParameter {

				varTypeField := child.AttrField(dwarf.AttrType)

				varNameField := child.AttrField(dwarf.AttrName)
				varLocationField := child.AttrField(dwarf.AttrLocation)

				if varNameField == nil || varLocationField == nil {
					continue
				}

				varName := varNameField.Val.(string)

				varTypeOffset := varTypeField.Val.(dwarf.Offset)

				varType, _ := d.Type(varTypeOffset)

				var varLocation uint64

				switch v := varLocationField.Val.(type) {
				case uint64:
					varLocation = v

				case []uint8:
					res := parseLEB128(v[1:])
					varLocation = res

				default:
					fmt.Fprintf(os.Stderr, "unsupported Location attribute. Func %#v has a vriable %s with location field: %v\n",
						record,
						varName,
						varLocationField.Val)
					continue
				}

				if child.Tag == dwarf.TagVariable {
					locals = append(locals, VariableRecord{
						Name:   varName,
						Offset: varLocation,
						Type:   varType,
					})
				} else if child.Tag == dwarf.TagFormalParameter {
					params = append(params, VariableRecord{
						Name:   varName,
						Offset: varLocation,
						Type:   varType,
					})
				}

			}
		}
		// TODO: append or rewrite?
		record.Params = append(record.Params, params...)
		record.Locals = append(record.Locals, locals...)
	}

	if funcTypeField := ent.AttrField(dwarf.AttrType); funcTypeField != nil {
		funcTypeOffset := funcTypeField.Val.(dwarf.Offset)
		returnType, _ := d.Type(funcTypeOffset)

		record.ReturnType = &returnType
	}

	return nil
}

func parseLEB128(v []uint8) uint64 {
	shift := 0
	var res uint64 = 0
	for i := 0; i < len(v); i++ {

		// get first 7 bits of 8 bit chunk
		payload := int(v[i]) & 0b01111111

		res += uint64(payload * (1 << shift))

		// 8th bit is used to check whether there's "more data to read"
		if (v[i] & 0b10000000) == 0 {
			break
		}

		shift += 7
	}
	return res
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

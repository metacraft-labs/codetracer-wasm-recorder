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
	Params         []VariableRecord
	Locals         []VariableRecord

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
			if err := indexFunctionEntry(r, ent, d, files, &ret); err != nil {
				fmt.Fprintf(os.Stderr, "error indexing function:\n\t%#v\n\t%v\n", ent, err)
			}
		}
	}

	return
}

func indexFunctionEntry(r *dwarf.Reader, ent *dwarf.Entry, d *dwarf.Data, files []*dwarf.LineFile, tree *PCRecord) error {
	lowPcWrapped := ent.AttrField(dwarf.AttrLowpc)
	highPcWrapped := ent.AttrField(dwarf.AttrHighpc)

	if lowPcWrapped == nil || highPcWrapped == nil {
		return fmt.Errorf("malformed function entry: no low or high pc attributes present")
	}

	var lowPc, highPc uint64

	switch v := lowPcWrapped.Val.(type) {
	case uint64:
		lowPc = v
	case int64:
		lowPc = uint64(v)
	default:
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
		return fmt.Errorf("malformed function entry: no name attribute")
	}

	functionName := functionNameAttr.Val.(string)

	// fmt.Printf("func %s has raw pcs: %v %v\n", functionName, lowPcWrapped, highPcWrapped)

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
		return fmt.Errorf("unsupported location attribute found: %v", locationFieldType)
	}

	params := make([]VariableRecord, 0)
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

				// LEB128 decoding
				shift := 0
				var res uint64 = 0
				for i := 1; i < len(v); i++ {

					// get first 7 bits of 8 bit chunk
					payload := int(v[i]) & 0b01111111

					res += uint64(payload*(1<<shift))

					// 8th bit is used to check whether there's "more data to read"
					if (v[i] & 0b10000000) == 0 {
						break
					}

					shift += 7
				}
				varLocation = res

			default:
				fmt.Fprintf(os.Stderr, "unsupported Location attribute. Func with name %s has a vriable %s with location field: %v\n",
					functionName,
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

	record := FunctionRecord{
		Name:           functionName,
		FileName:       functionFile,
		Line:           int64(functionLine),
		FrameBaseIndex: uint64(locationLocal),
		Params:         params,
		Locals:         locals,
		ReturnType:     nil,
	}

	funcTypeField := ent.AttrField(dwarf.AttrType)
	if funcTypeField != nil {
		funcTypeOffset := funcTypeField.Val.(dwarf.Offset)
		returnType, _ := d.Type(funcTypeOffset)

		record.ReturnType = &returnType
	}

	tree.Function.Insert(lowPc, highPc, record)

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

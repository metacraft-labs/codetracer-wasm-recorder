package wasmdebug

import (
	"debug/dwarf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

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
	LowPC        uint64
	HighPC       uint64
	Params       []VariableRecord
	Locals       []VariableRecord
	CallFileName string
	CallLine     int64
	CallColumn   int64
}

type Offset uint64

type TemplateParamMap map[string]dwarf.Type

type PCRecord struct {
	Line            *interval.SearchTree[LineRecord, uint64]
	Function        *interval.SearchTree[FunctionRecord, uint64]
	InlinedRoutines *interval.SearchTree[[]InlineRecord, uint64]
	Locals          *interval.SearchTree[[]VariableRecord, uint64]
	TypeParamMap    map[string]TemplateParamMap
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
		TypeParamMap:    make(map[string]TemplateParamMap),
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

		// case dwarf.TagInlinedSubroutine:
		// 	fmt.Println("INDEXING INLINED ENTRY INFORMATION!")
		// 	if err := indexInlineEntry(r, ent, d, files, &ret); err != nil {
		// 		fmt.Fprintf(os.Stderr, "error indexing inlined subroutine:\n\t%#v\n\t%v\n", ent, err)
		// 	}

		case dwarf.TagSubprogram:
			if err := indexFunctionEntry(r, ent, d, files, &ret, offset2function); err != nil {
				fmt.Fprintf(os.Stderr, "error indexing function:\n\t%#v\n\t%v\n", ent, err)
			}

		case dwarf.TagStructType:
			if err := indexStructType(d, ent, &ret); err != nil {
				fmt.Fprintf(os.Stderr, "error indexing struct type:\n\t%#v\n\t%v\n", ent, err)
			}
		}
	}

	fmt.Printf("Type param map: %#v\n", ret.TypeParamMap)

	for _, record := range offset2function {
		// TODO: are these all the cases when an entry is invalid?
		if record == nil || isTombstoneAddr(record.LowPC) || isTombstoneAddr(record.HighPC) || record.Name == "" {
			fmt.Fprintf(os.Stderr, "Malformed function entry %#v\n", record)
			continue
		}

		fmt.Printf("INDEXED %#v\n", record)

		ret.Function.Insert(record.LowPC, record.HighPC-1, *record)
	}

	return
}

func indexStructType(d *dwarf.Data, ent *dwarf.Entry, ret *PCRecord) error {
	sr := d.Reader()
	sr.Seek(ent.Offset)

	fmt.Printf("INDEXING TYPE PARAMS FOR STRUCT: %s\n", ent.AttrField(dwarf.AttrName).Val.(string))

	for ent.Children {
		child, err := sr.Next()

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

		if child.Tag == dwarf.TagTemplateTypeParameter {

			typOffset := child.AttrField(dwarf.AttrType).Val.(dwarf.Offset)
			templateName := child.AttrField(dwarf.AttrName).Val.(string)

			paramType, err := d.Type(typOffset)

			if err != nil {
				continue
			}

			typeName := "struct " + ent.AttrField(dwarf.AttrName).Val.(string)

			if _, ok := ret.TypeParamMap[typeName]; !ok {
				ret.TypeParamMap[typeName] = make(TemplateParamMap)
			}

			ret.TypeParamMap[typeName][templateName] = paramType

			fmt.Printf("FOUND TYPE PARAM: %s %s %#v\n", typeName, templateName, paramType)
		}

	}
	return nil
}

func indexFunctionEntry(r *dwarf.Reader, ent *dwarf.Entry, d *dwarf.Data, files []*dwarf.LineFile, ret *PCRecord, offset2function map[dwarf.Offset]*FunctionRecord) error {
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

			if child.Tag == dwarf.TagInlinedSubroutine {
				fmt.Printf("FOUND INLINED SUBROUTINE FOR %s\n", record.Name)
				if err := indexInlineEntry(r, child, d, files, ret); err != nil {
					return err
				}
				continue
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

func indexInlineEntry(r *dwarf.Reader, ent *dwarf.Entry, d *dwarf.Data, files []*dwarf.LineFile, recordTree *PCRecord) error {
	var lowPC, highPC uint64

	if lowPcField := ent.AttrField(dwarf.AttrLowpc); lowPcField != nil {
		switch v := lowPcField.Val.(type) {
		case uint64:
			lowPC = v
		case int64:
			lowPC = uint64(v)
		default:
			return fmt.Errorf("unrecognized lowPc format")
		}
	}

	if highPcField := ent.AttrField(dwarf.AttrHighpc); highPcField != nil {
		switch highPcField.Class {
		case dwarf.ClassAddress:
			highPC = highPcField.Val.(uint64)
		case dwarf.ClassConstant:
			highPC = lowPC + uint64(highPcField.Val.(int64))
		default:
			return fmt.Errorf("unrecognized highPc format")
		}
	}

	rec := InlineRecord{
		LowPC:  lowPC,
		HighPC: highPC,
	}

	if originField := ent.AttrField(dwarf.AttrAbstractOrigin); originField != nil {
		or := d.Reader()
		or.Seek(originField.Val.(dwarf.Offset))
		origin, err := or.Next()
		if err == nil && origin != nil {
			if nameField := origin.AttrField(dwarf.AttrName); nameField != nil {
				rec.Name = nameField.Val.(string)
			}
			if fileField := origin.AttrField(dwarf.AttrDeclFile); fileField != nil {
				if idx, ok := fileField.Val.(int64); ok && int(idx) < len(files) {
					rec.FileName = files[idx].Name
				}
			}
			if lineField := origin.AttrField(dwarf.AttrDeclLine); lineField != nil {
				rec.Line, _ = lineField.Val.(int64)
			}
		}
	}

	if fileField := ent.AttrField(dwarf.AttrCallFile); fileField != nil {
		if idx, ok := fileField.Val.(int64); ok && int(idx) < len(files) {
			rec.CallFileName = files[idx].Name
		}
	}
	if lineField := ent.AttrField(dwarf.AttrCallLine); lineField != nil {
		rec.CallLine, _ = lineField.Val.(int64)
	}
	if colField := ent.AttrField(dwarf.AttrCallColumn); colField != nil {
		rec.CallColumn, _ = colField.Val.(int64)
	}

	// if ent.Children {
	var lexCount uint32
	for ent.Children {
		child, err := r.Next()

		if err != nil {
			return err
		}
		if child == nil || child.Tag == 0 {
			if lexCount == 0 {
				break
			} else {
				lexCount--
			}
		}

		switch child.Tag {
		case dwarf.TagLexDwarfBlock:
			lexCount++
			continue
		case dwarf.TagInlinedSubroutine:
			if err := indexInlineEntry(r, child, d, files, recordTree); err != nil {
				return err
			}
			continue
		case dwarf.TagVariable:
			varTypeField := child.AttrField(dwarf.AttrType)
			varNameField := child.AttrField(dwarf.AttrName)
			varLocationField := child.AttrField(dwarf.AttrLocation)

			if varNameField == nil || varLocationField == nil {
				// try resolving via abstract origin
				if originAttr := child.AttrField(dwarf.AttrAbstractOrigin); originAttr != nil {
					or := d.Reader()
					or.Seek(originAttr.Val.(dwarf.Offset))
					orig, _ := or.Next()
					if orig != nil {
						if varNameField == nil {
							varNameField = orig.AttrField(dwarf.AttrName)
						}
						if varTypeField == nil {
							varTypeField = orig.AttrField(dwarf.AttrType)
						}
						if varLocationField == nil {
							varLocationField = orig.AttrField(dwarf.AttrLocation)
						}
					}
				}
			}

			if varNameField == nil || varLocationField == nil {
				continue
			}

			varName := varNameField.Val.(string)
			varType, _ := func() (dwarf.Type, error) {
				if varTypeField != nil {
					return d.Type(varTypeField.Val.(dwarf.Offset))
				}
				return nil, nil
			}()

			var loc uint64
			switch v := varLocationField.Val.(type) {
			case uint64:
				loc = v
			case []uint8:
				loc = parseLEB128(v[1:])
			default:
				continue
			}

			if child.Tag == dwarf.TagVariable {
				rec.Locals = append(rec.Locals, VariableRecord{Name: varName, Offset: loc, Type: varType})
			} else {
				rec.Params = append(rec.Params, VariableRecord{Name: varName, Offset: loc, Type: varType})
			}
		}
	}
	// } else {
	// 	r.SkipChildren()
	// }

	if !isTombstoneAddr(lowPC) && !isTombstoneAddr(highPC) && rec.Name != "" {

		entry, found := recordTree.InlinedRoutines.Find(lowPC, highPC)

		if !found {
			x := make([]InlineRecord, 0)
			x = append(x, rec)
			recordTree.InlinedRoutines.Insert(lowPC, highPC, x)
		} else {
			entry = append(entry, rec)
			recordTree.InlinedRoutines.Insert(lowPC, highPC, entry)
		}

		fmt.Printf("INSERTING INLINE INTERVAL [%d %d] : %#v\n", lowPC, highPC, rec)
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
	fmt.Println("---------------------------------------------------------")
	lineReader, err := d.LineReader(cu)
	if err != nil || lineReader == nil {
		return nil, fmt.Errorf("can't initialize line reader: %v", err)
	}

	var le dwarf.LineEntry
	var prevLe *dwarf.LineEntry

	for {
		err = lineReader.Next(&le)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}

		if prevLe != nil {
			if prevLe.IsStmt {
				fmt.Printf("LINE: %s:%d:%d RANGE: [%x; %x]\n", prevLe.File.Name, prevLe.Line, prevLe.Column, prevLe.Address, le.Address-1)
				tree.Line.Insert(prevLe.Address, le.Address-1, LineRecord{
					FileName: prevLe.File.Name,
					Line:     int64(prevLe.Line),
					Column:   int64(prevLe.Column),
				})
			}
		} else {
			prevLe = &dwarf.LineEntry{}
		}
		*prevLe = le

		if prevLe.EndSequence {
			prevLe = nil
		}
	}

	return lineReader.Files(), nil
}

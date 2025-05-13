package interpreter

import (
	"debug/dwarf"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero/internal/wasm"
)

func bytesToStringRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {

	record := m.Record

	mem := m.Memory()

	data, _ := bytesToStruct(rawBytes, typ, m)

	val, ok := data.(trace_record.StructValueRecord)

	if !ok {
		return nil, fmt.Errorf("not a string slice")
	}

	data_ptr_field, err := val.Fields[0].(trace_record.ReferenceValueRecord)

	if !err {
		return nil, fmt.Errorf("not a string slice")
	}

	addr := data_ptr_field.Address

	len_field, ok := val.Fields[1].(trace_record.IntValueRecord)

	if !ok {
		return nil, fmt.Errorf("not a string slice")
	}

	len := len_field.I

	str := ""

	for i := 0; i < int(len); i++ {
		data, _ := mem.Read(addr+uint32(i), 1)
		str += string(data[0])
	}

	pointerTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "String")
	typeId := record.RegisterTypeWithNewId(typ.Name, pointerTypeRecord)

	return trace_record.StringValue(str, typeId), nil

}

func bytesToSliceRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	record := m.Record

	mem := m.Memory()

	rawStruct, err := bytesToStruct(rawBytes, typ, m)
	if err != nil {
		return nil, err
	}

	fields := rawStruct.(trace_record.StructValueRecord).Fields

	if len(fields) != 2 {
		return nil, fmt.Errorf("not a slice")
	}

	var addr uint32
	if addrRecord, ok := fields[0].(trace_record.ReferenceValueRecord); ok {
		addr = addrRecord.Address
	} else {
		return nil, fmt.Errorf("not a slice")
	}

	var length uint32
	if lenRecord, ok := fields[1].(trace_record.IntValueRecord); ok {
		length = uint32(lenRecord.I)
	} else {
		return nil, fmt.Errorf("not a slice")
	}

	var elemType dwarf.Type
	var elemSize uint32
	if ptrTyp, ok := typ.Field[0].Type.(*dwarf.PtrType); ok {
		elemType = ptrTyp.Type
		elemSize = uint32(ptrTyp.Type.Common().ByteSize)
	} else {
		return nil, fmt.Errorf("not a slice")
	}

	// TODO: what type kind?
	// TODO: what should the string parameter be?
	sliceTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Slice")
	typeId := record.RegisterTypeWithNewId(typ.Name, sliceTypeRecord)

	elems := make([]trace_record.ValueRecord, 0)
	for i := uint32(0); i < length; i++ {
		elemBytes, ok := mem.Read(addr+i*elemSize, elemSize)
		if !ok {
			return trace_record.SequenceValue(elems, true, typeId), fmt.Errorf("invalid memory access")
		}

		elem, err := bytesToValueRecord(elemBytes, elemType, m)
		if err != nil {
			return trace_record.SequenceValue(elems, true, typeId), err
		}

		elems = append(elems, elem)
	}

	return trace_record.SequenceValue(elems, true, typeId), nil

}

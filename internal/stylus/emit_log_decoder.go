package stylus

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

type abiKind int

const (
	abiInvalid abiKind = iota
	abiUint
	abiInt
	abiBool
	abiAddress
	abiFixedBytes
	abiBytes
	abiString
	abiArray
)

type abiType struct {
	kind     abiKind
	bitSize  int
	byteSize int
	length   int // -1 for dynamic arrays
	elem     *abiType
	raw      string
}

func (t abiType) isDynamic() bool {
	switch t.kind {
	case abiBytes, abiString:
		return true
	case abiArray:
		if t.length < 0 {
			return true
		}
		if t.elem == nil {
			return true
		}
		return t.elem.isDynamic()
	default:
		return false
	}
}

func (t abiType) staticSize() (int, error) {
	switch t.kind {
	case abiUint, abiInt, abiBool, abiAddress, abiFixedBytes:
		return 32, nil
	case abiArray:
		if t.length < 0 {
			return 0, fmt.Errorf("dynamic array has no static size")
		}
		if t.elem == nil {
			return 0, fmt.Errorf("array missing element type")
		}
		elemSize, err := t.elem.staticSize()
		if err != nil {
			return 0, err
		}
		return t.length * elemSize, nil
	default:
		return 0, fmt.Errorf("type %q has no static size", t.raw)
	}
}

func (t abiType) topicEncodable() bool {
	if t.isDynamic() {
		return false
	}
	size, err := t.staticSize()
	if err != nil {
		return false
	}
	return size == 32
}

func parseABIType(typeStr string) (abiType, error) {
	clean := strings.TrimSpace(typeStr)
	clean = strings.ReplaceAll(clean, "payable", "")
	clean = strings.ReplaceAll(clean, " ", "")
	if clean == "" {
		return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("empty type")
	}

	baseBuilder := strings.Builder{}
	arrayDims := []int{}
	depth := 0
	for i := 0; i < len(clean); {
		ch := clean[i]
		switch ch {
		case '(':
			depth++
			baseBuilder.WriteByte(ch)
			i++
		case ')':
			if depth == 0 {
				return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unbalanced parentheses in type %q", typeStr)
			}
			depth--
			baseBuilder.WriteByte(ch)
			i++
		case '[':
			if depth > 0 {
				baseBuilder.WriteByte(ch)
				i++
				continue
			}
			end := strings.IndexByte(clean[i:], ']')
			if end == -1 {
				return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unclosed array dimension in %q", typeStr)
			}
			end += i
			dimStr := clean[i+1 : end]
			if dimStr == "" {
				arrayDims = append(arrayDims, -1)
			} else {
				dim, err := strconv.ParseInt(dimStr, 10, 32)
				if err != nil {
					return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("invalid array length %q in type %q", dimStr, typeStr)
				}
				if dim < 0 {
					return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("negative array length in type %q", typeStr)
				}
				arrayDims = append(arrayDims, int(dim))
			}
			i = end + 1
		default:
			baseBuilder.WriteByte(ch)
			i++
		}
	}

	if depth != 0 {
		return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unbalanced parentheses in type %q", typeStr)
	}

	base := baseBuilder.String()
	t := abiType{raw: typeStr}

	switch {
	case base == "address":
		t.kind = abiAddress
	case base == "bool":
		t.kind = abiBool
	case base == "string":
		t.kind = abiString
	case base == "bytes":
		t.kind = abiBytes
	case strings.HasPrefix(base, "uint"):
		size := 256
		if len(base) > len("uint") {
			val, err := strconv.Atoi(base[len("uint"):])
			if err != nil {
				return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("invalid uint size in type %q", typeStr)
			}
			size = val
		}
		if size <= 0 || size > 256 || size%8 != 0 {
			return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unsupported uint size %d in type %q", size, typeStr)
		}
		t.kind = abiUint
		t.bitSize = size
	case strings.HasPrefix(base, "int"):
		size := 256
		if len(base) > len("int") {
			val, err := strconv.Atoi(base[len("int"):])
			if err != nil {
				return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("invalid int size in type %q", typeStr)
			}
			size = val
		}
		if size <= 0 || size > 256 || size%8 != 0 {
			return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unsupported int size %d in type %q", size, typeStr)
		}
		t.kind = abiInt
		t.bitSize = size
	case strings.HasPrefix(base, "bytes"):
		if len(base) == len("bytes") {
			t.kind = abiBytes
		} else {
			v, err := strconv.Atoi(base[len("bytes"):])
			if err != nil {
				return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("invalid fixed bytes size in type %q", typeStr)
			}
			if v <= 0 || v > 32 {
				return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unsupported fixed bytes size %d in type %q", v, typeStr)
			}
			t.kind = abiFixedBytes
			t.byteSize = v
		}
	default:
		return abiType{kind: abiInvalid, raw: typeStr}, fmt.Errorf("unsupported type %q", typeStr)
	}

	if len(arrayDims) == 0 {
		return t, nil
	}

	current := t
	for i := len(arrayDims) - 1; i >= 0; i-- {
		length := arrayDims[i]
		elemCopy := current
		current = abiType{
			kind:   abiArray,
			length: length,
			elem:   &elemCopy,
			raw:    typeStr,
		}
	}
	return current, nil
}

type parameterValue struct {
	param   eventParameter
	value   string
	warning string
}

func decodeEventParameters(sig eventSignature, topics [][]byte, data []byte) ([]parameterValue, []string) {
	values := make([]parameterValue, len(sig.Params))
	for i, param := range sig.Params {
		values[i] = parameterValue{param: param}
	}

	var warnings []string

	targets := []dataTarget{}
	topicIdx := 0

	for i, param := range sig.Params {
		typ, err := parseABIType(param.Type)
		if err != nil {
			values[i].value = fmt.Sprintf("<unsupported type: %s>", err.Error())
			continue
		}

		if param.Indexed {
			if topicIdx >= len(topics) {
				label := parameterLabel(param, i)
				values[i].value = "<missing topic>"
				warnings = append(warnings, fmt.Sprintf("insufficient topics to decode indexed parameter %s", label))
				continue
			}

			topic := topics[topicIdx]
			topicIdx++

			if typ.isDynamic() || !typ.topicEncodable() {
				values[i].value = fmt.Sprintf("keccak256=%s", hexBytes(topic))
				if typ.isDynamic() {
					values[i].warning = "dynamic indexed parameters expose only the keccak256 hash"
				} else {
					values[i].warning = "indexed value exceeds 32 bytes; showing raw topic hash"
				}
				continue
			}

			decoded, err := decodeStaticTopicValue(typ, topic)
			if err != nil {
				values[i].value = hexBytes(topic)
				values[i].warning = fmt.Sprintf("decode error: %v", err)
				continue
			}

			values[i].value = decoded
		} else {
			targets = append(targets, dataTarget{paramIdx: i, typ: typ})
		}
	}

	if topicIdx < len(topics) {
		warnings = append(warnings, fmt.Sprintf("unused topics: %d", len(topics)-topicIdx))
	}

	dataResults, dataWarnings := decodeNonIndexedData(targets, data)
	for i, result := range dataResults {
		target := targets[i]
		if result.value != "" {
			values[target.paramIdx].value = result.value
		}
		if result.warning != "" {
			values[target.paramIdx].warning = appendWarning(values[target.paramIdx].warning, result.warning)
		}
	}

	warnings = append(warnings, dataWarnings...)

	return values, warnings
}

type dataTarget struct {
	paramIdx int
	typ      abiType
}

type decodeResult struct {
	value   string
	warning string
}

func decodeNonIndexedData(targets []dataTarget, data []byte) ([]decodeResult, []string) {
	results := make([]decodeResult, len(targets))
	warnings := []string{}

	type dynamicJob struct {
		targetIdx int
		typ       abiType
		offset    int
	}

	dynamicJobs := []dynamicJob{}
	cursor := 0

	for i, target := range targets {
		if target.typ.isDynamic() {
			if cursor+32 > len(data) {
				results[i].value = "<insufficient data>"
				results[i].warning = "not enough bytes for dynamic value offset"
				warnings = append(warnings, fmt.Sprintf("parameter %d offset out of bounds", target.paramIdx))
				cursor = len(data)
				continue
			}
			offset, err := wordToInt(data[cursor : cursor+32])
			if err != nil {
				results[i].value = "<invalid offset>"
				results[i].warning = err.Error()
				warnings = append(warnings, fmt.Sprintf("parameter %d has invalid offset", target.paramIdx))
				cursor += 32
				continue
			}
			dynamicJobs = append(dynamicJobs, dynamicJob{targetIdx: i, typ: target.typ, offset: offset})
			cursor += 32
			continue
		}

		size, err := target.typ.staticSize()
		if err != nil {
			results[i].value = "<unsupported static type>"
			results[i].warning = err.Error()
			continue
		}

		if cursor+size > len(data) {
			results[i].value = "<insufficient data>"
			results[i].warning = "not enough bytes for static value"
			warnings = append(warnings, fmt.Sprintf("parameter %d static data truncated", target.paramIdx))
			cursor = len(data)
			continue
		}

		chunk := data[cursor : cursor+size]
		value, err := decodeStaticBytes(target.typ, chunk)
		if err != nil {
			results[i].value = hexBytes(chunk)
			results[i].warning = err.Error()
		} else {
			results[i].value = value
		}

		cursor += size
	}

	for _, job := range dynamicJobs {
		if job.offset < 0 || job.offset > len(data) {
			results[job.targetIdx].value = "<invalid offset>"
			results[job.targetIdx].warning = "offset outside payload"
			warnings = append(warnings, fmt.Sprintf("parameter %d offset outside payload", targets[job.targetIdx].paramIdx))
			continue
		}

		value, err := decodeDynamicValue(job.typ, data, job.offset)
		if err != nil {
			results[job.targetIdx].value = fmt.Sprintf("<decode error: %v>", err)
			results[job.targetIdx].warning = err.Error()
		} else {
			results[job.targetIdx].value = value
		}
	}

	return results, warnings
}

func decodeStaticBytes(t abiType, chunk []byte) (string, error) {
	switch t.kind {
	case abiUint:
		return decodeUint(chunk), nil
	case abiInt:
		return decodeInt(chunk), nil
	case abiBool:
		if len(chunk) != 32 {
			return "", fmt.Errorf("invalid bool size %d", len(chunk))
		}
		val := chunk[len(chunk)-1]
		if val == 0 {
			return "false", nil
		}
		if val == 1 {
			return "true", nil
		}
		return "", fmt.Errorf("invalid bool value 0x%x", chunk)
	case abiAddress:
		if len(chunk) != 32 {
			return "", fmt.Errorf("invalid address size %d", len(chunk))
		}
		return fmt.Sprintf("0x%s", hex.EncodeToString(chunk[12:])), nil
	case abiFixedBytes:
		if len(chunk) != 32 {
			return "", fmt.Errorf("invalid fixed bytes size %d", len(chunk))
		}
		return fmt.Sprintf("0x%s", hex.EncodeToString(chunk[:t.byteSize])), nil
	case abiArray:
		if t.length < 0 {
			return "", fmt.Errorf("dynamic array treated as static")
		}
		if t.elem == nil {
			return "", fmt.Errorf("array missing element type")
		}
		elemSize, err := t.elem.staticSize()
		if err != nil {
			return "", err
		}
		expected := elemSize * t.length
		if len(chunk) != expected {
			return "", fmt.Errorf("array chunk size %d does not match expected %d", len(chunk), expected)
		}
		items := make([]string, t.length)
		for i := 0; i < t.length; i++ {
			start := i * elemSize
			value, err := decodeStaticBytes(*t.elem, chunk[start:start+elemSize])
			if err != nil {
				return "", err
			}
			items[i] = value
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	default:
		return "", fmt.Errorf("unsupported static type %q", t.raw)
	}
}

func decodeDynamicValue(t abiType, data []byte, offset int) (string, error) {
	switch t.kind {
	case abiBytes:
		return decodeDynamicBytes(data, offset)
	case abiString:
		return decodeDynamicString(data, offset)
	case abiArray:
		if t.elem == nil {
			return "", fmt.Errorf("array missing element type")
		}
		return decodeDynamicArray(t, data, offset)
	default:
		return "", fmt.Errorf("type %q is not dynamic", t.raw)
	}
}

func decodeDynamicBytes(data []byte, offset int) (string, error) {
	if offset+32 > len(data) {
		return "", fmt.Errorf("dynamic bytes length out of bounds")
	}

	length, err := wordToInt(data[offset : offset+32])
	if err != nil {
		return "", err
	}

	start := offset + 32
	if length == 0 {
		return "0x", nil
	}

	if start+length > len(data) {
		return "", fmt.Errorf("dynamic bytes truncated: need %d bytes", length)
	}

	return fmt.Sprintf("0x%s", hex.EncodeToString(data[start:start+length])), nil
}

func decodeDynamicString(data []byte, offset int) (string, error) {
	if offset+32 > len(data) {
		return "", fmt.Errorf("string length out of bounds")
	}

	length, err := wordToInt(data[offset : offset+32])
	if err != nil {
		return "", err
	}

	start := offset + 32
	if start+length > len(data) {
		return "", fmt.Errorf("string bytes truncated")
	}

	value := data[start : start+length]
	if utf8.Valid(value) {
		return strconv.Quote(string(value)), nil
	}
	return fmt.Sprintf("0x%s", hex.EncodeToString(value)), nil
}

func decodeDynamicArray(t abiType, data []byte, offset int) (string, error) {
	if offset+32 > len(data) {
		return "", fmt.Errorf("array length out of bounds")
	}

	length, err := wordToInt(data[offset : offset+32])
	if err != nil {
		return "", err
	}

	start := offset + 32
	if length == 0 {
		return "[]", nil
	}

	values := make([]string, length)
	if t.elem.isDynamic() {
		headSize := 32 * length
		if start+headSize > len(data) {
			return "", fmt.Errorf("array head truncated")
		}

		offsets := make([]int, length)
		for i := 0; i < length; i++ {
			word := data[start+i*32 : start+(i+1)*32]
			ptr, err := wordToInt(word)
			if err != nil {
				return "", fmt.Errorf("invalid offset for element %d: %w", i, err)
			}
			offsets[i] = ptr
		}

		for i := 0; i < length; i++ {
			elemOffset := start + offsets[i]
			if elemOffset < start || elemOffset > len(data) {
				return "", fmt.Errorf("element %d offset outside payload", i)
			}
			value, err := decodeDynamicValue(*t.elem, data, elemOffset)
			if err != nil {
				return "", err
			}
			values[i] = value
		}
	} else {
		elemSize, err := t.elem.staticSize()
		if err != nil {
			return "", err
		}
		total := elemSize * length
		if start+total > len(data) {
			return "", fmt.Errorf("array data truncated")
		}
		for i := 0; i < length; i++ {
			elemStart := start + i*elemSize
			val, err := decodeStaticBytes(*t.elem, data[elemStart:elemStart+elemSize])
			if err != nil {
				return "", err
			}
			values[i] = val
		}
	}

	return "[" + strings.Join(values, ", ") + "]", nil
}

func decodeStaticTopicValue(t abiType, topic []byte) (string, error) {
	if len(topic) != 32 {
		return "", fmt.Errorf("topic length %d", len(topic))
	}
	return decodeStaticBytes(t, topic)
}

func decodeUint(word []byte) string {
	val := new(big.Int).SetBytes(word)
	return val.String()
}

func decodeInt(word []byte) string {
	val := new(big.Int).SetBytes(word)
	if len(word) > 0 && word[0]&0x80 != 0 {
		max := new(big.Int).Lsh(big.NewInt(1), uint(len(word))*8)
		val.Sub(val, max)
	}
	return val.String()
}

func wordToInt(word []byte) (int, error) {
	if len(word) != 32 {
		return 0, fmt.Errorf("word length %d", len(word))
	}
	val := new(big.Int).SetBytes(word)
	if val.Sign() < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if val.BitLen() > 31 {
		if !val.IsInt64() {
			return 0, fmt.Errorf("value exceeds signed 64-bit range")
		}
	}
	if val.Cmp(big.NewInt(math.MaxInt)) > 0 {
		return 0, fmt.Errorf("value %s exceeds limits", val.String())
	}
	return int(val.Int64()), nil
}

func parameterLabel(param eventParameter, idx int) string {
	if param.Name != "" {
		return param.Name
	}
	return fmt.Sprintf("arg%d", idx)
}

func appendWarning(existing, additional string) string {
	if existing == "" {
		return additional
	}
	if additional == "" {
		return existing
	}
	return existing + "; " + additional
}

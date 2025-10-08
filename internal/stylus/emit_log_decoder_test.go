package stylus

import (
	"encoding/hex"
	"math/big"
	"testing"
)

func TestDecodeEventParameters_StaticTypes(t *testing.T) {
	fromAddr := bytesRepeat([]byte{0xde, 0xad}, 10)
	toAddr := bytesRepeat([]byte{0xca, 0xfe}, 10)

	sig := eventSignature{
		Name: "Transfer",
		Params: []eventParameter{
			{Type: "address", Name: "from", Indexed: true},
			{Type: "address", Name: "to", Indexed: true},
			{Type: "uint256", Name: "value"},
		},
	}

	topics := [][]byte{
		padAddress(fromAddr),
		padAddress(toAddr),
	}

	data := make([]byte, 32)
	big.NewInt(12345).FillBytes(data)

	values, warnings := decodeEventParameters(sig, topics, data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	if got, want := values[0].value, "0x"+hex.EncodeToString(fromAddr); got != want {
		t.Fatalf("from value = %s, want %s", got, want)
	}
	if got, want := values[1].value, "0x"+hex.EncodeToString(toAddr); got != want {
		t.Fatalf("to value = %s, want %s", got, want)
	}
	if got, want := values[2].value, "12345"; got != want {
		t.Fatalf("amount value = %s, want %s", got, want)
	}
}

func TestDecodeEventParameters_DynamicBytes(t *testing.T) {
	sig := eventSignature{
		Name: "Data",
		Params: []eventParameter{
			{Type: "bytes", Name: "blob"},
		},
	}

	value := []byte{0x01, 0x02, 0x03}
	data := make([]byte, 96)
	copy(data[0:32], word(big.NewInt(32)))
	copy(data[32:64], word(big.NewInt(int64(len(value)))))
	copy(data[64:], padRight(value, 32))

	values, warnings := decodeEventParameters(sig, nil, data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got, want := values[0].value, "0x010203"; got != want {
		t.Fatalf("blob value = %s, want %s", got, want)
	}
}

func TestDecodeEventParameters_DynamicArray(t *testing.T) {
	sig := eventSignature{
		Name: "Values",
		Params: []eventParameter{
			{Type: "uint256[]", Name: "values"},
		},
	}

	data := make([]byte, 32*5)
	copy(data[0:32], word(big.NewInt(32)))
	copy(data[32:64], word(big.NewInt(2)))
	copy(data[64:96], word(big.NewInt(1)))
	copy(data[96:128], word(big.NewInt(2)))

	values, warnings := decodeEventParameters(sig, nil, data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got, want := values[0].value, "[1, 2]"; got != want {
		t.Fatalf("values = %s, want %s", got, want)
	}
}

func TestDecodeEventParameters_IndexedDynamic(t *testing.T) {
	sig := eventSignature{
		Name: "Message",
		Params: []eventParameter{
			{Type: "bytes", Name: "payload", Indexed: true},
			{Type: "uint256", Name: "id"},
		},
	}

	topic := word(big.NewInt(0x1234))
	data := word(big.NewInt(99))

	values, warnings := decodeEventParameters(sig, [][]byte{topic}, data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	if got, want := values[0].value, "keccak256=0x"+hex.EncodeToString(topic); got != want {
		t.Fatalf("payload value = %s, want %s", got, want)
	}
	if values[0].warning == "" {
		t.Fatalf("expected warning for dynamic indexed parameter")
	}
	if got, want := values[1].value, "99"; got != want {
		t.Fatalf("id value = %s, want %s", got, want)
	}
}

func TestDecodeEventParameters_TupleStatic(t *testing.T) {
	sig := eventSignature{
		Name: "Pair",
		Params: []eventParameter{
			{Type: "(uint256,bool)", Name: "pair"},
		},
	}

	data := make([]byte, 64)
	copy(data[0:32], word(big.NewInt(42)))
	boolWord := make([]byte, 32)
	boolWord[31] = 1
	copy(data[32:64], boolWord)

	values, warnings := decodeEventParameters(sig, nil, data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got, want := values[0].value, "(42, true)"; got != want {
		t.Fatalf("pair value = %s, want %s", got, want)
	}
}

func TestDecodeEventParameters_TupleWithDynamic(t *testing.T) {
	sig := eventSignature{
		Name: "Details",
		Params: []eventParameter{
			{Type: "(address,string)", Name: "details"},
		},
	}

	addr := bytesRepeat([]byte{0x11}, 20)
	message := []byte("hello")

	data := make([]byte, 32*5)
	copy(data[0:32], word(big.NewInt(32)))
	copy(data[32:64], padAddress(addr))
	copy(data[64:96], word(big.NewInt(64)))
	copy(data[96:128], word(big.NewInt(int64(len(message)))))
	copy(data[128:], padRight(message, 32))

	values, warnings := decodeEventParameters(sig, nil, data)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	wantAddr := "0x" + hex.EncodeToString(addr)
	if got, want := values[0].value, "("+wantAddr+", \"hello\")"; got != want {
		t.Fatalf("details value = %s, want %s", got, want)
	}
}

func padAddress(addr []byte) []byte {
	word := make([]byte, 32)
	copy(word[32-len(addr):], addr)
	return word
}

func word(i *big.Int) []byte {
	out := make([]byte, 32)
	i.FillBytes(out)
	return out
}

func bytesRepeat(pattern []byte, count int) []byte {
	out := make([]byte, len(pattern)*count)
	for i := 0; i < count; i++ {
		copy(out[i*len(pattern):], pattern)
	}
	return out
}

func padRight(src []byte, size int) []byte {
	out := make([]byte, size)
	copy(out, src)
	return out
}

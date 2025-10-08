package stylus

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEventSignatureBasicIndexed(t *testing.T) {
	got, err := parseEventSignature("event Transfer(address indexed from, address indexed to, uint256 value);")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "Transfer",
		Params: []eventParameter{
			{Type: "address", Name: "from", Indexed: true},
			{Type: "address", Name: "to", Indexed: true},
			{Type: "uint256", Name: "value"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureTupleParam(t *testing.T) {
	got, err := parseEventSignature("event Complex(tuple(uint256,string) indexed meta, uint8 flag);")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "Complex",
		Params: []eventParameter{
			{Type: "tuple(uint256,string)", Name: "meta", Indexed: true},
			{Type: "uint8", Name: "flag"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureIndexedWithoutName(t *testing.T) {
	got, err := parseEventSignature("event Anonymous(uint256 indexed);")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "Anonymous",
		Params: []eventParameter{
			{Type: "uint256", Indexed: true},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureNoParams(t *testing.T) {
	got, err := parseEventSignature("event Ping();")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name:   "Ping",
		Params: []eventParameter{},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureTrimsWhitespace(t *testing.T) {
	got, err := parseEventSignature("   event   EventWithSpaces ( address  a , uint256  b )  ;   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "EventWithSpaces",
		Params: []eventParameter{
			{Type: "address", Name: "a"},
			{Type: "uint256", Name: "b"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureArrayTypes(t *testing.T) {
	got, err := parseEventSignature("event WithArrays(address[] indexed owners, uint256[3] balances, bytes32[][] data);")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "WithArrays",
		Params: []eventParameter{
			{Type: "address[]", Name: "owners", Indexed: true},
			{Type: "uint256[3]", Name: "balances"},
			{Type: "bytes32[][]", Name: "data"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignaturePayableAddress(t *testing.T) {
	got, err := parseEventSignature("event WithPayable(address payable indexed recipient, address payable sender);")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "WithPayable",
		Params: []eventParameter{
			{Type: "address payable", Name: "recipient", Indexed: true},
			{Type: "address payable", Name: "sender"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureMissingParentheses(t *testing.T) {
	assertEventSignatureError(t, "event BrokenEvent;", "invalid event signature")
}

func TestParseEventSignatureUnexpectedTokens(t *testing.T) {
	assertEventSignatureError(t, "event Bad(uint256 value extra);", "unexpected tokens")
}

func TestParseEventSignatureUnbalancedTuple(t *testing.T) {
	assertEventSignatureError(t, "event Bad(tuple(uint256,string) value;", "unbalanced parentheses")
}

func assertEventSignatureEqual(t *testing.T, got, want eventSignature) {
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseEventSignature() = %#v, want %#v", got, want)
	}
}

func assertEventSignatureError(t *testing.T, signature, substr string) {
	_, err := parseEventSignature(signature)
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %v", substr, err)
	}
}

func TestParseEventSignatureNoPrefix(t *testing.T) {
	got, err := parseEventSignature("Transfer(address indexed from, address indexed to, uint256 value);")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "Transfer",
		Params: []eventParameter{
			{Type: "address", Name: "from", Indexed: true},
			{Type: "address", Name: "to", Indexed: true},
			{Type: "uint256", Name: "value"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureNoSemicolon(t *testing.T) {
	got, err := parseEventSignature("event Transfer(address indexed from, address indexed to, uint256 value)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "Transfer",
		Params: []eventParameter{
			{Type: "address", Name: "from", Indexed: true},
			{Type: "address", Name: "to", Indexed: true},
			{Type: "uint256", Name: "value"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureNoPrefixOrSemicolon(t *testing.T) {
	got, err := parseEventSignature("Transfer(address indexed from, address indexed to, uint256 value)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "Transfer",
		Params: []eventParameter{
			{Type: "address", Name: "from", Indexed: true},
			{Type: "address", Name: "to", Indexed: true},
			{Type: "uint256", Name: "value"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

func TestParseEventSignatureBareEventKeyword(t *testing.T) {
	assertEventSignatureError(t, "event", "missing identifier and parameters")
}

func TestParseEventSignatureNestedTupleArray(t *testing.T) {
	sig := "\treplaceDeposit((bytes4,bytes2,bytes,bytes,bytes,bytes4)[],(bytes,uint256,uint256),uint256,bytes32)"
	got, err := parseEventSignature(sig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := eventSignature{
		Name: "replaceDeposit",
		Params: []eventParameter{
			{Type: "(bytes4,bytes2,bytes,bytes,bytes,bytes4)[]"},
			{Type: "(bytes,uint256,uint256)"},
			{Type: "uint256"},
			{Type: "bytes32"},
		},
	}

	assertEventSignatureEqual(t, got, want)
}

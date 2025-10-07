package stylus

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type evmEvent struct {
	name     string
	startInk uint64
	endInk   uint64
	args     []byte
	outs     []byte
}

func (e *evmEvent) UnmarshalJSON(data []byte) error {
	var obj map[string]interface{}
	json.Unmarshal(data, &obj)

	var ok bool
	e.name, ok = obj["name"].(string)
	if !ok {
		return fmt.Errorf("name is not string")
	}

	num, ok := obj["startInk"].(float64)
	if !ok {
		return fmt.Errorf("startInk is not a number")
	}

	e.startInk = uint64(num)

	num, ok = obj["endInk"].(float64)
	if !ok {
		return fmt.Errorf("endInk is not float64")
	}
	e.endInk = uint64(num)

	argString, ok := obj["args"].(string)
	if !ok || !strings.HasPrefix(argString, "0x") {
		return fmt.Errorf("args is not string, starting with 0x")
	}

	var err error
	e.args, err = hex.DecodeString(argString[2:])
	if err != nil {
		return fmt.Errorf("args is invalid hex string: %v", err)
	}

	outString, ok := obj["outs"].(string)
	if !ok || !strings.HasPrefix(outString, "0x") {
		return fmt.Errorf("outs is not string, starting with 0x")
	}

	e.outs, err = hex.DecodeString(outString[2:])
	if err != nil {
		return fmt.Errorf("outs is invalid hex string: %v", err)
	}

	return nil
}

type StylusState struct {
	txHash      string
	address     string
	blockNumber string
	blockHash   string
	txIndex     string

	evmEvents    []evmEvent
	currentEvent int

	storageSlots map[string][]byte
}

func (st *StylusState) nextEvent(event string) (evmEvent, error) {
	fmt.Printf("Current event requested: %v (%v/%v)\n", event, st.currentEvent+1, len(st.evmEvents))
	// TODO: maybe validate arguments?
	if st.currentEvent >= len(st.evmEvents) {
		return evmEvent{}, fmt.Errorf("no next stylus event")
	}

	res := st.evmEvents[st.currentEvent]
	if res.name != event {
		return evmEvent{}, fmt.Errorf("mismatched event types: expected %v but found %v", event, res.name)
	}

	st.currentEvent++

	return res, nil
}

func (st *StylusState) GetEntrypointArg() (uint64, error) {
	event, err := st.nextEvent("user_entrypoint")
	if err != nil {
		return 0, err
	}

	arg, err := byteArrToU32(event.args)
	return uint64(arg), err
}

func (st *StylusState) GetReturnedValue() (uint64, error) {
	event, err := st.nextEvent("user_returned")
	if err != nil {
		return 0, err
	}

	res, err := byteArrToU32(event.outs)
	return uint64(res), err
}

func byteArrToU32(arr []byte) (uint32, error) {
	if len(arr) != 4 {
		return 0, fmt.Errorf("not bytes of u32")
	}

	result := uint32(0)
	for _, byte := range arr {
		result <<= 8
		result += uint32(byte)
	}

	return result, nil
}

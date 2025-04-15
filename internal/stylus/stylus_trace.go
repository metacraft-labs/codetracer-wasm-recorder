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

type stylusTrace struct {
	events  []evmEvent
	current int
}

func (st *stylusTrace) nextEvent(event string) (evmEvent, error) {
	// TODO: maybe validate arguments?
	if st.current+1 >= len(st.events) {
		return evmEvent{}, fmt.Errorf("no next stylus event")
	}
	// TODO: validate if event types match

	res := st.events[st.current]

	st.current++

	return res, nil
}

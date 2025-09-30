package stylus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero"
)

func Instantiate(ctx context.Context, r wazero.Runtime, stylusTracePath string, stylusSignatureMapPath string, record *trace_record.TraceRecord) (*StylusTrace, error) {
	stylusState := StylusTrace{}

	stylusTraceJson, err := os.ReadFile(stylusTracePath)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(stylusTraceJson, &stylusState.events); err != nil {
		return nil, err
	}

	stylusSignatureMapJson, err := os.ReadFile(stylusSignatureMapPath)
	if err != nil {
		fmt.Printf("Can't read signature map. Error: %v\n", err)
	}

	if err := json.Unmarshal(stylusSignatureMapJson, &stylusState.eventSignatureMap); err != nil {
		fmt.Printf("Can't parse signature map. Error: %v\n", err)
	}

	moduleBuilder := r.NewHostModuleBuilder("vm_hooks")
	moduleBuilder = exportSylusFunctions(moduleBuilder, &stylusState, record)

	if _, err := moduleBuilder.Instantiate(ctx); err != nil {
		return nil, err
	}

	return &stylusState, nil
}

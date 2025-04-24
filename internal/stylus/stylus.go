package stylus

import (
	"context"
	"encoding/json"
	"os"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero"
)

func Instantiate(ctx context.Context, r wazero.Runtime, stylusTracePath string, record *trace_record.TraceRecord) (*StylusTrace, error) {
	stylusTraceJson, err := os.ReadFile(stylusTracePath)
	if err != nil {
		return nil, err
	}

	stylusState := StylusTrace{}

	if err := json.Unmarshal(stylusTraceJson, &stylusState.events); err != nil {
		return nil, err
	}

	moduleBuilder := r.NewHostModuleBuilder("vm_hooks")
	moduleBuilder = exportSylusFunctions(moduleBuilder, &stylusState, record)

	if _, err := moduleBuilder.Instantiate(ctx); err != nil {
		return nil, err
	}

	return &stylusState, nil
}

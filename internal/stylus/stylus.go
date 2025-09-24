package stylus

import (
	"context"
	"encoding/json"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero"
)

func (client *RpcClient) requestDebugTraceTransaction(txHash string) ([]evmEvent, error) {
	rawJson, err := client.Request(
		"debug_traceTransaction",
		[]interface{}{
			txHash,
			struct {
				Tracer string `json:"tracer"`
			}{
				Tracer: "stylusTracer",
			},
		},
	)

	if err != nil {
		return nil, err
	}

	var res []evmEvent
	json.Unmarshal(rawJson, &res)

	return res, nil
}

func Instantiate(ctx context.Context, r wazero.Runtime, rpcUrl string, txHash string, record *trace_record.TraceRecord) (*StylusTrace, error) {
	rpcClient := NewRpcClient(rpcUrl)

	stylusState := StylusTrace{}
	var err error

	stylusState.events, err = rpcClient.requestDebugTraceTransaction(txHash)
	if err != nil {
		return nil, err
	}

	moduleBuilder := r.NewHostModuleBuilder("vm_hooks")
	moduleBuilder = exportSylusFunctions(moduleBuilder, &stylusState, record)

	if _, err := moduleBuilder.Instantiate(ctx); err != nil {
		return nil, err
	}

	return &stylusState, nil
}

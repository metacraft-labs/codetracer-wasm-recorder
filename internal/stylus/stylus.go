package stylus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero"
)

func Instantiate(ctx context.Context, r wazero.Runtime, rpcUrl string, txHash string, record *trace_record.TraceRecord) (result *StylusState, err error) {
	rpcClient := NewRpcClient(rpcUrl)

	result = &StylusState{}

	result.txHash = txHash

	reciept, err := rpcClient.requestTxReciept(txHash)
	if err != nil {
		return
	}

	if reciept.To == nil {
		err = fmt.Errorf("the \"to\" of the transaction is null")
		return
	}

	result.address = *reciept.To
	result.blockNumber = reciept.BlockNumber
	result.blockHash = reciept.BlockHash
	result.txIndex = reciept.TransactionIndex

	result.evmEvents, err = rpcClient.requestDebugTraceTransaction(txHash)
	if err != nil {
		return
	}

	_, _ = rpcClient.requestAlteredStorageInBlockBeforeTransaction(result.blockHash, result.txIndex, result.address)

	moduleBuilder := r.NewHostModuleBuilder("vm_hooks")
	moduleBuilder = exportSylusFunctions(moduleBuilder, result, record)

	if _, err := moduleBuilder.Instantiate(ctx); err != nil {
		return nil, err
	}

	return
}

type TxReceipt struct {
	BlockNumber      string  `json:"blockNumber"`
	BlockHash        string  `json:"blockHash"`
	To               *string `json:"to"`
	TransactionIndex string  `json:"transactionIndex"`
}

func (client *RpcClient) requestTxReciept(txHash string) (result TxReceipt, err error) {
	rawTx, err := client.Request("eth_getTransactionReceipt", []interface{}{txHash})
	if err != nil {
		return
	}

	if err = json.Unmarshal(rawTx, &result); err != nil {
		return
	}

	return
}

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

func (client *RpcClient) requestAlteredStorageInBlockBeforeTransaction(blockHash string, txIndex string, address string) (interface{}, error) {
	fmt.Printf("%#v\n", []interface{}{
		blockHash,
		1,
		address,
		"0x0000000000000000000000000000000000000000000000000000000000000000",
		4096, // TODO: discuss if this is enough
	})
	rawJson, err := client.Request(
		"debug_storageRangeAt",
		[]interface{}{
			blockHash,
			1,
			address,
			"0x0000000000000000000000000000000000000000000000000000000000000000",
			4096, // TODO: discuss if this is enough
		},
	)

	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
		return nil, err
	}

	tmp, err := json.Marshal(rawJson)

	fmt.Printf("OK: %s\n", string(tmp))

	if err != nil {
		return nil, err
	}

	return rawJson, nil
}

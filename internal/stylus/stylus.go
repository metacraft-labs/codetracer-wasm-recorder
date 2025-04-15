package stylus

import (
	"context"
	"encoding/json"
	"os"

	"github.com/tetratelabs/wazero"
)

func Instantiate(ctx context.Context, r wazero.Runtime, stylusTracePath string) (string, error) {
	stylusTraceJson, err := os.ReadFile(stylusTracePath)
	if err != nil {
		return "", err
	}

	stylusState := stylusTrace{}

	if err := json.Unmarshal(stylusTraceJson, &stylusState.events); err != nil {
		return "", err
	}

	moduleBuilder := r.NewHostModuleBuilder("vm_hooks")
	moduleBuilder = exportSylusFunctions(moduleBuilder, &stylusState)

	if _, err := moduleBuilder.Instantiate(ctx); err != nil {
		return "", err
	}

	event, err := stylusState.nextEvent("user_entrypoint")
	if err != nil {
		return "", err
	}

	return string(event.args), nil
}

// Tests for the special-event kind vocabulary. No mocks: the assertions
// compare the Go constants with the ordinals and names the trace format
// specification assigns (codetracer-trace-format-spec trace-events.md
// §"EventLogKind (u8 enum)").
package tracetypes

import (
	"strconv"
	"testing"
)

// specEventLogKinds lists every assigned EventLogKind in ordinal order.
var specEventLogKinds = []struct {
	kind RecordEventKind
	name string
}{
	{EventKindWrite, "Write"},
	{EventKindWriteFile, "WriteFile"},
	{EventKindWriteOther, "WriteOther"},
	{EventKindRead, "Read"},
	{EventKindReadFile, "ReadFile"},
	{EventKindReadOther, "ReadOther"},
	{EventKindReadDir, "ReadDir"},
	{EventKindOpenDir, "OpenDir"},
	{EventKindCloseDir, "CloseDir"},
	{EventKindSocket, "Socket"},
	{EventKindOpen, "Open"},
	{EventKindError, "Error"},
	{EventKindTraceLogEvent, "TraceLogEvent"},
	{EventKindEvmEvent, "EvmEvent"},
	{EventKindWriteStderr, "WriteStderr"},
	{EventKindExec, "Exec"},
	{EventKindReadEnv, "ReadEnv"},
	{EventKindWriteEnv, "WriteEnv"},
	{EventKindStat, "Stat"},
	{EventKindCreateDir, "CreateDir"},
	{EventKindRemoveDir, "RemoveDir"},
	{EventKindRemoveFile, "RemoveFile"},
	{EventKindRename, "Rename"},
	{EventKindChangeDir, "ChangeDir"},
}

func TestRecordEventKindsAreTheTraceFormatKinds(t *testing.T) {
	if len(AllRecordEventKinds) != len(specEventLogKinds) {
		t.Fatalf("AllRecordEventKinds has %d kinds; the trace format assigns %d",
			len(AllRecordEventKinds), len(specEventLogKinds))
	}
	for ordinal, want := range specEventLogKinds {
		if int(want.kind) != ordinal {
			t.Errorf("%s has ordinal %d; the trace format assigns %d",
				want.name, int(want.kind), ordinal)
		}
		if AllRecordEventKinds[ordinal] != want.kind {
			t.Errorf("AllRecordEventKinds[%d] = %d; want %s",
				ordinal, AllRecordEventKinds[ordinal], want.name)
		}
		if !want.kind.Valid() {
			t.Errorf("%s (%d) is not reported valid", want.name, ordinal)
		}
		if got := want.kind.String(); got != want.name {
			t.Errorf("kind %d is named %q; want %q", ordinal, got, want.name)
		}
	}
}

func TestUnassignedRecordEventKindsAreInvalid(t *testing.T) {
	for _, v := range []int{-1, 24, 25, 255, 256} {
		k := RecordEventKind(v)
		if k.Valid() {
			t.Errorf("kind %d is reported valid; only 0-23 are assigned", v)
		}
		if got, want := k.String(), "RecordEventKind(unassigned "+strconv.Itoa(v)+")"; got != want {
			t.Errorf("kind %d is named %q; want %q", v, got, want)
		}
	}
}

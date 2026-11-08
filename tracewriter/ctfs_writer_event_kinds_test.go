//go:build cgo

// Tests that the CTFS writer stores each special event under its exact
// EventLogKind and refuses a kind the trace format does not assign.
//
// No mocks: each test drives the real Nim FFI writer into a `.ct` container
// on disk and reads it back with `ct-print` from the sibling
// codetracer-trace-format-nim checkout.
package tracewriter

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/tracetypes"
)

func ctPrintPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(wd), "..", "codetracer-trace-format-nim", "ct-print")
}

// writeTraceWithEvents records a one-step trace carrying one special event
// of each kind given, and returns the error ProduceTrace reported and the
// trace directory.
func writeTraceWithEvents(t *testing.T, kinds []tracetypes.RecordEventKind) (string, error) {
	t.Helper()
	w := NewCtfsTraceWriter()
	w.RegisterStep("/src/main.wat", 1)
	for _, k := range kinds {
		w.RegisterRecordEvent(k, "", k.String()+" content")
	}
	dir := t.TempDir()
	return dir, w.ProduceTrace(dir, "kinds", "/src")
}

func TestCtfsWriterStoresEveryEventKindExactly(t *testing.T) {
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Skipf("SKIP: ct-print not found at %s — only available within the "+
			"metacraft workspace where codetracer-trace-format-nim is a sibling.",
			ctPrint)
	}

	dir, err := writeTraceWithEvents(t, tracetypes.AllRecordEventKinds)
	if err != nil {
		t.Fatalf("ProduceTrace: %v", err)
	}
	cts, _ := filepath.Glob(filepath.Join(dir, "*.ct"))
	if len(cts) != 1 {
		t.Fatalf("expected one .ct under %s, found %v", dir, cts)
	}
	out, err := exec.Command(ctPrint, "--full", cts[0]).CombinedOutput()
	if err != nil {
		t.Fatalf("ct-print --full: %v\n%s", err, out)
	}
	var doc struct {
		Events []struct {
			Kind   string `json:"kind"`
			IoKind string `json:"io_kind"`
			Text   string `json:"text"`
		} `json:"events"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("ct-print --full emitted invalid JSON: %v\n%s", err, out)
	}
	var got []string
	for _, ev := range doc.Events {
		if ev.Kind != "io" {
			continue
		}
		got = append(got, ev.IoKind)
		if want := ev.IoKind + " content"; ev.Text != want {
			t.Errorf("io event %s carries %q; want %q", ev.IoKind, ev.Text, want)
		}
	}
	if len(got) != len(tracetypes.AllRecordEventKinds) {
		t.Fatalf("read back %d io events %v; wrote %d",
			len(got), got, len(tracetypes.AllRecordEventKinds))
	}
	for i, k := range tracetypes.AllRecordEventKinds {
		if got[i] != k.String() {
			t.Errorf("io event %d reads back as %q; it was written as %s (%d)",
				i, got[i], k, int(k))
		}
	}
}

func TestCtfsWriterRefusesAnUnassignedEventKind(t *testing.T) {
	for _, v := range []int{24, 255} {
		dir, err := writeTraceWithEvents(t, []tracetypes.RecordEventKind{
			tracetypes.EventKindWrite, tracetypes.RecordEventKind(v),
		})
		if err == nil {
			t.Fatalf("ProduceTrace accepted event kind %d", v)
		}
		want := "special event kind " + strconv.Itoa(v) + " is not an assigned EventLogKind"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ProduceTrace refused kind %d with %q; want it to say %q",
				v, err.Error(), want)
		}
		if cts, _ := filepath.Glob(filepath.Join(dir, "*.ct")); len(cts) != 0 {
			t.Errorf("a refused recording still left %v", cts)
		}
	}
}

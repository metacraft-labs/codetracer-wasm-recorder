package maintester

import "testing"

const dwarfWarning = "Error constructing DWARF data. Tracing will not work: decoding dwarf section info at offset 0x0: too short\n"

func TestStripKnownDWARFWarnings(t *testing.T) {
	t.Run("removes exact warning", func(t *testing.T) {
		if got := StripKnownDWARFWarnings(dwarfWarning); got != "" {
			t.Fatalf("expected warning to be stripped, but got %q", got)
		}
	})

	t.Run("removes repeated warning", func(t *testing.T) {
		input := dwarfWarning + "some output\n" + dwarfWarning
		expected := "some output\n"
		if got := StripKnownDWARFWarnings(input); got != expected {
			t.Fatalf("unexpected sanitized output, expected %q, but got %q", expected, got)
		}
	})

	t.Run("leaves other stderr intact", func(t *testing.T) {
		input := "info: hello\n"
		if got := StripKnownDWARFWarnings(input); got != input {
			t.Fatalf("expected non-warning stderr to remain, but got %q", got)
		}
	})
}

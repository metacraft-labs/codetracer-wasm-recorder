package maintester

import "strings"

const knownDWARFWarning = "Error constructing DWARF data. Tracing will not work: decoding dwarf section info at offset 0x0: too short\n"

// StripKnownDWARFWarnings removes occurrences of the known DWARF tracing warning
// emitted by minimal test fixtures when debug sections are absent. This allows
// example tests to assert on meaningful stderr content without being brittle.
func StripKnownDWARFWarnings(stderr string) string {
	return strings.ReplaceAll(stderr, knownDWARFWarning, "")
}

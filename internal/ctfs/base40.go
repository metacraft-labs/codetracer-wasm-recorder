package ctfs

import "fmt"

// base40Alphabet is the CTFS filename alphabet
// (`codetracer-trace-format-spec/ctfs-container.md` §3, mirrored in
// `codetracer-specs/Trace-Files/CTFS-Binary-Format.md` §3):
//
//	0        NUL (padding)
//	1..10    '0'..'9'
//	11..36   'a'..'z'
//	37       '.'
//	38       '/'
//	39       '-'
const base40Alphabet = "\x000123456789abcdefghijklmnopqrstuvwxyz./-"

// MaxNameLen is the longest CTFS internal filename: 40^12 < 2^64 < 40^13, so
// exactly twelve characters pack into the u64 `FileEntry.Name`.
//
// This is a hard constraint on every namespace name, and it is the reason the
// snapshot streams are spelled `wcpentry.lay` / `wcpentry.mem` rather than the
// `wcp.entry.lay` / `wcp.entry.mem` of
// `WASM-Replay-Snapshots-And-Slices.md` §6 — see `internal/wasmsnapshot`'s
// `NamespaceNames` for the full note.
const MaxNameLen = 12

// EncodeName packs a CTFS internal filename into the u64 `FileEntry.Name`
// field: `encoded = c[0]*40^0 + c[1]*40^1 + ... + c[11]*40^11`.
func EncodeName(name string) (uint64, error) {
	if len(name) == 0 {
		return 0, fmt.Errorf("ctfs: empty internal filename")
	}
	if len(name) > MaxNameLen {
		return 0, fmt.Errorf(
			"ctfs: internal filename %q is %d characters; base40 packs at most %d "+
				"into the u64 FileEntry.Name field", name, len(name), MaxNameLen)
	}
	var encoded uint64
	var scale uint64 = 1
	for i := 0; i < MaxNameLen; i++ {
		var idx uint64
		if i < len(name) {
			c := name[i]
			pos := -1
			for j := 0; j < len(base40Alphabet); j++ {
				if base40Alphabet[j] == c {
					pos = j
					break
				}
			}
			if pos <= 0 {
				return 0, fmt.Errorf(
					"ctfs: internal filename %q contains %q, which is not in the "+
						"base40 alphabet (digits, lowercase letters, '.', '/', '-')",
					name, string(rune(c)))
			}
			idx = uint64(pos)
		}
		encoded += idx * scale
		if i < MaxNameLen-1 {
			scale *= 40
		}
	}
	return encoded, nil
}

// DecodeName is the inverse of EncodeName. Decoding stops at the first
// padding character, per the reference algorithm in
// `ctfs-container.md` §3.
func DecodeName(encoded uint64) string {
	var out []byte
	for encoded > 0 {
		r := encoded % 40
		encoded /= 40
		if r == 0 {
			break
		}
		out = append(out, base40Alphabet[r])
	}
	return string(out)
}

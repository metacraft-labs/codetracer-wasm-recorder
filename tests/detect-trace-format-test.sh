#!/usr/bin/env bash
# Checks that `scripts/detect-trace-format.sh` links the trace-writer library
# built from the sibling's CURRENT sources:
#
#   1. a library older than any of its sources is rebuilt, not reused;
#   2. a fresh library is reused, not rebuilt;
#   3. with both a `.a` and a `.so` next to each other, the `-L` directory in
#      CGO_LDFLAGS offers the linker the chosen `.a` only (a linker given the
#      sibling directory takes the `.so`);
#   4. a library whose content changes yields a different CGO_LDFLAGS, so Go's
#      build cache, which keys a link on the flags rather than on the
#      libraries they name, relinks.
#
# Mock justification: the sibling is a scratch directory and `nimble` is a stub
# that writes a library file and logs that it ran. What is under test is the
# script's decision of WHETHER to build and WHICH library to hand the linker;
# a real Nim build would add minutes and a network-dependent dev shell without
# exercising any more of that decision. `repro` and `nix` are kept off PATH so
# the script reaches its plain-`nimble` builder. The real build and link are
# covered by `just test`, which links the recorder against the real sibling.
#
# No silent skips: every assertion either holds or fails loudly.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# A scratch workspace: <WORK>/rec/scripts/detect-trace-format.sh next to a
# <WORK>/codetracer-trace-format-nim sibling.
mkdir -p "$WORK/rec/scripts" "$WORK/codetracer-trace-format-nim/src" \
  "$WORK/codetracer-trace-format-nim/include" "$WORK/bin" "$WORK/cache"
cp "$REPO_ROOT/scripts/detect-trace-format.sh" "$WORK/rec/scripts/"
NIM="$WORK/codetracer-trace-format-nim"
echo 'proc f() = discard' >"$NIM/src/codetracer_trace_writer_ffi.nim"
echo '# nimble' >"$NIM/codetracer_trace_format.nimble"

# Stub nimble: writes a library whose content names the build, and logs it.
cat >"$WORK/bin/nimble" <<STUB
#!$(command -v bash)
n=\$(( \$(cat "$WORK/builds" 2>/dev/null || echo 0) + 1 ))
echo "\$n" >"$WORK/builds"
echo "library from build \$n" >libcodetracer_trace_writer.a
STUB
chmod +x "$WORK/bin/nimble"
# Only the tools the script needs; no repro, nix or nix-build.
for tool in bash find head cut cksum mkdir ln basename uname cat dirname; do
  ln -s "$(command -v "$tool")" "$WORK/bin/$tool"
done

builds() { cat "$WORK/builds" 2>/dev/null || echo 0; }

# Source the script in a clean subshell; print the CGO_LDFLAGS it exports.
detect() {
  env -i HOME="$WORK" XDG_CACHE_HOME="$WORK/cache" PATH="$WORK/bin" \
    bash -c "source '$WORK/rec/scripts/detect-trace-format.sh' >/dev/null 2>&1; printf '%s' \"\$CGO_LDFLAGS\""
}

link_dir() { # CGO_LDFLAGS -> the -L directory
  set -- $1
  echo "${1#-L}"
}

# Missing library: built once.
flags1="$(detect)"
[ "$(builds)" = 1 ] || fail "a missing library must be built once (builds: $(builds))"

# Fresh library: reused.
flags2="$(detect)"
[ "$(builds)" = 1 ] || fail "a library newer than its sources must be reused (builds: $(builds))"
[ "$flags1" = "$flags2" ] || fail "an unchanged library must give the same CGO_LDFLAGS ('$flags1' vs '$flags2')"

# A source newer than the library: rebuilt.
touch -d '1 hour ago' "$NIM/libcodetracer_trace_writer.a"
touch "$NIM/src/codetracer_trace_writer_ffi.nim"
flags3="$(detect)"
[ "$(builds)" = 2 ] || fail "a library older than a source must be rebuilt (builds: $(builds))"
grep -qx 'library from build 2' "$NIM/libcodetracer_trace_writer.a" ||
  fail "the rebuilt library is not the one on disk"
[ "$flags3" != "$flags2" ] ||
  fail "a rebuilt library must change CGO_LDFLAGS, or Go's build cache keeps the old link ('$flags3')"

# The same check for the .nimble.
touch -d '1 hour ago' "$NIM/libcodetracer_trace_writer.a"
touch "$NIM/codetracer_trace_format.nimble"
detect >/dev/null
[ "$(builds)" = 3 ] || fail "a library older than the .nimble must be rebuilt (builds: $(builds))"

# A .so next to the .a: the linker must be offered the .a alone.
echo 'stale shared library' >"$NIM/libcodetracer_trace_writer.so"
touch -d '1 hour ago' "$NIM/libcodetracer_trace_writer.so"
flags4="$(detect)"
dir="$(link_dir "$flags4")"
[ "$dir" != "$NIM" ] || fail "CGO_LDFLAGS points the linker at the sibling directory, where it takes the .so"
[ -e "$dir/libcodetracer_trace_writer.a" ] || fail "the -L directory $dir has no libcodetracer_trace_writer.a"
[ ! -e "$dir/libcodetracer_trace_writer.so" ] || fail "the -L directory $dir offers the linker a .so"
cmp -s "$dir/libcodetracer_trace_writer.a" "$NIM/libcodetracer_trace_writer.a" ||
  fail "the -L directory's library is not the sibling's current one"

echo "detect-trace-format: all assertions held"

## Producer of `nim-multilevel.ct`, the fixture `multilevel_layout_test.go`
## reads.
##
## It is written by the canonical CTFS writer (`codetracer-trace-format-nim`'s
## `codetracer_ctfs`), so the Go reader is pinned against the producer rather
## than against itself. The container holds one internal file, `big.dat`, of
## 130 blocks of 1024 bytes: one past the 127 data blocks a level-1 mapping
## block addresses at that block size, so the file needs the level-2 chain.
## The content is `fixturePattern()` in `multilevel_layout_test.go`.
##
## Regenerate from a `codetracer-trace-format-nim` checkout, whose dev shell
## provides Nim and the library's dependencies:
##
##   cd ../codetracer-trace-format-nim
##   direnv exec . nim c -r --hints:off -p:src \
##     --out:"$PWD/build/gen_nim_multilevel" \
##     ../codetracer-wasm-recorder/internal/ctfs/testdata/gen_nim_multilevel.nim \
##     ../codetracer-wasm-recorder/internal/ctfs/testdata/nim-multilevel.ct

import std/os
import results
import codetracer_ctfs

const
  blockSize = 1024'u32
  blockCount = 130
  maxRootEntries = 31'u32

proc fixturePattern(): seq[byte] =
  let n = blockCount * int(blockSize)
  result = newSeq[byte](n)
  for i in 0 ..< n:
    result[i] = byte((i * 7 + i div int(blockSize)) mod 251)

proc main() =
  if paramCount() != 1:
    quit "usage: gen_nim_multilevel <output.ct>"
  var c = createCtfs(blockSize = blockSize, maxRootEntries = maxRootEntries)
  var f = c.addFile("big.dat").expect("addFile big.dat")
  c.writeToFile(f, fixturePattern()).expect("write big.dat")
  c.closeCtfs().expect("close")
  c.writeCtfsToFile(paramStr(1)).expect("write " & paramStr(1))

main()

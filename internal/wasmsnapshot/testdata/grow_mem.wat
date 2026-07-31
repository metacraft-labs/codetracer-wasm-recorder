;; A module whose state is a *growing* linear memory.
;;
;; The committed `balance_calc.wasm` never calls `memory.grow`, so nothing else
;; in this repository exercises the branch of `Snapshot.Restore` that grows a
;; freshly instantiated memory up to a snapshot's recorded size — which
;; `WASM-Replay-Snapshots-And-Slices.md` §4 ("Memory size … so the snapshotter
;; can pre-size") and §5 ("64 KiB … aligns with `memory.grow`") both depend on.
;;
;; `bump(v)` grows the memory by one 64 KiB page and stores `v` at the first
;; byte of the page it just added, so every call changes both the memory size
;; and its contents. `size()` reports the current page count.
;;
;; Rebuild with: wat2wasm grow_mem.wat -o grow_mem.wasm
(module
  (memory (export "memory") 1 8)
  (global $calls (mut i32) (i32.const 0))

  (func (export "bump") (param $v i32) (result i32)
    (local $old i32)
    (local.set $old (memory.grow (i32.const 1)))
    (global.set $calls (i32.add (global.get $calls) (i32.const 1)))
    (i32.store (i32.mul (local.get $old) (i32.const 65536)) (local.get $v))
    (local.get $old))

  (func (export "size") (result i32)
    (memory.size))

  (func (export "calls") (result i32)
    (global.get $calls)))

;; entrypoint.wat — a minimal Stylus contract shape for the `--stylus`
;; replay regression test.
;;
;; It does what every Stylus contract does at its edges: read the calldata
;; the EVM recorded, write a result back, and return a status word.
;;
;; The return value is deliberately DERIVED from the calldata: it is the
;; i32 the `read_args` hook wrote at address 0. `cmd/wazero/wazero.go`
;; compares it against the recorded `user_returned` result, so the recorded
;; 0xefbeadde (the little-endian reading of the recorded 0xdeadbeef
;; calldata) can only match if the hook really replayed the recorded bytes
;; into linear memory. A stub that wrote nothing would return 0 and be
;; caught.
(module
  (import "vm_hooks" "read_args"    (func $read_args    (param i32)))
  (import "vm_hooks" "write_result" (func $write_result (param i32 i32)))

  (memory (export "memory") 1)

  (func (export "user_entrypoint") (param $len i32) (result i32)
    (call $read_args (i32.const 0))
    (call $write_result (i32.const 0) (i32.const 4))
    (i32.load (i32.const 0))))

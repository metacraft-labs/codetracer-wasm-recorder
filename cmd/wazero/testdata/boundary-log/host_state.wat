;; host_state.wat — exercises spec §3.3 (host-supplied initial state) and
;; §3.4 (host mutation during a call).
;;
;; The module DEFINES neither its memory nor its counter global: both are
;; imported, so their contents are part of the recording rather than part
;; of the `.wasm` (spec §3.3).
;;
;;   `sum`       reads the imported memory at address 0 and adds the
;;               imported global — pure §3.3 initial state.
;;   `after_tick` calls the host, then reads address 4 and the global
;;               again — the host wrote both while servicing that call, so
;;               the values it sees are §3.4 mutations.
(module
  (import "env" "memory"  (memory 1))
  (import "env" "counter" (global $counter (mut i32)))
  (import "env" "tick"    (func $tick (param i32) (result i32)))

  (func (export "sum") (result i32)
    (i32.add (i32.load (i32.const 0)) (global.get $counter)))

  (func (export "after_tick") (param $n i32) (result i32)
    (drop (call $tick (local.get $n)))
    (i32.add (i32.load (i32.const 4)) (global.get $counter))))

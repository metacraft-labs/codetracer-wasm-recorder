;; imports_demo.wat — exercises the generic import stubs of `--boundary-log`.
;;
;; `run(x)` calls a host function that returns a value (the non-determinism
;; replay has to feed back, spec §3.2), then a host function that returns
;; nothing, then computes from the result. Replaying it proves the stubs
;; both check the recorded arguments and supply the recorded results.
(module
  (import "env" "host_add"  (func $host_add  (param i32 i32) (result i32)))
  (import "env" "host_note" (func $host_note (param i32)))

  (func (export "run") (param $x i32) (result i32)
    (local $s i32)
    (local.set $s (call $host_add (local.get $x) (i32.const 10)))
    (call $host_note (local.get $s))
    (i32.mul (local.get $s) (i32.const 2))))

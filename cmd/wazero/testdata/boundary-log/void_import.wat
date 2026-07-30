;; void_import.wat — the boundary shape M39 closed.
;;
;; `host_ping` has the signature `() -> ()`. A crossing into it carries no
;; arguments and no results, so a browser recording of it contains no value
;; run; `browser_session.js` emits no `Call` and no `Return` for an import
;; either. Its two realm markers are therefore the ENTIRE record of it, and
;; until M39 spelled the import edge apart from the export edge they could
;; not be attributed — such a call was replayed unchecked (spec §8's
;; "silent degradation", reported on stderr rather than refused).
;;
;; Two exports, because the two things worth checking are different:
;;
;;   `ping_n(n)` makes exactly `n` value-less crossings and nothing else, so
;;              a recording with the wrong NUMBER of them is a divergence
;;              with nothing else in the way.
;;   `run(x)`   interleaves one value-less crossing with one that carries
;;              values, so the value-less crossing's POSITION in the cursor
;;              stream is checked and not only its index.
(module
  (import "env" "host_ping" (func $ping))                              ;; import #0: () -> ()
  (import "env" "host_add"  (func $add (param i32 i32) (result i32)))  ;; import #1

  (func (export "ping_n") (param $n i32) (result i32)
    (local $i i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_s (local.get $i) (local.get $n)))
        (call $ping)
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (local.get $n))

  (func (export "run") (param $x i32) (result i32)
    (call $ping)
    (call $add (local.get $x) (i32.const 100))))

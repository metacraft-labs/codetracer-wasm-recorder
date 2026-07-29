;; hook_imports.wat — a module that still carries the instrumenter's
;; `__codetracer` hook imports, i.e. the INSTRUMENTED module.
;;
;; Replay must refuse it: spec §6.1 requires the original, uninstrumented
;; module, and the extra imports would shift every import index the
;; recording refers to. Used by verify_uninstrumented_module_is_used.
(module
  (import "__codetracer" "__ct_emit_call" (func (param i32 i32)))

  (func (export "compute_balance") (param i32 i32) (result i32)
    (i32.const 620)))

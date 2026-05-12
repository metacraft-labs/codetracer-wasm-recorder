// panic_path.rs — recorder-golden fixture exercising the explicit
// panic path.  Per the recorder-test-requirements policy
// (`metacraft-specs/policies/recorder-test-requirements.md`
// §2 "Universal feature checklist"), every recorder MUST cover
// "raise without handler (program-terminating)".  The recorder
// MUST emit a `RecordEvent::Error` (or equivalent kind) when
// the WASM module traps via panic; the Go test asserts on this.

#[inline(never)]
fn bump(n: i32) -> i32 {
    n + 1
}

fn main() {
    let a = bump(40);
    let b = bump(a);              // 42
    if b == 42 {
        panic!("expected divergence at b=42");
    }
    // Unreachable when b==42; a defensive exit so the type-checker is happy.
    std::process::exit(0);
}

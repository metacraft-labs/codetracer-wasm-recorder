// column_aware.rs — recorder-golden fixture for column-aware step
// emission (FU-Column-Aware-Nav-Wasm).  Mirrors the JS recorder's
// `tests/integration/column-aware.test.ts` "multiple statements on one
// line each record distinct columns" fixture and the EVM
// (`tests/test_column_aware.rs`), Solana
// (`tests/test_column_aware_steps.rs`), and Cairo
// (`tests/test_column_aware.rs`) sibling tests.
//
// Line 16 packs three statements onto one source line so each one
// starts at a distinct 1-based column.  The recorder must surface a
// step event for each statement with a strictly distinct column value
// (acceptance criterion from `codetracer-specs/Planned-Features/
// Column-Aware-Navigation-Other-Languages.plan.md`).

#[inline(never)]
fn three_on_one_line() -> i32 {
    let a: i32 = 1; let b: i32 = 2; let c: i32 = 3;
    a + b + c
}

fn main() {
    let total = three_on_one_line();
    std::process::exit(if total == 6 { 0 } else { 1 });
}

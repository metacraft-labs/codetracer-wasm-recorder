// nested_calls.rs — recorder-golden fixture exercising 3+ deep
// function calls plus recursion.  Each call must produce one
// call_entry / call_exit pair in the trace.  Keep the line numbers
// stable.

#[inline(never)]
fn level3(x: i32) -> i32 {
    x + 1
}

#[inline(never)]
fn level2(x: i32) -> i32 {
    let v = level3(x);
    v * 2
}

#[inline(never)]
fn level1(x: i32) -> i32 {
    let v = level2(x);
    v + 10
}

#[inline(never)]
fn factorial(n: i32) -> i32 {
    if n <= 1 {
        1
    } else {
        n * factorial(n - 1)
    }
}

fn main() {
    let chain = level1(5);          // level1 → level2 → level3 → (5+1)*2+10 = 22
    let fact5 = factorial(5);       // 120
    let total = chain + fact5;      // 142
    std::process::exit(if total == 142 { 0 } else { 1 });
}

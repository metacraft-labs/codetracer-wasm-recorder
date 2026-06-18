// control_flow.rs — recorder-golden fixture exercising control-flow
// constructs that produce observable Step / Call / Function trace
// events.  Keep the line numbers stable: the Go test asserts exact
// source-line ordering.
//
// The program computes: classify(7) = 1, then sum_iter([1,2,3,4]) =
// 10, then nested_loop(4) (sum of triangular numbers 0+1+3+6 = 10),
// finally returns 1 + 10 + 10 = 21.

#[inline(never)]
fn classify(n: i32) -> i32 {
    if n > 0 {
        1
    } else if n < 0 {
        -1
    } else {
        0
    }
}

#[inline(never)]
fn sum_iter(xs: &[i32]) -> i32 {
    let mut total = 0;
    for x in xs.iter() {
        total += *x;
    }
    total
}

#[inline(never)]
fn nested_loop(n: i32) -> i32 {
    let mut total = 0;
    let mut i = 0;
    while i < n {
        let mut j = 0;
        while j <= i {
            total += j;
            j += 1;
        }
        i += 1;
    }
    total
}

fn main() {
    let sign = classify(7);
    let xs: [i32; 4] = [1, 2, 3, 4];
    let sum_val = sum_iter(&xs);
    let nested_val = nested_loop(4);
    let final_result = sign + sum_val + nested_val;
    std::process::exit(if final_result == 21 { 0 } else { 1 });
}

// collections.rs — recorder-golden fixture exercising collection
// types and struct/tuple field access.  The recorder must emit
// step events covering each let-binding; ValueRecord variants
// (Sequence / Struct / Tuple) are checked by the Go test.

use std::collections::HashMap;

#[derive(Debug)]
#[allow(dead_code)]
struct Point {
    x: i32,
    y: i32,
}

#[inline(never)]
fn make_vec() -> Vec<i32> {
    let mut v: Vec<i32> = Vec::new();
    v.push(10);
    v.push(20);
    v.push(30);
    v
}

#[inline(never)]
fn sum_vec(v: &Vec<i32>) -> i32 {
    let mut s = 0;
    for x in v.iter() {
        s += *x;
    }
    s
}

fn main() {
    let v = make_vec();
    let total = sum_vec(&v);              // 60

    let mut map: HashMap<&str, i32> = HashMap::new();
    map.insert("a", 1);
    map.insert("b", 2);
    let map_len = map.len() as i32;       // 2

    let pt = Point { x: 3, y: 4 };
    let pt_sum = pt.x + pt.y;             // 7

    let pair: (i32, i32) = (100, 200);
    let pair_sum = pair.0 + pair.1;       // 300

    let final_result = total + map_len + pt_sum + pair_sum; // 369
    std::process::exit(if final_result == 369 { 0 } else { 1 });
}

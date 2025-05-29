// use std::num::NonZero;

struct TestStruct1 {
    a: i32,
}

fn test_struct1(a: i32) -> TestStruct1 {
    TestStruct1 { a: a }
}

struct TestStruct2 {
    a: i32,
    b: i32,
}

fn test_struct2(a: i32, b: i32) -> TestStruct2 {
    TestStruct2 { a: a, b: b }
}

// fn number() -> usize {
//     1
// }
// fn non_zero() -> NonZero<usize> {
// NonZero::new(2).unwrap()
// }

fn main() {
    let test1 = test_struct1(123);

    let test2 = test_struct2(234, 345);

    let dummy = test_struct1(-1);
}

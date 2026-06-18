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

fn test_struct3(i: usize) -> usize {
    let a = i;
    let b = a;
    b
}

// fn number() -> usize {
//     1
// }
// fn non_zero() -> NonZero<usize> {
// NonZero::new(2).unwrap()
// }

fn main() {
    let test1 = test_struct1(1);
    let test2 = test_struct2(234, 345);

    for i in 0 .. 4 {
        let test3 = test_struct3(i);
        let test4 = test3 + 1;
    }
    let dummy = test_struct1(-1);
}

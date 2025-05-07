struct TestStruct {
    a: i32,
    b: i32,
}

fn test_struct(a: i32, b: i32) -> TestStruct {
    TestStruct { a: a, b: b }
}

fn main() {
    let test = test_struct(123, 234);

    std::process::exit(test.a + test.b)
}

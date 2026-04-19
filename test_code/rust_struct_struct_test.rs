struct A {
    x: i32,
}

struct B {
    y: i32,
}

struct C {
    a: A,
    b: B,
}

fn main() {
    let a = A { x: 1 };
    let b = B { y: 2 };
    let c = C { a, b };

    let d = &c;
}

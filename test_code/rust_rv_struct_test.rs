struct A {
    x: i32,
    y: i32,
}

fn test() -> A {
    A { x: 1, y: 2 }
}
fn main() {
    let a = test();
    let b = test();

    println!("{}", a.x);
}

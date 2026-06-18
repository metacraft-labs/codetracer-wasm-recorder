fn main() {
    a();
    let x = 1;
    a();
    println!("{}", x)
}

#[inline(always)]
fn a() {
    let y = 2;
    b();
}

#[inline(always)]
fn b() {
    println!("YES KING")
}
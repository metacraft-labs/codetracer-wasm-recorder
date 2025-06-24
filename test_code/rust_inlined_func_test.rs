fn main() {
    a();
}

#[inline(always)]
fn a() {
    b();
}

#[inline(always)]
fn b() {
    println!("YES KING")
}

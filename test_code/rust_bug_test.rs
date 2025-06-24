// static arr: [i32; 15] = [15; 15];
fn main() {
    let arr: [i32; 100] = [15; 100];
    let slc = &arr[0..2];

    println!("{:?}", slc);
}

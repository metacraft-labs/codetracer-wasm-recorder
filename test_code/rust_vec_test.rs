struct A {
    x: i32
}

fn main() {
    let vec1 = Vec::from([1,2,3,4]);
    let vec2 = vec![false, true];
    let vec3 = vec![1.1, 2.2, 3.3];
    let vec4 = vec![vec![1, 2], vec![3, 4]];
    let vec5 = vec![A{x: 1}, A{x: 2}, A{x: 3}];
    println!("{} {} {} {} {}", vec1[0], vec2[0], vec3[0], vec4[0][0], vec5[0].x);

}
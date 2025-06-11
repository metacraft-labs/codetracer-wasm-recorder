package interpreter

// Stack is a LIFO stack of any type.
type Stack[T any] struct {
	elems []T
}

// New returns an initialized Stack.
func NewStack[T any]() *Stack[T] {
	return &Stack[T]{}
}

// Push adds v to the top of the stack.
func (s *Stack[T]) Push(v T) {
	s.elems = append(s.elems, v)
}

// Pop removes and returns the top element.
// The bool is false if the stack was empty.
func (s *Stack[T]) Pop() (T, bool) {
	var zero T
	if len(s.elems) == 0 {
		return zero, false
	}
	idx := len(s.elems) - 1
	v := s.elems[idx]
	// avoid memory leak for reference types
	s.elems[idx] = zero
	s.elems = s.elems[:idx]
	return v, true
}

// Peek returns the top element without removing it.
// The bool is false if the stack is empty.
func (s *Stack[T]) Peek() (T, bool) {
	var zero T
	if len(s.elems) == 0 {
		return zero, false
	}
	return s.elems[len(s.elems)-1], true
}

// Len returns the number of elements in the stack.
func (s *Stack[T]) Len() int {
	return len(s.elems)
}

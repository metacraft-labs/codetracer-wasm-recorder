package interpreter

type Stack[T any] struct {
	elems []T
}

func NewStack[T any]() *Stack[T] {
	return &Stack[T]{}
}

func (s *Stack[T]) Push(v T) {
	s.elems = append(s.elems, v)
}

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

func (s *Stack[T]) Peek() (T, bool) {
	var zero T
	if len(s.elems) == 0 {
		return zero, false
	}
	return s.elems[len(s.elems)-1], true
}

func (s *Stack[T]) Len() int {
	return len(s.elems)
}

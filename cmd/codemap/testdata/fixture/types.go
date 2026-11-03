package fixture

// Speaker speaks.
type Speaker interface{ Speak() string }

// Box is a plain struct.
type Box struct{ n int }

// Pair has two type parameters.
type Pair[K comparable, V any] struct {
	k K
	v V
}

// List has one type parameter.
type List[T any] struct{ items []T }

func Multi(
	a int,
	b string,
	rest ...float64,
) (n int, err error) {
	return a + len(b) + len(rest), nil
}

func Unnamed(x int) (int, string) { return x, "" }

func (b *Box) Ptr() int { return b.n }

func (b Box) Val() int { return b.n }

func (Box) NoName() {}

func (p *Pair[K, V]) Set(k K, v V) { p.k, p.v = k, v }

func (l List[T]) Len() int { return len(l.items) }

func (l *List[T]) Add(x T) { l.items = append(l.items, x) }

func (p Pair[K, V]) Get() (K, V) { return p.k, p.v }

func Generic[T ~int | ~string](x T, ys ...T) []T { return append(ys, x) }

func init() { setup() }

func init() { setup() }

func init() { setup() }

func setup() {}

func helper() int { return 1 }

func second() {}

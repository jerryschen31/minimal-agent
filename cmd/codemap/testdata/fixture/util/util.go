package util

// Counter counts.
type Counter struct{ n int }

// Inc adds one.
func (c *Counter) Inc() { c.n++ }

// Double doubles.
func Double(x int) int { return x * 2 }

// Map maps.
func Map[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

// Identity returns its argument.
func Identity[T any](x T) T { return x }

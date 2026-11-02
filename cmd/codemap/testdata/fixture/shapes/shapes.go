package shapes

// Shape has an area.
type Shape interface{ Area() float64 }

// Circle is a Shape.
type Circle struct{ R float64 }

// Area returns the circle's area.
func (c Circle) Area() float64 { return 3 * c.R * c.R }

// New returns a Circle.
func New(r float64) Circle { return Circle{R: r} }

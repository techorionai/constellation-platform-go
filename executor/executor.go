package executor

import "context"

// Executor runs one use case and returns no result.
type Executor[P any] interface {
	Execute(ctx context.Context, params P) error
}

// ExecutorWithReturn runs one use case and returns a result.
type ExecutorWithReturn[P, R any] interface {
	Execute(ctx context.Context, params P) (R, error)
}

// Func is a function that implements [Executor].
type Func[P any] func(ctx context.Context, params P) error

// Execute calls f.
func (f Func[P]) Execute(ctx context.Context, params P) error { return f(ctx, params) }

// FuncWithReturn is a function that implements [ExecutorWithReturn].
type FuncWithReturn[P, R any] func(ctx context.Context, params P) (R, error)

// Execute calls f.
func (f FuncWithReturn[P, R]) Execute(ctx context.Context, params P) (R, error) {
	return f(ctx, params)
}

var (
	_ Executor[int]                = Func[int](nil)
	_ ExecutorWithReturn[int, int] = FuncWithReturn[int, int](nil)
)

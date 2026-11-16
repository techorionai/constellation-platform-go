// Package executor holds the generic executor interfaces of the hexagonal
// architecture guide.
//
// A command or a query implements [Executor] or [ExecutorWithReturn]. A
// decorator, for example the tracing decorator of the package decorators,
// implements the same interface and wraps another executor. [Func] and
// [FuncWithReturn] turn a function into an executor. Tests and small use cases
// use them.
//
//	var create executor.Executor[CreateOrder] = executor.Func[CreateOrder](
//		func(ctx context.Context, p CreateOrder) error { return nil },
//	)
package executor

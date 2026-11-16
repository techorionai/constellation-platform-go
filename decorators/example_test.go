package decorators_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/techorionai/constellation-platform-go/decorators"
	"github.com/techorionai/constellation-platform-go/executor"
)

type CreateOrder struct{ ID string }

func ExampleApplyTracing() {
	var cmd executor.ExecutorWithReturn[CreateOrder, string] = executor.FuncWithReturn[CreateOrder, string](
		func(_ context.Context, p CreateOrder) (string, error) { return "created " + p.ID, nil },
	)
	// The recommended order, innermost first: the command, logging, tracing.
	cmd = decorators.ApplyLogging(cmd, "CreateOrder")
	cmd = decorators.ApplyTracing(cmd, "CreateOrder")

	res, err := cmd.Execute(context.Background(), CreateOrder{ID: "o1"})
	fmt.Println(res, err)
	// Output: created o1 <nil>
}

func ExampleApplyLoggingNoResult() {
	var cmd executor.Executor[CreateOrder] = executor.Func[CreateOrder](
		func(context.Context, CreateOrder) error { return nil },
	)
	cmd = decorators.ApplyLoggingNoResult(cmd, "DeleteOrder")
	cmd = decorators.ApplyTracingNoResult(cmd, "DeleteOrder")
	fmt.Println(cmd.Execute(context.Background(), CreateOrder{ID: "o1"}))
	// Output: <nil>
}

func ExampleWithErrorClassifier() {
	errValidation := errors.New("validation")
	cmd := executor.FuncWithReturn[int, int](func(context.Context, int) (int, error) { return 0, errValidation })
	// A validation error is expected. The span keeps the status Unset.
	traced := decorators.ApplyTracing[int, int](cmd, "Validate",
		decorators.WithErrorClassifier(func(err error) bool { return !errors.Is(err, errValidation) }))
	_, err := traced.Execute(context.Background(), 1)
	fmt.Println(err)
	// Output: validation
}

package executor_test

import (
	"context"
	"fmt"

	"github.com/techorionai/constellation-platform-go/executor"
)

type CreateOrder struct{ ID string }

func ExampleFunc() {
	var create executor.Executor[CreateOrder] = executor.Func[CreateOrder](
		func(_ context.Context, p CreateOrder) error {
			fmt.Println("create", p.ID)
			return nil
		},
	)
	err := create.Execute(context.Background(), CreateOrder{ID: "o1"})
	fmt.Println(err)
	// Output:
	// create o1
	// <nil>
}

func ExampleFuncWithReturn() {
	var get executor.ExecutorWithReturn[string, int] = executor.FuncWithReturn[string, int](
		func(_ context.Context, s string) (int, error) { return len(s), nil },
	)
	n, err := get.Execute(context.Background(), "abc")
	fmt.Println(n, err)
	// Output: 3 <nil>
}

package executor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/techorionai/constellation-platform-go/executor"
)

// U1
func TestFuncExecutors(t *testing.T) {
	errX := errors.New("x")
	var e executor.Executor[int] = executor.Func[int](func(_ context.Context, p int) error {
		if p == 1 {
			return errX
		}
		return nil
	})
	if err := e.Execute(t.Context(), 1); !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	if err := e.Execute(t.Context(), 2); err != nil {
		t.Fatalf("err = %v", err)
	}
	var r executor.ExecutorWithReturn[int, string] = executor.FuncWithReturn[int, string](
		func(_ context.Context, p int) (string, error) {
			if p == 1 {
				return "", errX
			}
			return "ok", nil
		})
	if v, err := r.Execute(t.Context(), 2); v != "ok" || err != nil {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := r.Execute(t.Context(), 1); !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
}

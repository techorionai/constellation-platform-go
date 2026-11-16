package mock_test

import (
	"context"
	"fmt"

	"github.com/techorionai/constellation-platform-go/migrate"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

func ExampleMigrator() {
	l := &mock.Locker{}
	m := &mock.Migrator{UpFunc: func(context.Context) (migrate.Result, error) {
		return migrate.Result{ToVersion: 2, Applied: 2}, nil
	}}
	res, err := migrate.Run(context.Background(), l, m, migrate.Options{})
	fmt.Println(res.Applied, err, l.UnlockCalls())
	// Output: 2 <nil> 1
}

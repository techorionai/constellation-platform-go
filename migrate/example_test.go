package migrate_test

import (
	"context"
	"fmt"

	"github.com/techorionai/constellation-platform-go/migrate"
)

type sampleLocker struct{}

func (sampleLocker) Lock(context.Context) (func(context.Context) error, error) {
	return func(context.Context) error { return nil }, nil
}

type sampleMigrator struct{}

func (sampleMigrator) Up(context.Context) (migrate.Result, error) {
	return migrate.Result{FromVersion: 1, ToVersion: 3, Applied: 2}, nil
}

// ExampleRun uses a locker and a migrator that do no database work. A service
// uses the adapters of migrate/postgres.
func ExampleRun() {
	res, err := migrate.Run(context.Background(), sampleLocker{}, sampleMigrator{}, migrate.Options{})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(res.Applied, res.ToVersion)
	// Output: 2 3
}

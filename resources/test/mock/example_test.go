package mock_test

import (
	"context"
	"fmt"
	"time"

	"github.com/techorionai/constellation-platform-go/cache"
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

func ExampleCacheStore() {
	store := &mock.CacheStore{}
	c := cache.New[string](store, "ns")
	_ = c.Set(context.Background(), "k", "v", time.Minute)
	v, err := c.Get(context.Background(), "k")
	fmt.Println(v, err, store.SetCalls())
	// Output: v <nil> 1
}

func ExampleTransactor() {
	tx := &mock.Transactor{}
	err := tx.WithinTx(context.Background(), func(ctx context.Context) error {
		tx.AfterCommit(ctx, func(context.Context) { fmt.Println("after commit") })
		return nil
	})
	fmt.Println(err, tx.WithinTxCalls())
	// Output:
	// after commit
	// <nil> 1
}

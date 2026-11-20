package mock_test

import (
	"context"
	"fmt"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/cache"
	"github.com/techorionai/constellation-platform-go/migrate"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/retention"
	"github.com/techorionai/constellation-platform-go/secrets"
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

func ExampleSecretsRepository() {
	repo := &mock.SecretsRepository{}
	repo.Put("a", secrets.Envelope{Format: secrets.FormatV1, KeyID: "k1"})
	rec, ok := repo.Record("a")
	fmt.Println(rec.Name, rec.Envelope.KeyID, ok, repo.UpsertCalls())
	// Output: a k1 true 0
}

func ExampleRetentionPurger() {
	purger := &mock.RetentionPurger{PurgeFunc: func(context.Context, retention.PurgeParams) (int, error) {
		return 3, nil
	}}
	runner, err := retention.NewRunner(&mock.RetentionLocker{}, []retention.Job{{
		Name:          "events",
		Config:        retention.Config{Interval: time.Hour, BatchSize: 100},
		DefaultPeriod: time.Hour,
		Purger:        purger,
	}}, retention.RunnerOptions{})
	if err != nil {
		fmt.Println(err)
		return
	}
	n, err := runner.RunOnce(context.Background(), "events")
	fmt.Println(n, err, purger.PurgeCalls())
	// Output: 3 <nil> 1
}

func ExampleVerifier() {
	v := &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{ID: "u1"}, nil
	}}
	p, err := v.Verify(context.Background(), "token")
	fmt.Println(p.ID, err, v.VerifyCalls())
	// Output: u1 <nil> 1
}

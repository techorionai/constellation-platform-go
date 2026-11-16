package retention_test

import (
	"context"
	"fmt"
	"time"

	"github.com/techorionai/constellation-platform-go/retention"
)

type sampleLocker struct{}

func (sampleLocker) TryLock(context.Context, string) (func(context.Context) error, bool, error) {
	return func(context.Context) error { return nil }, true, nil
}

type samplePurger struct{ left int }

func (p *samplePurger) Purge(_ context.Context, params retention.PurgeParams) (int, error) {
	n := min(p.left, params.Limit)
	p.left -= n
	return n, nil
}

// ExampleRunner_RunOnce uses a locker and a purger that do no database work. A
// service uses the adapters of retention/postgres.
func ExampleRunner_RunOnce() {
	runner, err := retention.NewRunner(sampleLocker{}, []retention.Job{{
		Name:          "audit",
		Config:        retention.Config{Interval: time.Hour, BatchSize: 100},
		DefaultPeriod: 60 * 24 * time.Hour,
		Purger:        &samplePurger{left: 250},
	}}, retention.RunnerOptions{})
	if err != nil {
		fmt.Println(err)
		return
	}
	n, err := runner.RunOnce(context.Background(), "audit")
	fmt.Println(n, err)
	// Output: 250 <nil>
}

func ExampleConfig_Validate() {
	cfg := retention.Config{Interval: 30 * time.Second, BatchSize: 1000}
	fmt.Println(cfg.Validate())
	// Output: retention: invalid config: interval must be 1m0s or more
}

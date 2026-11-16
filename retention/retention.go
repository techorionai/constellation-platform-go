package retention

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Errors of [NewRunner], [Config.Validate], and [Runner.RunOnce].
var (
	// ErrInvalidConfig means that a [Config] value is out of range.
	ErrInvalidConfig = errors.New("retention: invalid config")
	// ErrInvalidJob means that a [Job] has a wrong name or no purger.
	ErrInvalidJob = errors.New("retention: invalid job")
	// ErrNoPeriod means that a job has no period: Config.Period and DefaultPeriod
	// are both 0, or DefaultPeriod is negative.
	ErrNoPeriod = errors.New("retention: the job has no period")
	// ErrDuplicateJob means that two jobs have the same name.
	ErrDuplicateJob = errors.New("retention: duplicate job name")
	// ErrUnknownJob means that the runner has no job with the name.
	ErrUnknownJob = errors.New("retention: unknown job")
	// ErrLocked means that another instance has the lock, so the tick did not
	// run.
	ErrLocked = errors.New("retention: another instance has the lock")
)

const (
	// DefaultInterval is the time between two ticks when Config.Interval is not
	// set.
	DefaultInterval = time.Hour
	// MinInterval is the smallest value of Config.Interval.
	MinInterval = time.Minute
	// DefaultBatchSize is the batch size when Config.BatchSize is not set.
	DefaultBatchSize = 1000
	// MaxBatchSize is the largest value of Config.BatchSize.
	MaxBatchSize = 100000
)

// Config holds the settings of one job. A service nests one Config for each job
// with the tag envPrefix:"RETENTION__<JOB>__", for example
// IDENTITY_RETENTION__AUDIT__PERIOD=1440h.
type Config struct {
	// Period is the age after which data expires. 0 means that the job uses
	// DefaultPeriod. Variable: PERIOD.
	Period time.Duration `env:"PERIOD"`
	// Interval is the time between the end of a tick and the start of the next
	// tick. It is 1 minute or more. Variable: INTERVAL.
	Interval time.Duration `env:"INTERVAL" envDefault:"1h"`
	// BatchSize is the maximum number of items that one Purge call deletes. It
	// is from 1 to 100000. Variable: BATCH_SIZE.
	BatchSize int `env:"BATCH_SIZE" envDefault:"1000"`
}

// Validate checks that Period >= 0, Interval >= 1 minute, and BatchSize is from
// 1 to 100000.
func (c Config) Validate() error {
	switch {
	case c.Period < 0:
		return fmt.Errorf("%w: period must not be negative", ErrInvalidConfig)
	case c.Interval < MinInterval:
		return fmt.Errorf("%w: interval must be %s or more", ErrInvalidConfig, MinInterval)
	case c.BatchSize < 1 || c.BatchSize > MaxBatchSize:
		return fmt.Errorf("%w: batch size must be from 1 to %d", ErrInvalidConfig, MaxBatchSize)
	}
	return nil
}

// PurgeParams are the parameters of [Purger.Purge].
type PurgeParams struct {
	// Before is the expiry time. The purger deletes items older than Before.
	Before time.Time
	// Limit is the maximum number of items to delete.
	Limit int
}

// Purger deletes expired items.
type Purger interface {
	// Purge deletes at most params.Limit items older than params.Before. It
	// returns the count.
	Purge(ctx context.Context, params PurgeParams) (int, error)
}

// Locker makes sure that only one instance runs a job at one time.
type Locker interface {
	// TryLock does not wait. acquired is false if another instance has the lock.
	// The caller calls unlock after the tick, also after an error.
	TryLock(ctx context.Context, name string) (unlock func(context.Context) error, acquired bool, err error)
}

// Job is one retention job.
type Job struct {
	// Name matches ^[a-z][a-z0-9_]{0,63}$. It is the value of the metric
	// attribute job.
	Name string
	// Config holds the settings that the environment gives.
	Config Config
	// DefaultPeriod is the period when Config.Period is 0.
	DefaultPeriod time.Duration
	// Purger deletes the expired items.
	Purger Purger
}

// period returns the period of the job.
func (j Job) period() time.Duration {
	if j.Config.Period > 0 {
		return j.Config.Period
	}
	return j.DefaultPeriod
}

package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Errors returned by [Run] and by the adapters.
var (
	// ErrLockTimeout means that Run did not get the lock in Options.LockTimeout.
	ErrLockTimeout = errors.New("migrate: lock timeout")
	// ErrDirty means that the database has a failed migration. An operator must
	// fix the database.
	ErrDirty = errors.New("migrate: database is dirty")
	// ErrInvalidOptions means that Options.Name or Options.LockTimeout is not
	// valid.
	ErrInvalidOptions = errors.New("migrate: invalid options")
)

const (
	// DefaultName is the name of the migration set when Options.Name is empty.
	DefaultName = "schema_migrations"
	// DefaultLockTimeout is the lock timeout when Options.LockTimeout is 0.
	DefaultLockTimeout = 10 * time.Minute

	unlockTimeout = 10 * time.Second
	scopeName     = "github.com/techorionai/constellation-platform-go/migrate"
)

var namePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Locker gives a lock that all instances of a service share.
type Locker interface {
	// Lock waits for the lock until ctx ends. It returns a function that releases
	// the lock.
	Lock(ctx context.Context) (unlock func(context.Context) error, err error)
}

// Migrator applies the pending up migrations.
type Migrator interface {
	// Up applies all pending up migrations. No pending migration is not an error.
	// When ctx ends, Up completes the current migration, stops, and returns
	// ctx.Err().
	Up(ctx context.Context) (Result, error)
}

// Result is the result of [Migrator.Up].
type Result struct {
	// FromVersion is the database version before Up.
	FromVersion uint
	// ToVersion is the database version after Up.
	ToVersion uint
	// Applied is the number of migrations that Up applied.
	Applied int
	// Ahead is true when the database version is higher than the newest version
	// in the source.
	Ahead bool
}

// Options are the settings for [Run].
type Options struct {
	// Name is the name of the migration set. An empty Name gives
	// "schema_migrations". It must match ^[a-z_][a-z0-9_]{0,62}$. Use the same
	// name as MigrationsTable of the migrator and as the name of the locker.
	Name string
	// LockTimeout is the maximum time to wait for the lock. 0 gives 10 minutes.
	// A negative value is not valid.
	LockTimeout time.Duration
}

// Run takes the lock, calls migrator.Up, and releases the lock. The release
// also happens when Up fails or when ctx ends.
//
// If the lock timeout ends, Run returns [ErrLockTimeout]. If ctx ends first, Run
// returns an error that wraps ctx.Err(). If the database is dirty, the error
// wraps [ErrDirty]. The caller stops the service on an error.
func Run(ctx context.Context, locker Locker, migrator Migrator, opts Options) (Result, error) {
	name := opts.Name
	if name == "" {
		name = DefaultName
	}
	if !namePattern.MatchString(name) {
		return Result{}, fmt.Errorf("%w: name must match %s", ErrInvalidOptions, namePattern)
	}
	timeout := opts.LockTimeout
	if timeout < 0 {
		return Result{}, fmt.Errorf("%w: lock timeout must not be negative", ErrInvalidOptions)
	}
	if timeout == 0 {
		timeout = DefaultLockTimeout
	}

	nameAttr := attribute.String("migrate.name", name)
	ctx, span := otel.Tracer(scopeName).Start(ctx, "migrate.run", trace.WithAttributes(nameAttr))
	defer span.End()

	start := time.Now()
	res, err := execute(ctx, locker, migrator, timeout)
	elapsed := time.Since(start)

	outcome := classify(ctx, res, err)
	logResult(ctx, name, res, err, elapsed)
	if err != nil {
		// The error text can hold SQL text and database detail, so the span gets
		// only the outcome and the error type.
		span.SetAttributes(attribute.String("error.type", errorType(err)))
		span.SetStatus(codes.Error, outcome)
	}
	span.SetAttributes(attribute.String("outcome", outcome))

	hist, herr := otel.GetMeterProvider().Meter(scopeName).Float64Histogram("migrate.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of one migration run."))
	if herr == nil {
		hist.Record(context.WithoutCancel(ctx), elapsed.Seconds(),
			metric.WithAttributes(nameAttr, attribute.String("outcome", outcome)))
	}
	return res, err
}

func execute(ctx context.Context, locker Locker, migrator Migrator, timeout time.Duration) (Result, error) {
	lockCtx, cancel := context.WithTimeout(ctx, timeout)
	unlock, err := locker.Lock(lockCtx)
	lockErr := lockCtx.Err()
	cancel()
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return Result{}, fmt.Errorf("migrate: wait for lock: %w", ctx.Err())
		case errors.Is(lockErr, context.DeadlineExceeded):
			return Result{}, ErrLockTimeout
		default:
			return Result{}, fmt.Errorf("migrate: lock: %w", err)
		}
	}

	res, upErr := migrator.Up(ctx)

	unlockCtx, unlockCancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer unlockCancel()
	if err := unlock(unlockCtx); err != nil {
		slog.WarnContext(unlockCtx, "migrate: cannot release the lock", slog.Any("error", err))
	}
	return res, upErr
}

func classify(ctx context.Context, res Result, err error) string {
	switch {
	case err == nil && res.Ahead:
		return "ahead"
	case err == nil:
		return "ok"
	case errors.Is(err, ErrLockTimeout):
		return "lock_timeout"
	case errors.Is(err, ErrDirty):
		return "dirty"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "error"
	}
}

// errorType returns the Go type of the innermost error in the chain of err.
func errorType(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}

func logResult(ctx context.Context, name string, res Result, err error, elapsed time.Duration) {
	ctx = context.WithoutCancel(ctx)
	attrs := []any{
		slog.String("migrate.name", name),
		slog.Duration("duration", elapsed),
	}
	if err != nil {
		slog.ErrorContext(ctx, "migrate: run failed", append(attrs, slog.Any("error", err))...)
		return
	}
	attrs = append(attrs,
		slog.Uint64("from_version", uint64(res.FromVersion)),
		slog.Uint64("to_version", uint64(res.ToVersion)),
		slog.Int("applied", res.Applied),
	)
	if res.Ahead {
		slog.WarnContext(ctx, "migrate: the database version is higher than the newest source version, so no migration was applied", attrs...)
		return
	}
	slog.InfoContext(ctx, "migrate: run complete", attrs...)
}

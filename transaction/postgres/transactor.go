package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	clientspg "github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/transaction"
)

const (
	scopeName = "github.com/techorionai/constellation-platform-go/transaction/postgres"

	// DefaultHookTimeout is the time limit of each AfterCommit hook when
	// [WithHookTimeout] is not set.
	DefaultHookTimeout = 5 * time.Second

	rollbackTimeout = 10 * time.Second
	commitTimeout   = 30 * time.Second

	sqlstateSerialization = "40001"
	sqlstateDeadlock      = "40P01"
)

var durationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

var _ transaction.Transactor = (*Transactor)(nil)

type hooksKey struct{}

// hookList holds the AfterCommit hooks of one outermost transaction. A mutex
// protects it, because goroutines that fn starts can register hooks.
type hookList struct {
	mu     sync.Mutex
	hooks  []func(context.Context)
	closed bool
}

func (h *hookList) add(fn func(context.Context)) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.hooks = append(h.hooks, fn)
	return true
}

// close closes the list and returns the hooks.
func (h *hookList) close() []func(context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	hooks := h.hooks
	h.hooks = nil
	return hooks
}

// TransactorOption changes the settings of a [Transactor].
type TransactorOption func(*Transactor)

// WithHookTimeout sets the time limit of each AfterCommit hook. The default is
// [DefaultHookTimeout]. A value of 0 or less causes a panic.
func WithHookTimeout(d time.Duration) TransactorOption {
	if d <= 0 {
		panic("transaction/postgres: the hook timeout must be more than 0")
	}
	return func(t *Transactor) { t.hookTimeout = d }
}

// Transactor is the PostgreSQL adapter of [transaction.Transactor].
type Transactor struct {
	pool        *pgxpool.Pool
	hookTimeout time.Duration

	duration   metric.Float64Histogram
	active     metric.Int64UpDownCounter
	begin      metric.Float64Histogram
	serialFail metric.Int64Counter
	nested     metric.Int64Counter
}

// NewTransactor returns a Transactor on the pool.
func NewTransactor(pool *pgxpool.Pool, opts ...TransactorOption) *Transactor {
	t := &Transactor{pool: pool, hookTimeout: DefaultHookTimeout}
	for _, o := range opts {
		o(t)
	}
	// A failed instrument creation gives a no-op instrument in the SDK.
	m := otel.GetMeterProvider().Meter(scopeName)
	t.duration, _ = m.Float64Histogram("constellation.db.transaction.duration", metric.WithUnit("s"),
		metric.WithDescription("Time from the start of the transaction to the end of the commit or the rollback."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	t.active, _ = m.Int64UpDownCounter("constellation.db.transaction.active", metric.WithUnit("{transaction}"),
		metric.WithDescription("Number of open transactions."))
	t.begin, _ = m.Float64Histogram("constellation.db.transaction.begin.duration", metric.WithUnit("s"),
		metric.WithDescription("Time to start a transaction, including the wait for a connection."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	t.serialFail, _ = m.Int64Counter("constellation.db.transaction.serialization_failures", metric.WithUnit("{failure}"),
		metric.WithDescription("Number of serialization failures and deadlocks."))
	t.nested, _ = m.Int64Counter("constellation.db.transaction.nested", metric.WithUnit("{call}"),
		metric.WithDescription("Number of WithinTx calls that joined an outer transaction."))
	return t
}

func pgxOptions(s transaction.Settings) pgx.TxOptions {
	o := pgx.TxOptions{AccessMode: pgx.ReadWrite}
	if s.ReadOnly {
		o.AccessMode = pgx.ReadOnly
	}
	switch s.Isolation {
	case transaction.RepeatableRead:
		o.IsoLevel = pgx.RepeatableRead
	case transaction.Serializable:
		o.IsoLevel = pgx.Serializable
	default:
		o.IsoLevel = pgx.ReadCommitted
	}
	return o
}

// WithinTx runs fn in one transaction. See [transaction.Transactor].
//
// A nested call joins the transaction of the outer call. There are no
// savepoints, so an inner error that the outer function ignores does not roll
// back the inner work alone. If ctx has a transaction that this adapter did not
// start, WithinTx returns [transaction.ErrForeignTx].
//
// The commit and the rollback run with a context that keeps the values of ctx
// but not its cancel, and they have a time limit. A cancel of ctx after fn
// returns nil does not stop the commit. If fn calls runtime.Goexit, for example
// through testing.T.FailNow, WithinTx rolls back, releases the connection, and
// records the outcome goexit before the goroutine ends.
//
// If the commit returns an error, the outcome is unknown: the server can have
// committed the transaction when the connection broke or the time limit ended.
// WithinTx then returns an error and does not run the AfterCommit hooks, so a
// cache entry that a hook would delete stays until its TTL ends. A caller must
// not assume that the work did not happen. Make the work idempotent, or read
// the data again before a retry.
func (t *Transactor) WithinTx(ctx context.Context, fn func(ctx context.Context) error, opts ...transaction.Option) error {
	s, err := transaction.Resolve(opts...)
	if err != nil {
		return err
	}
	nameAttr := attribute.String("db.transaction.name", s.Name)

	if _, hasTx := clientspg.TxFromContext(ctx); hasTx {
		if _, ok := ctx.Value(hooksKey{}).(*hookList); !ok {
			return transaction.ErrForeignTx
		}
		if s.HasTxOptions() {
			return transaction.ErrNestedOptions
		}
		t.nested.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(nameAttr))
		return fn(ctx)
	}

	isoAttr := attribute.String("db.transaction.isolation", s.Isolation.String())
	roAttr := attribute.Bool("db.transaction.read_only", s.ReadOnly)
	ctx, span := otel.Tracer(scopeName).Start(ctx, "db.transaction", trace.WithAttributes(nameAttr, isoAttr, roAttr))
	defer span.End()

	start := time.Now()
	tx, err := t.pool.BeginTx(ctx, pgxOptions(s))
	t.begin.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(), metric.WithAttributes(nameAttr))
	if err != nil {
		t.finish(ctx, span, start, "begin_error", false, nameAttr, isoAttr, roAttr)
		return fmt.Errorf("transaction: cannot start the transaction: %w", err)
	}
	t.active.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(nameAttr))

	list := &hookList{}
	txCtx := clientspg.ContextWithTx(context.WithValue(ctx, hooksKey{}, list), tx)

	// runtime.Goexit in fn runs the deferred calls but not the code after runFn.
	// This defer cleans up in that case only.
	fnReturned := false
	defer func() {
		if fnReturned {
			return
		}
		_ = t.rollback(ctx, tx)
		list.close()
		t.finish(ctx, span, start, "goexit", true, nameAttr, isoAttr, roAttr)
	}()

	panicked, panicValue, fnErr := runFn(txCtx, fn)
	fnReturned = true

	if panicked {
		_ = t.rollback(ctx, tx)
		list.close()
		t.finish(ctx, span, start, "panic", true, nameAttr, isoAttr, roAttr)
		panic(panicValue)
	}
	if fnErr != nil {
		rbErr := t.rollback(ctx, tx)
		list.close()
		t.finish(ctx, span, start, "rollback", true, nameAttr, isoAttr, roAttr)
		fnErr = t.wrapErr(ctx, nameAttr, fnErr)
		if rbErr != nil {
			return errors.Join(fnErr, rbErr)
		}
		return fnErr
	}

	if err := t.commit(ctx, tx); err != nil {
		_ = t.rollback(ctx, tx)
		list.close()
		t.finish(ctx, span, start, "commit_error", true, nameAttr, isoAttr, roAttr)
		return t.wrapErr(ctx, nameAttr, fmt.Errorf("transaction: commit failed: %w", err))
	}
	hooks := list.close()
	t.finish(ctx, span, start, "commit", true, nameAttr, isoAttr, roAttr)
	for _, h := range hooks {
		t.runHook(ctx, h)
	}
	return nil
}

// runFn calls fn and recovers a panic.
func runFn(ctx context.Context, fn func(ctx context.Context) error) (panicked bool, value any, err error) {
	panicked = true
	defer func() {
		if panicked {
			value = recover()
		}
	}()
	err = fn(ctx)
	panicked = false
	return false, nil, err
}

// commit commits the transaction. The caller context can be done, so the
// commit uses a context without cancel and with a time limit. A cancel then
// cannot give an error for a commit that the server accepts.
func (t *Transactor) commit(ctx context.Context, tx pgx.Tx) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	return tx.Commit(cctx)
}

// rollback rolls the transaction back. The caller context can be done, so the
// rollback uses a context without cancel and with a time limit. A rollback
// error is ignored when the transaction is already closed.
func (t *Transactor) rollback(ctx context.Context, tx pgx.Tx) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := tx.Rollback(rctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("transaction: rollback failed: %w", err)
	}
	return nil
}

// wrapErr wraps a serialization failure or a deadlock with
// transaction.ErrSerialization, and counts it. Other errors do not change.
func (t *Transactor) wrapErr(ctx context.Context, nameAttr attribute.KeyValue, err error) error {
	code := sqlstate(err)
	if code != sqlstateSerialization && code != sqlstateDeadlock {
		return err
	}
	t.serialFail.Add(context.WithoutCancel(ctx), 1,
		metric.WithAttributes(nameAttr, attribute.String("sqlstate", code)))
	return fmt.Errorf("%w: %w", transaction.ErrSerialization, err)
}

func sqlstate(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// finish records the duration, decreases the active count, and ends the span
// status.
func (t *Transactor) finish(ctx context.Context, span trace.Span, start time.Time, outcome string, wasActive bool,
	nameAttr, isoAttr, roAttr attribute.KeyValue) {
	ctx = context.WithoutCancel(ctx)
	if wasActive {
		t.active.Add(ctx, -1, metric.WithAttributes(nameAttr))
	}
	t.duration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(nameAttr, isoAttr, roAttr, attribute.String("outcome", outcome)))
	span.SetAttributes(attribute.String("outcome", outcome))
	if outcome != "commit" {
		// The error text can contain SQL detail, so the span gets the outcome only.
		span.SetStatus(codes.Error, outcome)
	}
}

// runHook runs one hook with a time limit. It recovers a panic of the hook.
func (t *Transactor) runHook(ctx context.Context, h func(context.Context)) {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.hookTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(hctx, "transaction: an AfterCommit hook panicked", slog.Any("panic", r))
			}
		}()
		h(hctx)
	}()
	select {
	case <-done:
	case <-hctx.Done():
	}
	// A hook that ends at the time limit, for example because it obeys its
	// context, also counts as a hook that hit the limit.
	if errors.Is(hctx.Err(), context.DeadlineExceeded) {
		slog.ErrorContext(ctx, "transaction: an AfterCommit hook hit the time limit, so the transaction call goes on",
			slog.Duration("hook_timeout", t.hookTimeout))
	}
}

// AfterCommit registers fn to run after the commit of the outermost
// transaction. See [transaction.Transactor].
//
// If ctx has no transaction, fn runs at once. If the transaction already ended,
// the hook is dropped and the adapter writes an error log record. If ctx has a
// transaction that this adapter did not start, AfterCommit panics.
func (t *Transactor) AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if list, ok := ctx.Value(hooksKey{}).(*hookList); ok {
		if !list.add(fn) {
			slog.ErrorContext(ctx, "transaction: AfterCommit was called after the transaction ended, so the hook is dropped")
		}
		return
	}
	if _, hasTx := clientspg.TxFromContext(ctx); hasTx {
		panic("transaction/postgres: AfterCommit called in a context with a transaction that this Transactor did not start")
	}
	fn(ctx)
}

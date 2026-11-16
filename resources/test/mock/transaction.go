package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/transaction"
)

var _ transaction.Transactor = (*Transactor)(nil)

type hooksKey struct{}

type hookList struct {
	mu    sync.Mutex
	hooks []func(context.Context)
}

// Transactor is a mock of [transaction.Transactor]. It runs the function
// without a database transaction, but it follows the rules of the real
// adapter for the options and for the AfterCommit hooks. Thus a unit test with
// the mock and the same code with the adapter run the hooks at the same point.
type Transactor struct {
	// BeforeFunc runs before fn. If it returns an error, WithinTx returns that
	// error and does not call fn. If it is nil, WithinTx goes on.
	BeforeFunc func(ctx context.Context) error

	mu          sync.Mutex
	txCalls     int
	commitCalls int
}

// WithinTx checks the options with [transaction.Resolve] and returns its error.
// A nested call with an option other than Name returns
// [transaction.ErrNestedOptions]. Then it calls BeforeFunc, then fn. The
// outermost call runs the AfterCommit hooks, in the order of registration, if
// fn returns nil. A nested call never runs hooks.
func (t *Transactor) WithinTx(ctx context.Context, fn func(ctx context.Context) error, opts ...transaction.Option) error {
	t.mu.Lock()
	t.txCalls++
	t.mu.Unlock()
	s, err := transaction.Resolve(opts...)
	if err != nil {
		return err
	}
	_, nested := ctx.Value(hooksKey{}).(*hookList)
	if nested && s.HasTxOptions() {
		return transaction.ErrNestedOptions
	}
	if t.BeforeFunc != nil {
		if err := t.BeforeFunc(ctx); err != nil {
			return err
		}
	}
	if nested {
		return fn(ctx)
	}
	list := &hookList{}
	if err := fn(context.WithValue(ctx, hooksKey{}, list)); err != nil {
		return err
	}
	t.mu.Lock()
	t.commitCalls++
	t.mu.Unlock()
	list.mu.Lock()
	hooks := list.hooks
	list.hooks = nil
	list.mu.Unlock()
	for _, h := range hooks {
		h(context.WithoutCancel(ctx))
	}
	return nil
}

// AfterCommit keeps the hook if ctx is in a WithinTx call. Else it runs the hook
// at once.
func (t *Transactor) AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if list, ok := ctx.Value(hooksKey{}).(*hookList); ok {
		list.mu.Lock()
		list.hooks = append(list.hooks, fn)
		list.mu.Unlock()
		return
	}
	fn(ctx)
}

// WithinTxCalls returns the number of WithinTx calls.
func (t *Transactor) WithinTxCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.txCalls
}

// CommitCalls returns the number of outermost WithinTx calls in which fn
// returned nil.
func (t *Transactor) CommitCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.commitCalls
}

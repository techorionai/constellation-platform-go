package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/migrate"
)

var (
	_ migrate.Locker   = (*Locker)(nil)
	_ migrate.Migrator = (*Migrator)(nil)
)

// Locker is a mock of [migrate.Locker].
type Locker struct {
	// LockFunc replaces Lock. If it is nil, Lock succeeds at once.
	LockFunc func(ctx context.Context) (func(context.Context) error, error)
	// UnlockFunc replaces the function that Lock returns. If it is nil, the
	// unlock function succeeds.
	UnlockFunc func(ctx context.Context) error

	mu         sync.Mutex
	lockCalls  int
	unlockCall int
	lockCtx    context.Context
}

// Lock calls LockFunc, or succeeds. The unlock function that it returns counts
// its calls.
func (l *Locker) Lock(ctx context.Context) (func(context.Context) error, error) {
	l.mu.Lock()
	l.lockCalls++
	l.lockCtx = ctx
	l.mu.Unlock()
	if l.LockFunc != nil {
		unlock, err := l.LockFunc(ctx)
		if err != nil {
			return nil, err
		}
		return l.wrap(unlock), nil
	}
	return l.wrap(l.UnlockFunc), nil
}

func (l *Locker) wrap(inner func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		l.mu.Lock()
		l.unlockCall++
		l.mu.Unlock()
		if inner == nil {
			return nil
		}
		return inner(ctx)
	}
}

// LockCalls returns the number of Lock calls.
func (l *Locker) LockCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lockCalls
}

// UnlockCalls returns the number of calls of the unlock function.
func (l *Locker) UnlockCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unlockCall
}

// LockContext returns the context of the last Lock call.
func (l *Locker) LockContext() context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lockCtx
}

// Migrator is a mock of [migrate.Migrator].
type Migrator struct {
	// UpFunc replaces Up. If it is nil, Up returns an empty result.
	UpFunc func(ctx context.Context) (migrate.Result, error)

	mu    sync.Mutex
	calls int
}

// Up calls UpFunc, or returns an empty result.
func (m *Migrator) Up(ctx context.Context) (migrate.Result, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.UpFunc != nil {
		return m.UpFunc(ctx)
	}
	return migrate.Result{}, nil
}

// UpCalls returns the number of Up calls.
func (m *Migrator) UpCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

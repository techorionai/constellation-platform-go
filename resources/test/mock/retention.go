package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/retention"
)

var (
	_ retention.Purger = (*RetentionPurger)(nil)
	_ retention.Locker = (*RetentionLocker)(nil)
)

// RetentionPurger is a mock of [retention.Purger]. If PurgeFunc is nil, Purge
// deletes nothing and returns 0.
type RetentionPurger struct {
	PurgeFunc func(ctx context.Context, params retention.PurgeParams) (int, error)

	mu     sync.Mutex
	params []retention.PurgeParams
}

// Purge records the parameters and calls PurgeFunc.
func (p *RetentionPurger) Purge(ctx context.Context, params retention.PurgeParams) (int, error) {
	p.mu.Lock()
	p.params = append(p.params, params)
	p.mu.Unlock()
	if p.PurgeFunc != nil {
		return p.PurgeFunc(ctx, params)
	}
	return 0, nil
}

// PurgeCalls returns the number of Purge calls.
func (p *RetentionPurger) PurgeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.params)
}

// PurgeParams returns the parameters of each Purge call.
func (p *RetentionPurger) PurgeParams() []retention.PurgeParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]retention.PurgeParams(nil), p.params...)
}

// RetentionLocker is a mock of [retention.Locker]. If TryLockFunc is nil, TryLock
// acquires the lock. If Busy is true, TryLock does not acquire it. The unlock
// function counts its calls.
type RetentionLocker struct {
	// TryLockFunc replaces TryLock. The mock still counts the calls of the
	// unlock function that it returns.
	TryLockFunc func(ctx context.Context, name string) (func(context.Context) error, bool, error)
	// Busy makes TryLock return acquired false, when TryLockFunc is nil.
	Busy bool

	mu       sync.Mutex
	names    []string
	unlocked int
}

// TryLock records the name and calls TryLockFunc, or acquires the lock.
func (l *RetentionLocker) TryLock(ctx context.Context, name string) (func(context.Context) error, bool, error) {
	l.mu.Lock()
	l.names = append(l.names, name)
	l.mu.Unlock()
	var (
		inner    func(context.Context) error
		acquired = !l.Busy
	)
	if l.TryLockFunc != nil {
		var err error
		inner, acquired, err = l.TryLockFunc(ctx, name)
		if err != nil {
			return nil, false, err
		}
	}
	if !acquired {
		return nil, false, nil
	}
	return func(ctx context.Context) error {
		l.mu.Lock()
		l.unlocked++
		l.mu.Unlock()
		if inner == nil {
			return nil
		}
		return inner(ctx)
	}, true, nil
}

// TryLockCalls returns the number of TryLock calls.
func (l *RetentionLocker) TryLockCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.names)
}

// UnlockCalls returns the number of calls of the unlock function.
func (l *RetentionLocker) UnlockCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unlocked
}

// LockNames returns the name of each TryLock call.
func (l *RetentionLocker) LockNames() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...)
}

package mock

import (
	"context"
	"sync"
	"time"

	"github.com/techorionai/constellation-platform-go/outbox"
)

var (
	_ outbox.Writer    = (*OutboxWriter)(nil)
	_ outbox.Inserter  = (*OutboxInserter)(nil)
	_ outbox.Store     = (*OutboxStore)(nil)
	_ outbox.Publisher = (*OutboxPublisher)(nil)
)

// OutboxWriter is a mock of [outbox.Writer]. It records the events. If WriteFunc
// is nil, Write returns nil.
type OutboxWriter struct {
	WriteFunc func(ctx context.Context, events ...outbox.Event) error

	mu     sync.Mutex
	events []outbox.Event
}

// Write records the events and calls WriteFunc.
func (w *OutboxWriter) Write(ctx context.Context, events ...outbox.Event) error {
	w.mu.Lock()
	w.events = append(w.events, events...)
	w.mu.Unlock()
	if w.WriteFunc != nil {
		return w.WriteFunc(ctx, events...)
	}
	return nil
}

// Events returns the events of all Write calls.
func (w *OutboxWriter) Events() []outbox.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]outbox.Event(nil), w.events...)
}

// OutboxInserter is a mock of [outbox.Inserter]. It records the rows. The mock
// reports a transaction when NoTx is false.
type OutboxInserter struct {
	// NoTx makes InTransaction return false.
	NoTx bool
	// InsertFunc replaces Insert. If it is nil, Insert returns nil.
	InsertFunc func(ctx context.Context, rows []outbox.Row) error

	mu   sync.Mutex
	rows []outbox.Row
}

// InTransaction returns true unless NoTx is set.
func (i *OutboxInserter) InTransaction(context.Context) bool { return !i.NoTx }

// Insert records the rows and calls InsertFunc.
func (i *OutboxInserter) Insert(ctx context.Context, rows []outbox.Row) error {
	i.mu.Lock()
	i.rows = append(i.rows, rows...)
	i.mu.Unlock()
	if i.InsertFunc != nil {
		return i.InsertFunc(ctx, rows)
	}
	return nil
}

// Rows returns the rows of all Insert calls.
func (i *OutboxInserter) Rows() []outbox.Row {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]outbox.Row(nil), i.rows...)
}

// OutboxStore is a mock of [outbox.Store]. If ProcessFunc is nil, Process finds
// no rows. If LagFunc is nil, Lag returns 0.
type OutboxStore struct {
	// ProcessFunc replaces Process. It must call fn itself.
	ProcessFunc func(ctx context.Context, limit, maxBytes int, fn func(ctx context.Context, events []outbox.Record) error) (int, bool, error)
	// LagFunc replaces Lag.
	LagFunc func(ctx context.Context) (time.Duration, error)

	mu    sync.Mutex
	calls int
}

// Process counts the call and calls ProcessFunc.
func (s *OutboxStore) Process(ctx context.Context, limit, maxBytes int, fn func(ctx context.Context, events []outbox.Record) error) (int, bool, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.ProcessFunc != nil {
		return s.ProcessFunc(ctx, limit, maxBytes, fn)
	}
	return 0, false, nil
}

// Lag calls LagFunc.
func (s *OutboxStore) Lag(ctx context.Context) (time.Duration, error) {
	if s.LagFunc != nil {
		return s.LagFunc(ctx)
	}
	return 0, nil
}

// ProcessCalls returns the number of Process calls.
func (s *OutboxStore) ProcessCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// OutboxPublisher is a mock of [outbox.Publisher]. It records the payloads. If
// PublishFunc is nil, Publish returns nil.
type OutboxPublisher struct {
	PublishFunc func(ctx context.Context, payloads [][]byte) error

	mu       sync.Mutex
	payloads [][]byte
}

// Publish records the payloads and calls PublishFunc.
func (p *OutboxPublisher) Publish(ctx context.Context, payloads [][]byte) error {
	p.mu.Lock()
	p.payloads = append(p.payloads, payloads...)
	p.mu.Unlock()
	if p.PublishFunc != nil {
		return p.PublishFunc(ctx, payloads)
	}
	return nil
}

// Payloads returns the payloads of all Publish calls.
func (p *OutboxPublisher) Payloads() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.payloads...)
}

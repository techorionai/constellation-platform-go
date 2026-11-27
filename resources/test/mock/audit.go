package mock

import (
	"context"
	"sync"

	"github.com/techorionai/constellation-platform-go/audit"
	"github.com/techorionai/constellation-platform-go/outbox"
)

var (
	_ audit.Writer   = (*AuditWriter)(nil)
	_ audit.Inserter = (*AuditInserter)(nil)
)

// AuditWriter is a mock of [audit.Writer]. It records the entries. If WriteFunc
// is nil, Write returns nil.
type AuditWriter struct {
	WriteFunc func(ctx context.Context, entries ...audit.Entry) error

	mu      sync.Mutex
	entries []audit.Entry
}

// Write records the entries and calls WriteFunc.
func (w *AuditWriter) Write(ctx context.Context, entries ...audit.Entry) error {
	w.mu.Lock()
	w.entries = append(w.entries, entries...)
	w.mu.Unlock()
	if w.WriteFunc != nil {
		return w.WriteFunc(ctx, entries...)
	}
	return nil
}

// Entries returns the entries of all Write calls.
func (w *AuditWriter) Entries() []audit.Entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]audit.Entry(nil), w.entries...)
}

// AuditInserter is a mock of [audit.Inserter]. It records the rows. The mock
// reports a transaction when NoTx is false.
type AuditInserter struct {
	// NoTx makes InTransaction return false.
	NoTx bool
	// InsertFunc replaces Insert. If it is nil, Insert returns nil.
	InsertFunc func(ctx context.Context, rows []outbox.Row) error

	mu    sync.Mutex
	rows  []outbox.Row
	calls int
}

// InTransaction returns true unless NoTx is set.
func (i *AuditInserter) InTransaction(context.Context) bool { return !i.NoTx }

// Insert records the rows and calls InsertFunc.
func (i *AuditInserter) Insert(ctx context.Context, rows []outbox.Row) error {
	i.mu.Lock()
	i.rows = append(i.rows, rows...)
	i.calls++
	i.mu.Unlock()
	if i.InsertFunc != nil {
		return i.InsertFunc(ctx, rows)
	}
	return nil
}

// Rows returns the rows of all Insert calls.
func (i *AuditInserter) Rows() []outbox.Row {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]outbox.Row(nil), i.rows...)
}

// InsertCalls returns the number of Insert calls.
func (i *AuditInserter) InsertCalls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

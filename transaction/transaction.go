package transaction

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// Errors of the transaction port.
var (
	// ErrSerialization means that the database stopped the transaction because
	// of a conflict (a serialization failure or a deadlock). The caller can try
	// again.
	ErrSerialization = errors.New("transaction: serialization failure")
	// ErrNestedOptions means that a nested WithinTx call has an option other
	// than [Name]. A nested call joins the outer transaction.
	ErrNestedOptions = errors.New("transaction: a nested call cannot set transaction options")
	// ErrInvalidOption means that an option is not valid.
	ErrInvalidOption = errors.New("transaction: invalid option")
	// ErrForeignTx means that the context has a transaction that the
	// transaction adapter did not start.
	ErrForeignTx = errors.New("transaction: the context has a transaction that this adapter did not start")
)

// Transactor runs code in a database transaction.
type Transactor interface {
	// WithinTx runs fn in one transaction. It commits if fn returns nil. It
	// rolls back if fn returns an error or panics.
	WithinTx(ctx context.Context, fn func(ctx context.Context) error, opts ...Option) error

	// AfterCommit registers fn to run after the commit of the transaction in
	// ctx. If ctx has no transaction, it runs fn at once.
	AfterCommit(ctx context.Context, fn func(ctx context.Context))
}

// Level is a transaction isolation level.
type Level int

// The isolation levels. ReadCommitted is the default.
const (
	ReadCommitted Level = iota
	RepeatableRead
	Serializable
)

// String returns the name of the level, for example "read_committed". It is
// the value of the span attribute db.transaction.isolation.
func (l Level) String() string {
	switch l {
	case ReadCommitted:
		return "read_committed"
	case RepeatableRead:
		return "repeatable_read"
	case Serializable:
		return "serializable"
	default:
		return fmt.Sprintf("invalid(%d)", int(l))
	}
}

// DefaultName is the name of a transaction when no [Name] option is set.
const DefaultName = "unnamed"

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Settings are the resolved options of a transaction.
type Settings struct {
	// ReadOnly is true for a read-only transaction.
	ReadOnly bool
	// Isolation is the isolation level.
	Isolation Level
	// Name names the transaction for the metrics and the span.
	Name string

	hasTxOptions bool
}

// HasTxOptions reports whether the options include [ReadOnly] or [Isolation].
// A nested WithinTx call joins the outer transaction, so it must not set them.
func (s Settings) HasTxOptions() bool { return s.hasTxOptions }

// Option changes the settings of a transaction.
type Option func(*Settings)

// ReadOnly makes the transaction read-only.
func ReadOnly() Option {
	return func(s *Settings) { s.ReadOnly = true; s.hasTxOptions = true }
}

// Isolation sets the isolation level. The default is [ReadCommitted].
func Isolation(level Level) Option {
	return func(s *Settings) { s.Isolation = level; s.hasTxOptions = true }
}

// Name gives the transaction a name for the metrics and the span, for example
// "create_order". It must match ^[a-z][a-z0-9_]{0,63}$. The names are fixed in
// the code, so the cardinality of the metrics stays low. The default is
// "unnamed".
func Name(name string) Option {
	return func(s *Settings) { s.Name = name }
}

// Resolve checks the options and returns the settings. It returns an error that
// wraps [ErrInvalidOption] for an invalid name or for an isolation level that
// this package does not define. An adapter and the mock call it, so that both
// check the options in the same way.
func Resolve(opts ...Option) (Settings, error) {
	s := Settings{Name: DefaultName}
	for _, o := range opts {
		o(&s)
	}
	if !namePattern.MatchString(s.Name) {
		return Settings{}, fmt.Errorf("%w: the name must match %s", ErrInvalidOption, namePattern)
	}
	switch s.Isolation {
	case ReadCommitted, RepeatableRead, Serializable:
	default:
		return Settings{}, fmt.Errorf("%w: unknown isolation level %d", ErrInvalidOption, int(s.Isolation))
	}
	return s, nil
}
